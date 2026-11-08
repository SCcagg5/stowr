package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fake PDP of the tests. It is an httptest server that serves the two paths of AuthZEN: /access/v1/evaluation and
// /access/v1/evaluations. Use it as follows.
//
//	fake, cfg := newFakePDP(t)
//	client := newPDPClient(cfg, false)
//	fake.SetAnswer(func(q fakeQuestion) map[string]any { return map[string]any{"decision": q.Action == "object.read"} })
//	... ask questions ...
//	fake.Questions()   // each question, one by one, also the items of a batch
//	fake.Requests()    // the number of HTTP requests that came with the good token
//	fake.Connections() // the number of TCP connections that the server accepted
//
// The fake PDP checks the bearer token and gives 401 for another token. Without an answer function, it answers
// {"decision": true} to each question. The failure injection (SetStatus, SetDelay, SetRaw, SetCloseConnection) applies
// to each request until Reset.

// fakePDPToken is the bearer token that the fake PDP accepts.
const fakePDPToken = "fake-pdp-token-0123456789"

// fakeQuestion is one question that the fake PDP received. A batch gives one record for each item.
type fakeQuestion struct {
	Subject      string
	Action       string
	ResourceType string
	ResourceID   string
	// Ancestors is the chain of resource.properties.ancestors, from the service.
	Ancestors []accessNode
	// Path has the form of accessResource.path(): "service:s/bucket:b/folder:f".
	Path string
	// Match is "node" or "subtree".
	Match string
	// Usage is true when the question has context.usage.
	Usage bool
	// Batch is true when the question came in an Access Evaluations request.
	Batch bool
	// RequestID is the header X-Request-ID of the request.
	RequestID string
}

// fakeRequest is one HTTP request that came with the good token.
type fakeRequest struct {
	Path   string
	Header http.Header
	Body   []byte
}

// fakePDP is the fake PDP. It is safe for concurrent use.
type fakePDP struct {
	Server *httptest.Server
	Token  string

	mu          sync.Mutex
	answer      func(fakeQuestion) map[string]any
	status      int
	delay       time.Duration
	raw         []byte
	hasRaw      bool
	closeConn   bool
	questions   []fakeQuestion
	requests    []fakeRequest
	connections int
}

// newFakePDP starts the fake PDP and gives it with an accessConfig for newPDPClient. The URL is plain HTTP. The
// timeouts are short: timeout 1s, usage timeout 2s. The cache lasts 30s. A request holds 64 items at most.
func newFakePDP(t testing.TB) (*fakePDP, accessConfig) {
	t.Helper()
	fake := &fakePDP{Token: fakePDPToken}
	server := httptest.NewUnstartedServer(http.HandlerFunc(fake.serve))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			fake.mu.Lock()
			fake.connections++
			fake.mu.Unlock()
		}
	}
	server.Start()
	t.Cleanup(server.Close)
	fake.Server = server
	return fake, accessConfig{
		Mode:              accessModeAuthZEN,
		PDPURL:            server.URL,
		PDPPlainHTTP:      true,
		PDPToken:          fakePDPToken,
		PDPTimeout:        time.Second,
		PDPUsageTimeout:   2 * time.Second,
		PDPCacheTTL:       30 * time.Second,
		PDPMaxEvaluations: 64,
	}
}

// SetAnswer sets the function that gives the JSON object of the decision of each question.
func (f *fakePDP) SetAnswer(answer func(fakeQuestion) map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answer = answer
}

// SetStatus makes each request give this HTTP status. A status of 300 to 399 adds a Location header. 0 or 200 stops it.
func (f *fakePDP) SetStatus(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = status
}

// SetDelay makes each request wait before it answers. The wait stops when the client leaves.
func (f *fakePDP) SetDelay(delay time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delay = delay
}

// SetRaw makes each request give this body, with the status of SetStatus (200 by default).
func (f *fakePDP) SetRaw(body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.raw, f.hasRaw = body, true
}

// SetCloseConnection makes each request close the TCP connection after it read the request, with no answer.
func (f *fakePDP) SetCloseConnection(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeConn = on
}

// Questions gives each question that the fake PDP received, in order of arrival.
func (f *fakePDP) Questions() []fakeQuestion {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeQuestion(nil), f.questions...)
}

// Requests gives the number of HTTP requests that came with the good token.
func (f *fakePDP) Requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// RawRequests gives each HTTP request that came with the good token: the path, the headers and the exact body.
func (f *fakePDP) RawRequests() []fakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeRequest(nil), f.requests...)
}

