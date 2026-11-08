package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

func requireExactByteRanges(t *testing.T, ranges []string) {
	t.Helper()
	if len(ranges) == 0 {
		t.Fatal("expected at least one byte-range request")
	}
	for _, value := range ranges {
		if !strings.HasPrefix(value, "bytes=") {
			t.Fatalf("range %q is not an exact byte range", value)
		}
		parts := strings.SplitN(strings.TrimPrefix(value, "bytes="), "-", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
			t.Fatalf("range %q is open-ended or malformed", value)
		}
		start, startErr := strconv.ParseInt(parts[0], 10, 64)
		end, endErr := strconv.ParseInt(parts[1], 10, 64)
		if startErr != nil || endErr != nil || start < 0 || end < start {
			t.Fatalf("range %q is invalid", value)
		}
	}
}

func exactRangeStart(t *testing.T, value string) int64 {
	t.Helper()
	requireExactByteRanges(t, []string{value})
	parts := strings.SplitN(strings.TrimPrefix(value, "bytes="), "-", 2)
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		t.Fatalf("parse range start %q: %v", value, err)
	}
	return start
}

type memoryObject struct {
	data        []byte
	contentType string
	modified    time.Time
}

type memoryReadCloser struct {
	backend *memoryBackend
	reader  *bytes.Reader
}

func (r *memoryReadCloser) Read(buffer []byte) (int, error) {
	read, err := r.reader.Read(buffer)
	r.backend.mu.Lock()
	r.backend.getBytesRead += int64(read)
	r.backend.mu.Unlock()
	return read, err
}

func (r *memoryReadCloser) Close() error { return nil }

type memoryMultipart struct {
	key         string
	contentType string
	parts       map[int][]byte
}

type memoryBackend struct {
	mu             sync.Mutex
	objects        map[string]memoryObject
	last           listOptions
	lastGetHeaders http.Header
	getRanges      []string
	getCount       int
	getBytesRead   int64
	headCount      int
	ignoreRanges   bool
	rangeStatusOK  bool
	rangeBodyFull  bool
	multipart      map[string]*memoryMultipart
	nextUpload     int
	listPages      []listPage
	listCalls      int
	listDelay      time.Duration
}

