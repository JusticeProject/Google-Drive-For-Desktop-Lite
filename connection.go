package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"golang.org/x/oauth2/google"
	"golang.org/x/oauth2/jwt"
	"google.golang.org/api/drive/v2"
)

//*************************************************************************************************
//*************************************************************************************************

type GoogleDriveConnection struct {
	conf        *jwt.Config
	client      *http.Client
	api_key     string
	ctx         context.Context
	numApiCalls int64
}

//*************************************************************************************************
//*************************************************************************************************

// these structs match the data that is received from Google Drive API, the json decoder will fill in these structs
type FileMetaData struct {
	// NOTE!!** if updating this then be sure to update the parameters when sending the GET request
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	MimeType     string   `json:"mimeType"`
	ModifiedTime string   `json:"modifiedTime"` // "modifiedTime": "2022-01-22T18:32:04.223Z"
	Md5Checksum  string   `json:"md5Checksum"`
	Parents      []string `json:"parents"`
	// NOTE!!** if updating this then be sure to update the parameters when sending the GET request
}

type ListFilesResponse struct {
	NextPageToken string         `json:"nextPageToken"`
	Files         []FileMetaData `json:"files"`
}

//*********************************************************

type GenerateIdsResponse struct {
	IDs []string `json:"ids"`
}

//*********************************************************

type UpdateFileRequest struct {
	ModifiedTime string `json:"modifiedTime"`
}

type CreateFileRequest struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Parents      []string `json:"parents"`
	ModifiedTime string   `json:"modifiedTime"`
}

type CreateFolderRequest struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	MimeType     string   `json:"mimeType"`
	Parents      []string `json:"parents"`
	ModifiedTime string   `json:"modifiedTime"`
}

//*************************************************************************************************
//*************************************************************************************************

func (conn *GoogleDriveConnection) initializeGoogleDrive() {
	// load the service account file
	data, err := ioutil.ReadFile("config/service-account.json")
	if err != nil {
		log.Fatal("failed to read json file")
	}

	// parse the json for our service account
	conf, err := google.JWTConfigFromJSON(data, drive.DriveScope)
	if err != nil {
		log.Fatal("failed to parse json file")
	}
	conn.conf = conf
	conn.ctx = context.Background()
	conn.client = conf.Client(conn.ctx)

	// load the api key from a file
	apiKeyBytes, err := ioutil.ReadFile("config/api-key.txt")
	if err != nil {
		log.Fatal("failed to read API key")
	}
	conn.api_key = string(apiKeyBytes)
}

//*************************************************************************************************
//*************************************************************************************************

func (conn *GoogleDriveConnection) getItemsInSharedFolder(localFolderPath, folderId string) (ListFilesResponse, error) {
	data, err := conn.getPageInSharedFolder(localFolderPath, folderId, "")
	if err != nil {
		return ListFilesResponse{}, err
	}

	for len(data.NextPageToken) > 0 {
		newData, err := conn.getPageInSharedFolder(localFolderPath, folderId, data.NextPageToken)
		if err != nil {
			return ListFilesResponse{}, err
		}
		data.Files = append(data.Files, newData.Files...)
		data.NextPageToken = newData.NextPageToken
	}

	return data, nil
}

//*********************************************************

func (conn *GoogleDriveConnection) getPageInSharedFolder(localFolderPath, folderId, nextPageToken string) (ListFilesResponse, error) {
	conn.numApiCalls++

	if len(nextPageToken) == 0 {
		DebugLog("getting first page in shared folder", localFolderPath)
	} else {
		DebugLog("getting next page for folder", localFolderPath)
	}

	parameters := "?fields=" + url.QueryEscape("nextPageToken,files(id,name,mimeType,modifiedTime,md5Checksum,parents)")
	if len(nextPageToken) > 0 {
		parameters += "&pageToken=" + nextPageToken
	}
	parameters += "&key=" + conn.api_key
	parameters += "&q=%27" + folderId + "%27%20in%20parents" // %27 is single quote, %20 is a space
	response, err := conn.client.Get("https://www.googleapis.com/drive/v3/files" + parameters)
	//fmt.Println("Sent request:", response.Request.Host, response.Request.URL, response.Request.Header)
	DebugLog("received StatusCode", response.StatusCode)

	if err != nil {
		fmt.Println(err)
		return ListFilesResponse{}, err
	}

	defer response.Body.Close()

	// if we didn't get what we were expecting, print out the response
	if response.StatusCode >= 400 {
		bodyData, err := io.ReadAll(response.Body)
		if err != nil {
			fmt.Println(err)
		}
		fmt.Println(string(bodyData))
		return ListFilesResponse{}, errors.New("unexpected response in getItemsInSharedFolder")
	}

	// decode the json data into our struct
	var data ListFilesResponse
	err = json.NewDecoder(response.Body).Decode(&data)
	if err != nil {
		fmt.Println(err)
	}

	return data, err
}

