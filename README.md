# Google-Drive-For-Desktop-Lite
Google Drive client for Linux (or any platform that Go supports).

### Features/Limitations
* Uploads supported for any file size
* Downloads supported for any file size
* Once every 300 seconds it will check for new uploads/downloads
* To delete files it is recommended that you manually delete files on the Google Drive shared folder and then delete the local files. (This is partially because the Google Drive service account may not have permission to delete files that are owned by the user.)

### Compiling
```go build -ldflags="-w -s"```

To see details of the build flags, run the command ```go doc cmd/link```

### Configuration
This quickstart for Google Drive for Developers might be useful: https://developers.google.com/drive/api/quickstart/go
* Create a Google Cloud project at https://console.cloud.google.com/
* In your newly created project:
  * go to APIs and Services
  * then click Create Credentials
  * select API key
  * save this key to the file config/api-key.txt
  * click Create Credentials again
  * select Service Account
  * follow the steps and save the JSON to the file config/service-account.json

### Running
Use the default configuration by running: ```./Google-Drive-For-Desktop-Lite```

Add debug statements while running: ```./Google-Drive-For-Desktop-Lite debug```
