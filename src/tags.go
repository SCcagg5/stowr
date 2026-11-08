package main

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Object tags (S3 ?tagging), the read of the lifecycle rules of a bucket (the editor is in lifecycle.go), and the job
// that changes the tags of a prefix. Only the S3 backend has them: a provider that does not know them answers 501.

const (
	maxObjectTags    = 10
	maxTagKeyChars   = 128
	maxTagValueChars = 256
	jobTypeTagPrefix = "tag_prefix"
)

type objectTag struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// tagChange is the change of a tag job: the tags to set (a tag with the same key is replaced) and the keys to remove.
type tagChange struct {
	Add    []objectTag `json:"add,omitempty"`
	Remove []string    `json:"remove,omitempty"`
}

// ObjectTagger is optional: a backend with the tags of an object. An empty set of tags removes them all.
type ObjectTagger interface {
	GetObjectTagging(ctx context.Context, key, version string) ([]objectTag, error)
	PutObjectTagging(ctx context.Context, key, version string, tags []objectTag) error
}

// BucketLifecycleReader is optional: a backend that gives the lifecycle rules of its bucket.
type BucketLifecycleReader interface {
	GetBucketLifecycle(ctx context.Context) ([]lifecycleRule, error)
}

type lifecycleTransition struct {
	Days         int    `json:"days,omitempty"`
	Date         string `json:"date,omitempty"`
	StorageClass string `json:"storageClass,omitempty"`
}

type lifecycleRule struct {
	ID                       string                `json:"id,omitempty"`
	Enabled                  bool                  `json:"enabled"`
	Prefix                   string                `json:"prefix,omitempty"`
	Tags                     []objectTag           `json:"tags,omitempty"`
	SizeGreaterThan          int64                 `json:"sizeGreaterThan,omitempty"`
	SizeLessThan             int64                 `json:"sizeLessThan,omitempty"`
	ExpirationDays           int                   `json:"expirationDays,omitempty"`
	ExpirationDate           string                `json:"expirationDate,omitempty"`
	ExpiredObjectDeleteMark  bool                  `json:"expiredObjectDeleteMarker,omitempty"`
	Transitions              []lifecycleTransition `json:"transitions,omitempty"`
	NoncurrentExpirationDays int                   `json:"noncurrentExpirationDays,omitempty"`
	AbortMultipartDays       int                   `json:"abortIncompleteMultipartDays,omitempty"`
}

// validTagText reports whether the text only has the characters that S3 allows in a tag: letters, digits, spaces
// and + - = . _ : / @
func validTagText(text string) bool {
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsSpace(r) || strings.ContainsRune("+-=._:/@", r) {
			continue
		}
		return false
	}
	return true
}

// validateTags checks a set of tags against the limits of S3: 10 tags, a key of 1 to 128 characters that is unique and
// does not start with "aws:", a value of 0 to 256 characters.
func validateTags(tags []objectTag) error {
	if len(tags) > maxObjectTags {
		return apiError{Status: http.StatusBadRequest, Code: "invalid_tags", Message: fmt.Sprintf("an object has at most %d tags, got %d", maxObjectTags, len(tags))}
	}
	seen := make(map[string]bool, len(tags))
	for _, tag := range tags {
		keyLength := utf8.RuneCountInString(tag.Key)
		switch {
		case keyLength == 0 || keyLength > maxTagKeyChars:
			return apiError{Status: http.StatusBadRequest, Code: "invalid_tags", Message: fmt.Sprintf("a tag key has 1 to %d characters, got %d", maxTagKeyChars, keyLength)}
		case utf8.RuneCountInString(tag.Value) > maxTagValueChars:
			return apiError{Status: http.StatusBadRequest, Code: "invalid_tags", Message: fmt.Sprintf("the value of the tag %q has at most %d characters", tag.Key, maxTagValueChars)}
		case strings.HasPrefix(strings.ToLower(tag.Key), "aws:"):
			return apiError{Status: http.StatusBadRequest, Code: "invalid_tags", Message: fmt.Sprintf("the tag key %q starts with the reserved prefix aws:", tag.Key)}
		case !validTagText(tag.Key) || !validTagText(tag.Value):
			return apiError{Status: http.StatusBadRequest, Code: "invalid_tags", Message: fmt.Sprintf("the tag %q has a character that S3 does not allow: use letters, digits, spaces and + - = . _ : / @", tag.Key)}
		case seen[tag.Key]:
			return apiError{Status: http.StatusBadRequest, Code: "invalid_tags", Message: fmt.Sprintf("the tag key %q is twice in the request", tag.Key)}
		}
		seen[tag.Key] = true
	}
	return nil
}