//*************************************************************************************************
//*************************************************************************************************

func (conn *GoogleDriveConnection) getMetadataById(name string, id string) (FileMetaData, error) {
	conn.numApiCalls++
	DebugLog("getting metadata for", name, id)

	parameters := "?fields=" + url.QueryEscape("id,name,mimeType,modifiedTime,md5Checksum,parents")
	parameters += "&key=" + conn.api_key
	response, err := conn.client.Get("https://www.googleapis.com/drive/v3/files/" + id + parameters)
	DebugLog("received StatusCode", response.StatusCode)

	if err != nil {
		fmt.Println(err)
		return FileMetaData{}, err
	}

	defer response.Body.Close()
	bodyData, err := io.ReadAll(response.Body)
	if err != nil {
		fmt.Println(err)
		return FileMetaData{}, err
	}

	// if we didn't get what we were expecting, print out the response
	if response.StatusCode >= 400 {
		fmt.Println(string(bodyData))
		return FileMetaData{}, errors.New("failed to get metadata by ID")
	}

	var data FileMetaData
	err = json.Unmarshal(bodyData, &data)
	if err != nil {
		fmt.Println(err)
	}

	DebugLog(data)

	return data, err
}

//*************************************************************************************************
//*************************************************************************************************

func (conn *GoogleDriveConnection) generateIds(count int) []string {
	conn.numApiCalls++
	DebugLog("generating ids with count:", count)

	parameters := "?count=" + fmt.Sprintf("%v", count)
	parameters += "&key=" + conn.api_key
	response, err := conn.client.Get("https://www.googleapis.com/drive/v3/files/generateIds" + parameters)
	DebugLog("received StatusCode", response.StatusCode)

	if err != nil {
		fmt.Println(err)
		return []string{}
	}

	defer response.Body.Close()

	// if we didn't get what we were expecting, print out the response
	if response.StatusCode >= 400 {
		bodyData, err := io.ReadAll(response.Body)
		if err != nil {
			fmt.Println(err)
		}
		fmt.Println(string(bodyData))
		return []string{}
	}

	// decode the json data into our struct
	var data GenerateIdsResponse
	err = json.NewDecoder(response.Body).Decode(&data)
	if err != nil {
		fmt.Println(err)
	}

	return data.IDs
}

//*************************************************************************************************
//*************************************************************************************************

func (conn *GoogleDriveConnection) createRemoteFolder(folderRequest CreateFolderRequest) error {
	conn.numApiCalls++
	DebugLog("creating remote folder:", folderRequest)

	data, _ := json.Marshal(folderRequest)
	reader := bytes.NewReader(data)

	parameters := "?key=" + conn.api_key
	response, err := conn.client.Post("https://www.googleapis.com/drive/v3/files"+parameters, "application/json; charset=UTF-8", reader)
	//fmt.Println("Sent request:", response.Request.Host, response.Request.URL, response.Request.Header, response.Request.Body)
	DebugLog("received StatusCode", response.StatusCode)

	if err != nil {
		fmt.Println(err)
		return err
	}

	defer response.Body.Close()
	bodyData, err := io.ReadAll(response.Body)
	if err != nil {
		fmt.Println(err)
		return err
	}
	DebugLog(string(bodyData))

	// if we didn't get what we were expecting, print out the response
	if response.StatusCode >= 400 {
		fmt.Println(string(bodyData))
		return errors.New("failed")
	}

	return nil
}

//*************************************************************************************************
//*************************************************************************************************

