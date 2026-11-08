package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
	"unicode/utf8"
)

func (s *storageInstance) acquire(ctx context.Context) (func(), error) {
	if s == nil {
		return nil, errors.New("storage instance is nil")
	}
	if gateExempt(ctx) {
		// The caller already holds a slot for this operation.
		return func() {}, nil
	}
	var global chan struct{}
	if s.concurrency != nil {
		global = s.concurrency.global
	}
	return acquireGate(ctx, global, s.gate)
}

func (s *storageInstance) beginStorageRequest(ctx context.Context) (func(), error) {
	if budget := budgetFromContext(ctx); budget != nil {
		if err := budget.consumeRequest(); err != nil {
			return nil, err
		}
	}
	return s.acquire(ctx)
}

func (s *storageInstance) observePermission(permission string, err error) {
	if s == nil || permission == "" {
		return
	}
	if s.forceReadOnly && permission != permissionRead {
		return
	}
	if s.cfg.PermissionsDefined && !containsString(s.cfg.Permissions, permission) {
		return
	}

	var next capabilityState
	if err == nil {
		next = allowedCapability("observed", "a real provider operation succeeded during this process", true)
	} else {
		var upstream *upstreamError
		if !errors.As(err, &upstream) || (upstream.StatusCode != http.StatusUnauthorized && upstream.StatusCode != http.StatusForbidden) {
			return
		}
		next = deniedCapability("provider", "the storage provider denied a real operation during this process", true)
		// One object's 403 is not an instance-wide verdict: report it briefly, never enforce it (see permitted).
		next.expires = timeNowUTC().Add(observedDenialTTL)
	}

	s.mu.Lock()
	current := s.caps.state(permission)
	// An explicit administrative/configuration denial is a hard ceiling. A
	// provider denial can later be replaced by a successful observed operation
	// because credentials or bucket policies may be refreshed without restart.
	if current.Source != "runtime" && current.Source != "configuration" {
		s.caps.set(permission, next)
		now := timeNowUTC()
		s.caps.CheckedAt = &now
	}
	s.mu.Unlock()
}

// timeNowUTC is a tiny seam used by capability observations and tests without
// coupling storage operations to a process-wide clock abstraction.
var timeNowUTC = func() time.Time { return time.Now().UTC() }

func (s *storageInstance) List(ctx context.Context, options listOptions) (listPage, error) {
	release, err := s.beginStorageRequest(ctx)
	if err != nil {
		return listPage{}, err
	}
	page, err := s.backend.List(ctx, options)
	release()
	s.observePermission(permissionRead, err)
	if err == nil && options.Delimiter != "" && len(page.Prefixes) > 0 {
		page.Prefixes = s.withoutEmptyPrefixes(ctx, page.Prefixes)
	}
	return page, err
}

// maxPrefixProbes bounds the requests that one page of a listing adds to look for an empty folder.
const maxPrefixProbes = 200

// withoutEmptyPrefixes removes the folders that hold no object. In a bucket with versioning, MinIO keeps the folder of an
// object whose current version is a delete marker in the listing with a delimiter, and shows no key for it in the listing
// without a delimiter. The person then sees a folder that is empty, and a delete of that folder finds nothing to delete.
// A provider that does not do this (AWS, SeaweedFS) costs no request. A probe that fails keeps its folder.
func (s *storageInstance) withoutEmptyPrefixes(ctx context.Context, prefixes []string) []string {
	if !s.versioningAvailable() {
		return prefixes
	}
	if flavour := s.lifecycleFlavour(ctx); flavour == flavourAWS || flavour == flavourSeaweedFS || flavour == flavourGarage {
		return prefixes
	}
	checked := prefixes
	if len(checked) > maxPrefixProbes {
		checked = checked[:maxPrefixProbes]
	}
	empty := make([]bool, len(checked))
	var wg sync.WaitGroup
	slots := make(chan struct{}, 4)
	for index, prefix := range checked {
		wg.Add(1)
		slots <- struct{}{}
		go func(index int, prefix string) {
			defer wg.Done()
			defer func() { <-slots }()
			release, err := s.beginStorageRequest(ctx)
			if err != nil {
				return
			}
			defer release()
			page, err := s.backend.List(ctx, listOptions{Prefix: prefix, MaxResults: 1})
			empty[index] = err == nil && len(page.Objects) == 0
		}(index, prefix)
	}
	wg.Wait()
	kept := make([]string, 0, len(prefixes))
	for index, prefix := range prefixes {
		if index < len(empty) && empty[index] {
			continue
		}
		kept = append(kept, prefix)
	}
	return kept
}

