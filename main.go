package main

import (
	"crypto/md5"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

//*************************************************************************************************
//*************************************************************************************************

var debug bool = false

var conn GoogleDriveConnection

var localFiles map[string]bool = make(map[string]bool)
var localToRemoteLookup map[string]FileMetaData = make(map[string]FileMetaData)

var verifiedAt time.Time
var filesToUpload map[string]bool = make(map[string]bool)

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
	fixedPath := strings.ReplaceAll(path, `\`, "/")
	localFiles[fixedPath] = true
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

func walkAndCheckForModified(path string, fileInfo os.FileInfo, err error) error {
	// ignore the desktop.ini files
	if fileInfo.Name() == "desktop.ini" {
		return nil
	}

	fixedPath := strings.ReplaceAll(path, `\`, "/")

	// if file shows up locally that was not there before
	_, inLocalMap := localFiles[fixedPath]
	if !inLocalMap {
		DebugLog(fixedPath, "suddenly appeared")
		filesToUpload[fixedPath] = true
		localFiles[fixedPath] = true
		return nil
	}

	modifiedAt := fileInfo.ModTime()
	diff := modifiedAt.Sub(verifiedAt)

	if diff > 0 {
		DebugLog(fixedPath, "has changed")
		filesToUpload[fixedPath] = true
		return nil
	}

	return nil
}

//*************************************************************************************************
//*************************************************************************************************

func handleCreate(localPath string, isDir bool, fileName string, modifiedTime time.Time) error {
	ids := conn.generateIds(1)
	if len(ids) != 1 {
		fmt.Println("failed to get ids for new file", localPath)
		return errors.New("failed to generate id") // we'll try again next time
	}

	parentPath := strings.TrimSuffix(localPath, "/"+fileName)
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

func handleUpload(localPath string, modifiedTime time.Time) {
	fileMetaData := localToRemoteLookup[localPath]

	data, err := os.ReadFile(localPath)
	if err != nil {
		fmt.Println(err)
	} else {
		formattedTime := modifiedTime.Format(time.RFC3339)
		conn.updateFileAndMetadata(fileMetaData.ID, formattedTime, data)
	}
}

//*************************************************************************************************
//*************************************************************************************************

func walkAndUpload(path string, fileInfo os.FileInfo, err error) error {
	// ignore the desktop.ini files
	if fileInfo.Name() == "desktop.ini" {
		return nil
	}

	fixedPath := strings.ReplaceAll(path, `\`, "/")
	_, markedForUpload := filesToUpload[fixedPath]
	fileData, existsOnServer := localToRemoteLookup[fixedPath]
	if markedForUpload {
		if !existsOnServer {
			DebugLog(fixedPath, "does not exist on server")
			// create file/folder
			err := handleCreate(fixedPath, fileInfo.IsDir(), fileInfo.Name(), fileInfo.ModTime())
			if err != nil {
				return nil
			}
		} else if !fileInfo.IsDir() {
			localMd5 := getMd5OfFile(fixedPath)
			md5mismatch := localMd5 != fileData.Md5Checksum

			localModTime := fileInfo.ModTime()
			remoteModTime, _ := time.Parse(time.RFC3339Nano, fileData.ModifiedTime)
			diff := localModTime.Sub(remoteModTime)
			DebugLog("local mod time is newer by", diff.Seconds(), "seconds")
			localIsNewer := diff > 0

			if md5mismatch && localIsNewer {
				DebugLog("md5's do not match", localMd5, fileData.Md5Checksum)
				DebugLog("local mod time is newer", localModTime, remoteModTime)
				handleUpload(fixedPath, fileInfo.ModTime())
			}
		}
	}

	return nil
}

//*************************************************************************************************
//*************************************************************************************************

func walkAndVerify(path string, fileInfo os.FileInfo, err error) error {
	fixedPath := strings.ReplaceAll(path, `\`, "/")

	fileData, onServer := localToRemoteLookup[fixedPath]

	if !onServer {
		DebugLog(fixedPath, "not on server")
		filesToUpload[fixedPath] = true
		return nil
	}

	// if we got this far it is on the server
	if fileInfo.IsDir() {
		delete(filesToUpload, fixedPath)
	} else {
		localMd5 := getMd5OfFile(fixedPath)
		if localMd5 == fileData.Md5Checksum {
			delete(filesToUpload, fixedPath)
		} else {
			// add it to the map if it's not already there
			DebugLog("md5 did not match for", fixedPath)
			filesToUpload[fixedPath] = true
		}
	}

	return nil
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

	// DebugLog("these are the files and folders that were found on the shared drive:")
	// for localFolder, fileData := range localToRemoteLookup {
	// 	msg := fmt.Sprintf("%v\n%v\n\n", localFolder, fileData)
	// 	DebugLog(msg)
	// }

	for {
		DebugLog("Checking for any new or modified local files/folders")
		for folder := range conn.baseFolders {
			filepath.Walk(folder, walkAndCheckForModified)
		}

		if len(filesToUpload) > 0 {
			DebugLog("Uploading files")
			// grab all the metadata for the files/folders that are currently on the remote shared drive
			// because we need the ids of files/folders
			for localFolder := range conn.baseFolders {
				conn.fillLookupMap(localFolder)
			}

			for folder := range conn.baseFolders {
				filepath.Walk(folder, walkAndUpload)
			}

			DebugLog("Grabbing remote metadata so we can verify")
			// again grab all the metadata for the files/folders that are currently on the remote shared drive
			for localFolder := range conn.baseFolders {
				conn.fillLookupMap(localFolder)
			}

			verifyingAt := time.Now()

			for localFolder := range conn.baseFolders {
				filepath.Walk(localFolder, walkAndVerify)
			}

			if len(filesToUpload) == 0 {
				DebugLog("verified! updating new verified timestamp to", verifyingAt)
				verifiedAt = verifyingAt
				conn.clearLookupMap()
			} else {
				DebugLog("not verified, will try again next time")
			}
		}

		// TODO: slow it down a bit
		time.Sleep(20 * time.Second)
	}
}
