package main

import (
	"crypto/md5"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

//*************************************************************************************************
//*************************************************************************************************

const MAX_UPLOAD_BYTES int64 = 5 * 1024 * 1024

var debug bool = false

var conn GoogleDriveConnection

var localFiles map[string]bool = make(map[string]bool)
var localToRemoteLookup map[string]FileMetaData = make(map[string]FileMetaData) // key=local file name

var verifiedAt time.Time = time.Date(2000, time.January, 1, 12, 0, 0, 0, time.UTC)
var verifiedAtPlusOneSec time.Time = verifiedAt
var filesToUpload map[string]bool = make(map[string]bool)
var filesToDownload map[string]FileMetaData = make(map[string]FileMetaData)

//*************************************************************************************************
//*************************************************************************************************

func getMd5OfFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Println("could not read file for md5", err)
		return ""
	}
	result := md5.Sum(data)
	result_string := fmt.Sprintf("%x", result)
	return result_string
}

//*************************************************************************************************
//*************************************************************************************************

func walkAndFillLocal(path string, fileInfo os.FileInfo, err error) error {
	if err != nil {
		return err
	}

	localFiles[path] = true
	return nil
}

//*************************************************************************************************
//*************************************************************************************************

func fillLocalMap() {
	for folder := range conn.baseFolders {
		filepath.Walk(folder, walkAndFillLocal)
	}
}

//*************************************************************************************************
//*************************************************************************************************

func wipeServiceAcctFiles() {
	//conn.listFolderById()
	filesToDelete := conn.listFilesOwnedByServiceAcct()
	for _, item := range filesToDelete {
		conn.deleteFileOrFolder(item)
	}
}

//*************************************************************************************************
//*************************************************************************************************

func walkAndCheckForModified(path string, fileInfo os.FileInfo, err error) error {
	if err != nil {
		return err
	}

	// ignore the desktop.ini files
	if fileInfo.Name() == "desktop.ini" {
		return nil
	}

	// ignore files that are too big to upload (for now)
	if fileInfo.Size() > MAX_UPLOAD_BYTES {
		return nil
	}

	// if file shows up locally that was not there before
	_, inLocalMap := localFiles[path]
	if !inLocalMap {
		DebugLog(path, "suddenly appeared")
		filesToUpload[path] = true
		localFiles[path] = true
		return nil
	}

	modifiedAt := fileInfo.ModTime()
	diff := modifiedAt.Sub(verifiedAt)

	if diff > 0 {
		DebugLog(path, "has changed")
		filesToUpload[path] = true
		return nil
	}

	return nil
}

//*************************************************************************************************
//*************************************************************************************************

func remoteSideWasModified() bool {
	// rate limits are:
	//  Queries per 100 seconds	20,000
	// Queries per day	1,000,000,000

	DebugLog("checking if remote side was modified")

	timestamp := verifiedAtPlusOneSec.UTC().Format(time.RFC3339)
	files := conn.getModifiedItems(timestamp)
	return len(files) > 0
}

//*************************************************************************************************
//*************************************************************************************************

func checkForDownloads() {
	for localPath := range localToRemoteLookup {
		remoteFileInfo := localToRemoteLookup[localPath]

		// first check if it already exists
		localFileInfo, err := os.Stat(localPath)
		if err != nil {
			// doesn't exist on local side, add to download list
			filesToDownload[localPath] = remoteFileInfo
		} else {
			// it does exist locally

			// if folder then don't need to download
			if localFileInfo.IsDir() {
				delete(filesToDownload, localPath)
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
					filesToDownload[localPath] = remoteFileInfo
				} else {
					delete(filesToDownload, localPath)
				}
			} else {
				delete(filesToDownload, localPath)
			}
		}
	}
}

//*************************************************************************************************
//*************************************************************************************************

func handleDownloads() {
	// need to do the folders first, start with the shortest path length
	var foldersToCreate []string
	for localPath := range filesToDownload {
		remoteFileInfo := filesToDownload[localPath]
		if strings.Contains(remoteFileInfo.MimeType, "folder") {
			foldersToCreate = append(foldersToCreate, localPath)
		}
	}
	sort.Strings(foldersToCreate)

	for _, localPath := range foldersToCreate {
		err := os.Mkdir(localPath, os.ModeDir)
		if err == nil {
			localFiles[localPath] = true // save this so we aren't surprised later that a new folder appeared
			DebugLog("created local folder", localPath)
		} else {
			fmt.Println(err)
		}
	}

	// download the files after the folders have been created
	for localPath := range filesToDownload {
		remoteFileInfo := filesToDownload[localPath]

		// if it's a file
		if !strings.Contains(remoteFileInfo.MimeType, "folder") {
			err := conn.downloadFile(remoteFileInfo.ID, localPath)
			if err == nil {
				localFiles[localPath] = true // save this so we aren't surprised later that a new file appeared

				modTime, _ := time.Parse(time.RFC3339Nano, remoteFileInfo.ModifiedTime)
				err := os.Chtimes(localPath, modTime, modTime)
				if err != nil {
					fmt.Println(err)
				}
			}
		}
	}
}

