package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// Object Lock: the retention and the legal hold of one version of an object (GET and PUT /api/object-lock). A bucket
// has Object Lock only when it was made with it, and such a bucket always keeps versions. The probe at start reads the
// configuration of the bucket (GetObjectLockConfiguration). The feature shows only when the answer says "Enabled".
// "No configuration", 501 and 405 (Garage), and any other failure hide it.
//
// The read gives the mode and the date of the retention, the legal hold, and the default retention of the bucket. A
// change names the version. The server reads the lock of the version first and applies these rules:
//   - a version without an active retention gets any mode and any date in the future;
//   - an active Governance retention gets a later date in the same mode. Any other change (an earlier date, the mode
//     Compliance) needs the bypass of the Governance retention (x-amz-bypass-governance-retention: true), and the
//     request must ask for it;
//   - an active Compliance retention gets only a later date in the same mode: nobody can shorten it or remove it.
//
// The legal hold goes on and off with no rule.
//
// A delete that the lock refuses gets a clear error (object_locked). The providers refuse with different answers
// (measured, see test/README.md): SeaweedFS answers 403 AccessDenied, MinIO 400 InvalidRequest, and DeleteObjects gives
// the same codes for each key in a 200 answer. Thus the server does not trust the status alone: after such a refusal it
// reads the lock of the version, and only an active lock makes the error object_locked. Only the S3 backend has Object
// Lock. The GCS backend answers 501.

const (
	objectLockGovernance = "GOVERNANCE"
	objectLockCompliance = "COMPLIANCE"

	// maxObjectLockDays is the latest retain-until date that Stowr sets, in days from now.
	maxObjectLockDays = 36500
	// maxObjectLockVersionBytes is the limit of Stowr for the version id of a request.
	maxObjectLockVersionBytes = 1024
	// maxObjectLockDateChars is the limit of Stowr for the text of a retain-until date.
	maxObjectLockDateChars = 64
	// maxLockedKeys is the number of locked keys that a job keeps to show them.
	maxLockedKeys = 20
	// maxLockChecks is the largest number of refused keys of one batch delete for which the server reads the lock.
	maxLockChecks = 100
)

// objectRetention is the retention of one version: the mode and the retain-until date.
type objectRetention struct {
	Mode        string
	RetainUntil time.Time
}

// active reports a retention whose date is after now.
func (r *objectRetention) active(now time.Time) bool {
	return r != nil && r.RetainUntil.After(now)
}

// objectLockDefault is the default retention of a bucket: a mode and a period in days or in years.
type objectLockDefault struct {
	Mode  string
	Days  int
	Years int
}

// objectLockConfiguration is the configuration of a bucket. Enabled is false for a bucket without Object Lock.
type objectLockConfiguration struct {
	Enabled bool
	Default *objectLockDefault
}

// objectLockState is the lock of one version. Retention is nil for a version without a retention.
type objectLockState struct {
	Retention *objectRetention
	LegalHold bool
}

// locked reports a legal hold or an active retention.
func (s objectLockState) locked(now time.Time) bool {
	return s.LegalHold || s.Retention.active(now)
}

// ObjectLockManager is optional: a backend that reads and changes the Object Lock of its bucket and of the versions.
type ObjectLockManager interface {
	ObjectLockConfiguration(ctx context.Context) (objectLockConfiguration, error)
	ObjectRetention(ctx context.Context, key, version string) (*objectRetention, error)
	ObjectLegalHold(ctx context.Context, key, version string) (bool, error)
	PutObjectRetention(ctx context.Context, key, version string, retention objectRetention, bypassGovernance bool) error
	PutObjectLegalHold(ctx context.Context, key, version string, on bool) error
}

func objectLockUnsupported() error {
	return apiError{Status: http.StatusNotImplemented, Code: "object_lock_unsupported", Message: "this storage bucket has no Object Lock"}
}

func invalidObjectLock(format string, values ...any) error {
	return apiError{Status: http.StatusBadRequest, Code: "invalid_object_lock", Message: fmt.Sprintf(format, values...)}
}

