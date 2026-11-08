package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	uploadStatusUploading = "uploading"
	uploadStatusCompleted = "completed"
	uploadStatusCanceled  = "canceled"

	minimumS3PartSize = int64(5 << 20)
	maximumS3PartSize = int64(5 << 30)
	maximumS3Parts    = 10000
	maximumObjectSize = int64(5) << 40
	gcsChunkAlignment = int64(256 << 10)

	// Upload session state is intentionally memory-only. Bounding the number of
	// simultaneously registered sessions prevents abandoned browser tabs from
	// growing the process indefinitely. A client-held resume token can recreate
	// a provider session after it has disappeared from memory.
	maxInMemoryUploadSessions = 4
)

// An upload session that makes no progress for this long is abandoned: its
// provider session is aborted and its slot is released. The client-held resume
// token can still re-register it later.
var (
	uploadIdleTTL        = 30 * time.Minute
	uploadReapInterval   = time.Minute
	uploadReapAbortLimit = 30 * time.Second
)

type s3MultipartAPI interface {
	InitiateMultipart(context.Context, string, string) (string, error)
	UploadPart(context.Context, string, string, int, io.Reader, int64) (string, error)
	CompleteMultipart(context.Context, string, string, []s3CompletedPart) error
	AbortMultipart(context.Context, string, string) error
}

type gcsResumableAPI interface {
	InitiateResumable(context.Context, string, int64, string) (string, error)
	UploadResumableChunk(context.Context, string, io.Reader, int64, int64, int64, string) (int64, bool, error)
	QueryResumable(context.Context, string, int64) (int64, bool, error)
	AbortResumable(context.Context, string) error
}

type uploadPartState struct {
	PartNumber int    `json:"part_number"`
	ETag       string `json:"etag"`
	Size       int64  `json:"size"`
	// ChecksumSHA256 is the base64 digest of the part, when the upload has the SHA-256 checksum.
	ChecksumSHA256 string `json:"checksum_sha256,omitempty"`
	// ChecksumStored is true when the provider answered with the same digest: it checked the bytes of the part.
	ChecksumStored bool `json:"checksum_stored,omitempty"`
}

type uploadSession struct {
	ID               string            `json:"id"`
	Instance         string            `json:"instance"`
	Key              string            `json:"key"`
	Provider         string            `json:"provider"`
	Status           string            `json:"status"`
	ContentType      string            `json:"content_type"`
	TotalSize        int64             `json:"total_size"`
	UploadedBytes    int64             `json:"uploaded_bytes"`
	ChunkSize        int64             `json:"chunk_size"`
	ProviderUploadID string            `json:"provider_upload_id,omitempty"`
	SessionURL       string            `json:"session_url,omitempty"`
	Parts            []uploadPartState `json:"parts,omitempty"`
	Error            string            `json:"error,omitempty"`
	CreatedAt        time.Time         `json:"created_at"`
	UpdatedAt        time.Time         `json:"updated_at"`
	CompletedAt      *time.Time        `json:"completed_at,omitempty"`
	Verified         bool              `json:"verified,omitempty"`
	// Checksum is "sha256" when each chunk of the upload carries its SHA-256 digest, and empty otherwise.
	Checksum string `json:"checksum,omitempty"`
	// ChecksumVerified is true when the provider confirmed the digest of every part.
	ChecksumVerified bool   `json:"checksum_verified,omitempty"`
	ResumeToken      string `json:"-"`
	// lastActivity moves only on creation and real progress, so status polls
	// cannot keep an abandoned session alive.
	lastActivity time.Time
}

type publicUpload struct {
	ID               string     `json:"id"`
	Instance         string     `json:"instance"`
	Key              string     `json:"key"`
	Provider         string     `json:"provider"`
	Status           string     `json:"status"`
	ContentType      string     `json:"contentType"`
	TotalSize        int64      `json:"totalSize"`
	UploadedBytes    int64      `json:"uploadedBytes"`
	NextOffset       int64      `json:"nextOffset"`
	ChunkSize        int64      `json:"chunkSize"`
	PartCount        int        `json:"partCount,omitempty"`
	Error            string     `json:"error,omitempty"`
	CreatedAt        time.Time  `json:"createdAt"`
	UpdatedAt        time.Time  `json:"updatedAt"`
	CompletedAt      *time.Time `json:"completedAt,omitempty"`
	Verified         bool       `json:"verified,omitempty"`
	Checksum         string     `json:"checksum,omitempty"`
	ChecksumVerified bool       `json:"checksumVerified,omitempty"`
	ResumeToken      string     `json:"resumeToken,omitempty"`
}

func (u uploadSession) public() publicUpload {
	return publicUpload{
		ID:               u.ID,
		Instance:         u.Instance,
		Key:              u.Key,
		Provider:         u.Provider,
		Status:           u.Status,
		ContentType:      u.ContentType,
		TotalSize:        u.TotalSize,
		UploadedBytes:    u.UploadedBytes,
		NextOffset:       u.UploadedBytes,
		ChunkSize:        u.ChunkSize,
		PartCount:        len(u.Parts),
		Error:            u.Error,
		CreatedAt:        u.CreatedAt,
		UpdatedAt:        u.UpdatedAt,
		CompletedAt:      cloneTimePointer(u.CompletedAt),
		Verified:         u.Verified,
		Checksum:         u.Checksum,
		ChecksumVerified: u.ChecksumVerified,
		ResumeToken:      u.ResumeToken,
	}
}

