package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// objectVersion describes one immutable provider version or generation.
// Version is the provider identifier that must be supplied to subsequent
// version-aware operations.
type storedObjectVersion struct {
	Version      string            `json:"version"`
	IsCurrent    bool              `json:"isCurrent"`
	DeleteMarker bool              `json:"deleteMarker,omitempty"`
	Size         int64             `json:"size"`
	LastModified time.Time         `json:"lastModified,omitempty"`
	ETag         string            `json:"etag,omitempty"`
	ContentType  string            `json:"contentType,omitempty"`
	Checksums    map[string]string `json:"checksums,omitempty"`
}

type objectVersionPage struct {
	Versions      []storedObjectVersion `json:"versions"`
	NextPageToken string                `json:"nextPageToken,omitempty"`
}

type multipartPart struct {
	PartNumber     int
	ETag           string
	Size           int64
	ChecksumSHA256 string
}

type ObjectVersionStore interface {
	ListObjectVersions(context.Context, string, string, int) (objectVersionPage, error)
	HeadObjectVersion(context.Context, string, string) (objectResponse, error)
	GetObjectVersion(context.Context, string, string, http.Header) (objectResponse, error)
	DeleteObjectVersion(context.Context, string, string) error
	RestoreObjectVersion(context.Context, string, string) error
}

// versionRestorerWithHeaders restores a version with extra headers of the copy, for example its encryption.
type versionRestorerWithHeaders interface {
	RestoreObjectVersionWithHeaders(context.Context, string, string, http.Header) error
}

type MultipartPartLister interface {
	ListMultipartParts(context.Context, string, string) ([]multipartPart, error)
}

func (s *storageInstance) versionStore() (ObjectVersionStore, error) {
	store, ok := s.backend.(ObjectVersionStore)
	if !ok || (s.versioningProbeFinished() && !s.versioningAvailable()) {
		return nil, apiError{Status: http.StatusNotImplemented, Code: "versioning_unavailable", Message: "object versioning is not available for this storage bucket"}
	}
	return store, nil
}

func (s *storageInstance) ListObjectVersions(ctx context.Context, key, pageToken string, maximum int) (objectVersionPage, error) {
	store, err := s.versionStore()
	if err != nil {
		return objectVersionPage{}, err
	}
	release, err := s.beginStorageRequest(ctx)
	if err != nil {
		return objectVersionPage{}, err
	}
	defer release()
	page, err := store.ListObjectVersions(ctx, key, pageToken, maximum)
	s.observePermission(permissionRead, err)
	return page, err
}

func (s *storageInstance) HeadVersion(ctx context.Context, key, version string) (objectResponse, error) {
	if strings.TrimSpace(version) == "" {
		return s.Head(ctx, key)
	}
	store, err := s.versionStore()
	if err != nil {
		return objectResponse{}, err
	}
	release, err := s.beginStorageRequest(ctx)
	if err != nil {
		return objectResponse{}, err
	}
	defer release()
	response, err := store.HeadObjectVersion(ctx, key, version)
	if err == nil && (response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden) {
		err = &upstreamError{StatusCode: response.StatusCode, Code: "AccessDenied"}
	}
	s.observePermission(permissionRead, err)
	return response, err
}

func (s *storageInstance) GetVersion(ctx context.Context, key, version string, headers http.Header) (objectResponse, error) {
	if strings.TrimSpace(version) == "" {
		return s.Get(ctx, key, headers)
	}
	store, err := s.versionStore()
	if err != nil {
		return objectResponse{}, err
	}
	release, err := s.beginStorageRequest(ctx)
	if err != nil {
		return objectResponse{}, err
	}
	headers = withoutVersionPreconditions(headers)
	response, err := store.GetObjectVersion(ctx, key, version, headers)
	if err != nil {
		release()
		s.observePermission(permissionRead, err)
		return objectResponse{}, err
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		release()
		if response.Body != nil {
			_ = response.Body.Close()
		}
		err = &upstreamError{StatusCode: response.StatusCode, Code: "AccessDenied"}
		s.observePermission(permissionRead, err)
		return objectResponse{}, err
	}
	// The ceiling object.read: the check comes before any byte goes to the caller.
	if err := checkReadAnswer(ctx, response); err != nil {
		release()
		if response.Body != nil {
			_ = response.Body.Close()
		}
		return objectResponse{}, err
	}
	if response.Body != nil {
		response.Body = &releaseReadCloser{
			ReadCloser: &budgetReadCloser{body: response.Body, budget: budgetFromContext(ctx)},
			release:    release,
		}
	} else {
		release()
	}
	s.observePermission(permissionRead, nil)
	return response, nil
}

