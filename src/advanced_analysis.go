package main

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 -- used only for provider metadata comparison, never as a security primitive.
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
)

const analysisInlineWait = 100 * time.Millisecond

type objectReference struct {
	Instance string `json:"instance,omitempty"`
	Key      string `json:"key"`
	Version  string `json:"version,omitempty"`
}

type integrityRequest struct {
	Key     string `json:"key"`
	Version string `json:"version,omitempty"`
}

type integrityEntry struct {
	Key               string            `json:"key"`
	Version           string            `json:"version,omitempty"`
	Size              int64             `json:"size"`
	ContentType       string            `json:"contentType,omitempty"`
	ETag              string            `json:"etag,omitempty"`
	LastModified      string            `json:"lastModified,omitempty"`
	SHA256            string            `json:"sha256"`
	SHA256Base64      string            `json:"sha256Base64"`
	MD5               string            `json:"md5"`
	MD5Base64         string            `json:"md5Base64"`
	CRC32             string            `json:"crc32"`
	CRC32C            string            `json:"crc32c"`
	ProviderChecksums map[string]string `json:"providerChecksums,omitempty"`
	Matches           map[string]bool   `json:"matches,omitempty"`
}

type integrityResult struct {
	Entries []integrityEntry `json:"entries"`
}

type inspectProbe struct {
	Name  string `json:"name"`
	Range string `json:"range"`
	Bytes int    `json:"bytes"`
	Hex   string `json:"hex"`
	ASCII string `json:"ascii"`
}

type inspectResponse struct {
	Instance       string            `json:"instance"`
	Key            string            `json:"key"`
	Version        string            `json:"version,omitempty"`
	Provider       string            `json:"provider"`
	DetectedKind   string            `json:"detectedKind"`
	DetectedMIME   string            `json:"detectedMime,omitempty"`
	DeclaredMIME   string            `json:"declaredMime,omitempty"`
	Size           int64             `json:"size"`
	ETag           string            `json:"etag,omitempty"`
	Generation     string            `json:"generation,omitempty"`
	LastModified   string            `json:"lastModified,omitempty"`
	Headers        map[string]string `json:"headers"`
	ProviderHashes map[string]string `json:"providerChecksums,omitempty"`
	Probes         []inspectProbe    `json:"probes"`
	Structure      map[string]string `json:"structure,omitempty"`
	Resources      resourceUsage     `json:"resources"`
}

func (a *application) handleVersions(w http.ResponseWriter, r *http.Request) {
	instance, err := a.instanceFromRequest(r, "")
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if !instance.versioningAvailable() {
		writeAPIError(w, apiError{Status: http.StatusNotImplemented, Code: "versioning_unavailable", Message: "object versioning is not available for this storage bucket"})
		return
	}
	key := cleanRelativeKey(r.URL.Query().Get("key"))
	if key == "" {
		writeAPIError(w, apiError{Status: http.StatusBadRequest, Code: "invalid_key", Message: "object key is required"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		if err := requirePermission(instance, permissionRead); err != nil {
			writeAPIError(w, err)
			return
		}
		if _, err := a.authorizeRead(r, instance, key); err != nil {
			writeAPIError(w, err)
			return
		}
		maximum := int(parseInt64Default(r.URL.Query().Get("max"), 250))
		if maximum < 1 || maximum > 1000 {
			maximum = 250
		}
		page, err := instance.ListObjectVersions(r.Context(), instance.fullKey(key), r.URL.Query().Get("pageToken"), maximum)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"instance": instance.cfg.ID, "key": key,
			"versions": page.Versions, "nextPageToken": page.NextPageToken,
		})
	case http.MethodDelete:
		if err := requirePermission(instance, permissionDelete); err != nil {
			writeAPIError(w, err)
			return
		}
		if _, err := a.authorizeUse(r, actionObjectDelete, instance, key); err != nil {
			writeAPIError(w, err)
			return
		}
		if err := instance.DeleteVersion(r.Context(), instance.fullKey(key), strings.TrimSpace(r.URL.Query().Get("version"))); err != nil {
			writeAPIError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodDelete)
	}
}

type versionCountsRequest struct {
	Instance string   `json:"instance"`
	Keys     []string `json:"keys"`
}

