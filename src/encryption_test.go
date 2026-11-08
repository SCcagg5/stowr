package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// encryptionConfigText gives a configuration with one s3 bucket "b". The lines go in the block of the bucket.
func encryptionConfigText(lines string) string {
	return `
auth "public" {
  provider = "s3"
  mode     = "anonymous"
  endpoint = "http://localhost:9000"
  region   = "test"
}
bucket "b" {
  auth   = "public"
  bucket = "bucket"
` + lines + `
}
`
}

// encryptionGCSConfigText gives a configuration with one gcs bucket "b". The lines go in the block of the bucket.
func encryptionGCSConfigText(lines string) string {
	return `
auth "gcs" {
  provider         = "gcs"
  mode             = "service_account"
  credentials_file = "./gcs.json"
}
bucket "b" {
  auth   = "gcs"
  bucket = "bucket"
` + lines + `
}
`
}

func loadEncryptionConfig(t *testing.T, content string) (appConfig, error) {
	t.Helper()
	dir := t.TempDir()
	writeConfig(t, filepath.Join(dir, "gcs.json"), `{"type":"service_account"}`)
	path := filepath.Join(dir, "config.hcl")
	writeConfig(t, path, content+testOpenAccess)
	return loadConfig(path)
}

// The two valid values of bucket.encryption are "sse-s3" and "". A bucket without the key has "".
func TestBucketEncryptionConfigAcceptsTheTwoValues(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct{ lines, want string }{
		"sse-s3":    {`encryption = "sse-s3"`, "sse-s3"},
		"empty":     {`encryption = ""`, ""},
		"no key":    {``, ""},
		"with more": {"root_prefix = \"tenant/\"\nencryption = \"sse-s3\"", "sse-s3"},
	} {
		cfg, err := loadEncryptionConfig(t, encryptionConfigText(test.lines))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := cfg.Buckets[0].Encryption; got != test.want {
			t.Fatalf("%s: encryption = %q, want %q", name, got, test.want)
		}
	}
}

// Any other value stops the start. The server does not change the case and does not remove a space.
func TestBucketEncryptionConfigRefusesOtherValues(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"SSE-S3", "sse-s3 ", " sse-s3", "aes256", "AES256", "sse-kms", "aws:kms", "sse-c"} {
		_, err := loadEncryptionConfig(t, encryptionConfigText(fmt.Sprintf("encryption = %q", value)))
		if err == nil {
			t.Fatalf("%q: the start must stop", value)
		}
		for _, want := range []string{`bucket "b".encryption`, `"sse-s3"`, fmt.Sprintf("got %q", value), "SSE-C and SSE-KMS are not supported"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("%q: error = %v, want %q", value, err, want)
			}
		}
	}
}

// A value that is not a string stops the start.
func TestBucketEncryptionConfigRefusesAWrongType(t *testing.T) {
	t.Parallel()
	for _, line := range []string{`encryption = true`, `encryption = 1`, `encryption = ["sse-s3"]`} {
		_, err := loadEncryptionConfig(t, encryptionConfigText(line))
		if err == nil || !strings.Contains(err.Error(), `"encryption"`) {
			t.Fatalf("%s: error = %v", line, err)
		}
	}
}

// The GCS API has no SSE header: a gcs bucket with encryption = "sse-s3" stops the start.
func TestBucketEncryptionConfigRefusesAGCSBucket(t *testing.T) {
	t.Parallel()
	_, err := loadEncryptionConfig(t, encryptionGCSConfigText(`encryption = "sse-s3"`))
	if err == nil {
		t.Fatal("the start must stop")
	}
	for _, want := range []string{`bucket "b".encryption = "sse-s3"`, `needs an auth block with provider "s3"`, `auth "gcs" has provider "gcs"`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want %q", err, want)
		}
	}
	for _, line := range []string{``, `encryption = ""`} {
		cfg, err := loadEncryptionConfig(t, encryptionGCSConfigText(line))
		if err != nil {
			t.Fatalf("gcs bucket with %q: %v", line, err)
		}
		if cfg.Buckets[0].Provider != "gcs" || cfg.Buckets[0].Encryption != "" {
			t.Fatalf("gcs bucket = %+v", cfg.Buckets[0])
		}
	}
}

