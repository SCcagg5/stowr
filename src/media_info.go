package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type mediaSourceMetadata struct {
	Size         int64
	MIME         string
	ETag         string
	LastModified string
}

func parseInt64Default(value string, fallback int64) int64 {
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

type mediaInfoResponse struct {
	Instance        string            `json:"instance"`
	Key             string            `json:"key"`
	Size            int64             `json:"size"`
	MIME            string            `json:"mime,omitempty"`
	Headers         map[string]string `json:"headers,omitempty"`
	Container       string            `json:"container,omitempty"`
	Width           int               `json:"width,omitempty"`
	Height          int               `json:"height,omitempty"`
	DurationSeconds float64           `json:"durationSeconds,omitempty"`
	Codecs          []string          `json:"codecs,omitempty"`
	Tracks          []mediaTrackInfo  `json:"tracks,omitempty"`
	Properties      map[string]string `json:"properties,omitempty"`
	// CustomerKey is true for an object with a key of the client (SSE-C): the provider refused to read it.
	CustomerKey bool `json:"customerKey,omitempty"`
}

// writeMediaInfoError writes the error of a read for the details. The refusal of an object with a key of the client
// (SSE-C) is not an error for the details: the answer tells that the object has such a key.
func writeMediaInfoError(w http.ResponseWriter, r *http.Request, instance *storageInstance, key string, err error) {
	if response, ok := customerKeyMediaInfo(r.Context(), instance, key, err); ok {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, response)
		return
	}
	writeAPIError(w, err)
}

func (a *application) handleMediaInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
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
		writeAPIError(w, apiError{Status: http.StatusBadRequest, Code: "invalid_key", Message: "object key cannot be empty"})
		return
	}
	if _, err := a.authorizeRead(r, instance, key); err != nil {
		writeAPIError(w, err)
		return
	}

	extension := strings.ToLower(strings.TrimPrefix(filepath.Ext(key), "."))
	contentTypeHint := strings.TrimSpace(strings.Split(r.URL.Query().Get("mime"), ";")[0])
	listedMetadata := mediaSourceMetadata{
		Size:         parseInt64Default(r.URL.Query().Get("size"), 0),
		MIME:         contentTypeHint,
		ETag:         strings.TrimSpace(r.URL.Query().Get("etag")),
		LastModified: strings.TrimSpace(r.URL.Query().Get("lastModified")),
	}
	response := mediaInfoResponse{Instance: instance.cfg.ID, Key: key}
	if documentResponse, handled, documentErr := inspectDocumentDetails(r.Context(), instance, key, extension, contentTypeHint, listedMetadata); handled {
		if documentErr != nil {
			writeMediaInfoError(w, r, instance, key, documentErr)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, documentResponse)
		return
	}
	if !extensionMayContainInspectableMetadata(extension) && !contentTypeMayContainInspectableMetadata(contentTypeHint) {
		head, headErr := instance.Head(r.Context(), instance.fullKey(key))
		if headErr != nil {
			writeMediaInfoError(w, r, instance, key, headErr)
			return
		}
		if head.Body != nil {
			_ = head.Body.Close()
		}
		populateMediaStorageFields(&response, head.Header, 0)
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, response)
		return
	}

	source, err := openObjectRangeSource(r.Context(), instance, key)
	if err != nil {
		writeMediaInfoError(w, r, instance, key, err)
		return
	}
	// The format parser performs the first byte-range request. This keeps each
	// Details action format-aware: PNG reads only its fixed header, while JPEG,
	// TIFF, ISO-BMFF, Matroska, and audio containers follow their own declared
	// metadata lengths and offsets. No metadata is read during folder listing.
	metadata, inspectErr := inspectAdaptiveMetadata(source, extension, contentTypeHint)
	if inspectErr != nil {
		// Some providers answer a zero-byte Range request with 416. Only that
		// specific case falls back to HEAD. Range-policy errors must remain errors;
		// hiding them behind HEAD could silently turn later preview reads into full,
		// unexpectedly expensive object downloads.
		var upstream *upstreamError
		if source.RequestCount() == 0 && errors.As(inspectErr, &upstream) && upstream.StatusCode == http.StatusRequestedRangeNotSatisfiable {
			head, headErr := instance.Head(r.Context(), instance.fullKey(key))
			if headErr == nil {
				if head.Body != nil {
					_ = head.Body.Close()
				}
				populateMediaStorageFields(&response, head.Header, 0)
				w.Header().Set("Cache-Control", "no-store")
				writeJSON(w, http.StatusOK, response)
				return
			}
		}
		writeMediaInfoError(w, r, instance, key, inspectErr)
		return
	}
	if source.Headers() == nil {
		head, headErr := instance.Head(r.Context(), instance.fullKey(key))
		if headErr != nil {
			writeMediaInfoError(w, r, instance, key, headErr)
			return
		}
		if head.Body != nil {
			_ = head.Body.Close()
		}
		populateMediaStorageFields(&response, head.Header, 0)
	} else {
		populateMediaStorageFields(&response, source.Headers(), source.Size())
	}
	response.Container = mediaContainer(extension, response.MIME)
	response.Width = metadata.Width
	response.Height = metadata.Height
	response.DurationSeconds = metadata.DurationSeconds
	response.Codecs = metadata.Codecs
	response.Tracks = metadata.Tracks
	response.Properties = metadata.Properties
	if len(response.Properties) == 0 {
		response.Properties = nil
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, response)
}