type versionCountResult struct {
	Count          int    `json:"count"`
	CurrentVersion string `json:"currentVersion,omitempty"`
}

func (a *application) handleVersionCounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var request versionCountsRequest
	if err := decodeJSONBody(r, &request); err != nil {
		writeAPIError(w, err)
		return
	}
	instance, err := a.instanceFromRequest(r, request.Instance)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if !instance.versioningAvailable() {
		writeAPIError(w, apiError{Status: http.StatusNotImplemented, Code: "versioning_unavailable", Message: "object versioning is not available for this storage bucket"})
		return
	}
	if err := requirePermission(instance, permissionRead); err != nil {
		writeAPIError(w, err)
		return
	}
	if len(request.Keys) == 0 || len(request.Keys) > 100 {
		writeAPIError(w, apiError{Status: http.StatusBadRequest, Code: "invalid_keys", Message: "between 1 and 100 object keys are required"})
		return
	}
	keys := make([]string, 0, len(request.Keys))
	seen := make(map[string]struct{}, len(request.Keys))
	for _, rawKey := range request.Keys {
		key := cleanRelativeKey(rawKey)
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	// The first key is the use. The other keys are the other permissions.
	if _, err := a.authorizeReadMany(r, instance, keys); err != nil {
		writeAPIError(w, err)
		return
	}
	counts := make(map[string]versionCountResult, len(keys))
	errorsByKey := make(map[string]string)
	for _, key := range keys {
		count, current, err := countObjectVersions(r.Context(), instance, instance.fullKey(key))
		if err != nil {
			errorsByKey[key] = publicStorageError(err)
			continue
		}
		counts[key] = versionCountResult{Count: count, CurrentVersion: current}
	}
	response := map[string]any{"instance": instance.cfg.ID, "counts": counts}
	if len(errorsByKey) > 0 {
		response["errors"] = errorsByKey
	}
	writeJSON(w, http.StatusOK, response)
}

func countObjectVersions(ctx context.Context, instance *storageInstance, fullKey string) (int, string, error) {
	count := 0
	current := ""
	pageToken := ""
	seenTokens := make(map[string]struct{})
	for {
		page, err := instance.ListObjectVersions(ctx, fullKey, pageToken, 1000)
		if err != nil {
			return 0, "", err
		}
		for _, version := range page.Versions {
			if version.DeleteMarker {
				continue
			}
			count++
			if version.IsCurrent {
				current = version.Version
			}
		}
		if page.NextPageToken == "" {
			return count, current, nil
		}
		if page.NextPageToken == pageToken {
			return 0, "", fmt.Errorf("provider returned the same version page token twice")
		}
		if _, exists := seenTokens[page.NextPageToken]; exists {
			return 0, "", fmt.Errorf("provider returned a repeated version page token")
		}
		seenTokens[page.NextPageToken] = struct{}{}
		pageToken = page.NextPageToken
	}
}

func (a *application) handleVersionRestore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var request struct {
		Instance string `json:"instance"`
		Key      string `json:"key"`
		Version  string `json:"version"`
	}
	if err := decodeJSONBody(r, &request); err != nil {
		writeAPIError(w, err)
		return
	}
	instance, err := a.instanceFromRequest(r, request.Instance)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if !instance.versioningAvailable() {
		writeAPIError(w, apiError{Status: http.StatusNotImplemented, Code: "versioning_unavailable", Message: "object versioning is not available for this storage bucket"})
		return
	}
	for _, permission := range []string{permissionRead, permissionWrite} {
		if err := requirePermission(instance, permission); err != nil {
			writeAPIError(w, err)
			return
		}
	}
	key := cleanRelativeKey(request.Key)
	if key == "" || strings.TrimSpace(request.Version) == "" {
		writeAPIError(w, apiError{Status: http.StatusBadRequest, Code: "invalid_version", Message: "key and version are required"})
		return
	}
	if _, err := a.authorize(r, a.restoreOperation(instance, key)); err != nil {
		writeAPIError(w, err)
		return
	}
	if err := instance.RestoreVersion(r.Context(), instance.fullKey(key), request.Version); err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"restored": true, "key": key, "version": request.Version})
}