func (conn *GoogleDriveConnection) createRemoteFile(fileRequest CreateFileRequest, fileData []byte) error {
	conn.numApiCalls++
	DebugLog("Creating remote file:", fileRequest)

	// build the url
	parameters := "?uploadType=multipart"
	parameters += "&key=" + conn.api_key
	url := "https://www.googleapis.com/upload/drive/v3/files" + parameters

	// build the body
	json_data, _ := json.Marshal(fileRequest)

	body := "--foo_bar_baz\n"
	body += "Content-Type: application/json; charset=UTF-8\n\n"
	body += string(json_data)
	body += "\n--foo_bar_baz\n"
	body += "Content-Type: application/octet-stream\n\n"
	body += string(fileData) + "\n"
	body += "--foo_bar_baz--"

	// create a new request, then call the Do function
	reader := bytes.NewReader([]byte(body))
	req, err := http.NewRequestWithContext(conn.ctx, "POST", url, reader)
	req.Header.Add("Content-Type", "multipart/related; boundary=foo_bar_baz")
	req.Header.Add("Content-Length", fmt.Sprintf("%v", len(body)))
	if err != nil {
		fmt.Println(err)
		return err
	}

	response, err := conn.client.Do(req)
	//fmt.Println("Sent request:", response.Request.Host, response.Request.URL, response.Request.Header, response.Request.Body)
	DebugLog("received StatusCode", response.StatusCode)
	if err != nil {
		fmt.Println(err)
		return err
	}

	defer response.Body.Close()
	bodyData, err := io.ReadAll(response.Body)
	if err != nil {
		fmt.Println(err)
		return err
	}
	DebugLog(string(bodyData))

	// if we didn't get what we were expecting, print out the response
	if response.StatusCode >= 400 {
		fmt.Println(string(bodyData))
		return errors.New("failed")
	}

	return nil
}

//*************************************************************************************************
//*************************************************************************************************

func (conn *GoogleDriveConnection) updateFileAndMetadata(id string, modifiedTime string, fileData []byte) error {
	conn.numApiCalls++
	DebugLog("uploading data for remote file:", id, modifiedTime)

	// build the url
	parameters := "?uploadType=multipart"
	parameters += "&key=" + conn.api_key
	url := "https://www.googleapis.com/upload/drive/v3/files/" + id + parameters

	// build the body
	request := UpdateFileRequest{ModifiedTime: modifiedTime}
	json_data, _ := json.Marshal(request)

	body := "--foo_bar_baz\n"
	body += "Content-Type: application/json; charset=UTF-8\n\n"
	body += string(json_data)
	body += "\n--foo_bar_baz\n"
	body += "Content-Type: application/octet-stream\n\n"
	body += string(fileData) + "\n"
	body += "--foo_bar_baz--"

	// for PATCH requests create a new request, then call the Do function
	reader := bytes.NewReader([]byte(body))
	req, err := http.NewRequestWithContext(conn.ctx, "PATCH", url, reader)
	req.Header.Add("Content-Type", "multipart/related; boundary=foo_bar_baz")
	req.Header.Add("Content-Length", fmt.Sprintf("%v", len(body)))
	if err != nil {
		fmt.Println(err)
		return err
	}

	response, err := conn.client.Do(req)
	//fmt.Println("Sent request:", response.Request.Host, response.Request.URL, response.Request.Header, response.Request.Body)
	DebugLog("received StatusCode", response.StatusCode)
	if err != nil {
		fmt.Println(err)
		return err
	}

	defer response.Body.Close()
	bodyData, err := io.ReadAll(response.Body)
	if err != nil {
		fmt.Println(err)
		return err
	}
	//DebugLog(string(bodyData))

	// if we didn't get what we were expecting, print out the response
	if response.StatusCode >= 400 {
		fmt.Println(string(bodyData))
		return errors.New("failed")
	}

	return nil
}

//*************************************************************************************************
//*************************************************************************************************