func contentTypeMayContainInspectableMetadata(contentType string) bool {
	value := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	return strings.HasPrefix(value, "image/") || strings.HasPrefix(value, "video/") || strings.HasPrefix(value, "audio/") ||
		strings.Contains(value, "matroska") || strings.Contains(value, "heif") || strings.Contains(value, "avif")
}

func extensionMayContainInspectableMetadata(extension string) bool {
	switch strings.ToLower(extension) {
	case "jpg", "jpeg", "png", "gif", "webp", "bmp", "dib", "svg", "avif", "ico", "cur",
		"tif", "tiff", "heic", "heif", "jxl", "jp2", "j2k", "jpf", "jpx", "jpc", "psd", "psb", "tga", "exr", "hdr", "rgbe", "dds", "pcx", "pnm", "ppm", "pgm", "pbm", "pam", "sgi", "rgb", "rgba", "bw", "qoi", "ff", "farbfeld", "fits", "fit", "fts",
		"raf", "raw", "dng", "cr2", "cr3", "nef", "nrw", "arw", "srf", "sr2", "orf", "rw2", "pef", "x3f", "erf", "mef", "mos", "kdc", "dcr", "mrw", "rwl", "iiq", "3fr", "fff",
		"mp4", "mkv", "webm", "avi", "mov", "m4v", "mpg", "mpeg", "flv", "f4v", "3gp", "3g2", "wmv", "asf", "ogv", "mts", "m2ts", "ts", "vob", "mxf", "dv", "m2v",
		"mp3", "flac", "wav", "wave", "m4a", "aac", "ogg", "oga", "opus", "aiff", "aif", "alac", "wma", "amr", "midi", "mid", "ape", "wv", "tta", "ac3", "eac3", "dts", "mka", "au", "caf":
		return true
	default:
		return false
	}
}

func populateMediaStorageFields(response *mediaInfoResponse, headers http.Header, knownSize int64) {
	if knownSize <= 0 {
		knownSize, _ = strconv.ParseInt(strings.TrimSpace(headers.Get("Content-Length")), 10, 64)
	}
	response.Size = maxInt64(0, knownSize)
	response.MIME = strings.TrimSpace(strings.Split(headers.Get("Content-Type"), ";")[0])
	filtered := make(http.Header)
	copyObjectHeaders(filtered, headers)
	response.Headers = make(map[string]string)
	for name, values := range filtered {
		if len(values) == 0 {
			continue
		}
		response.Headers[strings.ToLower(name)] = strings.Join(values, ", ")
	}
	if len(response.Headers) == 0 {
		response.Headers = nil
	}
}