func (a *application) handleIntegrity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var request struct {
		Instance string `json:"instance"`
		integrityRequest
	}
	if err := decodeJSONBody(r, &request); err != nil {
		writeAPIError(w, err)
		return
	}
	instance, err := a.instanceFromRequest(r, request.Instance)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	request.Key = cleanRelativeKey(request.Key)
	if err := requirePermission(instance, permissionRead); err != nil {
		writeAPIError(w, err)
		return
	}
	if request.Key == "" {
		writeAPIError(w, apiError{Status: http.StatusBadRequest, Code: "invalid_integrity_request", Message: "an object key is required"})
		return
	}
	if _, err := a.authorizeRead(r, instance, request.Key); err != nil {
		writeAPIError(w, err)
		return
	}
	job, err := a.jobs.create(jobState{Type: jobTypeIntegrity, Instance: instance.cfg.ID, IntegrityRequest: &request.integrityRequest,
		Ceilings: accessCeilingsOf(r.Context())})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	a.writeAnalysisJobResponse(w, r, job)
}

func (a *application) writeAnalysisJobResponse(w http.ResponseWriter, r *http.Request, job jobState) {
	if completed, terminal := a.jobs.waitForTerminal(r.Context(), job.ID, analysisInlineWait); terminal {
		if completed.Status == jobStatusFailed {
			if completed.Refusal != nil {
				writeAPIError(w, *completed.Refusal)
				return
			}
			writeAPIError(w, apiError{Status: http.StatusUnprocessableEntity, Code: "analysis_failed", Message: completed.Error})
			return
		}
		writeJSON(w, http.StatusOK, completed.public())
		return
	}
	writeJSON(w, http.StatusAccepted, job.public())
}

func (a *application) handleInspect(w http.ResponseWriter, r *http.Request) {
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
	version := strings.TrimSpace(r.URL.Query().Get("version"))
	if key == "" {
		writeAPIError(w, apiError{Status: http.StatusBadRequest, Code: "invalid_key", Message: "object key is required"})
		return
	}
	if _, err := a.authorizeRead(r, instance, key); err != nil {
		writeAPIError(w, err)
		return
	}
	budget := budgetFromContext(r.Context())
	before := resourceUsage{}
	if budget != nil {
		before = budget.usage()
	}
	result, err := inspectObject(r.Context(), instance, objectReference{Instance: instance.cfg.ID, Key: key, Version: version})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if budget != nil {
		after := budget.usage()
		result.Resources = resourceUsage{
			StorageRequests: after.StorageRequests - before.StorageRequests,
			StorageBytes:    after.StorageBytes - before.StorageBytes,
			StartedAt:       before.StartedAt,
			ElapsedMS:       after.ElapsedMS - before.ElapsedMS,
		}
	}
	writeJSON(w, http.StatusOK, result)
}

