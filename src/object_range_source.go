package main

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

const maxSingleObjectRangeBytes = int64(128 << 20)

type objectVersion struct {
	ETag       string `json:"e,omitempty"`
	Generation string `json:"g,omitempty"`
	Modified   string `json:"m,omitempty"`
}

func (v objectVersion) empty() bool {
	return strings.TrimSpace(v.ETag) == "" && strings.TrimSpace(v.Generation) == "" && strings.TrimSpace(v.Modified) == ""
}

type objectByteSpan struct {
	start int64
	data  []byte
}

func (s objectByteSpan) end() int64 { return s.start + int64(len(s.data)) }

type rangeFlight struct {
	done chan struct{}
	data []byte
	err  error
}

type objectRangeState struct {
	mu sync.Mutex

	size       int64
	headers    http.Header
	etag       string
	generation string
	modified   string
	requests   int
	bytes      int64

	cache         *list.List
	cacheBytes    int64
	maxCacheBytes int64
	inflight      map[string]*rangeFlight
}

// objectRangeSource provides version-bound random access to one remote object.
// Its cache is strictly bounded and can be shared by short-lived readers that
// use different request contexts (SQLite sessions use this facility).
type objectRangeSource struct {
	ctx      context.Context
	instance *storageInstance
	key      string
	version  string
	state    *objectRangeState
}

func openObjectRangeSource(ctx context.Context, instance *storageInstance, key string) (*objectRangeSource, error) {
	return openObjectRangeSourceVersion(ctx, instance, key, "")
}

func openObjectRangeSourceVersion(ctx context.Context, instance *storageInstance, key, version string) (*objectRangeSource, error) {
	if instance == nil || instance.backend == nil {
		return nil, fmt.Errorf("storage instance is not initialized")
	}
	maxCache := instance.policy.MaxRangeCacheBytes
	if maxCache <= 0 {
		maxCache = defaultMaxRangeCacheBytes
	}
	return &objectRangeSource{
		ctx:      ctx,
		instance: instance,
		key:      key,
		version:  strings.TrimSpace(version),
		state: &objectRangeState{
			cache:         list.New(),
			maxCacheBytes: maxCache,
			inflight:      make(map[string]*rangeFlight),
		},
	}, nil
}

func (s *objectRangeSource) withContext(ctx context.Context) *objectRangeSource {
	if s == nil {
		return nil
	}
	return &objectRangeSource{ctx: ctx, instance: s.instance, key: s.key, version: s.version, state: s.state}
}

func (s *objectRangeSource) Size() int64 {
	if s == nil || s.state == nil {
		return 0
	}
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	return s.state.size
}

func (s *objectRangeSource) SetExpectedVersion(version objectVersion) {
	if s == nil || s.state == nil {
		return
	}
	version.ETag = normalizeExpectedETag(version.ETag)
	version.Generation = strings.TrimSpace(version.Generation)
	version.Modified = strings.TrimSpace(version.Modified)
	s.state.mu.Lock()
	if s.state.etag == "" {
		s.state.etag = version.ETag
	}
	if s.state.generation == "" {
		s.state.generation = version.Generation
	}
	if s.state.modified == "" {
		s.state.modified = version.Modified
	}
	s.state.mu.Unlock()
}

func (s *objectRangeSource) Version() objectVersion {
	if s == nil || s.state == nil {
		return objectVersion{}
	}
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	return objectVersion{ETag: s.state.etag, Generation: s.state.generation, Modified: s.state.modified}
}

func (s *objectRangeSource) SetExpectedETag(etag string) {
	if s == nil || s.state == nil {
		return
	}
	etag = normalizeExpectedETag(etag)
	if etag == "" {
		return
	}
	s.state.mu.Lock()
	if s.state.etag == "" {
		s.state.etag = etag
	}
	s.state.mu.Unlock()
}

func (s *objectRangeSource) SetKnownSize(size int64) {
	if s == nil || s.state == nil || size <= 0 {
		return
	}
	s.state.mu.Lock()
	if s.state.size == 0 {
		s.state.size = size
	}
	s.state.mu.Unlock()
}

func (s *objectRangeSource) Headers() http.Header {
	if s == nil || s.state == nil {
		return nil
	}
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	if s.state.headers == nil {
		return nil
	}
	return s.state.headers.Clone()
}

func (s *objectRangeSource) RequestCount() int {
	if s == nil || s.state == nil {
		return 0
	}
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	return s.state.requests
}

func (s *objectRangeSource) BytesRead() int64 {
	if s == nil || s.state == nil {
		return 0
	}
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	return s.state.bytes
}

