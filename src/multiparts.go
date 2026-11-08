package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The cleanup of the incomplete multipart uploads of a bucket: S3 ListMultipartUploads, ListParts and
// AbortMultipartUpload. The list never sends a prefix to the provider: MinIO gives an empty list for a prefix that is not
// a full key. Stowr keeps the uploads under the root prefix of the instance and under the prefix of the request.
// Each page of the provider continues with the markers that the provider sent: SeaweedFS sends NextUploadIdMarker only,
// Garage and AWS send both markers, and MinIO sends all the uploads in one page. The age of an upload comes from its
// Initiated date. SeaweedFS sends no such date: the age is unknown, and the abort by age never aborts such an upload.
// An abort by age never aborts an upload younger than minimumMultipartAbortAge. No abort touches an upload that the
// resumable-upload store of this process uses. Only the S3 backend has the cleanup: the GCS backend answers 501.

const (
	jobTypeAbortMultiparts = "abort_multiparts"

	// defaultMultipartListSize and maximumMultipartListSize are the number of uploads in one answer of the list.
	defaultMultipartListSize = 100
	maximumMultipartListSize = 500
	// multipartListPages is the limit of provider pages that one answer of the list reads.
	multipartListPages = 10
	// multipartScanPageSize is the max-uploads of each provider page that the abort job reads.
	multipartScanPageSize = 1000
	// multipartPartsPageSize is the max-parts of the one ListParts request for each listed upload. An upload with
	// more parts shows "at least" this count and the size of these parts.
	multipartPartsPageSize = 1000
	// multipartPartsWorkers is the number of ListParts requests of one answer at the same time.
	multipartPartsWorkers = 4
	// maximumMultipartAbortItems is the limit of uploads in one request of "Abort selected".
	maximumMultipartAbortItems = 1000
	// maximumMultipartAbortDays is the largest age in days that the abort by age accepts.
	maximumMultipartAbortDays = 36500
	// minimumMultipartAbortAge is the youngest upload that the abort by age can abort.
	minimumMultipartAbortAge = 24 * time.Hour
)

// MultipartUploadLister is optional: a backend that lists the incomplete multipart uploads of its bucket, and the
// first page of the parts of one upload.
type MultipartUploadLister interface {
	ListMultipartUploads(ctx context.Context, cursor multipartCursor, maximum int) (multipartUploadPage, error)
	MultipartUploadParts(ctx context.Context, key, uploadID string, maximum int) (multipartPartsSummary, error)
}

// IncompleteUploadRemover is optional: a backend without S3 multipart uploads that removes one incomplete upload of its
// list. A volume removes the temporary file of the upload.
type IncompleteUploadRemover interface {
	RemoveIncompleteUpload(ctx context.Context, key, uploadID string) error
}

// multipartUpload is one incomplete upload with the full key of the provider. Initiated is zero when the provider
// gives no date.
type multipartUpload struct {
	Key       string
	UploadID  string
	Initiated time.Time
}

// multipartCursor is the place of a page in the list of the provider: the markers that the provider sent, and the
// position in the page from which the answer continues (a provider that ignores max-uploads sends more).
type multipartCursor struct {
	KeyMarker      string `json:"k,omitempty"`
	UploadIDMarker string `json:"u,omitempty"`
	Skip           int    `json:"i,omitempty"`
}

type multipartUploadPage struct {
	Uploads   []multipartUpload
	Truncated bool
	Next      multipartCursor
}

// multipartPartsSummary is the count and the size of the parts of the first page. Truncated is true when the upload
// has more parts.
type multipartPartsSummary struct {
	Parts     int
	Bytes     int64
	Truncated bool
}

func multipartUnsupported() error {
	return apiError{Status: http.StatusNotImplemented, Code: "multipart_unsupported", Message: "this storage provider does not let Stowr list the incomplete multipart uploads"}
}

func invalidMultipartRequest(format string, values ...any) error {
	return apiError{Status: http.StatusBadRequest, Code: "invalid_multipart_request", Message: fmt.Sprintf(format, values...)}
}