// print-config shows the source of the key, and the reference documents it.
func TestBucketEncryptionIsInTheKeyTable(t *testing.T) {
	t.Parallel()
	if !strings.Contains(configReference(), "| `encryption` | text | \"\" |") {
		t.Fatal("the configuration reference does not document bucket.encryption")
	}
	cfg, err := loadEncryptionConfig(t, encryptionConfigText(`encryption = "sse-s3"`))
	if err != nil {
		t.Fatal(err)
	}
	if text := formatEffectiveConfiguration(cfg); !strings.Contains(text, `bucket "b".encryption = "sse-s3"  # file`) {
		t.Fatalf("print-config with the key:\n%s", text)
	}
	cfg, err = loadEncryptionConfig(t, encryptionConfigText(``))
	if err != nil {
		t.Fatal(err)
	}
	if text := formatEffectiveConfiguration(cfg); !strings.Contains(text, `bucket "b".encryption = ""  # default`) {
		t.Fatalf("print-config without the key:\n%s", text)
	}
}

// recordedRequest is one request that fakeEncryptionS3 received.
type recordedRequest struct {
	method string
	path   string
	query  string
	header http.Header
}

// creates tells if the request makes an object: PutObject, CopyObject or CreateMultipartUpload.
func (r recordedRequest) creates() bool {
	query := mustParseQuery(r.query)
	if r.method == http.MethodPost {
		return query.Has("uploads")
	}
	return r.method == http.MethodPut && !query.Has("uploadId")
}

// writesInAnUpload tells if the request is UploadPart, UploadPartCopy or CompleteMultipartUpload.
func (r recordedRequest) writesInAnUpload() bool {
	return (r.method == http.MethodPut || r.method == http.MethodPost) && mustParseQuery(r.query).Has("uploadId")
}

// fakeEncryptionS3 is an S3 server for the encryption tests. It records each request. The fields choose the answers
// of HEAD, of the ranged GET, of GetBucketEncryption and of the listing.
type fakeEncryptionS3 struct {
	t  *testing.T
	mu sync.Mutex

	requests []recordedRequest

	// headStatus is the status of a HEAD (200 when 0). headHeader gives more headers to a successful HEAD.
	headStatus int
	headHeader http.Header
	// rangeStatus and rangeBody are the answer of a GET with a Range header (200 with no body when 0).
	rangeStatus int
	rangeBody   string
	// encryptionStatus and encryptionBody are the answer of GetBucketEncryption (404 without a body when 0).
	encryptionStatus int
	encryptionBody   string
	// listBody is the answer of ListObjectsV2.
	listBody string
	// server is the Server header of each answer. It tells the provider to the probe (empty: an unknown provider).
	server string
}

func newFakeEncryptionS3(t *testing.T) (*fakeEncryptionS3, *httptest.Server) {
	fake := &fakeEncryptionS3{t: t}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	return fake, server
}

func mustParseQuery(raw string) url.Values {
	values, _ := url.ParseQuery(raw)
	return values
}

func (f *fakeEncryptionS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, recordedRequest{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, header: r.Header.Clone()})
	if f.server != "" {
		w.Header().Set("Server", f.server)
	}
	query := r.URL.Query()
	bucketLevel := strings.Trim(r.URL.Path, "/") == "bucket"
	switch {
	case bucketLevel && query.Has("encryption"):
		status := f.encryptionStatus
		body := f.encryptionBody
		if status == 0 {
			status = http.StatusNotFound
			body = `<Error><Code>ServerSideEncryptionConfigurationNotFoundError</Code><Message>The server side encryption configuration was not found</Message></Error>`
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	case bucketLevel && query.Get("list-type") == "2":
		_, _ = io.WriteString(w, f.listBody)
	case r.Method == http.MethodHead:
		if f.headStatus != 0 && f.headStatus != http.StatusOK {
			w.WriteHeader(f.headStatus)
			return
		}
		for name, values := range f.headHeader {
			w.Header()[name] = values
		}
		w.Header().Set("Content-Length", "12")
		w.Header().Set("ETag", `"e"`)
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("x-amz-tagging-count", "0")
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet && query.Has("tagging"):
		_, _ = io.WriteString(w, `<Tagging><TagSet></TagSet></Tagging>`)
	case r.Method == http.MethodGet && r.Header.Get("Range") != "":
		if f.rangeStatus != 0 {
			w.WriteHeader(f.rangeStatus)
		}
		_, _ = io.WriteString(w, f.rangeBody)
	case r.Method == http.MethodPost && query.Has("uploads"):
		_, _ = io.WriteString(w, `<InitiateMultipartUploadResult><UploadId>u1</UploadId></InitiateMultipartUploadResult>`)
	case r.Method == http.MethodPost && query.Has("uploadId"):
		_, _ = io.WriteString(w, `<CompleteMultipartUploadResult/>`)
	case r.Method == http.MethodPut && query.Has("uploadId") && r.Header.Get("x-amz-copy-source") != "":
		_, _ = io.WriteString(w, `<CopyPartResult><ETag>"p"</ETag></CopyPartResult>`)
	case r.Method == http.MethodPut && query.Has("uploadId"):
		w.Header().Set("ETag", `"p"`)
	case r.Method == http.MethodPut && r.Header.Get("x-amz-copy-source") != "":
		_, _ = io.WriteString(w, `<CopyObjectResult><ETag>"e"</ETag></CopyObjectResult>`)
	case r.Method == http.MethodPut:
		w.Header().Set("ETag", `"e"`)
	default:
		w.WriteHeader(http.StatusNotImplemented)
	}
}

