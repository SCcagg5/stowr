package main

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Server-side encryption (SSE) of the objects.
//
// The display: a HEAD answer gives the encryption of an object (x-amz-server-side-encryption is AES256 for SSE-S3 or
// aws:kms for SSE-KMS, with the KMS key id). The details get these headers through GET /api/media-info, as the other
// headers of the object. A provider does not let a client read an object with a key of the client (SSE-C) without that
// key: it refuses the HEAD and the GET with 400. The server does not ask for such a key. The details then say that
// the object has a key of the client. GET /api/encryption gives the default encryption of the bucket
// (GetBucketEncryption). The probe at start reads it one time. A provider without the request (Garage answers 501)
// hides it.
//
// The writes: the key bucket.encryption = "sse-s3" adds x-amz-server-side-encryption: AES256 to each request that
// writes an object: PutObject, CreateMultipartUpload (uploads and copies in parts), CopyObject (copy, move, rename,
// metadata edit, restore of a version). A copy without this header gets the default encryption of the destination
// bucket, so an object can lose its encryption in a copy. A metadata edit and a restore keep the SSE-S3 or SSE-KMS
// headers of their source. The key does not apply to UploadPart and UploadPartCopy: S3 takes the encryption of a
// multipart upload from its creation.
//
// Measured (see test/README.md): SeaweedFS 4.41 encrypts with a key encryption key (WEED_S3_SSE_KEK) and answers 500
// without it. MinIO encrypts with a KMS (MINIO_KMS_SECRET_KEY) and answers 501 without it. Garage v2.4.1 accepts the
// header and does not encrypt: the server writes a warning at start for such an instance.

const (
	// encryptionSSES3 is the value of bucket.encryption that asks for SSE-S3.
	encryptionSSES3 = "sse-s3"
	// sseS3Algorithm is the value of x-amz-server-side-encryption for SSE-S3.
	sseS3Algorithm = "AES256"

	sseHeader                  = "x-amz-server-side-encryption"
	sseKMSKeyHeader            = "x-amz-server-side-encryption-aws-kms-key-id"
	sseBucketKeyHeader         = "x-amz-server-side-encryption-bucket-key-enabled"
	sseCustomerAlgorithmHeader = "x-amz-server-side-encryption-customer-algorithm"
)

// encryptionResponseHeaders are the headers of a HEAD answer that tell the encryption of the object. The details show
// them. None of them is a secret: the MD5 of a key of the client (x-amz-server-side-encryption-customer-key-md5) does
// not go to the page.
var encryptionResponseHeaders = []string{sseHeader, sseKMSKeyHeader, sseBucketKeyHeader, sseCustomerAlgorithmHeader}

// applyWriteEncryption adds x-amz-server-side-encryption: AES256 to the headers of a write when the bucket has
// encryption = "sse-s3". Headers that already name an encryption (the SSE-KMS of the source of a metadata edit) stay.
func (s *s3Backend) applyWriteEncryption(headers http.Header) {
	if s.cfg.Encryption != encryptionSSES3 || headers.Get(sseHeader) != "" || headers.Get(sseCustomerAlgorithmHeader) != "" {
		return
	}
	headers.Set(sseHeader, sseS3Algorithm)
}

// keptEncryptionHeaders gives the SSE-S3 and SSE-KMS headers of an answer. A copy that sends them keeps the encryption
// of its source.
func keptEncryptionHeaders(header http.Header) http.Header {
	out := make(http.Header)
	for _, name := range []string{sseHeader, sseKMSKeyHeader, sseBucketKeyHeader} {
		if value := strings.TrimSpace(header.Get(name)); value != "" {
			out.Set(name, value)
		}
	}
	return out
}

// bucketEncryption is the default encryption of a bucket: the algorithm (AES256, aws:kms or aws:kms:dsse), the KMS key
// and the S3 Bucket Key.
type bucketEncryption struct {
	Algorithm        string `json:"algorithm"`
	KMSKeyID         string `json:"kmsKeyId,omitempty"`
	BucketKeyEnabled bool   `json:"bucketKeyEnabled,omitempty"`
}

// BucketEncryptionReader reads the default encryption of the bucket. Only the S3 backend has it.
type BucketEncryptionReader interface {
	BucketEncryption(context.Context) (*bucketEncryption, error)
}

// noBucketEncryption reports the answer of a bucket without a default encryption.
func noBucketEncryption(err error) bool {
	var upstream *upstreamError
	return errors.As(err, &upstream) && upstream.Code == "ServerSideEncryptionConfigurationNotFoundError"
}

