package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The client of the AuthZEN PDP (docs/ACCESS.md, ProcurAuth 4.4, 6.4, 6.11, 6.12, 7.3 to 7.6, 10.1, 10.3, 10.6, 10.7).
//
// The client sends each question to the PDP. It never sends a question again: a lost answer is a failure. It gives a
// failed answer when the PDP does not give a valid answer, and the route then answers 503. It keeps the answers for
// cache_ttl at most, in two separate tables.

const (
	pdpEvaluationPath  = "/access/v1/evaluation"
	pdpEvaluationsPath = "/access/v1/evaluations"

	// maxPDPAnswerBytes is the largest body of an answer that the client reads.
	maxPDPAnswerBytes = 256 << 10

	pdpCacheMaxEntries    = 16384
	pdpCacheMaxBytes      = 8 << 20
	pdpCacheEntryOverhead = 96

	pdpDialTimeout       = 5 * time.Second
	pdpIdleConnTimeout   = 5 * time.Second
	pdpMaxIdleConns      = 4
	pdpReasonBudget      = "budget_exceeded"
	pdpSourcePDP         = "pdp"
	pdpSourceCache       = "cache"
	pdpSourceError       = "error"
	pdpMatchNode         = "node"
	pdpMatchSubtree      = "subtree"
	pdpMaxLogTokenLength = 128
)

// pdpQuestion is one question to the PDP.
type pdpQuestion struct {
	Action   string
	Resource accessResource
	// Subtree is the match "subtree": a grant below the resource also gives true. The server uses it only to show
	// or hide an element of the interface, never to let an operation run (ProcurAuth 4.4).
	Subtree bool
}

// match gives the value of context.match.
func (q pdpQuestion) match() string {
	if q.Subtree {
		return pdpMatchSubtree
	}
	return pdpMatchNode
}

// pdpAnswer is the answer to one question.
type pdpAnswer struct {
	Decision bool
	// Failed is true when the client has no valid answer. The route then answers 503.
	Failed bool
	// Reason is the context.reason of a false answer, or "".
	Reason string
	// RetryAfter is the context.retry_after of a false answer with the reason budget_exceeded. It is at least 1.
	RetryAfter int64
	// Limits is the context.limits of a true answer. Each value is at least 0.
	Limits map[string]int64
	// Units is the context.usage.units of a true answer. It is valid only when HasUnits is true.
	Units    int64
	HasUnits bool
	// Source is "pdp", "cache" or "error".
	Source string
}

// pdpCacheEntry is one kept answer. For a kept budget_exceeded answer, answer.RetryAfter is the first value.
type pdpCacheEntry struct {
	answer  pdpAnswer
	expires time.Time
}

// pdpClient asks the PDP. It is safe for concurrent use.
type pdpClient struct {
	baseURL        string
	token          string
	timeout        time.Duration
	usageTimeout   time.Duration
	cacheTTL       time.Duration
	maxEvaluations int
	logCacheHits   bool
	client         *http.Client
	// now is the clock of the cache. A test replaces it.
	now func() time.Time

	// The bounds of the two tables together. A test makes them smaller.
	maxEntries int
	maxBytes   int

	mu         sync.Mutex
	decideKept map[string]pdpCacheEntry
	useKept    map[string]pdpCacheEntry
	keptBytes  int
}

// newPDPClient makes the client from the access block. logCacheHits is true when server.log_mode is "detailed": a
// cache hit then writes an access.decision line too, and the line shows the folder ids (loggedResourcePath).
func newPDPClient(cfg accessConfig, logCacheHits bool) *pdpClient {
	transport := &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: pdpDialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: pdpDialTimeout,
		MaxIdleConns:        pdpMaxIdleConns,
		MaxIdleConnsPerHost: pdpMaxIdleConns,
		IdleConnTimeout:     pdpIdleConnTimeout,
	}
	return &pdpClient{
		baseURL:        cfg.PDPURL,
		token:          cfg.PDPToken,
		timeout:        cfg.PDPTimeout,
		usageTimeout:   cfg.PDPUsageTimeout,
		cacheTTL:       cfg.PDPCacheTTL,
		maxEvaluations: cfg.PDPMaxEvaluations,
		logCacheHits:   logCacheHits,
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		now:        time.Now,
		maxEntries: pdpCacheMaxEntries,
		maxBytes:   pdpCacheMaxBytes,
		decideKept: make(map[string]pdpCacheEntry),
		useKept:    make(map[string]pdpCacheEntry),
	}
}