func (f *fakeEncryptionS3) snapshot() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.requests...)
}

// checkEncryptionHeaders makes sure that each request that creates an object has the header with the value want
// (no header for ""), and that no other request has it. It gives the number of requests that create an object and the
// number of requests of the upload.
func checkEncryptionHeaders(t *testing.T, fake *fakeEncryptionS3, want string) (creating, inUpload int) {
	t.Helper()
	for _, request := range fake.snapshot() {
		got := request.header.Get(sseHeader)
		switch {
		case request.creates():
			creating++
			if got != want {
				t.Fatalf("%s %s?%s: %s = %q, want %q", request.method, request.path, request.query, sseHeader, got, want)
			}
		default:
			if request.writesInAnUpload() {
				inUpload++
			}
			if got != "" {
				t.Fatalf("%s %s?%s must not send %s, got %q", request.method, request.path, request.query, sseHeader, got)
			}
		}
	}
	return creating, inUpload
}

// Each write that creates an object sends the header with sse-s3. UploadPart, UploadPartCopy and CompleteMultipartUpload
// do not send it. With encryption "" no request has it.
func TestS3BackendSendsTheEncryptionHeaderOnEachWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	operations := []struct {
		name     string
		run      func(*s3Backend) error
		creating int
		inUpload int
	}{
		{"Put", func(b *s3Backend) error { return b.Put(ctx, "a.txt", strings.NewReader("hello"), 5, "text/plain") }, 1, 0},
		{"PutIfAbsent", func(b *s3Backend) error {
			return b.PutIfAbsent(ctx, "a.txt", strings.NewReader("hello"), 5, "text/plain")
		}, 1, 0},
		{"InitiateMultipart", func(b *s3Backend) error {
			uploadID, err := b.InitiateMultipart(ctx, "a.bin", "application/octet-stream")
			if err != nil {
				return err
			}
			etag, err := b.UploadPart(ctx, "a.bin", uploadID, 1, strings.NewReader("hello"), 5)
			if err != nil {
				return err
			}
			return b.CompleteMultipart(ctx, "a.bin", uploadID, []s3CompletedPart{{PartNumber: 1, ETag: etag}})
		}, 1, 2},
		{"Copy", func(b *s3Backend) error { return b.Copy(ctx, "a.txt", "b.txt") }, 1, 0},
		{"CopyMultipart", func(b *s3Backend) error {
			return b.CopyMultipart(ctx, "a.bin", "b.bin", multipartCopyOptions{partSize: 5})
		}, 1, 4},
		{"RestoreObjectVersion", func(b *s3Backend) error { return b.RestoreObjectVersion(ctx, "a.txt", "v1") }, 1, 0},
		{"ReplaceObjectMetadata", func(b *s3Backend) error {
			return b.ReplaceObjectMetadata(ctx, "a.txt", "", "e", http.Header{"Content-Type": []string{"text/csv"}})
		}, 1, 0},
	}
	for _, mode := range []struct{ encryption, want string }{{encryptionSSES3, "AES256"}, {"", ""}} {
		for _, operation := range operations {
			t.Run(operation.name+"/"+fmt.Sprintf("%q", mode.encryption), func(t *testing.T) {
				t.Parallel()
				fake, server := newFakeEncryptionS3(t)
				backend := newAnonymousS3BackendForTest(t, server.URL, "bucket")
				backend.cfg.Encryption = mode.encryption
				if err := operation.run(backend); err != nil {
					t.Fatal(err)
				}
				creating, inUpload := checkEncryptionHeaders(t, fake, mode.want)
				if creating != operation.creating || inUpload != operation.inUpload {
					t.Fatalf("requests that create = %d, in the upload = %d, want %d and %d: %+v", creating, inUpload, operation.creating, operation.inUpload, fake.snapshot())
				}
			})
		}
	}
}