//*************************************************************************************************
//*************************************************************************************************

func handleCreate(localPath string, isDir bool, fileName string, modifiedTime time.Time) error {
	ids := conn.generateIds(1)
	if len(ids) != 1 {
		fmt.Println("failed to get ids for new file", localPath)
		return errors.New("failed to generate id") // we'll try again next time
	}

	parentPath := filepath.Dir(localPath)
	parentId, parentInMap := localToRemoteLookup[parentPath]
	if !parentInMap {
		// if parent folder is not on remote side yet just skip the file for now, we'll handle it on the next loop
		DebugLog("parent not in map yet")
		return errors.New("parent not in map yet")
	}
	parents := []string{parentId.ID}

	if isDir {
		request := CreateFolderRequest{ID: ids[0], Name: fileName, MimeType: "application/vnd.google-apps.folder", Parents: parents}
		err := conn.createRemoteFolder(request)
		if err != nil {
			fmt.Println(err)
		} else {
			localToRemoteLookup[localPath] = FileMetaData{ID: ids[0], Name: fileName, MimeType: "application/vnd.google-apps.folder", Md5Checksum: ""}
		}
	} else {
		formattedTime := modifiedTime.Format(time.RFC3339Nano)
		request := CreateFileRequest{ID: ids[0], Name: fileName, Parents: parents, ModifiedTime: formattedTime}
		fileData, _ := os.ReadFile(localPath)
		err := conn.createRemoteFile(request, fileData)
		if err != nil {
			fmt.Println(err)
		}
	}

	return nil
}

//*************************************************************************************************
//*************************************************************************************************

func handleSingleUpload(localPath string, modifiedTime time.Time) {
	fileMetaData := localToRemoteLookup[localPath]

	data, err := os.ReadFile(localPath)
	if err != nil {
		fmt.Println(err)
	} else {
		formattedTime := modifiedTime.Format(time.RFC3339Nano)
		conn.updateFileAndMetadata(fileMetaData.ID, formattedTime, data)
	}
}

//*************************************************************************************************
//*************************************************************************************************

func handleUploads() {
	allLocalFileInfo := make(map[string]os.FileInfo)

	// need to do the folders first, start by collecting the folders and sorting them by the shortest path length
	var foldersToCreate []string
	for localPath := range filesToUpload {
		localFileInfo, err := os.Stat(localPath)
		if err == nil {
			allLocalFileInfo[localPath] = localFileInfo
		} else {
			// it must have been removed after we detected it but before we could upload it
			delete(filesToUpload, localPath)
			delete(localFiles, localPath)
			continue
		}

		if localFileInfo.IsDir() {
			foldersToCreate = append(foldersToCreate, localPath)
		}
	}
	sort.Strings(foldersToCreate)

	// create the folders
	for _, localPath := range foldersToCreate {
		_, existsOnServer := localToRemoteLookup[localPath]
		if !existsOnServer {
			DebugLog(localPath, "does not exist on server")
			folderName := filepath.Base(localPath)
			localFileData := allLocalFileInfo[localPath]
			err := handleCreate(localPath, true, folderName, localFileData.ModTime())
			if err != nil {
				fmt.Println(err)
			}
		}
	}

	// now handle the files
	for localPath := range filesToUpload {
		// get local fileInfo
		localFileInfo := allLocalFileInfo[localPath]
		if localFileInfo.IsDir() {
			continue // we already handled the folders
		}

		remoteFileData, existsOnServer := localToRemoteLookup[localPath]
		if !existsOnServer {
			DebugLog(localPath, "does not exist on server")

			// create file
			err := handleCreate(localPath, localFileInfo.IsDir(), localFileInfo.Name(), localFileInfo.ModTime())
			if err != nil {
				fmt.Println(err)
			}
		} else {
			localModTime := localFileInfo.ModTime()
			remoteModTime, _ := time.Parse(time.RFC3339Nano, remoteFileData.ModifiedTime)
			diff := localModTime.Sub(remoteModTime)
			DebugLog("local mod time is newer by", diff.Seconds(), "seconds")

			// if the local file is newer, then calculate the md5's
			// allow for some floating point roundoff error
			if diff.Seconds() > 0.5 {
				localMd5 := getMd5OfFile(localPath)

				if localMd5 != remoteFileData.Md5Checksum {
					DebugLog("md5's do not match", localMd5, remoteFileData.Md5Checksum)
					DebugLog("local mod time is newer", localModTime, remoteModTime)
					handleSingleUpload(localPath, localFileInfo.ModTime())
				}
			}
		}
	}
}

