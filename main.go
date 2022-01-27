package main

import (
	"bufio"
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
var uploadLookupMap map[string]FileMetaData = make(map[string]FileMetaData)

var verifiedAt time.Time = time.Date(2000, time.January, 1, 12, 0, 0, 0, time.UTC)
var verifiedAtPlusOneSec time.Time = verifiedAt

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

func listServiceAcctFiles(folderId string) {
	if len(folderId) > 0 {
		conn.listFolderById(folderId)
	} else {
		conn.listFilesOwnedByServiceAcct(true)
	}
}

//*************************************************************************************************
//*************************************************************************************************

func wipeDeletedFiles(promptUser bool) {
	if promptUser {
		fmt.Println("\nAre you sure you want to delete files belonging to the service account?")
		fmt.Println("This only deletes files that are no longer in the user's shared folder.")
		fmt.Println("Type Y then hit Enter to proceed.")

		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			line := scanner.Text()
			if line == "Y" {
				break
			} else {
				fmt.Println("Aborting")
				return
			}
		}
	}

	DebugLog("Proceeding to delete files...")

	// TODO: if there are any errors when filling the lookup map, then don't proceed!!
	conn.fillLookupMap(conn.getBaseFolderSlice())

	allServiceAcctFiles := conn.listFilesOwnedByServiceAcct(false)
	for _, serviceFile := range allServiceAcctFiles {
		needToDelete := true

		// check if it's in one of the user's folders
		for _, remoteMetaData := range localToRemoteLookup {
			if len(serviceFile.Parents) == 0 || serviceFile.Parents[0] == remoteMetaData.ID {
				needToDelete = false
				break
			}
		}

		if needToDelete {
			conn.deleteFileOrFolder(serviceFile)
		}
	}

	conn.clearLookupMap()
}

//*************************************************************************************************
//*************************************************************************************************

func checkForUploads(filesToUpload map[string]bool) {
	// use a closure to give the walk function access to filesToUpload

	// this is the callback function that Walk will call for each local file/folder
	var walkAndCheckForModified = func(path string, fileInfo os.FileInfo, err error) error {
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

	// do the walking
	for folder := range conn.baseFolders {
		filepath.Walk(folder, walkAndCheckForModified)
	}
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

func checkForDownloads(filesToDownload map[string]FileMetaData) {
	for localPath, remoteFileInfo := range localToRemoteLookup {
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

func handleDownloads(filesToDownload map[string]FileMetaData) {
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
	parentId, parentInMap := uploadLookupMap[parentPath]
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
			uploadLookupMap[localPath] = FileMetaData{ID: ids[0], Name: fileName, MimeType: "application/vnd.google-apps.folder", Md5Checksum: ""}
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
	fileMetaData := uploadLookupMap[localPath]

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

func handleUploads(filesToUpload map[string]bool) {
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
		_, existsOnServer := uploadLookupMap[localPath]
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

		remoteFileData, existsOnServer := uploadLookupMap[localPath]
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

func verifyUploads(filesToUpload map[string]bool) {
	for localPath := range filesToUpload {

		localFileInfo, err := os.Stat(localPath)
		if err != nil {
			fmt.Println("error from Stat", err)
			delete(filesToUpload, localPath)
			continue
		}
		remoteFileData, onServer := uploadLookupMap[localPath]

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

func verifyDownloads(filesToDownload map[string]FileMetaData) {
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
	conn.initializeGoogleDrive()

	// check if we need to print debug statements
	if len(os.Args) > 1 {
		arg := os.Args[1]

		switch arg {
		case "debug":
			debug = true
		case "list":
			if len(os.Args) > 2 {
				debug = true
				listServiceAcctFiles(os.Args[2])
			} else {
				listServiceAcctFiles("")
			}
			os.Exit(0)
		case "delete":
			debug = true
			wipeDeletedFiles(true)
			os.Exit(0)
		default:
			fmt.Println("unknown arg", arg)
			os.Exit(1)
		}
	}

	fillLocalMap()
	filesToUpload := make(map[string]bool)
	filesToDownload := make(map[string]FileMetaData)

	for {
		// check if we need to upload anything
		DebugLog("Checking for any new or modified local files/folders")
		checkForUploads(filesToUpload)

		// do the upload
		if len(filesToUpload) > 0 {
			DebugLog("Preparing to upload files")
			conn.clearUploadLookupMap()
			conn.fillUploadLookupMap(conn.getBaseFolderSlice(), filesToUpload)
			handleUploads(filesToUpload)
		}

		//***********************************************************

		if remoteSideWasModified() {
			// grab all the metadata for the files/folders that are currently on the remote shared drive
			// because we need the ids of files/folders, timestamps, md5's, etc.
			conn.clearLookupMap()
			conn.fillLookupMap(conn.getBaseFolderSlice())

			// check if we need to download anything
			checkForDownloads(filesToDownload)
		}

		// do the download or re-download if it was not verified from the last loop
		if len(filesToDownload) > 0 {
			DebugLog("Preparing to download files")
			handleDownloads(filesToDownload)
		}

		//***********************************************************

		if len(filesToUpload) > 0 {
			DebugLog("Need to verify uploads. Grabbing remote metadata first.")
			conn.clearUploadLookupMap()
			conn.fillUploadLookupMap(conn.getBaseFolderSlice(), filesToUpload)
		}

		if len(filesToDownload) > 0 {
			DebugLog("Need to verify downloads. Grabbing remote metadata first.")
			// again grab all the metadata for the files/folders that are currently on the remote shared drive
			conn.clearLookupMap()
			conn.fillLookupMap(conn.getBaseFolderSlice())
		}

		// do a verify if we uploaded or downloaded anything
		if len(filesToUpload) > 0 || len(filesToDownload) > 0 {
			verifyingAt := time.Now()

			// verify local files were uploaded to the remote server
			verifyUploads(filesToUpload)

			// verify remote files were downloaded to the local side
			verifyDownloads(filesToDownload)

			if len(filesToUpload) == 0 && len(filesToDownload) == 0 {
				DebugLog("verified! updating new verified timestamp to", verifyingAt)
				verifiedAt = verifyingAt
				verifiedAtPlusOneSec = verifiedAt.Add(time.Second)
				conn.clearUploadLookupMap()
				conn.clearLookupMap()
			} else {
				DebugLog("not verified, will try again next time")
			}
		}

		// TODO: slow it down a bit
		time.Sleep(20 * time.Second)
	}
}
