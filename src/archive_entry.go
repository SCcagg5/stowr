package main

import (
	"archive/zip"
	"context"
	"crypto/md5" // #nosec G501 -- used only to compare a provider-supplied digest.
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"io"
	"mime"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"
)

type archiveExtractRequest struct {
	Instance       string   `json:"instance"`
	Key            string   `json:"key"`
	Version        string   `json:"version,omitempty"`
	Entries        []string `json:"entries"`
	TargetInstance string   `json:"targetInstance,omitempty"`
	Target         string   `json:"target"`
}

type archiveExtractResult struct {
	ArchiveKey     string    `json:"archiveKey"`
	Version        string    `json:"version,omitempty"`
	TargetInstance string    `json:"targetInstance"`
	Target         string    `json:"target"`
	Extracted      int64     `json:"extracted"`
	Bytes          int64     `json:"bytes"`
	CompletedAt    time.Time `json:"completedAt,omitempty"`
}

type archiveEntryIntegrityRequest struct {
	Key     string `json:"key"`
	Version string `json:"version,omitempty"`
	Entry   string `json:"entry"`
}

type archiveEntryIntegrityResult struct {
	ArchiveKey     string `json:"archiveKey"`
	Version        string `json:"version,omitempty"`
	Entry          string `json:"entry"`
	Size           int64  `json:"size"`
	CompressedSize int64  `json:"compressedSize"`
	SHA256         string `json:"sha256"`
	MD5            string `json:"md5"`
	CRC32          string `json:"crc32"`
	ExpectedCRC32  string `json:"expectedCrc32"`
	CRC32Matches   bool   `json:"crc32Matches"`
}

type archiveCountingReader struct {
	reader  io.Reader
	ctx     context.Context
	manager *jobManager
	jobID   string
	count   int64
}

func (r *archiveCountingReader) Read(buffer []byte) (int, error) {
	if r.ctx != nil {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
	}
	if r.manager != nil && r.jobID != "" {
		if err := r.manager.controlState(r.jobID); err != nil {
			return 0, err
		}
	}
	count, err := r.reader.Read(buffer)
	r.count += int64(count)
	return count, err
}

func (a *application) handleArchiveEntry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	// Same sandbox as the raw object routes: member bytes are bucket-author
	// controlled and must never run with the application origin.
	applyObjectSafetyHeaders(w.Header())
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
	entryName := strings.TrimSpace(r.URL.Query().Get("entry"))
	if key == "" || entryName == "" {
		writeAPIError(w, apiError{Status: http.StatusBadRequest, Code: "invalid_archive_entry", Message: "archive key and entry are required"})
		return
	}
	if _, err := a.authorizeRead(r, instance, key); err != nil {
		writeAPIError(w, err)
		return
	}
	listed := mediaSourceMetadata{
		Size:         parseInt64Default(r.URL.Query().Get("size"), 0),
		ETag:         strings.TrimSpace(r.URL.Query().Get("etag")),
		LastModified: strings.TrimSpace(r.URL.Query().Get("lastModified")),
	}
	reader, err := openArchiveReader(r.Context(), instance, key, version, listed, a.config.Runtime.MaxArchiveEntries)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	file, err := exactArchiveEntry(reader, entryName)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if err := validateSelectableArchiveEntry(file, a.config.Runtime.MaxStorageBytesPerRequest); err != nil {
		writeAPIError(w, err)
		return
	}
	filename := path.Base(file.Name)
	if filename == "." || filename == "/" || filename == "" {
		filename = "archive-entry"
	}
	contentType := previewContentType(filename)
	if contentType == "" {
		contentType = mime.TypeByExtension(strings.ToLower(path.Ext(filename)))
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	disposition := "attachment"
	if r.URL.Query().Get("inline") == "1" && browserInlineContent(filename, contentType) {
		disposition = "inline"
	}
	if archiveEntryActiveContent(filename, contentType) {
		// Archive members are authored by whoever can write the bucket and are
		// served from the application origin. Never hand a browser an active
		// document or a script type. The only exception is an SVG loaded as an
		// image sub-resource, which cannot run script.
		if !(isSVGContent(filename, contentType) && r.Header.Get("Sec-Fetch-Dest") == "image") {
			contentType = "application/octet-stream"
			disposition = "attachment"
		}
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", file.UncompressedSize64))
	w.Header().Set("Cache-Control", "private, no-store")
	if value := mime.FormatMediaType(disposition, map[string]string{"filename": filename}); value != "" {
		w.Header().Set("Content-Disposition", value)
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	body, err := file.Open()
	if err != nil {
		writeAPIError(w, archiveEntryOpenError(file.Name, err))
		return
	}
	defer body.Close()
	w.WriteHeader(http.StatusOK)
	written, copyErr := copyStream(w, io.LimitReader(body, int64(file.UncompressedSize64)+1))
	if copyErr != nil || written != int64(file.UncompressedSize64) {
		// The response headers are already committed. Terminating the response is
		// safer than appending a misleading error body to a truncated member.
		return
	}
}

// archiveEntryActiveContent reports members that a browser could execute as a
// document or script on the application origin.
func archiveEntryActiveContent(filename, contentType string) bool {
	switch strings.ToLower(path.Ext(filename)) {
	case ".html", ".htm", ".xhtml", ".xht", ".shtml", ".svg", ".svgz", ".xml", ".xsl", ".xslt", ".js", ".mjs", ".cjs", ".wasm", ".hta":
		return true
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		mediaType = strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0])
	}
	mediaType = strings.ToLower(mediaType)
	return mediaType == "text/html" || mediaType == "image/svg+xml" || mediaType == "text/xml" || mediaType == "application/xml" ||
		mediaType == "application/wasm" || mediaType == "text/xsl" || strings.HasSuffix(mediaType, "+xml") ||
		strings.Contains(mediaType, "javascript") || strings.Contains(mediaType, "ecmascript")
}