type uploadManager struct {
	app     *application
	mu      sync.RWMutex
	uploads map[string]*uploadSession
	lockMu  sync.Mutex
	uplocks map[string]*sync.Mutex
	ctx     context.Context
	stop    context.CancelFunc
	wg      sync.WaitGroup
}

func newUploadManager(app *application) (*uploadManager, error) {
	if app == nil {
		return nil, fmt.Errorf("application is nil")
	}
	// The reaper stops with the job manager when the application closes.
	parent := context.Background()
	if app.jobs != nil {
		parent = app.jobs.ctx
	}
	ctx, cancel := context.WithCancel(parent)
	manager := &uploadManager{app: app, uploads: make(map[string]*uploadSession), uplocks: make(map[string]*sync.Mutex), ctx: ctx, stop: cancel}
	manager.wg.Add(1)
	go manager.reaper()
	return manager, nil
}

func (m *uploadManager) close() {
	if m == nil {
		return
	}
	m.stop()
	m.wg.Wait()
}

func (m *uploadManager) reaper() {
	defer m.wg.Done()
	ticker := time.NewTicker(uploadReapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case now := <-ticker.C:
			m.reapIdle(now.UTC())
		}
	}
}

// reapIdle aborts and forgets sessions with no progress for uploadIdleTTL.
// Sessions currently handling a request hold their upload lock and are skipped,
// so a slow but live chunk transfer is never reaped.
func (m *uploadManager) reapIdle(now time.Time) int {
	m.mu.RLock()
	var idle []string
	for id, upload := range m.uploads {
		if now.Sub(upload.lastActivity) > uploadIdleTTL {
			idle = append(idle, id)
		}
	}
	m.mu.RUnlock()
	reaped := 0
	for _, id := range idle {
		lock := m.uploadLock(id)
		if !lock.TryLock() {
			continue
		}
		upload, ok := m.get(id)
		if ok && now.Sub(upload.lastActivity) > uploadIdleTTL {
			if upload.Status == uploadStatusUploading {
				if instance := m.app.instances[upload.Instance]; instance != nil {
					abortContext, cancel := context.WithTimeout(m.ctx, uploadReapAbortLimit)
					_ = m.abortProvider(abortContext, instance, upload)
					cancel()
				}
			}
			m.remove(id)
			reaped++
		}
		lock.Unlock()
	}
	return reaped
}

func (m *uploadManager) uploadLock(id string) *sync.Mutex {
	m.lockMu.Lock()
	defer m.lockMu.Unlock()
	lock := m.uplocks[id]
	if lock == nil {
		lock = &sync.Mutex{}
		m.uplocks[id] = lock
	}
	return lock
}

func (m *uploadManager) get(id string) (uploadSession, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	upload, ok := m.uploads[id]
	if !ok {
		return uploadSession{}, false
	}
	return cloneUploadSession(*upload), true
}

func cloneUploadSession(upload uploadSession) uploadSession {
	upload.Parts = append([]uploadPartState(nil), upload.Parts...)
	upload.CompletedAt = cloneTimePointer(upload.CompletedAt)
	return upload
}

func cloneTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func (m *uploadManager) put(upload uploadSession) error {
	if strings.TrimSpace(upload.ID) == "" {
		return fmt.Errorf("upload id is empty")
	}
	copyUpload := cloneUploadSession(upload)
	m.mu.Lock()
	existing, exists := m.uploads[upload.ID]
	if !exists && len(m.uploads) >= maxInMemoryUploadSessions {
		m.mu.Unlock()
		return uploadCapacityError()
	}
	if exists && existing.UploadedBytes == copyUpload.UploadedBytes && existing.Status == copyUpload.Status {
		copyUpload.lastActivity = existing.lastActivity
	} else {
		copyUpload.lastActivity = time.Now().UTC()
	}
	m.uploads[upload.ID] = &copyUpload
	m.mu.Unlock()
	return nil
}

func (m *uploadManager) hasCapacityFor(id string) bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, exists := m.uploads[id]; exists {
		return true
	}
	return len(m.uploads) < maxInMemoryUploadSessions
}

func uploadCapacityError() error {
	return apiError{
		Status:  http.StatusTooManyRequests,
		Code:    "upload_session_limit_reached",
		Message: fmt.Sprintf("the server is already tracking %d active upload sessions", maxInMemoryUploadSessions),
	}
}

func (m *uploadManager) remove(id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	m.mu.Lock()
	delete(m.uploads, id)
	m.mu.Unlock()
	m.lockMu.Lock()
	delete(m.uplocks, id)
	m.lockMu.Unlock()
}