func (conn *GoogleDriveConnection) createLargeRemoteFile(fileRequest CreateFileRequest, fileData []byte) error {
	conn.numApiCalls++
	DebugLog("Creating large remote file:", fileRequest)

	// Step 1: get a session URI where we can upload the data to

	// build the url
	parameters := "?uploadType=resumable"
	parameters += "&key=" + conn.api_key
	url := "https://www.googleapis.com/upload/drive/v3/files" + parameters

	// create a new request, then call the Do function
	json_data, _ := json.Marshal(fileRequest)
	reader := bytes.NewReader(json_data)
	req, err := http.NewRequestWithContext(conn.ctx, "POST", url, reader)
	req.Header.Add("Content-Type", "application/json; charset=UTF-8")
	req.Header.Add("Content-Length", fmt.Sprintf("%v", len(json_data)))
	if err != nil {
		fmt.Println(err)
		return err
	}

	response, err := conn.client.Do(req)
	//fmt.Println("Sent request:", response.Request.Host, response.Request.URL, response.Request.Header, response.Request.Body)
	DebugLog("received StatusCode", response.StatusCode)
	if err != nil {
		fmt.Println(err)
		return err
	}

	locationHeader, inHeader := response.Header["Location"]
	if !inHeader || len(locationHeader) == 0 {
		err := errors.New("header Location not available for createLargeRemoteFile")
		fmt.Println(err)
		return err
	}
	DebugLog("received locationHeader:", locationHeader)

	bodyData, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		fmt.Println(err)
		return err
	}
	DebugLog(string(bodyData))

	// if we didn't get what we were expecting, print out the response
	if response.StatusCode >= 400 {
		fmt.Println(string(bodyData))
		return errors.New("failed")
	}

	//*************************************************************************

	// Step 2: upload data to the session URI

	bytesUploaded := 0
	for try := 0; try < 5; try++ {
		parameters = ""
		if strings.Contains(locationHeader[0], "&key=") {
			DebugLog("session URI already has the API key")
		} else {
			DebugLog("session URI did not have the API key, adding it")
			parameters += "&key=" + conn.api_key
		}
		url = locationHeader[0] + parameters
		req, err = http.NewRequestWithContext(conn.ctx, "PUT", url, bytes.NewReader(fileData))
		if err != nil {
			fmt.Println(err)
			continue // do a retry
		}
		req.Header.Add("Content-Length", fmt.Sprintf("%v", len(fileData)-bytesUploaded))
		if bytesUploaded > 0 {
			req.Header.Add("Content-Range", fmt.Sprintf("bytes %v-%v/%v", bytesUploaded, len(fileData)-1, len(fileData)))
		}

		response, err = conn.client.Do(req)
		DebugLog("received StatusCode", response.StatusCode)
		if err != nil {
			fmt.Println(err)
			bytesUploaded, err := conn.getBytesUploaded(url, len(fileData))
			if err != nil {
				return err
			}
			if bytesUploaded < len(fileData) {
				DebugLog("trying again after", bytesUploaded, "bytes were uploaded")
				continue // do a retry
			}
		}

		if response.StatusCode >= 400 {
			err = errors.New("error uploading large file")
			fmt.Println(err)
			bytesUploaded, err := conn.getBytesUploaded(url, len(fileData))
			if err != nil {
				return err
			}
			if bytesUploaded < len(fileData) {
				DebugLog("trying again after", bytesUploaded, "bytes were uploaded")
				continue // do a retry
			}
		}

		bodyData, err = io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			fmt.Println(err)
			bytesUploaded, err := conn.getBytesUploaded(url, len(fileData))
			if err != nil {
				return err
			}
			if bytesUploaded < len(fileData) {
				DebugLog("trying again after", bytesUploaded, "bytes were uploaded")
				continue // do a retry
			}
		}
		DebugLog(string(bodyData))

		// if we got this far then it was successful
		return nil
	}

	return errors.New("ran out of retries in createLargeRemoteFile")
}

//*************************************************************************************************
//*************************************************************************************************

func (conn *GoogleDriveConnection) getBytesUploaded(url string, fileSize int) (int, error) {
	DebugLog("requesting the number of bytes uploaded")

	req, err := http.NewRequestWithContext(conn.ctx, "PUT", url, nil)
	req.Header.Add("Content-Range", fmt.Sprintf("*/%v", fileSize))
	if err != nil {
		fmt.Println(err)
		return 0, err
	}

	response, err := conn.client.Do(req)
	DebugLog("received StatusCode", response.StatusCode)
	if err != nil {
		return 0, err
	}

	switch response.StatusCode {
	case 200, 201:
		return fileSize, nil
	case 308:
		rangeHeader, inHeaders := response.Header["Range"]
		if !inHeaders || len(rangeHeader) == 0 {
			return 0, nil
		}
		rangeSplit := strings.Split(rangeHeader[0], "-")
		if len(rangeSplit) > 1 {
			bytesUploaded, err := strconv.ParseInt(rangeSplit[1], 10, 0)
			if err == nil {
				return int(bytesUploaded), nil
			}
		}
	default:
		return 0, errors.New("unknown number of bytes uploaded")
	}

	return 0, nil
}

//*************************************************************************************************
//*************************************************************************************************

