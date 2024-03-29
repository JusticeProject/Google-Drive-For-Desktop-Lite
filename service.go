package main

import (
	"bufio"
	"crypto/md5"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

//*************************************************************************************************
//*************************************************************************************************

type GoogleDriveService struct {
	conn        GoogleDriveConnection
	baseFolders map[string]string // key = local folder name, value = folder id on Google Drive

	localFiles map[string]bool

	filesToUpload     map[string]bool
	filesToDownload   map[string]FileMetaData
	uploadLookupMap   map[string]FileMetaData
	downloadLookupMap map[string]FileMetaData // key = path + filename, value = metadata

	verifiedAt              time.Time // if anything is newer than the verifiedAt timestamp, then we will upload/download
	verifiedAtPlusOneSec    time.Time
	mostRecentTimestampSeen time.Time // when successfully verified, the most recent timestamp seen will be set to verifiedAt

	cleanedAt time.Time
}

//*************************************************************************************************
//*************************************************************************************************

const LARGE_FILE_THRESHOLD_BYTES int64 = 5 * 1024 * 1024

//*************************************************************************************************
//*************************************************************************************************

func (service *GoogleDriveService) initializeService() {
	service.conn.initializeGoogleDrive()

	// read our config file that tells us the folder id for each shared folder
	fh, err := os.Open("config/folder-ids.txt")
	if err != nil {
		log.Fatal("failed to read folder IDs")
	}
	defer fh.Close()

	// get the id number for each main folder that is shared, save it for later
	service.baseFolders = make(map[string]string)
	scanner := bufio.NewScanner(fh)
	for scanner.Scan() {
		line := scanner.Text()
		line_split := strings.SplitN(line, "=", 2)
		service.baseFolders[line_split[0]] = line_split[1]
	}

	fmt.Println("these are our starting baseFolders:", service.baseFolders)

	service.localFiles = make(map[string]bool)
	service.filesToUpload = make(map[string]bool)
	service.filesToDownload = make(map[string]FileMetaData)
	service.uploadLookupMap = make(map[string]FileMetaData)
	service.downloadLookupMap = make(map[string]FileMetaData)
}

//*************************************************************************************************
//*************************************************************************************************

func (service *GoogleDriveService) resetVerifiedTime() {
	service.verifiedAt = time.Date(2000, time.January, 1, 12, 0, 0, 0, time.UTC)
	service.verifiedAtPlusOneSec = service.verifiedAt
}

//*************************************************************************************************
//*************************************************************************************************

func (service *GoogleDriveService) setVerifiedTime() {
	service.verifiedAt = service.mostRecentTimestampSeen
	service.verifiedAtPlusOneSec = service.verifiedAt.Add(time.Second)
}

//*************************************************************************************************
//*************************************************************************************************

func (service *GoogleDriveService) hoursSinceLastClean() float64 {
	now := time.Now()
	diff := now.Sub(service.cleanedAt)
	return diff.Hours()
}

//*************************************************************************************************
//*************************************************************************************************

func (service *GoogleDriveService) setCleanTime(cleaningAt time.Time) {
	service.cleanedAt = cleaningAt
}

//*************************************************************************************************
//*************************************************************************************************

func (service *GoogleDriveService) saveTimestamp(timestamp time.Time) {
	// always keep the newest timestamp
	diff := timestamp.Sub(service.mostRecentTimestampSeen)
	if diff > 0 {
		service.mostRecentTimestampSeen = timestamp
	}
}

//*************************************************************************************************
//*************************************************************************************************

func (service *GoogleDriveService) fillLocalMap() {
	// use a closure so the walk function has access to localFiles

	var walkFunc = func(path string, fileInfo os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		service.localFiles[path] = true
		return nil
	}

	for folder := range service.baseFolders {
		filepath.Walk(folder, walkFunc)
	}
}

//*************************************************************************************************
//*************************************************************************************************

func (service *GoogleDriveService) getBaseFolderSlice() []string {
	keys := make([]string, len(service.baseFolders))

	i := 0
	for k := range service.baseFolders {
		keys[i] = k
		i++
	}

	return keys
}

//*************************************************************************************************
//*************************************************************************************************

func (service *GoogleDriveService) fillLookupMap(localToRemoteLookup map[string]FileMetaData, localFolders []string) error {
	for _, localFolder := range localFolders {
		var folderId string

		// if localFolder is a base folder and not in the lookupMap, then add it
		baseId, isBaseFolder := service.baseFolders[localFolder]
		remoteMetaData, inLookupMap := localToRemoteLookup[localFolder]
		if isBaseFolder && !inLookupMap {
			localToRemoteLookup[localFolder] = FileMetaData{ID: baseId}
			folderId = baseId
		} else if inLookupMap {
			folderId = remoteMetaData.ID
		}

		data, err := service.conn.getItemsInSharedFolder(localFolder, folderId)
		if err != nil {
			return err
		}

		// add the files and folders to our map
		for _, file := range data.Files {
			localToRemoteLookup[filepath.Join(localFolder, file.Name)] = file
		}

		// if any are folders then we will need to look up their contents as well, call this same function recursively
		for _, file := range data.Files {
			if strings.Contains(file.MimeType, "folder") {
				foldersToLookup := []string{filepath.Join(localFolder, file.Name)}
				err = service.fillLookupMap(localToRemoteLookup, foldersToLookup)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

//*************************************************************************************************
//*************************************************************************************************

func (service *GoogleDriveService) clearUploadLookupMap() {
	if len(service.uploadLookupMap) > 0 {
		service.uploadLookupMap = make(map[string]FileMetaData)
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

func (service *GoogleDriveService) fillUploadLookupMap(localFolders []string) error {
	for _, localFolder := range localFolders {

		// check if this localFolder is in the path of any of the filesToUpload
		if !localPathIsNeeded(localFolder, service.filesToUpload) {
			continue
		}

		var folderId string

		// if localFolder is a base folder and not in the lookupMap, then add it
		baseId, isBaseFolder := service.baseFolders[localFolder]
		remoteMetaData, inLookupMap := service.uploadLookupMap[localFolder]
		if isBaseFolder && !inLookupMap {
			service.uploadLookupMap[localFolder] = FileMetaData{ID: baseId}
			folderId = baseId
		} else if inLookupMap {
			folderId = remoteMetaData.ID
		}

		data, err := service.conn.getItemsInSharedFolder(localFolder, folderId)
		if err != nil {
			return err
		}

		// add the files and folders to our map
		for _, file := range data.Files {
			service.uploadLookupMap[filepath.Join(localFolder, file.Name)] = file
		}

		// if any are folders then we will need to look up their contents as well, call this same function recursively
		for _, file := range data.Files {
			if strings.Contains(file.MimeType, "folder") {
				foldersToLookup := []string{filepath.Join(localFolder, file.Name)}
				err = service.fillUploadLookupMap(foldersToLookup)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

//*************************************************************************************************
//*************************************************************************************************

func (service *GoogleDriveService) clearDownloadLookupMap() {
	if len(service.downloadLookupMap) > 0 {
		service.downloadLookupMap = make(map[string]FileMetaData)
	}
}

//*************************************************************************************************
//*************************************************************************************************

func (service *GoogleDriveService) fillDownloadLookupMap(remoteModifiedFiles []FileMetaData, doExtraFolderSearch bool) error {
	tempIdToMetaData := make(map[string]FileMetaData) // key = id, value = metadata

	// add the known base folders to the temp map and download lookup map
	for folderName, id := range service.baseFolders {
		tempIdToMetaData[id] = FileMetaData{ID: id}
		service.downloadLookupMap[folderName] = FileMetaData{ID: id}
	}

	// add all the modified files/folders to our temp map, and the parents if necessary
	for _, remoteMetaData := range remoteModifiedFiles {
		tempIdToMetaData[remoteMetaData.ID] = remoteMetaData

		if doExtraFolderSearch && strings.Contains(remoteMetaData.MimeType, "folder") {
			response, err := service.conn.getItemsInSharedFolder(remoteMetaData.Name, remoteMetaData.ID)
			if err != nil {
				return err
			}
			for _, metadata := range response.Files {
				tempIdToMetaData[metadata.ID] = metadata
			}
		}

		// add all the parents recursively
		// if it fails then return an error from this function so we can try again next time, don't want to download the wrong paths
		err := service.addParents(remoteMetaData, tempIdToMetaData)
		if err != nil {
			return err
		}
	}

	// now piece together all the modified items by using the parent ids to create the file hierarchy
	for id, metadata := range tempIdToMetaData {
		fullPath, err := service.getFullPath(id, tempIdToMetaData)

		// for deleted files the path might be "" with an error, we won't add those to the lookup map
		if fullPath != "" && err == nil {
			service.downloadLookupMap[fullPath] = metadata
		}
	}

	return nil
}

//***********************************************

func (service *GoogleDriveService) addParents(metadata FileMetaData, tempIdToMetaData map[string]FileMetaData) error {
	if len(metadata.Parents) > 0 {
		parentId := metadata.Parents[0]
		_, parentInMap := tempIdToMetaData[parentId]

		if parentId != "" && !parentInMap {
			parentMetadata, err := service.conn.getMetadataById("?", parentId)
			if err != nil {
				return err
			}
			tempIdToMetaData[parentMetadata.ID] = parentMetadata
			err = service.addParents(parentMetadata, tempIdToMetaData)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

//***********************************************

func (service *GoogleDriveService) getFullPath(id string, tempIdToMetaData map[string]FileMetaData) (string, error) {
	metadata, inMap := tempIdToMetaData[id]

	if inMap {
		if len(metadata.Parents) > 0 {
			parentPath, err := service.getFullPath(metadata.Parents[0], tempIdToMetaData)
			if err != nil {
				return "", err
			}

			if parentPath == "" {
				return "", errors.New("something went wrong when trying to getFullPath")
			} else {
				fullPath := parentPath + string(filepath.Separator) + metadata.Name
				return fullPath, nil
			}
		} else {
			// check if this is a base folder
			for baseFolderName, baseFolderId := range service.baseFolders {
				if id == baseFolderId {
					return baseFolderName, nil
				}
			}
			msg := fmt.Sprintln("no base folder found for file:", metadata.Name, "id:", id)
			return "", errors.New(msg)
		}
	}
	return "", errors.New("id was not found")
}

//*************************************************************************************************
//*************************************************************************************************

func getMd5OfFile(path string) string {
	fh, err := os.Open(path)
	if err != nil {
		fmt.Println("could not open file for md5", err)
		return ""
	}
	defer fh.Close()

	result := md5.New()
	if _, err := io.Copy(result, fh); err != nil {
		fmt.Println("could could copy data from file for md5", err)
		return ""
	}

	result_string := fmt.Sprintf("%x", result.Sum(nil))
	return result_string
}

//*************************************************************************************************
//*************************************************************************************************

func (service *GoogleDriveService) localFilesModified() bool {
	// use a closure to give the walk function access to filesToUpload and localFiles

	// this is the callback function that Walk will call for each local file/folder
	var walkAndCheckForModified = func(path string, fileInfo os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// ignore the desktop.ini files
		if fileInfo.Name() == "desktop.ini" {
			return nil
		}

		modifiedAt := fileInfo.ModTime()

		// if file shows up locally that was not there before
		_, inLocalMap := service.localFiles[path]
		if !inLocalMap {
			if debug {
				fmt.Println(path, "suddenly appeared")
			}
			service.filesToUpload[path] = true
			service.localFiles[path] = true
			service.saveTimestamp(modifiedAt)
			return nil
		}

		timestampDiff := modifiedAt.Sub(service.verifiedAt)
		if timestampDiff > 0 {
			if debug {
				fmt.Println(path, "has changed")
			}
			service.filesToUpload[path] = true
			service.saveTimestamp(modifiedAt)
			return nil
		}

		return nil
	}

	// do the walking
	for folder := range service.baseFolders {
		filepath.Walk(folder, walkAndCheckForModified)
	}

	return len(service.filesToUpload) > 0
}

//*************************************************************************************************
//*************************************************************************************************

func (service *GoogleDriveService) getRemoteModifiedFiles() ([]FileMetaData, error) {
	// rate limits are:
	// Queries per 100 seconds	20,000
	// Queries per day	1,000,000,000

	if debug {
		fmt.Println("checking if remote side was modified")
	}

	timestamp := service.verifiedAtPlusOneSec.UTC().Format(time.RFC3339)
	files, err := service.conn.getModifiedItems(timestamp)
	if err != nil {
		return []FileMetaData{}, err
	}

	if debug {
		fmt.Println(len(files), "files were modified")
		fmt.Println(files)
	}

	// save the newest timestamp that we see
	for _, file := range files {
		modifiedAt, err := time.Parse(time.RFC3339Nano, file.ModifiedTime)
		if err == nil {
			service.saveTimestamp(modifiedAt)
		}
	}

	return files, nil
}

//*************************************************************************************************
//*************************************************************************************************

func (service *GoogleDriveService) checkForDownloads() {
	for localPath, remoteFileInfo := range service.downloadLookupMap {
		// first check if it already exists
		localFileInfo, err := os.Stat(localPath)
		if err != nil {
			// doesn't exist on local side, add to download list
			service.filesToDownload[localPath] = remoteFileInfo
		} else {
			// it does exist locally

			// if folder then don't need to download
			if localFileInfo.IsDir() {
				delete(service.filesToDownload, localPath)
				continue
			}

			// it's a file, but check if the remote file is newer
			localModTime := localFileInfo.ModTime()
			remoteModTime, _ := time.Parse(time.RFC3339Nano, remoteFileInfo.ModifiedTime)
			diff := remoteModTime.Sub(localModTime)

			// allow for some floating point roundoff error
			if diff.Seconds() > 0.5 {
				// the remote file is newer
				localMD5 := getMd5OfFile(localPath)
				if localMD5 != remoteFileInfo.Md5Checksum {
					service.filesToDownload[localPath] = remoteFileInfo
				} else {
					delete(service.filesToDownload, localPath)
				}
			} else {
				delete(service.filesToDownload, localPath)
			}
		}
	}
}

//*************************************************************************************************
//*************************************************************************************************

func (service *GoogleDriveService) handleDownloads() bool {
	somethingWasDownloaded := false

	// need to do the folders first, start with the shortest path length
	var foldersToCreate []string
	for localPath := range service.filesToDownload {
		remoteFileInfo := service.filesToDownload[localPath]
		if strings.Contains(remoteFileInfo.MimeType, "folder") {
			foldersToCreate = append(foldersToCreate, localPath)
		}
	}
	sort.Strings(foldersToCreate)

	for _, localPath := range foldersToCreate {
		err := os.Mkdir(localPath, 0766)
		if err == nil {
			service.localFiles[localPath] = true // save this so we aren't surprised later that a new folder appeared
			somethingWasDownloaded = true
			if debug {
				fmt.Println("created local folder", localPath)
			}
		} else {
			fmt.Println(err)
		}
	}

	// download the files after the folders have been created
	for localPath := range service.filesToDownload {
		remoteFileInfo := service.filesToDownload[localPath]

		// if it's a file
		if !strings.Contains(remoteFileInfo.MimeType, "folder") {
			err := service.conn.downloadFile(remoteFileInfo.ID, localPath)
			if err == nil {
				service.localFiles[localPath] = true // save this so we aren't surprised later that a new file appeared
				somethingWasDownloaded = true

				modTime, _ := time.Parse(time.RFC3339Nano, remoteFileInfo.ModifiedTime)
				err := os.Chtimes(localPath, modTime, modTime)
				if err != nil {
					fmt.Println(err)
				}
			}
		}
	}

	return somethingWasDownloaded
}

//*************************************************************************************************
//*************************************************************************************************

func (service *GoogleDriveService) handleCreate(localPath string, localFileInfo fs.FileInfo) error {
	ids, err := service.conn.generateIds(1)
	if len(ids) != 1 || err != nil {
		fmt.Println("failed to get ids for new file:", localPath, "err:", err)
		return errors.New("failed to generate id") // we'll try again next time
	}

	parentPath := filepath.Dir(localPath)
	parentId, parentInMap := service.uploadLookupMap[parentPath]
	if !parentInMap {
		// if parent folder is not on remote side yet just skip the file for now, we'll handle it on the next loop
		if debug {
			fmt.Println("parent not in map yet")
		}
		return errors.New("parent not in map yet")
	}
	parents := []string{parentId.ID}

	formattedTime := localFileInfo.ModTime().Format(time.RFC3339Nano)

	if localFileInfo.IsDir() {
		request := CreateFolderRequest{ID: ids[0], Name: localFileInfo.Name(), MimeType: "application/vnd.google-apps.folder", Parents: parents, ModifiedTime: formattedTime}
		err := service.conn.createRemoteFolder(request)
		if err != nil {
			return err
		} else {
			service.uploadLookupMap[localPath] = FileMetaData{ID: ids[0], Name: localFileInfo.Name(), MimeType: "application/vnd.google-apps.folder", Md5Checksum: ""}
		}
	} else {
		request := CreateFileRequest{ID: ids[0], Name: localFileInfo.Name(), Parents: parents, ModifiedTime: formattedTime}

		if localFileInfo.Size() > LARGE_FILE_THRESHOLD_BYTES {
			fh, err := os.Open(localPath)
			if err != nil {
				return err
			}
			err = service.conn.uploadLargeFile(request.ID, &request, fh, localFileInfo.Size())
			if err != nil {
				return err
			}
		} else {
			fileData, err := os.ReadFile(localPath)
			if err != nil {
				return err
			}
			err = service.conn.uploadFile(request.ID, &request, fileData)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

//*************************************************************************************************
//*************************************************************************************************

func (service *GoogleDriveService) handleSingleUpload(localPath string, modifiedTime time.Time, fileLength int64) error {
	fileMetaData := service.uploadLookupMap[localPath]

	formattedTime := modifiedTime.Format(time.RFC3339Nano)
	request := UpdateFileRequest{ModifiedTime: formattedTime}

	if fileLength > LARGE_FILE_THRESHOLD_BYTES {
		fh, err := os.Open(localPath)
		if err != nil {
			return err
		}
		err = service.conn.uploadLargeFile(fileMetaData.ID, &request, fh, fileLength)
		if err != nil {
			return err
		}
	} else {
		data, err := os.ReadFile(localPath)
		if err != nil {
			return err
		}
		err = service.conn.uploadFile(fileMetaData.ID, &request, data)
		if err != nil {
			return err
		}
	}

	return nil
}

//*************************************************************************************************
//*************************************************************************************************

func (service *GoogleDriveService) handleUploads() error {
	allLocalFileInfo := make(map[string]os.FileInfo)

	// need to do the folders first, start by collecting the folders and sorting them by the shortest path length
	var foldersToCreate []string
	for localPath := range service.filesToUpload {
		localFileInfo, err := os.Stat(localPath)
		if err == nil {
			allLocalFileInfo[localPath] = localFileInfo
		} else {
			// it must have been removed after we detected it but before we could upload it
			delete(service.filesToUpload, localPath)
			delete(service.localFiles, localPath)
			continue
		}

		if localFileInfo.IsDir() {
			foldersToCreate = append(foldersToCreate, localPath)
		}
	}
	sort.Strings(foldersToCreate)

	// create the folders
	for _, localPath := range foldersToCreate {
		_, existsOnServer := service.uploadLookupMap[localPath]
		if !existsOnServer {
			if debug {
				fmt.Println(localPath, "does not exist on server")
			}
			localFileInfo := allLocalFileInfo[localPath]
			err := service.handleCreate(localPath, localFileInfo)
			if err != nil {
				return err
			}
		}
	}

	// now handle the files
	for localPath := range service.filesToUpload {
		// get local fileInfo
		localFileInfo := allLocalFileInfo[localPath]
		if localFileInfo.IsDir() {
			continue // we already handled the folders
		}

		remoteFileData, existsOnServer := service.uploadLookupMap[localPath]
		if !existsOnServer {
			if debug {
				fmt.Println(localPath, "does not exist on server")
			}

			// create file
			err := service.handleCreate(localPath, localFileInfo)
			if err != nil {
				return err
			}
		} else {
			localModTime := localFileInfo.ModTime()
			remoteModTime, _ := time.Parse(time.RFC3339Nano, remoteFileData.ModifiedTime)
			diff := localModTime.Sub(remoteModTime)
			if debug {
				fmt.Println(localFileInfo.Name(), "local mod time is newer by", diff.Seconds(), "seconds")
			}

			// if the local file is newer, then calculate the md5's
			// allow for some floating point roundoff error
			if diff.Seconds() > 0.5 {
				localMd5 := getMd5OfFile(localPath)

				if localMd5 != remoteFileData.Md5Checksum {
					if debug {
						fmt.Println("md5's do not match", localMd5, remoteFileData.Md5Checksum)
						fmt.Println("local mod time is newer", localModTime, remoteModTime)
					}
					err := service.handleSingleUpload(localPath, localFileInfo.ModTime(), localFileInfo.Size())
					if err != nil {
						return err
					}
				}
			}
		}
	}

	return nil
}

//*************************************************************************************************
//*************************************************************************************************

func (service *GoogleDriveService) verifyUploads() {
	for localPath := range service.filesToUpload {

		localFileInfo, err := os.Stat(localPath)
		if err != nil {
			fmt.Println("error from Stat", err)
			delete(service.filesToUpload, localPath)
			continue
		}
		remoteFileData, onServer := service.uploadLookupMap[localPath]

		if !onServer {
			if debug {
				fmt.Println(localPath, "not on server")
			}
			continue
		}

		// if we got this far it is on the server
		if localFileInfo.IsDir() {
			delete(service.filesToUpload, localPath)
		} else {
			localMd5 := getMd5OfFile(localPath)
			if localMd5 == remoteFileData.Md5Checksum {
				delete(service.filesToUpload, localPath)
			} else {
				if debug {
					fmt.Println("md5 did not match for", localPath)
				}
			}
		}
	}
}

//*************************************************************************************************
//*************************************************************************************************

func (service *GoogleDriveService) verifyDownloads() {
	// according to the go spec, deleting keys while iterating over the map is allowed:
	// https://go.dev/ref/spec#For_statements
	for localPath := range service.filesToDownload {
		remoteFileData := service.downloadLookupMap[localPath]

		if strings.Contains(remoteFileData.MimeType, "folder") {
			// it's a folder
			folderInfo, err := os.Stat(localPath)
			if err == nil && folderInfo.IsDir() {
				delete(service.filesToDownload, localPath)
			}
		} else {
			// it's a file
			localMd5 := getMd5OfFile(localPath)

			if localMd5 == remoteFileData.Md5Checksum {
				delete(service.filesToDownload, localPath)
			}
		}
	}
}