func isSVGContent(filename, contentType string) bool {
	return strings.EqualFold(path.Ext(filename), ".svg") || strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "image/svg+xml")
}

func (a *application) handleArchiveExtract(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var request archiveExtractRequest
	if err := decodeJSONBody(r, &request); err != nil {
		writeAPIError(w, err)
		return
	}
	source, err := a.instanceFromRequest(r, request.Instance)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if strings.TrimSpace(request.TargetInstance) == "" {
		request.TargetInstance = source.cfg.ID
	}
	target := a.instances[request.TargetInstance]
	if target == nil {
		writeAPIError(w, apiError{Status: http.StatusNotFound, Code: "unknown_target_instance", Message: "target storage instance was not found"})
		return
	}
	if err := requirePermission(source, permissionRead); err != nil {
		writeAPIError(w, err)
		return
	}
	if err := requirePermission(target, permissionWrite); err != nil {
		writeAPIError(w, err)
		return
	}
	if err := requirePermission(target, permissionRead); err != nil {
		writeAPIError(w, err)
		return
	}
	request.Key = cleanRelativeKey(request.Key)
	request.Version = strings.TrimSpace(request.Version)
	request.Target = normalizePrefix(cleanRelativeKey(request.Target))
	request.Entries, err = normalizeArchiveEntrySelection(request.Entries, a.config.Runtime.MaxArchiveEntries)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if request.Key == "" {
		writeAPIError(w, apiError{Status: http.StatusBadRequest, Code: "invalid_archive", Message: "archive key is required"})
		return
	}
	job, err := a.jobs.create(jobState{
		Type: jobTypeExtractArchive, Instance: source.cfg.ID, TargetInstance: target.cfg.ID,
		Source: request.Key, Version: request.Version, Target: request.Target, Entries: request.Entries,
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, job.public())
}

func (a *application) handleArchiveEntryIntegrity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var request struct {
		Instance string `json:"instance"`
		archiveEntryIntegrityRequest
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
	request.Version = strings.TrimSpace(request.Version)
	request.Entry = strings.TrimSpace(request.Entry)
	if request.Key == "" || request.Entry == "" {
		writeAPIError(w, apiError{Status: http.StatusBadRequest, Code: "invalid_archive_entry", Message: "archive key and entry are required"})
		return
	}
	if _, err := a.authorizeRead(r, instance, request.Key); err != nil {
		writeAPIError(w, err)
		return
	}
	payload := request.archiveEntryIntegrityRequest
	job, err := a.jobs.create(jobState{Type: jobTypeArchiveEntryIntegrity, Instance: instance.cfg.ID, ArchiveEntryIntegrityRequest: &payload})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	a.writeAnalysisJobResponse(w, r, job)
}

func normalizeArchiveEntrySelection(entries []string, maximum int) ([]string, error) {
	if maximum <= 0 {
		maximum = defaultMaxArchiveEntries
	}
	if len(entries) == 0 {
		return nil, apiError{Status: http.StatusBadRequest, Code: "empty_archive_selection", Message: "select at least one archive entry"}
	}
	if len(entries) > maximum {
		return nil, apiError{Status: http.StatusRequestEntityTooLarge, Code: "archive_selection_limit", Message: fmt.Sprintf("the selection exceeds the configured limit of %d entries", maximum)}
	}
	seen := make(map[string]struct{}, len(entries))
	result := make([]string, 0, len(entries))
	for _, raw := range entries {
		name := strings.TrimSpace(raw)
		if name == "" || strings.ContainsRune(name, '\x00') {
			return nil, apiError{Status: http.StatusBadRequest, Code: "invalid_archive_entry", Message: "the archive selection contains an invalid entry name"}
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	sort.Strings(result)
	return result, nil
}

func openArchiveReader(ctx context.Context, instance *storageInstance, key, version string, listed mediaSourceMetadata, maxEntries int) (*zip.Reader, error) {
	extension := strings.ToLower(strings.TrimPrefix(path.Ext(key), "."))
	if !isZipArchiveExtension(extension) {
		return nil, apiError{Status: http.StatusUnsupportedMediaType, Code: "unsupported_archive", Message: "selective extraction is available only for indexed ZIP-compatible archives"}
	}
	source, err := openStructuredRangeSourceVersion(ctx, instance, key, version, listed)
	if err != nil {
		return nil, err
	}
	reader, err := openRemoteZIPReader(source, extension)
	if err != nil {
		return nil, apiError{Status: http.StatusUnprocessableEntity, Code: "invalid_zip", Message: "the object does not contain a readable ZIP central directory"}
	}
	// Same cap as the archive preview so selection cost stays bounded by the
	// configured entry limit rather than by an attacker-sized central directory.
	if maxEntries <= 0 {
		maxEntries = defaultMaxArchiveEntries
	}
	if len(reader.File) > maxEntries {
		return nil, apiError{Status: http.StatusRequestEntityTooLarge, Code: "archive_too_many_entries", Message: fmt.Sprintf("the archive contains more than the configured limit of %d entries", maxEntries)}
	}
	return reader, nil
}

func exactArchiveEntry(reader *zip.Reader, name string) (*zip.File, error) {
	if reader == nil {
		return nil, apiError{Status: http.StatusUnprocessableEntity, Code: "invalid_zip", Message: "the archive central directory is unavailable"}
	}
	var match *zip.File
	for _, file := range reader.File {
		if file.Name != name {
			continue
		}
		if match != nil {
			return nil, apiError{Status: http.StatusConflict, Code: "ambiguous_archive_entry", Message: "the archive contains multiple entries with the selected name"}
		}
		match = file
	}
	if match == nil {
		return nil, apiError{Status: http.StatusNotFound, Code: "archive_entry_not_found", Message: "archive entry was not found"}
	}
	return match, nil
}

func validateSelectableArchiveEntry(file *zip.File, maximum int64) error {
	if file == nil {
		return apiError{Status: http.StatusNotFound, Code: "archive_entry_not_found", Message: "archive entry was not found"}
	}
	if file.FileInfo().IsDir() || strings.HasSuffix(file.Name, "/") {
		return apiError{Status: http.StatusBadRequest, Code: "archive_entry_is_directory", Message: "directories cannot be extracted as files"}
	}
	if safeName, ok := safeArchiveEntryName(file.Name); !ok || safeName != strings.ReplaceAll(file.Name, "\\", "/") {
		return apiError{Status: http.StatusBadRequest, Code: "unsafe_archive_entry", Message: "the archive entry has an unsafe path"}
	}
	if file.Mode()&0o170000 == 0o120000 {
		return apiError{Status: http.StatusForbidden, Code: "archive_symlink_blocked", Message: "symbolic link entries cannot be opened"}
	}
	if file.Flags&0x1 != 0 {
		return apiError{Status: http.StatusUnprocessableEntity, Code: "archive_entry_encrypted", Message: "encrypted archive entries are not supported"}
	}
	if maximum > 0 && file.UncompressedSize64 > uint64(maximum) {
		return apiError{Status: http.StatusRequestEntityTooLarge, Code: "archive_entry_too_large", Message: fmt.Sprintf("the entry exceeds the configured %d-byte extraction limit", maximum)}
	}
	return nil
}

func archiveEntryOpenError(name string, err error) error {
	message := "archive entry could not be decompressed"
	if strings.Contains(strings.ToLower(err.Error()), "unsupported compression") {
		message = "archive entry uses an unsupported compression method"
	}
	return apiError{Status: http.StatusUnprocessableEntity, Code: "archive_entry_open_failed", Message: fmt.Sprintf("%s: %s", message, path.Base(name))}
}

func runArchiveExtraction(ctx context.Context, manager *jobManager, job *jobState) error {
	source := manager.app.instances[job.Instance]
	target := manager.app.instances[job.TargetInstance]
	if source == nil || target == nil {
		return apiError{Status: http.StatusNotFound, Code: "unknown_instance", Message: "source or target storage instance was not found"}
	}
	if err := requirePermission(source, permissionRead); err != nil {
		return err
	}
	if err := requirePermission(target, permissionWrite); err != nil {
		return err
	}
	// Create-only extraction reports whether a destination already exists, which
	// would reveal keys on a target whose read permission is denied.
	if err := requirePermission(target, permissionRead); err != nil {
		return err
	}
	// The ZIP is read lazily while the destination upload holds a storage slot.
	// Those nested source reads must not wait for another slot, otherwise
	// concurrent extraction jobs hold-and-wait on each other forever.
	reader, err := openArchiveReader(withGateExempt(ctx), source, job.Source, job.Version, mediaSourceMetadata{}, manager.app.config.Runtime.MaxArchiveEntries)
	if err != nil {
		return err
	}
	// One O(N) index instead of a linear scan per selected name.
	index := make(map[string][]*zip.File, len(reader.File))
	for position, file := range reader.File {
		if position%1024 == 0 {
			if err := manager.interrupted(ctx, job.ID); err != nil {
				return err
			}
		}
		index[file.Name] = append(index[file.Name], file)
	}
	selected := make([]*zip.File, 0, len(job.Entries))
	destinations := make(map[string]struct{}, len(job.Entries))
	for _, name := range job.Entries {
		if err := manager.interrupted(ctx, job.ID); err != nil {
			return err
		}
		var file *zip.File
		switch matches := index[name]; len(matches) {
		case 0:
			return apiError{Status: http.StatusNotFound, Code: "archive_entry_not_found", Message: "archive entry was not found"}
		case 1:
			file = matches[0]
		default:
			return apiError{Status: http.StatusConflict, Code: "ambiguous_archive_entry", Message: "the archive contains multiple entries with the selected name"}
		}
		if err := validateSelectableArchiveEntry(file, manager.app.config.Runtime.MaxBackgroundStorageBytes); err != nil {
			return err
		}
		safeName, _ := safeArchiveEntryName(file.Name)
		destination := cleanRelativeKey(job.Target + safeName)
		if destination == "" {
			return apiError{Status: http.StatusBadRequest, Code: "invalid_archive_target", Message: "an extracted archive entry has an invalid destination"}
		}
		if source == target && destination == job.Source {
			return apiError{Status: http.StatusConflict, Code: "archive_source_collision", Message: "an extracted entry would overwrite the archive while it is being read"}
		}
		if _, exists := destinations[destination]; exists {
			return apiError{Status: http.StatusConflict, Code: "archive_target_collision", Message: "multiple selected entries resolve to the same destination"}
		}
		destinations[destination] = struct{}{}
		selected = append(selected, file)
	}

	result := cloneArchiveExtractResult(job.ArchiveExtract)
	if result == nil || result.ArchiveKey != job.Source || result.Version != job.Version || result.TargetInstance != job.TargetInstance || result.Target != job.Target || result.Extracted != job.Processed {
		job.Processed = 0
		job.LastKey = ""
		result = &archiveExtractResult{ArchiveKey: job.Source, Version: job.Version, TargetInstance: job.TargetInstance, Target: job.Target}
	}
	job.ArchiveExtract = result
	if err := manager.put(*job); err != nil {
		return err
	}
	start := int(job.Processed)
	if start < 0 || start > len(selected) {
		return apiError{Status: http.StatusConflict, Code: "invalid_archive_progress", Message: "archive extraction progress is invalid"}
	}
	for index := start; index < len(selected); index++ {
		if err := manager.controlState(job.ID); err != nil {
			return err
		}
		file := selected[index]
		body, err := file.Open()
		if err != nil {
			return archiveEntryOpenError(file.Name, err)
		}
		counter := &archiveCountingReader{reader: io.LimitReader(body, int64(file.UncompressedSize64)+1), ctx: ctx, manager: manager, jobID: job.ID}
		safeName, _ := safeArchiveEntryName(file.Name)
		destination := cleanRelativeKey(job.Target + safeName)
		contentType := previewContentType(file.Name)
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		putErr := target.PutIfAbsent(ctx, target.fullKey(destination), counter, int64(file.UncompressedSize64), contentType)
		closeErr := body.Close()
		if putErr != nil {
			return fmt.Errorf("extract %q: %w", file.Name, putErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close archive entry %q: %w", file.Name, closeErr)
		}
		if counter.count != int64(file.UncompressedSize64) {
			return apiError{Status: http.StatusBadGateway, Code: "archive_entry_truncated", Message: fmt.Sprintf("archive entry %q was truncated while being extracted", file.Name)}
		}
		result.Bytes += counter.count
		result.Extracted = int64(index + 1)
		job.Processed = result.Extracted
		job.LastKey = file.Name
		job.ArchiveExtract = result
		job.UpdatedAt = time.Now().UTC()
		if err := manager.put(*job); err != nil {
			return err
		}
	}
	result.CompletedAt = time.Now().UTC()
	job.ArchiveExtract = result
	return manager.put(*job)
}

func runArchiveEntryIntegrity(ctx context.Context, manager *jobManager, job *jobState) error {
	request := job.ArchiveEntryIntegrityRequest
	if request == nil {
		return fmt.Errorf("archive entry integrity request is missing")
	}
	instance := manager.app.instances[job.Instance]
	if instance == nil {
		return apiError{Status: http.StatusNotFound, Code: "unknown_instance", Message: "storage instance was not found"}
	}
	if err := requirePermission(instance, permissionRead); err != nil {
		return err
	}
	reader, err := openArchiveReader(ctx, instance, request.Key, request.Version, mediaSourceMetadata{}, manager.app.config.Runtime.MaxArchiveEntries)
	if err != nil {
		return err
	}
	file, err := exactArchiveEntry(reader, request.Entry)
	if err != nil {
		return err
	}
	if err := validateSelectableArchiveEntry(file, manager.app.config.Runtime.MaxBackgroundStorageBytes); err != nil {
		return err
	}
	body, err := file.Open()
	if err != nil {
		return archiveEntryOpenError(file.Name, err)
	}
	defer body.Close()
	sha := sha256.New()
	md := md5.New() // #nosec G401 -- used only to compare a provider-supplied digest.
	crc := crc32.NewIEEE()
	counter := &archiveCountingReader{reader: io.LimitReader(body, int64(file.UncompressedSize64)+1), ctx: ctx, manager: manager, jobID: job.ID}
	written, err := io.CopyBuffer(io.MultiWriter(sha, md, crc), counter, make([]byte, 128<<10))
	if err != nil {
		return err
	}
	if written != int64(file.UncompressedSize64) || counter.count != written {
		return apiError{Status: http.StatusBadGateway, Code: "archive_entry_size_mismatch", Message: "archive entry size did not match its central-directory metadata"}
	}
	actualCRC := crc.Sum32()
	job.ArchiveEntryIntegrity = &archiveEntryIntegrityResult{
		ArchiveKey: request.Key, Version: request.Version, Entry: request.Entry,
		Size: written, CompressedSize: int64(file.CompressedSize64),
		SHA256: hex.EncodeToString(sha.Sum(nil)), MD5: hex.EncodeToString(md.Sum(nil)),
		CRC32: fmt.Sprintf("%08x", actualCRC), ExpectedCRC32: fmt.Sprintf("%08x", file.CRC32), CRC32Matches: actualCRC == file.CRC32,
	}
	job.Processed = 1
	job.LastKey = request.Entry
	return manager.put(*job)
}

func cloneArchiveExtractResult(value *archiveExtractResult) *archiveExtractResult {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func cloneArchiveEntryIntegrityRequest(value *archiveEntryIntegrityRequest) *archiveEntryIntegrityRequest {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func cloneArchiveEntryIntegrityResult(value *archiveEntryIntegrityResult) *archiveEntryIntegrityResult {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}