func (s *objectRangeSource) requestHeaders(rangeValue string) http.Header {
	headers := make(http.Header)
	headers.Set("Range", rangeValue)
	if s == nil || s.state == nil {
		return headers
	}
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	switch {
	case s.state.etag != "":
		headers.Set("If-Match", s.state.etag)
	case s.state.generation != "":
		headers.Set("x-goog-if-generation-match", s.state.generation)
	case s.state.modified != "":
		headers.Set("If-Unmodified-Since", s.state.modified)
	}
	return headers
}

func objectSizeFromResponse(response objectResponse, received int64) int64 {
	if contentRange := strings.TrimSpace(response.Header.Get("Content-Range")); contentRange != "" {
		if slash := strings.LastIndex(contentRange, "/"); slash >= 0 && slash+1 < len(contentRange) {
			if total, err := strconv.ParseInt(strings.TrimSpace(contentRange[slash+1:]), 10, 64); err == nil && total >= 0 {
				return total
			}
		}
	}
	if value, err := strconv.ParseInt(strings.TrimSpace(response.Header.Get("Content-Length")), 10, 64); err == nil && value >= 0 {
		if response.StatusCode == http.StatusPartialContent {
			return maxInt64(value, received)
		}
		return value
	}
	return received
}

func (s *objectRangeSource) validateAndRecordResponse(response objectResponse, total int64, received int64) error {
	if s == nil || s.state == nil {
		return errors.New("range source is unavailable")
	}
	etag := strings.TrimSpace(response.Header.Get("ETag"))
	generation := strings.TrimSpace(response.Header.Get("x-goog-generation"))
	modified := strings.TrimSpace(response.Header.Get("Last-Modified"))

	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	if s.state.size > 0 && total > 0 && s.state.size != total {
		return apiError{Status: http.StatusPreconditionFailed, Code: "object_changed", Message: "the object changed while it was being previewed"}
	}
	if s.state.etag != "" && etag != "" && s.state.etag != etag {
		return apiError{Status: http.StatusPreconditionFailed, Code: "object_changed", Message: "the object changed while it was being previewed"}
	}
	if s.state.generation != "" && generation != "" && s.state.generation != generation {
		return apiError{Status: http.StatusPreconditionFailed, Code: "object_changed", Message: "the object generation changed while it was being previewed"}
	}
	if s.state.etag == "" && s.state.generation == "" && s.state.modified != "" && modified != "" && s.state.modified != modified {
		return apiError{Status: http.StatusPreconditionFailed, Code: "object_changed", Message: "the object changed while it was being previewed"}
	}
	if s.state.headers == nil {
		s.state.headers = response.Header.Clone()
	}
	if s.state.size == 0 {
		s.state.size = total
	}
	if s.state.etag == "" {
		s.state.etag = etag
	}
	if s.state.generation == "" {
		s.state.generation = generation
	}
	if s.state.modified == "" {
		s.state.modified = modified
	}
	s.state.requests++
	s.state.bytes += received
	return nil
}

func (s *objectRangeSource) beginFlight(key string) (*rangeFlight, bool) {
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	if flight := s.state.inflight[key]; flight != nil {
		return flight, false
	}
	flight := &rangeFlight{done: make(chan struct{})}
	s.state.inflight[key] = flight
	return flight, true
}

func (s *objectRangeSource) finishFlight(key string, flight *rangeFlight, data []byte, err error) {
	s.state.mu.Lock()
	flight.data = append([]byte(nil), data...)
	flight.err = err
	delete(s.state.inflight, key)
	close(flight.done)
	s.state.mu.Unlock()
}

func (s *objectRangeSource) waitFlight(flight *rangeFlight) ([]byte, error) {
	select {
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	case <-flight.done:
		s.state.mu.Lock()
		defer s.state.mu.Unlock()
		return append([]byte(nil), flight.data...), flight.err
	}
}

