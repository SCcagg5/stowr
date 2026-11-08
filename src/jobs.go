package main

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	maxStatsLargestEntries = 100
	maxStatsRecentEntries  = 100
	maxStatsFolderDepth    = 5
	statsLayoutVersion     = 4
)

type statsEntryHeap []statsEntry

func (h statsEntryHeap) Len() int { return len(h) }
func (h statsEntryHeap) Less(i, j int) bool {
	if h[i].Bytes != h[j].Bytes {
		return h[i].Bytes < h[j].Bytes
	}
	return h[i].Path > h[j].Path
}
func (h statsEntryHeap) Swap(i, j int)   { h[i], h[j] = h[j], h[i] }
func (h *statsEntryHeap) Push(value any) { *h = append(*h, value.(statsEntry)) }
func (h *statsEntryHeap) Pop() any {
	old := *h
	last := len(old) - 1
	value := old[last]
	*h = old[:last]
	return value
}

type statsRecentHeap []statsEntry

func (h statsRecentHeap) Len() int { return len(h) }
func (h statsRecentHeap) Less(i, j int) bool {
	if h[i].LastModified != h[j].LastModified {
		return h[i].LastModified < h[j].LastModified
	}
	return h[i].Path > h[j].Path
}
func (h statsRecentHeap) Swap(i, j int)   { h[i], h[j] = h[j], h[i] }
func (h *statsRecentHeap) Push(value any) { *h = append(*h, value.(statsEntry)) }
func (h *statsRecentHeap) Pop() any {
	old := *h
	last := len(old) - 1
	value := old[last]
	*h = old[:last]
	return value
}

func addRecentStatsEntry(entries *[]statsEntry, entry statsEntry) {
	if entries == nil || entry.Path == "" || entry.LastModified == "" {
		return
	}
	h := (*statsRecentHeap)(entries)
	if h.Len() < maxStatsRecentEntries {
		heap.Push(h, entry)
		return
	}
	if h.Len() == 0 {
		return
	}
	oldest := (*h)[0]
	if entry.LastModified < oldest.LastModified || (entry.LastModified == oldest.LastModified && entry.Path >= oldest.Path) {
		return
	}
	heap.Pop(h)
	heap.Push(h, entry)
}

func addLargestStatsEntry(entries *[]statsEntry, entry statsEntry) {
	if entries == nil || entry.Bytes < 0 || entry.Path == "" {
		return
	}
	h := (*statsEntryHeap)(entries)
	if h.Len() < maxStatsLargestEntries {
		heap.Push(h, entry)
		return
	}
	if h.Len() == 0 {
		return
	}
	minimum := (*h)[0]
	if entry.Bytes < minimum.Bytes || (entry.Bytes == minimum.Bytes && entry.Path >= minimum.Path) {
		return
	}
	heap.Pop(h)
	heap.Push(h, entry)
}

const (
	jobTypeCopyPrefix            = "copy_prefix"
	jobTypeMovePrefix            = "move_prefix"
	jobTypeCopyObject            = "copy_object"
	jobTypeMoveObject            = "move_object"
	jobTypeDeletePrefix          = "delete_prefix"
	jobTypeStatsPrefix           = "stats_prefix"
	jobTypeIntegrity             = "integrity"
	jobTypeExtractArchive        = "extract_archive"
	jobTypeArchiveEntryIntegrity = "archive_entry_integrity"

	jobStatusQueued    = "queued"
	jobStatusRunning   = "running"
	jobStatusPaused    = "paused"
	jobStatusCompleted = "completed"
	jobStatusFailed    = "failed"
	jobStatusCanceled  = "canceled"

	// Jobs are held only in process memory. Keep the active set deliberately
	// small so a burst of expensive scans cannot exhaust a low-memory server.
	maxActiveJobs  = 4
	jobWorkerCount = 2
	jobQueueSize   = 8
)

// A paused job still counts against maxActiveJobs, and only the holder of its
// id can resume or cancel it. Abandoned paused jobs are canceled after this
// idle period so they cannot pin the shared capacity forever.
var (
	pausedJobTTL    = 30 * time.Minute
	jobReapInterval = time.Minute
)

var (
	errJobPaused   = errors.New("job paused")
	errJobCanceled = errors.New("job canceled")
)