func uploadChunkSize(provider string, total int64) (int64, error) {
	if total < 0 || total > maximumObjectSize {
		return 0, fmt.Errorf("object size must be between 0 and %d bytes", maximumObjectSize)
	}
	if provider == "s3" {
		chunk := int64(16 << 20)
		minimumForPartLimit := (total + maximumS3Parts - 1) / maximumS3Parts
		if minimumForPartLimit > chunk {
			chunk = minimumForPartLimit
		}
		const mebibyte = int64(1 << 20)
		chunk = ((chunk + mebibyte - 1) / mebibyte) * mebibyte
		if chunk < minimumS3PartSize {
			chunk = minimumS3PartSize
		}
		if chunk > maximumS3PartSize {
			return 0, fmt.Errorf("object is too large for at most %d S3 parts", maximumS3Parts)
		}
		return chunk, nil
	}
	chunk := int64(8 << 20)
	chunk = ((chunk + gcsChunkAlignment - 1) / gcsChunkAlignment) * gcsChunkAlignment
	return chunk, nil
}

type createUploadRequest struct {
	Instance    string `json:"instance"`
	Key         string `json:"key"`
	Size        int64  `json:"size"`
	ContentType string `json:"contentType"`
	// Checksum is "" or "sha256". With "sha256" the client sends the SHA-256 digest of each chunk in X-Checksum-Sha256.
	Checksum string `json:"checksum"`
	// Tags are the object tags of the new object. Only an S3 provider that supports tags keeps them.
	Tags []objectTag `json:"tags"`
}

const (
	uploadChecksumSHA256 = "sha256"
	checksumHeader       = "X-Checksum-Sha256"
)

// parseChecksumHeader reads the base64 SHA-256 digest of a request body. An empty header gives an empty value.
func parseChecksumHeader(header http.Header) (string, error) {
	value := strings.TrimSpace(header.Get(checksumHeader))
	if value == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(raw) != sha256.Size {
		return "", apiError{Status: http.StatusBadRequest, Code: "invalid_checksum", Message: checksumHeader + " must be the base64 encoding of a 32-byte SHA-256 digest"}
	}
	return value, nil
}

// compositeChecksum is the SHA-256 checksum that S3 reports for a multipart object: the SHA-256 of the part digests
// (base64), and the number of parts.
func compositeChecksum(parts []uploadPartState) (string, int, error) {
	hash := sha256.New()
	for _, part := range parts {
		raw, err := base64.StdEncoding.DecodeString(part.ChecksumSHA256)
		if err != nil || len(raw) != sha256.Size {
			return "", 0, fmt.Errorf("part %d has no valid SHA-256 digest", part.PartNumber)
		}
		hash.Write(raw)
	}
	return base64.StdEncoding.EncodeToString(hash.Sum(nil)), len(parts), nil
}

// sameCompositeChecksum compares the checksum that the provider reports with the one of the parts. S3 writes it as
// "<digest>-<number of parts>"; other providers write the digest only.
func sameCompositeChecksum(reported, digest string, count int) bool {
	reported = strings.TrimSpace(reported)
	if dash := strings.LastIndexByte(reported, '-'); dash >= 0 {
		if reported[dash+1:] != strconv.Itoa(count) {
			return false
		}
		reported = reported[:dash]
	}
	return reported == digest
}

// accessCheck is the access check of a request that has a key: an upload request or a SQLite request. It gets the
// instance and the key.
type accessCheck func(instance *storageInstance, key string) error

func (m *uploadManager) create(ctx context.Context, request createUploadRequest) (uploadSession, error) {
	return m.createChecked(ctx, request, nil)
}