// The metadata edit of an object with SSE-KMS keeps aws:kms and its key. It does not add AES256, with or without the key
// encryption in the configuration.
func TestReplaceObjectMetadataKeepsTheKMSEncryptionOfTheSource(t *testing.T) {
	t.Parallel()
	source := http.Header{}
	source.Set("x-amz-server-side-encryption", "aws:kms")
	source.Set("x-amz-server-side-encryption-aws-kms-key-id", "arn:k")
	source.Set("x-amz-server-side-encryption-bucket-key-enabled", "true")
	source.Set("x-amz-storage-class", "STANDARD_IA")
	source.Set("x-amz-checksum-crc32", "ignored")
	for _, encryption := range []string{encryptionSSES3, ""} {
		t.Run(fmt.Sprintf("%q", encryption), func(t *testing.T) {
			t.Parallel()
			fake, server := newFakeEncryptionS3(t)
			backend := newAnonymousS3BackendForTest(t, server.URL, "bucket")
			backend.cfg.Encryption = encryption
			headers := keptObjectHeaders(source)
			headers.Set("Content-Type", "text/csv")
			if err := backend.ReplaceObjectMetadata(context.Background(), "a.txt", "", "e", headers); err != nil {
				t.Fatal(err)
			}
			requests := fake.snapshot()
			if len(requests) != 1 {
				t.Fatalf("requests = %+v", requests)
			}
			header := requests[0].header
			if got := header.Values(sseHeader); len(got) != 1 || got[0] != "aws:kms" {
				t.Fatalf("%s = %v, want aws:kms only", sseHeader, got)
			}
			if got := header.Get(sseKMSKeyHeader); got != "arn:k" {
				t.Fatalf("%s = %q", sseKMSKeyHeader, got)
			}
			if got := header.Get(sseBucketKeyHeader); got != "true" {
				t.Fatalf("%s = %q", sseBucketKeyHeader, got)
			}
			if got := header.Get("x-amz-storage-class"); got != "STANDARD_IA" {
				t.Fatalf("storage class = %q", got)
			}
		})
	}
}

// The keptEncryptionHeaders function gives only the SSE-S3 and SSE-KMS headers, without a space at the ends.
func TestKeptEncryptionHeadersGiveOnlyTheServerKeyHeaders(t *testing.T) {
	t.Parallel()
	source := http.Header{}
	source.Set("x-amz-server-side-encryption", " aws:kms ")
	source.Set("x-amz-server-side-encryption-aws-kms-key-id", "arn:k")
	source.Set("x-amz-server-side-encryption-customer-algorithm", "AES256")
	source.Set("x-amz-server-side-encryption-customer-key-md5", "abc")
	source.Set("Content-Type", "text/plain")
	kept := keptEncryptionHeaders(source)
	if len(kept) != 2 || kept.Get(sseHeader) != "aws:kms" || kept.Get(sseKMSKeyHeader) != "arn:k" {
		t.Fatalf("kept = %v", kept)
	}
	if len(keptEncryptionHeaders(http.Header{})) != 0 {
		t.Fatal("an answer without SSE must give no header")
	}
}

// restoreEncryptionApplication gives the instance "rw" a fake bucket with versioning.
func restoreEncryptionApplication(t *testing.T, encryption string) (*storageInstance, *fakeEncryptionS3) {
	t.Helper()
	app, _, _ := testApplication(t)
	fake, server := newFakeEncryptionS3(t)
	instance := app.instances["rw"]
	backend := newAnonymousS3BackendForTest(t, server.URL, "bucket")
	backend.cfg.Encryption = encryption
	instance.backend = backend
	instance.cfg.Encryption = encryption
	instance.setVersioningAvailable(true)
	return instance, fake
}

