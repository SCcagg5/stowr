package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"sync"
	"time"
)

// Usage and quota of a bucket.
//
// The S3 API gives no usage and no quota of a bucket. Each provider gives them in its own admin API. The usage block of
// an auth block names the reader (source) and its URLs: the server never guesses the source. Without the block, the
// server makes no admin request and the page shows no usage. One reader for each source, in its own file:
// usage_garage.go, usage_seaweedfs.go and usage_minio.go. AWS S3 has no quota: no reader.
//
// Cost and safety: each admin request is a read (GET), with a time limit of usageTimeout and a size limit for the
// answer. The server keeps the result of an instance for usageCacheTTL (a failure for usageRetryInterval), and one read
// at a time runs for an instance. A request with fresh=1 (the reload of the page, or the end of a change) reads again
// when the result is older than usageRetryInterval. The token and the URLs of the admin API never go into an answer
// or a log line: the errors of the readers name the step and the HTTP status only.
//
// The numbers are for the whole bucket, also for an instance with a root prefix.

// The sources of a usage block. They are also the names of the providers of the lifecycle probe.
const (
	usageSourceGarage    = flavourGarage
	usageSourceSeaweedFS = flavourSeaweedFS
	usageSourceMinIO     = flavourMinIO
	// usageSourceStatfs is the source of the usage of a volume: the size of its file system. A volume block has it;
	// a usage block cannot name it.
	usageSourceStatfs = "statfs"
)

const (
	// usageTimeout is the longest time of one read of the usage, with all its admin requests.
	usageTimeout = 5 * time.Second
	// usageCacheTTL is the life of a result in the cache of an instance.
	usageCacheTTL = 60 * time.Second
	// usageRetryInterval is the life of a failure in the cache, and the shortest interval between two reads with fresh=1.
	usageRetryInterval = 10 * time.Second

	// usageWarningRatio and usageErrorRatio are the levels of a used part of the limit: "warning" from 80 %, "error"
	// from 95 %.
	usageWarningRatio = 0.80
	usageErrorRatio   = 0.95
)

// bucketUsage is the usage and the quota of a bucket, the same for each source. A count that the source does not give
// is -1. A limit of 0 is no limit.
type bucketUsage struct {
	UsedBytes    int64
	UsedObjects  int64
	LimitBytes   int64
	LimitObjects int64
	Source       string
	// Approximate tells that the source counts with a delay or counts more than the objects (refer to
	// usageApproximateReasons).
	Approximate bool
	// AsOf is the time of the count at the source.
	AsOf time.Time
	// SelfState is the state of the count of the files of a volume: "computing", "partial" or "ready". It is "" for a
	// bucket.
	SelfState string
}

// BucketUsageReader reads the usage and the quota of one bucket from the admin API of a provider. The usage block of
// the auth block makes one reader, and the buckets of the auth block share it.
type BucketUsageReader interface {
	BucketUsage(ctx context.Context, bucket string) (bucketUsage, error)
}

// The scopes of the capacity of the storage behind a bucket or a volume.
const (
	// storageScopeCluster is the data partitions of the nodes of a Garage cluster.
	storageScopeCluster = "cluster"
	// storageScopeDisks is the disks of the volume servers of SeaweedFS or the drives of MinIO.
	storageScopeDisks = "disks"
	// storageScopeFilesystem is the file system of a volume (statfs).
	storageScopeFilesystem = "filesystem"
)

// storageCapacity is the capacity of the storage behind a bucket: the disks or the nodes of the provider, for all its
// buckets. For a volume, it is the file system of the folder. The numbers are raw: the replication or the erasure code
// of the provider uses more than one byte of the disks for one byte of an object.
type storageCapacity struct {
	Scope      string
	UsedBytes  int64
	TotalBytes int64
	FreeBytes  int64
	// Approximate tells that a part of the storage did not answer, or that the storage counts with a delay. Reason then
	// says why.
	Approximate bool
	Reason      string
	// Note says what the numbers count, for example "raw capacity of 3 storage nodes".
	Note string
	AsOf time.Time
}

// StorageCapacityReader is optional: a usage reader that also reads the capacity of the storage behind the buckets.
// AWS S3 has none: its storage has no limit for the client.
type StorageCapacityReader interface {
	StorageCapacity(ctx context.Context) (storageCapacity, error)
}