// createChecked creates an upload session. It calls check after the validation of the request and before the first
// call to the storage. A nil check is a request without access rule.
func (m *uploadManager) createChecked(ctx context.Context, request createUploadRequest, check accessCheck) (uploadSession, error) {
	if !m.hasCapacityFor("") {
		return uploadSession{}, uploadCapacityError()
	}
	instance, ok := m.app.instances[strings.TrimSpace(request.Instance)]
	if !ok {
		return uploadSession{}, apiError{Status: http.StatusNotFound, Code: "unknown_instance", Message: "storage instance was not found"}
	}
	if err := requirePermission(instance, permissionWrite); err != nil {
		return uploadSession{}, err
	}
	key := cleanRelativeKey(request.Key)
	if key == "" {
		return uploadSession{}, apiError{Status: http.StatusBadRequest, Code: "invalid_key", Message: "object key is required"}
	}
	chunkSize, err := uploadChunkSize(instance.cfg.Provider, request.Size)
	if err != nil {
		return uploadSession{}, apiError{Status: http.StatusBadRequest, Code: "invalid_size", Message: err.Error()}
	}
	contentType := strings.TrimSpace(request.ContentType)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if err := validateTags(request.Tags); err != nil {
		return uploadSession{}, err
	}
	if err := instance.requireTags(request.Tags); err != nil {
		return uploadSession{}, err
	}
	ctx = withUploadTags(ctx, request.Tags)
	if request.Checksum != "" && request.Checksum != uploadChecksumSHA256 {
		return uploadSession{}, apiError{Status: http.StatusBadRequest, Code: "invalid_checksum", Message: `checksum must be "" or "sha256"`}
	}
	if check != nil {
		if err := check(instance, key); err != nil {
			return uploadSession{}, err
		}
	}
	now := time.Now().UTC()
	upload := uploadSession{
		Instance:      instance.cfg.ID,
		Key:           key,
		Provider:      instance.cfg.Provider,
		Status:        uploadStatusUploading,
		ContentType:   contentType,
		TotalSize:     request.Size,
		ChunkSize:     chunkSize,
		CreatedAt:     now,
		UpdatedAt:     now,
		UploadedBytes: 0,
	}
	fullKey := instance.fullKey(key)
	if request.Size == 0 {
		if err := instance.Put(ctx, fullKey, bytes.NewReader(nil), 0, contentType); err != nil {
			return uploadSession{}, err
		}
		id, err := newStateID("upl")
		if err != nil {
			return uploadSession{}, err
		}
		upload.ID = id
		upload.Status = uploadStatusCompleted
		upload.CompletedAt = &now
		upload.Verified = true
		return upload, nil
	}

	switch instance.cfg.Provider {
	case "s3":
		initiate := ctx
		if request.Checksum == uploadChecksumSHA256 {
			initiate = withChecksumAlgorithm(ctx)
			upload.Checksum = uploadChecksumSHA256
		}
		uploadID, err := instance.InitiateMultipart(initiate, fullKey, contentType)
		if err != nil {
			return uploadSession{}, err
		}
		upload.ProviderUploadID = uploadID
	case "gcs", providerVolume:
		sessionURL, err := instance.InitiateResumable(ctx, fullKey, request.Size, contentType)
		if err != nil {
			return uploadSession{}, err
		}
		upload.SessionURL = sessionURL
	default:
		return uploadSession{}, fmt.Errorf("provider %q does not support resumable uploads", instance.cfg.Provider)
	}
	resumeToken, err := m.app.sealUploadResumeToken(instance, upload)
	if err != nil {
		_ = m.abortProvider(context.Background(), instance, upload)
		return uploadSession{}, err
	}
	upload.ResumeToken = resumeToken
	upload.ID = uploadIDFromResumeToken(resumeToken)
	if err := m.put(upload); err != nil {
		// Best effort cleanup when the in-memory session cannot be registered.
		_ = m.abortProvider(context.Background(), instance, upload)
		return uploadSession{}, err
	}
	return upload, nil
}

type resumeUploadRequest struct {
	ResumeToken string `json:"resumeToken"`
}

func (m *uploadManager) resume(ctx context.Context, token string) (uploadSession, error) {
	return m.resumeChecked(ctx, token, nil)
}

// resumeChecked gives the state of an upload from its resume token. It calls check before the first read of the
// session and of the storage. The instance and the key come from the sealed token, never from the client.
func (m *uploadManager) resumeChecked(ctx context.Context, token string, check accessCheck) (uploadSession, error) {
	descriptor, instance, err := m.app.openUploadResumeToken(token)
	if err != nil {
		return uploadSession{}, err
	}
	if err := requirePermission(instance, permissionWrite); err != nil {
		return uploadSession{}, err
	}
	if check != nil {
		if err := check(instance, descriptor.Key); err != nil {
			return uploadSession{}, err
		}
	}
	id := uploadIDFromResumeToken(token)
	if existing, ok := m.get(id); ok {
		return m.status(ctx, existing.ID)
	}
	if !m.hasCapacityFor(id) {
		return uploadSession{}, uploadCapacityError()
	}
	now := time.Now().UTC()
	upload := uploadSession{
		ID: id, Instance: descriptor.Instance, Key: descriptor.Key, Provider: descriptor.Provider,
		Status: uploadStatusUploading, ContentType: descriptor.ContentType, TotalSize: descriptor.TotalSize,
		ChunkSize: descriptor.ChunkSize, ProviderUploadID: descriptor.ProviderUploadID, SessionURL: descriptor.SessionURL,
		Checksum:  descriptor.Checksum,
		CreatedAt: time.Unix(descriptor.IssuedAt, 0).UTC(), UpdatedAt: now, ResumeToken: token,
	}
	if upload.ChunkSize <= 0 {
		upload.ChunkSize, err = uploadChunkSize(upload.Provider, upload.TotalSize)
		if err != nil {
			return uploadSession{}, err
		}
	}
	if err := m.put(upload); err != nil {
		return uploadSession{}, err
	}
	return m.status(ctx, id)
}

func (m *uploadManager) abortProvider(ctx context.Context, instance *storageInstance, upload uploadSession) error {
	switch upload.Provider {
	case "s3":
		if upload.ProviderUploadID == "" {
			return nil
		}
		return instance.AbortMultipart(ctx, instance.fullKey(upload.Key), upload.ProviderUploadID)
	case "gcs", providerVolume:
		if upload.SessionURL == "" {
			return nil
		}
		return instance.AbortResumable(ctx, upload.SessionURL)
	default:
		return nil
	}
}