func (s *objectRangeSource) fetch(start, length int64) ([]byte, objectResponse, error) {
	if start < 0 || length <= 0 || length > maxSingleObjectRangeBytes {
		return nil, objectResponse{}, fmt.Errorf("invalid object metadata range")
	}
	end := start + length - 1
	if end < start {
		return nil, objectResponse{}, fmt.Errorf("invalid object metadata range")
	}
	flightKey := fmt.Sprintf("%d:%d", start, end)
	flight, owner := s.beginFlight(flightKey)
	if !owner {
		data, err := s.waitFlight(flight)
		return data, objectResponse{}, err
	}
	var result []byte
	var resultErr error
	defer func() { s.finishFlight(flightKey, flight, result, resultErr) }()

	headers := s.requestHeaders(fmt.Sprintf("bytes=%d-%d", start, end))
	response, err := s.instance.GetVersion(s.ctx, s.instance.fullKey(s.key), s.version, headers)
	if err != nil {
		resultErr = err
		return nil, objectResponse{}, err
	}
	if response.Body == nil {
		resultErr = apiError{Status: http.StatusBadGateway, Code: "empty_metadata_response", Message: "the storage provider returned an empty metadata response"}
		return nil, objectResponse{}, resultErr
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusPreconditionFailed {
		resultErr = apiError{Status: http.StatusPreconditionFailed, Code: "object_changed", Message: "the object changed while it was being previewed"}
		return nil, objectResponse{}, resultErr
	}
	if response.StatusCode == http.StatusOK {
		contentLength, parseErr := strconv.ParseInt(strings.TrimSpace(response.Header.Get("Content-Length")), 10, 64)
		// An empty object has no satisfiable byte range. Some providers answer 200 with an empty body instead of 416; accepting that
		// response cannot accidentally transfer a large object.
		if parseErr == nil && contentLength == 0 && start == 0 {
			if err := s.validateAndRecordResponse(response, 0, 0); err != nil {
				resultErr = err
				return nil, objectResponse{}, err
			}
			return []byte{}, response, nil
		}
		if !s.instance.policy.AllowFullObjectFallback {
			resultErr = apiError{Status: http.StatusBadGateway, Code: "range_not_supported", Message: "the storage provider ignored the byte-range request; the complete object was not downloaded"}
			return nil, objectResponse{}, resultErr
		}
		if start != 0 || parseErr != nil || contentLength < 0 || contentLength > length {
			resultErr = apiError{Status: http.StatusBadGateway, Code: "range_not_supported", Message: "the storage provider ignored a byte-range request"}
			return nil, objectResponse{}, resultErr
		}
		data, readErr := io.ReadAll(io.LimitReader(response.Body, length+1))
		if readErr != nil {
			resultErr = fmt.Errorf("read object metadata range: %w", readErr)
			return nil, objectResponse{}, resultErr
		}
		if int64(len(data)) != contentLength || int64(len(data)) > length {
			resultErr = apiError{Status: http.StatusBadGateway, Code: "range_length_mismatch", Message: "the storage provider returned an invalid byte count"}
			return nil, objectResponse{}, resultErr
		}
		if err := s.validateAndRecordResponse(response, contentLength, int64(len(data))); err != nil {
			resultErr = err
			return nil, objectResponse{}, err
		}
		result = data
		return data, response, nil
	}
	if response.StatusCode != http.StatusPartialContent {
		resultErr = &upstreamError{StatusCode: response.StatusCode, Code: "MetadataRangeReadFailed"}
		return nil, objectResponse{}, resultErr
	}
	contentRange, total, parseErr := parseGatewayContentRange(response.Header.Get("Content-Range"))
	if parseErr != nil || total <= 0 {
		resultErr = apiError{Status: http.StatusBadGateway, Code: "invalid_content_range", Message: "the storage provider returned an invalid Content-Range header"}
		return nil, objectResponse{}, resultErr
	}
	expectedEnd := minInt64(end, total-1)
	if start >= total || contentRange.start != start || contentRange.end != expectedEnd {
		resultErr = apiError{Status: http.StatusBadGateway, Code: "range_mismatch", Message: "the storage provider returned a different byte range than requested"}
		return nil, objectResponse{}, resultErr
	}
	expectedLength := contentRange.end - contentRange.start + 1
	if advertised := strings.TrimSpace(response.Header.Get("Content-Length")); advertised != "" {
		parsed, parseLengthErr := strconv.ParseInt(advertised, 10, 64)
		if parseLengthErr != nil || parsed != expectedLength {
			resultErr = apiError{Status: http.StatusBadGateway, Code: "range_length_mismatch", Message: "the storage provider advertised an invalid range length"}
			return nil, objectResponse{}, resultErr
		}
	}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, expectedLength+1))
	if readErr != nil {
		resultErr = fmt.Errorf("read object metadata range: %w", readErr)
		return nil, objectResponse{}, resultErr
	}
	if int64(len(data)) != expectedLength {
		resultErr = apiError{Status: http.StatusBadGateway, Code: "range_truncated", Message: "the storage provider returned a truncated or oversized byte range"}
		return nil, objectResponse{}, resultErr
	}
	if err := s.validateAndRecordResponse(response, total, int64(len(data))); err != nil {
		resultErr = err
		return nil, objectResponse{}, err
	}
	result = data
	return data, response, nil
}

