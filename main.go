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

var debug bool = false

var conn GoogleDriveConnection

var localFiles map[string]bool = make(map[string]bool)
var localToRemoteLookup map[string]FileMetaData = make(map[string]FileMetaData) // key=local file name

var verifiedAt time.Time
var checkedForDownloadsAt time.Time
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
	} // else {
	// 	delete(filesToUpload, fixedPath) // TODO: double check the logic on this call
	// }

	return nil
}

//*************************************************************************************************
//*************************************************************************************************

func needToCheckForDownloads() bool {
	now := time.Now()
	diff := now.Sub(checkedForDownloadsAt)

	// rate limits are:
	//  Queries per 100 seconds	20,000
	// Queries per day	1,000,000,000

	// tODO: slow this down a bit
	if diff.Seconds() > 100 {
		DebugLog("It's been", diff.Seconds(), "since last check for download, will check again")
		checkedForDownloadsAt = time.Now()
		return true
	} else {
		DebugLog("Don't need to check for downloads")
	}

	return false
}

//*************************************************************************************************
//*************************************************************************************************

func checkForDownloads() {
	for localPath := range localToRemoteLookup {
		remoteFileInfo := localToRemoteLookup[localPath]

		// first check if it already exists
		localFileInfo, err := os.Stat(localPath)
		if os.IsNotExist(err) {
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
		formattedTime := modifiedTime.Format(time.RFC3339Nano)
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
			localModTime := fileInfo.ModTime()
			remoteModTime, _ := time.Parse(time.RFC3339Nano, fileData.ModifiedTime)
			diff := localModTime.Sub(remoteModTime)
			DebugLog("local mod time is newer by", diff.Seconds(), "seconds")

			// if the local file is newer, then calculate the md5's
			// allow for some floating point roundoff error
			if diff.Seconds() > 0.5 {
				localMd5 := getMd5OfFile(fixedPath)

				if localMd5 != fileData.Md5Checksum {
					DebugLog("md5's do not match", localMd5, fileData.Md5Checksum)
					DebugLog("local mod time is newer", localModTime, remoteModTime)
					handleUpload(fixedPath, fileInfo.ModTime())
				}
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

func verifyRemoteFilesAreOnLocal() {
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

	for {
		// check if we need to upload anything
		DebugLog("Checking for any new or modified local files/folders")
		for folder := range conn.baseFolders {
			filepath.Walk(folder, walkAndCheckForModified)
		}

		willCheckForDownloads := needToCheckForDownloads()

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

			for folder := range conn.baseFolders {
				filepath.Walk(folder, walkAndUpload)
			}
		}

		// do the download
		if len(filesToDownload) > 0 {
			DebugLog("Preparing to download files")
			handleDownloads()
		}

		// do a verify if we uploaded or downloaded anything
		if len(filesToUpload) > 0 || len(filesToDownload) > 0 {
			DebugLog("Grabbing remote metadata so we can verify")
			// again grab all the metadata for the files/folders that are currently on the remote shared drive
			conn.clearLookupMap()
			conn.fillLookupMap(conn.getBaseFolderSlice())

			verifyingAt := time.Now()

			// verify local files are on the remote server
			for localFolder := range conn.baseFolders {
				filepath.Walk(localFolder, walkAndVerify)
			}

			// verify remote files are on the local side
			verifyRemoteFilesAreOnLocal()

			if len(filesToUpload) == 0 && len(filesToDownload) == 0 {
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