//*************************************************************************************************
//*************************************************************************************************

func verifyUploads() {
	for localPath := range filesToUpload {

		localFileInfo, err := os.Stat(localPath)
		if err != nil {
			fmt.Println("error from Stat", err)
			delete(filesToUpload, localPath)
			continue
		}
		remoteFileData, onServer := localToRemoteLookup[localPath]

		if !onServer {
			DebugLog(localPath, "not on server")
			continue
		}

		// if we got this far it is on the server
		if localFileInfo.IsDir() {
			delete(filesToUpload, localPath)
		} else {
			localMd5 := getMd5OfFile(localPath)
			if localMd5 == remoteFileData.Md5Checksum {
				delete(filesToUpload, localPath)
			} else {
				DebugLog("md5 did not match for", localPath)
			}
		}
	}
}

//*************************************************************************************************
//*************************************************************************************************

func verifyDownloads() {
	// according to the go spec, deleting keys while iterating over the map is allowed:
	// https://go.dev/ref/spec#For_statements
	for localPath := range filesToDownload {
		remoteFileData := localToRemoteLookup[localPath]

		if strings.Contains(remoteFileData.MimeType, "folder") {
			// it's a folder
			folderInfo, err := os.Stat(localPath)
			if err == nil && folderInfo.IsDir() {
				delete(filesToDownload, localPath)
			}
		} else {
			// it's a file
			localMd5 := getMd5OfFile(localPath)

			if localMd5 == remoteFileData.Md5Checksum {
				delete(filesToDownload, localPath)
			}
		}
	}
}

//*************************************************************************************************
//*************************************************************************************************

func main() {
	// check if we need to print debug statements
	if len(os.Args) > 1 {
		debug = true
	}

	conn.initializeGoogleDrive()
	fillLocalMap()
	//wipeServiceAcctFiles()

	for {
		conn.clearLookupMap()

		// check if we need to upload anything
		DebugLog("Checking for any new or modified local files/folders")
		for folder := range conn.baseFolders {
			filepath.Walk(folder, walkAndCheckForModified)
		}

		willCheckForDownloads := remoteSideWasModified()

		// check if we need to refresh the lookup map
		if willCheckForDownloads || len(filesToUpload) > 0 {
			// grab all the metadata for the files/folders that are currently on the remote shared drive
			// because we need the ids of files/folders, timestamps, md5's, etc.
			conn.clearLookupMap()
			conn.fillLookupMap(conn.getBaseFolderSlice())
		}

		// check if we need to download anything
		if willCheckForDownloads {
			checkForDownloads()
		}

		// do the upload
		if len(filesToUpload) > 0 {
			DebugLog("Preparing to upload files")
			handleUploads()
		}

		// do the download
		if len(filesToDownload) > 0 {
			DebugLog("Preparing to download files")
			handleDownloads()
		}

		// do a verify if we uploaded or downloaded anything
		if len(filesToUpload) > 0 || len(filesToDownload) > 0 {
			DebugLog("Verifying. Grabbing remote metadata first.")
			// again grab all the metadata for the files/folders that are currently on the remote shared drive
			conn.clearLookupMap()
			conn.fillLookupMap(conn.getBaseFolderSlice())

			verifyingAt := time.Now()

			// verify local files were uploaded to the remote server
			verifyUploads()

			// verify remote files were downloaded to the local side
			verifyDownloads()

			if len(filesToUpload) == 0 && len(filesToDownload) == 0 {
				DebugLog("verified! updating new verified timestamp to", verifyingAt)
				verifiedAt = verifyingAt
				verifiedAtPlusOneSec = verifiedAt.Add(time.Second)
				conn.clearLookupMap()
			} else {
				DebugLog("not verified, will try again next time")
			}
		}

		// TODO: slow it down a bit
		time.Sleep(20 * time.Second)
	}
}