// BucketEncryption reads GetBucketEncryption. A bucket without a default encryption gives nil.
func (s *s3Backend) BucketEncryption(ctx context.Context) (*bucketEncryption, error) {
	query := url.Values{}
	query.Set("encryption", "")
	resp, err := s.do(ctx, http.MethodGet, "", query, nil, 0, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := s3ResponseError(resp)
		if noBucketEncryption(err) {
			return nil, nil
		}
		return nil, err
	}
	var result struct {
		Rules []struct {
			Algorithm string `xml:"ApplyServerSideEncryptionByDefault>SSEAlgorithm"`
			KMSKeyID  string `xml:"ApplyServerSideEncryptionByDefault>KMSMasterKeyID"`
			BucketKey string `xml:"BucketKeyEnabled"`
		} `xml:"Rule"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("decode s3 bucket encryption response: %w", err)
	}
	for _, rule := range result.Rules {
		if algorithm := strings.TrimSpace(rule.Algorithm); algorithm != "" {
			return &bucketEncryption{
				Algorithm:        algorithm,
				KMSKeyID:         strings.TrimSpace(rule.KMSKeyID),
				BucketKeyEnabled: strings.EqualFold(strings.TrimSpace(rule.BucketKey), "true"),
			}, nil
		}
	}
	return nil, nil
}

// --- Storage instance -----------------------------------------------------------------------------------------------

// encryptionCapability is the state of the default encryption of the bucket of an instance in this process.
type encryptionCapability struct {
	// on is set when the probe read the default encryption of the bucket, or found that the bucket has none.
	on atomic.Bool

	mu sync.Mutex
	// algorithm is the default encryption of the bucket at start (AES256, aws:kms or aws:kms:dsse). It is empty when the
	// bucket has none, or when the probe could not read it.
	algorithm string
	// applied tells if the provider applies the encryption = "sse-s3" of the writes: encryptionAppliedYes, No or
	// Unknown. It is empty for an instance without the key.
	applied string
}

// The values of encryptionApplied in /api/instances.
const (
	encryptionAppliedYes     = "yes"
	encryptionAppliedNo      = "no"
	encryptionAppliedUnknown = "unknown"
)

// sseS3ProviderApplied tells what each known provider does with x-amz-server-side-encryption: AES256 (measured, see
// test/README.md). AWS S3, MinIO and SeaweedFS encrypt the object, or refuse the write when they have no key. Garage
// accepts the header and does not encrypt.
var sseS3ProviderApplied = map[string]string{
	flavourAWS:       encryptionAppliedYes,
	flavourMinIO:     encryptionAppliedYes,
	flavourSeaweedFS: encryptionAppliedYes,
	flavourGarage:    encryptionAppliedNo,
}

// encryptionState gives the default encryption of the bucket at start, and what the provider does with the
// encryption of the writes. The page shows a lock next to the name of the bucket from these two facts.
func (s *storageInstance) encryptionState() (algorithm, applied string) {
	if s == nil {
		return "", ""
	}
	s.encryption.mu.Lock()
	defer s.encryption.mu.Unlock()
	return s.encryption.algorithm, s.encryption.applied
}

// bucketEncryptionAvailable tells if the page can read the default encryption of the bucket. Until the probe has run,
// the answer is no.
func (s *storageInstance) bucketEncryptionAvailable() bool {
	if s == nil {
		return false
	}
	if _, ok := s.backend.(BucketEncryptionReader); !ok {
		return false
	}
	return s.encryption.on.Load()
}

// probeBucketEncryption reads the default encryption of the bucket. An answer (a configuration, or "no configuration")
// shows it. A provider without the request (501, 405) and any other failure hide it. The request is a read. An
// instance without the read permission is not probed. For an instance with encryption = "sse-s3", the probe finds the
// provider and keeps what it does with the header. On Garage, the probe writes a warning: Garage accepts the header and
// does not encrypt.
func (s *storageInstance) probeBucketEncryption(ctx context.Context) {
	reader, ok := s.backend.(BucketEncryptionReader)
	if !ok || !s.permitted(permissionRead) {
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	config, err := reader.BucketEncryption(probeCtx)
	s.encryption.on.Store(err == nil)
	algorithm := ""
	if err == nil && config != nil {
		algorithm = config.Algorithm
	}
	applied := ""
	if s.cfg.Encryption == encryptionSSES3 {
		flavour := s.lifecycleFlavour(probeCtx)
		applied = sseS3ProviderApplied[flavour]
		if applied == "" {
			applied = encryptionAppliedUnknown
		}
		if flavour == flavourGarage {
			log.Printf("config.warning bucket %q encryption = %q: the provider is Garage, which accepts x-amz-server-side-encryption and does not encrypt the objects", s.cfg.ID, s.cfg.Encryption)
		}
	}
	s.encryption.mu.Lock()
	s.encryption.algorithm, s.encryption.applied = algorithm, applied
	s.encryption.mu.Unlock()
}

// readBucketEncryption reads the default encryption of the bucket in a slot of the gates of the instance.
func (s *storageInstance) readBucketEncryption(ctx context.Context) (*bucketEncryption, error) {
	reader, ok := s.backend.(BucketEncryptionReader)
	if !ok || !s.encryption.on.Load() {
		return nil, apiError{Status: http.StatusNotImplemented, Code: "bucket_encryption_unsupported", Message: "this storage provider does not give the default encryption of the bucket"}
	}
	release, err := s.beginStorageRequest(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	config, err := reader.BucketEncryption(ctx)
	s.observePermission(permissionRead, err)
	return config, err
}

// --- Objects with a key of the client (SSE-C) -----------------------------------------------------------------------

// customerKeyRefusal reports the refusal of a read of an object with a key of the client. The providers answer 400 with
// different texts (measured): SeaweedFS "...Customer provided keys must provide the customer key", Garage "Object is
// encrypted", AWS and MinIO "The object was stored using a form of Server Side Encryption...". The answer to a HEAD
// request has no text.
func customerKeyRefusal(err error) bool {
	var upstream *upstreamError
	if !errors.As(err, &upstream) || upstream.StatusCode != http.StatusBadRequest {
		return false
	}
	text := strings.ToLower(upstream.Code + " " + upstream.Message)
	return strings.Contains(text, "encrypt") || strings.Contains(text, "customer")
}

// customerKeyMediaInfo gives the details of an object that the provider refused to read with err, when the refusal is
// for a key of the client. A refused HEAD has no text: a GET of one byte gives the text of the refusal. The size, the
// ETag and the date come from the listing (or from the list of the versions). The second result is false for any
// other error.
func customerKeyMediaInfo(ctx context.Context, instance *storageInstance, key string, err error) (mediaInfoResponse, bool) {
	var upstream *upstreamError
	if !errors.As(err, &upstream) || upstream.StatusCode != http.StatusBadRequest {
		return mediaInfoResponse{}, false
	}
	full := instance.fullKey(key)
	if !customerKeyRefusal(err) {
		response, getErr := instance.Get(ctx, full, http.Header{"Range": []string{"bytes=0-0"}})
		if getErr == nil {
			closeObjectResponse(response)
			return mediaInfoResponse{}, false
		}
		if !customerKeyRefusal(getErr) {
			return mediaInfoResponse{}, false
		}
	}
	response := mediaInfoResponse{Instance: instance.cfg.ID, Key: key, CustomerKey: true, Headers: map[string]string{}}
	if version := objectVersionFromContext(ctx); version != "" {
		response.Headers["x-amz-version-id"] = version
		if page, listErr := instance.ListObjectVersions(ctx, full, "", 1000); listErr == nil {
			for _, item := range page.Versions {
				if item.Version == version && !item.DeleteMarker {
					response.Size = item.Size
					response.Headers["etag"] = `"` + item.ETag + `"`
					response.Headers["last-modified"] = item.LastModified.UTC().Format(http.TimeFormat)
				}
			}
		}
	} else if page, listErr := instance.List(ctx, listOptions{Prefix: full, MaxResults: 1}); listErr == nil {
		for _, item := range page.Objects {
			if item.Key == full {
				response.Size = item.Size
				response.Headers["etag"] = `"` + item.ETag + `"`
				response.Headers["last-modified"] = item.LastModified.UTC().Format(http.TimeFormat)
			}
		}
	}
	return response, true
}

// --- HTTP -----------------------------------------------------------------------------------------------------------

type encryptionResponse struct {
	Instance string `json:"instance"`
	// Uploads is the encryption that the server asks for each object that it writes: "" or "sse-s3".
	Uploads string `json:"uploads"`
	// BucketDefault is the default encryption of the bucket. Null means that the bucket has none.
	BucketDefault *bucketEncryption `json:"bucketDefault"`
}

// handleEncryption gives the default encryption of the bucket and the encryption of the writes of the instance (GET,
// permission read).
func (a *application) handleEncryption(w http.ResponseWriter, r *http.Request) {
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
	if _, err := a.authorizeReadBucket(r, instance); err != nil {
		writeAPIError(w, err)
		return
	}
	config, err := instance.readBucketEncryption(r.Context())
	if err != nil {
		writeAPIError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, encryptionResponse{Instance: instance.cfg.ID, Uploads: instance.cfg.Encryption, BucketDefault: config})
}