func parseContentRange(value string) (start, end, total int64, err error) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "bytes ") {
		return 0, 0, 0, fmt.Errorf("Content-Range must use bytes start-end/total")
	}
	parts := strings.SplitN(strings.TrimPrefix(value, "bytes "), "/", 2)
	if len(parts) != 2 {
		return 0, 0, 0, fmt.Errorf("Content-Range must use bytes start-end/total")
	}
	rangeParts := strings.SplitN(parts[0], "-", 2)
	if len(rangeParts) != 2 {
		return 0, 0, 0, fmt.Errorf("Content-Range must use bytes start-end/total")
	}
	start, err = strconv.ParseInt(rangeParts[0], 10, 64)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("invalid Content-Range start")
	}
	end, err = strconv.ParseInt(rangeParts[1], 10, 64)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("invalid Content-Range end")
	}
	total, err = strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("invalid Content-Range total")
	}
	if start < 0 || end < start || total <= end {
		return 0, 0, 0, fmt.Errorf("Content-Range is outside the object size")
	}
	return start, end, total, nil
}

func (m *uploadManager) uploadChunk(ctx context.Context, id string, request *http.Request) (uploadSession, error) {
	lock := m.uploadLock(id)
	lock.Lock()
	defer lock.Unlock()

	upload, ok := m.get(id)
	if !ok {
		return uploadSession{}, apiError{Status: http.StatusNotFound, Code: "upload_not_found", Message: "upload session was not found"}
	}
	if upload.Status != uploadStatusUploading {
		return uploadSession{}, apiError{Status: http.StatusConflict, Code: "upload_not_active", Message: "upload session is not active"}
	}
	instance := m.app.instances[upload.Instance]
	if err := requirePermission(instance, permissionWrite); err != nil {
		return uploadSession{}, err
	}
	start, end, total, err := parseContentRange(request.Header.Get("Content-Range"))
	if err != nil {
		return uploadSession{}, apiError{Status: http.StatusBadRequest, Code: "invalid_content_range", Message: err.Error()}
	}
	if total != upload.TotalSize {
		return uploadSession{}, apiError{Status: http.StatusConflict, Code: "size_mismatch", Message: "Content-Range total does not match the upload session"}
	}
	size := end - start + 1
	if request.ContentLength < 0 {
		return uploadSession{}, apiError{Status: http.StatusLengthRequired, Code: "content_length_required", Message: "Content-Length is required for upload chunks"}
	}
	if request.ContentLength != size {
		return uploadSession{}, apiError{Status: http.StatusBadRequest, Code: "content_length_mismatch", Message: "Content-Length does not match Content-Range"}
	}

	if start != upload.UploadedBytes {
		switch upload.Provider {
		case "gcs", providerVolume:
			if err := m.synchronizeGCSOffset(ctx, instance, &upload); err != nil {
				return uploadSession{}, err
			}
		case "s3":
			if err := m.synchronizeS3Parts(ctx, instance, &upload); err != nil {
				return uploadSession{}, err
			}
		}
	}
	if start != upload.UploadedBytes {
		return uploadSession{}, apiError{
			Status:  http.StatusConflict,
			Code:    "offset_mismatch",
			Message: fmt.Sprintf("upload must resume at byte %d", upload.UploadedBytes),
		}
	}

	body := io.LimitReader(request.Body, size)
	upload.Error = ""
	switch upload.Provider {
	case "s3":
		if size > maximumS3PartSize {
			return uploadSession{}, apiError{Status: http.StatusBadRequest, Code: "part_too_large", Message: "S3 upload parts cannot exceed 5 GiB"}
		}
		if end+1 < total && size < minimumS3PartSize {
			return uploadSession{}, apiError{Status: http.StatusBadRequest, Code: "part_too_small", Message: "non-final S3 upload parts must be at least 5 MiB"}
		}
		partNumber := len(upload.Parts) + 1
		if partNumber > maximumS3Parts {
			return uploadSession{}, apiError{Status: http.StatusBadRequest, Code: "too_many_parts", Message: "S3 multipart uploads cannot exceed 10000 parts"}
		}
		partCtx := ctx
		var transfer *checksumTransfer
		if upload.Checksum == uploadChecksumSHA256 {
			digest, err := parseChecksumHeader(request.Header)
			if err != nil {
				return uploadSession{}, err
			}
			if digest == "" {
				return uploadSession{}, apiError{Status: http.StatusBadRequest, Code: "checksum_required", Message: checksumHeader + " is required for each chunk of this upload"}
			}
			transfer = &checksumTransfer{SHA256: digest}
			partCtx = withChecksumTransfer(ctx, transfer)
		}
		etag, err := instance.UploadPart(partCtx, instance.fullKey(upload.Key), upload.ProviderUploadID, partNumber, body, size)
		if err != nil {
			upload.Error = publicStorageError(err)
			upload.UpdatedAt = time.Now().UTC()
			_ = m.put(upload)
			return uploadSession{}, err
		}
		part := uploadPartState{PartNumber: partNumber, ETag: etag, Size: size}
		if transfer != nil {
			part.ChecksumSHA256 = transfer.SHA256
			part.ChecksumStored = transfer.Echoed == transfer.SHA256
		}
		upload.Parts = append(upload.Parts, part)
		upload.UploadedBytes = end + 1
		upload.UpdatedAt = time.Now().UTC()
		if err := m.put(upload); err != nil {
			return uploadSession{}, err
		}
		if upload.UploadedBytes == upload.TotalSize {
			if err := m.completeS3(ctx, instance, &upload); err != nil {
				return uploadSession{}, err
			}
		}
	case "gcs", providerVolume:
		next, complete, err := instance.UploadResumableChunk(ctx, upload.SessionURL, body, start, size, total, upload.ContentType)
		if err != nil {
			upload.Error = publicStorageError(err)
			upload.UpdatedAt = time.Now().UTC()
			_ = m.put(upload)
			return uploadSession{}, err
		}
		if next < start || next > total {
			return uploadSession{}, fmt.Errorf("gcs resumable upload returned an invalid offset")
		}
		upload.UploadedBytes = next
		upload.UpdatedAt = time.Now().UTC()
		if complete {
			now := upload.UpdatedAt
			upload.Status = uploadStatusCompleted
			upload.CompletedAt = &now
			if err := m.verifyCompletedObject(ctx, instance, &upload); err != nil {
				return uploadSession{}, err
			}
		}
		if err := m.put(upload); err != nil {
			return uploadSession{}, err
		}
	default:
		return uploadSession{}, fmt.Errorf("unsupported upload provider %q", upload.Provider)
	}
	return upload, nil
}