func newMemoryBackend(objects map[string]string) *memoryBackend {
	backend := &memoryBackend{objects: make(map[string]memoryObject), multipart: make(map[string]*memoryMultipart)}
	for key, value := range objects {
		backend.objects[key] = memoryObject{data: []byte(value), contentType: "text/plain", modified: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	}
	return backend
}

func (m *memoryBackend) List(_ context.Context, options listOptions) (listPage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.last = options
	m.listCalls++
	if m.listDelay > 0 {
		time.Sleep(m.listDelay)
	}
	if len(m.listPages) > 0 {
		index := 0
		if options.PageToken != "" {
			parsed, err := strconv.Atoi(strings.TrimPrefix(options.PageToken, "page-"))
			if err != nil || parsed < 0 || parsed >= len(m.listPages) {
				return listPage{}, fmt.Errorf("invalid test page token %q", options.PageToken)
			}
			index = parsed
		}
		page := m.listPages[index]
		page.Objects = append([]objectInfo(nil), page.Objects...)
		page.Prefixes = append([]string(nil), page.Prefixes...)
		return page, nil
	}
	keys := make([]string, 0, len(m.objects))
	for key := range m.objects {
		if strings.HasPrefix(key, options.Prefix) && (options.StartAfter == "" || key > options.StartAfter) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	page := listPage{}
	prefixes := make(map[string]struct{})
	for _, key := range keys {
		if options.Delimiter != "" {
			remainder := strings.TrimPrefix(key, options.Prefix)
			if index := strings.Index(remainder, options.Delimiter); index >= 0 {
				prefix := options.Prefix + remainder[:index+len(options.Delimiter)]
				prefixes[prefix] = struct{}{}
				continue
			}
		}
		object := m.objects[key]
		page.Objects = append(page.Objects, objectInfo{
			Key: key, Size: int64(len(object.data)), LastModified: object.modified, ETag: "etag-" + key, ContentType: object.contentType,
		})
	}
	for prefix := range prefixes {
		page.Prefixes = append(page.Prefixes, prefix)
	}
	sort.Strings(page.Prefixes)
	return page, nil
}

func (m *memoryBackend) Head(_ context.Context, key string) (objectResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.headCount++
	object, ok := m.objects[key]
	if !ok {
		return objectResponse{}, &upstreamError{StatusCode: http.StatusNotFound, Code: "NotFound", Message: "missing"}
	}
	headers := make(http.Header)
	headers.Set("Content-Type", object.contentType)
	headers.Set("Content-Length", strconv.Itoa(len(object.data)))
	headers.Set("ETag", `"etag-`+key+`"`)
	headers.Set("Last-Modified", object.modified.UTC().Format(http.TimeFormat))
	headers.Set("Set-Cookie", "should-not-leak=1")
	headers.Set("x-amz-meta-test", "visible")
	return objectResponse{StatusCode: http.StatusOK, Header: headers}, nil
}

func (m *memoryBackend) Get(_ context.Context, key string, requestHeaders http.Header) (objectResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	object, ok := m.objects[key]
	if !ok {
		return objectResponse{}, &upstreamError{StatusCode: http.StatusNotFound, Code: "NotFound", Message: "missing"}
	}
	m.lastGetHeaders = requestHeaders.Clone()
	m.getCount++
	m.getRanges = append(m.getRanges, requestHeaders.Get("Range"))
	data := object.data
	fullData := object.data
	status := http.StatusOK
	contentRange := ""
	if value := strings.TrimSpace(requestHeaders.Get("Range")); !m.ignoreRanges && strings.HasPrefix(value, "bytes=") {
		raw := strings.TrimPrefix(value, "bytes=")
		parts := strings.SplitN(raw, "-", 2)
		if len(parts) == 2 && parts[0] == "" {
			length, lengthErr := strconv.Atoi(parts[1])
			if lengthErr == nil && length > 0 && len(data) > 0 {
				start := len(data) - length
				if start < 0 {
					start = 0
				}
				end := len(data) - 1
				contentRange = fmt.Sprintf("bytes %d-%d/%d", start, end, len(data))
				data = data[start : end+1]
				status = http.StatusPartialContent
			}
		} else if len(parts) == 2 {
			start, startErr := strconv.Atoi(parts[0])
			end := len(data) - 1
			endErr := error(nil)
			if strings.TrimSpace(parts[1]) != "" {
				end, endErr = strconv.Atoi(parts[1])
			}
			if startErr == nil && endErr == nil && start >= 0 && start < len(data) && end >= start {
				if end >= len(data) {
					end = len(data) - 1
				}
				contentRange = fmt.Sprintf("bytes %d-%d/%d", start, end, len(data))
				data = data[start : end+1]
				status = http.StatusPartialContent
			}
		}
	}
	headers := make(http.Header)
	headers.Set("Content-Type", object.contentType)
	headers.Set("Content-Length", strconv.Itoa(len(data)))
	headers.Set("ETag", `"etag-`+key+`"`)
	headers.Set("Last-Modified", object.modified.UTC().Format(http.TimeFormat))
	if contentRange != "" {
		headers.Set("Content-Range", contentRange)
		if m.rangeStatusOK {
			status = http.StatusOK
		}
		if m.rangeBodyFull {
			data = fullData
			headers.Set("Content-Length", strconv.Itoa(len(data)))
		}
	}
	headers.Set("Set-Cookie", "should-not-leak=1")
	headers.Set("x-amz-meta-test", "visible")
	return objectResponse{StatusCode: status, Header: headers, Body: &memoryReadCloser{backend: m, reader: bytes.NewReader(data)}}, nil
}

func (m *memoryBackend) Put(_ context.Context, key string, body io.Reader, _ int64, contentType string) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = memoryObject{data: data, contentType: contentType, modified: time.Now().UTC()}
	return nil
}

func (m *memoryBackend) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

func (m *memoryBackend) Copy(_ context.Context, source, destination string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	object, ok := m.objects[source]
	if !ok {
		return &upstreamError{StatusCode: http.StatusNotFound, Code: "NotFound", Message: "missing"}
	}
	object.data = append([]byte(nil), object.data...)
	m.objects[destination] = object
	return nil
}

func (m *memoryBackend) DiscoverCapabilities(context.Context) (discoveredCapabilities, error) {
	return discoveredCapabilities{}, nil
}

func (m *memoryBackend) InitiateMultipart(_ context.Context, key, contentType string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextUpload++
	id := fmt.Sprintf("upload-%d", m.nextUpload)
	m.multipart[id] = &memoryMultipart{key: key, contentType: contentType, parts: make(map[int][]byte)}
	return id, nil
}

func (m *memoryBackend) UploadPart(_ context.Context, _ string, uploadID string, partNumber int, body io.Reader, _ int64) (string, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	upload := m.multipart[uploadID]
	if upload == nil {
		return "", errors.New("multipart upload not found")
	}
	upload.parts[partNumber] = append([]byte(nil), data...)
	return fmt.Sprintf("etag-%d", partNumber), nil
}

func (m *memoryBackend) ListMultipartParts(_ context.Context, _ string, uploadID string) ([]multipartPart, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	upload := m.multipart[uploadID]
	if upload == nil {
		return nil, &upstreamError{StatusCode: http.StatusNotFound, Code: "NoSuchUpload", Message: "multipart upload not found"}
	}
	partNumbers := make([]int, 0, len(upload.parts))
	for partNumber := range upload.parts {
		partNumbers = append(partNumbers, partNumber)
	}
	sort.Ints(partNumbers)
	parts := make([]multipartPart, 0, len(partNumbers))
	for _, partNumber := range partNumbers {
		parts = append(parts, multipartPart{PartNumber: partNumber, ETag: fmt.Sprintf("etag-%d", partNumber), Size: int64(len(upload.parts[partNumber]))})
	}
	return parts, nil
}

func (m *memoryBackend) CompleteMultipart(_ context.Context, _ string, uploadID string, parts []s3CompletedPart) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	upload := m.multipart[uploadID]
	if upload == nil {
		return errors.New("multipart upload not found")
	}
	var data []byte
	for _, part := range parts {
		data = append(data, upload.parts[part.PartNumber]...)
	}
	m.objects[upload.key] = memoryObject{data: data, contentType: upload.contentType, modified: time.Now().UTC()}
	delete(m.multipart, uploadID)
	return nil
}

func (m *memoryBackend) AbortMultipart(_ context.Context, _ string, uploadID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.multipart, uploadID)
	return nil
}