// usageApproximateReasons tells why the numbers of a source are approximate. The page shows the text in a tooltip.
var usageApproximateReasons = map[string]string{
	usageSourceSeaweedFS: "SeaweedFS gives the size of the volumes of the bucket, with a delay of some seconds. The number of objects is not known.",
	usageSourceMinIO:     "MinIO counts the usage in a background scan, about one time each minute or less often on a large server.",
}

// newBucketUsageReader makes the reader of the usage block of an auth block. It gives nil without a usage block.
func newBucketUsageReader(auth authConfig, client *http.Client, creds s3Credentials) (BucketUsageReader, error) {
	usage := auth.Usage
	if usage == nil {
		return nil, nil
	}
	endpoint, err := parseStorageEndpoint("usage", usage.Endpoint)
	if err != nil {
		return nil, err
	}
	switch usage.Source {
	case usageSourceGarage:
		return &garageUsageReader{endpoint: endpoint, client: client, token: usage.Token}, nil
	case usageSourceSeaweedFS:
		filer, err := parseStorageEndpoint("usage filer", usage.FilerEndpoint)
		if err != nil {
			return nil, err
		}
		return &seaweedfsUsageReader{master: endpoint, filer: filer, client: client}, nil
	case usageSourceMinIO:
		return &minioUsageReader{endpoint: endpoint, client: client, creds: creds, region: auth.Region}, nil
	}
	return nil, fmt.Errorf("auth %q: unknown usage source %q", auth.ID, usage.Source)
}

// usageHTTPError is an answer of an admin API that is not a success. It names the step and the status, never the URL.
// Code is the error code of a JSON error document ("Code" for MinIO, "code" for Garage), when the answer has one.
type usageHTTPError struct {
	Step   string
	Status int
	Code   string
}

func (e *usageHTTPError) Error() string {
	switch e.Status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Sprintf("%s: the admin API refused the credentials (HTTP %d)", e.Step, e.Status)
	case http.StatusNotFound:
		return fmt.Sprintf("%s: the admin API does not know the bucket (HTTP %d)", e.Step, e.Status)
	}
	return fmt.Sprintf("%s: the admin API answered HTTP %d", e.Step, e.Status)
}

// usageTransportError gives the reason of a failed admin request without the URL. The errors of net/http contain the
// URL, and the URL can contain the address of a private admin API.
func usageTransportError(step string, err error) error {
	var certificate *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var netError net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%s: the admin API did not answer in %s", step, usageTimeout)
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("%s: the request was canceled", step)
	case errors.As(err, &certificate), errors.As(err, &unknownAuthority), errors.As(err, &hostname):
		return fmt.Errorf("%s: the TLS certificate of the admin API is not valid", step)
	case errors.As(err, &netError) && netError.Timeout():
		return fmt.Errorf("%s: the admin API did not answer in %s", step, usageTimeout)
	}
	return fmt.Errorf("%s: cannot connect to the admin API", step)
}