// lockDate gives a date in the form of the messages, such as "2026-10-21 07:28:00 UTC".
func lockDate(value time.Time) string {
	return value.UTC().Format("2006-01-02 15:04:05 UTC")
}

// modeName gives the name of a mode for a person.
func modeName(mode string) string {
	switch mode {
	case objectLockGovernance:
		return "Governance"
	case objectLockCompliance:
		return "Compliance"
	}
	return mode
}

// --- S3 backend -----------------------------------------------------------------------------------------------------

// The S3 error codes that mean "no configuration" or "no lock on this version". SeaweedFS answers
// ObjectLockConfigurationNotFoundError (404) for a version without a retention and NoSuchObjectLegalHold (404) without
// a legal hold. MinIO answers NoSuchObjectLockConfiguration (400) for both. AWS S3 answers NoSuchObjectLockConfiguration
// (404).
var noObjectLockCodes = map[string]bool{
	"ObjectLockConfigurationNotFoundError": true,
	"NoSuchObjectLockConfiguration":        true,
	"NoSuchObjectLegalHold":                true,
}

// noObjectLock reports an answer that says "no lock".
func noObjectLock(err error) bool {
	var upstream *upstreamError
	return errors.As(err, &upstream) && noObjectLockCodes[upstream.Code]
}

func versionQuery(name, version string) url.Values {
	query := url.Values{}
	query.Set(name, "")
	if version != "" {
		query.Set("versionId", version)
	}
	return query
}

// getLockDocument sends a GET request with the query and decodes the XML answer into result.
func (s *s3Backend) getLockDocument(ctx context.Context, key string, query url.Values, result any) error {
	resp, err := s.do(ctx, http.MethodGet, key, query, nil, 0, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return s3ResponseError(resp)
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(result); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode s3 object lock response: %w", err)
	}
	return nil
}

// putLockDocument sends a PUT request with the XML body. S3 needs Content-MD5 for these requests.
func (s *s3Backend) putLockDocument(ctx context.Context, key string, query url.Values, body []byte, headers http.Header) error {
	sum := md5.Sum(body)
	headers.Set("Content-Type", "application/xml")
	headers.Set("Content-MD5", base64.StdEncoding.EncodeToString(sum[:]))
	resp, err := s.do(ctx, http.MethodPut, key, query, bytes.NewReader(body), int64(len(body)), headers)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return s3ResponseError(resp)
	}
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var embedded struct {
		XMLName xml.Name
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if len(answer) > 0 && xml.Unmarshal(answer, &embedded) == nil && embedded.XMLName.Local == "Error" {
		return &upstreamError{StatusCode: http.StatusBadGateway, Code: embedded.Code, Message: embedded.Message}
	}
	return nil
}

// ObjectLockConfiguration reads GetObjectLockConfiguration. A bucket without a configuration gives Enabled false.
func (s *s3Backend) ObjectLockConfiguration(ctx context.Context) (objectLockConfiguration, error) {
	var result struct {
		Enabled string `xml:"ObjectLockEnabled"`
		Rule    *struct {
			Mode  string `xml:"DefaultRetention>Mode"`
			Days  int    `xml:"DefaultRetention>Days"`
			Years int    `xml:"DefaultRetention>Years"`
		} `xml:"Rule"`
	}
	if err := s.getLockDocument(ctx, "", versionQuery("object-lock", ""), &result); err != nil {
		if noObjectLock(err) {
			return objectLockConfiguration{}, nil
		}
		return objectLockConfiguration{}, err
	}
	config := objectLockConfiguration{Enabled: strings.TrimSpace(result.Enabled) == "Enabled"}
	if result.Rule != nil && strings.TrimSpace(result.Rule.Mode) != "" {
		config.Default = &objectLockDefault{Mode: strings.ToUpper(strings.TrimSpace(result.Rule.Mode)), Days: result.Rule.Days, Years: result.Rule.Years}
	}
	return config, nil
}