func computeObjectIntegrity(ctx context.Context, app *application, ref objectReference) (integrityEntry, error) {
	instance := app.instances[ref.Instance]
	if instance == nil {
		return integrityEntry{}, apiError{Status: http.StatusNotFound, Code: "unknown_instance", Message: "storage instance was not found"}
	}
	key := cleanRelativeKey(ref.Key)
	if key == "" {
		return integrityEntry{}, apiError{Status: http.StatusBadRequest, Code: "invalid_key", Message: "object key is required"}
	}
	fullKey := instance.fullKey(key)
	head, err := instance.HeadVersion(ctx, fullKey, ref.Version)
	if err != nil {
		return integrityEntry{}, err
	}
	defer closeObjectResponse(head)
	expectedSize := int64(-1)
	if rawSize := strings.TrimSpace(head.Header.Get("Content-Length")); rawSize != "" {
		parsed, parseErr := strconv.ParseInt(rawSize, 10, 64)
		if parseErr != nil || parsed < 0 {
			return integrityEntry{}, fmt.Errorf("provider returned an invalid Content-Length for integrity verification")
		}
		expectedSize = parsed
	}
	requestHeaders := make(http.Header)
	if etag := strings.TrimSpace(head.Header.Get("ETag")); etag != "" {
		requestHeaders.Set("If-Match", etag)
	}
	response, err := instance.GetVersion(ctx, fullKey, ref.Version, requestHeaders)
	if err != nil {
		return integrityEntry{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return integrityEntry{}, fmt.Errorf("provider returned HTTP %d instead of a complete object for integrity verification", response.StatusCode)
	}
	sha := sha256.New()
	md := md5.New() // #nosec G401 -- used only to compare a provider-supplied digest.
	crcIEEE := crc32.NewIEEE()
	crcCastagnoli := crc32.New(crc32.MakeTable(crc32.Castagnoli))
	written, err := io.Copy(io.MultiWriter(sha, md, crcIEEE, crcCastagnoli), response.Body)
	if err != nil {
		return integrityEntry{}, err
	}
	if expectedSize >= 0 && written != expectedSize {
		return integrityEntry{}, fmt.Errorf("object size changed while integrity was being verified")
	}
	shaSum := sha.Sum(nil)
	md5Sum := md.Sum(nil)
	provider := providerChecksums(head.Header)
	matches := make(map[string]bool)
	if value := provider["sha256"]; value != "" {
		if matched, comparable := checksumMatches(value, shaSum); comparable {
			matches["sha256"] = matched
		}
	}
	if value := provider["md5"]; value != "" {
		if matched, comparable := checksumMatches(value, md5Sum); comparable {
			matches["md5"] = matched
		}
	}
	if value := provider["crc32"]; value != "" {
		if matched, comparable := checksumMatchesUint32(value, crcIEEE.Sum32()); comparable {
			matches["crc32"] = matched
		}
	}
	if value := provider["crc32c"]; value != "" {
		if matched, comparable := checksumMatchesUint32(value, crcCastagnoli.Sum32()); comparable {
			matches["crc32c"] = matched
		}
	}
	etag := strings.Trim(strings.TrimSpace(head.Header.Get("ETag")), `"`)
	if isSimpleMD5ETag(etag) {
		matches["etagMd5"] = strings.EqualFold(etag, hex.EncodeToString(md5Sum))
	}
	if len(matches) == 0 {
		matches = nil
	}
	return integrityEntry{
		Key: key, Version: ref.Version, Size: written,
		ContentType: head.Header.Get("Content-Type"), ETag: etag,
		LastModified: head.Header.Get("Last-Modified"),
		SHA256:       hex.EncodeToString(shaSum), SHA256Base64: base64.StdEncoding.EncodeToString(shaSum),
		MD5: hex.EncodeToString(md5Sum), MD5Base64: base64.StdEncoding.EncodeToString(md5Sum),
		CRC32: fmt.Sprintf("%08x", crcIEEE.Sum32()), CRC32C: fmt.Sprintf("%08x", crcCastagnoli.Sum32()),
		ProviderChecksums: provider, Matches: matches,
	}, nil
}

func checksumMatches(value string, sum []byte) (matched bool, comparable bool) {
	value = strings.TrimSpace(strings.Trim(value, `"`))
	if value == "" {
		return false, false
	}
	if strings.EqualFold(value, hex.EncodeToString(sum)) || value == base64.StdEncoding.EncodeToString(sum) {
		return true, true
	}
	if decoded, err := base64.StdEncoding.DecodeString(value); err == nil && len(decoded) == len(sum) {
		return bytes.Equal(decoded, sum), true
	}
	return false, false
}

func checksumMatchesUint32(value string, sum uint32) (matched bool, comparable bool) {
	bytesValue := []byte{byte(sum >> 24), byte(sum >> 16), byte(sum >> 8), byte(sum)}
	return checksumMatches(value, bytesValue)
}

func isSimpleMD5ETag(value string) bool {
	if len(value) != 32 || strings.Contains(value, "-") {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func runIntegrityAnalysis(ctx context.Context, manager *jobManager, job *jobState) error {
	if job.IntegrityRequest == nil {
		return fmt.Errorf("integrity request is missing")
	}
	if err := manager.controlState(job.ID); err != nil {
		return err
	}
	entry, err := computeObjectIntegrity(ctx, manager.app, objectReference{
		Instance: job.Instance, Key: job.IntegrityRequest.Key, Version: job.IntegrityRequest.Version,
	})
	if err != nil {
		return err
	}
	job.Processed = 1
	job.Integrity = &integrityResult{Entries: []integrityEntry{entry}}
	return manager.put(*job)
}

func inspectObject(ctx context.Context, instance *storageInstance, ref objectReference) (inspectResponse, error) {
	fullKey := instance.fullKey(ref.Key)
	head, err := instance.HeadVersion(ctx, fullKey, ref.Version)
	if err != nil {
		return inspectResponse{}, err
	}
	defer closeObjectResponse(head)
	rawSize := strings.TrimSpace(head.Header.Get("Content-Length"))
	if rawSize == "" {
		return inspectResponse{}, apiError{Status: http.StatusBadGateway, Code: "missing_content_length", Message: "the storage provider did not return Content-Length for technical inspection"}
	}
	size, parseErr := strconv.ParseInt(rawSize, 10, 64)
	if parseErr != nil || size < 0 {
		return inspectResponse{}, apiError{Status: http.StatusBadGateway, Code: "invalid_content_length", Message: "the storage provider returned an invalid Content-Length for technical inspection"}
	}
	etag := strings.TrimSpace(head.Header.Get("ETag"))
	result := inspectResponse{
		Instance: instance.cfg.ID, Key: ref.Key, Version: ref.Version, Provider: instance.cfg.Provider,
		Size: size, ETag: strings.Trim(etag, `"`), Generation: head.Header.Get("x-goog-generation"),
		LastModified: head.Header.Get("Last-Modified"), DeclaredMIME: head.Header.Get("Content-Type"),
		Headers: safeInspectionHeaders(head.Header), ProviderHashes: providerChecksums(head.Header), Structure: make(map[string]string),
	}
	probeSize := int64(64 << 10)
	if size > 0 && probeSize > size {
		probeSize = size
	}
	first, err := readInspectionRange(ctx, instance, fullKey, ref.Version, 0, probeSize, size, etag)
	if err != nil {
		return inspectResponse{}, err
	}
	result.Probes = append(result.Probes, inspectionProbe("Header", 0, first))
	var suffix []byte
	if size > probeSize {
		start := size - probeSize
		suffix, err = readInspectionRange(ctx, instance, fullKey, ref.Version, start, probeSize, size, etag)
		if err != nil {
			return inspectResponse{}, err
		}
		result.Probes = append(result.Probes, inspectionProbe("Trailer", start, suffix))
	}
	detected := http.DetectContentType(first)
	if inferred := mime.TypeByExtension(strings.ToLower(path.Ext(ref.Key))); inferred != "" && (detected == "application/octet-stream" || detected == "text/plain; charset=utf-8") {
		detected = inferred
	}
	result.DetectedMIME = detected
	result.DetectedKind = detectKind(ref.Key, detected)
	inspectStructure(result.Structure, first, suffix, size)
	if len(result.Structure) == 0 {
		result.Structure = nil
	}
	return result, nil
}

func readInspectionRange(ctx context.Context, instance *storageInstance, fullKey, version string, start, length, expectedTotal int64, etag string) ([]byte, error) {
	if length <= 0 {
		return nil, nil
	}
	end := start + length - 1
	headers := make(http.Header)
	headers.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	headers.Set("Accept-Encoding", "identity")
	if strings.TrimSpace(etag) != "" {
		headers.Set("If-Match", etag)
	}
	response, err := instance.GetVersion(ctx, fullKey, version, headers)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusPartialContent {
		return nil, apiError{Status: http.StatusBadGateway, Code: "range_not_honored", Message: "the storage provider did not honor the inspection byte-range request"}
	}
	contentRange, total, parseErr := parseGatewayContentRange(response.Header.Get("Content-Range"))
	if parseErr != nil || contentRange.start != start || contentRange.end != end || total != expectedTotal {
		return nil, apiError{Status: http.StatusBadGateway, Code: "invalid_content_range", Message: "the storage provider returned an unexpected Content-Range for technical inspection"}
	}
	if rawLength := strings.TrimSpace(response.Header.Get("Content-Length")); rawLength != "" {
		returnedLength, lengthErr := strconv.ParseInt(rawLength, 10, 64)
		if lengthErr != nil || returnedLength != length {
			return nil, apiError{Status: http.StatusBadGateway, Code: "invalid_content_length", Message: "the storage provider returned an unexpected ranged Content-Length for technical inspection"}
		}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, length+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != length {
		return nil, apiError{Status: http.StatusBadGateway, Code: "incomplete_range", Message: "the storage provider returned an incomplete byte range for technical inspection"}
	}
	return data, nil
}

func inspectionProbe(name string, start int64, data []byte) inspectProbe {
	preview := data
	if len(preview) > 256 {
		preview = preview[:256]
	}
	ascii := make([]byte, len(preview))
	for index, value := range preview {
		if value >= 32 && value <= 126 {
			ascii[index] = value
		} else {
			ascii[index] = '.'
		}
	}
	end := start + int64(len(data)) - 1
	rangeLabel := fmt.Sprintf("bytes=%d-%d", start, end)
	if len(data) == 0 {
		rangeLabel = "empty"
	}
	return inspectProbe{Name: name, Range: rangeLabel, Bytes: len(data), Hex: hex.EncodeToString(preview), ASCII: string(ascii)}
}

func safeInspectionHeaders(header http.Header) map[string]string {
	out := make(map[string]string)
	for name, values := range header {
		lower := strings.ToLower(name)
		if lower == "authorization" || lower == "cookie" || lower == "set-cookie" || strings.Contains(lower, "credential") || strings.Contains(lower, "token") {
			continue
		}
		out[http.CanonicalHeaderKey(name)] = strings.Join(values, ", ")
	}
	return out
}

func inspectStructure(output map[string]string, first, suffix []byte, size int64) {
	if bytes.HasPrefix(first, []byte("%PDF-")) {
		line := first
		if index := bytes.IndexByte(line, '\n'); index >= 0 {
			line = line[:index]
		}
		output["Format"] = "PDF"
		output["PDF version"] = strings.TrimPrefix(strings.TrimSpace(string(line)), "%PDF-")
		if index := bytes.LastIndex(suffix, []byte("startxref")); index >= 0 {
			fields := strings.Fields(string(suffix[index:]))
			if len(fields) > 1 {
				output["Start xref"] = fields[1]
			}
		}
		return
	}
	if bytes.HasPrefix(first, []byte("PK\x03\x04")) || bytes.Contains(suffix, []byte("PK\x05\x06")) {
		output["Format"] = "ZIP-compatible archive"
		output["Central directory"] = "Present"
		return
	}
	if bytes.HasPrefix(first, []byte("SQLite format 3\x00")) {
		output["Format"] = "SQLite 3"
		if len(first) >= 18 {
			pageSize := int(first[16])<<8 | int(first[17])
			if pageSize == 1 {
				pageSize = 65536
			}
			output["Page size"] = strconv.Itoa(pageSize)
		}
		return
	}
	if bytes.HasPrefix(first, []byte("\x89PNG\r\n\x1a\n")) {
		output["Format"] = "PNG"
		return
	}
	if bytes.HasPrefix(first, []byte("\xff\xd8\xff")) {
		output["Format"] = "JPEG"
		return
	}
	if len(first) >= 12 && string(first[4:8]) == "ftyp" {
		output["Format"] = "ISO Base Media"
		output["Major brand"] = string(first[8:12])
		return
	}
	if size >= 0 {
		output["Object length"] = strconv.FormatInt(size, 10)
	}
}

func cloneIntegrityRequest(value *integrityRequest) *integrityRequest {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func cloneIntegrityResult(value *integrityResult) *integrityResult {
	if value == nil {
		return nil
	}
	copyValue := *value
	copyValue.Entries = append([]integrityEntry(nil), value.Entries...)
	for index := range copyValue.Entries {
		copyValue.Entries[index].ProviderChecksums = cloneStringMap(value.Entries[index].ProviderChecksums)
		copyValue.Entries[index].Matches = cloneBoolMap(value.Entries[index].Matches)
	}
	return &copyValue
}

func cloneStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	out := make(map[string]string, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

func cloneBoolMap(source map[string]bool) map[string]bool {
	if source == nil {
		return nil
	}
	out := make(map[string]bool, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}