func (s *objectRangeSource) cacheSpan(span objectByteSpan) {
	if s == nil || s.state == nil || len(span.data) == 0 {
		return
	}
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	if s.state.maxCacheBytes <= 0 || int64(len(span.data)) > s.state.maxCacheBytes {
		return
	}
	for element := s.state.cache.Front(); element != nil; {
		next := element.Next()
		existing := element.Value.(objectByteSpan)
		if existing.start == span.start && existing.end() == span.end() {
			s.state.cacheBytes -= int64(len(existing.data))
			s.state.cache.Remove(element)
		}
		element = next
	}
	copySpan := objectByteSpan{start: span.start, data: append([]byte(nil), span.data...)}
	s.state.cache.PushFront(copySpan)
	s.state.cacheBytes += int64(len(copySpan.data))
	for s.state.cacheBytes > s.state.maxCacheBytes && s.state.cache.Len() > 0 {
		last := s.state.cache.Back()
		value := last.Value.(objectByteSpan)
		s.state.cacheBytes -= int64(len(value.data))
		s.state.cache.Remove(last)
	}
}

func (s *objectRangeSource) cachedSpanAt(offset int64) (objectByteSpan, bool) {
	if s == nil || s.state == nil {
		return objectByteSpan{}, false
	}
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	for element := s.state.cache.Front(); element != nil; element = element.Next() {
		span := element.Value.(objectByteSpan)
		if offset >= span.start && offset < span.end() {
			s.state.cache.MoveToFront(element)
			return objectByteSpan{start: span.start, data: append([]byte(nil), span.data...)}, true
		}
	}
	return objectByteSpan{}, false
}

func (s *objectRangeSource) nextCachedStart(offset, limit int64) int64 {
	next := limit
	if s == nil || s.state == nil {
		return next
	}
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	for element := s.state.cache.Front(); element != nil; element = element.Next() {
		span := element.Value.(objectByteSpan)
		if span.start > offset && span.start < next {
			next = span.start
		}
	}
	return next
}

func (s *objectRangeSource) hasByte(offset int64) bool {
	_, ok := s.cachedSpanAt(offset)
	return ok
}

func (s *objectRangeSource) ReadRange(start, length int64) ([]byte, error) {
	if start < 0 || length <= 0 || length > maxSingleObjectRangeBytes {
		return nil, fmt.Errorf("invalid object metadata range")
	}
	if size := s.Size(); size > 0 {
		// A cached span can come from a request with a higher ceiling: the ceiling of this request applies to it too.
		if err := checkReadCeiling(s.ctx, size); err != nil {
			return nil, err
		}
		if start >= size {
			return nil, io.EOF
		}
		length = minInt64(length, size-start)
	}
	end := start + length
	out := make([]byte, length)
	cursor := start
	for cursor < end {
		if span, ok := s.cachedSpanAt(cursor); ok {
			copyEnd := minInt64(end, span.end())
			copy(out[cursor-start:copyEnd-start], span.data[cursor-span.start:copyEnd-span.start])
			cursor = copyEnd
			continue
		}
		gapEnd := s.nextCachedStart(cursor, end)
		fetched, _, err := s.fetch(cursor, gapEnd-cursor)
		if err != nil {
			return nil, err
		}
		if len(fetched) == 0 {
			return out[:cursor-start], io.EOF
		}
		copy(out[cursor-start:], fetched)
		s.cacheSpan(objectByteSpan{start: cursor, data: fetched})
		cursor += int64(len(fetched))
		if cursor < gapEnd {
			return out[:cursor-start], io.EOF
		}
	}
	return out, nil
}

func (s *objectRangeSource) ReadAt(destination []byte, offset int64) (int, error) {
	if len(destination) == 0 {
		return 0, nil
	}
	data, err := s.ReadRange(offset, int64(len(destination)))
	read := copy(destination, data)
	if err != nil {
		return read, err
	}
	if read != len(destination) {
		return read, io.EOF
	}
	return read, nil
}