// ObjectRetention reads GetObjectRetention. A version without a retention gives nil.
func (s *s3Backend) ObjectRetention(ctx context.Context, key, version string) (*objectRetention, error) {
	var result struct {
		Mode        string `xml:"Mode"`
		RetainUntil string `xml:"RetainUntilDate"`
	}
	if err := s.getLockDocument(ctx, key, versionQuery("retention", version), &result); err != nil {
		if noObjectLock(err) {
			return nil, nil
		}
		return nil, err
	}
	mode, date := strings.ToUpper(strings.TrimSpace(result.Mode)), strings.TrimSpace(result.RetainUntil)
	if mode == "" && date == "" {
		return nil, nil
	}
	until, err := time.Parse(time.RFC3339, date)
	if err != nil {
		return nil, fmt.Errorf("decode s3 retain-until date %q: %w", date, err)
	}
	return &objectRetention{Mode: mode, RetainUntil: until.UTC()}, nil
}

// ObjectLegalHold reads GetObjectLegalHold. A version without a legal hold gives false.
func (s *s3Backend) ObjectLegalHold(ctx context.Context, key, version string) (bool, error) {
	var result struct {
		Status string `xml:"Status"`
	}
	if err := s.getLockDocument(ctx, key, versionQuery("legal-hold", version), &result); err != nil {
		if noObjectLock(err) {
			return false, nil
		}
		return false, err
	}
	return strings.EqualFold(strings.TrimSpace(result.Status), "ON"), nil
}

// PutObjectRetention sends PutObjectRetention. bypassGovernance adds x-amz-bypass-governance-retention: true.
func (s *s3Backend) PutObjectRetention(ctx context.Context, key, version string, retention objectRetention, bypassGovernance bool) error {
	body := []byte(`<Retention xmlns="` + lifecycleNamespace + `"><Mode>` + retention.Mode + `</Mode><RetainUntilDate>` +
		retention.RetainUntil.UTC().Format(time.RFC3339) + `</RetainUntilDate></Retention>`)
	headers := make(http.Header)
	if bypassGovernance {
		headers.Set("x-amz-bypass-governance-retention", "true")
	}
	return s.putLockDocument(ctx, key, versionQuery("retention", version), body, headers)
}

// PutObjectLegalHold sends PutObjectLegalHold with the status ON or OFF.
func (s *s3Backend) PutObjectLegalHold(ctx context.Context, key, version string, on bool) error {
	status := "OFF"
	if on {
		status = "ON"
	}
	body := []byte(`<LegalHold xmlns="` + lifecycleNamespace + `"><Status>` + status + `</Status></LegalHold>`)
	return s.putLockDocument(ctx, key, versionQuery("legal-hold", version), body, make(http.Header))
}

// --- Storage instance -----------------------------------------------------------------------------------------------

// objectLockCapability is the state of Object Lock of an instance in this process.
type objectLockCapability struct {
	// on is set when the probe found a bucket with Object Lock.
	on atomic.Bool
	// bucketDefault is the default retention of the bucket that the probe read. It is nil for a bucket without one.
	bucketDefault atomic.Pointer[objectLockDefault]
}

// objectLockAvailable tells if the bucket of the instance has Object Lock. Until the probe has run, the answer is no.
func (s *storageInstance) objectLockAvailable() bool {
	if s == nil {
		return false
	}
	if _, ok := s.backend.(ObjectLockManager); !ok {
		return false
	}
	return s.objectLock.on.Load()
}