// Connections gives the number of TCP connections that the server accepted since the last Reset.
func (f *fakePDP) Connections() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connections
}

// Reset clears the records and the failure injection. It keeps the token and the answer function. The client can still
// hold an open connection: Connections then counts only the new ones.
func (f *fakePDP) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.delay, f.raw, f.hasRaw, f.closeConn = 0, 0, nil, false, false
	f.questions, f.requests, f.connections = nil, nil, 0
}

// fakeEncodeID is the percent-encoding of ProcurAuth 5.2, written again here: a fault in the encoder of the server
// cannot hide itself.
func fakeEncodeID(id string) string {
	var b strings.Builder
	for _, c := range []byte(id) {
		if strings.IndexByte("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// fakeParseQuestion reads the action, the resource and the context of one evaluation. subject is the subject of the
// request, and an item can replace it.
func fakeParseQuestion(subject map[string]any, item map[string]any, batch bool, requestID string) fakeQuestion {
	if own, ok := item["subject"].(map[string]any); ok {
		subject = own
	}
	question := fakeQuestion{Match: "node", Batch: batch, RequestID: requestID}
	question.Subject, _ = subject["id"].(string)
	action, _ := item["action"].(map[string]any)
	question.Action, _ = action["name"].(string)
	resource, _ := item["resource"].(map[string]any)
	question.ResourceType, _ = resource["type"].(string)
	question.ResourceID, _ = resource["id"].(string)
	if properties, ok := resource["properties"].(map[string]any); ok {
		ancestors, _ := properties["ancestors"].([]any)
		for _, raw := range ancestors {
			node, _ := raw.(map[string]any)
			kind, _ := node["type"].(string)
			id, _ := node["id"].(string)
			question.Ancestors = append(question.Ancestors, accessNode{Type: kind, ID: id})
		}
	}
	var parts []string
	for _, node := range question.Ancestors {
		parts = append(parts, node.Type+":"+fakeEncodeID(node.ID))
	}
	parts = append(parts, question.ResourceType+":"+fakeEncodeID(question.ResourceID))
	question.Path = strings.Join(parts, "/")
	if values, ok := item["context"].(map[string]any); ok {
		if match, ok := values["match"].(string); ok {
			question.Match = match
		}
		_, question.Usage = values["usage"]
	}
	return question
}

func (f *fakePDP) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if r.Header.Get("Authorization") != "Bearer "+f.Token {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var document map[string]any
	_ = json.Unmarshal(body, &document)
	requestID := r.Header.Get("X-Request-ID")
	subject, _ := document["subject"].(map[string]any)
	var questions []fakeQuestion
	switch r.URL.Path {
	case pdpEvaluationPath:
		questions = append(questions, fakeParseQuestion(subject, document, false, requestID))
	case pdpEvaluationsPath:
		items, _ := document["evaluations"].([]any)
		for _, raw := range items {
			item, _ := raw.(map[string]any)
			questions = append(questions, fakeParseQuestion(subject, item, true, requestID))
		}
	default:
		w.WriteHeader(http.StatusNotFound)
		return
	}

	f.mu.Lock()
	f.requests = append(f.requests, fakeRequest{Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
	f.questions = append(f.questions, questions...)
	answer, status, delay, raw, hasRaw, closeConn := f.answer, f.status, f.delay, f.raw, f.hasRaw, f.closeConn
	f.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	if closeConn {
		if hijacker, ok := w.(http.Hijacker); ok {
			if conn, _, err := hijacker.Hijack(); err == nil {
				_ = conn.Close()
			}
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if status >= 300 && status < 400 {
		w.Header().Set("Location", "/redirected")
	}
	if status == 0 {
		status = http.StatusOK
	}
	if hasRaw {
		w.WriteHeader(status)
		_, _ = w.Write(raw)
		return
	}
	if status != http.StatusOK {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"injected"}`))
		return
	}
	decide := func(q fakeQuestion) map[string]any {
		if answer == nil {
			return map[string]any{"decision": true}
		}
		return answer(q)
	}
	if r.URL.Path == pdpEvaluationPath {
		_ = json.NewEncoder(w).Encode(decide(questions[0]))
		return
	}
	items := make([]any, 0, len(questions))
	for _, question := range questions {
		items = append(items, decide(question))
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"evaluations": items})
}