// getUsageJSON sends one admin request and decodes its JSON answer into target. The answer must be a success and must
// not be longer than limit bytes.
func getUsageJSON(client *http.Client, req *http.Request, step string, limit int64, target any) error {
	resp, err := client.Do(req)
	if err != nil {
		return usageTransportError(step, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return usageTransportError(step, err)
	}
	if resp.StatusCode != http.StatusOK {
		var document struct {
			Code      string `json:"Code"`
			LowerCode string `json:"code"`
		}
		_ = json.Unmarshal(data, &document)
		code := document.Code
		if code == "" {
			code = document.LowerCode
		}
		return &usageHTTPError{Step: step, Status: resp.StatusCode, Code: safeProviderErrorCode(code)}
	}
	if int64(len(data)) > limit {
		return fmt.Errorf("%s: the answer of the admin API is longer than %d bytes", step, limit)
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("%s: the answer of the admin API is not the expected JSON", step)
	}
	return nil
}

// --- Storage instance -----------------------------------------------------------------------------------------------

// usageCapability is the usage reader of an instance and its cache.
type usageCapability struct {
	// reader is nil for an instance whose auth block has no usage block.
	reader BucketUsageReader
	source string

	mu     sync.Mutex
	result usageResult
	readAt time.Time
	// flight is open while a read runs. The other requests wait for it.
	flight chan struct{}
}

// usageResult is one read of the usage of an instance: the part of the bucket and the part of the storage behind it,
// each with its error. A failure of one part does not hide the other. storage is nil for a reader without the capacity
// of the storage, and for a failed read of it.
type usageResult struct {
	value      bucketUsage
	err        error
	storage    *storageCapacity
	storageErr error
}

// failed tells if a part of the read failed: the cache then keeps it for usageRetryInterval only.
func (r usageResult) failed() bool {
	return r.err != nil || r.storageErr != nil
}

// errUsageUnsupported is the answer for an instance without a usage block.
var errUsageUnsupported = apiError{Status: http.StatusNotImplemented, Code: "usage_unsupported", Message: "this bucket has no usage source in the configuration"}

// usageAvailable tells if the page can show the usage of the bucket.
func (s *storageInstance) usageAvailable() bool {
	return s != nil && s.usage.reader != nil
}

// bucketUsage gives the part of the bucket of usageRead.
func (s *storageInstance) bucketUsage(ctx context.Context, fresh bool) (bucketUsage, error) {
	result := s.usageRead(ctx, fresh)
	return result.value, result.err
}

// liveUsageReader marks a reader whose read costs no request (a volume: a statfs call and the count in memory of the
// size of the folder). The server reads it at each request, without the cache.
type liveUsageReader interface {
	liveUsage()
}

// usageRead gives the usage of the instance from the cache, or reads it. One read at a time runs for the instance: the
// other requests wait for its result. With fresh, a result older than usageRetryInterval is read again. The read does
// not stop when the request that started it stops, so the other requests get its result.
func (s *storageInstance) usageRead(ctx context.Context, fresh bool) usageResult {
	usage := &s.usage
	if usage.reader == nil {
		return usageResult{err: errUsageUnsupported}
	}
	if _, live := usage.reader.(liveUsageReader); live {
		return s.readUsage(ctx)
	}
	for {
		usage.mu.Lock()
		if usage.flight == nil && !usage.readAt.IsZero() {
			life := usageCacheTTL
			if usage.result.failed() || fresh {
				life = usageRetryInterval
			}
			if timeNowUTC().Sub(usage.readAt) < life {
				result := usage.result
				usage.mu.Unlock()
				return result
			}
		}
		if flight := usage.flight; flight != nil {
			usage.mu.Unlock()
			select {
			case <-flight:
				continue
			case <-ctx.Done():
				return usageResult{err: ctx.Err(), storageErr: ctx.Err()}
			}
		}
		flight := make(chan struct{})
		usage.flight = flight
		usage.mu.Unlock()
		result := s.readUsage(ctx)
		usage.mu.Lock()
		usage.result, usage.readAt = result, timeNowUTC()
		usage.flight = nil
		usage.mu.Unlock()
		close(flight)
		return result
	}
}

// readUsage makes one read of the two parts at the same time, in usageTimeout for the two.
func (s *storageInstance) readUsage(ctx context.Context) (result usageResult) {
	usage := &s.usage
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), usageTimeout)
	defer cancel()
	var wg sync.WaitGroup
	if capacity, ok := usage.reader.(StorageCapacityReader); ok {
		wg.Add(1)
		go func() {
			defer wg.Done()
			storage, err := capacity.StorageCapacity(readCtx)
			if err != nil {
				log.Printf("usage.error bucket=%q source=%s part=storage: %v", s.cfg.ID, usage.source, err)
				result.storageErr = err
				return
			}
			result.storage = &storage
		}()
	}
	value, err := usage.reader.BucketUsage(readCtx, s.cfg.Bucket)
	wg.Wait()
	if err != nil {
		log.Printf("usage.error bucket=%q source=%s: %v", s.cfg.ID, usage.source, err)
		result.err = err
		return result
	}
	value.Source = usage.source
	result.value = value
	return result
}