func (conn *GoogleDriveConnection) updateLargeFileAndMetadata(id string, modifiedTime string, fileData []byte) error {
	// TODO: this is just a copy/paste of the small file version, need to update for large files
	conn.numApiCalls++
	DebugLog("uploading data for large remote file:", id, modifiedTime)

	// build the url
	parameters := "?uploadType=multipart"
	parameters += "&key=" + conn.api_key
	url := "https://www.googleapis.com/upload/drive/v3/files/" + id + parameters

	// build the body
	request := UpdateFileRequest{ModifiedTime: modifiedTime}
	json_data, _ := json.Marshal(request)

	body := "--foo_bar_baz\n"
	body += "Content-Type: application/json; charset=UTF-8\n\n"
	body += string(json_data)
	body += "\n--foo_bar_baz\n"
	body += "Content-Type: application/octet-stream\n\n"
	body += string(fileData) + "\n"
	body += "--foo_bar_baz--"

	// for PATCH requests create a new request, then call the Do function
	reader := bytes.NewReader([]byte(body))
	// TODO: use PUT?
	req, err := http.NewRequestWithContext(conn.ctx, "PATCH", url, reader)
	req.Header.Add("Content-Type", "multipart/related; boundary=foo_bar_baz")
	req.Header.Add("Content-Length", fmt.Sprintf("%v", len(body)))
	if err != nil {
		fmt.Println(err)
		return err
	}

	response, err := conn.client.Do(req)
	//fmt.Println("Sent request:", response.Request.Host, response.Request.URL, response.Request.Header, response.Request.Body)
	DebugLog("received StatusCode", response.StatusCode)
	if err != nil {
		fmt.Println(err)
		return err
	}

	defer response.Body.Close()
	bodyData, err := io.ReadAll(response.Body)
	if err != nil {
		fmt.Println(err)
		return err
	}
	//DebugLog(string(bodyData))

	// if we didn't get what we were expecting, print out the response
	if response.StatusCode >= 400 {
		fmt.Println(string(bodyData))
		return errors.New("failed")
	}

	return nil
}

//*************************************************************************************************
//*************************************************************************************************

func (conn *GoogleDriveConnection) downloadFile(id string, localFileName string) error {
	conn.numApiCalls++
	DebugLog("downloading", localFileName, id)

	parameters := "?alt=media"
	parameters += "&key=" + conn.api_key
	response, err := conn.client.Get("https://www.googleapis.com/drive/v3/files/" + id + parameters)
	DebugLog("received StatusCode", response.StatusCode)

	if err != nil {
		fmt.Println(err)
		return err
	}

	defer response.Body.Close()

	// if we didn't get what we were expecting, print out the response
	if response.StatusCode >= 400 {
		bodyData, err := io.ReadAll(response.Body)
		if err != nil {
			fmt.Println(err)
			return err
		}
		fmt.Println(string(bodyData))
		return errors.New("failed to download")
	}

	fh, err := os.Create(localFileName)
	if err != nil {
		fmt.Println(err)
		return err
	}

	n, err := io.Copy(fh, response.Body)
	DebugLog(fmt.Sprintf("Wrote %v bytes to file", n))
	if err != nil {
		fmt.Println(err)

		// if we only downloaded half the file, remove the local file so we don't upload the half file later on
		fh.Close()
		os.Remove(localFileName)

		return err
	}

	fh.Close()

	return nil
}

//*************************************************************************************************
//*************************************************************************************************

func (conn *GoogleDriveConnection) getModifiedItems(timestamp string) ([]FileMetaData, error) {
	data, err := conn.getPageOfModifiedItems(timestamp, "")
	if err != nil {
		return []FileMetaData{}, err
	}

	for len(data.NextPageToken) > 0 {
		newData, err := conn.getPageOfModifiedItems(timestamp, data.NextPageToken)
		if err != nil {
			return []FileMetaData{}, err
		}
		data.Files = append(data.Files, newData.Files...)
		data.NextPageToken = newData.NextPageToken
	}

	return data.Files, nil
}

//*********************************************************