// ReadSuffix performs one exact suffix request when the object size is not yet
// known. It both discovers the total size and supplies the tail bytes required
// by ZIP readers.
func (s *objectRangeSource) ReadSuffix(length int64) ([]byte, error) {
	if length <= 0 || length > maxSingleObjectRangeBytes {
		return nil, fmt.Errorf("invalid object metadata suffix length")
	}
	if size := s.Size(); size > 0 {
		length = minInt64(length, size)
		return s.ReadRange(size-length, length)
	}
	flightKey := fmt.Sprintf("suffix:%d", length)
	flight, owner := s.beginFlight(flightKey)
	if !owner {
		return s.waitFlight(flight)
	}
	var result []byte
	var resultErr error
	defer func() { s.finishFlight(flightKey, flight, result, resultErr) }()

	response, err := s.instance.GetVersion(s.ctx, s.instance.fullKey(s.key), s.version, s.requestHeaders(fmt.Sprintf("bytes=-%d", length)))
	if err != nil {
		resultErr = err
		return nil, err
	}
	if response.Body == nil {
		resultErr = apiError{Status: http.StatusBadGateway, Code: "empty_metadata_response", Message: "the storage provider returned an empty metadata response"}
		return nil, resultErr
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusPreconditionFailed {
		resultErr = apiError{Status: http.StatusPreconditionFailed, Code: "object_changed", Message: "the object changed while it was being previewed"}
		return nil, resultErr
	}
	if response.StatusCode == http.StatusOK {
		if !s.instance.policy.AllowFullObjectFallback {
			resultErr = apiError{Status: http.StatusBadGateway, Code: "range_not_supported", Message: "the storage provider ignored the suffix byte-range request; the complete object was not downloaded"}
			return nil, resultErr
		}
		contentLength, parseErr := strconv.ParseInt(strings.TrimSpace(response.Header.Get("Content-Length")), 10, 64)
		if parseErr != nil || contentLength < 0 || contentLength > length {
			resultErr = apiError{Status: http.StatusBadGateway, Code: "range_not_supported", Message: "the storage provider ignored a suffix byte-range request"}
			return nil, resultErr
		}
		data, readErr := io.ReadAll(io.LimitReader(response.Body, length+1))
		if readErr != nil || int64(len(data)) != contentLength {
			if readErr != nil {
				resultErr = readErr
			} else {
				resultErr = apiError{Status: http.StatusBadGateway, Code: "range_length_mismatch", Message: "the storage provider returned an invalid suffix length"}
			}
			return nil, resultErr
		}
		if err := s.validateAndRecordResponse(response, contentLength, int64(len(data))); err != nil {
			resultErr = err
			return nil, err
		}
		result = data
		s.cacheSpan(objectByteSpan{start: 0, data: data})
		return data, nil
	}
	if response.StatusCode != http.StatusPartialContent {
		resultErr = &upstreamError{StatusCode: response.StatusCode, Code: "MetadataRangeReadFailed"}
		return nil, resultErr
	}
	contentRange, total, parseErr := parseGatewayContentRange(response.Header.Get("Content-Range"))
	if parseErr != nil || total <= 0 {
		resultErr = apiError{Status: http.StatusBadGateway, Code: "invalid_content_range", Message: "the storage provider returned an invalid suffix Content-Range"}
		return nil, resultErr
	}
	expectedStart := maxInt64(0, total-length)
	if contentRange.start != expectedStart || contentRange.end != total-1 {
		resultErr = apiError{Status: http.StatusBadGateway, Code: "range_mismatch", Message: "the storage provider returned a different suffix range than requested"}
		return nil, resultErr
	}
	expectedLength := contentRange.end - contentRange.start + 1
	data, readErr := io.ReadAll(io.LimitReader(response.Body, expectedLength+1))
	if readErr != nil {
		resultErr = readErr
		return nil, readErr
	}
	if int64(len(data)) != expectedLength {
		resultErr = apiError{Status: http.StatusBadGateway, Code: "range_truncated", Message: "the storage provider returned a truncated or oversized suffix range"}
		return nil, resultErr
	}
	if err := s.validateAndRecordResponse(response, total, int64(len(data))); err != nil {
		resultErr = err
		return nil, err
	}
	result = data
	s.cacheSpan(objectByteSpan{start: contentRange.start, data: data})
	return data, nil
}

func (s *objectRangeSource) ReadPrefix(length int64) ([]byte, error) {
	if length <= 0 {
		return nil, nil
	}
	if size := s.Size(); size > 0 {
		length = minInt64(length, size)
	}
	return s.ReadRange(0, length)
}

func (s *objectRangeSource) clearCache() {
	if s == nil || s.state == nil {
		return
	}
	s.state.mu.Lock()
	s.state.cache.Init()
	s.state.cacheBytes = 0
	s.state.mu.Unlock()
}