// probeObjectLock reads the Object Lock configuration of the bucket. Only "Enabled" shows the feature. A bucket
// without a configuration, a provider without Object Lock (501, 405) and any other failure hide it. The request is a
// read. An instance without the read permission is not probed.
func (s *storageInstance) probeObjectLock(ctx context.Context) {
	manager, ok := s.backend.(ObjectLockManager)
	if !ok || !s.permitted(permissionRead) {
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	config, err := manager.ObjectLockConfiguration(probeCtx)
	s.objectLock.on.Store(err == nil && config.Enabled)
	if err == nil && config.Enabled {
		s.objectLock.bucketDefault.Store(config.Default)
	}
}

// objectLockDefaultInfo gives the default retention of the bucket that the probe read at start, for /api/instances.
// It is nil for a bucket without Object Lock or without a default retention.
func (s *storageInstance) objectLockDefaultInfo() *objectLockDefaultJSON {
	if !s.objectLockAvailable() {
		return nil
	}
	value := s.objectLock.bucketDefault.Load()
	if value == nil {
		return nil
	}
	return &objectLockDefaultJSON{Mode: value.Mode, Days: value.Days, Years: value.Years}
}

func (s *storageInstance) objectLockManager() (ObjectLockManager, error) {
	manager, ok := s.backend.(ObjectLockManager)
	if !ok || !s.objectLock.on.Load() {
		return nil, objectLockUnsupported()
	}
	return manager, nil
}

// lockRequest runs one request to the provider in a slot of the gates of the instance.
func (s *storageInstance) lockRequest(ctx context.Context, request func() error) error {
	release, err := s.beginStorageRequest(ctx)
	if err != nil {
		return err
	}
	defer release()
	return request()
}

// readObjectLock reads the retention and the legal hold of the version (the current version for "").
func (s *storageInstance) readObjectLock(ctx context.Context, key, version string) (objectLockState, error) {
	manager, err := s.objectLockManager()
	if err != nil {
		return objectLockState{}, err
	}
	var state objectLockState
	if err := s.lockRequest(ctx, func() (err error) {
		state.Retention, err = manager.ObjectRetention(ctx, key, version)
		return err
	}); err != nil {
		return objectLockState{}, err
	}
	if err := s.lockRequest(ctx, func() (err error) {
		state.LegalHold, err = manager.ObjectLegalHold(ctx, key, version)
		return err
	}); err != nil {
		return objectLockState{}, err
	}
	s.observePermission(permissionRead, nil)
	return state, nil
}

// lockRefusalCandidate reports a refusal that a lock can give: the codes AccessDenied (SeaweedFS, AWS S3) and
// InvalidRequest (MinIO), or a message that names the lock. The lock of the version decides.
func lockRefusalCandidate(err error) bool {
	var upstream *upstreamError
	if !errors.As(err, &upstream) {
		return false
	}
	switch upstream.Code {
	case "AccessDenied", "InvalidRequest", "ObjectLocked":
		return true
	}
	message := strings.ToLower(upstream.Message)
	return strings.Contains(message, "worm") || strings.Contains(message, "object lock")
}

// isObjectLocked reports the error of a delete that the lock refused.
func isObjectLocked(err error) bool {
	var apiErr apiError
	return errors.As(err, &apiErr) && apiErr.Code == "object_locked"
}

// objectLockedError tells which lock keeps the version, and what the person can do.
func objectLockedError(key, version string, state objectLockState, now time.Time) error {
	what := fmt.Sprintf("%q", key)
	if version != "" {
		what = fmt.Sprintf("the version %s of %q", version, key)
	}
	var reasons, actions []string
	if state.LegalHold {
		reasons = append(reasons, "a legal hold is on")
		actions = append(actions, "remove the legal hold in the details of the file")
	}
	if retention := state.Retention; retention.active(now) {
		reasons = append(reasons, fmt.Sprintf("a %s retention keeps it until %s", modeName(retention.Mode), lockDate(retention.RetainUntil)))
		if retention.Mode == objectLockCompliance {
			actions = append(actions, "wait for the end of the Compliance retention (nobody can remove it before this date)")
		} else {
			actions = append(actions, "wait for the end of the Governance retention, or shorten it with the bypass in the details of the file")
		}
	}
	return apiError{Status: http.StatusConflict, Code: "object_locked", Message: fmt.Sprintf("Object Lock keeps %s: %s. To delete it, %s.",
		what, strings.Join(reasons, " and "), strings.Join(actions, ", and "))}
}

// explainLockRefusal turns the refusal of a delete into the error object_locked when the version has an active lock.
// key is the full key. Any other error comes back unchanged. The caller holds no slot of the gates.
func (s *storageInstance) explainLockRefusal(ctx context.Context, key, version string, err error) error {
	if err == nil || !s.objectLockAvailable() || !lockRefusalCandidate(err) || ctx.Err() != nil {
		return err
	}
	state, readErr := s.readObjectLock(ctx, key, version)
	now := time.Now()
	if readErr != nil || !state.locked(now) {
		return err
	}
	relative, ok := s.relativeKey(key)
	if !ok {
		relative = key
	}
	return objectLockedError(relative, version, state, now)
}

// retentionChange decides how a new retention applies to the current one. It gives true when the change needs the
// bypass of the Governance retention, and an error for a change that no request can make.
func retentionChange(current *objectRetention, next objectRetention, now time.Time) (bool, error) {
	if !current.active(now) {
		return false, nil
	}
	// SeaweedFS keeps the seconds and MinIO the milliseconds: the comparison uses the seconds.
	currentUntil, nextUntil := current.RetainUntil.Truncate(time.Second), next.RetainUntil.Truncate(time.Second)
	if current.Mode == objectLockCompliance {
		if next.Mode != objectLockCompliance {
			return false, apiError{Status: http.StatusConflict, Code: "compliance_retention", Message: fmt.Sprintf(
				"A Compliance retention cannot change to %s: it keeps the version until %s, and nobody can remove it or change it before this date, not even the owner of the bucket.",
				modeName(next.Mode), lockDate(current.RetainUntil))}
		}
		if nextUntil.Before(currentUntil) {
			return false, apiError{Status: http.StatusConflict, Code: "compliance_retention", Message: fmt.Sprintf(
				"A Compliance retention cannot be shortened: it keeps the version until %s, and nobody can remove it or shorten it before this date, not even the owner of the bucket. Give the same date or a later date.",
				lockDate(current.RetainUntil))}
		}
		return false, nil
	}
	return next.Mode != current.Mode || nextUntil.Before(currentUntil), nil
}

// objectLockChange is a checked change of the lock of one version. A nil field does not change.
type objectLockChange struct {
	Retention        *objectRetention
	LegalHold        *bool
	BypassGovernance bool
}

// ChangeObjectLock reads the lock of the version, checks the change against the rules, and sends it: first the
// retention, then the legal hold. A part that changes nothing is not sent.
func (s *storageInstance) ChangeObjectLock(ctx context.Context, key, version string, change objectLockChange) error {
	manager, err := s.objectLockManager()
	if err != nil {
		return err
	}
	current, err := s.readObjectLock(ctx, key, version)
	if err != nil {
		return err
	}
	now := time.Now()
	if next := change.Retention; next != nil {
		bypass, err := retentionChange(current.Retention, *next, now)
		if err != nil {
			return err
		}
		if bypass && !change.BypassGovernance {
			return apiError{Status: http.StatusConflict, Code: "governance_bypass_required", Message: fmt.Sprintf(
				"This change shortens or changes a Governance retention that keeps the version until %s. Confirm the bypass of the Governance retention to make it.",
				lockDate(current.Retention.RetainUntil))}
		}
		same := current.Retention.active(now) && current.Retention.Mode == next.Mode &&
			current.Retention.RetainUntil.Truncate(time.Second).Equal(next.RetainUntil.Truncate(time.Second))
		if !same {
			if err := s.lockRequest(ctx, func() error { return manager.PutObjectRetention(ctx, key, version, *next, bypass) }); err != nil {
				return objectLockWriteError(err, "retention")
			}
		}
	}
	if hold := change.LegalHold; hold != nil && *hold != current.LegalHold {
		if err := s.lockRequest(ctx, func() error { return manager.PutObjectLegalHold(ctx, key, version, *hold) }); err != nil {
			return objectLockWriteError(err, "legal hold")
		}
	}
	s.observePermission(permissionWrite, nil)
	return nil
}

// objectLockWriteError turns the refusal of a change into the answer of the API. A provider that refuses for the lock
// answers 403 on SeaweedFS and 400 on MinIO: the refusal does not change the write permission of the instance.
func objectLockWriteError(err error, part string) error {
	var upstream *upstreamError
	if !errors.As(err, &upstream) {
		return err
	}
	switch {
	case providerRefusedFeature(err):
		return objectLockUnsupported()
	case upstream.StatusCode == http.StatusNotFound:
		return err
	case upstream.StatusCode >= 400 && upstream.StatusCode < 500:
		return apiError{Status: http.StatusConflict, Code: "object_lock_refused", Message: fmt.Sprintf(
			"The storage provider refused the change of the %s (%s): the lock of the version forbids it, or the credentials of this instance cannot change it.",
			part, upstreamStatusText(upstream))}
	}
	return err
}

// --- HTTP -----------------------------------------------------------------------------------------------------------

type objectLockRetentionJSON struct {
	Mode string `json:"mode"`
	// RetainUntil is an RFC 3339 date in UTC.
	RetainUntil string `json:"retainUntil"`
	// Active is false for a date in the past: the retention keeps the version no longer.
	Active bool `json:"active"`
}

type objectLockDefaultJSON struct {
	Mode  string `json:"mode"`
	Days  int    `json:"days,omitempty"`
	Years int    `json:"years,omitempty"`
}

type objectLockResponse struct {
	Instance string `json:"instance"`
	Key      string `json:"key"`
	// Version is the version that the lock applies to: the request names it, or the HEAD of the object gives it.
	Version       string                   `json:"version"`
	Retention     *objectLockRetentionJSON `json:"retention"`
	LegalHold     bool                     `json:"legalHold"`
	BucketDefault *objectLockDefaultJSON   `json:"bucketDefault"`
	// Now is the date of the server (RFC 3339): the form compares the dates with it.
	Now              string `json:"now"`
	MaxRetentionDays int    `json:"maxRetentionDays"`
}

type objectLockRequest struct {
	Instance  string `json:"instance"`
	Key       string `json:"key"`
	Version   string `json:"version"`
	LegalHold *bool  `json:"legalHold"`
	Retention *struct {
		Mode        string `json:"mode"`
		RetainUntil string `json:"retainUntil"`
	} `json:"retention"`
	BypassGovernance bool `json:"bypassGovernance"`
}

// validVersionID reports a version id of 1 to maxObjectLockVersionBytes printable characters.
func validVersionID(version string) bool {
	return version != "" && len(version) <= maxObjectLockVersionBytes && printableASCII(version)
}

// validateObjectLockRequest checks the request without the provider: the version, a closed set of modes, and a
// retain-until date in the future and not later than maxObjectLockDays from now.
func validateObjectLockRequest(request objectLockRequest, now time.Time) (objectLockChange, error) {
	if !validVersionID(request.Version) {
		return objectLockChange{}, invalidObjectLock("version is required: give the id of the version that the lock applies to")
	}
	if request.LegalHold == nil && request.Retention == nil {
		return objectLockChange{}, invalidObjectLock("give legalHold, retention or both")
	}
	change := objectLockChange{LegalHold: request.LegalHold, BypassGovernance: request.BypassGovernance}
	if request.Retention != nil {
		mode := request.Retention.Mode
		if mode != objectLockGovernance && mode != objectLockCompliance {
			return objectLockChange{}, invalidObjectLock("retention.mode must be %s or %s, got %q", objectLockGovernance, objectLockCompliance, mode)
		}
		text := request.Retention.RetainUntil
		until, err := time.Parse(time.RFC3339, text)
		if len(text) > maxObjectLockDateChars || err != nil {
			return objectLockChange{}, invalidObjectLock("retention.retainUntil must be an RFC 3339 date such as 2026-10-21T07:28:00Z, got %q", text)
		}
		if !until.After(now) {
			return objectLockChange{}, invalidObjectLock("retention.retainUntil must be in the future, got %s", lockDate(until))
		}
		if until.After(now.AddDate(0, 0, maxObjectLockDays)) {
			return objectLockChange{}, invalidObjectLock("retention.retainUntil must be at most %d days from now, got %s", maxObjectLockDays, lockDate(until))
		}
		change.Retention = &objectRetention{Mode: mode, RetainUntil: until.UTC().Truncate(time.Second)}
	}
	return change, nil
}

// objectLockResponseFor reads the lock of the version and the default retention of the bucket. An empty version reads
// the current version, and the HEAD of the object gives its id.
func objectLockResponseFor(ctx context.Context, instance *storageInstance, key, version string) (objectLockResponse, error) {
	manager, err := instance.objectLockManager()
	if err != nil {
		return objectLockResponse{}, err
	}
	full := instance.fullKey(key)
	if version == "" {
		head, err := instance.Head(context.WithValue(ctx, objectVersionContextKey{}, ""), full)
		if err != nil {
			return objectLockResponse{}, err
		}
		closeObjectResponse(head)
		version = strings.TrimSpace(head.Header.Get("x-amz-version-id"))
	}
	state, err := instance.readObjectLock(ctx, full, version)
	if err != nil {
		return objectLockResponse{}, err
	}
	var config objectLockConfiguration
	if err := instance.lockRequest(ctx, func() (err error) {
		config, err = manager.ObjectLockConfiguration(ctx)
		return err
	}); err != nil {
		return objectLockResponse{}, err
	}
	now := time.Now().UTC()
	response := objectLockResponse{
		Instance: instance.cfg.ID, Key: key, Version: version, LegalHold: state.LegalHold,
		Now: now.Format(time.RFC3339), MaxRetentionDays: maxObjectLockDays,
	}
	if retention := state.Retention; retention != nil {
		response.Retention = &objectLockRetentionJSON{Mode: retention.Mode, RetainUntil: retention.RetainUntil.UTC().Format(time.RFC3339), Active: retention.active(now)}
	}
	if config.Default != nil {
		response.BucketDefault = &objectLockDefaultJSON{Mode: config.Default.Mode, Days: config.Default.Days, Years: config.Default.Years}
	}
	return response, nil
}

// handleObjectLock reads (GET, permission read) and changes (PUT, permission write) the lock of one version of an
// object. The answer of a PUT is the new state. Each change goes to the log.
func (a *application) handleObjectLock(w http.ResponseWriter, r *http.Request) {
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
		if version != "" && !validVersionID(version) {
			writeAPIError(w, invalidObjectLock("version must be the id of a version of the object"))
			return
		}
		response, err := objectLockResponseFor(r.Context(), instance, key, version)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, response)
	case http.MethodPut:
		var request objectLockRequest
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
		for _, permission := range []string{permissionRead, permissionWrite} {
			if err := requirePermission(instance, permission); err != nil {
				writeAPIError(w, err)
				return
			}
		}
		if key == "" {
			writeAPIError(w, apiError{Status: http.StatusBadRequest, Code: "invalid_key", Message: "object key is required"})
			return
		}
		change, err := validateObjectLockRequest(request, time.Now())
		if err != nil {
			writeAPIError(w, err)
			return
		}
		if _, err := a.authorizeUse(r, actionObjectLock, instance, key); err != nil {
			writeAPIError(w, err)
			return
		}
		if err := instance.ChangeObjectLock(r.Context(), instance.fullKey(key), request.Version, change); err != nil {
			writeAPIError(w, err)
			return
		}
		logObjectLockChange(instance, key, request.Version, change)
		response, err := objectLockResponseFor(r.Context(), instance, key, request.Version)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, response)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPut)
	}
}

// logObjectLockChange writes one log line for a change of a lock: what changed and the use of the bypass.
func logObjectLockChange(instance *storageInstance, key, version string, change objectLockChange) {
	hold, retention := "unchanged", "unchanged"
	if change.LegalHold != nil {
		hold = map[bool]string{true: "on", false: "off"}[*change.LegalHold]
	}
	if change.Retention != nil {
		retention = change.Retention.Mode + " until " + change.Retention.RetainUntil.UTC().Format(time.RFC3339)
	}
	log.Printf("object_lock.change instance=%s key=%q version=%q legal_hold=%s retention=%q bypass_governance=%t",
		instance.cfg.ID, key, version, hold, retention, change.BypassGovernance)
}
