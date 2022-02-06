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

	"golang.org/x/oauth2/google"
	"golang.org/x/oauth2/jwt"
	"google.golang.org/api/drive/v2"
)

//*************************************************************************************************
//*************************************************************************************************

type GoogleDriveConnection struct {
	conf    *jwt.Config
	client  *http.Client
	api_key string
	ctx     context.Context
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
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	MimeType string   `json:"mimeType"`
	Parents  []string `json:"parents"`
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
		return FileMetaData{}, errors.New("failed to download")
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

func (conn *GoogleDriveConnection) downloadFile(id string, localFileName string) error {
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

	defer fh.Close()
	n, err := io.Copy(fh, response.Body)
	DebugLog(fmt.Sprintf("Wrote %v bytes to file", n))
	if err != nil {
		fmt.Println(err)
		return err
	}

	return nil
}

//*************************************************************************************************
//*************************************************************************************************

func (conn *GoogleDriveConnection) getModifiedItems(timestamp string) []FileMetaData {
	// TODO: may need to handle nextPageToken

	DebugLog("querying modified items for timestamp >", timestamp)

	parameters := "?q=" + url.QueryEscape("modifiedTime > '"+timestamp+"'")
	parameters += "&fields=" + url.QueryEscape("nextPageToken,files(id,name,mimeType,modifiedTime,md5Checksum,parents)")
	parameters += "&key=" + conn.api_key

	response, err := conn.client.Get("https://www.googleapis.com/drive/v3/files" + parameters)
	//fmt.Println("Sent request:", response.Request.URL)
	DebugLog("received StatusCode", response.StatusCode)

	if err != nil {
		fmt.Println(err)
		return []FileMetaData{}
	}

	defer response.Body.Close()

	// if we didn't get what we were expecting, print out the response
	if response.StatusCode >= 400 {
		bodyData, err := io.ReadAll(response.Body)
		if err != nil {
			fmt.Println(err)
		}
		fmt.Println(string(bodyData))
		return []FileMetaData{}
	}

	// decode the json data into our struct
	var data ListFilesResponse
	err = json.NewDecoder(response.Body).Decode(&data)
	if err != nil {
		fmt.Println(err)
	}

	//DebugLog(data.Files)
	return data.Files
}

//*************************************************************************************************
//*************************************************************************************************

func (conn *GoogleDriveConnection) listFolderById(folderId string) {
	DebugLog("listing folder", folderId)

	// TODO: do I need to implement nextPageToken? This function is for command line debugging

	parameters := "?fields=" + url.QueryEscape("nextPageToken,files(id,name,mimeType,modifiedTime,md5Checksum,parents)")
	parameters += "&key=" + conn.api_key
	parameters += "&q=%27" + folderId + "%27%20in%20parents" // %27 is single quote, %20 is a space
	response, err := conn.client.Get("https://www.googleapis.com/drive/v3/files" + parameters)
	//fmt.Println("Sent request:", response.Request.Host, response.Request.URL, response.Request.Header)
	DebugLog("received StatusCode", response.StatusCode)

	if err != nil {
		fmt.Println(err)
		return
	}

	defer response.Body.Close()

	// if we didn't get what we were expecting, print out the response
	if response.StatusCode >= 400 {
		bodyData, err := io.ReadAll(response.Body)
		if err != nil {
			fmt.Println(err)
		}
		fmt.Println(string(bodyData))
		return
	}

	// decode the json data into our struct
	var data ListFilesResponse
	err = json.NewDecoder(response.Body).Decode(&data)
	if err != nil {
		fmt.Println(err)
	}

	DebugLog(data.Files)
}

//*************************************************************************************************
//*************************************************************************************************

func (conn *GoogleDriveConnection) listFilesOwnedByServiceAcct(verbose bool) []FileMetaData {
	DebugLog("listing files owned by service acct")

	// TODO: implement nextPageToken

	parameters := "?fields=" + url.QueryEscape("nextPageToken,files(id,name,mimeType,modifiedTime,md5Checksum,parents)")
	parameters += "&key=" + conn.api_key
	response, err := conn.client.Get("https://www.googleapis.com/drive/v3/files" + parameters)
	DebugLog("received StatusCode", response.StatusCode)

	if err != nil {
		fmt.Println(err)
		return []FileMetaData{}
	}

	defer response.Body.Close()

	// read the data
	bodyData, err := io.ReadAll(response.Body)
	if err != nil {
		fmt.Println(err)
	}

	// if we didn't get what we were expecting, print out the response
	if response.StatusCode >= 400 {
		fmt.Println(string(bodyData))
		return []FileMetaData{}
	}

	if verbose {
		fmt.Println(string(bodyData))
	}

	// decode the json data into our struct
	var data ListFilesResponse
	err = json.Unmarshal(bodyData, &data)
	if err != nil {
		fmt.Println(err)
	}

	DebugLog(data.Files)
	return data.Files
}

//*************************************************************************************************
//*************************************************************************************************

func (conn *GoogleDriveConnection) deleteFileOrFolder(item FileMetaData) error {
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