// DeleteVersion deletes one version. A refusal that an active Object Lock explains gives the error object_locked.
func (s *storageInstance) DeleteVersion(ctx context.Context, key, version string) error {
	if strings.TrimSpace(version) == "" {
		return apiError{Status: http.StatusBadRequest, Code: "version_required", Message: "version is required"}
	}
	store, err := s.versionStore()
	if err != nil {
		return err
	}
	release, err := s.beginStorageRequest(ctx)
	if err != nil {
		return err
	}
	err = store.DeleteObjectVersion(ctx, key, version)
	release()
	// SeaweedFS refuses the delete of a locked version with 403: that refusal says nothing about the delete permission.
	if err = s.explainLockRefusal(ctx, key, version, err); isObjectLocked(err) {
		return err
	}
	s.observePermission(permissionDelete, err)
	return err
}

func (s *storageInstance) RestoreVersion(ctx context.Context, key, version string) error {
	if strings.TrimSpace(version) == "" {
		return apiError{Status: http.StatusBadRequest, Code: "version_required", Message: "version is required"}
	}
	store, err := s.versionStore()
	if err != nil {
		return err
	}
	// A version from multipartCopyThreshold up is too large for CopyObject: it goes in parts. The copy sends the SSE-S3
	// or SSE-KMS headers of the version, so the restored object keeps its encryption. When the HEAD fails, the plain
	// restore below gives the error of the provider.
	//
	// A ceiling of read or write applies to the size of the version: the HEAD request gives it, and the check comes before
	// the restore. A HEAD request that fails stops the restore when a ceiling applies.
	var kept http.Header
	_, inParts := s.backend.(ObjectMultipartCopier)
	bound := ceilingApplies(ctx, actionObjectRead, actionObjectWrite)
	if inParts || bound {
		response, headErr := s.HeadVersion(ctx, key, version)
		if headErr == nil {
			closeObjectResponse(response)
			size, sizeErr := strconv.ParseInt(strings.TrimSpace(response.Header.Get("Content-Length")), 10, 64)
			if bound {
				known := size
				if sizeErr != nil || size < 0 {
					known = -1
				}
				if err := checkCopyCeilings(ctx, known); err != nil {
					return err
				}
			}
			if inParts {
				kept = keptEncryptionHeaders(response.Header)
				if sizeErr == nil && s.copiesInParts(size) {
					return s.copyMultipartObject(ctx, key, key, multipartCopyOptions{SourceVersion: version, ExtraHeaders: kept})
				}
			}
		} else if bound {
			return headErr
		}
	}
	release, err := s.beginStorageRequest(ctx)
	if err != nil {
		return err
	}
	defer release()
	if restorer, ok := store.(versionRestorerWithHeaders); ok && len(kept) > 0 {
		err = restorer.RestoreObjectVersionWithHeaders(ctx, key, version, kept)
	} else {
		err = store.RestoreObjectVersion(ctx, key, version)
	}
	if err == nil {
		s.observePermission(permissionRead, nil)
		s.observePermission(permissionWrite, nil)
	} else {
		s.observePermission(permissionWrite, err)
	}
	return err
}

