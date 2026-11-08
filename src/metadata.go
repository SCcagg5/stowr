package main

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The edit of the metadata of an object: S3 CopyObject onto the same key with x-amz-metadata-directive: REPLACE. The
// request gives the complete state: the standard headers of copiedObjectHeaders and the user metadata (x-amz-meta-*).
// A header that the request does not give is removed. The tags stay (x-amz-tagging-directive: COPY). The copy names
// the ETag that the form read, so the provider refuses the copy of an object whose content changed in the meantime.
// A change of the metadata only keeps the ETag on SeaweedFS, MinIO and Garage: a HEAD request before the copy also
// compares the modification date that the form read. From
// multipartCopyThreshold up, the copy goes in parts and CreateMultipartUpload gets the metadata. The copy of an older
// version makes that version the current version, as a restore does. Only the S3 backend has the edit: the GCS
// backend answers 501.

const (
	// maxUserMetadataBytes is the S3 limit of the user metadata: the sum of the bytes of each key and each value.
	maxUserMetadataBytes = 2048
	// maxStandardHeaderBytes is the limit of Stowr for the value of one standard header.
	maxStandardHeaderBytes = 1024
	// maxETagBytes is the limit of Stowr for the ETag of a request.
	maxETagBytes = 128
	// metadataProbeKey is the object that the start probe names. The probe never writes it.
	metadataProbeKey = ".stowr-metadata-probe"
)

// ObjectMetadataEditor is optional: a backend that replaces the metadata of an object, or of one version of it, by a
// copy onto its key. The provider refuses the copy with 412 when the ETag of the source is not etag.
type ObjectMetadataEditor interface {
	ReplaceObjectMetadata(ctx context.Context, key, version, etag string, headers http.Header) error
}

// objectHeaders are the standard headers that the edit sets. An empty field means no header.
type objectHeaders struct {
	ContentType        string `json:"contentType,omitempty"`
	CacheControl       string `json:"cacheControl,omitempty"`
	ContentDisposition string `json:"contentDisposition,omitempty"`
	ContentEncoding    string `json:"contentEncoding,omitempty"`
	ContentLanguage    string `json:"contentLanguage,omitempty"`
	Expires            string `json:"expires,omitempty"`
}

// fields gives each header with its HTTP name, in the order of copiedObjectHeaders.
func (h *objectHeaders) fields() []struct {
	name  string
	value *string
} {
	return []struct {
		name  string
		value *string
	}{
		{"Content-Type", &h.ContentType},
		{"Cache-Control", &h.CacheControl},
		{"Content-Disposition", &h.ContentDisposition},
		{"Content-Encoding", &h.ContentEncoding},
		{"Content-Language", &h.ContentLanguage},
		{"Expires", &h.Expires},
	}
}

// metadataEntry is one user metadata: the name without the prefix x-amz-meta-, and its value.
type metadataEntry struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