// jobState is a job in the memory of the process. In a copy job, CopiedBytes counts the bytes of the copied objects.
// CopyingBytes counts the copied parts of the object in progress: a pause or an error drops them. TotalBytes is the size
// of the source of a copy_object or a move_object job.
type jobState struct {
	ID                           string                        `json:"id"`
	Type                         string                        `json:"type"`
	Instance                     string                        `json:"instance"`
	TargetInstance               string                        `json:"target_instance,omitempty"`
	Source                       string                        `json:"source,omitempty"`
	Version                      string                        `json:"version,omitempty"`
	Target                       string                        `json:"target,omitempty"`
	Entries                      []string                      `json:"entries,omitempty"`
	Prefix                       string                        `json:"prefix,omitempty"`
	Status                       string                        `json:"status"`
	Processed                    int64                         `json:"processed"`
	StorageRequests              int64                         `json:"storage_requests,omitempty"`
	StorageBytes                 int64                         `json:"storage_bytes,omitempty"`
	CopiedBytes                  int64                         `json:"copied_bytes,omitempty"`
	CopyingBytes                 int64                         `json:"copying_bytes,omitempty"`
	TotalBytes                   int64                         `json:"total_bytes,omitempty"`
	LastKey                      string                        `json:"last_key,omitempty"`
	Stats                        *statsResponse                `json:"stats,omitempty"`
	IntegrityRequest             *integrityRequest             `json:"integrity_request,omitempty"`
	Integrity                    *integrityResult              `json:"integrity,omitempty"`
	ArchiveExtract               *archiveExtractResult         `json:"archive_extract,omitempty"`
	ArchiveEntryIntegrityRequest *archiveEntryIntegrityRequest `json:"archive_entry_integrity_request,omitempty"`
	ArchiveEntryIntegrity        *archiveEntryIntegrityResult  `json:"archive_entry_integrity,omitempty"`
	TagChange                    *tagChange                    `json:"tag_change,omitempty"`
	MultipartAbort               *multipartAbortJob            `json:"multipart_abort,omitempty"`
	MultipartResult              *multipartAbortResult         `json:"multipart_result,omitempty"`
	Locked                       int64                         `json:"locked,omitempty"`
	LockedKeys                   []string                      `json:"locked_keys,omitempty"`
	// Refusal is the refusal for a ceiling that stopped the job. A route that waits for the job gives it as it is.
	Refusal *apiError `json:"-"`
	// Ceilings has the ceilings of the request that made the job. The job applies them in its own context.
	Ceilings  accessCeilings `json:"-"`
	Error     string         `json:"error,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	StartedAt *time.Time     `json:"started_at,omitempty"`
	EndedAt   *time.Time     `json:"ended_at,omitempty"`
}

type publicJob struct {
	ID                    string                       `json:"id"`
	Type                  string                       `json:"type"`
	Instance              string                       `json:"instance"`
	TargetInstance        string                       `json:"targetInstance,omitempty"`
	Source                string                       `json:"source,omitempty"`
	Version               string                       `json:"version,omitempty"`
	Target                string                       `json:"target,omitempty"`
	Entries               []string                     `json:"entries,omitempty"`
	Prefix                string                       `json:"prefix,omitempty"`
	Status                string                       `json:"status"`
	Processed             int64                        `json:"processed"`
	StorageRequests       int64                        `json:"storageRequests,omitempty"`
	StorageBytes          int64                        `json:"storageBytes,omitempty"`
	CopiedBytes           int64                        `json:"copiedBytes,omitempty"`
	TotalBytes            int64                        `json:"totalBytes,omitempty"`
	LastKey               string                       `json:"lastKey,omitempty"`
	Stats                 *statsResponse               `json:"stats,omitempty"`
	Integrity             *integrityResult             `json:"integrity,omitempty"`
	ArchiveExtract        *archiveExtractResult        `json:"archiveExtract,omitempty"`
	ArchiveEntryIntegrity *archiveEntryIntegrityResult `json:"archiveEntryIntegrity,omitempty"`
	OlderThanDays         int                          `json:"olderThanDays,omitempty"`
	MultipartAbort        *multipartAbortResult        `json:"multipartAbort,omitempty"`
	Locked                int64                        `json:"locked,omitempty"`
	LockedKeys            []string                     `json:"lockedKeys,omitempty"`
	Error                 string                       `json:"error,omitempty"`
	CreatedAt             time.Time                    `json:"createdAt"`
	UpdatedAt             time.Time                    `json:"updatedAt"`
	StartedAt             *time.Time                   `json:"startedAt,omitempty"`
	EndedAt               *time.Time                   `json:"endedAt,omitempty"`
}

func (j jobState) public() publicJob {
	result := publicJob{
		ID:              j.ID,
		Type:            j.Type,
		Instance:        j.Instance,
		TargetInstance:  j.TargetInstance,
		Source:          j.Source,
		Version:         j.Version,
		Target:          j.Target,
		Entries:         append([]string(nil), j.Entries...),
		Prefix:          j.Prefix,
		Status:          j.Status,
		Processed:       j.Processed,
		StorageRequests: j.StorageRequests,
		StorageBytes:    j.StorageBytes,
		CopiedBytes:     j.CopiedBytes + j.CopyingBytes,
		TotalBytes:      j.TotalBytes,
		LastKey:         j.LastKey,
		Stats:           cloneStatsForPublic(j.Stats),
		Locked:          j.Locked,
		LockedKeys:      append([]string(nil), j.LockedKeys...),
		Error:           j.Error,
		CreatedAt:       j.CreatedAt,
		UpdatedAt:       j.UpdatedAt,
		StartedAt:       cloneTimePointer(j.StartedAt),
		EndedAt:         cloneTimePointer(j.EndedAt),
	}
	if j.MultipartAbort != nil {
		result.OlderThanDays = j.MultipartAbort.OlderThanDays
	}
	if j.MultipartResult != nil {
		counts := *j.MultipartResult
		result.MultipartAbort = &counts
	}
	// Analysis results can be large. Keep progress responses compact and expose
	// the immutable result only after the job reaches the completed state.
	if j.Status == jobStatusCompleted {
		result.Integrity = cloneIntegrityResult(j.Integrity)
		result.ArchiveExtract = cloneArchiveExtractResult(j.ArchiveExtract)
		result.ArchiveEntryIntegrity = cloneArchiveEntryIntegrityResult(j.ArchiveEntryIntegrity)
	}
	return result
}

func cloneStats(stats *statsResponse) *statsResponse {
	if stats == nil {
		return nil
	}
	copyStats := *stats
	copyStats.ByType = cloneAggregateMap(stats.ByType)
	copyStats.ByFolder = cloneAggregateMap(stats.ByFolder)
	copyStats.Treemap = cloneStatsTreemapNode(stats.Treemap)
	copyStats.Largest = append([]statsEntry(nil), stats.Largest...)
	copyStats.Recent = append([]statsEntry(nil), stats.Recent...)
	copyStats.Newest = cloneTimePointer(stats.Newest)
	copyStats.Oldest = cloneTimePointer(stats.Oldest)
	return &copyStats
}

func cloneStatsForPublic(stats *statsResponse) *statsResponse {
	copyStats := cloneStats(stats)
	if copyStats == nil {
		return nil
	}
	sort.SliceStable(copyStats.Largest, func(i, j int) bool {
		if copyStats.Largest[i].Bytes != copyStats.Largest[j].Bytes {
			return copyStats.Largest[i].Bytes > copyStats.Largest[j].Bytes
		}
		return copyStats.Largest[i].Path < copyStats.Largest[j].Path
	})
	sort.SliceStable(copyStats.Recent, func(i, j int) bool {
		if copyStats.Recent[i].LastModified != copyStats.Recent[j].LastModified {
			return copyStats.Recent[i].LastModified > copyStats.Recent[j].LastModified
		}
		return copyStats.Recent[i].Path < copyStats.Recent[j].Path
	})
	return copyStats
}

func cloneStatsTreemapNode(node *statsTreemapNode) *statsTreemapNode {
	if node == nil {
		return nil
	}
	copyNode := *node
	if len(node.Children) > 0 {
		copyNode.Children = make([]statsTreemapNode, len(node.Children))
		for index := range node.Children {
			child := cloneStatsTreemapNode(&node.Children[index])
			if child != nil {
				copyNode.Children[index] = *child
			}
		}
	}
	return &copyNode
}

func cloneAggregateMap(source map[string]aggregate) map[string]aggregate {
	if source == nil {
		return nil
	}
	out := make(map[string]aggregate, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

func cloneJobState(job jobState) jobState {
	job.Stats = cloneStats(job.Stats)
	job.IntegrityRequest = cloneIntegrityRequest(job.IntegrityRequest)
	job.Integrity = cloneIntegrityResult(job.Integrity)
	job.Entries = append([]string(nil), job.Entries...)
	job.Ceilings = job.Ceilings.clone()
	job.ArchiveExtract = cloneArchiveExtractResult(job.ArchiveExtract)
	job.ArchiveEntryIntegrityRequest = cloneArchiveEntryIntegrityRequest(job.ArchiveEntryIntegrityRequest)
	job.ArchiveEntryIntegrity = cloneArchiveEntryIntegrityResult(job.ArchiveEntryIntegrity)
	if job.TagChange != nil {
		change := *job.TagChange
		change.Add = append([]objectTag(nil), change.Add...)
		change.Remove = append([]string(nil), change.Remove...)
		job.TagChange = &change
	}
	if job.MultipartAbort != nil {
		request := *job.MultipartAbort
		job.MultipartAbort = &request
	}
	if job.MultipartResult != nil {
		counts := *job.MultipartResult
		job.MultipartResult = &counts
	}
	job.StartedAt = cloneTimePointer(job.StartedAt)
	job.EndedAt = cloneTimePointer(job.EndedAt)
	return job
}

type jobManager struct {
	app          *application
	ctx          context.Context
	cancel       context.CancelFunc
	queue        chan string
	mu           sync.RWMutex
	jobs         map[string]*jobState
	lockMu       sync.Mutex
	locks        map[string]*sync.Mutex
	activeMu     sync.Mutex
	activeCancel map[string]context.CancelFunc
	wg           sync.WaitGroup
	historyLimit int
}

func newJobManager(app *application, historyLimit int) (*jobManager, error) {
	if historyLimit <= 0 {
		return nil, fmt.Errorf("job history limit must be positive")
	}
	ctx, cancel := context.WithCancel(context.Background())
	manager := &jobManager{
		app: app, historyLimit: historyLimit, ctx: ctx, cancel: cancel,
		queue: make(chan string, jobQueueSize), jobs: make(map[string]*jobState), locks: make(map[string]*sync.Mutex),
		activeCancel: make(map[string]context.CancelFunc),
	}
	for worker := 0; worker < jobWorkerCount; worker++ {
		manager.wg.Add(1)
		go manager.worker()
	}
	manager.wg.Add(1)
	go manager.reaper()
	return manager, nil
}

func (m *jobManager) close() {
	if m == nil {
		return
	}
	m.cancel()
	m.wg.Wait()
}

func (m *jobManager) reaper() {
	defer m.wg.Done()
	ticker := time.NewTicker(jobReapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case now := <-ticker.C:
			m.reapExpiredPaused(now.UTC())
		}
	}
}

// reapExpiredPaused cancels jobs that have been paused longer than
// pausedJobTTL so they become terminal and release their active-job slot.
func (m *jobManager) reapExpiredPaused(now time.Time) int {
	var expired []string
	m.mu.Lock()
	for id, job := range m.jobs {
		if job == nil || job.Status != jobStatusPaused || now.Sub(job.UpdatedAt) <= pausedJobTTL {
			continue
		}
		job.Status = jobStatusCanceled
		job.Error = ""
		job.UpdatedAt = now
		ended := now
		job.EndedAt = &ended
		expired = append(expired, id)
	}
	m.mu.Unlock()
	for _, id := range expired {
		m.cancelActive(id)
	}
	if len(expired) > 0 {
		_ = m.pruneHistory()
	}
	return len(expired)
}

func (m *jobManager) setActiveCancel(id string, cancel context.CancelFunc) {
	if m == nil || cancel == nil {
		return
	}
	m.activeMu.Lock()
	m.activeCancel[id] = cancel
	m.activeMu.Unlock()
}

func (m *jobManager) clearActiveCancel(id string) {
	if m == nil {
		return
	}
	m.activeMu.Lock()
	delete(m.activeCancel, id)
	m.activeMu.Unlock()
}

func (m *jobManager) cancelActive(id string) {
	if m == nil {
		return
	}
	m.activeMu.Lock()
	cancel := m.activeCancel[id]
	m.activeMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (m *jobManager) jobLock(id string) *sync.Mutex {
	m.lockMu.Lock()
	defer m.lockMu.Unlock()
	lock := m.locks[id]
	if lock == nil {
		lock = &sync.Mutex{}
		m.locks[id] = lock
	}
	return lock
}

func (m *jobManager) get(id string) (jobState, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	job, ok := m.jobs[id]
	if !ok {
		return jobState{}, false
	}
	return cloneJobState(*job), true
}

func (m *jobManager) put(job jobState) error {
	copyJob := cloneJobState(job)
	m.mu.Lock()
	m.jobs[job.ID] = &copyJob
	m.mu.Unlock()
	return nil
}

func terminalJobStatus(status string) bool {
	switch status {
	case jobStatusCompleted, jobStatusFailed, jobStatusCanceled:
		return true
	default:
		return false
	}
}

func (m *jobManager) pruneHistory() error {
	if m == nil || m.historyLimit <= 0 {
		return nil
	}
	m.mu.Lock()
	terminal := make([]*jobState, 0, len(m.jobs))
	for _, job := range m.jobs {
		if terminalJobStatus(job.Status) {
			terminal = append(terminal, job)
		}
	}
	sort.Slice(terminal, func(i, j int) bool {
		if !terminal[i].UpdatedAt.Equal(terminal[j].UpdatedAt) {
			return terminal[i].UpdatedAt.After(terminal[j].UpdatedAt)
		}
		return terminal[i].ID > terminal[j].ID
	})
	if len(terminal) <= m.historyLimit {
		m.mu.Unlock()
		return nil
	}
	remove := append([]*jobState(nil), terminal[m.historyLimit:]...)
	for _, job := range remove {
		delete(m.jobs, job.ID)
	}
	m.mu.Unlock()

	for _, job := range remove {
		m.lockMu.Lock()
		delete(m.locks, job.ID)
		m.lockMu.Unlock()
	}
	return nil
}

const (
	statsInlineWait     = 100 * time.Millisecond
	statsResultReuseTTL = 30 * time.Second
)

func (m *jobManager) reusableStatsJob(instance, prefix string, now time.Time) (jobState, bool) {
	if m == nil {
		return jobState{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var active *jobState
	var completed *jobState
	for _, candidate := range m.jobs {
		if candidate.Type != jobTypeStatsPrefix || candidate.Instance != instance || candidate.Prefix != prefix {
			continue
		}
		switch candidate.Status {
		case jobStatusQueued, jobStatusRunning:
			if active == nil || candidate.UpdatedAt.After(active.UpdatedAt) {
				copyCandidate := cloneJobState(*candidate)
				active = &copyCandidate
			}
		case jobStatusCompleted:
			if candidate.Stats == nil || candidate.EndedAt == nil || now.Sub(*candidate.EndedAt) > statsResultReuseTTL {
				continue
			}
			if completed == nil || candidate.UpdatedAt.After(completed.UpdatedAt) {
				copyCandidate := cloneJobState(*candidate)
				completed = &copyCandidate
			}
		}
	}
	if completed != nil {
		return *completed, true
	}
	if active != nil {
		return *active, true
	}
	return jobState{}, false
}

func (m *jobManager) waitForTerminal(ctx context.Context, id string, maximum time.Duration) (jobState, bool) {
	if maximum <= 0 {
		job, ok := m.get(id)
		return job, ok && terminalJobStatus(job.Status)
	}
	timer := time.NewTimer(maximum)
	defer timer.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		job, ok := m.get(id)
		if !ok {
			return jobState{}, false
		}
		if terminalJobStatus(job.Status) || job.Status == jobStatusPaused {
			return job, true
		}
		select {
		case <-ctx.Done():
			return job, false
		case <-timer.C:
			return job, false
		case <-ticker.C:
		}
	}
}

func (m *jobManager) activeJobCountLocked(excludeID string) int {
	active := 0
	for id, existing := range m.jobs {
		if id == excludeID || existing == nil || terminalJobStatus(existing.Status) {
			continue
		}
		active++
	}
	return active
}

func (m *jobManager) create(job jobState) (jobState, error) {
	if _, ok := m.app.instances[job.Instance]; !ok {
		return jobState{}, apiError{Status: http.StatusNotFound, Code: "unknown_instance", Message: "storage instance was not found"}
	}
	_ = m.pruneHistory()
	id, err := newStateID("job")
	if err != nil {
		return jobState{}, err
	}
	now := time.Now().UTC()
	job.ID = id
	job.Status = jobStatusQueued
	job.CreatedAt = now
	job.UpdatedAt = now
	job.Error = ""
	if job.Type == jobTypeStatsPrefix && job.Stats == nil {
		job.Stats = newStatsResponseWithLimit(job.Instance, job.Prefix, m.app.config.Runtime.MaxStatsFolders)
	}
	copyJob := cloneJobState(job)
	m.mu.Lock()
	if m.activeJobCountLocked("") >= maxActiveJobs {
		m.mu.Unlock()
		return jobState{}, apiError{
			Status:  http.StatusTooManyRequests,
			Code:    "job_limit_reached",
			Message: fmt.Sprintf("the server is already running or queuing %d background jobs", maxActiveJobs),
		}
	}
	m.jobs[job.ID] = &copyJob
	m.mu.Unlock()
	m.enqueue(job.ID)
	return job, nil
}

func (m *jobManager) enqueue(id string) {
	select {
	case m.queue <- id:
	case <-m.ctx.Done():
	}
}

func (m *jobManager) worker() {
	defer m.wg.Done()
	for {
		select {
		case <-m.ctx.Done():
			return
		case id := <-m.queue:
			m.run(id)
		}
	}
}

func (m *jobManager) run(id string) {
	// The execution lock serializes duplicate queue entries. Status controls do
	// not take this lock so pause and cancel remain responsive while a provider
	// request is in flight.
	lock := m.jobLock(id)
	lock.Lock()
	defer lock.Unlock()

	job, ok := m.get(id)
	if !ok || job.Status != jobStatusQueued {
		return
	}
	now := time.Now().UTC()
	job.Status = jobStatusRunning
	job.Error = ""
	job.UpdatedAt = now
	if job.StartedAt == nil {
		job.StartedAt = &now
	}
	if err := m.put(job); err != nil {
		return
	}

	budget := newResourceBudget(m.app.config.Runtime)
	if job.Type == jobTypeIntegrity || job.Type == jobTypeExtractArchive || job.Type == jobTypeArchiveEntryIntegrity ||
		job.Type == jobTypeCopyObject || job.Type == jobTypeMoveObject || job.Type == jobTypeAbortMultiparts {
		budget = newBackgroundResourceBudget(m.app.config.Runtime)
	}
	runContext, runCancel := context.WithCancel(m.ctx)
	m.setActiveCancel(id, runCancel)
	defer func() {
		runCancel()
		m.clearActiveCancel(id)
	}()
	// Background jobs are expected to queue for storage slots instead of failing
	// with the short wait applied to interactive requests.
	jobContext := withAccessCeilings(withUnboundedGateWait(withResourceBudget(runContext, budget)), job.Ceilings)
	err := m.executeRecovered(jobContext, &job)
	usage := budget.usage()
	job.StorageRequests = usage.StorageRequests
	job.StorageBytes = usage.StorageBytes
	latest, ok := m.get(id)
	if ok {
		job = latest
	}
	if errors.Is(err, errJobPaused) || job.Status == jobStatusPaused {
		return
	}
	if errors.Is(err, errJobCanceled) || job.Status == jobStatusCanceled {
		return
	}
	if errors.Is(err, context.Canceled) && m.ctx.Err() != nil {
		job.Status = jobStatusQueued
		job.Error = ""
		job.UpdatedAt = time.Now().UTC()
		job.EndedAt = nil
		_ = m.put(job)
		return
	}
	end := time.Now().UTC()
	job.UpdatedAt = end
	job.EndedAt = &end
	if err != nil {
		job.Status = jobStatusFailed
		job.Error = publicJobError(err)
		if refusal, ok := ceilingErrorOf(err); ok {
			job.Refusal = &refusal
		}
	} else {
		job.Status = jobStatusCompleted
		job.Error = ""
		if job.Stats != nil && job.StartedAt != nil {
			job.Stats.TookMS = end.Sub(*job.StartedAt).Milliseconds()
			job.Stats.TreemapThresholdBytes, job.Stats.Treemap = buildStatsTreemap(job.Stats)
		}
	}
	_ = m.put(job)
	_ = m.pruneHistory()
}

// executeRecovered keeps a panic inside one job from taking down the process
// or its worker; the job is marked failed instead.
func (m *jobManager) executeRecovered(ctx context.Context, job *jobState) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("job %s (%s) panicked: %v", job.ID, job.Type, recovered)
			err = apiError{Status: http.StatusInternalServerError, Code: "job_panicked", Message: "the job failed unexpectedly"}
		}
	}()
	return m.execute(ctx, job)
}

func publicJobError(err error) string {
	if err == nil {
		return ""
	}
	var apiErr apiError
	if errors.As(err, &apiErr) {
		return apiErr.Message
	}
	if budgetErr, ok := resourceLimitAPIError(err); ok {
		return budgetErr.Message
	}
	return publicStorageError(err)
}

func (m *jobManager) execute(ctx context.Context, job *jobState) error {
	instance := m.app.instances[job.Instance]
	switch job.Type {
	case jobTypeCopyPrefix:
		for _, permission := range []string{permissionRead, permissionWrite} {
			if err := requirePermission(instance, permission); err != nil {
				return err
			}
		}
		// The ceilings of read and write: the check of each listed size comes before the first copy.
		if job.LastKey == "" {
			if err := checkListedSizes(ctx, instance, job.Source, actionObjectRead, actionObjectWrite); err != nil {
				return err
			}
		}
		return m.runObjectJob(ctx, job, instance, func(ctx context.Context, object objectInfo, relative string) error {
			destination := job.Target + strings.TrimPrefix(relative, job.Source)
			return m.copyCounted(ctx, job, instance, relative, destination, object.Size)
		})
	case jobTypeMovePrefix:
		for _, permission := range []string{permissionRead, permissionWrite, permissionDelete} {
			if err := requirePermission(instance, permission); err != nil {
				return err
			}
		}
		// The ceilings of read and write: the check of each listed size comes before the first move.
		if job.LastKey == "" {
			if err := checkListedSizes(ctx, instance, job.Source, actionObjectRead, actionObjectWrite); err != nil {
				return err
			}
		}
		err := m.runObjectJob(ctx, job, instance, func(ctx context.Context, object objectInfo, relative string) error {
			destination := job.Target + strings.TrimPrefix(relative, job.Source)
			if instance.movesObjects() {
				// A volume moves each file with one rename.
				if err := instance.Move(ctx, instance.fullKey(relative), instance.fullKey(destination)); err != nil {
					return err
				}
				job.CopiedBytes += object.Size
				return nil
			}
			if err := m.copyCounted(ctx, job, instance, relative, destination, object.Size); err != nil {
				return err
			}
			// A source that Object Lock keeps stays: the job counts it and goes on.
			if err := instance.Delete(ctx, instance.fullKey(relative)); err != nil {
				if !isObjectLocked(err) {
					return err
				}
				job.addLocked(relative)
			}
			return nil
		})
		if err != nil {
			return err
		}
		// A volume keeps the folders of the moved files: the move removes the folders that it emptied.
		return instance.removeEmptyFolders(ctx, instance.fullKey(job.Source))
	case jobTypeCopyObject, jobTypeMoveObject:
		permissions := []string{permissionRead, permissionWrite}
		if job.Type == jobTypeMoveObject {
			permissions = append(permissions, permissionDelete)
		}
		for _, permission := range permissions {
			if err := requirePermission(instance, permission); err != nil {
				return err
			}
		}
		return m.runObjectCopyJob(ctx, job, instance)
	case jobTypeDeletePrefix:
		for _, permission := range []string{permissionRead, permissionDelete} {
			if err := requirePermission(instance, permission); err != nil {
				return err
			}
		}
		if err := m.runDeleteJob(ctx, job, instance); err != nil {
			return err
		}
		// A volume keeps the folders of the deleted files: the delete of a folder removes them.
		return instance.removeEmptyFolders(ctx, instance.fullKey(job.Prefix))
	case jobTypeTagPrefix:
		for _, permission := range []string{permissionRead, permissionWrite} {
			if err := requirePermission(instance, permission); err != nil {
				return err
			}
		}
		return m.runTagJob(ctx, job, instance)
	case jobTypeAbortMultiparts:
		for _, permission := range []string{permissionRead, permissionWrite} {
			if err := requirePermission(instance, permission); err != nil {
				return err
			}
		}
		return m.runAbortMultipartsJob(ctx, job, instance)
	case jobTypeStatsPrefix:
		if err := requirePermission(instance, permissionRead); err != nil {
			return err
		}
		// Older in-memory statistics did not retain exact aggregates for every
		// displayed folder level. Restart those jobs once so local "Others"
		// rectangles can report exact byte and object totals.
		if job.Stats == nil || job.Stats.LayoutVersion != statsLayoutVersion {
			job.Processed = 0
			job.LastKey = ""
			job.Stats = newStatsResponseWithLimit(job.Instance, job.Prefix, m.app.config.Runtime.MaxStatsFolders)
			if err := m.put(*job); err != nil {
				return err
			}
		}
		largest := (*statsEntryHeap)(&job.Stats.Largest)
		heap.Init(largest)
		recent := (*statsRecentHeap)(&job.Stats.Recent)
		heap.Init(recent)
		return m.runObjectJob(ctx, job, instance, func(_ context.Context, object objectInfo, relative string) error {
			if strings.HasSuffix(relative, "/") && object.Size == 0 {
				return nil
			}
			stats := job.Stats
			stats.Count++
			stats.TotalBytes += object.Size
			kind := detectKind(relative, object.ContentType)
			typeAggregate := stats.ByType[kind]
			typeAggregate.Count++
			typeAggregate.Bytes += object.Size
			stats.ByType[kind] = typeAggregate
			modified := ""
			if !object.LastModified.IsZero() {
				modified = object.LastModified.UTC().Format(time.RFC3339)
			}
			entry := statsEntry{
				Path:         relative,
				Bytes:        object.Size,
				Type:         kind,
				MIME:         object.ContentType,
				ETag:         object.ETag,
				LastModified: modified,
			}
			addLargestStatsEntry(&stats.Largest, entry)
			addRecentStatsEntry(&stats.Recent, entry)

			addStatsFolderAggregatesLimited(stats, job.Prefix, relative, object.Size, stats.FolderLimit)

			if !object.LastModified.IsZero() {
				modified := object.LastModified
				if stats.Newest == nil || modified.After(*stats.Newest) {
					stats.Newest = cloneTimePointer(&modified)
				}
				if stats.Oldest == nil || modified.Before(*stats.Oldest) {
					stats.Oldest = cloneTimePointer(&modified)
				}
			}
			return nil
		})
	case jobTypeIntegrity:
		if err := requirePermission(instance, permissionRead); err != nil {
			return err
		}
		return runIntegrityAnalysis(ctx, m, job)
	case jobTypeExtractArchive:
		return runArchiveExtraction(ctx, m, job)
	case jobTypeArchiveEntryIntegrity:
		return runArchiveEntryIntegrity(ctx, m, job)
	default:
		return fmt.Errorf("unknown job type %q", job.Type)
	}
}

func newStatsResponse(instance, prefix string) *statsResponse {
	return newStatsResponseWithLimit(instance, prefix, defaultMaxStatsFolders)
}

func newStatsResponseWithLimit(instance, prefix string, limit int) *statsResponse {
	if limit <= 0 {
		limit = defaultMaxStatsFolders
	}
	return &statsResponse{
		Instance:      instance,
		Prefix:        prefix,
		LayoutVersion: statsLayoutVersion,
		ByType:        make(map[string]aggregate),
		ByFolder:      make(map[string]aggregate),
		FolderLimit:   limit,
	}
}

// addStatsFolderAggregates records exact totals for each ancestor folder that
// may appear in the five-level treemap. Keys are relative to the selected
// prefix, so the frontend can attach a local "Others" rectangle to every
// folder without retaining every object as an individual node.
func addStatsFolderAggregates(stats *statsResponse, prefix, relative string, size int64) {
	addStatsFolderAggregatesLimited(stats, prefix, relative, size, defaultMaxStatsFolders)
}

func addStatsFolderAggregatesLimited(stats *statsResponse, prefix, relative string, size int64, limit int) {
	if stats == nil {
		return
	}
	remaining := strings.TrimPrefix(relative, prefix)
	parts := strings.Split(remaining, "/")
	folderCount := len(parts) - 1
	if folderCount <= 0 {
		return
	}
	if folderCount > maxStatsFolderDepth {
		folderCount = maxStatsFolderDepth
	}
	for depth := 1; depth <= folderCount; depth++ {
		folder := strings.Join(parts[:depth], "/") + "/"
		if _, exists := stats.ByFolder[folder]; !exists && limit > 0 && len(stats.ByFolder) >= limit {
			stats.FoldersTruncated = true
			stats.FolderAggregatesOmitted++
			continue
		}
		value := stats.ByFolder[folder]
		value.Count++
		value.Bytes += size
		stats.ByFolder[folder] = value
	}
}

type statsTreemapBuilder struct {
	node           statsTreemapNode
	children       map[string]*statsTreemapBuilder
	folder         bool
	aggregateKnown bool
}

func newStatsTreemapFolder(name, path string) *statsTreemapBuilder {
	return &statsTreemapBuilder{
		node:     statsTreemapNode{Name: name, Path: path, Kind: "folder", Type: "folder"},
		children: make(map[string]*statsTreemapBuilder),
		folder:   true,
	}
}

func statsTreemapRootName(prefix string) string {
	trimmed := strings.Trim(strings.TrimSpace(prefix), "/")
	if trimmed == "" {
		return "/"
	}
	parts := strings.Split(trimmed, "/")
	return parts[len(parts)-1]
}

func normalizedStatsTreemapPath(value, prefix string) string {
	clean := strings.TrimLeft(strings.TrimSpace(value), "/")
	if clean == "" || clean == "(root)" {
		return ""
	}
	cleanPrefix := normalizePrefix(cleanRelativeKey(prefix))
	if cleanPrefix != "" && strings.HasPrefix(clean, cleanPrefix) {
		clean = strings.TrimPrefix(clean, cleanPrefix)
	}
	return normalizePrefix(clean)
}

func ensureStatsTreemapFolder(root *statsTreemapBuilder, parts []string, prefix string) *statsTreemapBuilder {
	parent := root
	pathValue := normalizePrefix(cleanRelativeKey(prefix))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		pathValue += part + "/"
		key := "folder:" + part
		child := parent.children[key]
		if child == nil {
			child = newStatsTreemapFolder(part, pathValue)
			parent.children[key] = child
		}
		parent = child
	}
	return parent
}

func finalizeStatsTreemapBuilder(node *statsTreemapBuilder) {
	if node == nil {
		return
	}
	var childBytes int64
	var childCount int64
	for _, child := range node.children {
		finalizeStatsTreemapBuilder(child)
		childBytes += maxInt64(0, child.node.Bytes)
		childCount += maxInt64(0, child.node.Count)
	}
	if node.folder {
		if !node.aggregateKnown {
			node.node.Bytes = childBytes
			node.node.Count = childCount
		} else {
			node.node.Bytes = maxInt64(node.node.Bytes, childBytes)
			node.node.Count = maxInt64(node.node.Count, childCount)
		}
	}
}

func statsTreemapThreshold(totalBytes int64) int64 {
	if totalBytes <= 0 {
		return 0
	}
	// Ceiling division keeps objects that are exactly one percent visible while
	// avoiding floating-point differences between the backend and browser.
	return (totalBytes + 99) / 100
}

func materializeStatsTreemap(node *statsTreemapBuilder, threshold int64) statsTreemapNode {
	result := node.node
	if !node.folder {
		return result
	}

	children := make([]*statsTreemapBuilder, 0, len(node.children))
	for _, child := range node.children {
		if child != nil && child.node.Bytes > 0 {
			children = append(children, child)
		}
	}
	sort.SliceStable(children, func(i, j int) bool {
		if children[i].node.Bytes != children[j].node.Bytes {
			return children[i].node.Bytes > children[j].node.Bytes
		}
		return children[i].node.Name < children[j].node.Name
	})

	var listedBytes int64
	var listedCount int64
	for _, child := range children {
		listedBytes += maxInt64(0, child.node.Bytes)
		listedCount += maxInt64(0, child.node.Count)
	}
	otherBytes := maxInt64(0, result.Bytes-listedBytes)
	otherCount := maxInt64(0, result.Count-listedCount)
	visible := make([]statsTreemapNode, 0, len(children)+1)
	for _, child := range children {
		if threshold == 0 || child.node.Bytes >= threshold {
			visible = append(visible, materializeStatsTreemap(child, threshold))
			continue
		}
		otherBytes += maxInt64(0, child.node.Bytes)
		otherCount += maxInt64(0, child.node.Count)
	}
	if otherBytes > 0 || otherCount > 0 {
		visible = append(visible, statsTreemapNode{
			Name: "Others", Path: result.Path, Bytes: otherBytes, Count: otherCount,
			Kind: "other", Type: "other",
		})
	}
	sort.SliceStable(visible, func(i, j int) bool {
		if visible[i].Bytes != visible[j].Bytes {
			return visible[i].Bytes > visible[j].Bytes
		}
		return visible[i].Name < visible[j].Name
	})

	// Contract every folder chain that does not introduce a real branch. This
	// is deliberately a backend responsibility so every client receives the
	// same semantic tree and cannot render redundant single-child rectangles.
	for len(visible) == 1 {
		only := visible[0]
		switch {
		case only.Kind == "folder" && len(only.Children) > 0:
			visible = append([]statsTreemapNode(nil), only.Children...)
		case only.Kind == "file":
			// Preserve the only meaningful filename and action target instead of
			// replacing it with an otherwise uninformative root rectangle.
			result.Name = only.Name
			result.Path = only.Path
			result.Kind = only.Kind
			result.Type = only.Type
			result.MIME = only.MIME
			result.ETag = only.ETag
			result.LastModified = only.LastModified
			result.Children = nil
			return result
		default:
			// A sole Others node adds no information. The parent already carries
			// the exact byte and object totals.
			visible = nil
		}
	}
	result.Children = visible
	return result
}

func buildStatsTreemap(stats *statsResponse) (int64, *statsTreemapNode) {
	if stats == nil {
		return 0, nil
	}
	prefix := normalizePrefix(cleanRelativeKey(stats.Prefix))
	root := newStatsTreemapFolder(statsTreemapRootName(prefix), prefix)
	root.node.Bytes = maxInt64(0, stats.TotalBytes)
	root.node.Count = maxInt64(0, stats.Count)
	root.aggregateKnown = true

	folderPaths := make([]string, 0, len(stats.ByFolder))
	for folderPath := range stats.ByFolder {
		folderPaths = append(folderPaths, folderPath)
	}
	sort.SliceStable(folderPaths, func(i, j int) bool {
		left := normalizedStatsTreemapPath(folderPaths[i], prefix)
		right := normalizedStatsTreemapPath(folderPaths[j], prefix)
		leftDepth := strings.Count(left, "/")
		rightDepth := strings.Count(right, "/")
		if leftDepth != rightDepth {
			return leftDepth < rightDepth
		}
		return left < right
	})
	for _, folderPath := range folderPaths {
		relative := normalizedStatsTreemapPath(folderPath, prefix)
		parts := strings.Split(strings.Trim(relative, "/"), "/")
		if relative == "" || len(parts) == 0 {
			continue
		}
		folder := ensureStatsTreemapFolder(root, parts, prefix)
		aggregateValue := stats.ByFolder[folderPath]
		folder.node.Bytes = maxInt64(0, aggregateValue.Bytes)
		folder.node.Count = maxInt64(0, aggregateValue.Count)
		folder.aggregateKnown = true
	}

	for _, entry := range stats.Largest {
		fullPath := strings.TrimLeft(strings.TrimSpace(entry.Path), "/")
		if fullPath == "" || (prefix != "" && !strings.HasPrefix(fullPath, prefix)) {
			continue
		}
		relative := fullPath
		if prefix != "" {
			relative = strings.TrimPrefix(fullPath, prefix)
		}
		parts := strings.Split(strings.Trim(relative, "/"), "/")
		if len(parts) == 0 || parts[0] == "" {
			continue
		}
		fileName := parts[len(parts)-1]
		parent := ensureStatsTreemapFolder(root, parts[:len(parts)-1], prefix)
		parent.children["file:"+fileName] = &statsTreemapBuilder{
			node: statsTreemapNode{
				Name: fileName, Path: fullPath, Bytes: maxInt64(0, entry.Bytes), Count: 1,
				Kind: "file", Type: entry.Type, MIME: entry.MIME, ETag: entry.ETag,
				LastModified: entry.LastModified,
			},
			children: make(map[string]*statsTreemapBuilder),
		}
	}

	finalizeStatsTreemapBuilder(root)
	threshold := statsTreemapThreshold(root.node.Bytes)
	tree := materializeStatsTreemap(root, threshold)
	return threshold, &tree
}

func (m *jobManager) runObjectJob(ctx context.Context, job *jobState, instance *storageInstance, operation func(context.Context, objectInfo, string) error) error {
	prefix := job.Prefix
	if prefix == "" {
		prefix = job.Source
	}
	processedThisRun := int64(0)
	lastCheckpoint := time.Now()
	err := forEachObjectAfter(ctx, instance, prefix, job.LastKey, func(object objectInfo, relative string) error {
		if err := m.controlState(job.ID); err != nil {
			return err
		}
		if err := operation(ctx, object, relative); err != nil {
			return err
		}
		job.Processed++
		processedThisRun++
		job.LastKey = relative
		job.UpdatedAt = time.Now().UTC()
		latest, ok := m.get(job.ID)
		if ok {
			job.Status = latest.Status
			job.EndedAt = cloneTimePointer(latest.EndedAt)
			// A resume request can change paused to queued before the active
			// worker observes the pause. That worker already owns the execution
			// lock, so it can safely continue without starting a duplicate run.
			if job.Status == jobStatusQueued {
				job.Status = jobStatusRunning
			}
		}
		forceCheckpoint := job.Type != jobTypeStatsPrefix ||
			processedThisRun%100 == 0 ||
			time.Since(lastCheckpoint) >= time.Second ||
			job.Status == jobStatusPaused ||
			job.Status == jobStatusCanceled
		if forceCheckpoint {
			if err := m.put(*job); err != nil {
				return err
			}
			lastCheckpoint = time.Now()
		}
		switch job.Status {
		case jobStatusPaused:
			return errJobPaused
		case jobStatusCanceled:
			return errJobCanceled
		}
		return nil
	})
	if err != nil {
		return err
	}
	if job.Type == jobTypeStatsPrefix && processedThisRun > 0 {
		if err := m.put(*job); err != nil {
			return err
		}
	}
	if job.Processed == 0 && processedThisRun == 0 && job.Type != jobTypeStatsPrefix {
		return apiError{Status: http.StatusNotFound, Code: "empty_prefix", Message: "source prefix contains no objects"}
	}
	return nil
}

// copyCounted copies one object of a copy job and adds its size to the copied bytes of the job. During a copy in parts,
// the stored job shows the copied parts, so a poll of the job sees the progress.
func (m *jobManager) copyCounted(ctx context.Context, job *jobState, instance *storageInstance, source, destination string, size int64) error {
	job.CopyingBytes = 0
	copying := int64(0)
	err := instance.CopySized(ctx, instance.fullKey(source), instance.fullKey(destination), size, func(part int64) {
		copying += part
		m.setCopyingBytes(job.ID, copying)
	})
	if err != nil {
		m.setCopyingBytes(job.ID, 0)
		return err
	}
	job.CopiedBytes += size
	return nil
}

// lowerCeilings gives the stored job the lower value of each ceiling of its own ceilings and of the ceilings given.
func (m *jobManager) lowerCeilings(id string, ceilings accessCeilings) {
	if len(ceilings) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if job := m.jobs[id]; job != nil {
		job.Ceilings = job.Ceilings.merge(ceilings)
	}
}

// setCopyingBytes changes only the copied parts of the stored job. The job loop keeps the other fields.
func (m *jobManager) setCopyingBytes(id string, bytes int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if job := m.jobs[id]; job != nil && !terminalJobStatus(job.Status) {
		job.CopyingBytes = bytes
		job.UpdatedAt = time.Now().UTC()
	}
}

// runObjectCopyJob copies or moves one large object. A pause or a cancel aborts the copy in parts; a resume starts it
// again from the first part.
func (m *jobManager) runObjectCopyJob(ctx context.Context, job *jobState, instance *storageInstance) error {
	if err := m.controlState(job.ID); err != nil {
		return err
	}
	job.CopiedBytes = 0
	if err := m.copyCounted(ctx, job, instance, job.Source, job.Target, job.TotalBytes); err != nil {
		return err
	}
	if job.Type == jobTypeMoveObject {
		if err := instance.Delete(ctx, instance.fullKey(job.Source)); err != nil {
			return sourceDeletionError(err)
		}
	}
	job.Processed = 1
	job.LastKey = job.Source
	job.UpdatedAt = time.Now().UTC()
	if latest, ok := m.get(job.ID); ok {
		job.Status = latest.Status
		job.EndedAt = cloneTimePointer(latest.EndedAt)
		if job.Status == jobStatusQueued {
			job.Status = jobStatusRunning
		}
	}
	return m.put(*job)
}

// runDeleteJob deletes the objects of a prefix in batches: one request for each listing page, up to
// maximumS3DeleteBatch keys. The job counts and checkpoints a batch only after the provider deleted it, so a pause or
// a stop between two batches resumes after the last deleted key.
func (m *jobManager) runDeleteJob(ctx context.Context, job *jobState, instance *storageInstance) error {
	prefix := job.Prefix
	batch := make([]string, 0, maximumS3DeleteBatch)
	processedThisRun := int64(0)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		keys := make([]string, len(batch))
		for index, relative := range batch {
			keys[index] = instance.fullKey(relative)
		}
		locked, err := instance.DeleteMany(ctx, keys)
		if err != nil {
			return err
		}
		// A key that Object Lock keeps stays in the bucket: the job counts it as locked, not as deleted.
		for _, full := range locked {
			relative, _ := instance.relativeKey(full)
			job.addLocked(relative)
		}
		job.Processed += int64(len(batch) - len(locked))
		processedThisRun += int64(len(batch))
		job.LastKey = batch[len(batch)-1]
		batch = batch[:0]
		job.UpdatedAt = time.Now().UTC()
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
		return nil
	}
	err := forEachObjectAfter(ctx, instance, prefix, job.LastKey, func(_ objectInfo, relative string) error {
		if err := m.controlState(job.ID); err != nil {
			return err
		}
		batch = append(batch, relative)
		if len(batch) >= maximumS3DeleteBatch {
			return flush()
		}
		return nil
	})
	if err == nil {
		err = flush()
	}
	if err != nil {
		return err
	}
	if job.Processed == 0 && job.Locked == 0 && processedThisRun == 0 {
		return apiError{Status: http.StatusNotFound, Code: "empty_prefix", Message: "source prefix contains no objects"}
	}
	return nil
}

// addLocked counts a key that Object Lock kept, and keeps the first maxLockedKeys keys to show them.
func (j *jobState) addLocked(key string) {
	j.Locked++
	if len(j.LockedKeys) < maxLockedKeys {
		j.LockedKeys = append(j.LockedKeys, key)
	}
}

// sourceDeletionError is the error of a move whose copy is made and whose source stays.
func sourceDeletionError(err error) error {
	if isObjectLocked(err) {
		return apiError{Status: http.StatusConflict, Code: "object_locked", Message: "The copy is made, but the source stays. " + err.Error()}
	}
	return fmt.Errorf("copy succeeded but source deletion failed: %w", err)
}

func (m *jobManager) controlState(id string) error {
	// Read only the status under the lock. Cloning the whole job here would make
	// per-iteration checks cost O(len(Entries)).
	m.mu.RLock()
	job, ok := m.jobs[id]
	status := ""
	if ok && job != nil {
		status = job.Status
	}
	m.mu.RUnlock()
	if !ok {
		return errJobCanceled
	}
	switch status {
	case jobStatusPaused:
		return errJobPaused
	case jobStatusCanceled:
		return errJobCanceled
	}
	select {
	case <-m.ctx.Done():
		return m.ctx.Err()
	default:
		return nil
	}
}

// interrupted reports why a long CPU-bound loop inside a job should stop: its
// run context was canceled (pause or cancel) or the job state changed.
func (m *jobManager) interrupted(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		if control := m.controlState(id); control != nil {
			return control
		}
		return err
	}
	return m.controlState(id)
}

func forEachObjectAfter(ctx context.Context, instance *storageInstance, prefix, lastKey string, callback func(objectInfo, string) error) error {
	fullPrefix := instance.fullKey(prefix)
	for {
		options := listOptions{Prefix: fullPrefix, Delimiter: "", MaxResults: 1000}
		if lastKey != "" {
			options.StartAfter = instance.fullKey(lastKey)
		}
		page, err := instance.List(ctx, options)
		if err != nil {
			return err
		}
		sort.Slice(page.Objects, func(i, j int) bool { return page.Objects[i].Key < page.Objects[j].Key })
		progressed := false
		for _, object := range page.Objects {
			relative, ok := instance.relativeKey(object.Key)
			if !ok || relative == "" || !strings.HasPrefix(relative, prefix) || (lastKey != "" && relative <= lastKey) {
				continue
			}
			if err := callback(object, relative); err != nil {
				return err
			}
			lastKey = relative
			progressed = true
		}
		if !progressed || (page.NextPageToken == "" && len(page.Objects) < options.MaxResults) {
			return nil
		}
	}
}

func (m *jobManager) changeStatus(id, action string) (jobState, error) {
	now := time.Now().UTC()
	m.mu.Lock()
	stored := m.jobs[id]
	if stored == nil {
		m.mu.Unlock()
		return jobState{}, errUnknownJob()
	}
	job := cloneJobState(*stored)
	switch action {
	case "pause":
		if job.Status != jobStatusQueued && job.Status != jobStatusRunning {
			m.mu.Unlock()
			return jobState{}, apiError{Status: http.StatusConflict, Code: "job_not_active", Message: "only queued or running jobs can be paused"}
		}
		job.Status = jobStatusPaused
	case "resume":
		if job.Status != jobStatusPaused && job.Status != jobStatusFailed {
			m.mu.Unlock()
			return jobState{}, apiError{Status: http.StatusConflict, Code: "job_not_resumable", Message: "only paused or failed jobs can be resumed"}
		}
		if m.activeJobCountLocked(job.ID) >= maxActiveJobs {
			m.mu.Unlock()
			return jobState{}, apiError{Status: http.StatusTooManyRequests, Code: "job_limit_reached", Message: fmt.Sprintf("the server is already running or queuing %d background jobs", maxActiveJobs)}
		}
		job.Status = jobStatusQueued
		job.Error = ""
		job.EndedAt = nil
	case "cancel":
		if job.Status == jobStatusCompleted || job.Status == jobStatusCanceled {
			m.mu.Unlock()
			return job, nil
		}
		job.Status = jobStatusCanceled
		job.Error = ""
		job.EndedAt = &now
	default:
		m.mu.Unlock()
		return jobState{}, apiError{Status: http.StatusBadRequest, Code: "invalid_job_action", Message: "unknown job action"}
	}
	job.UpdatedAt = now
	copyJob := cloneJobState(job)
	m.jobs[id] = &copyJob
	m.mu.Unlock()

	if action == "pause" || action == "cancel" {
		m.cancelActive(id)
	}
	if action == "cancel" {
		_ = m.pruneHistory()
	}
	if action == "resume" {
		m.enqueue(job.ID)
	}
	return job, nil
}

func validatePrefixOperation(source, target string) (string, string, error) {
	source = normalizePrefix(cleanRelativeKey(source))
	target = normalizePrefix(cleanRelativeKey(target))
	if source == "" || target == "" {
		return "", "", apiError{Status: http.StatusBadRequest, Code: "invalid_path", Message: "src and dst are required"}
	}
	if source == target {
		return "", "", apiError{Status: http.StatusBadRequest, Code: "same_path", Message: "src and dst must be different"}
	}
	if strings.HasPrefix(target, source) {
		return "", "", apiError{Status: http.StatusBadRequest, Code: "recursive_destination", Message: "destination cannot be inside source prefix"}
	}
	if strings.HasPrefix(source, target) {
		return "", "", apiError{Status: http.StatusBadRequest, Code: "overlapping_prefixes", Message: "destination cannot contain source prefix because object paths would overlap"}
	}
	return source, target, nil
}

// errUnknownJob is the answer for a job that does not exist, for a job that has no access rule, and for a job that the
// subject cannot do again. The three cases give the same answer.
func errUnknownJob() apiError {
	return apiError{Status: http.StatusNotFound, Code: "unknown_job", Message: "job was not found"}
}

// authorizeJob asks again the permissions of the operation that made the job, without usage. A refusal gives the same
// answer as an unknown job. A failure of the PDP and a budget refusal keep their status.
func (a *application) authorizeJob(r *http.Request, job jobState) error {
	if !a.config.Access.enforced() {
		return nil
	}
	op, ok := a.jobOperation(job)
	if !ok {
		return errUnknownJob()
	}
	if err := a.authorizeContinuation(r, op); err != nil {
		if isForbiddenError(err) {
			return errUnknownJob()
		}
		return err
	}
	return nil
}

func (a *application) handleJobs(w http.ResponseWriter, r *http.Request) {
	if a.jobs == nil {
		writeAPIError(w, fmt.Errorf("job manager is not initialized"))
		return
	}
	remainder := strings.TrimPrefix(r.URL.Path, "/api/jobs/")
	parts := strings.Split(remainder, "/")
	if len(parts) == 0 || parts[0] == "" {
		writeAPIError(w, errUnknownJob())
		return
	}
	id := parts[0]
	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		job, ok := a.jobs.get(id)
		if !ok {
			writeAPIError(w, errUnknownJob())
			return
		}
		if err := a.authorizeJob(r, job); err != nil {
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, job.public())
		return
	}
	if len(parts) != 2 || r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	job, ok := a.jobs.get(id)
	if !ok {
		writeAPIError(w, errUnknownJob())
		return
	}
	switch parts[1] {
	case "pause", "resume", "cancel":
	default:
		writeAPIError(w, apiError{Status: http.StatusBadRequest, Code: "invalid_job_action", Message: "unknown job action"})
		return
	}
	if err := a.authorizeJob(r, job); err != nil {
		writeAPIError(w, err)
		return
	}
	if parts[1] == "resume" {
		// The answers of the PDP can have a lower ceiling now: the job runs again with the lower value of each ceiling.
		a.jobs.lowerCeilings(id, accessCeilingsOf(r.Context()))
	}
	job, err := a.jobs.changeStatus(id, parts[1])
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, job.public())
}
