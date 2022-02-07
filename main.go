package main

import (
	"bufio"
	"fmt"
	"os"
	"time"
)

//*************************************************************************************************
//*************************************************************************************************

var debug bool = false

//*************************************************************************************************
//*************************************************************************************************

func removeDeletedFiles(service *GoogleDriveService, promptUser bool) {
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

	// if there are any errors when filling the lookup map, then don't proceed!!
	localToRemoteLookup := make(map[string]FileMetaData) // key=local file name
	err := service.fillLookupMap(localToRemoteLookup, service.getBaseFolderSlice())
	if err != nil {
		fmt.Println("failed to fillLookupMap, not removing the deleted files")
		return
	}

	allServiceAcctFiles, err := service.conn.getFilesOwnedByServiceAcct(false)
	if err != nil {
		fmt.Println("failed to getFilesOwnedByServiceAcct, not removing the deleted files")
		return
	}
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
			service.conn.deleteFileOrFolder(serviceFile)
		}
	}
}

//*************************************************************************************************
//*************************************************************************************************

func main() {
	var service GoogleDriveService
	service.initializeService()

	// check if we need to print debug statements
	if len(os.Args) > 1 {
		arg := os.Args[1]

		switch arg {
		case "debug":
			debug = true
		case "list":
			if len(os.Args) > 2 {
				debug = true
				resp, err := service.conn.getItemsInSharedFolder("?", os.Args[2])
				fmt.Println("err", err)
				for _, file := range resp.Files {
					fmt.Println(file)
				}
			} else {
				service.conn.getFilesOwnedByServiceAcct(true)
			}
			os.Exit(0)
		case "delete":
			debug = true
			removeDeletedFiles(&service, true)
			os.Exit(0)
		default:
			fmt.Println("unknown arg", arg)
			os.Exit(1)
		}
	}

	service.fillLocalMap()

	var verified bool = false
	const SLEEP_SECONDS time.Duration = 20 // TODO: slow it down a bit?
	firstPass := true

	for {
		if !firstPass {
			time.Sleep(SLEEP_SECONDS * time.Second)
		}
		firstPass = false

		if !verified {
			service.resetVerifiedTime()
		}

		//***********************************************************

		// upload section

		// check if we need to upload anything
		DebugLog("Checking for any new or modified local files/folders")
		localModified := service.localFilesModified()

		// do the upload
		if localModified {
			DebugLog("Preparing to upload files")
			service.clearUploadLookupMap()
			err := service.fillUploadLookupMap(service.getBaseFolderSlice())
			if err != nil {
				continue
			}
			err = service.handleUploads()
			if err != nil {
				// if we only uploaded half a file then we don't want to download that half-written file,
				// so we will try again from the beginning of the loop
				continue
			}
		}

		//***********************************************************

		// download section

		// check if anything was modified on the remote shared drive
		remoteModifiedFiles, err := service.getRemoteModifiedFiles()
		if err != nil {
			continue
		}
		if len(remoteModifiedFiles) > 0 {
			// grab all the metadata for the files/folders that are currently on the remote shared drive
			// because we need the ids of files/folders, timestamps, md5's, etc.
			service.clearDownloadLookupMap()
			err := service.fillDownloadLookupMap(remoteModifiedFiles, verified)
			if err != nil {
				continue
			}

			// check if we need to download anything
			service.checkForDownloads()
		}

		// do the download or re-download if it was not verified from the last loop
		if len(service.filesToDownload) > 0 {
			DebugLog("Preparing to download files")
			service.handleDownloads()
		}

		//***********************************************************

		// verify section

		if len(service.filesToUpload) > 0 {
			DebugLog("Need to verify uploads. Grabbing remote metadata first.")
			service.clearUploadLookupMap()
			err := service.fillUploadLookupMap(service.getBaseFolderSlice())
			if err != nil {
				continue
			}
		}

		if len(service.filesToDownload) > 0 {
			DebugLog("Need to verify downloads. Grabbing remote metadata first.")
			// again grab all the metadata for the files/folders that are currently on the remote shared drive
			service.clearDownloadLookupMap()
			err := service.fillDownloadLookupMap(remoteModifiedFiles, verified)
			if err != nil {
				continue
			}
		}

		// do a verify if we uploaded or downloaded anything
		if len(service.filesToUpload) > 0 || len(service.filesToDownload) > 0 {
			// verify local files were uploaded to the remote server
			service.verifyUploads()

			// verify remote files were downloaded to the local side
			service.verifyDownloads()

			if len(service.filesToUpload) == 0 && len(service.filesToDownload) == 0 {
				DebugLog("verified! updating new verified timestamp to", service.mostRecentTimestampSeen)
				service.setVerifiedTime()
				service.clearUploadLookupMap()
				service.clearDownloadLookupMap()
				verified = true
			} else {
				DebugLog("not verified, will try again next time")
			}
		}
	}
}