func (s *storageInstance) Head(ctx context.Context, key string) (objectResponse, error) {
	if version := objectVersionFromContext(ctx); version != "" {
		return s.HeadVersion(ctx, key, version)
	}
	release, err := s.beginStorageRequest(ctx)
	if err != nil {
		return objectResponse{}, err
	}
	defer release()
	response, err := s.backend.Head(ctx, key)
	if err == nil && (response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden) {
		err = &upstreamError{StatusCode: response.StatusCode, Code: "AccessDenied"}
	}
	s.observePermission(permissionRead, err)
	return response, err
}

type budgetReadCloser struct {
	body   io.ReadCloser
	budget *resourceBudget
	once   sync.Once
}

func (r *budgetReadCloser) Read(buffer []byte) (int, error) {
	count, err := r.body.Read(buffer)
	if count > 0 && r.budget != nil {
		if budgetErr := r.budget.consumeBytes(int64(count)); budgetErr != nil {
			return count, budgetErr
		}
	}
	return count, err
}

func (r *budgetReadCloser) Close() error {
	var err error
	r.once.Do(func() { err = r.body.Close() })
	return err
}

func (s *storageInstance) Get(ctx context.Context, key string, headers http.Header) (objectResponse, error) {
	if version := objectVersionFromContext(ctx); version != "" {
		return s.GetVersion(ctx, key, version, headers)
	}
	release, err := s.beginStorageRequest(ctx)
	if err != nil {
		return objectResponse{}, err
	}
	response, err := s.backend.Get(ctx, key, headers)
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

type releaseReadCloser struct {
	io.ReadCloser
	release func()
	once    sync.Once
}

func (r *releaseReadCloser) Close() error {
	err := r.ReadCloser.Close()
	r.once.Do(r.release)
	return err
}

func (s *storageInstance) Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) error {
	// The ceiling object.write: the check comes before the read of the body. A size below 0 is an unknown size.
	if err := checkWriteCeiling(ctx, size); err != nil {
		return err
	}
	release, err := s.beginStorageRequest(ctx)
	if err != nil {
		return err
	}
	defer release()
	if budget := budgetFromContext(ctx); budget != nil && size > 0 {
		if err := budget.consumeBytes(size); err != nil {
			return err
		}
	}
	err = s.backend.Put(ctx, key, body, size, contentType)
	s.observePermission(permissionWrite, err)
	return err
}

// PutIfAbsent creates an object only when no current object already exists at
// the destination. S3 and GCS implement this atomically through provider
// preconditions. The fallback is intentionally conservative and is used only
// by test or third-party backends that do not expose conditional writes.
func (s *storageInstance) PutIfAbsent(ctx context.Context, key string, body io.Reader, size int64, contentType string) error {
	if err := checkWriteCeiling(ctx, size); err != nil {
		return err
	}
	if conditional, ok := s.backend.(ConditionalObjectWriter); ok {
		release, err := s.beginStorageRequest(ctx)
		if err != nil {
			return err
		}
		defer release()
		if budget := budgetFromContext(ctx); budget != nil && size > 0 {
			if err := budget.consumeBytes(size); err != nil {
				return err
			}
		}
		err = conditional.PutIfAbsent(ctx, key, body, size, contentType)
		s.observePermission(permissionWrite, err)
		return err
	}

	response, err := s.Head(ctx, key)
	if err == nil {
		closeObjectResponse(response)
		return apiError{Status: http.StatusConflict, Code: "object_exists", Message: "an object already exists at the extraction destination"}
	}
	if !isStorageNotFound(err) {
		return err
	}
	return s.Put(ctx, key, body, size, contentType)
}

// Delete deletes the key. A refusal that an active Object Lock explains gives the error object_locked.
func (s *storageInstance) Delete(ctx context.Context, key string) error {
	release, err := s.beginStorageRequest(ctx)
	if err != nil {
		return err
	}
	err = s.backend.Delete(ctx, key)
	release()
	if err = s.explainLockRefusal(ctx, key, "", err); isObjectLocked(err) {
		return err
	}
	s.observePermission(permissionDelete, err)
	return err
}