func mediaContainer(extension, contentType string) string {
	if extension != "" {
		aliases := map[string]string{
			"jpg": "JPEG", "jpeg": "JPEG", "tif": "TIFF", "tiff": "TIFF",
			"m4v": "MP4", "m4a": "MP4", "qt": "MOV", "wave": "WAV",
		}
		if value := aliases[extension]; value != "" {
			return value
		}
		return strings.ToUpper(extension)
	}
	switch contentType {
	case "image/jpeg":
		return "JPEG"
	case "image/png":
		return "PNG"
	case "image/gif":
		return "GIF"
	case "image/webp":
		return "WEBP"
	case "video/mp4", "audio/mp4":
		return "MP4"
	case "video/quicktime":
		return "MOV"
	default:
		return ""
	}
}

func isISOBaseMedia(extension, contentType string) bool {
	switch extension {
	case "mp4", "m4v", "m4a", "mov", "qt":
		return true
	}
	return contentType == "video/mp4" || contentType == "audio/mp4" || contentType == "video/quicktime"
}

func parseImageDimensions(data []byte, extension, contentType string) (int, int, bool) {
	switch {
	case extension == "png" || contentType == "image/png":
		if len(data) >= 24 && bytes.Equal(data[:8], []byte{137, 80, 78, 71, 13, 10, 26, 10}) {
			width := int(binary.BigEndian.Uint32(data[16:20]))
			height := int(binary.BigEndian.Uint32(data[20:24]))
			return width, height, width > 0 && height > 0
		}
	case extension == "gif" || contentType == "image/gif":
		if len(data) >= 10 && (bytes.Equal(data[:6], []byte("GIF87a")) || bytes.Equal(data[:6], []byte("GIF89a"))) {
			width := int(binary.LittleEndian.Uint16(data[6:8]))
			height := int(binary.LittleEndian.Uint16(data[8:10]))
			return width, height, width > 0 && height > 0
		}
	case extension == "webp" || contentType == "image/webp":
		return parseWebPDimensions(data)
	case extension == "jpg" || extension == "jpeg" || contentType == "image/jpeg":
		return parseJPEGDimensions(data)
	}
	return 0, 0, false
}

func parseJPEGDimensions(data []byte) (int, int, bool) {
	if len(data) < 4 || data[0] != 0xff || data[1] != 0xd8 {
		return 0, 0, false
	}
	for offset := 2; offset+4 <= len(data); {
		for offset < len(data) && data[offset] != 0xff {
			offset++
		}
		for offset < len(data) && data[offset] == 0xff {
			offset++
		}
		if offset >= len(data) {
			break
		}
		marker := data[offset]
		offset++
		if marker == 0xd8 || marker == 0xd9 || (marker >= 0xd0 && marker <= 0xd7) || marker == 0x01 {
			continue
		}
		if offset+2 > len(data) {
			break
		}
		segmentLength := int(binary.BigEndian.Uint16(data[offset : offset+2]))
		if segmentLength < 2 || offset+segmentLength > len(data) {
			break
		}
		if isJPEGStartOfFrame(marker) && segmentLength >= 7 {
			height := int(binary.BigEndian.Uint16(data[offset+3 : offset+5]))
			width := int(binary.BigEndian.Uint16(data[offset+5 : offset+7]))
			return width, height, width > 0 && height > 0
		}
		offset += segmentLength
	}
	return 0, 0, false
}

func isJPEGStartOfFrame(marker byte) bool {
	switch marker {
	case 0xc0, 0xc1, 0xc2, 0xc3, 0xc5, 0xc6, 0xc7, 0xc9, 0xca, 0xcb, 0xcd, 0xce, 0xcf:
		return true
	default:
		return false
	}
}

func parseWebPDimensions(data []byte) (int, int, bool) {
	if len(data) < 30 || !bytes.Equal(data[:4], []byte("RIFF")) || !bytes.Equal(data[8:12], []byte("WEBP")) {
		return 0, 0, false
	}
	chunk := string(data[12:16])
	switch chunk {
	case "VP8X":
		width := 1 + int(data[24]) + int(data[25])<<8 + int(data[26])<<16
		height := 1 + int(data[27]) + int(data[28])<<8 + int(data[29])<<16
		return width, height, width > 0 && height > 0
	case "VP8L":
		if len(data) < 25 || data[20] != 0x2f {
			return 0, 0, false
		}
		width := 1 + int(data[21]) + (int(data[22])&0x3f)<<8
		height := 1 + (int(data[22]) >> 6) + int(data[23])<<2 + (int(data[24])&0x0f)<<10
		return width, height, width > 0 && height > 0
	case "VP8 ":
		if len(data) < 30 || data[23] != 0x9d || data[24] != 0x01 || data[25] != 0x2a {
			return 0, 0, false
		}
		width := int(binary.LittleEndian.Uint16(data[26:28]) & 0x3fff)
		height := int(binary.LittleEndian.Uint16(data[28:30]) & 0x3fff)
		return width, height, width > 0 && height > 0
	}
	return 0, 0, false
}