// probeUsage compares the source of the usage block with the provider that the lifecycle probe found. The source of
// the file applies. A different known provider gives a warning in the log. The probe makes no admin request.
func (s *storageInstance) probeUsage(ctx context.Context) {
	if !s.usageAvailable() || !s.permitted(permissionRead) {
		return
	}
	flavour := s.lifecycleFlavour(ctx)
	if flavour != "" && flavour != s.usage.source {
		log.Printf("config.warning bucket %q: the usage block of auth %q has source = %q, but the provider answers as %s; the server uses the source of the file", s.cfg.ID, s.cfg.AuthID, s.usage.source, flavourNames[flavour])
	}
}

// usageLevel gives the used part of the limit in percent, with one decimal (the next decimals are cut), and the level:
// "" without a limit, "ok", "warning" from usageWarningRatio, "error" from usageErrorRatio. With a limit of bytes and a
// limit of objects, the larger part applies.
func usageLevel(value bucketUsage) (float64, string) {
	ratio := -1.0
	if value.LimitBytes > 0 && value.UsedBytes >= 0 {
		ratio = float64(value.UsedBytes) / float64(value.LimitBytes)
	}
	if value.LimitObjects > 0 && value.UsedObjects >= 0 {
		ratio = math.Max(ratio, float64(value.UsedObjects)/float64(value.LimitObjects))
	}
	switch {
	case ratio < 0:
		return 0, ""
	case ratio >= usageErrorRatio:
		return math.Floor(ratio*1000) / 10, "error"
	case ratio >= usageWarningRatio:
		return math.Floor(ratio*1000) / 10, "warning"
	}
	return math.Floor(ratio*1000) / 10, "ok"
}

// storageLevel gives the used part of the storage in percent, with one decimal, and the level, as usageLevel does.
func storageLevel(storage storageCapacity) (float64, string) {
	return usageLevel(bucketUsage{UsedBytes: storage.UsedBytes, LimitBytes: storage.TotalBytes})
}

// --- HTTP -----------------------------------------------------------------------------------------------------------

type usageResponse struct {
	Supported bool `json:"supported"`
	// Available is false when the read failed. Error then gives the reason, and the numbers are 0.
	Available   bool            `json:"available"`
	Error       *usageErrorJSON `json:"error,omitempty"`
	Instance    string          `json:"instance"`
	Bucket      string          `json:"bucket"`
	RootPrefix  string          `json:"rootPrefix,omitempty"`
	Source      string          `json:"source"`
	UsedBytes   int64           `json:"usedBytes"`
	UsedObjects int64           `json:"usedObjects"`
	// LimitBytes and LimitObjects are 0 without a limit. LimitSource is "provider", "config" (bucket.quota_bytes) or
	// "" without a limit.
	LimitBytes        int64     `json:"limitBytes"`
	LimitObjects      int64     `json:"limitObjects"`
	LimitSource       string    `json:"limitSource"`
	Approximate       bool      `json:"approximate"`
	ApproximateReason string    `json:"approximateReason,omitempty"`
	AsOf              time.Time `json:"asOf"`
	// Percent and Level are the more constraining part: the quota of the bucket or the storage behind it. Constraint
	// names it: "quota", "storage", or "" when the answer has no limit.
	Percent    float64 `json:"percent"`
	Level      string  `json:"level"`
	Constraint string  `json:"constraint"`
	// QuotaPercent and QuotaLevel are the used part of the quota of the bucket alone.
	QuotaPercent float64 `json:"quotaPercent"`
	QuotaLevel   string  `json:"quotaLevel"`
	// SelfBytes and SelfState are the size of the files of a volume, and the state of its count: "computing" (the count
	// runs: at least SelfBytes), "partial" (the count stopped at its budget: at least SelfBytes) or "ready".
	SelfBytes *int64 `json:"selfBytes,omitempty"`
	SelfState string `json:"selfState,omitempty"`
	// Storage is the capacity of the storage behind the bucket. StorageError is the reason of a failed read of it.
	Storage      *storageJSON    `json:"storage,omitempty"`
	StorageError *usageErrorJSON `json:"storageError,omitempty"`
}

type storageJSON struct {
	Scope             string    `json:"scope"`
	UsedBytes         int64     `json:"usedBytes"`
	TotalBytes        int64     `json:"totalBytes"`
	FreeBytes         int64     `json:"freeBytes"`
	Percent           float64   `json:"percent"`
	Level             string    `json:"level"`
	Approximate       bool      `json:"approximate"`
	ApproximateReason string    `json:"approximateReason,omitempty"`
	Note              string    `json:"note,omitempty"`
	AsOf              time.Time `json:"asOf"`
}