// The restore of a version keeps the SSE-S3 or SSE-KMS of this version, and it adds the encryption of the instance when
// the version has none.
func TestRestoreVersionKeepsTheEncryptionOfTheVersion(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		encryption string
		version    http.Header
		want       string
		wantKey    string
	}{
		{"version AES256, instance none", "", http.Header{sseHeader: {"AES256"}}, "AES256", ""},
		{"version none, instance none", "", http.Header{}, "", ""},
		{"version none, instance sse-s3", encryptionSSES3, http.Header{}, "AES256", ""},
		{"version KMS, instance sse-s3", encryptionSSES3, http.Header{sseHeader: {"aws:kms"}, sseKMSKeyHeader: {"arn:k"}}, "aws:kms", "arn:k"},
		{"version KMS, instance none", "", http.Header{sseHeader: {"aws:kms"}, sseKMSKeyHeader: {"arn:k"}}, "aws:kms", "arn:k"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			instance, fake := restoreEncryptionApplication(t, test.encryption)
			fake.headHeader = test.version
			if err := instance.RestoreVersion(context.Background(), "tenant/doc.txt", "v1"); err != nil {
				t.Fatal(err)
			}
			var copies []recordedRequest
			for _, request := range fake.snapshot() {
				if request.method == http.MethodPut {
					copies = append(copies, request)
				}
			}
			if len(copies) != 1 || copies[0].header.Get("x-amz-copy-source") == "" {
				t.Fatalf("copy requests = %+v", copies)
			}
			if got := copies[0].header.Values(sseHeader); test.want == "" && len(got) != 0 || test.want != "" && (len(got) != 1 || got[0] != test.want) {
				t.Fatalf("%s = %v, want %q", sseHeader, got, test.want)
			}
			if got := copies[0].header.Get(sseKMSKeyHeader); got != test.wantKey {
				t.Fatalf("%s = %q, want %q", sseKMSKeyHeader, got, test.wantKey)
			}
		})
	}
}

// A version that goes in parts keeps its encryption too: the header is on CreateMultipartUpload only.
// The test changes a package variable, so it does not run in parallel.
func TestRestoreVersionInPartsKeepsTheEncryptionOfTheVersion(t *testing.T) {
	previous := multipartCopyThreshold
	multipartCopyThreshold = 10
	t.Cleanup(func() { multipartCopyThreshold = previous })
	instance, fake := restoreEncryptionApplication(t, "")
	fake.headHeader = http.Header{sseHeader: {"AES256"}}
	if err := instance.RestoreVersion(context.Background(), "tenant/doc.txt", "v1"); err != nil {
		t.Fatal(err)
	}
	creating, inUpload := checkEncryptionHeaders(t, fake, "AES256")
	if creating != 1 || inUpload != 2 {
		t.Fatalf("requests that create = %d, in the upload = %d: %+v", creating, inUpload, fake.snapshot())
	}
}

const bucketEncryptionKMS = `<ServerSideEncryptionConfiguration><Rule><ApplyServerSideEncryptionByDefault><SSEAlgorithm>aws:kms</SSEAlgorithm><KMSMasterKeyID>arn:k</KMSMasterKeyID></ApplyServerSideEncryptionByDefault><BucketKeyEnabled>true</BucketKeyEnabled></Rule></ServerSideEncryptionConfiguration>`

const bucketEncryptionAES = `<ServerSideEncryptionConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Rule><ApplyServerSideEncryptionByDefault><SSEAlgorithm>AES256</SSEAlgorithm></ApplyServerSideEncryptionByDefault><BucketKeyEnabled>false</BucketKeyEnabled></Rule></ServerSideEncryptionConfiguration>`

const bucketEncryptionGarage = `<Error><Code>NotImplemented</Code><Message>Unimplemented action: GetBucketEncryption</Message><Resource>/default</Resource><Region>garage</Region></Error>`

// BucketEncryption reads the configuration of the bucket, and it sends GET /bucket?encryption.
func TestBucketEncryptionReadsTheDefaultEncryption(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		status int
		body   string
		want   *bucketEncryption
	}{
		{"kms", http.StatusOK, bucketEncryptionKMS, &bucketEncryption{Algorithm: "aws:kms", KMSKeyID: "arn:k", BucketKeyEnabled: true}},
		{"aes256", http.StatusOK, bucketEncryptionAES, &bucketEncryption{Algorithm: "AES256"}},
		{"empty answer", http.StatusOK, "", nil},
		{"no rule", http.StatusOK, `<ServerSideEncryptionConfiguration/>`, nil},
		{"not found", http.StatusNotFound, `<Error><Code>ServerSideEncryptionConfigurationNotFoundError</Code><Message>none</Message></Error>`, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fake, server := newFakeEncryptionS3(t)
			fake.encryptionStatus, fake.encryptionBody = test.status, test.body
			backend := newAnonymousS3BackendForTest(t, server.URL, "bucket")
			got, err := backend.BucketEncryption(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if (got == nil) != (test.want == nil) || got != nil && *got != *test.want {
				t.Fatalf("encryption = %+v, want %+v", got, test.want)
			}
			requests := fake.snapshot()
			if len(requests) != 1 || requests[0].method != http.MethodGet || requests[0].path != "/bucket" || requests[0].query != "encryption=" {
				t.Fatalf("requests = %+v", requests)
			}
		})
	}
}