func (m *uploadManager) completeS3(ctx context.Context, instance *storageInstance, upload *uploadSession) error {
	parts := make([]s3CompletedPart, 0, len(upload.Parts))
	for _, part := range upload.Parts {
		parts = append(parts, s3CompletedPart{PartNumber: part.PartNumber, ETag: part.ETag, ChecksumSHA256: part.ChecksumSHA256})
	}
	if err := instance.CompleteMultipart(ctx, instance.fullKey(upload.Key), upload.ProviderUploadID, parts); err != nil {
		upload.Error = publicStorageError(err)
		upload.UpdatedAt = time.Now().UTC()
		_ = m.put(*upload)
		return err
	}
	now := time.Now().UTC()
	upload.Status = uploadStatusCompleted
	upload.Error = ""
	upload.UpdatedAt = now
	upload.CompletedAt = &now
	if err := m.verifyCompletedObject(ctx, instance, upload); err != nil {
		return err
	}
	return m.put(*upload)
}

func (m *uploadManager) synchronizeS3Parts(ctx context.Context, instance *storageInstance, upload *uploadSession) error {
	if upload == nil || upload.ProviderUploadID == "" {
		return nil
	}
	parts, err := instance.ListMultipartParts(ctx, instance.fullKey(upload.Key), upload.ProviderUploadID)
	if err != nil {
		return err
	}
	state := make([]uploadPartState, 0, len(parts))
	var uploaded int64
	known := make(map[int]uploadPartState, len(upload.Parts))
	for _, part := range upload.Parts {
		known[part.PartNumber] = part
	}
	for index, part := range parts {
		if part.PartNumber != index+1 || part.Size <= 0 || strings.TrimSpace(part.ETag) == "" {
			return fmt.Errorf("s3 multipart upload contains a non-contiguous or invalid part sequence")
		}
		item := uploadPartState{PartNumber: part.PartNumber, ETag: part.ETag, Size: part.Size}
		if upload.Checksum == uploadChecksumSHA256 {
			if local, ok := known[part.PartNumber]; ok && local.ETag == part.ETag && local.ChecksumSHA256 != "" {
				// The server sent this part and the provider confirmed its digest: keep what it knows.
				item.ChecksumSHA256 = local.ChecksumSHA256
				item.ChecksumStored = local.ChecksumStored
			} else {
				// A provider such as Garage lists the digest of each stored part: it is what it checked.
				item.ChecksumSHA256 = part.ChecksumSHA256
				item.ChecksumStored = part.ChecksumSHA256 != ""
			}
		}
		state = append(state, item)
		uploaded += part.Size
	}
	if uploaded > upload.TotalSize {
		return fmt.Errorf("s3 multipart upload contains more bytes than the local upload session")
	}
	upload.Parts = state
	upload.UploadedBytes = uploaded
	upload.UpdatedAt = time.Now().UTC()
	return m.put(*upload)
}