func encodeMultipartCursor(cursor multipartCursor) string {
	if cursor == (multipartCursor{}) {
		return ""
	}
	data, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodeMultipartCursor(token string) (multipartCursor, error) {
	var cursor multipartCursor
	if token == "" {
		return cursor, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(data) > 4096 || json.Unmarshal(data, &cursor) != nil || cursor.Skip < 0 {
		return multipartCursor{}, apiError{Status: http.StatusBadRequest, Code: "invalid_page_token", Message: "the page token of the incomplete uploads is not valid: open the list again"}
	}
	return cursor, nil
}

// --- S3 backend -----------------------------------------------------------------------------------------------------

// ListMultipartUploads reads one page of the incomplete uploads of the bucket, without a prefix. The next cursor has
// the markers that the provider sent, as the provider sent them.
func (s *s3Backend) ListMultipartUploads(ctx context.Context, cursor multipartCursor, maximum int) (multipartUploadPage, error) {
	query := url.Values{}
	query.Set("uploads", "")
	if maximum > 0 {
		query.Set("max-uploads", strconv.Itoa(maximum))
	}
	if cursor.KeyMarker != "" {
		query.Set("key-marker", cursor.KeyMarker)
	}
	if cursor.UploadIDMarker != "" {
		query.Set("upload-id-marker", cursor.UploadIDMarker)
	}
	resp, err := s.do(ctx, http.MethodGet, "", query, nil, 0, nil)
	if err != nil {
		return multipartUploadPage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return multipartUploadPage{}, s3ResponseError(resp)
	}
	var result struct {
		IsTruncated        bool   `xml:"IsTruncated"`
		NextKeyMarker      string `xml:"NextKeyMarker"`
		NextUploadIDMarker string `xml:"NextUploadIdMarker"`
		Uploads            []struct {
			Key       string `xml:"Key"`
			UploadID  string `xml:"UploadId"`
			Initiated string `xml:"Initiated"`
		} `xml:"Upload"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&result); err != nil {
		return multipartUploadPage{}, fmt.Errorf("decode s3 multipart uploads response: %w", err)
	}
	page := multipartUploadPage{
		Truncated: result.IsTruncated,
		Next:      multipartCursor{KeyMarker: result.NextKeyMarker, UploadIDMarker: result.NextUploadIDMarker},
		Uploads:   make([]multipartUpload, 0, len(result.Uploads)),
	}
	for _, item := range result.Uploads {
		initiated, _ := time.Parse(time.RFC3339Nano, strings.TrimSpace(item.Initiated))
		page.Uploads = append(page.Uploads, multipartUpload{Key: item.Key, UploadID: item.UploadID, Initiated: initiated.UTC()})
	}
	return page, nil
}

// MultipartUploadParts reads the first page of the parts of one upload: up to maximum parts.
func (s *s3Backend) MultipartUploadParts(ctx context.Context, key, uploadID string, maximum int) (multipartPartsSummary, error) {
	query := url.Values{}
	query.Set("uploadId", uploadID)
	query.Set("max-parts", strconv.Itoa(maximum))
	resp, err := s.do(ctx, http.MethodGet, key, query, nil, 0, nil)
	if err != nil {
		return multipartPartsSummary{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return multipartPartsSummary{}, s3ResponseError(resp)
	}
	var result struct {
		IsTruncated bool `xml:"IsTruncated"`
		Parts       []struct {
			Size int64 `xml:"Size"`
		} `xml:"Part"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&result); err != nil {
		return multipartPartsSummary{}, fmt.Errorf("decode s3 multipart parts response: %w", err)
	}
	summary := multipartPartsSummary{Parts: len(result.Parts), Truncated: result.IsTruncated}
	for _, part := range result.Parts {
		summary.Bytes += part.Size
	}
	return summary, nil
}

// --- Storage instance -----------------------------------------------------------------------------------------------

// multipartUploadsAvailable tells if the instance can list the incomplete uploads. Until the probe has run, the answer
// is yes for a backend with the list.
func (s *storageInstance) multipartUploadsAvailable() bool {
	if s == nil {
		return false
	}
	if _, ok := s.backend.(MultipartUploadLister); !ok {
		return false
	}
	return !s.multipartsOff.Load()
}

// probeMultipartUploads asks for one incomplete upload. A provider without the list answers 501 or 405. Any other
// failure (denied, timeout) leaves the answer at yes. An instance without the read permission is not probed.
func (s *storageInstance) probeMultipartUploads(ctx context.Context) {
	lister, ok := s.backend.(MultipartUploadLister)
	// A volume always lists its temporary files, and the probe would read its tree at start.
	if !ok || !s.permitted(permissionRead) || s.cfg.Provider == providerVolume {
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := lister.ListMultipartUploads(probeCtx, multipartCursor{}, 1)
	if providerRefusedFeature(err) {
		s.multipartsOff.Store(true)
	}
}

func (s *storageInstance) multipartLister() (MultipartUploadLister, error) {
	lister, ok := s.backend.(MultipartUploadLister)
	if !ok || s.multipartsOff.Load() {
		return nil, multipartUnsupported()
	}
	return lister, nil
}

// ListMultipartUploads reads one page of the provider. A provider that refuses the request turns the list off.
func (s *storageInstance) ListMultipartUploads(ctx context.Context, cursor multipartCursor, maximum int) (multipartUploadPage, error) {
	lister, err := s.multipartLister()
	if err != nil {
		return multipartUploadPage{}, err
	}
	release, err := s.beginStorageRequest(ctx)
	if err != nil {
		return multipartUploadPage{}, err
	}
	defer release()
	page, err := lister.ListMultipartUploads(ctx, multipartCursor{KeyMarker: cursor.KeyMarker, UploadIDMarker: cursor.UploadIDMarker}, maximum)
	if providerRefusedFeature(err) {
		s.multipartsOff.Store(true)
		return multipartUploadPage{}, multipartUnsupported()
	}
	s.observePermission(permissionRead, err)
	return page, err
}

func (s *storageInstance) MultipartUploadParts(ctx context.Context, key, uploadID string) (multipartPartsSummary, error) {
	lister, err := s.multipartLister()
	if err != nil {
		return multipartPartsSummary{}, err
	}
	release, err := s.beginStorageRequest(ctx)
	if err != nil {
		return multipartPartsSummary{}, err
	}
	defer release()
	summary, err := lister.MultipartUploadParts(ctx, key, uploadID, multipartPartsPageSize)
	s.observePermission(permissionRead, err)
	return summary, err
}

// nextMultipartPage reads the page of the cursor and checks that the provider moves its markers forward.
func (s *storageInstance) nextMultipartPage(ctx context.Context, cursor multipartCursor, maximum int) (multipartUploadPage, error) {
	page, err := s.ListMultipartUploads(ctx, cursor, maximum)
	if err != nil {
		return page, err
	}
	if page.Truncated && (page.Next == (multipartCursor{}) ||
		(page.Next.KeyMarker == cursor.KeyMarker && page.Next.UploadIDMarker == cursor.UploadIDMarker)) {
		return page, apiError{Status: http.StatusBadGateway, Code: "provider_paging", Message: "the storage provider sent the same page of incomplete uploads again: Stowr stops the list"}
	}
	return page, nil
}

// multipartRelativeKey gives the key of an upload relative to the root prefix of the instance, and false for an upload
// outside the root prefix or outside prefix.
func (s *storageInstance) multipartRelativeKey(upload multipartUpload, prefix string) (string, bool) {
	relative, ok := s.relativeKey(upload.Key)
	if !ok || relative == "" || !strings.HasPrefix(relative, prefix) {
		return "", false
	}
	return relative, true
}

// --- Resumable-upload store -----------------------------------------------------------------------------------------

// activeProviderUploads gives the provider upload ids of the sessions of the instance that this process still sends.
func (m *uploadManager) activeProviderUploads(instance string) map[string]bool {
	active := make(map[string]bool)
	if m == nil {
		return active
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, upload := range m.uploads {
		if upload.Instance != instance || upload.Status != uploadStatusUploading {
			continue
		}
		switch {
		case upload.ProviderUploadID != "":
			active[upload.ProviderUploadID] = true
		case upload.Provider == providerVolume && upload.SessionURL != "":
			// The list of a volume gives the name of the temporary file as the upload id.
			active[volumeSessionUploadID(upload.SessionURL)] = true
		}
	}
	return active
}

// --- List -----------------------------------------------------------------------------------------------------------

type multipartUploadJSON struct {
	Key       string `json:"key"`
	UploadID  string `json:"uploadId"`
	Initiated string `json:"initiated,omitempty"`
	// Parts and Bytes count the first page of the parts. PartsAtLeast is true when the upload has more parts.
	// PartsUnknown is true when the provider did not give the parts.
	Parts        int   `json:"parts"`
	Bytes        int64 `json:"bytes"`
	PartsAtLeast bool  `json:"partsAtLeast,omitempty"`
	PartsUnknown bool  `json:"partsUnknown,omitempty"`
	// Active is true when the resumable-upload store of this process sends this upload now. No abort touches it.
	Active bool `json:"active,omitempty"`
}

type multipartListResponse struct {
	Instance  string                `json:"instance"`
	Prefix    string                `json:"prefix"`
	Uploads   []multipartUploadJSON `json:"uploads"`
	NextToken string                `json:"nextToken,omitempty"`
	// Now is the time of the server: the page gives the age of each upload from it.
	Now string `json:"now"`
	// MinimumAbortAgeHours is the youngest upload that the abort by age can abort.
	MinimumAbortAgeHours int `json:"minimumAbortAgeHours"`
	PartsPageSize        int `json:"partsPageSize"`
}

// listMultipartUploads collects up to limit uploads under prefix from the cursor. It reads at most multipartListPages
// pages of the provider, and gives the cursor of the next answer, or an empty cursor at the end of the list.
func listMultipartUploads(ctx context.Context, instance *storageInstance, prefix string, cursor multipartCursor, limit int) ([]multipartUpload, []string, multipartCursor, error) {
	uploads := make([]multipartUpload, 0, limit)
	relatives := make([]string, 0, limit)
	for pages := 0; pages < multipartListPages; pages++ {
		page, err := instance.nextMultipartPage(ctx, cursor, min(limit-len(uploads)+cursor.Skip, multipartScanPageSize))
		if err != nil {
			return nil, nil, multipartCursor{}, err
		}
		for index := cursor.Skip; index < len(page.Uploads); index++ {
			if len(uploads) == limit {
				// The provider sent more than the request asked: the next answer starts at this position of the page.
				return uploads, relatives, multipartCursor{KeyMarker: cursor.KeyMarker, UploadIDMarker: cursor.UploadIDMarker, Skip: index}, nil
			}
			upload := page.Uploads[index]
			if relative, ok := instance.multipartRelativeKey(upload, prefix); ok {
				uploads = append(uploads, upload)
				relatives = append(relatives, relative)
			}
		}
		if !page.Truncated {
			return uploads, relatives, multipartCursor{}, nil
		}
		cursor = page.Next
		if len(uploads) == limit {
			return uploads, relatives, cursor, nil
		}
	}
	return uploads, relatives, cursor, nil
}

// describeMultipartUploads reads the first page of the parts of each upload, multipartPartsWorkers at a time. An
// upload that the provider no longer knows (completed or aborted since the list) is left out.
func describeMultipartUploads(ctx context.Context, instance *storageInstance, uploads []multipartUpload, relatives []string, active map[string]bool) []multipartUploadJSON {
	described := make([]multipartUploadJSON, len(uploads))
	gone := make([]bool, len(uploads))
	work := make(chan int)
	var wg sync.WaitGroup
	for worker := 0; worker < multipartPartsWorkers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range work {
				upload := uploads[index]
				item := multipartUploadJSON{Key: relatives[index], UploadID: upload.UploadID, Active: active[upload.UploadID]}
				if !upload.Initiated.IsZero() {
					item.Initiated = upload.Initiated.Format(time.RFC3339)
				}
				summary, err := instance.MultipartUploadParts(ctx, upload.Key, upload.UploadID)
				switch {
				case isNoSuchUpload(err):
					gone[index] = true
				case err != nil:
					item.PartsUnknown = true
				default:
					item.Parts, item.Bytes, item.PartsAtLeast = summary.Parts, summary.Bytes, summary.Truncated
				}
				described[index] = item
			}
		}()
	}
	for index := range uploads {
		work <- index
	}
	close(work)
	wg.Wait()
	out := make([]multipartUploadJSON, 0, len(described))
	for index, item := range described {
		if !gone[index] {
			out = append(out, item)
		}
	}
	return out
}

// isNoSuchUpload reports the 404 of an upload that the provider does not know.
func isNoSuchUpload(err error) bool {
	var upstream *upstreamError
	return errors.As(err, &upstream) && (upstream.StatusCode == http.StatusNotFound || upstream.Code == "NoSuchUpload")
}

// --- Abort ----------------------------------------------------------------------------------------------------------

type multipartAbortItem struct {
	Key      string `json:"key"`
	UploadID string `json:"uploadId"`
}

type multipartAbortRequest struct {
	Instance string `json:"instance"`
	// Uploads are the uploads of "Abort selected". The request gives these or OlderThanDays, not both.
	Uploads []multipartAbortItem `json:"uploads"`
	// OlderThanDays starts the job that aborts each dated upload under Prefix that is older than this number of days.
	OlderThanDays int    `json:"olderThanDays"`
	Prefix        string `json:"prefix"`
}

type multipartAbortFailure struct {
	Key      string `json:"key"`
	UploadID string `json:"uploadId"`
	Reason   string `json:"reason"`
}

type multipartAbortResponse struct {
	Aborted []multipartAbortItem    `json:"aborted"`
	Skipped []multipartAbortFailure `json:"skipped"`
	Failed  []multipartAbortFailure `json:"failed"`
}

// multipartAbortJob is the request of an abort_multiparts job. Cutoff is fixed at the creation of the job, so a resumed
// job aborts the same uploads.
type multipartAbortJob struct {
	OlderThanDays int       `json:"older_than_days"`
	Cutoff        time.Time `json:"cutoff"`
}

// multipartAbortResult counts the uploads of an abort_multiparts job.
type multipartAbortResult struct {
	Aborted int64 `json:"aborted"`
	// SkippedUndated counts the uploads without a start date: the provider gives none.
	SkippedUndated int64 `json:"skippedUndated"`
	// SkippedRecent counts the uploads that are not older than the age of the job.
	SkippedRecent int64 `json:"skippedRecent"`
	// SkippedActive counts the uploads that the resumable-upload store of this process sends now.
	SkippedActive int64 `json:"skippedActive"`
	Failed        int64 `json:"failed"`
}

const activeUploadReason = "this Stowr process sends this upload now"

func validMultipartUploadID(id string) bool {
	return id != "" && len(id) <= 1024 && printableASCII(id)
}

// abortSelectedMultiparts aborts each listed upload, at any age. It skips an upload that this process sends now. An
// upload that the provider no longer knows counts as aborted.
func (a *application) abortSelectedMultiparts(ctx context.Context, instance *storageInstance, items []multipartAbortItem) multipartAbortResponse {
	response := multipartAbortResponse{Aborted: []multipartAbortItem{}, Skipped: []multipartAbortFailure{}, Failed: []multipartAbortFailure{}}
	active := a.uploads.activeProviderUploads(instance.cfg.ID)
	for _, item := range items {
		if active[item.UploadID] {
			response.Skipped = append(response.Skipped, multipartAbortFailure{Key: item.Key, UploadID: item.UploadID, Reason: activeUploadReason})
			continue
		}
		if err := instance.AbortMultipart(ctx, instance.fullKey(item.Key), item.UploadID); err != nil {
			response.Failed = append(response.Failed, multipartAbortFailure{Key: item.Key, UploadID: item.UploadID, Reason: publicJobError(err)})
			continue
		}
		response.Aborted = append(response.Aborted, item)
	}
	return response
}

// runAbortMultipartsJob reads the whole list of the provider and aborts each upload under the prefix of the job that
// started before the cutoff of the job. An upload without a date, an upload younger than minimumMultipartAbortAge and
// an upload that this process sends stay. A resumed job reads the list again from the start: the aborted uploads are
// gone, and the counts of the skipped uploads start again from zero.
func (m *jobManager) runAbortMultipartsJob(ctx context.Context, job *jobState, instance *storageInstance) error {
	if job.MultipartAbort == nil || job.MultipartAbort.OlderThanDays < 1 {
		return invalidMultipartRequest("the job has no age")
	}
	if job.MultipartResult == nil {
		job.MultipartResult = &multipartAbortResult{}
	}
	result := job.MultipartResult
	result.SkippedUndated, result.SkippedRecent, result.SkippedActive, result.Failed = 0, 0, 0, 0
	job.Processed = 0
	cutoff := job.MultipartAbort.Cutoff
	cursor := multipartCursor{}
	for {
		if err := m.controlState(job.ID); err != nil {
			return err
		}
		page, err := instance.nextMultipartPage(ctx, cursor, multipartScanPageSize)
		if err != nil {
			return err
		}
		active := m.app.uploads.activeProviderUploads(instance.cfg.ID)
		for _, upload := range page.Uploads {
			relative, ok := instance.multipartRelativeKey(upload, job.Prefix)
			if !ok {
				continue
			}
			if err := m.controlState(job.ID); err != nil {
				return err
			}
			job.Processed++
			now := time.Now().UTC()
			switch {
			case upload.Initiated.IsZero():
				result.SkippedUndated++
			case upload.Initiated.After(cutoff) || now.Sub(upload.Initiated) < minimumMultipartAbortAge:
				result.SkippedRecent++
			case active[upload.UploadID]:
				result.SkippedActive++
			default:
				if err := instance.AbortMultipart(ctx, upload.Key, upload.UploadID); err != nil {
					if ctx.Err() != nil {
						return err
					}
					result.Failed++
				} else {
					result.Aborted++
					job.LastKey = relative
				}
			}
			job.UpdatedAt = time.Now().UTC()
		}
		if latest, ok := m.get(job.ID); ok {
			job.Status = latest.Status
			job.EndedAt = cloneTimePointer(latest.EndedAt)
			if job.Status == jobStatusQueued {
				job.Status = jobStatusRunning
			}
		}
		if err := m.put(*job); err != nil {
			return err
		}
		switch job.Status {
		case jobStatusPaused:
			return errJobPaused
		case jobStatusCanceled:
			return errJobCanceled
		}
		if !page.Truncated {
			return nil
		}
		cursor = page.Next
	}
}

// --- HTTP -----------------------------------------------------------------------------------------------------------

// handleMultiparts lists the incomplete uploads under a prefix (GET /api/multiparts).
func (a *application) handleMultiparts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	query := r.URL.Query()
	prefix := cleanRelativeKey(query.Get("prefix"))
	instance, err := a.instanceFromRequest(r, "")
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if err := requirePermission(instance, permissionRead); err != nil {
		writeAPIError(w, err)
		return
	}
	if _, err := a.authorizeRead(r, instance, prefix); err != nil {
		writeAPIError(w, err)
		return
	}
	if !instance.multipartUploadsAvailable() {
		writeAPIError(w, multipartUnsupported())
		return
	}
	limit := defaultMultipartListSize
	if raw := query.Get("max"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > maximumMultipartListSize {
			writeAPIError(w, invalidMultipartRequest("max must be an integer from 1 to %d, got %q", maximumMultipartListSize, raw))
			return
		}
		limit = value
	}
	cursor, err := decodeMultipartCursor(query.Get("token"))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	uploads, relatives, next, err := listMultipartUploads(r.Context(), instance, prefix, cursor, limit)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	active := a.uploads.activeProviderUploads(instance.cfg.ID)
	writeJSON(w, http.StatusOK, multipartListResponse{
		Instance: instance.cfg.ID, Prefix: prefix,
		Uploads:              describeMultipartUploads(r.Context(), instance, uploads, relatives, active),
		NextToken:            encodeMultipartCursor(next),
		Now:                  time.Now().UTC().Format(time.RFC3339),
		MinimumAbortAgeHours: int(minimumMultipartAbortAge / time.Hour),
		PartsPageSize:        multipartPartsPageSize,
	})
}

// handleMultipartAbort aborts the uploads of the request (200 with the result of each), or starts the job that aborts
// the uploads older than a number of days (202 with the job).
func (a *application) handleMultipartAbort(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var request multipartAbortRequest
	if err := decodeJSONBody(r, &request); err != nil {
		writeAPIError(w, err)
		return
	}
	instance, err := a.instanceFromRequest(r, request.Instance)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	switch {
	case len(request.Uploads) > 0 && request.OlderThanDays != 0:
		writeAPIError(w, invalidMultipartRequest("give uploads or olderThanDays, not both"))
		return
	case len(request.Uploads) == 0 && request.OlderThanDays == 0:
		writeAPIError(w, invalidMultipartRequest("give the uploads to abort, or olderThanDays"))
		return
	case len(request.Uploads) > maximumMultipartAbortItems:
		writeAPIError(w, invalidMultipartRequest("give at most %d uploads in one request, got %d", maximumMultipartAbortItems, len(request.Uploads)))
		return
	case request.OlderThanDays != 0 && (request.OlderThanDays < 1 || request.OlderThanDays > maximumMultipartAbortDays):
		writeAPIError(w, invalidMultipartRequest("olderThanDays must be an integer from 1 to %d, got %d", maximumMultipartAbortDays, request.OlderThanDays))
		return
	case len(request.Uploads) > 0 && request.Prefix != "":
		writeAPIError(w, invalidMultipartRequest("prefix goes with olderThanDays only"))
		return
	}
	for index, item := range request.Uploads {
		request.Uploads[index].Key = cleanRelativeKey(item.Key)
		if request.Uploads[index].Key == "" || !validMultipartUploadID(item.UploadID) {
			writeAPIError(w, invalidMultipartRequest("each upload needs a key and the upload id that the list gave"))
			return
		}
	}
	for _, permission := range []string{permissionRead, permissionWrite} {
		if err := requirePermission(instance, permission); err != nil {
			writeAPIError(w, err)
			return
		}
	}
	if _, err := instance.multipartLister(); err != nil {
		writeAPIError(w, err)
		return
	}
	if len(request.Uploads) > 0 {
		keys := make([]string, len(request.Uploads))
		for index, item := range request.Uploads {
			keys[index] = item.Key
		}
		if _, err := a.authorize(r, a.abortUploadsOperation(instance, keys)); err != nil {
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, a.abortSelectedMultiparts(r.Context(), instance, request.Uploads))
		return
	}
	prefix := cleanRelativeKey(request.Prefix)
	if _, err := a.authorize(r, a.writePrefixOperation(instance, prefix)); err != nil {
		writeAPIError(w, err)
		return
	}
	cutoff := time.Now().UTC().Add(-time.Duration(request.OlderThanDays) * 24 * time.Hour)
	job, err := a.jobs.create(jobState{
		Type: jobTypeAbortMultiparts, Instance: instance.cfg.ID, Prefix: prefix,
		MultipartAbort:  &multipartAbortJob{OlderThanDays: request.OlderThanDays, Cutoff: cutoff},
		MultipartResult: &multipartAbortResult{},
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, job.public())
}