// DeleteMany deletes the keys, in batches of maximumS3DeleteBatch when the backend can. A key that XML cannot carry, a
// backend without a batch delete, and a provider that refuses the batch request all go to the delete of one key. A key
// that an active Object Lock keeps does not stop the delete: it goes to locked, with its full key. The error is the
// first other failure.
func (s *storageInstance) DeleteMany(ctx context.Context, keys []string) ([]string, error) {
	var locked []string
	batcher, canBatch := s.backend.(ObjectBatchDeleter)
	if !canBatch || s.batchDeleteOff.Load() {
		return s.deleteEach(ctx, keys)
	}
	batch := make([]string, 0, minInt(len(keys), maximumS3DeleteBatch))
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		defer func() { batch = batch[:0] }()
		release, err := s.beginStorageRequest(ctx)
		if err != nil {
			return err
		}
		err = batcher.DeleteMany(ctx, batch)
		release()
		if err != nil && batchDeleteUnsupported(err) {
			s.batchDeleteOff.Store(true)
			each, err := s.deleteEach(ctx, batch)
			locked = append(locked, each...)
			return err
		}
		var refused *batchDeleteError
		if errors.As(err, &refused) {
			var kept []string
			kept, err = s.lockedKeys(ctx, refused)
			locked = append(locked, kept...)
			if err == nil && len(kept) == len(batch) {
				// The lock kept every key: the batch says nothing about the delete permission.
				return nil
			}
		}
		s.observePermission(permissionDelete, err)
		return err
	}
	for _, key := range keys {
		if !xmlSafeKey(key) {
			each, err := s.deleteEach(ctx, []string{key})
			locked = append(locked, each...)
			if err != nil {
				return locked, err
			}
			continue
		}
		batch = append(batch, key)
		if len(batch) == maximumS3DeleteBatch {
			if err := flush(); err != nil {
				return locked, err
			}
		}
	}
	return locked, flush()
}

// lockedKeys reads the lock of the refused keys of a batch delete, for up to maxLockChecks keys. It gives the keys
// that an active lock keeps, and a batchDeleteError with the other refused keys, or nil when the lock explains each
// refusal.
func (s *storageInstance) lockedKeys(ctx context.Context, refused *batchDeleteError) ([]string, error) {
	var locked []string
	others := &batchDeleteError{Total: refused.Total}
	for index, failure := range refused.Failures {
		candidate := &upstreamError{StatusCode: http.StatusBadGateway, Code: failure.Code, Message: failure.Message}
		if index < maxLockChecks && isObjectLocked(s.explainLockRefusal(ctx, failure.Key, "", candidate)) {
			locked = append(locked, failure.Key)
			continue
		}
		others.Failures = append(others.Failures, failure)
	}
	if len(others.Failures) == 0 {
		return locked, nil
	}
	return locked, others
}

// deleteEach deletes the keys one by one. A key that an active Object Lock keeps goes to locked, and the delete goes on.
func (s *storageInstance) deleteEach(ctx context.Context, keys []string) ([]string, error) {
	var locked []string
	for _, key := range keys {
		if err := s.Delete(ctx, key); err != nil {
			if isObjectLocked(err) {
				locked = append(locked, key)
				continue
			}
			return locked, err
		}
	}
	return locked, nil
}

// batchDeleteUnsupported reports a provider that does not know the DeleteObjects request.
func batchDeleteUnsupported(err error) bool {
	var upstream *upstreamError
	if !errors.As(err, &upstream) {
		return false
	}
	return upstream.StatusCode == http.StatusNotImplemented || upstream.StatusCode == http.StatusMethodNotAllowed ||
		upstream.Code == "NotImplemented" || upstream.Code == "MethodNotAllowed"
}

// xmlSafeKey reports whether the key goes through an XML document unchanged: valid UTF-8 and no control character
// that XML 1.0 forbids.
func xmlSafeKey(key string) bool {
	if !utf8.ValidString(key) {
		return false
	}
	for _, r := range key {
		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' || r == 0xFFFE || r == 0xFFFF {
			return false
		}
	}
	return true
}

// Copy copies one object. A ceiling of read or write applies to the size of the source: the check comes before the copy.
func (s *storageInstance) Copy(ctx context.Context, source, destination string) error {
	if err := s.checkCopySource(ctx, source, ""); err != nil {
		return err
	}
	return s.copyObject(ctx, source, destination)
}

// copyObject copies one object with one request. The caller checks the ceilings.
func (s *storageInstance) copyObject(ctx context.Context, source, destination string) error {
	release, err := s.beginStorageRequest(ctx)
	if err != nil {
		return err
	}
	defer release()
	err = s.backend.Copy(ctx, source, destination)
	if err == nil {
		s.observePermission(permissionRead, nil)
		s.observePermission(permissionWrite, nil)
	} else {
		// S3 API implementations do not reliably identify whether a CopyObject denial
		// came from reading the source or writing the destination. Keep read
		// tentable and learn the conservative write denial only.
		s.observePermission(permissionWrite, err)
	}
	return err
}

// movesObjects tells if the backend moves an object with one rename.
func (s *storageInstance) movesObjects() bool {
	_, ok := s.backend.(ObjectMover)
	return ok
}