func (m *uploadManager) verifyCompletedObject(ctx context.Context, instance *storageInstance, upload *uploadSession) error {
	if requirePermission(instance, permissionRead) != nil {
		// Verification reads the object back. On a read-denied instance it would
		// reveal whether and how large a key is, so leave the session unverified.
		upload.Verified = false
		return nil
	}
	response, err := instance.Head(ctx, instance.fullKey(upload.Key))
	if err != nil {
		upload.Error = "the upload completed but the final object could not be verified"
		upload.Verified = false
		upload.UpdatedAt = time.Now().UTC()
		_ = m.put(*upload)
		return err
	}
	defer closeObjectResponse(response)
	size, err := strconv.ParseInt(strings.TrimSpace(response.Header.Get("Content-Length")), 10, 64)
	if err != nil || size != upload.TotalSize {
		upload.Error = fmt.Sprintf("the upload completed but the provider reports %d bytes instead of %d", size, upload.TotalSize)
		upload.Verified = false
		upload.UpdatedAt = time.Now().UTC()
		_ = m.put(*upload)
		return fmt.Errorf("completed upload size verification failed")
	}
	if upload.Checksum == uploadChecksumSHA256 {
		if err := verifyUploadChecksum(response.Header, upload); err != nil {
			upload.Error = err.Error()
			upload.Verified = false
			upload.ChecksumVerified = false
			upload.UpdatedAt = time.Now().UTC()
			_ = m.put(*upload)
			return err
		}
	}
	upload.Verified = true
	upload.Error = ""
	return nil
}

// verifyUploadChecksum sets ChecksumVerified when the provider stored the digest of every part. When the provider also
// reports the checksum of the whole object, it must be the one that the digests of the parts give.
func verifyUploadChecksum(header http.Header, upload *uploadSession) error {
	upload.ChecksumVerified = false
	if len(upload.Parts) == 0 {
		return nil
	}
	for _, part := range upload.Parts {
		if !part.ChecksumStored {
			return nil
		}
	}
	digest, count, err := compositeChecksum(upload.Parts)
	if err != nil {
		return nil
	}
	if reported := strings.TrimSpace(header.Get("x-amz-checksum-sha256")); reported != "" && !sameCompositeChecksum(reported, digest, count) {
		return fmt.Errorf("the upload completed but the SHA-256 checksum of the provider (%s) is not the one of the uploaded parts (%s-%d)", reported, digest, count)
	}
	upload.ChecksumVerified = true
	return nil
}

func (m *uploadManager) synchronizeGCSOffset(ctx context.Context, instance *storageInstance, upload *uploadSession) error {
	next, complete, err := instance.QueryResumable(ctx, upload.SessionURL, upload.TotalSize)
	if err != nil {
		return err
	}
	upload.UploadedBytes = next
	upload.UpdatedAt = time.Now().UTC()
	if complete {
		now := upload.UpdatedAt
		upload.Status = uploadStatusCompleted
		upload.CompletedAt = &now
		if err := m.verifyCompletedObject(ctx, instance, upload); err != nil {
			return err
		}
	}
	return m.put(*upload)
}

func (m *uploadManager) status(ctx context.Context, id string) (uploadSession, error) {
	lock := m.uploadLock(id)
	lock.Lock()
	defer lock.Unlock()
	upload, ok := m.get(id)
	if !ok {
		return uploadSession{}, apiError{Status: http.StatusNotFound, Code: "upload_not_found", Message: "upload session was not found"}
	}
	instance := m.app.instances[upload.Instance]
	if instance == nil {
		return uploadSession{}, apiError{Status: http.StatusNotFound, Code: "unknown_instance", Message: "storage instance was not found"}
	}
	if upload.Status == uploadStatusUploading {
		switch upload.Provider {
		case "gcs", providerVolume:
			if err := m.synchronizeGCSOffset(ctx, instance, &upload); err != nil {
				// Preserve the last in-memory offset when the provider is temporarily unavailable.
				upload.Error = publicStorageError(err)
				upload.UpdatedAt = time.Now().UTC()
				_ = m.put(upload)
			}
		case "s3":
			err := m.synchronizeS3Parts(ctx, instance, &upload)
			if err != nil {
				// The provider may remove the multipart session immediately after completion. Verify the final object before marking the session complete.
				if isStorageNotFound(err) && requirePermission(instance, permissionRead) != nil {
					// A vanished multipart session is only proof of completion when the
					// object can be checked. Without read access, answer identically
					// whether or not the key exists.
					upload.Error = "the upload session is no longer available"
					upload.UpdatedAt = time.Now().UTC()
					_ = m.put(upload)
				} else if isStorageNotFound(err) {
					now := time.Now().UTC()
					upload.Status = uploadStatusCompleted
					upload.UploadedBytes = upload.TotalSize
					upload.CompletedAt = &now
					upload.UpdatedAt = now
					if verifyErr := m.verifyCompletedObject(ctx, instance, &upload); verifyErr == nil {
						_ = m.put(upload)
					} else {
						upload.Status = uploadStatusUploading
						upload.UploadedBytes = 0
						upload.CompletedAt = nil
						upload.Error = publicStorageError(verifyErr)
						_ = m.put(upload)
					}
				} else {
					upload.Error = publicStorageError(err)
					upload.UpdatedAt = time.Now().UTC()
					_ = m.put(upload)
				}
			} else if upload.UploadedBytes == upload.TotalSize && len(upload.Parts) > 0 {
				if err := m.completeS3(ctx, instance, &upload); err != nil {
					upload.Error = publicStorageError(err)
					_ = m.put(upload)
				}
			}
		}
	}
	if upload.Status == uploadStatusCompleted && !upload.Verified {
		if err := m.verifyCompletedObject(ctx, instance, &upload); err != nil {
			upload.Error = publicStorageError(err)
		} else {
			upload.Error = ""
		}
		upload.UpdatedAt = time.Now().UTC()
		_ = m.put(upload)
	}
	return upload, nil
}

