package main

import (
	"bufio"
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
	"path/filepath"
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
	baseFolders map[string]string // key = local folder name, value = folder id on Google Drive
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

	// read our config file that tells us the folder id for each shared folder
	fh, err := os.Open("config/folder-ids.txt")
	if err != nil {
		log.Fatal("failed to read folder IDs")
	}
	defer fh.Close()

	// get the id number for each main folder that is shared, save it for later
	conn.baseFolders = make(map[string]string)
	scanner := bufio.NewScanner(fh)
	for scanner.Scan() {
		line := scanner.Text()
		line_split := strings.SplitN(line, "=", 2)
		conn.baseFolders[line_split[0]] = line_split[1]
	}

	fmt.Println("these are our starting baseFolders:", conn.baseFolders)
}

//*************************************************************************************************
//*************************************************************************************************

func (conn *GoogleDriveConnection) getBaseFolderSlice() []string {
	keys := make([]string, len(conn.baseFolders))

	i := 0
	for k := range conn.baseFolders {
		keys[i] = k
		i++
	}

	return keys
}

//*************************************************************************************************
//*************************************************************************************************

func (conn *GoogleDriveConnection) clearLookupMap() {
	if len(localToRemoteLookup) > 0 {
		localToRemoteLookup = make(map[string]FileMetaData)
	}
}

//*************************************************************************************************
//*************************************************************************************************

func (conn *GoogleDriveConnection) fillLookupMap(localFolders []string) {
	for _, localFolder := range localFolders {
		var folderId string

		// if localFolder is a base folder and not in the lookupMap, then add it
		baseId, isBaseFolder := conn.baseFolders[localFolder]
		remoteMetaData, inLookupMap := localToRemoteLookup[localFolder]
		if isBaseFolder && !inLookupMap {
			localToRemoteLookup[localFolder] = FileMetaData{ID: baseId}
			folderId = baseId
		} else if inLookupMap {
			folderId = remoteMetaData.ID
		}

		data := conn.getItemsInSharedFolder(localFolder, folderId, "")
		for len(data.NextPageToken) > 0 {
			newData := conn.getItemsInSharedFolder(localFolder, folderId, data.NextPageToken)
			data.Files = append(data.Files, newData.Files...)
			data.NextPageToken = newData.NextPageToken
		}

		// add the files and folders to our map
		for _, file := range data.Files {
			localToRemoteLookup[filepath.Join(localFolder, file.Name)] = file
		}

		// if any are folders then we will need to look up their contents as well, call this same function recursively
		for _, file := range data.Files {
			if strings.Contains(file.MimeType, "folder") {
				foldersToLookup := []string{filepath.Join(localFolder, file.Name)}
				conn.fillLookupMap(foldersToLookup)
			}
		}
	}
}

//*************************************************************************************************
//*************************************************************************************************

func (conn *GoogleDriveConnection) clearUploadLookupMap() {
	if len(uploadLookupMap) > 0 {
		uploadLookupMap = make(map[string]FileMetaData)
	}
}

//*************************************************************************************************
//*************************************************************************************************

func localPathIsNeeded(localPath string, filesToUpload map[string]bool) bool {
	// if there is one that does not result in .. then we need this path
	for fileToUpload := range filesToUpload {
		relativePath, err := filepath.Rel(localPath, fileToUpload)
		if err == nil {
			if !strings.Contains(relativePath, "..") {
				return true
			}
		}
	}

	return false
}

func (conn *GoogleDriveConnection) fillUploadLookupMap(localFolders []string, filesToUpload map[string]bool) {
	for _, localFolder := range localFolders {

		// check if this localFolder is in the path of any of the filesToUpload
		if !localPathIsNeeded(localFolder, filesToUpload) {
			continue
		}

		var folderId string

		// if localFolder is a base folder and not in the lookupMap, then add it
		baseId, isBaseFolder := conn.baseFolders[localFolder]
		remoteMetaData, inLookupMap := uploadLookupMap[localFolder]
		if isBaseFolder && !inLookupMap {
			uploadLookupMap[localFolder] = FileMetaData{ID: baseId}
			folderId = baseId
		} else if inLookupMap {
			folderId = remoteMetaData.ID
		}

		data := conn.getItemsInSharedFolder(localFolder, folderId, "")
		for len(data.NextPageToken) > 0 {
			newData := conn.getItemsInSharedFolder(localFolder, folderId, data.NextPageToken)
			data.Files = append(data.Files, newData.Files...)
			data.NextPageToken = newData.NextPageToken
		}

		// add the files and folders to our map
		for _, file := range data.Files {
			uploadLookupMap[filepath.Join(localFolder, file.Name)] = file
		}

		// if any are folders then we will need to look up their contents as well, call this same function recursively
		for _, file := range data.Files {
			if strings.Contains(file.MimeType, "folder") {
				foldersToLookup := []string{filepath.Join(localFolder, file.Name)}
				conn.fillUploadLookupMap(foldersToLookup, filesToUpload)
			}
		}
	}
}