func (conn *GoogleDriveConnection) getPageOfModifiedItems(timestamp, nextPageToken string) (ListFilesResponse, error) {
	conn.numApiCalls++
	DebugLog("getting page of modified items for timestamp >", timestamp)

	parameters := "?q=" + url.QueryEscape("modifiedTime > '"+timestamp+"'")
	parameters += "&pageSize=1000"
	if len(nextPageToken) > 0 {
		parameters += "&pageToken=" + nextPageToken
	}
	parameters += "&fields=" + url.QueryEscape("nextPageToken,files(id,name,mimeType,modifiedTime,md5Checksum,parents)")
	parameters += "&key=" + conn.api_key

	response, err := conn.client.Get("https://www.googleapis.com/drive/v3/files" + parameters)
	DebugLog("received StatusCode", response.StatusCode)

	if err != nil {
		fmt.Println(err)
		return ListFilesResponse{}, err
	}

	defer response.Body.Close()

	// if we didn't get what we were expecting, print out the response
	if response.StatusCode >= 400 {
		bodyData, err := io.ReadAll(response.Body)
		if err != nil {
			fmt.Println(err)
			return ListFilesResponse{}, err
		}
		fmt.Println(string(bodyData))
		return ListFilesResponse{}, errors.New("unexpected response when getting modified items")
	}

	// decode the json data into our struct
	var data ListFilesResponse
	err = json.NewDecoder(response.Body).Decode(&data)
	if err != nil {
		fmt.Println(err)
		return ListFilesResponse{}, err
	}

	return data, nil
}

//*************************************************************************************************
//*************************************************************************************************

func (conn *GoogleDriveConnection) getFilesOwnedByServiceAcct(verbose bool) ([]FileMetaData, error) {
	data, err := conn.getPageOfFilesOwnedByServiceAcct(verbose, "")
	if err != nil {
		return []FileMetaData{}, err
	}

	for len(data.NextPageToken) > 0 {
		newData, err := conn.getPageOfFilesOwnedByServiceAcct(verbose, data.NextPageToken)
		if err != nil {
			return []FileMetaData{}, err
		}
		data.Files = append(data.Files, newData.Files...)
		data.NextPageToken = newData.NextPageToken
	}

	return data.Files, nil
}

//*********************************************************

func (conn *GoogleDriveConnection) getPageOfFilesOwnedByServiceAcct(verbose bool, nextPageToken string) (ListFilesResponse, error) {
	conn.numApiCalls++
	if len(nextPageToken) == 0 {
		DebugLog("getting first page of files owned by service acct")
	} else {
		DebugLog("getting another page of files owned by service acct")
	}

	parameters := "?fields=" + url.QueryEscape("nextPageToken,files(id,name,mimeType,modifiedTime,md5Checksum,parents)")
	parameters += "&pageSize=1000"
	if len(nextPageToken) > 0 {
		parameters += "&pageToken=" + nextPageToken
	}
	parameters += "&key=" + conn.api_key
	response, err := conn.client.Get("https://www.googleapis.com/drive/v3/files" + parameters)
	DebugLog("received StatusCode", response.StatusCode)

	if err != nil {
		fmt.Println(err)
		return ListFilesResponse{}, err
	}

	defer response.Body.Close()

	// read the data
	bodyData, err := io.ReadAll(response.Body)
	if err != nil {
		fmt.Println(err)
		return ListFilesResponse{}, err
	}

	// if we didn't get what we were expecting, print out the response
	if response.StatusCode >= 400 {
		fmt.Println(string(bodyData))
		return ListFilesResponse{}, errors.New("received unexpected response when getting page of files owned by service acct")
	}

	if verbose {
		fmt.Println(string(bodyData))
	}

	// decode the json data into our struct
	var data ListFilesResponse
	err = json.Unmarshal(bodyData, &data)
	if err != nil {
		fmt.Println(err)
		return ListFilesResponse{}, err
	}

	DebugLog(data.Files)
	return data, nil
}

//*************************************************************************************************
//*************************************************************************************************

func (conn *GoogleDriveConnection) deleteFileOrFolder(item FileMetaData) error {
	conn.numApiCalls++
	DebugLog("deleting", item.Name, item.ID)

	url := "https://www.googleapis.com/drive/v3/files/" + item.ID
	req, err := http.NewRequestWithContext(conn.ctx, "DELETE", url, nil)
	if err != nil {
		fmt.Println(err)
		return err
	}

	response, err := conn.client.Do(req)
	//fmt.Println("Sent request:", response.Request.Host, response.Request.URL, response.Request.Header, response.Request.Body)
	DebugLog("received StatusCode", response.StatusCode)
	if err != nil {
		fmt.Println(err)
		return err
	}

	defer response.Body.Close()
	bodyData, err := io.ReadAll(response.Body)
	if err != nil {
		fmt.Println(err)
		return err
	}
	DebugLog(string(bodyData))

	// if we didn't get what we were expecting, print out the response
	if response.StatusCode >= 400 {
		fmt.Println(string(bodyData))
		return errors.New("failed")
	}

	return nil
}