// applyTagChange gives the tags of an object after the change. The result is sorted by key.
func applyTagChange(current []objectTag, change tagChange) []objectTag {
	byKey := make(map[string]string, len(current)+len(change.Add))
	for _, tag := range current {
		byKey[tag.Key] = tag.Value
	}
	for _, key := range change.Remove {
		delete(byKey, key)
	}
	for _, tag := range change.Add {
		byKey[tag.Key] = tag.Value
	}
	result := make([]objectTag, 0, len(byKey))
	for key, value := range byKey {
		result = append(result, objectTag{Key: key, Value: value})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Key < result[j].Key })
	return result
}

func sameTags(left, right []objectTag) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

type uploadTagsKey struct{}

// withUploadTags makes the next upload request of the context carry the tags, in the header x-amz-tagging.
func withUploadTags(ctx context.Context, tags []objectTag) context.Context {
	if len(tags) == 0 {
		return ctx
	}
	values := make(url.Values, len(tags))
	for _, tag := range tags {
		values.Set(tag.Key, tag.Value)
	}
	return context.WithValue(ctx, uploadTagsKey{}, values.Encode())
}

func setUploadTagsRequest(ctx context.Context, headers http.Header) {
	if encoded, _ := ctx.Value(uploadTagsKey{}).(string); encoded != "" {
		headers.Set("x-amz-tagging", encoded)
	}
}

// parseTaggingHeader reads the header x-amz-tagging: the tags as a query string (a=b&c=d).
func parseTaggingHeader(header http.Header) ([]objectTag, error) {
	raw := strings.TrimSpace(header.Get("X-Amz-Tagging"))
	if raw == "" {
		return nil, nil
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return nil, apiError{Status: http.StatusBadRequest, Code: "invalid_tags", Message: "X-Amz-Tagging must be a query string such as a=b&c=d"}
	}
	tags := make([]objectTag, 0, len(values))
	for key, list := range values {
		tags = append(tags, objectTag{Key: key, Value: strings.Join(list, "")})
	}
	sort.Slice(tags, func(i, j int) bool { return tags[i].Key < tags[j].Key })
	if err := validateTags(tags); err != nil {
		return nil, err
	}
	return tags, nil
}

// --- S3 backend -----------------------------------------------------------------------------------------------------