type usageErrorJSON struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// usageAnswer makes the answer of GET /api/usage from a result. The limit of the operator (bucket.quota_bytes)
// applies when the source gives no limit of bytes.
func usageAnswer(instance *storageInstance, value bucketUsage) usageResponse {
	answer := usageResponse{
		Supported: true, Available: true, Instance: instance.cfg.ID, Bucket: instance.cfg.Bucket, RootPrefix: instance.cfg.RootPrefix,
		Source: value.Source, UsedBytes: value.UsedBytes, UsedObjects: value.UsedObjects,
		LimitBytes: value.LimitBytes, LimitObjects: value.LimitObjects, Approximate: value.Approximate, AsOf: value.AsOf.UTC(),
	}
	if answer.LimitBytes > 0 || answer.LimitObjects > 0 {
		answer.LimitSource = "provider"
	}
	if answer.LimitBytes == 0 && instance.cfg.QuotaBytes > 0 {
		answer.LimitBytes, answer.LimitSource = instance.cfg.QuotaBytes, "config"
	}
	if answer.Approximate {
		answer.ApproximateReason = usageApproximateReasons[value.Source]
	}
	answer.Percent, answer.Level = usageLevel(bucketUsage{UsedBytes: answer.UsedBytes, UsedObjects: answer.UsedObjects, LimitBytes: answer.LimitBytes, LimitObjects: answer.LimitObjects})
	answer.QuotaPercent, answer.QuotaLevel = answer.Percent, answer.Level
	if answer.Level != "" {
		answer.Constraint = "quota"
	}
	if value.SelfState != "" {
		self := value.UsedBytes
		answer.SelfBytes, answer.SelfState = &self, value.SelfState
	}
	return answer
}

// usageAnswerOf makes the answer of GET /api/usage from the two parts of a read. A failed part gives its error, and
// the other part stays. Percent, Level and Constraint are the part with the larger used part.
func usageAnswerOf(instance *storageInstance, result usageResult) usageResponse {
	noun := "bucket"
	if instance.cfg.Provider == providerVolume {
		noun = "volume"
	}
	var answer usageResponse
	if result.err != nil {
		answer = usageResponse{
			Supported: true, Instance: instance.cfg.ID, Bucket: instance.cfg.Bucket, RootPrefix: instance.cfg.RootPrefix, Source: instance.usage.source,
			Error: &usageErrorJSON{Code: "usage_unavailable", Message: "The usage of the " + noun + " is unavailable: " + result.err.Error() + "."},
		}
	} else {
		answer = usageAnswer(instance, result.value)
	}
	if result.storageErr != nil {
		answer.StorageError = &usageErrorJSON{Code: "storage_unavailable", Message: "The capacity of the storage is unavailable: " + result.storageErr.Error() + "."}
	}
	if storage := result.storage; storage != nil {
		percent, level := storageLevel(*storage)
		answer.Storage = &storageJSON{
			Scope: storage.Scope, UsedBytes: storage.UsedBytes, TotalBytes: storage.TotalBytes, FreeBytes: storage.FreeBytes,
			Percent: percent, Level: level, Approximate: storage.Approximate, ApproximateReason: storage.Reason, Note: storage.Note,
			AsOf: storage.AsOf.UTC(),
		}
		if level != "" && (answer.Constraint == "" || percent > answer.Percent) {
			answer.Percent, answer.Level, answer.Constraint = percent, level, "storage"
		}
	}
	return answer
}

// handleUsage gives the usage and the quota of the bucket of an instance (GET, permission read). An instance without a
// usage block gives {"supported": false}. A failed read gives 200 with "available": false and the reason: an answer
// 502 would tell the page that the server does not answer (session.js), and a failure must not break the page.
func (a *application) handleUsage(w http.ResponseWriter, r *http.Request) {
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
	w.Header().Set("Cache-Control", "no-store")
	if !instance.usageAvailable() {
		writeJSON(w, http.StatusOK, map[string]bool{"supported": false})
		return
	}
	writeJSON(w, http.StatusOK, usageAnswerOf(instance, instance.usageRead(r.Context(), r.URL.Query().Get("fresh") == "1")))
}