func testApplication(t *testing.T) (*application, *memoryBackend, *memoryBackend) {
	t.Helper()
	readOnly := newMemoryBackend(map[string]string{"readonly.txt": "read"})
	readWrite := newMemoryBackend(map[string]string{
		"tenant/folder/direct.txt":      "direct",
		"tenant/folder/nested/deep.txt": "deep",
		"tenant/a//./../b\\c":           "opaque",
	})
	makeInstance := func(id string, backend *memoryBackend, root string, permissions ...string) *storageInstance {
		cfg := bucketConfig{
			ID: id, Name: id, AuthID: "test-auth", Provider: "s3", Bucket: id + "-bucket", Region: "test",
			RootPrefix: root, PermissionsDefined: true, Permissions: permissions,
		}
		return &storageInstance{cfg: cfg, backend: backend, caps: initialCapabilities(cfg)}
	}
	publicFS := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("ok"), Mode: fs.FileMode(0o444)}}
	app := &application{
		config: appConfig{JobHistoryLimit: 10, Runtime: defaultRuntimePolicy()},
		authentications: map[string]*sharedAuthentication{
			"test-auth": {
				cfg:     authConfig{ID: "test-auth", Provider: "s3", Mode: "access_key"},
				s3Creds: s3Credentials{AccessKeyID: "test-key", SecretAccessKey: "stable-test-secret"},
			},
		},
		instances: map[string]*storageInstance{
			"readonly": makeInstance("readonly", readOnly, "", permissionRead),
			"rw":       makeInstance("rw", readWrite, "tenant/", permissionRead, permissionWrite, permissionDelete),
		},
		order:    []string{"readonly", "rw"},
		publicFS: publicFS,
	}
	var err error
	app.jobs, err = newJobManager(app, app.config.JobHistoryLimit)
	if err != nil {
		t.Fatal(err)
	}
	app.uploads, err = newUploadManager(app)
	if err != nil {
		app.jobs.close()
		t.Fatal(err)
	}
	app.sqlite = newSQLiteSessionManager(app)
	t.Cleanup(app.close)
	return app, readOnly, readWrite
}

func TestEmbeddedIndexKeepsAFlatTable(t *testing.T) {
	t.Parallel()
	data, err := embeddedPublic.ReadFile("public/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(data)
	for _, required := range []string{
		"assets/css/icons.css",
		`id="storageSwitcher"`,
		"assets/js/app.js",
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("embedded index is missing %q", required)
		}
	}
	for _, forbidden := range []string{
		" detailed",
		":detailed",
		"#detail",
		"mdi-chevron-right folder-expander",
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("embedded index contains table-detail markup %q", forbidden)
		}
	}
}