// close closes the idle connections to the PDP.
func (c *pdpClient) close() {
	if c == nil {
		return
	}
	closeHTTPClient(c.client)
}

// failedAnswer is the answer when the client has no valid answer.
func failedAnswer() pdpAnswer {
	return pdpAnswer{Failed: true, Source: pdpSourceError}
}

// pdpCacheKey gives the key of a kept answer: the subject, the action, the resource with its chain and the match.
// The length of the subject makes the key without ambiguity.
func pdpCacheKey(subject string, question pdpQuestion) string {
	return strconv.Itoa(len(subject)) + ":" + subject + "\x1f" + question.Action + "\x1f" + question.Resource.path() + "\x1f" + question.match()
}

// decide asks questions without usage. One question is one Access Evaluation. Several questions are Access
// Evaluations requests of PDPMaxEvaluations items at most. One deadline (PDPTimeout) covers the whole call. The
// client sends the same question one time. The answers are in the order of the questions. When a request fails, the
// client sends no other request of the call, and each question without an answer is failed.
func (c *pdpClient) decide(ctx context.Context, requestID, subject string, questions []pdpQuestion) []pdpAnswer {
	out := make([]pdpAnswer, len(questions))
	if len(questions) == 0 {
		return out
	}
	groups := make(map[string][]int, len(questions))
	order := make([]string, 0, len(questions))
	for index, question := range questions {
		key := pdpCacheKey(subject, question)
		if _, found := groups[key]; !found {
			order = append(order, key)
		}
		groups[key] = append(groups[key], index)
	}

	pending := make([]string, 0, len(order))
	now := c.now()
	c.mu.Lock()
	for _, key := range order {
		if answer, hit := c.lookupLocked(c.decideKept, key, now, false); hit {
			for _, index := range groups[key] {
				out[index] = answer
			}
			continue
		}
		pending = append(pending, key)
	}
	c.mu.Unlock()

	if len(pending) > 0 {
		c.askPending(ctx, requestID, subject, questions, out, groups, pending)
	}
	for index, question := range questions {
		c.logDecision(requestID, subject, question, false, out[index])
	}
	return out
}

// askPending sends the questions that no kept answer covers. It writes each answer in out.
func (c *pdpClient) askPending(ctx context.Context, requestID, subject string, questions []pdpQuestion, out []pdpAnswer, groups map[string][]int, pending []string) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	step := c.maxEvaluations
	if step < 1 {
		step = defaultPDPMaxEvaluations
	}
	stopped := false
	for start := 0; start < len(pending); start += step {
		end := start + step
		if end > len(pending) {
			end = len(pending)
		}
		chunk := pending[start:end]
		answers := make([]pdpAnswer, len(chunk))
		if stopped {
			for position := range answers {
				answers[position] = failedAnswer()
			}
		} else {
			asked := make([]pdpQuestion, len(chunk))
			for position, key := range chunk {
				asked[position] = questions[groups[key][0]]
			}
			var ok bool
			answers, ok = c.evaluate(ctx, requestID, subject, asked)
			stopped = !ok
		}
		now := c.now()
		c.mu.Lock()
		for position, key := range chunk {
			answer := answers[position]
			for _, index := range groups[key] {
				out[index] = answer
			}
			if !answer.Failed {
				c.storeLocked(c.decideKept, key, answer, c.cacheTTL, now)
			}
		}
		c.mu.Unlock()
	}
}

