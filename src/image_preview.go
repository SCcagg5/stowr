package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
)

const maxEmbeddedImagePreviewBytes = int64(128 << 20)

func (a *application) handleImagePreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	instance, err := a.instanceFromRequest(r, "")
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if err := requirePermission(instance, permissionRead); err != nil {
		writeAPIError(w, err)
		return
	}
	key := cleanRelativeKey(r.URL.Query().Get("key"))
	if key == "" {
		writeAPIError(w, apiError{Status: http.StatusBadRequest, Code: "invalid_key", Message: "image key cannot be empty"})
		return
	}
	if _, err := a.authorizeRead(r, instance, key); err != nil {
		writeAPIError(w, err)
		return
	}
	extension := strings.ToLower(strings.TrimPrefix(filepath.Ext(key), "."))

	// RAW/TIFF containers often carry a camera-generated JPEG preview. Reading
	// the format header and then returning that existing byte range is cheaper
	// than downloading and decoding the complete RAW object.
	if extension == "raf" || isTIFFBasedImage(extension, "") {
		source, sourceErr := openObjectRangeSource(r.Context(), instance, key)
		if sourceErr == nil {
			if extension == "raf" {
				header, headerErr := source.ReadRange(0, 128)
				if (headerErr == nil || errors.Is(headerErr, io.EOF)) && source.Size() > 0 {
					if offset, length, ok := rafPreviewRange(header); ok && embeddedPreviewFitsObject(offset, length, source.Size()) {
						a.serveEmbeddedImageRange(w, r, instance, key, offset, length)
						return
					}
				}
			} else if header, valid, headerErr := readAdaptiveTIFFHeader(source); headerErr == nil && valid && source.Size() > 0 {
				if offset, length, ok := embeddedTIFFJPEGRange(header); ok && embeddedPreviewFitsObject(offset, length, source.Size()) {
					a.serveEmbeddedImageRange(w, r, instance, key, offset, length)
					return
				}
			}
		}
	}

	writeAPIError(w, apiError{
		Status:  http.StatusNotImplemented,
		Code:    "embedded_image_preview_unavailable",
		Message: "this self-contained build can preview this image only when the original is browser-readable or the file contains an embedded JPEG preview",
	})
}

func embeddedPreviewFitsObject(offset, length, objectSize int64) bool {
	return offset >= 0 && length > 0 && length <= maxEmbeddedImagePreviewBytes && objectSize > 0 && offset <= objectSize && length <= objectSize-offset
}

func (a *application) serveEmbeddedImageRange(w http.ResponseWriter, r *http.Request, instance *storageInstance, key string, offset, length int64) {
	if offset < 0 || length <= 0 || length > maxEmbeddedImagePreviewBytes {
		writeAPIError(w, apiError{Status: http.StatusUnprocessableEntity, Code: "invalid_embedded_preview", Message: "the embedded image preview range is invalid"})
		return
	}
	headers := make(http.Header)
	headers.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
	object, err := instance.Get(r.Context(), instance.fullKey(key), headers)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if object.Body == nil {
		writeAPIError(w, apiError{Status: http.StatusBadGateway, Code: "empty_image_preview", Message: "the storage provider returned an empty image preview"})
		return
	}
	defer object.Body.Close()
	if !isSuccessfulObjectReadStatus(object.StatusCode) {
		writeAPIError(w, &upstreamError{StatusCode: object.StatusCode, Code: "ImagePreviewReadFailed"})
		return
	}
	contentRange := strings.TrimSpace(object.Header.Get("Content-Range"))
	contentLength, contentLengthErr := strconv.ParseInt(strings.TrimSpace(object.Header.Get("Content-Length")), 10, 64)
	if object.StatusCode == http.StatusPartialContent && contentRange == "" {
		writeAPIError(w, apiError{Status: http.StatusBadGateway, Code: "range_not_supported", Message: "the storage provider omitted Content-Range for the embedded image preview"})
		return
	}
	if object.StatusCode == http.StatusOK && contentRange == "" {
		// Do not accept a provider that silently turns a small embedded-preview
		// Range into a complete RAW download. That would make a right-sized
		// preview unexpectedly expensive on object storage.
		if offset != 0 || contentLengthErr != nil || contentLength != length {
			writeAPIError(w, apiError{Status: http.StatusBadGateway, Code: "range_not_supported", Message: "the storage provider ignored the embedded image preview range"})
			return
		}
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.Header().Set("Cache-Control", "no-store")
	applyObjectSafetyHeaders(w.Header())
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.CopyN(w, object.Body, length)
}

func embeddedTIFFJPEGRange(data []byte) (int64, int64, bool) {
	reader, firstIFD, ok := newTIFFReader(data)
	if !ok {
		return 0, 0, false
	}
	visited := make(map[uint64]struct{})
	var inspect func(uint64, int) (int64, int64, bool)
	inspect = func(offset uint64, depth int) (int64, int64, bool) {
		if depth > 5 {
			return 0, 0, false
		}
		if _, exists := visited[offset]; exists {
			return 0, 0, false
		}
		visited[offset] = struct{}{}
		tags := reader.readIFD(offset)
		jpegOffset := int64(reader.asUint(tags[0x0201]))
		jpegLength := int64(reader.asUint(tags[0x0202]))
		if jpegOffset >= 0 && jpegLength > 0 {
			return jpegOffset, jpegLength, true
		}
		for _, tag := range []uint16{0x014a, 0x8769} {
			if child := reader.asUint(tags[tag]); child > 0 {
				if foundOffset, foundLength, found := inspect(child, depth+1); found {
					return foundOffset, foundLength, true
				}
			}
		}
		return 0, 0, false
	}
	return inspect(firstIFD, 0)
}