func isStorageNotFound(err error) bool {
	var upstream *upstreamError
	return errors.As(err, &upstream) && upstream.StatusCode == http.StatusNotFound
}

func (m *uploadManager) cancel(ctx context.Context, id string) (uploadSession, error) {
	lock := m.uploadLock(id)
	lock.Lock()
	defer lock.Unlock()
	upload, ok := m.get(id)
	if !ok {
		return uploadSession{}, apiError{Status: http.StatusNotFound, Code: "upload_not_found", Message: "upload session was not found"}
	}
	if upload.Status == uploadStatusCompleted || upload.Status == uploadStatusCanceled {
		return upload, nil
	}
	instance := m.app.instances[upload.Instance]
	if err := requirePermission(instance, permissionWrite); err != nil {
		return uploadSession{}, err
	}
	if err := m.abortProvider(ctx, instance, upload); err != nil {
		return uploadSession{}, err
	}
	now := time.Now().UTC()
	upload.Status = uploadStatusCanceled
	upload.Error = ""
	upload.UpdatedAt = now
	if err := m.put(upload); err != nil {
		return uploadSession{}, err
	}
	return upload, nil
}

func newStateID(prefix string) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate state id: %w", err)
	}
	return prefix + "_" + hex.EncodeToString(random[:]), nil
}

func uploadResumeTokenFromRequest(r *http.Request) string {
	if r == nil {
		return ""
	}
	return strings.TrimSpace(r.Header.Get("X-Stowr-Resume-Token"))
}

func (a *application) writeUploadResponse(w http.ResponseWriter, status int, upload uploadSession) {
	public := upload.public()
	if a != nil && a.uploads != nil && (upload.Status == uploadStatusCompleted || upload.Status == uploadStatusCanceled) {
		a.uploads.remove(upload.ID)
	}
	writeJSON(w, status, public)
}

func (a *application) handleUploads(w http.ResponseWriter, r *http.Request) {
	if a.uploads == nil {
		writeAPIError(w, fmt.Errorf("upload manager is not initialized"))
		return
	}
	if r.URL.Path == "/api/uploads/resume" {
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		var request resumeUploadRequest
		if err := decodeJSONBody(r, &request); err != nil {
			writeAPIError(w, err)
			return
		}
		upload, err := a.uploads.resumeChecked(r.Context(), request.ResumeToken, a.uploadAuthorizer(r, true))
		if err != nil {
			writeAPIError(w, err)
			return
		}
		a.writeUploadResponse(w, http.StatusOK, upload)
		return
	}
	if r.URL.Path != "/api/uploads" && r.URL.Path != "/api/uploads/" {
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodPost:
		var request createUploadRequest
		if err := decodeJSONBody(r, &request); err != nil {
			writeAPIError(w, err)
			return
		}
		if request.Instance == "" {
			request.Instance = r.URL.Query().Get("instance")
		}
		upload, err := a.uploads.createChecked(r.Context(), request, a.uploadAuthorizer(r, false))
		if err != nil {
			writeAPIError(w, err)
			return
		}
		a.writeUploadResponse(w, http.StatusCreated, upload)
	case http.MethodGet:
		token := uploadResumeTokenFromRequest(r)
		if token == "" {
			writeAPIError(w, apiError{Status: http.StatusBadRequest, Code: "resume_token_required", Message: "upload resume token is required"})
			return
		}
		upload, err := a.uploads.resumeChecked(r.Context(), token, a.uploadAuthorizer(r, true))
		if err != nil {
			writeAPIError(w, err)
			return
		}
		a.writeUploadResponse(w, http.StatusOK, upload)
	case http.MethodPut:
		token := uploadResumeTokenFromRequest(r)
		if token == "" {
			writeAPIError(w, apiError{Status: http.StatusBadRequest, Code: "resume_token_required", Message: "upload resume token is required"})
			return
		}
		upload, err := a.uploads.resumeChecked(r.Context(), token, a.uploadAuthorizer(r, true))
		if err != nil {
			writeAPIError(w, err)
			return
		}
		upload, err = a.uploads.uploadChunk(r.Context(), upload.ID, r)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		a.writeUploadResponse(w, http.StatusOK, upload)
	case http.MethodDelete:
		token := uploadResumeTokenFromRequest(r)
		if token == "" {
			writeAPIError(w, apiError{Status: http.StatusBadRequest, Code: "resume_token_required", Message: "upload resume token is required"})
			return
		}
		upload, err := a.uploads.resumeChecked(r.Context(), token, a.uploadAuthorizer(r, true))
		if err != nil {
			writeAPIError(w, err)
			return
		}
		upload, err = a.uploads.cancel(r.Context(), upload.ID)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		a.writeUploadResponse(w, http.StatusOK, upload)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete)
	}
}