// use asks one question with context.usage {"units":1} as one Access Evaluation. The deadline is PDPUsageTimeout. The
// client never sends the question again (ProcurAuth 10.6). The question must not have the match "subtree". A true
// answer without usage.units gives the decision false (ProcurAuth 6.12).
func (c *pdpClient) use(ctx context.Context, requestID, subject string, question pdpQuestion) pdpAnswer {
	if question.Subtree {
		// A caller bug. The client sends no request.
		log.Printf("access.pdp_failed request_id=%s evaluations=1 error=%q", pdpLogToken(requestID), "a usage question cannot have the match subtree")
		answer := failedAnswer()
		c.logDecision(requestID, subject, question, true, answer)
		return answer
	}
	key := pdpCacheKey(subject, question)
	c.mu.Lock()
	cached, hit := c.lookupLocked(c.useKept, key, c.now(), true)
	c.mu.Unlock()
	if hit {
		c.logDecision(requestID, subject, question, true, cached)
		return cached
	}

	ctx, cancel := context.WithTimeout(ctx, c.usageTimeout)
	defer cancel()
	answer := failedAnswer()
	body, err := json.Marshal(pdpEvaluation{Subject: pdpSubjectOf(subject), Action: pdpActionOf(question.Action), Resource: question.Resource, Context: &pdpContext{Usage: &pdpUsage{Units: 1}}})
	if err == nil {
		var data []byte
		data, err = c.post(ctx, requestID, pdpEvaluationPath, body)
		if err == nil {
			answer, err = parsePDPAnswer(data, true)
		}
	}
	if err != nil {
		c.logFailure(requestID, 1, err)
		answer = failedAnswer()
	} else {
		now := c.now()
		c.mu.Lock()
		c.storeUseLocked(key, answer, now)
		c.mu.Unlock()
	}
	c.logDecision(requestID, subject, question, true, answer)
	return answer
}

type pdpSubject struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type pdpAction struct {
	Name string `json:"name"`
}

type pdpUsage struct {
	Units int `json:"units"`
}

// pdpContext is the context of a question. A question has either a match or a usage, never both.
type pdpContext struct {
	Match string    `json:"match,omitempty"`
	Usage *pdpUsage `json:"usage,omitempty"`
}

// pdpEvaluation is an Access Evaluation request. In an item of an Access Evaluations request, the subject is empty
// and the request holds it at the top.
type pdpEvaluation struct {
	Subject  *pdpSubject    `json:"subject,omitempty"`
	Action   pdpAction      `json:"action"`
	Resource accessResource `json:"resource"`
	Context  *pdpContext    `json:"context,omitempty"`
}

func pdpSubjectOf(id string) *pdpSubject {
	return &pdpSubject{Type: "user", ID: id}
}

func pdpActionOf(name string) pdpAction {
	return pdpAction{Name: name}
}

// questionContext gives the context of a question without usage: only the match "subtree" needs one.
func questionContext(question pdpQuestion) *pdpContext {
	if question.Subtree {
		return &pdpContext{Match: pdpMatchSubtree}
	}
	return nil
}

// evaluate sends one request for the questions: an Access Evaluation for one question, an Access Evaluations
// request for more. The result has one answer for each question. The boolean is false when the request failed as a
// whole (no answer, bad status, bad body, or a wrong count of items).
func (c *pdpClient) evaluate(ctx context.Context, requestID, subject string, asked []pdpQuestion) ([]pdpAnswer, bool) {
	failAll := func(err error) ([]pdpAnswer, bool) {
		c.logFailure(requestID, len(asked), err)
		out := make([]pdpAnswer, len(asked))
		for index := range out {
			out[index] = failedAnswer()
		}
		return out, false
	}
	var body []byte
	var err error
	path := pdpEvaluationPath
	if len(asked) == 1 {
		question := asked[0]
		body, err = json.Marshal(pdpEvaluation{Subject: pdpSubjectOf(subject), Action: pdpActionOf(question.Action), Resource: question.Resource, Context: questionContext(question)})
	} else {
		path = pdpEvaluationsPath
		items := make([]pdpEvaluation, len(asked))
		for index, question := range asked {
			items[index] = pdpEvaluation{Action: pdpActionOf(question.Action), Resource: question.Resource, Context: questionContext(question)}
		}
		body, err = json.Marshal(struct {
			Subject     *pdpSubject     `json:"subject"`
			Evaluations []pdpEvaluation `json:"evaluations"`
		}{pdpSubjectOf(subject), items})
	}
	if err != nil {
		return failAll(err)
	}
	data, err := c.post(ctx, requestID, path, body)
	if err != nil {
		return failAll(err)
	}
	if len(asked) == 1 {
		answer, err := parsePDPAnswer(data, false)
		if err != nil {
			return failAll(err)
		}
		return []pdpAnswer{answer}, true
	}
	var batch struct {
		Evaluations []json.RawMessage `json:"evaluations"`
	}
	if err := decodePDPDocument(data, &batch); err != nil {
		return failAll(err)
	}
	if len(batch.Evaluations) != len(asked) {
		return failAll(fmt.Errorf("the answer has %d items for %d questions", len(batch.Evaluations), len(asked)))
	}
	out := make([]pdpAnswer, len(asked))
	for index, item := range batch.Evaluations {
		answer, err := parsePDPAnswer(item, false)
		if err != nil {
			c.logFailure(requestID, 1, err)
			answer = failedAnswer()
		}
		out[index] = answer
	}
	return out, true
}