func (s *storageInstance) ListMultipartParts(ctx context.Context, key, uploadID string) ([]multipartPart, error) {
	backend, ok := s.backend.(MultipartPartLister)
	if !ok {
		return nil, errors.New("storage backend does not support multipart part reconciliation")
	}
	release, err := s.beginStorageRequest(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	parts, err := backend.ListMultipartParts(ctx, key, uploadID)
	s.observePermission(permissionWrite, err)
	return parts, err
}

// providerChecksums returns normalized checksums carried by object response
// headers. It never interprets multipart ETags as hashes.
func providerChecksums(header http.Header) map[string]string {
	if header == nil {
		return nil
	}
	candidates := map[string][]string{
		"sha256": {"x-amz-checksum-sha256", "x-goog-hash-sha256"},
		"sha1":   {"x-amz-checksum-sha1"},
		"crc32c": {"x-amz-checksum-crc32c", "x-goog-hash-crc32c"},
		"crc32":  {"x-amz-checksum-crc32"},
		"md5":    {"content-md5", "x-goog-hash-md5"},
	}
	out := make(map[string]string)
	for algorithm, names := range candidates {
		for _, name := range names {
			value := strings.TrimSpace(header.Get(name))
			if value == "" {
				continue
			}
			out[algorithm] = strings.Trim(value, `"`)
			break
		}
	}
	if hashHeader := header.Values("x-goog-hash"); len(hashHeader) > 0 {
		for _, combined := range hashHeader {
			for _, item := range strings.Split(combined, ",") {
				name, value, found := strings.Cut(strings.TrimSpace(item), "=")
				if !found || strings.TrimSpace(value) == "" {
					continue
				}
				switch strings.ToLower(strings.TrimSpace(name)) {
				case "crc32c", "md5":
					out[strings.ToLower(strings.TrimSpace(name))] = strings.TrimSpace(value)
				}
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func sortedVersions(versions []storedObjectVersion) {
	sort.SliceStable(versions, func(i, j int) bool {
		if versions[i].IsCurrent != versions[j].IsCurrent {
			return versions[i].IsCurrent
		}
		if !versions[i].LastModified.Equal(versions[j].LastModified) {
			return versions[i].LastModified.After(versions[j].LastModified)
		}
		return versions[i].Version > versions[j].Version
	})
}

type s3VersionCursor struct {
	KeyMarker       string `json:"keyMarker,omitempty"`
	VersionIDMarker string `json:"versionIdMarker,omitempty"`
}

func encodeS3VersionCursor(cursor s3VersionCursor) string {
	data, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodeS3VersionCursor(token string) (s3VersionCursor, error) {
	if strings.TrimSpace(token) == "" {
		return s3VersionCursor{}, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return s3VersionCursor{}, fmt.Errorf("decode version page token: %w", err)
	}
	var cursor s3VersionCursor
	if err := json.Unmarshal(data, &cursor); err != nil {
		return s3VersionCursor{}, fmt.Errorf("decode version page token: %w", err)
	}
	return cursor, nil
}

func closeObjectResponse(response objectResponse) {
	if response.Body != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		_ = response.Body.Close()
	}
}

// versioningAvailable reports the result of the startup capability probe. It
// remains false when the provider rejects or does not implement version listing.
func (s *storageInstance) versioningAvailable() bool {
	if s == nil {
		return false
	}
	s.versioningMu.RLock()
	defer s.versioningMu.RUnlock()
	return s.versioningChecked && s.versioningSupported
}

func (s *storageInstance) versioningProbeFinished() bool {
	if s == nil {
		return false
	}
	s.versioningMu.RLock()
	defer s.versioningMu.RUnlock()
	return s.versioningChecked
}

func (s *storageInstance) setVersioningAvailable(supported bool) {
	if s == nil {
		return
	}
	s.versioningMu.Lock()
	s.versioningSupported = supported
	s.versioningChecked = true
	s.versioningMu.Unlock()
}

// probeVersioning performs one bounded provider request. Unsupported, denied,
// and failed probes are non-fatal and simply keep version controls out of the UI.
func (s *storageInstance) probeVersioning(ctx context.Context) bool {
	if s == nil {
		return false
	}
	if _, ok := s.backend.(ObjectVersionStore); !ok {
		s.setVersioningAvailable(false)
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := s.ListObjectVersions(probeCtx, s.cfg.RootPrefix, "", 1)
	supported := err == nil
	s.setVersioningAvailable(supported)
	// The status of the bucket goes into /api/instances, so the bucket selector shows the versioning of each bucket.
	// A failure of this read hides the status only.
	if reader, ok := s.backend.(BucketVersioningReader); ok && supported {
		if status, err := reader.BucketVersioningStatus(probeCtx); err == nil {
			s.setVersioningStatus(status)
		}
	}
	return supported
}

// setVersioningStatus keeps the last status of GetBucketVersioning that the server read.
func (s *storageInstance) setVersioningStatus(status string) {
	s.versioningMu.Lock()
	s.versioningStatus, s.versioningStatusKnown = status, true
	s.versioningMu.Unlock()
}

// versioningState gives the versioning of the bucket for /api/instances: "enabled", "suspended", "off", or "" when
// the server has no status. A provider without versions is "off".
func (s *storageInstance) versioningState() string {
	if s == nil {
		return ""
	}
	s.versioningMu.RLock()
	defer s.versioningMu.RUnlock()
	switch {
	case s.versioningChecked && !s.versioningSupported:
		return "off"
	case !s.versioningStatusKnown:
		return ""
	case s.versioningStatus == "Enabled":
		return "enabled"
	case s.versioningStatus == "Suspended":
		return "suspended"
	default:
		return "off"
	}
}

// withoutVersionPreconditions removes If-Match and If-Unmodified-Since from the headers of a read that names a
// version. The bytes of a version never change, so the precondition adds no safety. A provider can also compare
// If-Match with the current object instead of the version: SeaweedFS then answers 412 for every older version.
// The caller still compares the ETag of the response with the ETag that it expects.
func withoutVersionPreconditions(headers http.Header) http.Header {
	if headers.Get("If-Match") == "" && headers.Get("If-Unmodified-Since") == "" {
		return headers
	}
	clean := headers.Clone()
	clean.Del("If-Match")
	clean.Del("If-Unmodified-Since")
	return clean
}

type objectVersionContextKey struct{}

func withObjectVersion(ctx context.Context, version string) context.Context {
	version = strings.TrimSpace(version)
	if version == "" {
		return ctx
	}
	return context.WithValue(ctx, objectVersionContextKey{}, version)
}

func objectVersionFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	version, _ := ctx.Value(objectVersionContextKey{}).(string)
	return strings.TrimSpace(version)
}