// Move moves one object with one rename. The caller checks movesObjects first.
func (s *storageInstance) Move(ctx context.Context, source, destination string) error {
	mover, ok := s.backend.(ObjectMover)
	if !ok {
		return apiError{Status: http.StatusNotImplemented, Code: "move_unsupported", Message: "this storage provider does not move objects with a rename"}
	}
	if err := s.checkCopySource(ctx, source, ""); err != nil {
		return err
	}
	release, err := s.beginStorageRequest(ctx)
	if err != nil {
		return err
	}
	defer release()
	err = mover.Move(ctx, source, destination)
	if err == nil {
		for _, permission := range []string{permissionRead, permissionWrite, permissionDelete} {
			s.observePermission(permission, nil)
		}
	} else {
		s.observePermission(permissionWrite, err)
	}
	return err
}

// removeEmptyFolders removes the folders under prefix (a full key) that have no file, for a backend with real folders.
// Other backends have no folders: it does nothing.
func (s *storageInstance) removeEmptyFolders(ctx context.Context, prefix string) error {
	remover, ok := s.backend.(FolderRemover)
	if !ok {
		return nil
	}
	release, err := s.beginStorageRequest(ctx)
	if err != nil {
		return err
	}
	defer release()
	return remover.RemoveEmptyFolders(ctx, prefix)
}

// CopySized copies one object whose size the caller knows. From multipartCopyThreshold up, a backend that copies in
// parts gets a copy in parts. Other copies are one CopyObject request. Progress receives the size of each copied part.
//
// The caller gives the size: the check of the ceilings of read and write uses it and makes no HEAD request.
func (s *storageInstance) CopySized(ctx context.Context, source, destination string, size int64, progress func(int64)) error {
	if ceilingApplies(ctx, actionObjectRead, actionObjectWrite) {
		if err := checkCopyCeilings(ctx, size); err != nil {
			return err
		}
	}
	if s.copiesInParts(size) {
		return s.copyMultipartObject(ctx, source, destination, multipartCopyOptions{Progress: progress})
	}
	return s.copyObject(ctx, source, destination)
}

// copiesInParts reports whether a copy of size bytes goes in parts.
func (s *storageInstance) copiesInParts(size int64) bool {
	_, ok := s.backend.(ObjectMultipartCopier)
	return ok && size >= multipartCopyThreshold
}

// CopyMultipart copies an object in parts. Each request of the copy takes a slot of the gates of the instance, so the
// limits of concurrent requests apply. A request waits for its slot without time limit: a copy in parts is long.
//
// A ceiling of read or write applies to the size of the source: the check comes before the copy.
func (s *storageInstance) CopyMultipart(ctx context.Context, source, destination string, options multipartCopyOptions) error {
	if _, ok := s.backend.(ObjectMultipartCopier); ok {
		if err := s.checkCopySource(ctx, source, options.SourceVersion); err != nil {
			return err
		}
	}
	return s.copyMultipartObject(ctx, source, destination, options)
}

// copyMultipartObject copies an object in parts. The caller checks the ceilings.
func (s *storageInstance) copyMultipartObject(ctx context.Context, source, destination string, options multipartCopyOptions) error {
	copier, ok := s.backend.(ObjectMultipartCopier)
	if !ok {
		return apiError{Status: http.StatusNotImplemented, Code: "multipart_copy_unsupported", Message: "this storage provider does not copy objects in parts"}
	}
	options.gate = s.beginStorageRequest
	err := copier.CopyMultipart(withUnboundedGateWait(ctx), source, destination, options)
	if err == nil {
		s.observePermission(permissionRead, nil)
		s.observePermission(permissionWrite, nil)
	} else {
		// As for CopyObject: keep read tentable and learn the conservative write denial only.
		s.observePermission(permissionWrite, err)
	}
	return err
}

func (s *storageInstance) discoverCapabilities(ctx context.Context) (discoveredCapabilities, error) {
	release, err := s.beginStorageRequest(ctx)
	if err != nil {
		return discoveredCapabilities{}, err
	}
	defer release()
	return s.backend.DiscoverCapabilities(ctx)
}