// pdpStatusError is the error of an HTTP status other than 200.
type pdpStatusError struct{ status int }

func (e *pdpStatusError) Error() string { return "HTTP status " + strconv.Itoa(e.status) }

// post sends one request and gives the body of a 200 answer. It reads each body to the end, within a bound, and
// closes it: the connection then serves the next question. It never sends the request again.
func (c *pdpClient) post(ctx context.Context, requestID, path string, body []byte) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("the request is not valid")
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Request-ID", requestID)
	response, err := c.client.Do(request)
	if err != nil {
		return nil, transportError(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxPDPAnswerBytes))
		return nil, &pdpStatusError{status: response.StatusCode}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxPDPAnswerBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read the answer: %w", transportError(err))
	}
	if len(data) > maxPDPAnswerBytes {
		return nil, fmt.Errorf("the answer is larger than %d bytes", maxPDPAnswerBytes)
	}
	return data, nil
}

// transportError removes the URL from an error of the HTTP client. The URL is not secret, but the log needs the cause only.
func transportError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("no answer before the deadline")
	}
	return fmt.Errorf("no answer: %v", err)
}

// decodePDPDocument decodes one JSON document into target. A body with data after the document is not valid.
func decodePDPDocument(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(target); err != nil {
		return errors.New("the answer is not valid JSON of the expected form")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("the answer has data after the JSON document")
	}
	return nil
}

// decodePDPObject decodes a JSON object into its members. null and any other JSON type are not valid.
func decodePDPObject(data []byte, what string) (map[string]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, fmt.Errorf("%s is not a JSON object", what)
	}
	var members map[string]json.RawMessage
	if err := decodePDPDocument(trimmed, &members); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON", what)
	}
	return members, nil
}

// parsePDPInteger reads a JSON integer without fraction, exponent or sign. It gives false for any other text.
func parsePDPInteger(raw json.RawMessage) (int64, bool) {
	text := string(bytes.TrimSpace(raw))
	if text == "" || len(text) > 18 {
		return 0, false
	}
	for index := 0; index < len(text); index++ {
		if text[index] < '0' || text[index] > '9' {
			return 0, false
		}
	}
	value, err := strconv.ParseInt(text, 10, 64)
	return value, err == nil
}

