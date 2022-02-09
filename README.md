# Google-Drive-For-Desktop-Lite
Google Drive client for Linux (or any platform that Go supports).

### Features/Limitations
* Uploads supported for any file size
* Downloads supported for any file size
* Once every 30 seconds it will check for new uploads/downloads
* To delete files it is recommended that you manually delete files on the Google Drive shared folder and then delete the local files. (This is partially because the Google Drive service account may not have permission to delete files that are owned by the user.)

### Compiling
```go build -ldflags="-w -s"```

To see details of the build flags, run the command ```go doc cmd/link```

### Configuration


### Running