func parseISOBaseMedia(segments [][]byte) (int, int, float64, []string) {
	width, height := 0, 0
	duration := float64(0)
	codecSet := make(map[string]struct{})
	knownCodecs := []string{"avc1", "avc3", "hvc1", "hev1", "vp09", "av01", "mp4v", "mp4a", "Opus", "ac-3", "ec-3"}
	for _, data := range segments {
		for _, codec := range knownCodecs {
			if bytes.Contains(data, []byte(codec)) {
				codecSet[codec] = struct{}{}
			}
		}
		for _, payload := range findISOBoxPayloads(data, "mvhd") {
			if value := parseMVHDDuration(payload); value > 0 && (duration == 0 || value > duration) {
				duration = value
			}
		}
		for _, payload := range findISOBoxPayloads(data, "tkhd") {
			candidateWidth, candidateHeight := parseTKHDDimensions(payload)
			if candidateWidth*candidateHeight > width*height {
				width, height = candidateWidth, candidateHeight
			}
		}
	}
	codecs := make([]string, 0, len(codecSet))
	for codec := range codecSet {
		codecs = append(codecs, codec)
	}
	sort.Strings(codecs)
	return width, height, duration, codecs
}

func findISOBoxPayloads(data []byte, boxType string) [][]byte {
	needle := []byte(boxType)
	payloads := make([][]byte, 0, 2)
	for searchFrom := 4; searchFrom+4 <= len(data); {
		relative := bytes.Index(data[searchFrom:], needle)
		if relative < 0 {
			break
		}
		typeOffset := searchFrom + relative
		boxStart := typeOffset - 4
		boxSize := int64(binary.BigEndian.Uint32(data[boxStart:typeOffset]))
		payloadStart := typeOffset + 4
		if boxSize == 1 {
			if payloadStart+8 > len(data) {
				searchFrom = typeOffset + 1
				continue
			}
			boxSize = int64(binary.BigEndian.Uint64(data[payloadStart : payloadStart+8]))
			payloadStart += 8
		}
		if boxSize >= int64(payloadStart-boxStart) && boxSize <= int64(len(data)-boxStart) {
			boxEnd := boxStart + int(boxSize)
			if payloadStart <= boxEnd {
				payloads = append(payloads, data[payloadStart:boxEnd])
			}
		}
		searchFrom = typeOffset + len(needle)
	}
	return payloads
}

func parseMVHDDuration(payload []byte) float64 {
	if len(payload) < 20 {
		return 0
	}
	version := payload[0]
	if version == 1 {
		if len(payload) < 32 {
			return 0
		}
		timescale := binary.BigEndian.Uint32(payload[20:24])
		duration := binary.BigEndian.Uint64(payload[24:32])
		if timescale == 0 {
			return 0
		}
		return float64(duration) / float64(timescale)
	}
	timescale := binary.BigEndian.Uint32(payload[12:16])
	duration := binary.BigEndian.Uint32(payload[16:20])
	if timescale == 0 {
		return 0
	}
	return float64(duration) / float64(timescale)
}

func parseTKHDDimensions(payload []byte) (int, int) {
	if len(payload) < 8 {
		return 0, 0
	}
	width := int(binary.BigEndian.Uint32(payload[len(payload)-8:len(payload)-4]) >> 16)
	height := int(binary.BigEndian.Uint32(payload[len(payload)-4:]) >> 16)
	if width <= 0 || height <= 0 || width > 100000 || height > 100000 {
		return 0, 0
	}
	return width, height
}

func minInt64(left, right int64) int64 {
	if left < right {
		return left
	}
	return right
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}