// parsePDPAnswer reads the answer to one question. usage is true for a use question.
func parsePDPAnswer(data []byte, usage bool) (pdpAnswer, error) {
	members, err := decodePDPObject(data, "the answer")
	if err != nil {
		return pdpAnswer{}, err
	}
	var answer pdpAnswer
	switch string(bytes.TrimSpace(members["decision"])) {
	case "true":
		answer.Decision = true
	case "false":
	default:
		return pdpAnswer{}, errors.New("decision is not a JSON boolean")
	}
	answer.Source = pdpSourcePDP
	rawContext, hasContext := members["context"]
	if !hasContext {
		return finishPDPAnswer(answer, usage), nil
	}
	members, err = decodePDPObject(rawContext, "context")
	if err != nil {
		return pdpAnswer{}, err
	}
	reason := ""
	if rawReason, found := members["reason"]; found {
		if err := json.Unmarshal(rawReason, &reason); err != nil || bytes.TrimSpace(rawReason)[0] != '"' {
			return pdpAnswer{}, errors.New("context.reason is not a string")
		}
	}
	if !answer.Decision {
		answer.Reason = reason
		if reason == pdpReasonBudget {
			retry, ok := parsePDPInteger(members["retry_after"])
			if !ok || retry < 1 {
				return pdpAnswer{}, errors.New("context.retry_after is not a JSON integer of 1 or more")
			}
			answer.RetryAfter = retry
		}
		// ProcurAuth 7.6: the client ignores context.limits of a false answer, even a malformed one.
		return finishPDPAnswer(answer, usage), nil
	}
	if rawLimits, found := members["limits"]; found {
		values, err := decodePDPObject(rawLimits, "context.limits")
		if err != nil {
			return pdpAnswer{}, err
		}
		answer.Limits = make(map[string]int64, len(values))
		for name, raw := range values {
			value, ok := parsePDPInteger(raw)
			if !ok {
				return pdpAnswer{}, fmt.Errorf("context.limits.%s is not a JSON integer of 0 or more", name)
			}
			answer.Limits[name] = value
		}
	}
	if rawUsage, found := members["usage"]; found {
		values, err := decodePDPObject(rawUsage, "context.usage")
		if err != nil {
			return pdpAnswer{}, err
		}
		if rawUnits, found := values["units"]; found {
			units, ok := parsePDPInteger(rawUnits)
			if !ok || units > 1 {
				return pdpAnswer{}, errors.New("context.usage.units is not the integer 0 or 1")
			}
			answer.Units = units
			answer.HasUnits = true
		}
	}
	return finishPDPAnswer(answer, usage), nil
}

// finishPDPAnswer applies ProcurAuth 6.12: a true answer to a use question without usage.units is false.
func finishPDPAnswer(answer pdpAnswer, usage bool) pdpAnswer {
	if usage && answer.Decision && !answer.HasUnits {
		return pdpAnswer{Source: pdpSourcePDP}
	}
	return answer
}

// lookupLocked gives a kept answer that is not expired. When countdown is true, a kept budget_exceeded answer gives
// the whole seconds that remain, at least 1 and never more than its first retry_after. The caller holds c.mu.
func (c *pdpClient) lookupLocked(table map[string]pdpCacheEntry, key string, now time.Time, countdown bool) (pdpAnswer, bool) {
	entry, found := table[key]
	if !found || !now.Before(entry.expires) {
		return pdpAnswer{}, false
	}
	answer := entry.answer
	answer.Source = pdpSourceCache
	if answer.Limits != nil {
		limits := make(map[string]int64, len(answer.Limits))
		for name, value := range answer.Limits {
			limits[name] = value
		}
		answer.Limits = limits
	}
	if countdown && !answer.Decision && answer.Reason == pdpReasonBudget {
		remaining := int64((entry.expires.Sub(now) + time.Second - 1) / time.Second)
		if remaining < 1 {
			remaining = 1
		}
		if remaining > answer.RetryAfter {
			remaining = answer.RetryAfter
		}
		answer.RetryAfter = remaining
	}
	return answer, true
}

// storeUseLocked keeps an answer to a use question when the rules of ProcurAuth 10.1 allow it: a true answer with
// units 0 for cache_ttl, a budget_exceeded answer for the lower of retry_after and cache_ttl. It keeps nothing else.
func (c *pdpClient) storeUseLocked(key string, answer pdpAnswer, now time.Time) {
	switch {
	case answer.Failed:
		return
	case answer.Decision && answer.HasUnits && answer.Units == 0:
		c.storeLocked(c.useKept, key, answer, c.cacheTTL, now)
	case !answer.Decision && answer.Reason == pdpReasonBudget && answer.RetryAfter >= 1:
		ttl := c.cacheTTL
		if answer.RetryAfter <= int64(ttl/time.Second) {
			ttl = time.Duration(answer.RetryAfter) * time.Second
		}
		c.storeLocked(c.useKept, key, answer, ttl, now)
	}
}

