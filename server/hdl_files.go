/******************************************************************************
 *
 *  Description :
 *
 *    Handler of large file uploads/downloads. Validates request first then calls
 *    a handler.
 *
 *    Use commercial services (Amazon S3 or Google's/MSFT's equivalents),
 *    or open source like Ceph or Minio.
 *
 *****************************************************************************/

package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/tinode/chat/server/store"
	"github.com/tinode/chat/server/store/types"
)

func largeFileServe(wrt http.ResponseWriter, req *http.Request) {
	log.Println("Request to download file", req.URL.Path)

	now := time.Now().UTC().Round(time.Millisecond)
	enc := json.NewEncoder(wrt)
	mh := store.GetMediaHandler(globals.fileHandler)

	if redirTo := mh.Redirect(req.URL.String()); redirTo != "" {
		wrt.Header().Set("Location", redirTo)
		wrt.Header().Set("Content-Type", "application/json; charset=utf-8")
		wrt.WriteHeader(http.StatusFound)
		enc.Encode(InfoFound("", "", now))
		return
	}

	writeHttpResponse := func(msg *ServerComMessage) {
		if msg == nil {
			msg = ErrUnknown("", "", now)
		}
		// Gorilla CompressHandler requires Content-Type to be set.
		wrt.Header().Set("Content-Type", "application/json; charset=utf-8")
		wrt.WriteHeader(msg.Ctrl.Code)
		enc.Encode(msg)
	}

	// Check for API key presence
	if isValid, _ := checkAPIKey(getAPIKey(req)); !isValid {
		writeHttpResponse(ErrAPIKeyRequired(now))
		return
	}

	// Check authorization: either the token or SID must be present
	uid, err := authHttpRequest(req)
	if err != nil {
		writeHttpResponse(decodeStoreError(err, "", "", now, nil))
		return
	}
	if uid.IsZero() {
		// Not authenticated
		writeHttpResponse(ErrAuthRequired("", "", now))
		return
	}

	fd, rsc, err := mh.Download(req.URL.String())
	defer rsc.Close()

	wrt.Header().Set("Content-Type", fd.MimeType)
	wrt.Header().Set("Content-Disposition", "attachment")
	http.ServeContent(wrt, req, "", fd.UpdatedAt, rsc)
}

// largeFileUpload receives files from client over HTTP(S) and saves them to local file
// system.
func largeFileUpload(wrt http.ResponseWriter, req *http.Request) {
	now := time.Now().UTC().Round(time.Millisecond)
	enc := json.NewEncoder(wrt)
	mh := store.GetMediaHandler(globals.fileHandler)

	if redirTo := mh.Redirect(req.URL.String()); redirTo != "" {
		wrt.Header().Set("Location", redirTo)
		wrt.Header().Set("Content-Type", "application/json; charset=utf-8")
		wrt.WriteHeader(http.StatusFound)
		enc.Encode(InfoFound("", "", now))
		return
	}

	writeHttpResponse := func(msg *ServerComMessage) {
		if msg == nil {
			msg = ErrUnknown("", "", now)
		}
		// Gorilla CompressHandler requires Content-Type to be set.
		wrt.Header().Set("Content-Type", "application/json; charset=utf-8")
		wrt.WriteHeader(msg.Ctrl.Code)
		enc.Encode(msg)
	}

	// Check if this is a POST request
	if req.Method != http.MethodPost {
		writeHttpResponse(ErrOperationNotAllowed("", "", now))
		return
	}

	if globals.maxFileUploadSize > 0 {
		req.Body = http.MaxBytesReader(wrt, req.Body, globals.maxFileUploadSize)
	}

	// Check for API key presence
	if isValid, _ := checkAPIKey(getAPIKey(req)); !isValid {
		writeHttpResponse(ErrAPIKeyRequired(now))
		return
	}
	// Check authorization: either the token or SID must be present
	uid, err := authHttpRequest(req)
	if err != nil {
		writeHttpResponse(decodeStoreError(err, "", "", now, nil))
		return
	}
	if uid.IsZero() {
		// Not authenticated
		writeHttpResponse(ErrAuthRequired("", "", now))
		return
	}

	log.Println("Starting upload", req.FormValue("name"))
	file, _, err := req.FormFile("file")
	if err != nil {
		log.Println("Error reading file", err)
		if strings.Contains(err.Error(), "request body too large") {
			log.Println("Uploaded file is too large", err)
			writeHttpResponse(ErrTooLarge("", "", now))
			if strings.Contains(err.Error(), "unexpected EOF") {
				log.Println("Upload interrupted by client", err)
				writeHttpResponse(ErrMalformed("", "", now))
			} else {
				log.Println("Upload error", err)
				writeHttpResponse(ErrMalformed("", "", now))
			}
			return
		}
	}
	fdef := types.FileDef{}
	fdef.Id = store.GetUidString()
	fdef.InitTimes()
	fdef.User = uid.String()

	buff := make([]byte, 512)
	if _, err = file.Read(buff); err != nil {
		log.Println("Failed to detect mime type", err)
		writeHttpResponse(nil)
		return
	}

	fdef.MimeType = http.DetectContentType(buff)
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		log.Println("Failed to reset request buffer", err)
		writeHttpResponse(nil)
		return
	}

	url, err := mh.Upload(&fdef, file)
	if err != nil {
		log.Println("Failed to upload file", fdef.Id, err)
		writeHttpResponse(decodeStoreError(err, "", "", now, nil))
		return
	}

	resp := NoErr("", "", now)
	resp.Ctrl.Params = map[string]string{"url": url}
	writeHttpResponse(resp)
}
