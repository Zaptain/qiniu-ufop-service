package ufop

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/qiniu/api/auth/digest"
	fio "github.com/qiniu/api/io"
	rio "github.com/qiniu/api/resumable/io"
	"github.com/qiniu/api/rs"
	"github.com/qiniu/iconv"
	"io/ioutil"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

type UnZipResult struct {
	Files []UnZipFile `json:""`
}

type UnZipFile struct {
	Key   string `json:"key"`
	Hash  string `json:"hash,omitempty"`
	Error string `json:"error,omitempty"`
}

type UnZipper struct {
	mac *digest.Mac
}

func (this *UnZipper) parse(cmd string) (bucket string, overwrite bool, err error) {
	pattern := "^unzip/bucket/[a-zA-Z-_=]+(/overwrite/[0|1]){0,1}$"
	matched, _ := regexp.Match(pattern, []byte(cmd))
	if !matched {
		err = errors.New("invalid unzip command format")
		return
	}
	items := strings.Split(cmd, "/")

	if len(items) >= 3 {
		bucketBytes, paramErr := base64.URLEncoding.DecodeString(items[2])
		if paramErr != nil {
			err = errors.New("invalid unzip parameter 'bucket'")
			return
		}
		bucket = string(bucketBytes)
	}
	if len(items) == 5 {
		overwriteVal, paramErr := strconv.ParseInt(items[4], 10, 64)
		if paramErr != nil {
			err = errors.New("invalid unzip parameter 'overwrite'")
			return
		}
		if overwriteVal == 1 {
			overwrite = true
		}
	}
	return
}

func (this *UnZipper) Do(req UfopRequest) (result interface{}, err error) {
	//check mimetype
	if req.Src.MimeType != "application/zip" {
		err = errors.New("unsupported mimetype to unzip")
		return
	}

	//parse command
	bucket, overwrite, pErr := this.parse(req.Cmd)
	if pErr != nil {
		err = pErr
		return
	}

	//get resource
	resUrl := req.Src.Url
	resResp, respErr := http.Get(resUrl)
	if respErr != nil {
		err = errors.New("retrieve resource data failed")
		return
	}
	defer resResp.Body.Close()

	respData, respErr := ioutil.ReadAll(resResp.Body)
	if respErr != nil {
		err = errors.New("read resource data failed")
		return
	}

	//read zip
	respReader := bytes.NewReader(respData)
	zipReader, zipErr := zip.NewReader(respReader, int64(respReader.Len()))
	if zipErr != nil {
		err = errors.New("invalid zip file")
		return
	}

	//parse zip
	cd, cErr := iconv.Open("utf-8", "gbk")
	if cErr != nil {
		err = errors.New(fmt.Sprintf("create charset converter error"))
		return
	}
	defer cd.Close()
	rputSettings := rio.Settings{
		ChunkSize: 4 * 1024 * 1024,
		Workers:   1,
	}
	rio.SetSettings(&rputSettings)
	var rputThreshold int64 = 100 * 1024 * 1024
	policy := rs.PutPolicy{
		Scope: bucket,
	}
	var unzipResult UnZipResult
	unzipResult.Files = make([]UnZipFile, 0)

	zipFiles := zipReader.File
	for _, zipFile := range zipFiles {
		fileInfo := zipFile.FileHeader.FileInfo()
		fileName := zipFile.FileHeader.Name
		fileSize := fileInfo.Size()

		if !utf8.Valid([]byte(fileName)) {
			fileName = cd.ConvString(fileName)
		}

		if fileInfo.IsDir() {
			continue
		}

		zipFileReader, zipErr := zipFile.Open()
		if zipErr != nil {
			err = errors.New("open zip file content failed")
			return
		}
		defer zipFileReader.Close()

		unzipData, unzipErr := ioutil.ReadAll(zipFileReader)
		if unzipErr != nil {
			err = errors.New("unzip the file content failed")
			return
		}
		unzipReader := bytes.NewReader(unzipData)
		//save file to bucket
		if overwrite {
			policy.Scope = bucket + ":" + fileName
		}
		uptoken := policy.Token(this.mac)
		var unzipFile UnZipFile
		unzipFile.Key = fileName
		if fileSize <= rputThreshold {
			var fputRet fio.PutRet
			fErr := fio.Put(nil, &fputRet, uptoken, fileName, unzipReader, nil)
			if fErr != nil {
				unzipFile.Error = "save unzip file to bucket error"
			} else {
				unzipFile.Hash = fputRet.Hash
			}

		} else {
			var rputRet rio.PutRet
			rErr := rio.Put(nil, &rputRet, uptoken, fileName, unzipReader, fileSize, nil)
			if rErr != nil {
				unzipFile.Error = "save unzip file to bucket error"
			} else {
				unzipFile.Hash = rputRet.Hash
			}
		}
		unzipResult.Files = append(unzipResult.Files, unzipFile)
	}
	return unzipResult, err
}