//*************************************************************************************************
//*************************************************************************************************

func (conn *GoogleDriveConnection) clearDownloadLookupMap() {
	if len(downloadLookupMap) > 0 {
		downloadLookupMap = make(map[string]FileMetaData)
	}
}

//*************************************************************************************************
//*************************************************************************************************

func (conn *GoogleDriveConnection) fillDownloadLookupMap(remoteModifiedFiles []FileMetaData, skipExtraFolderSearch bool) {
	tempIdToMetaData := make(map[string]FileMetaData) // key = id, value = metadata

	// add the known base folders to the temp map and download lookup map
	for folderName, id := range conn.baseFolders {
		tempIdToMetaData[id] = FileMetaData{ID: id}
		downloadLookupMap[folderName] = FileMetaData{ID: id}
	}

	// add all the modified files/folders to our temp map, and the parents if necessary
	for _, remoteMetaData := range remoteModifiedFiles {
		tempIdToMetaData[remoteMetaData.ID] = remoteMetaData

		if !skipExtraFolderSearch && strings.Contains(remoteMetaData.MimeType, "folder") {
			response := conn.getItemsInSharedFolder(remoteMetaData.Name, remoteMetaData.ID, "")
			for _, metadata := range response.Files {
				tempIdToMetaData[metadata.ID] = metadata
			}
		}

		// add all the parents recursively
		// TODO: if it fails then return an error from this function so we can try again next time, don't want to download the wrong paths, or will
		// getFullPath handle it by not finding a link from the file all the way up to the base folder?
		conn.addParents(remoteMetaData, tempIdToMetaData)
	}

	// now piece together all the modified items by using the parent ids to create the file hierarchy
	for id, metadata := range tempIdToMetaData {
		fullPath, err := conn.getFullPath(id, tempIdToMetaData)
		if fullPath != "" && err == nil {
			downloadLookupMap[fullPath] = metadata
		}
	}
}

//***********************************************

func (conn *GoogleDriveConnection) addParents(metadata FileMetaData, tempIdToMetaData map[string]FileMetaData) {
	if len(metadata.Parents) > 0 {
		parentId := metadata.Parents[0]
		_, parentInMap := tempIdToMetaData[parentId]

		if parentId != "" && !parentInMap {
			parentMetadata, err := conn.getMetadataById("?", parentId)
			if err != nil {
				return
			}
			tempIdToMetaData[parentMetadata.ID] = parentMetadata
			conn.addParents(parentMetadata, tempIdToMetaData)
		}
	}
}

//***********************************************

func (conn *GoogleDriveConnection) getFullPath(id string, tempIdToMetaData map[string]FileMetaData) (string, error) {
	metadata, inMap := tempIdToMetaData[id]

	if inMap {
		if len(metadata.Parents) > 0 {
			parentPath, err := conn.getFullPath(metadata.Parents[0], tempIdToMetaData)
			if err != nil {
				return "", err
			}

			if parentPath == "" {
				return "", errors.New("something went wrong")
			} else {
				fullPath := parentPath + string(filepath.Separator) + metadata.Name
				return fullPath, nil
			}
		} else {
			// check if this is a base folder
			for baseFolderName, baseFolderId := range conn.baseFolders {
				if id == baseFolderId {
					return baseFolderName, nil
				}
			}
			return "", errors.New("no base folder found")
		}
	}
	return "", errors.New("id was not found")
}

//*************************************************************************************************
//*************************************************************************************************

// TODO: should I handle the token entirely within this function or a helper function so the caller doesn't have to deal with it?
func (conn *GoogleDriveConnection) getItemsInSharedFolder(localFolderPath string, folderId string, nextPageToken string) ListFilesResponse {
	if len(nextPageToken) == 0 {
		DebugLog("getting items in shared folder", localFolderPath)
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
		return ListFilesResponse{}
	}

	defer response.Body.Close()

	// if we didn't get what we were expecting, print out the response
	if response.StatusCode >= 400 {
		bodyData, err := io.ReadAll(response.Body)
		if err != nil {
			fmt.Println(err)
		}
		fmt.Println(string(bodyData))
		return ListFilesResponse{}
	}

	// decode the json data into our struct
	var data ListFilesResponse
	err = json.NewDecoder(response.Body).Decode(&data)
	if err != nil {
		fmt.Println(err)
	}

	return data
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