// A provider without the request gives an error (Garage answers 501).
func TestBucketEncryptionGivesAnErrorWhenTheProviderHasNoRequest(t *testing.T) {
	t.Parallel()
	fake, server := newFakeEncryptionS3(t)
	fake.encryptionStatus, fake.encryptionBody = http.StatusNotImplemented, bucketEncryptionGarage
	backend := newAnonymousS3BackendForTest(t, server.URL, "bucket")
	got, err := backend.BucketEncryption(context.Background())
	if err == nil || got != nil || noBucketEncryption(err) {
		t.Fatalf("encryption = %+v, err = %v", got, err)
	}
}

// encryptionProbeApplication gives the instance "rw" a fake bucket, and it runs the probe.
func encryptionProbeApplication(t *testing.T, status int, body string) (*application, *storageInstance) {
	t.Helper()
	app, _, _ := testApplication(t)
	fake, server := newFakeEncryptionS3(t)
	fake.encryptionStatus, fake.encryptionBody = status, body
	instance := app.instances["rw"]
	instance.backend = newAnonymousS3BackendForTest(t, server.URL, "bucket")
	if instance.bucketEncryptionAvailable() {
		t.Fatal("the bucket encryption must be hidden before the probe")
	}
	instance.probeBucketEncryption(context.Background())
	return app, instance
}

func instancesBucketEncryptionSupported(t *testing.T, app *application) bool {
	t.Helper()
	recorder := doJSON(app, http.MethodGet, "/api/instances", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Instances []instanceInfo `json:"instances"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	for _, info := range payload.Instances {
		if info.ID == "rw" {
			return info.BucketEncryption
		}
	}
	t.Fatal("no instance rw")
	return false
}

// A configuration, and the answer "no configuration", show the bucket encryption. A 501 hides it.
func TestProbeBucketEncryptionShowsOrHidesTheFeature(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"configuration", http.StatusOK, bucketEncryptionKMS, true},
		{"not found", 0, "", true},
		{"not implemented", http.StatusNotImplemented, bucketEncryptionGarage, false},
		{"server error", http.StatusInternalServerError, `<Error><Code>InternalError</Code></Error>`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			app, instance := encryptionProbeApplication(t, test.status, test.body)
			if got := instance.bucketEncryptionAvailable(); got != test.want {
				t.Fatalf("available = %v, want %v", got, test.want)
			}
			if got := instancesBucketEncryptionSupported(t, app); got != test.want {
				t.Fatalf("bucketEncryptionSupported = %v, want %v", got, test.want)
			}
		})
	}
}

// /api/instances gives the key encryption, the default encryption of the bucket at start, and what the provider does
// with the key. The page shows the lock of the bucket from them.
func TestInstancesGiveTheEncryptionOfTheBucket(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		encryption string
		server     string
		status     int
		body       string
		algorithm  string
		applied    string
	}{
		{"no key and no default", "", "MinIO", 0, "", "", ""},
		{"default of the bucket", "", "MinIO", http.StatusOK, bucketEncryptionAES, "AES256", ""},
		{"default with KMS", "", "SeaweedFS 30GB 4.41", http.StatusOK, bucketEncryptionKMS, "aws:kms", ""},
		{"key on MinIO", encryptionSSES3, "MinIO", 0, "", "", encryptionAppliedYes},
		{"key on SeaweedFS", encryptionSSES3, "SeaweedFS 30GB 4.41", 0, "", "", encryptionAppliedYes},
		{"key on AWS", encryptionSSES3, "AmazonS3", 0, "", "", encryptionAppliedYes},
		{"key on Garage", encryptionSSES3, "Garage", http.StatusNotImplemented, bucketEncryptionGarage, "", encryptionAppliedNo},
		{"key on an unknown provider", encryptionSSES3, "", 0, "", "", encryptionAppliedUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			app, _, _ := testApplication(t)
			fake, server := newFakeEncryptionS3(t)
			fake.encryptionStatus, fake.encryptionBody, fake.server = test.status, test.body, test.server
			instance := app.instances["rw"]
			instance.backend = newAnonymousS3BackendForTest(t, server.URL, "bucket")
			instance.cfg.Encryption = test.encryption
			instance.probeBucketEncryption(context.Background())
			recorder := doJSON(app, http.MethodGet, "/api/instances", nil)
			var payload struct {
				Instances []map[string]any `json:"instances"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			for _, info := range payload.Instances {
				if info["id"] != "rw" {
					continue
				}
				applied, present := info["encryptionApplied"]
				if info["encryption"] != test.encryption || info["defaultEncryption"] != test.algorithm || present != (test.applied != "") || present && applied != test.applied {
					t.Fatalf("instance = %+v", info)
				}
				return
			}
			t.Fatal("no instance rw")
		})
	}
}