func (s *storageInstance) InitiateMultipart(ctx context.Context, key, contentType string) (string, error) {
	backend, ok := s.backend.(s3MultipartAPI)
	if !ok {
		return "", errors.New("s3 backend does not support multipart uploads")
	}
	release, err := s.beginStorageRequest(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	uploadID, err := backend.InitiateMultipart(ctx, key, contentType)
	s.observePermission(permissionWrite, err)
	return uploadID, err
}

func (s *storageInstance) UploadPart(ctx context.Context, key, uploadID string, partNumber int, body io.Reader, size int64) (string, error) {
	// A part is not larger than its object: a part over the ceiling object.write is a refusal.
	if err := checkWriteCeiling(ctx, size); err != nil {
		return "", err
	}
	backend, ok := s.backend.(s3MultipartAPI)
	if !ok {
		return "", errors.New("s3 backend does not support multipart uploads")
	}
	release, err := s.beginStorageRequest(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	if budget := budgetFromContext(ctx); budget != nil && size > 0 {
		if err := budget.consumeBytes(size); err != nil {
			return "", err
		}
	}
	etag, err := backend.UploadPart(ctx, key, uploadID, partNumber, body, size)
	s.observePermission(permissionWrite, err)
	return etag, err
}

func (s *storageInstance) CompleteMultipart(ctx context.Context, key, uploadID string, parts []s3CompletedPart) error {
	backend, ok := s.backend.(s3MultipartAPI)
	if !ok {
		return errors.New("s3 backend does not support multipart uploads")
	}
	release, err := s.beginStorageRequest(ctx)
	if err != nil {
		return err
	}
	defer release()
	err = backend.CompleteMultipart(ctx, key, uploadID, parts)
	s.observePermission(permissionWrite, err)
	return err
}

func (s *storageInstance) AbortMultipart(ctx context.Context, key, uploadID string) error {
	if remover, ok := s.backend.(IncompleteUploadRemover); ok {
		release, err := s.beginStorageRequest(ctx)
		if err != nil {
			return err
		}
		defer release()
		err = remover.RemoveIncompleteUpload(ctx, key, uploadID)
		s.observePermission(permissionWrite, err)
		return err
	}
	backend, ok := s.backend.(s3MultipartAPI)
	if !ok {
		return errors.New("s3 backend does not support multipart uploads")
	}
	release, err := s.beginStorageRequest(ctx)
	if err != nil {
		return err
	}
	defer release()
	err = backend.AbortMultipart(ctx, key, uploadID)
	s.observePermission(permissionWrite, err)
	return err
}

func (s *storageInstance) InitiateResumable(ctx context.Context, key string, total int64, contentType string) (string, error) {
	if err := checkWriteCeiling(ctx, total); err != nil {
		return "", err
	}
	backend, ok := s.backend.(gcsResumableAPI)
	if !ok {
		return "", errors.New("gcs backend does not support resumable uploads")
	}
	release, err := s.beginStorageRequest(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	sessionURL, err := backend.InitiateResumable(ctx, key, total, contentType)
	s.observePermission(permissionWrite, err)
	return sessionURL, err
}

func (s *storageInstance) UploadResumableChunk(ctx context.Context, sessionURL string, body io.Reader, start, size, total int64, contentType string) (int64, bool, error) {
	// The total is the size of the object that the upload writes.
	if err := checkWriteCeiling(ctx, total); err != nil {
		return 0, false, err
	}
	backend, ok := s.backend.(gcsResumableAPI)
	if !ok {
		return 0, false, errors.New("gcs backend does not support resumable uploads")
	}
	release, err := s.beginStorageRequest(ctx)
	if err != nil {
		return 0, false, err
	}
	defer release()
	if budget := budgetFromContext(ctx); budget != nil && size > 0 {
		if err := budget.consumeBytes(size); err != nil {
			return 0, false, err
		}
	}
	next, complete, err := backend.UploadResumableChunk(ctx, sessionURL, body, start, size, total, contentType)
	s.observePermission(permissionWrite, err)
	return next, complete, err
}

func (s *storageInstance) QueryResumable(ctx context.Context, sessionURL string, total int64) (int64, bool, error) {
	backend, ok := s.backend.(gcsResumableAPI)
	if !ok {
		return 0, false, errors.New("gcs backend does not support resumable uploads")
	}
	release, err := s.beginStorageRequest(ctx)
	if err != nil {
		return 0, false, err
	}
	defer release()
	next, complete, err := backend.QueryResumable(ctx, sessionURL, total)
	s.observePermission(permissionWrite, err)
	return next, complete, err
}

func (s *storageInstance) AbortResumable(ctx context.Context, sessionURL string) error {
	backend, ok := s.backend.(gcsResumableAPI)
	if !ok {
		return errors.New("gcs backend does not support resumable uploads")
	}
	release, err := s.beginStorageRequest(ctx)
	if err != nil {
		return err
	}
	defer release()
	err = backend.AbortResumable(ctx, sessionURL)
	s.observePermission(permissionWrite, err)
	return err
}