var (
	metadataKeyPattern      = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	headerTokenPattern      = regexp.MustCompile(`^[!#$%&'*+.^_` + "`" + `|~0-9A-Za-z-]+$`)
	contentEncodingPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.+-]*$`)
	contentLanguagePattern  = regexp.MustCompile(`^(\*|[A-Za-z]{1,8}(-[A-Za-z0-9]{1,8})*)$`)
	cacheDirectiveValueChar = regexp.MustCompile(`^("[\x20\x21\x23-\x7e]*"|[!#$%&'*+.^_` + "`" + `|~0-9A-Za-z-]+)$`)
)

func invalidMetadata(format string, values ...any) error {
	return apiError{Status: http.StatusBadRequest, Code: "invalid_metadata", Message: fmt.Sprintf(format, values...)}
}

// printableASCII reports whether the text has only the characters 0x20 to 0x7E, with no space at the start or the end.
// S3 sends the metadata in HTTP headers: other characters do not come back unchanged.
func printableASCII(text string) bool {
	if text != strings.TrimSpace(text) {
		return false
	}
	for index := 0; index < len(text); index++ {
		if text[index] < 0x20 || text[index] > 0x7e {
			return false
		}
	}
	return true
}

// splitList gives the items of a list that a comma separates. An empty item gives false.
func splitList(value string) ([]string, bool) {
	items := strings.Split(value, ",")
	for index, item := range items {
		items[index] = strings.TrimSpace(item)
		if items[index] == "" {
			return nil, false
		}
	}
	return items, true
}

// validateObjectHeaders checks each standard header against its HTTP syntax. An empty field means no header.
func validateObjectHeaders(headers objectHeaders) error {
	for _, field := range headers.fields() {
		value := *field.value
		if value == "" {
			continue
		}
		if len(value) > maxStandardHeaderBytes {
			return invalidMetadata("%s has at most %d characters, got %d", field.name, maxStandardHeaderBytes, len(value))
		}
		if !printableASCII(value) {
			return invalidMetadata("%s has ASCII letters, digits, spaces and punctuation only, with no space at the start or the end", field.name)
		}
		switch field.name {
		case "Content-Type":
			mediaType, _, err := mime.ParseMediaType(value)
			if err != nil || !strings.Contains(mediaType, "/") {
				return invalidMetadata("Content-Type must be a media type such as text/plain or application/json; charset=utf-8, got %q", value)
			}
		case "Content-Disposition":
			disposition, _, err := mime.ParseMediaType(value)
			if err != nil || (disposition != "inline" && disposition != "attachment") {
				return invalidMetadata("Content-Disposition must be inline or attachment, with optional parameters such as filename=\"report.pdf\", got %q", value)
			}
		case "Content-Encoding":
			items, ok := splitList(value)
			for _, item := range items {
				ok = ok && contentEncodingPattern.MatchString(item) && !strings.EqualFold(item, "aws-chunked")
			}
			if !ok {
				return invalidMetadata("Content-Encoding must be a list of codings such as gzip or br, separated by commas (aws-chunked is not stored), got %q", value)
			}
		case "Content-Language":
			items, ok := splitList(value)
			for _, item := range items {
				ok = ok && contentLanguagePattern.MatchString(item)
			}
			if !ok {
				return invalidMetadata("Content-Language must be a list of language tags such as en or fr-CA, separated by commas, got %q", value)
			}
		case "Cache-Control":
			items, ok := splitList(value)
			for _, item := range items {
				name, argument, hasArgument := strings.Cut(item, "=")
				ok = ok && headerTokenPattern.MatchString(name) && (!hasArgument || cacheDirectiveValueChar.MatchString(argument))
			}
			if !ok {
				return invalidMetadata("Cache-Control must be a list of directives such as max-age=3600 or no-cache, separated by commas, got %q", value)
			}
		case "Expires":
			parsed, err := http.ParseTime(value)
			if err != nil || parsed.UTC().Format(http.TimeFormat) != value {
				return invalidMetadata("Expires must be an HTTP date such as %s, got %q", time.Date(2026, 10, 21, 7, 28, 0, 0, time.UTC).Format(http.TimeFormat), value)
			}
		}
	}
	return nil
}

// validateUserMetadata checks the user metadata: a unique key of lowercase letters, digits, '.', '_' and '-', a value
// of printable ASCII, and the S3 limit of 2 KiB for the sum of the keys and the values.
func validateUserMetadata(entries []metadataEntry) error {
	seen := make(map[string]bool, len(entries))
	total := 0
	for _, entry := range entries {
		switch {
		case !metadataKeyPattern.MatchString(entry.Key):
			return invalidMetadata("the metadata key %q must start with a lowercase letter or a digit, and have only lowercase letters, digits, '.', '_' and '-'", entry.Key)
		case seen[entry.Key]:
			return invalidMetadata("the metadata key %q is twice in the request", entry.Key)
		case entry.Value == "":
			return invalidMetadata("the metadata %q has no value: remove it, or give a value", entry.Key)
		case !printableASCII(entry.Value):
			return invalidMetadata("the value of the metadata %q has ASCII letters, digits, spaces and punctuation only, with no space at the start or the end", entry.Key)
		}
		seen[entry.Key] = true
		total += len(entry.Key) + len(entry.Value)
	}
	if total > maxUserMetadataBytes {
		return invalidMetadata("the user metadata has at most %d bytes (keys and values), got %d", maxUserMetadataBytes, total)
	}
	return nil
}

// validateETag checks the ETag of a request: 1 to maxETagBytes printable characters, with no quote, space or comma.
func validateETag(etag string) error {
	if etag == "" {
		return invalidMetadata("etag is required: give the ETag that the form read")
	}
	if len(etag) > maxETagBytes || !printableASCII(etag) || strings.ContainsAny(etag, "\" ,") {
		return invalidMetadata("etag must be the ETag of the object without quotes, got %q", etag)
	}
	return nil
}

// metadataRequestHeaders gives the HTTP headers of the copy: the standard headers and the x-amz-meta-* headers.
func metadataRequestHeaders(headers objectHeaders, entries []metadataEntry) http.Header {
	out := make(http.Header)
	for _, field := range headers.fields() {
		if *field.value != "" {
			out.Set(field.name, *field.value)
		}
	}
	for _, entry := range entries {
		out.Set("X-Amz-Meta-"+entry.Key, entry.Value)
	}
	return out
}

// storedMetadata reads the standard headers and the user metadata from the HEAD answer of the provider. The values are
// the stored values: the gateway /s3 changes some of them, for example Cache-Control.
func storedMetadata(header http.Header) (objectHeaders, []metadataEntry) {
	var headers objectHeaders
	for _, field := range headers.fields() {
		*field.value = strings.Join(header.Values(field.name), ", ")
	}
	entries := make([]metadataEntry, 0)
	for name, values := range header {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-amz-meta-") && len(values) > 0 {
			entries = append(entries, metadataEntry{Key: strings.TrimPrefix(lower, "x-amz-meta-"), Value: strings.Join(values, ", ")})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	return headers, entries
}

// keptObjectHeaders gives the headers of the source that the copy must send again, or the provider changes them:
// the storage class, the redirect of a website and the server-side encryption with a key of the provider.
func keptObjectHeaders(header http.Header) http.Header {
	out := keptEncryptionHeaders(header)
	for _, name := range []string{"x-amz-storage-class", "x-amz-website-redirect-location"} {
		if value := strings.TrimSpace(header.Get(name)); value != "" {
			out.Set(name, value)
		}
	}
	return out
}

// isPreconditionFailed reports the 412 of a copy whose source does not have the ETag of x-amz-copy-source-if-match.
func isPreconditionFailed(err error) bool {
	var upstream *upstreamError
	return errors.As(err, &upstream) && (upstream.StatusCode == http.StatusPreconditionFailed || upstream.Code == "PreconditionFailed")
}

func metadataEditUnsupported() error {
	return apiError{Status: http.StatusNotImplemented, Code: "metadata_unsupported", Message: "this storage provider does not let Stowr change the metadata of an object"}
}

func objectChangedError() error {
	return apiError{Status: http.StatusPreconditionFailed, Code: "object_changed", Message: "the object changed after the form read its metadata: read the metadata again and make the change again"}
}

// --- S3 backend -----------------------------------------------------------------------------------------------------

// ReplaceObjectMetadata copies the object, or one version of it, onto its key with the headers (CopyObject with
// x-amz-metadata-directive: REPLACE). The tags stay (x-amz-tagging-directive: COPY). The header
// x-amz-copy-source-if-match makes the provider refuse a source with another ETag.
func (s *s3Backend) ReplaceObjectMetadata(ctx context.Context, key, version, etag string, headers http.Header) error {
	request := headers.Clone()
	if request == nil {
		request = make(http.Header)
	}
	request.Set("x-amz-copy-source", s.copySourceHeader(key, version))
	request.Set("x-amz-metadata-directive", "REPLACE")
	request.Set("x-amz-tagging-directive", "COPY")
	request.Set("x-amz-copy-source-if-match", `"`+etag+`"`)
	s.applyWriteEncryption(request)
	resp, err := s.do(ctx, http.MethodPut, key, nil, nil, 0, request)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return s3ResponseError(resp)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read s3 metadata copy response: %w", err)
	}
	// Some S3 implementations return HTTP 200 with an embedded Error document.
	var result struct {
		XMLName xml.Name
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if len(body) > 0 && xml.Unmarshal(body, &result) == nil && result.XMLName.Local == "Error" {
		return &upstreamError{StatusCode: http.StatusBadGateway, Code: result.Code, Message: result.Message}
	}
	return nil
}

// --- Storage instance -----------------------------------------------------------------------------------------------

// metadataEditAvailable tells if the instance can change the metadata of an object. Until the probe has run, and
// until the provider refuses an edit, the answer is yes for a backend with the edit.
func (s *storageInstance) metadataEditAvailable() bool {
	if s == nil {
		return false
	}
	if _, ok := s.backend.(ObjectMetadataEditor); !ok {
		return false
	}
	return !s.metadataOff.Load()
}

// probeMetadataEdit sends the copy of the edit for an object that does not exist, with an ETag that no object has. A
// provider with CopyObject answers "no such key" and writes nothing. A provider without it answers 501 or 405. Any
// other failure (denied, timeout) leaves the answer at yes. An instance without the write permission is not probed.
func (s *storageInstance) probeMetadataEdit(ctx context.Context) {
	editor, ok := s.backend.(ObjectMetadataEditor)
	if !ok || !s.permitted(permissionWrite) {
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := editor.ReplaceObjectMetadata(probeCtx, s.fullKey(metadataProbeKey), "", "stowr-probe", http.Header{})
	if providerRefusedFeature(err) {
		s.metadataOff.Store(true)
	}
}

// ReplaceMetadata gives the object, or the version that version names, the headers of metadata. etag and modified are
// the ETag and the modification date that the caller read; a zero modified is not compared. A HEAD request compares
// both, and the copy names the ETag again, so the content of an object that changes between the two requests is not
// overwritten. An older version becomes the current version.
func (s *storageInstance) ReplaceMetadata(ctx context.Context, key, version, etag string, modified time.Time, metadata http.Header) error {
	editor, ok := s.backend.(ObjectMetadataEditor)
	if !ok || s.metadataOff.Load() {
		return metadataEditUnsupported()
	}
	if version != "" {
		if _, err := s.versionStore(); err != nil {
			return err
		}
	}
	head, err := s.HeadVersion(ctx, key, version)
	if err != nil {
		return err
	}
	closeObjectResponse(head)
	if strings.Trim(strings.TrimSpace(head.Header.Get("ETag")), `"`) != etag {
		return objectChangedError()
	}
	if !modified.IsZero() {
		if stored, err := http.ParseTime(head.Header.Get("Last-Modified")); err != nil || !stored.Equal(modified) {
			return objectChangedError()
		}
	}
	if head.Header.Get("x-amz-server-side-encryption-customer-algorithm") != "" {
		return apiError{Status: http.StatusBadRequest, Code: "metadata_unsupported", Message: "the object is encrypted with a key of the customer (SSE-C): Stowr cannot copy it"}
	}
	kept := keptObjectHeaders(head.Header)
	size, _ := strconv.ParseInt(strings.TrimSpace(head.Header.Get("Content-Length")), 10, 64)
	if s.copiesInParts(size) {
		err = s.CopyMultipart(ctx, key, key, multipartCopyOptions{
			SourceVersion: version, SourceETag: etag, ReplaceMetadata: true, Metadata: metadata, ExtraHeaders: kept,
		})
	} else {
		headers := metadata.Clone()
		for name, values := range kept {
			headers[name] = values
		}
		release, gateErr := s.beginStorageRequest(ctx)
		if gateErr != nil {
			return gateErr
		}
		err = editor.ReplaceObjectMetadata(ctx, key, version, etag, headers)
		release()
		if providerRefusedFeature(err) {
			s.metadataOff.Store(true)
			return metadataEditUnsupported()
		}
		s.observePermission(permissionWrite, err)
	}
	if isPreconditionFailed(err) {
		return objectChangedError()
	}
	return err
}

// --- HTTP -----------------------------------------------------------------------------------------------------------

type metadataRequest struct {
	Instance string `json:"instance"`
	Key      string `json:"key"`
	Version  string `json:"version"`
	ETag     string `json:"etag"`
	// LastModified is the date of metadataResponse that the form read (RFC 3339). Empty means no comparison.
	LastModified string          `json:"lastModified"`
	Headers      objectHeaders   `json:"headers"`
	Metadata     []metadataEntry `json:"metadata"`
}

type metadataResponse struct {
	Instance     string          `json:"instance"`
	Key          string          `json:"key"`
	Version      string          `json:"version,omitempty"`
	ETag         string          `json:"etag"`
	LastModified string          `json:"lastModified,omitempty"`
	Size         int64           `json:"size"`
	Headers      objectHeaders   `json:"headers"`
	Metadata     []metadataEntry `json:"metadata"`
	// MultipartETag is true when the ETag has the form <digest>-<parts>: the object was uploaded in parts.
	MultipartETag bool `json:"multipartETag"`
	// CopyInParts is true when a save copies the object in parts (from multipartCopyThreshold up).
	CopyInParts      bool `json:"copyInParts"`
	MaxMetadataBytes int  `json:"maxMetadataBytes"`
}

var multipartETagPattern = regexp.MustCompile(`-[0-9]+$`)

// metadataState reads the stored metadata of the object, or of one version of it, with a HEAD request.
func metadataState(ctx context.Context, instance *storageInstance, key, version string) (metadataResponse, error) {
	head, err := instance.HeadVersion(ctx, instance.fullKey(key), version)
	if err != nil {
		return metadataResponse{}, err
	}
	closeObjectResponse(head)
	headers, entries := storedMetadata(head.Header)
	etag := strings.Trim(strings.TrimSpace(head.Header.Get("ETag")), `"`)
	size, _ := strconv.ParseInt(strings.TrimSpace(head.Header.Get("Content-Length")), 10, 64)
	response := metadataResponse{
		Instance: instance.cfg.ID, Key: key, Version: strings.TrimSpace(head.Header.Get("x-amz-version-id")),
		ETag: etag, Size: size, Headers: headers, Metadata: entries,
		MultipartETag: multipartETagPattern.MatchString(etag), CopyInParts: instance.copiesInParts(size),
		MaxMetadataBytes: maxUserMetadataBytes,
	}
	if modified, err := http.ParseTime(head.Header.Get("Last-Modified")); err == nil {
		response.LastModified = modified.UTC().Format(time.RFC3339)
	}
	return response, nil
}

// handleMetadata reads (GET) and replaces (PUT) the metadata of an object. The answer of a PUT is the new state.
func (a *application) handleMetadata(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		key := cleanRelativeKey(r.URL.Query().Get("key"))
		instance, err := a.instanceFromRequest(r, "")
		if err != nil {
			writeAPIError(w, err)
			return
		}
		if err := requirePermission(instance, permissionRead); err != nil {
			writeAPIError(w, err)
			return
		}
		if key == "" {
			writeAPIError(w, apiError{Status: http.StatusBadRequest, Code: "invalid_key", Message: "object key is required"})
			return
		}
		if _, err := a.authorizeRead(r, instance, key); err != nil {
			writeAPIError(w, err)
			return
		}
		if !instance.metadataEditAvailable() {
			writeAPIError(w, metadataEditUnsupported())
			return
		}
		response, err := metadataState(r.Context(), instance, key, strings.TrimSpace(r.URL.Query().Get("version")))
		if err != nil {
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, response)
	case http.MethodPut:
		var request metadataRequest
		if err := decodeJSONBody(r, &request); err != nil {
			writeAPIError(w, err)
			return
		}
		key := cleanRelativeKey(request.Key)
		instance, err := a.instanceFromRequest(r, request.Instance)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		if err := requirePermission(instance, permissionWrite); err != nil {
			writeAPIError(w, err)
			return
		}
		if key == "" {
			writeAPIError(w, apiError{Status: http.StatusBadRequest, Code: "invalid_key", Message: "object key is required"})
			return
		}
		for _, check := range []error{validateETag(request.ETag), validateObjectHeaders(request.Headers), validateUserMetadata(request.Metadata)} {
			if check != nil {
				writeAPIError(w, check)
				return
			}
		}
		version := request.Version
		if version != "" && (len(version) > 1024 || !printableASCII(version)) {
			writeAPIError(w, invalidMetadata("version must be the id of a version of the object, got %q", version))
			return
		}
		var modified time.Time
		if request.LastModified != "" {
			if modified, err = time.Parse(time.RFC3339, request.LastModified); err != nil {
				writeAPIError(w, invalidMetadata("lastModified must be the date that the metadata request gave (RFC 3339), got %q", request.LastModified))
				return
			}
		}
		metadata := metadataRequestHeaders(request.Headers, request.Metadata)
		if err := instance.ReplaceMetadata(r.Context(), instance.fullKey(key), version, request.ETag, modified, metadata); err != nil {
			writeAPIError(w, err)
			return
		}
		response, err := metadataState(r.Context(), instance, key, "")
		if err != nil {
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, response)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPut)
	}
}