// storeLocked keeps an answer for ttl. The two tables hold pdpCacheMaxEntries entries and pdpCacheMaxBytes bytes at
// most. When the tables are full, the expired entries go first. When they are still full, the client keeps no new
// answer (ProcurAuth 10.1b). The caller holds c.mu.
func (c *pdpClient) storeLocked(table map[string]pdpCacheEntry, key string, answer pdpAnswer, ttl time.Duration, now time.Time) {
	if ttl <= 0 {
		return
	}
	entry := pdpCacheEntry{answer: answer, expires: now.Add(ttl)}
	if _, found := table[key]; found {
		table[key] = entry
		return
	}
	size := len(key) + pdpCacheEntryOverhead
	if c.fullLocked(size) {
		c.purgeLocked(c.decideKept, now)
		c.purgeLocked(c.useKept, now)
		if c.fullLocked(size) {
			return
		}
	}
	table[key] = entry
	c.keptBytes += size
}

// fullLocked tells if an entry of the given size does not fit in the two tables.
func (c *pdpClient) fullLocked(size int) bool {
	return len(c.decideKept)+len(c.useKept) >= c.maxEntries || c.keptBytes+size > c.maxBytes
}

// purgeLocked deletes the expired entries of a table.
func (c *pdpClient) purgeLocked(table map[string]pdpCacheEntry, now time.Time) {
	for key, entry := range table {
		if !now.Before(entry.expires) {
			c.keptBytes -= len(key) + pdpCacheEntryOverhead
			delete(table, key)
		}
	}
}

// pdpLogToken gives a value for a log line: "-" when empty, the value when it is a short token, else a quoted text.
// A value of the PDP or of a header then cannot add a field or a line to the log.
func pdpLogToken(value string) string {
	if value == "" {
		return "-"
	}
	if len(value) <= pdpMaxLogTokenLength {
		safe := true
		for index := 0; index < len(value); index++ {
			c := value[index]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-._:", c) >= 0) {
				safe = false
				break
			}
		}
		if safe {
			return value
		}
	}
	return strconv.Quote(value)
}

// logDecision writes the access.decision line of one question. A cache hit writes it only when logCacheHits is true.
func (c *pdpClient) logDecision(requestID, subject string, question pdpQuestion, usage bool, answer pdpAnswer) {
	if answer.Source == pdpSourceCache && !c.logCacheHits {
		return
	}
	units, retry := "-", "-"
	if answer.HasUnits {
		units = strconv.FormatInt(answer.Units, 10)
	}
	if answer.RetryAfter > 0 {
		retry = strconv.FormatInt(answer.RetryAfter, 10)
	}
	log.Printf("access.decision request_id=%s subject=%q action=%s resource=%q match=%s usage=%t decision=%t units=%s retry_after=%s reason=%s source=%s failed=%t",
		pdpLogToken(requestID), subject, pdpLogToken(question.Action), loggedResourcePath(question.Resource, c.logCacheHits), question.match(), usage,
		answer.Decision, units, retry, pdpLogToken(answer.Reason), answer.Source, answer.Failed)
}

// loggedResourcePath gives the path of a resource for the log. A folder id and a subfolder id are parts of object keys.
// With server.log_mode = "anonymous" (detailed is false), the log shows "-" in place of these ids, as the request log
// keeps the object keys out. The service id and the bucket id stay.
func loggedResourcePath(resource accessResource, detailed bool) string {
	if detailed {
		return resource.path()
	}
	masked := accessResource{Type: resource.Type, ID: resource.ID, Ancestors: make([]accessNode, len(resource.Ancestors))}
	for index, node := range resource.Ancestors {
		if node.Type == accessTypeFolder || node.Type == accessTypeSubfolder {
			node.ID = "-"
		}
		masked.Ancestors[index] = node
	}
	if masked.Type == accessTypeFolder || masked.Type == accessTypeSubfolder {
		masked.ID = "-"
	}
	return masked.path()
}

// logFailure writes the line of a failed request. The line never holds the token or a header.
func (c *pdpClient) logFailure(requestID string, evaluations int, err error) {
	var status *pdpStatusError
	if errors.As(err, &status) && (status.status == http.StatusUnauthorized || status.status == http.StatusForbidden) {
		log.Printf("access.pdp_credential_refused request_id=%s status=%d", pdpLogToken(requestID), status.status)
		return
	}
	log.Printf("access.pdp_failed request_id=%s evaluations=%d error=%q", pdpLogToken(requestID), evaluations, err.Error())
}