func TestInstancesResponseDoesNotExposeCredentialsOrEndpoint(t *testing.T) {
	t.Parallel()
	app, _, _ := testApplication(t)
	recorder := httptest.NewRecorder()
	app.routes().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/instances", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, forbidden := range []string{"internal.invalid", "secret_access_key", "access_key_id"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("response leaked %q: %s", forbidden, body)
		}
	}
	var response struct {
		Default   string         `json:"default"`
		Instances []instanceInfo `json:"instances"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Default != "readonly" || len(response.Instances) != 2 {
		t.Fatalf("unexpected response: %+v", response)
	}
}

func TestListScansConfiguredProviderPagesAndEnablesGlobalSortOnlyWhenComplete(t *testing.T) {
	app, _, backend := testApplication(t)
	backend.mu.Lock()
	backend.listPages = []listPage{
		{Objects: []objectInfo{{Key: "tenant/a.txt", Size: 1}}, NextPageToken: "page-1"},
		{Objects: []objectInfo{{Key: "tenant/b.txt", Size: 2}}, NextPageToken: "page-2"},
		{Objects: []objectInfo{{Key: "tenant/c.txt", Size: 3}}},
	}
	backend.listCalls = 0
	backend.mu.Unlock()
	app.instances["rw"].cfg.MaxScanPages = 2

	recorder := httptest.NewRecorder()
	app.routes().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/list?instance=rw&delimiter=/", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var limited listResponseJSON
	if err := json.Unmarshal(recorder.Body.Bytes(), &limited); err != nil {
		t.Fatal(err)
	}
	if limited.ScanComplete || limited.SortAvailable || limited.NextContinuationToken != "page-2" || len(limited.Items) != 2 {
		t.Fatalf("limited scan = %+v", limited)
	}
	backend.mu.Lock()
	if backend.listCalls != 2 || backend.last.MaxResults != providerListPageSize {
		t.Fatalf("provider calls = %d, max results = %d", backend.listCalls, backend.last.MaxResults)
	}
	backend.listCalls = 0
	backend.mu.Unlock()

	app.instances["rw"].cfg.MaxScanPages = 3
	recorder = httptest.NewRecorder()
	app.routes().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/list?instance=rw&delimiter=/", nil))
	var complete listResponseJSON
	if err := json.Unmarshal(recorder.Body.Bytes(), &complete); err != nil {
		t.Fatal(err)
	}
	if !complete.ScanComplete || !complete.SortAvailable || complete.NextContinuationToken != "" || len(complete.Items) != 3 {
		t.Fatalf("complete scan = %+v", complete)
	}

	backend.mu.Lock()
	backend.listCalls = 0
	backend.mu.Unlock()
	app.instances["rw"].cfg.MaxScanPages = 0
	recorder = httptest.NewRecorder()
	app.routes().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/list?instance=rw&delimiter=/", nil))
	var unlimited listResponseJSON
	if err := json.Unmarshal(recorder.Body.Bytes(), &unlimited); err != nil {
		t.Fatal(err)
	}
	if !unlimited.ScanComplete || !unlimited.SortAvailable || unlimited.NextContinuationToken != "" || len(unlimited.Items) != 3 {
		t.Fatalf("unlimited scan = %+v", unlimited)
	}
	backend.mu.Lock()
	if backend.listCalls != 3 {
		t.Fatalf("unlimited provider calls = %d, want 3", backend.listCalls)
	}
	backend.mu.Unlock()

	recorder = httptest.NewRecorder()
	app.routes().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/list?instance=rw&delimiter=/&continuationToken=page-2", nil))
	var continued listResponseJSON
	if err := json.Unmarshal(recorder.Body.Bytes(), &continued); err != nil {
		t.Fatal(err)
	}
	if !continued.ScanComplete || continued.SortAvailable || len(continued.Items) != 1 {
		t.Fatalf("continued scan = %+v", continued)
	}
}

func TestPermissionEnforcementAndNativeRename(t *testing.T) {
	t.Parallel()
	app, readOnly, readWrite := testApplication(t)
	handler := app.routes()

	forbidden := httptest.NewRecorder()
	handler.ServeHTTP(forbidden, httptest.NewRequest(http.MethodPut, "/s3?instance=readonly&key=new.txt", strings.NewReader("blocked")))
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("readonly PUT status = %d", forbidden.Code)
	}
	if _, exists := readOnly.objects["new.txt"]; exists {
		t.Fatal("readonly backend was modified")
	}

	body := `{"instance":"rw","src":"folder/direct.txt","dst":"folder/renamed.txt","isPrefix":false}`
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/rename", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("rename status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	readWrite.mu.Lock()
	defer readWrite.mu.Unlock()
	if _, exists := readWrite.objects["tenant/folder/direct.txt"]; exists {
		t.Fatal("source still exists after rename")
	}
	if got := string(readWrite.objects["tenant/folder/renamed.txt"].data); got != "direct" {
		t.Fatalf("destination data = %q", got)
	}
}

func TestRecursiveListUsesExplicitEmptyDelimiter(t *testing.T) {
	t.Parallel()
	app, _, backend := testApplication(t)
	recorder := httptest.NewRecorder()
	app.routes().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/list?instance=rw&prefix=folder%2F&delimiter=", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response listResponseJSON
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Delimiter != "" || len(response.Items) != 2 {
		t.Fatalf("response = %+v", response)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.last.Delimiter != "" {
		t.Fatalf("backend delimiter = %q", backend.last.Delimiter)
	}
}

func TestObjectGatewayRejectsAmbiguousQueryKey(t *testing.T) {
	t.Parallel()
	app, _, _ := testApplication(t)
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/s3?instance=rw&key=first&key=second", nil)
	app.routes().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestObjectGatewayPreservesOpaqueKeyAndFiltersHeaders(t *testing.T) {
	t.Parallel()
	app, _, _ := testApplication(t)
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/s3?instance=rw&key=a%2F%2F.%2F..%2Fb%5Cc", nil)
	app.routes().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Body.String(); got != "opaque" {
		t.Fatalf("body = %q", got)
	}
	if got := recorder.Header().Get("Set-Cookie"); got != "" {
		t.Fatalf("Set-Cookie leaked: %q", got)
	}
	if got := recorder.Header().Get("x-amz-meta-test"); got != "visible" {
		t.Fatalf("metadata header = %q", got)
	}
	if got := recorder.Header().Get("Content-Security-Policy"); !strings.Contains(got, "sandbox") {
		t.Fatalf("CSP = %q", got)
	}
}

func TestPreviewGatewayInfersMediaTypeAndForcesInline(t *testing.T) {
	t.Parallel()
	app, _, backend := testApplication(t)
	backend.mu.Lock()
	backend.objects["tenant/video.mp4"] = memoryObject{
		data:        []byte("0123456789"),
		contentType: "application/octet-stream",
		modified:    time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	backend.mu.Unlock()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/s3?instance=rw&preview=1&key=video.mp4", nil)
	request.Header.Set("Range", "bytes=0-3")
	app.routes().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "video/mp4" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := recorder.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "inline") {
		t.Fatalf("Content-Disposition = %q", got)
	}
	if got := recorder.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("Accept-Ranges = %q", got)
	}
	if got := recorder.Body.String(); got != "0123" {
		t.Fatalf("body = %q", got)
	}
}

func TestPreviewGatewayRejectsIgnoredRangeWithoutReadingTheObject(t *testing.T) {
	t.Parallel()
	app, _, backend := testApplication(t)
	backend.mu.Lock()
	backend.ignoreRanges = true
	backend.objects["tenant/document.pdf"] = memoryObject{
		data:        bytes.Repeat([]byte("x"), 1024*1024),
		contentType: "application/pdf",
		modified:    time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	backend.mu.Unlock()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/s3?instance=rw&preview=1&key=document.pdf", nil)
	request.Header.Set("Range", "bytes=900000-900031")
	app.routes().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "refused to scan or buffer the complete object") {
		t.Fatalf("body = %s", recorder.Body.String())
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.getBytesRead != 0 {
		t.Fatalf("ignored range read %d bytes from the complete object", backend.getBytesRead)
	}
	if backend.headCount != 0 {
		t.Fatalf("ignored range performed %d unnecessary HEAD requests", backend.headCount)
	}
}

func TestPreviewGatewayRejectsFullBodyWithMisleadingContentRange(t *testing.T) {
	t.Parallel()
	app, _, backend := testApplication(t)
	backend.mu.Lock()
	backend.rangeStatusOK = true
	backend.rangeBodyFull = true
	backend.objects["tenant/document.pdf"] = memoryObject{
		data:        bytes.Repeat([]byte("x"), 1024*1024),
		contentType: "application/pdf",
		modified:    time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	backend.mu.Unlock()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/s3?instance=rw&preview=1&key=document.pdf", nil)
	request.Header.Set("Range", "bytes=900000-900031")
	app.routes().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "invalid byte count") {
		t.Fatalf("body = %s", recorder.Body.String())
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.getBytesRead != 0 {
		t.Fatalf("misleading range read %d bytes from the complete object", backend.getBytesRead)
	}
}

func TestPreviewGatewayNormalizesStatus200WithContentRange(t *testing.T) {
	t.Parallel()
	app, _, backend := testApplication(t)
	backend.mu.Lock()
	backend.rangeStatusOK = true
	backend.objects["tenant/document.pdf"] = memoryObject{
		data:        []byte("0123456789"),
		contentType: "application/pdf",
		modified:    time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	backend.mu.Unlock()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/s3?instance=rw&preview=1&key=document.pdf", nil)
	request.Header.Set("Range", "bytes=3-6")
	app.routes().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Range"); got != "bytes 3-6/10" {
		t.Fatalf("Content-Range = %q", got)
	}
	if got := recorder.Body.String(); got != "3456" {
		t.Fatalf("body = %q", got)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.getBytesRead != 4 {
		t.Fatalf("normalized range read %d bytes, want 4", backend.getBytesRead)
	}
}

func TestPrefixDestinationCannotBeInsideSource(t *testing.T) {
	t.Parallel()
	app, _, _ := testApplication(t)
	body := `{"instance":"rw","src":"folder/","dst":"folder/copy/","isPrefix":true}`
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/copy", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	app.routes().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "destination cannot be inside source") {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestPrefixDestinationCannotContainSource(t *testing.T) {
	t.Parallel()
	app, _, backend := testApplication(t)
	body := `{"instance":"rw","src":"folder/nested/","dst":"folder/","isPrefix":true}`
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/copy", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	app.routes().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "object paths would overlap") {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if got := string(backend.objects["tenant/folder/nested/deep.txt"].data); got != "deep" {
		t.Fatalf("source was modified: %q", got)
	}
}

func TestAPIStorageErrorsDoNotExposeRequestURL(t *testing.T) {
	t.Parallel()
	recorder := httptest.NewRecorder()
	err := fmt.Errorf("s3 request failed: %w", &url.Error{
		Op:  "Get",
		URL: "https://internal.example.test/private?credential=secret",
		Err: errors.New("dial failed"),
	})
	writeAPIError(recorder, err)
	if strings.Contains(recorder.Body.String(), "internal.example.test") || strings.Contains(recorder.Body.String(), "credential") {
		t.Fatalf("response leaked request URL: %s", recorder.Body.String())
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
}

func TestFolderListingDoesNotProbeObjectHeaders(t *testing.T) {
	app, _, backend := testApplication(t)
	recorder := httptest.NewRecorder()
	app.routes().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/list?instance=rw&prefix=folder%2F&delimiter=%2F", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.getCount != 0 || backend.headCount != 0 {
		t.Fatalf("listing performed %d GET and %d HEAD object probes", backend.getCount, backend.headCount)
	}
}

func TestApplicationCSPAllowsOnlySameOriginEmbeddedPDFs(t *testing.T) {
	t.Parallel()
	app, _, _ := testApplication(t)
	recorder := httptest.NewRecorder()
	app.routes().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	policy := recorder.Header().Get("Content-Security-Policy")
	if !strings.Contains(policy, "object-src 'none'") {
		t.Fatalf("CSP = %q, want browser plugins disabled", policy)
	}
	if strings.Contains(policy, "object-src *") || strings.Contains(policy, "object-src https:") || strings.Contains(policy, "object-src 'self'") {
		t.Fatalf("CSP is too broad: %q", policy)
	}
}

// The browser page is plain JavaScript, so scripts never need eval-style evaluation.
func TestApplicationCSPForbidsScriptEvaluation(t *testing.T) {
	t.Parallel()
	app, _, _ := testApplication(t)
	recorder := httptest.NewRecorder()
	app.routes().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	policy := recorder.Header().Get("Content-Security-Policy")
	if strings.Contains(policy, "unsafe-eval") || strings.Contains(policy, "unsafe-inline'; style") {
		t.Fatalf("CSP allows script evaluation: %q", policy)
	}
	if !strings.Contains(policy, "script-src 'self';") {
		t.Fatalf("CSP = %q, want script-src limited to the same origin", policy)
	}
}

func TestBoundedPDFPreviewRangeRequiresExplicitFiniteRange(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{name: "exact range", input: "bytes=2-5", want: true},
		{name: "maximum range", input: "bytes=0-16777215", want: true},
		{name: "missing range", input: "", want: false},
		{name: "open ended range", input: "bytes=1048576-", want: false},
		{name: "oversized range", input: "bytes=0-16777216", want: false},
		{name: "suffix range", input: "bytes=-65536", want: false},
		{name: "multiple ranges", input: "bytes=0-1,4-5", want: false},
		{name: "malformed", input: "bytes=nope", want: false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := isBoundedPreviewRange(test.input, 16<<20); got != test.want {
				t.Fatalf("isBoundedPreviewRange(%q) = %t, want %t", test.input, got, test.want)
			}
		})
	}
}

func TestNativePDFPreviewPlainGetIsNotForcedPartial(t *testing.T) {
	t.Parallel()
	app, _, backend := testApplication(t)
	backend.mu.Lock()
	backend.objects["tenant/document.pdf"] = memoryObject{
		data:        []byte("%PDF-1.7\nsmall test document"),
		contentType: "application/pdf",
		modified:    time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	backend.mu.Unlock()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/s3?instance=rw&preview=1&key=document.pdf", nil)
	app.routes().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Range"); got != "" {
		t.Fatalf("native preview was forced partial: Content-Range = %q", got)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.getRanges) != 1 || backend.getRanges[0] != "" {
		t.Fatalf("native preview unexpectedly sent a provider range: %#v", backend.getRanges)
	}
}

func TestRangeOnlyPreviewRejectsUnboundedRequestsBeforeStorageRead(t *testing.T) {
	t.Parallel()
	for _, rangeHeader := range []string{"", "bytes=0-", "bytes=-99999999", "bytes=0-99999999"} {
		rangeHeader := rangeHeader
		t.Run(rangeHeader, func(t *testing.T) {
			app, _, backend := testApplication(t)
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/s3?instance=rw&preview=1&range_only=1&key=document.pdf", nil)
			if rangeHeader != "" {
				request.Header.Set("Range", rangeHeader)
			}
			app.routes().ServeHTTP(recorder, request)

			if recorder.Code != http.StatusRequestedRangeNotSatisfiable {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			backend.mu.Lock()
			defer backend.mu.Unlock()
			if backend.getCount != 0 || backend.getBytesRead != 0 {
				t.Fatalf("unbounded preview reached storage: gets=%d bytes=%d", backend.getCount, backend.getBytesRead)
			}
		})
	}
}

func TestRangeOnlyPreviewRejectsMultipleRangesBeforeStorageRead(t *testing.T) {
	t.Parallel()
	app, _, backend := testApplication(t)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/s3?instance=rw&preview=1&range_only=1&key=document.pdf", nil)
	request.Header.Set("Range", "bytes=0-1,4-5")
	app.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.getCount != 0 || backend.getBytesRead != 0 {
		t.Fatalf("storage read occurred: gets=%d bytes=%d", backend.getCount, backend.getBytesRead)
	}
}

func TestRangeOnlyPreviewAllowsExactBoundedRange(t *testing.T) {
	t.Parallel()
	app, _, backend := testApplication(t)
	backend.mu.Lock()
	backend.objects["tenant/document.pdf"] = memoryObject{
		data:        []byte("0123456789"),
		contentType: "application/pdf",
		modified:    time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	backend.mu.Unlock()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/s3?instance=rw&preview=1&range_only=1&key=document.pdf", nil)
	request.Header.Set("Range", "bytes=2-5")
	app.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusPartialContent || recorder.Body.String() != "2345" {
		t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
	}
}

func TestRoutesAreMountedAtRootForReverseProxyPrefixStripping(t *testing.T) {
	app, _, _ := testApplication(t)
	handler := app.routes()

	for _, requestPath := range []string{"/", "/api/instances", "/healthz"} {
		t.Run(requestPath, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, requestPath, nil))
			if recorder.Code != http.StatusOK {
				t.Fatalf("GET %s status = %d; body=%s", requestPath, recorder.Code, recorder.Body.String())
			}
			if recorder.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatalf("GET %s did not receive security headers", requestPath)
			}
		})
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/instances", nil))
	if strings.Contains(recorder.Body.String(), "maxScanPages") {
		t.Fatalf("instances response exposes the internal listing scan limit: %s", recorder.Body.String())
	}
}

func TestApplicationReusesAuthenticationAcrossBuckets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	cfg, err := decodeConfig(`
server {}

auth "shared" {
  provider = "s3"
  mode     = "anonymous"
  endpoint = "`+server.URL+`"
  region   = "test"
}

bucket "one" {
  auth        = "shared"
  bucket      = "one"
  permissions = ["read"]
}

bucket "two" {
  auth        = "shared"
  bucket      = "two"
  permissions = ["read", "write"]
}
`+testOpenAccess, "test.hcl", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app, err := newApplication(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer app.close()

	one, ok := app.instances["one"].backend.(*s3Backend)
	if !ok {
		t.Fatalf("bucket one backend = %T", app.instances["one"].backend)
	}
	two, ok := app.instances["two"].backend.(*s3Backend)
	if !ok {
		t.Fatalf("bucket two backend = %T", app.instances["two"].backend)
	}
	if one.client != two.client || one.endpoint != two.endpoint {
		t.Fatal("buckets referencing the same auth do not share their HTTP client and endpoint")
	}
	if len(app.authentications) != 1 {
		t.Fatalf("authentication count = %d, want 1", len(app.authentications))
	}
	if app.instances["one"].capabilities().Write.State != capabilityDenied {
		t.Fatal("bucket-specific permission ceiling was not applied to bucket one")
	}
	if app.instances["two"].capabilities().Write.State != capabilityUnknown {
		t.Fatal("bucket-specific permission ceiling was not applied independently to bucket two")
	}
}

func TestFrontendBaseHrefForFriendlyRoutes(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		path string
		want string
	}{
		{path: "/", want: "./"},
		{path: "/index.html", want: "./"},
		{path: "/preview.html", want: "./"},
		{path: "/-/host/api/", want: "../../../"},
		{path: "/-/host/api/folder/", want: "../../../../"},
		{path: "/-/host/api/file.pdf", want: "../../../"},
		{path: "/-/host/api/folder/file.pdf", want: "../../../../"},
	} {
		t.Run(test.path, func(t *testing.T) {
			t.Parallel()
			if got := frontendBaseHref(test.path); got != test.want {
				t.Fatalf("frontendBaseHref(%q) = %q; want %q", test.path, got, test.want)
			}
		})
	}
}

func TestFriendlyStorageFrontendRoutes(t *testing.T) {
	t.Parallel()
	publicFS, err := fs.Sub(embeddedPublic, "public")
	if err != nil {
		t.Fatal(err)
	}
	app := &application{
		config: appConfig{Runtime: defaultRuntimePolicy()},
		instances: map[string]*storageInstance{
			"api": {cfg: bucketConfig{ID: "api", AuthID: "host"}},
			"-":   {cfg: bucketConfig{ID: "-", AuthID: "-"}},
		},
		publicFS: publicFS,
	}
	handler := app.routes()

	for _, test := range []struct {
		path        string
		status      int
		content     string
		base        string
		location    string
		contentType string
	}{
		{path: "/", status: http.StatusOK, content: `id="browser"`, base: `<base href="./" />`, contentType: "text/html"},
		{path: "/-/host/api", status: http.StatusPermanentRedirect, location: "api/"},
		{path: "/-/host/api/", status: http.StatusOK, content: `id="browser"`, base: `<base href="../../../" />`, contentType: "text/html"},
		{path: "/-/-/-/", status: http.StatusOK, content: `id="browser"`, base: `<base href="../../../" />`, contentType: "text/html"},
		{path: "/-/host/api/folder/", status: http.StatusOK, content: `id="browser"`, base: `<base href="../../../../" />`, contentType: "text/html"},
		{path: "/-/host/api/document.pdf", status: http.StatusOK, content: `class="preview-root"`, base: `<base href="../../../" />`, contentType: "text/html"},
		{path: "/-/host/api/folder/document.pdf", status: http.StatusOK, content: `class="preview-root"`, base: `<base href="../../../../" />`, contentType: "text/html"},
		{path: "/-/api/", status: http.StatusNotFound},
		{path: "/-/missing/", status: http.StatusNotFound},
	} {
		t.Run(test.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, test.path, nil))
			if recorder.Code != test.status {
				t.Fatalf("GET %s status = %d; want %d; body=%s", test.path, recorder.Code, test.status, recorder.Body.String())
			}
			if test.location != "" && recorder.Header().Get("Location") != test.location {
				t.Fatalf("GET %s Location = %q; want %q", test.path, recorder.Header().Get("Location"), test.location)
			}
			if test.content != "" && !strings.Contains(recorder.Body.String(), test.content) {
				t.Fatalf("GET %s did not serve expected document marker %q", test.path, test.content)
			}
			if test.base != "" && !strings.Contains(recorder.Body.String(), test.base) {
				t.Fatalf("GET %s did not inject base %q", test.path, test.base)
			}
			if test.contentType != "" && !strings.Contains(recorder.Header().Get("Content-Type"), test.contentType) {
				t.Fatalf("GET %s Content-Type = %q", test.path, recorder.Header().Get("Content-Type"))
			}
		})
	}
}

func TestObjectReadEndpointsRefuseWriteOnlyInstance(t *testing.T) {
	t.Parallel()
	app, _, _ := testApplication(t)
	backend := newMemoryBackend(map[string]string{"a.txt": "secret"})
	cfg := bucketConfig{
		ID: "wo", Name: "wo", AuthID: "test-auth", Provider: "s3", Bucket: "wo-bucket", Region: "test",
		PermissionsDefined: true, Permissions: []string{permissionWrite},
	}
	app.instances["wo"] = &storageInstance{cfg: cfg, backend: backend, caps: initialCapabilities(cfg)}
	app.order = append(app.order, "wo")

	for _, endpoint := range []string{
		"/api/list", "/api/stats", "/api/archive", "/api/spreadsheet", "/api/delimited", "/api/document-count",
		"/api/document/word", "/api/parquet", "/api/json/raw", "/api/json/beautify", "/api/json/summary",
		"/api/json/tree", "/api/search", "/api/media-info", "/api/structured-preview", "/api/archive-preview",
		"/api/archive-entry", "/api/inspect", "/api/image-preview", "/s3", "/open/",
	} {
		recorder := httptest.NewRecorder()
		target := endpoint + "?instance=wo&key=a.txt&q=secret&entry=x&prefix=&size=6"
		app.routes().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
		if recorder.Code != http.StatusForbidden {
			t.Errorf("%s status = %d, want 403: %s", endpoint, recorder.Code, recorder.Body.String())
		}
	}
	if backend.getCount != 0 || backend.headCount != 0 || backend.listCalls != 0 {
		t.Fatalf("write-only instance reached the backend: gets=%d heads=%d lists=%d", backend.getCount, backend.headCount, backend.listCalls)
	}
}

// forbiddenKeyBackend answers 403 for one key, like a prefix-scoped IAM policy.
type forbiddenKeyBackend struct {
	*memoryBackend
	forbidden string
}

func (b *forbiddenKeyBackend) Get(ctx context.Context, key string, headers http.Header) (objectResponse, error) {
	if key == b.forbidden {
		return objectResponse{}, &upstreamError{StatusCode: http.StatusForbidden, Code: "AccessDenied"}
	}
	return b.memoryBackend.Get(ctx, key, headers)
}

func TestObjectDenialDoesNotBlockOtherRequests(t *testing.T) {
	app, _, _ := testApplication(t)
	instance := app.instances["rw"]
	instance.backend = &forbiddenKeyBackend{memoryBackend: newMemoryBackend(map[string]string{"tenant/ok.txt": "ok"}), forbidden: "tenant/secret.txt"}
	get := func(key string) int {
		recorder := httptest.NewRecorder()
		app.routes().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/s3?instance=rw&key="+key, nil))
		return recorder.Code
	}
	if code := get("secret.txt"); code != http.StatusForbidden {
		t.Fatalf("forbidden key status = %d, want 403", code)
	}
	if state := instance.capabilities().Read; state.State != capabilityDenied || state.Source != "provider" {
		t.Fatalf("observed denial is not reported: %#v", state)
	}
	if code := get("ok.txt"); code != http.StatusOK {
		t.Fatalf("one object's 403 blocked a different key: status %d", code)
	}
	if state := instance.capabilities().Read; state.State != capabilityAllowed {
		t.Fatalf("a later success did not replace the observed denial: %#v", state)
	}

	// The reported denial is a short-lived hint, not a lasting verdict.
	instance.observePermission(permissionWrite, &upstreamError{StatusCode: http.StatusForbidden})
	if instance.capabilities().Write.State != capabilityDenied {
		t.Fatal("write denial was not reported")
	}
	originalNow := timeNowUTC
	defer func() { timeNowUTC = originalNow }()
	timeNowUTC = func() time.Time { return originalNow().Add(observedDenialTTL + time.Second) }
	if state := instance.capabilities().Write; state.State != capabilityUnknown || !state.Allowed {
		t.Fatalf("observed denial did not expire: %#v", state)
	}

	// Configuration ceilings stay hard gates.
	if err := requirePermission(app.instances["readonly"], permissionWrite); err == nil {
		t.Fatal("configuration denial must still be enforced")
	}
}

func TestPermissionRefreshKeepsStateOnProbeErrors(t *testing.T) {
	t.Parallel()
	status := http.StatusInternalServerError
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	}))
	defer server.Close()
	backend := newAnonymousS3BackendForTest(t, server.URL, "bucket")
	cfg := bucketConfig{ID: "s3", Provider: "s3", Bucket: "bucket"}
	instance := &storageInstance{cfg: cfg, backend: backend, caps: initialCapabilities(cfg)}

	// A 5xx probe does not prove the credentials are denied.
	if read := instance.refreshCapabilities(context.Background()).Read; read.Verified || !read.Allowed {
		t.Fatalf("5xx probe produced a verified denial: %#v", read)
	}
	// A cancelled probe leaves the shared state untouched.
	status = http.StatusForbidden
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if read := instance.refreshCapabilities(ctx).Read; read.Verified || !read.Allowed {
		t.Fatalf("cancelled probe changed the capabilities: %#v", read)
	}
	// An explicit 403 on the bucket-level probe is still a verified denial.
	if read := instance.refreshCapabilities(context.Background()).Read; !read.Verified || read.Allowed || read.Source != "s3-list-probe" {
		t.Fatalf("403 probe was not recorded as a verified denial: %#v", read)
	}
}

func TestFrontendDepthIsCapped(t *testing.T) {
	// Not parallel: the prepared-response cache is process-wide.
	staticMu.Lock()
	savedCache, savedBytes := staticCache, staticCacheBytes
	staticCache, staticCacheBytes = map[string]*staticEntry{}, 0
	staticMu.Unlock()
	defer func() {
		staticMu.Lock()
		staticCache, staticCacheBytes = savedCache, savedBytes
		staticMu.Unlock()
	}()

	deep := "/-/host/bucket/" + strings.Repeat("/", 5000) + "x"
	if got := strings.Count(frontendBaseHref(deep), "../"); got != maxFrontendDepth {
		t.Fatalf("base href depth = %d, want clamp to %d", got, maxFrontendDepth)
	}
	app, _, _ := testApplication(t)
	for _, target := range []string{deep, deep + "/", "/" + strings.Repeat("a/", maxFrontendDepth+1) + "index.html"} {
		recorder := httptest.NewRecorder()
		app.routes().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("path of %d bytes: status %d, want 404", len(target), recorder.Code)
		}
	}
	staticMu.RLock()
	entries := len(staticCache)
	staticMu.RUnlock()
	if entries != 0 {
		t.Fatalf("refused paths left %d cached entries", entries)
	}

	// The byte budget stops caching; the response is still produced.
	block := make([]byte, staticCacheByteLimit/4)
	for i := 0; i < 8; i++ {
		if _, err := preparedStatic("v", "big.html", strconv.Itoa(i), func() ([]byte, error) { return block, nil }); err != nil {
			t.Fatal(err)
		}
	}
	staticMu.RLock()
	size := staticCacheBytes
	staticMu.RUnlock()
	if size > staticCacheByteLimit {
		t.Fatalf("static cache holds %d accounted bytes, over the %d budget", size, staticCacheByteLimit)
	}
}

func TestRequestLogHidesObjectPathsAndEscapesControlBytes(t *testing.T) {
	// Not parallel: the standard logger is process-wide.
	var output bytes.Buffer
	originalWriter := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(originalWriter)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	request := func(target string) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.URL.Path = target
		requestLogMiddleware(logModeAnonymous, next).ServeHTTP(httptest.NewRecorder(), req)
	}
	request("/-/host/bucket/customers/secret-report.pdf")
	request("/healthz\nGET /forged 200")
	request("/healthz\n\x1b[31mforged")
	logged := output.String()
	if strings.Contains(logged, "secret-report") || !strings.Contains(logged, `"/-/<path>"`) {
		t.Fatalf("object key path was logged in anonymous mode: %q", logged)
	}
	if strings.Count(logged, "\n") != 3 || strings.ContainsRune(logged, 0x1b) {
		t.Fatalf("control characters reached the log: %q", logged)
	}
}