// GET /api/encryption gives the encryption of the writes and the default encryption of the bucket.
func TestEncryptionAPIGivesTheDefaultsOfTheBucket(t *testing.T) {
	t.Parallel()
	app, instance := encryptionProbeApplication(t, http.StatusOK, bucketEncryptionKMS)
	instance.cfg.Encryption = encryptionSSES3
	recorder := doJSON(app, http.MethodGet, "/api/encryption?instance=rw", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Instance      string `json:"instance"`
		Uploads       string `json:"uploads"`
		BucketDefault *struct {
			Algorithm        string `json:"algorithm"`
			KMSKeyID         string `json:"kmsKeyId"`
			BucketKeyEnabled bool   `json:"bucketKeyEnabled"`
		} `json:"bucketDefault"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Instance != "rw" || response.Uploads != "sse-s3" || response.BucketDefault == nil ||
		response.BucketDefault.Algorithm != "aws:kms" || response.BucketDefault.KMSKeyID != "arn:k" || !response.BucketDefault.BucketKeyEnabled {
		t.Fatalf("response = %s", recorder.Body.String())
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if post := doJSON(app, http.MethodPost, "/api/encryption?instance=rw", nil); post.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d", post.Code)
	}
}

// A bucket without a default encryption gives null.
func TestEncryptionAPIGivesNullForABucketWithoutEncryption(t *testing.T) {
	t.Parallel()
	app, _ := encryptionProbeApplication(t, 0, "")
	recorder := doJSON(app, http.MethodGet, "/api/encryption?instance=rw", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d %s", recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	value, present := response["bucketDefault"]
	if !present || value != nil || response["uploads"] != "" {
		t.Fatalf("response = %s", recorder.Body.String())
	}
}

// After a probe with a 501, the API answers 501 with the code bucket_encryption_unsupported.
func TestEncryptionAPIAnswers501WhenTheProviderHasNoRequest(t *testing.T) {
	t.Parallel()
	app, _ := encryptionProbeApplication(t, http.StatusNotImplemented, bucketEncryptionGarage)
	recorder := doJSON(app, http.MethodGet, "/api/encryption?instance=rw", nil)
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d %s", recorder.Code, recorder.Body.String())
	}
	if code, _ := lockErrorCode(t, recorder); code != "bucket_encryption_unsupported" {
		t.Fatalf("code = %q, body = %s", code, recorder.Body.String())
	}
}

// mediaInfoApplication gives the instance "rw" (root prefix tenant/) a fake bucket.
func mediaInfoApplication(t *testing.T) (*application, *fakeEncryptionS3) {
	t.Helper()
	app, _, _ := testApplication(t)
	fake, server := newFakeEncryptionS3(t)
	app.instances["rw"].backend = newAnonymousS3BackendForTest(t, server.URL, "bucket")
	return app, fake
}

func decodeMediaInfo(t *testing.T, recorder *httptest.ResponseRecorder) mediaInfoResponse {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d %s", recorder.Code, recorder.Body.String())
	}
	var response mediaInfoResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

// The details give the encryption headers of the object. They never give the MD5 of a key of the client.
func TestMediaInfoGivesTheEncryptionHeaders(t *testing.T) {
	t.Parallel()
	app, fake := mediaInfoApplication(t)
	fake.headHeader = http.Header{
		sseHeader:          {"aws:kms"},
		sseKMSKeyHeader:    {"arn:k"},
		sseBucketKeyHeader: {"true"},
		"X-Amz-Server-Side-Encryption-Customer-Key-Md5": {"abc"},
	}
	response := decodeMediaInfo(t, doJSON(app, http.MethodGet, "/api/media-info?instance=rw&key=notes.txt", nil))
	if response.CustomerKey {
		t.Fatal("an object with a head answer is not an object with a key of the client")
	}
	if response.Headers[sseHeader] != "aws:kms" || response.Headers[sseKMSKeyHeader] != "arn:k" || response.Headers[sseBucketKeyHeader] != "true" {
		t.Fatalf("headers = %v", response.Headers)
	}
	if _, found := response.Headers["x-amz-server-side-encryption-customer-key-md5"]; found {
		t.Fatalf("the MD5 of the key of the client is in the headers: %v", response.Headers)
	}
}

const (
	customerKeySeaweedFS = `<Error><Code>InvalidArgument</Code><Message>Requests specifying Server Side Encryption with Customer provided keys must provide the customer key.</Message></Error>`
	customerKeyGarage    = `<Error><Code>InvalidRequest</Code><Message>Bad request: Object is encrypted</Message><Region>garage</Region></Error>`
	secretListing        = `<ListBucketResult><Contents><Key>tenant/secret.txt</Key><Size>6</Size><LastModified>2026-10-20T10:00:00Z</LastModified><ETag>"abc"</ETag></Contents></ListBucketResult>`
)

// An object with a key of the client: the provider refuses the HEAD (400 without a body) and the ranged GET (400 with a
// text about the key). The details answer 200 with customerKey and the size of the listing.
func TestMediaInfoTellsAnObjectWithAKeyOfTheClient(t *testing.T) {
	t.Parallel()
	for name, refusal := range map[string]string{"SeaweedFS": customerKeySeaweedFS, "Garage": customerKeyGarage} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			app, fake := mediaInfoApplication(t)
			fake.headStatus = http.StatusBadRequest
			fake.rangeStatus, fake.rangeBody = http.StatusBadRequest, refusal
			fake.listBody = secretListing
			response := decodeMediaInfo(t, doJSON(app, http.MethodGet, "/api/media-info?instance=rw&key=secret.txt", nil))
			if !response.CustomerKey || response.Size != 6 || response.Key != "secret.txt" || response.Instance != "rw" {
				t.Fatalf("response = %+v", response)
			}
			if response.Headers["etag"] != `"abc"` || response.Headers["last-modified"] == "" {
				t.Fatalf("headers = %v", response.Headers)
			}
		})
	}
}

// A refused HEAD whose ranged GET gives another 400 stays an error. A refused HEAD whose ranged GET works stays an error too.
func TestMediaInfoKeepsTheErrorOfAnOtherRefusal(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		status int
		body   string
		want   int
	}{
		"unrelated text":   {http.StatusBadRequest, `<Error><Code>InvalidArgument</Code><Message>bad range</Message></Error>`, http.StatusBadRequest},
		"ranged GET works": {0, "x", http.StatusBadRequest},
		"forbidden":        {http.StatusForbidden, customerKeyGarage, http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			app, fake := mediaInfoApplication(t)
			fake.headStatus = http.StatusBadRequest
			fake.rangeStatus, fake.rangeBody = test.status, test.body
			fake.listBody = secretListing
			recorder := doJSON(app, http.MethodGet, "/api/media-info?instance=rw&key=secret.txt", nil)
			if recorder.Code != test.want || strings.Contains(recorder.Body.String(), "customerKey") {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

// The function customerKeyRefusal accepts only a 400 whose text names the encryption or the customer.
func TestCustomerKeyRefusalNeedsA400WithTheRightText(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		err  error
		want bool
	}{
		{&upstreamError{StatusCode: 400, Code: "InvalidRequest", Message: "Bad request: Object is encrypted"}, true},
		{&upstreamError{StatusCode: 400, Code: "InvalidArgument", Message: "Server Side Encryption with Customer provided keys"}, true},
		{&upstreamError{StatusCode: 400, Code: "InvalidArgument", Message: "bad range"}, false},
		{&upstreamError{StatusCode: 400, Message: "400 Bad Request"}, false},
		{&upstreamError{StatusCode: 403, Code: "AccessDenied", Message: "encrypted"}, false},
		{fmt.Errorf("encrypted"), false},
		{nil, false},
	} {
		if got := customerKeyRefusal(test.err); got != test.want {
			t.Fatalf("customerKeyRefusal(%v) = %v, want %v", test.err, got, test.want)
		}
	}
}