func (s *s3Backend) GetObjectTagging(ctx context.Context, key, version string) ([]objectTag, error) {
	query := url.Values{}
	query.Set("tagging", "")
	if version != "" {
		query.Set("versionId", version)
	}
	resp, err := s.do(ctx, http.MethodGet, key, query, nil, 0, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, s3ResponseError(resp)
	}
	var result struct {
		Tags []struct {
			Key   string `xml:"Key"`
			Value string `xml:"Value"`
		} `xml:"TagSet>Tag"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode s3 tagging response: %w", err)
	}
	tags := make([]objectTag, 0, len(result.Tags))
	for _, tag := range result.Tags {
		tags = append(tags, objectTag{Key: tag.Key, Value: tag.Value})
	}
	sort.Slice(tags, func(i, j int) bool { return tags[i].Key < tags[j].Key })
	return tags, nil
}

func (s *s3Backend) PutObjectTagging(ctx context.Context, key, version string, tags []objectTag) error {
	query := url.Values{}
	query.Set("tagging", "")
	if version != "" {
		query.Set("versionId", version)
	}
	if len(tags) == 0 {
		resp, err := s.do(ctx, http.MethodDelete, key, query, nil, 0, nil)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return s3ResponseError(resp)
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	type tagXML struct {
		Key   string `xml:"Key"`
		Value string `xml:"Value"`
	}
	payload := struct {
		XMLName xml.Name `xml:"Tagging"`
		Tags    []tagXML `xml:"TagSet>Tag"`
	}{Tags: make([]tagXML, 0, len(tags))}
	for _, tag := range tags {
		payload.Tags = append(payload.Tags, tagXML{Key: tag.Key, Value: tag.Value})
	}
	body, err := xml.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode s3 tagging request: %w", err)
	}
	sum := md5.Sum(body)
	headers := make(http.Header)
	headers.Set("Content-Type", "application/xml")
	headers.Set("Content-MD5", base64.StdEncoding.EncodeToString(sum[:]))
	resp, err := s.do(ctx, http.MethodPut, key, query, strings.NewReader(string(body)), int64(len(body)), headers)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return s3ResponseError(resp)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return nil
}

// GetBucketLifecycle reads the lifecycle rules. A bucket without rules gives an empty list.
func (s *s3Backend) GetBucketLifecycle(ctx context.Context) ([]lifecycleRule, error) {
	document, err := s.ReadBucketLifecycle(ctx)
	if err != nil {
		return nil, err
	}
	return parseLifecycleRules(document.Body)
}

// parseLifecycleRules reads the rules of a configuration. An empty document gives an empty list.
func parseLifecycleRules(body []byte) ([]lifecycleRule, error) {
	if len(strings.TrimSpace(string(body))) == 0 {
		return []lifecycleRule{}, nil
	}
	type tagXML struct {
		Key   string `xml:"Key"`
		Value string `xml:"Value"`
	}
	var result struct {
		Rules []struct {
			ID     string `xml:"ID"`
			Status string `xml:"Status"`
			Prefix string `xml:"Prefix"`
			Filter struct {
				Prefix          string  `xml:"Prefix"`
				Tag             *tagXML `xml:"Tag"`
				SizeGreaterThan int64   `xml:"ObjectSizeGreaterThan"`
				SizeLessThan    int64   `xml:"ObjectSizeLessThan"`
				And             *struct {
					Prefix          string   `xml:"Prefix"`
					Tags            []tagXML `xml:"Tag"`
					SizeGreaterThan int64    `xml:"ObjectSizeGreaterThan"`
					SizeLessThan    int64    `xml:"ObjectSizeLessThan"`
				} `xml:"And"`
			} `xml:"Filter"`
			Expiration struct {
				Days                      int    `xml:"Days"`
				Date                      string `xml:"Date"`
				ExpiredObjectDeleteMarker bool   `xml:"ExpiredObjectDeleteMarker"`
			} `xml:"Expiration"`
			Transitions []struct {
				Days         int    `xml:"Days"`
				Date         string `xml:"Date"`
				StorageClass string `xml:"StorageClass"`
			} `xml:"Transition"`
			NoncurrentExpiration struct {
				Days int `xml:"NoncurrentDays"`
			} `xml:"NoncurrentVersionExpiration"`
			AbortMultipart struct {
				Days int `xml:"DaysAfterInitiation"`
			} `xml:"AbortIncompleteMultipartUpload"`
		} `xml:"Rule"`
	}
	if err := xml.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode s3 lifecycle response: %w", err)
	}
	rules := make([]lifecycleRule, 0, len(result.Rules))
	for _, item := range result.Rules {
		rule := lifecycleRule{
			ID:                       item.ID,
			Enabled:                  item.Status == "Enabled",
			Prefix:                   item.Prefix,
			ExpirationDays:           item.Expiration.Days,
			ExpirationDate:           item.Expiration.Date,
			ExpiredObjectDeleteMark:  item.Expiration.ExpiredObjectDeleteMarker,
			NoncurrentExpirationDays: item.NoncurrentExpiration.Days,
			AbortMultipartDays:       item.AbortMultipart.Days,
			SizeGreaterThan:          item.Filter.SizeGreaterThan,
			SizeLessThan:             item.Filter.SizeLessThan,
		}
		if item.Filter.Prefix != "" {
			rule.Prefix = item.Filter.Prefix
		}
		if item.Filter.Tag != nil {
			rule.Tags = append(rule.Tags, objectTag{Key: item.Filter.Tag.Key, Value: item.Filter.Tag.Value})
		}
		if and := item.Filter.And; and != nil {
			if and.Prefix != "" {
				rule.Prefix = and.Prefix
			}
			for _, tag := range and.Tags {
				rule.Tags = append(rule.Tags, objectTag{Key: tag.Key, Value: tag.Value})
			}
			if and.SizeGreaterThan > 0 {
				rule.SizeGreaterThan = and.SizeGreaterThan
			}
			if and.SizeLessThan > 0 {
				rule.SizeLessThan = and.SizeLessThan
			}
		}
		for _, transition := range item.Transitions {
			rule.Transitions = append(rule.Transitions, lifecycleTransition{Days: transition.Days, Date: transition.Date, StorageClass: transition.StorageClass})
		}
		rules = append(rules, rule)
	}
	return rules, nil
}

// --- Storage instance -----------------------------------------------------------------------------------------------

func taggingUnsupported() error {
	return apiError{Status: http.StatusNotImplemented, Code: "tagging_unsupported", Message: "this storage provider does not support object tags"}
}

// tagsAvailable tells if the instance keeps object tags. A provider that does not know the tag requests, such as Garage,
// answers 501 to them, or accepts the header x-amz-tagging and drops the tags without a word: the page must know it before
// an upload. Until the probe has run, the answer is yes.
func (s *storageInstance) tagsAvailable() bool {
	if s == nil {
		return false
	}
	if _, ok := s.backend.(ObjectTagger); !ok {
		return false
	}
	return !s.tagsOff.Load()
}

// probeTags reads the tags of an object that does not exist. A provider with tags answers "no such key". A provider
// without tags answers 501 or 405. Any other failure (denied, timeout) leaves the answer at yes.
func (s *storageInstance) probeTags(ctx context.Context) {
	tagger, ok := s.backend.(ObjectTagger)
	if !ok {
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := tagger.GetObjectTagging(probeCtx, s.fullKey(".stowr-tags-probe"), "")
	if providerRefusedFeature(err) {
		s.tagsOff.Store(true)
	}
}

// requireTags refuses tags for an instance that does not keep them: the provider would drop them without a word.
func (s *storageInstance) requireTags(tags []objectTag) error {
	if len(tags) > 0 && !s.tagsAvailable() {
		return taggingUnsupported()
	}
	return nil
}

// providerRefusedFeature reports a provider that does not know a request: it answers 501 or 405.
func providerRefusedFeature(err error) bool {
	return batchDeleteUnsupported(err)
}

// tagVersion gives the version that a tag request names. A bucket with versioning is asked for the version of the
// object that a HEAD request reports, "null" for an object made before the versioning. A provider such as SeaweedFS
// takes more than ten seconds to read the tags of such an object when no version is named. Other buckets get "".
func (s *storageInstance) tagVersion(ctx context.Context, key, version string) (string, error) {
	if version != "" || !s.versioningAvailable() {
		return version, nil
	}
	response, err := s.Head(ctx, key)
	if err != nil {
		return "", err
	}
	defer closeObjectResponse(response)
	return strings.TrimSpace(response.Header.Get("x-amz-version-id")), nil
}

func (s *storageInstance) GetTags(ctx context.Context, key, version string) ([]objectTag, error) {
	tagger, ok := s.backend.(ObjectTagger)
	if !ok || s.tagsOff.Load() {
		return nil, taggingUnsupported()
	}
	version, err := s.tagVersion(ctx, key, version)
	if err != nil {
		return nil, err
	}
	release, err := s.beginStorageRequest(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	tags, err := tagger.GetObjectTagging(ctx, key, version)
	if providerRefusedFeature(err) {
		return nil, taggingUnsupported()
	}
	s.observePermission(permissionRead, err)
	return tags, err
}

func (s *storageInstance) PutTags(ctx context.Context, key, version string, tags []objectTag) error {
	tagger, ok := s.backend.(ObjectTagger)
	if !ok || s.tagsOff.Load() {
		return taggingUnsupported()
	}
	version, err := s.tagVersion(ctx, key, version)
	if err != nil {
		return err
	}
	release, err := s.beginStorageRequest(ctx)
	if err != nil {
		return err
	}
	defer release()
	err = tagger.PutObjectTagging(ctx, key, version, tags)
	if providerRefusedFeature(err) {
		return taggingUnsupported()
	}
	s.observePermission(permissionWrite, err)
	return err
}

func (s *storageInstance) Lifecycle(ctx context.Context) ([]lifecycleRule, error) {
	reader, ok := s.backend.(BucketLifecycleReader)
	if !ok || s.lifecycle.off.Load() {
		return nil, lifecycleUnsupported()
	}
	release, err := s.beginStorageRequest(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	rules, err := reader.GetBucketLifecycle(ctx)
	if providerRefusedFeature(err) {
		s.lifecycle.off.Store(true)
		return nil, lifecycleUnsupported()
	}
	return rules, err
}

// --- HTTP -----------------------------------------------------------------------------------------------------------

type tagsRequest struct {
	Instance string      `json:"instance"`
	Key      string      `json:"key"`
	Version  string      `json:"version"`
	Tags     []objectTag `json:"tags"`
}

type tagsResponse struct {
	Instance string      `json:"instance"`
	Key      string      `json:"key"`
	Version  string      `json:"version,omitempty"`
	Tags     []objectTag `json:"tags"`
	MaxTags  int         `json:"maxTags"`
}

func (a *application) handleTags(w http.ResponseWriter, r *http.Request) {
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
		version := strings.TrimSpace(r.URL.Query().Get("version"))
		tags, err := instance.GetTags(r.Context(), instance.fullKey(key), version)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, tagsResponse{Instance: instance.cfg.ID, Key: key, Version: version, Tags: tags, MaxTags: maxObjectTags})
	case http.MethodPut:
		var request tagsRequest
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
		if err := validateTags(request.Tags); err != nil {
			writeAPIError(w, err)
			return
		}
		tags := applyTagChange(nil, tagChange{Add: request.Tags})
		version := strings.TrimSpace(request.Version)
		if err := instance.PutTags(r.Context(), instance.fullKey(key), version, tags); err != nil {
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, tagsResponse{Instance: instance.cfg.ID, Key: key, Version: version, Tags: tags, MaxTags: maxObjectTags})
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPut)
	}
}

type tagsBulkRequest struct {
	Instance string      `json:"instance"`
	Prefix   string      `json:"prefix"`
	Add      []objectTag `json:"add"`
	Remove   []string    `json:"remove"`
}

// handleTagsBulk starts the job that changes the tags of every object under a prefix.
func (a *application) handleTagsBulk(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var request tagsBulkRequest
	if err := decodeJSONBody(r, &request); err != nil {
		writeAPIError(w, err)
		return
	}
	instance, err := a.instanceFromRequest(r, request.Instance)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	prefix := cleanRelativeKey(request.Prefix)
	if prefix != "" {
		prefix = normalizePrefix(prefix)
	}
	for _, permission := range []string{permissionRead, permissionWrite} {
		if err := requirePermission(instance, permission); err != nil {
			writeAPIError(w, err)
			return
		}
	}
	if len(request.Add) == 0 && len(request.Remove) == 0 {
		writeAPIError(w, apiError{Status: http.StatusBadRequest, Code: "invalid_tags", Message: "give at least one tag to add or one key to remove"})
		return
	}
	if err := validateTags(request.Add); err != nil {
		writeAPIError(w, err)
		return
	}
	for _, key := range request.Remove {
		if key == "" {
			writeAPIError(w, apiError{Status: http.StatusBadRequest, Code: "invalid_tags", Message: "a key to remove cannot be empty"})
			return
		}
	}
	job, err := a.jobs.create(jobState{
		Type: jobTypeTagPrefix, Instance: instance.cfg.ID, Prefix: prefix,
		TagChange: &tagChange{Add: append([]objectTag(nil), request.Add...), Remove: append([]string(nil), request.Remove...)},
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, job.public())
}

// runTagJob changes the tags of each object under the prefix of the job. An object that already has the result is not
// written again.
func (m *jobManager) runTagJob(ctx context.Context, job *jobState, instance *storageInstance) error {
	if job.TagChange == nil {
		return apiError{Status: http.StatusBadRequest, Code: "invalid_tags", Message: "the tag job has no change"}
	}
	change := *job.TagChange
	return m.runObjectJob(ctx, job, instance, func(ctx context.Context, _ objectInfo, relative string) error {
		fullKey := instance.fullKey(relative)
		version, err := instance.tagVersion(ctx, fullKey, "")
		if err != nil {
			return err
		}
		current, err := instance.GetTags(ctx, fullKey, version)
		if err != nil {
			return err
		}
		next := applyTagChange(current, change)
		if sameTags(current, next) {
			return nil
		}
		if err := validateTags(next); err != nil {
			return apiError{Status: http.StatusBadRequest, Code: "invalid_tags", Message: fmt.Sprintf("%s: %s", relative, err.(apiError).Message)}
		}
		return instance.PutTags(ctx, fullKey, version, next)
	})
}

type lifecycleResponse struct {
	Instance string          `json:"instance"`
	Rules    []lifecycleRule `json:"rules"`
	// Editor is the state of the editor (lifecycle.go). It is absent for a backend that only reads the rules.
	Editor *lifecycleEditorState `json:"editor,omitempty"`
	// Warnings tells what the provider changed in the rules of a save (PUT only).
	Warnings []string `json:"warnings,omitempty"`
}

// handleLifecycle gives the rules of the bucket (GET) with the state of the editor, or saves the rules of the editor
// (PUT, lifecycle.go).
func (a *application) handleLifecycle(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
	case http.MethodPut:
		a.handleLifecycleSave(w, r)
		return
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPut)
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
	if _, ok := instance.backend.(BucketLifecycleWriter); ok {
		state, err := instance.readLifecycleState(r.Context())
		if err != nil {
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, lifecycleResponse{Instance: instance.cfg.ID, Rules: state.rules, Editor: &state.editor})
		return
	}
	rules, err := instance.Lifecycle(r.Context())
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, lifecycleResponse{Instance: instance.cfg.ID, Rules: rules})
}
