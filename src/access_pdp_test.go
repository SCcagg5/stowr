package main

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The names of the actions. The tests do not depend on the action document.
const (
	pdpTestRead  = "object.read"
	pdpTestWrite = "object.write"
	pdpTestAdmin = "bucket.admin"
)

// pdpTestClock is a clock that a test moves by hand.
type pdpTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *pdpTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *pdpTestClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// newPDPTestClient makes a client for a new fake PDP. mutate can change the configuration.
func newPDPTestClient(t *testing.T, mutate func(*accessConfig)) (*pdpClient, *fakePDP, *pdpTestClock) {
	t.Helper()
	fake, cfg := newFakePDP(t)
	if mutate != nil {
		mutate(&cfg)
	}
	client := newPDPClient(cfg, false)
	t.Cleanup(client.close)
	clock := &pdpTestClock{now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	client.now = clock.Now
	return client, fake, clock
}

// pdpFolderQuestion asks an action on the folder of the key "<name>/x" in the bucket "logs" of the service "svc".
func pdpFolderQuestion(action, name string, subtree bool) pdpQuestion {
	return pdpQuestion{Action: action, Resource: keyResource("svc", "logs", name+"/x"), Subtree: subtree}
}

func pdpReadQuestion(name string) pdpQuestion {
	return pdpFolderQuestion(pdpTestRead, name, false)
}

func pdpWriteQuestion(name string) pdpQuestion {
	return pdpFolderQuestion(pdpTestWrite, name, false)
}

func pdpDecideOne(client *pdpClient, subject string, question pdpQuestion) pdpAnswer {
	return client.decide(context.Background(), "req-1", subject, []pdpQuestion{question})[0]
}

func pdpUseOne(client *pdpClient, subject string, question pdpQuestion) pdpAnswer {
	return client.use(context.Background(), "req-1", subject, question)
}

const (
	pdpJSONSubject = `{"type":"user","id":"alice"}`
	pdpJSONFolder  = `{"type":"folder","id":"2026","properties":{"ancestors":[{"type":"service","id":"svc"},{"type":"bucket","id":"logs"}]}}`
	pdpJSONBucket  = `{"type":"bucket","id":"logs","properties":{"ancestors":[{"type":"service","id":"svc"}]}}`
)

func TestPDPRequestJSON(t *testing.T) {
	t.Parallel()
	folder := pdpFolderQuestion(pdpTestRead, "2026", false)
	subtree := pdpQuestion{Action: pdpTestRead, Resource: folder.Resource, Subtree: true}
	bucket := pdpQuestion{Action: pdpTestAdmin, Resource: bucketResource("svc", "logs")}
	cases := []struct {
		name      string
		ask       func(*pdpClient)
		path      string
		body      string
		questions int
	}{
		{
			name: "one node question",
			ask:  func(c *pdpClient) { pdpDecideOne(c, "alice", folder) },
			path: "/access/v1/evaluation",
			body: `{"subject":` + pdpJSONSubject + `,"action":{"name":"object.read"},"resource":` + pdpJSONFolder + `}`,
		},
		{
			name: "one subtree question",
			ask:  func(c *pdpClient) { pdpDecideOne(c, "alice", subtree) },
			path: "/access/v1/evaluation",
			body: `{"subject":` + pdpJSONSubject + `,"action":{"name":"object.read"},"resource":` + pdpJSONFolder + `,"context":{"match":"subtree"}}`,
		},
		{
			name: "a batch with a subtree item and a node item",
			ask: func(c *pdpClient) {
				c.decide(context.Background(), "req-1", "alice", []pdpQuestion{subtree, bucket})
			},
			path: "/access/v1/evaluations",
			body: `{"subject":` + pdpJSONSubject + `,"evaluations":[` +
				`{"action":{"name":"object.read"},"resource":` + pdpJSONFolder + `,"context":{"match":"subtree"}},` +
				`{"action":{"name":"bucket.admin"},"resource":` + pdpJSONBucket + `}]}`,
		},
		{
			name: "a use question",
			ask:  func(c *pdpClient) { pdpUseOne(c, "alice", bucket) },
			path: "/access/v1/evaluation",
			body: `{"subject":` + pdpJSONSubject + `,"action":{"name":"bucket.admin"},"resource":` + pdpJSONBucket + `,"context":{"usage":{"units":1}}}`,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client, fake, _ := newPDPTestClient(t, nil)
			tc.ask(client)
			requests := fake.RawRequests()
			if len(requests) != 1 {
				t.Fatalf("requests = %d, want 1", len(requests))
			}
			if requests[0].Path != tc.path {
				t.Fatalf("path = %q, want %q", requests[0].Path, tc.path)
			}
			if string(requests[0].Body) != tc.body {
				t.Fatalf("body =\n%s\nwant\n%s", requests[0].Body, tc.body)
			}
		})
	}
}

func TestPDPRequestHeadersAndTransport(t *testing.T) {
	t.Parallel()
	client, fake, _ := newPDPTestClient(t, nil)
	client.decide(context.Background(), "req-77", "alice", []pdpQuestion{pdpReadQuestion("a")})
	requests := fake.RawRequests()
	if len(requests) != 1 {
		t.Fatalf("requests = %d", len(requests))
	}
	want := map[string]string{
		"Authorization": "Bearer " + fakePDPToken,
		"Content-Type":  "application/json",
		"Accept":        "application/json",
		"X-Request-Id":  "req-77",
	}
	for name, value := range want {
		if got := requests[0].Header.Get(name); got != value {
			t.Errorf("header %s = %q, want %q", name, got, value)
		}
	}
	transport, ok := client.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type %T", client.client.Transport)
	}
	if transport.Proxy != nil || transport.MaxIdleConns != 4 || transport.MaxIdleConnsPerHost != 4 || transport.IdleConnTimeout != 5*time.Second || transport.DialContext == nil {
		t.Fatalf("transport settings are wrong: %+v", transport)
	}
}

func TestPDPWrongTokenIsFailed(t *testing.T) {
	t.Parallel()
	client, fake, _ := newPDPTestClient(t, nil)
	client.token = "another-token"
	answer := pdpDecideOne(client, "alice", pdpReadQuestion("a"))
	if !answer.Failed || answer.Source != "error" || answer.Decision {
		t.Fatalf("answer = %+v", answer)
	}
	if fake.Requests() != 0 || len(fake.Questions()) != 0 {
		t.Fatalf("the fake PDP must not count a request with a wrong token")
	}
}

func pdpBigBody() string {
	return `{"decision":true,"context":{"pad":"` + strings.Repeat("x", 300<<10) + `"}}`
}

func TestPDPAnswerRules(t *testing.T) {
	t.Parallel()
	failed := failedAnswer()
	pdp := func(a pdpAnswer) pdpAnswer { a.Source = "pdp"; return a }
	cases := []struct {
		name   string
		status int
		body   string
		usage  bool
		want   pdpAnswer
	}{
		{name: "true", body: `{"decision":true}`, want: pdp(pdpAnswer{Decision: true})},
		{name: "false", body: `{"decision":false}`, want: pdp(pdpAnswer{})},
		{name: "unknown members are ignored", body: `{"decision":true,"extra":1,"context":{"foo":[1],"limits":{}}}`, want: pdp(pdpAnswer{Decision: true, Limits: map[string]int64{}})},
		{name: "false keeps the reason", body: `{"decision":false,"context":{"reason":"unknown_subject"}}`, want: pdp(pdpAnswer{Reason: "unknown_subject"})},
		{name: "true drops the reason", body: `{"decision":true,"context":{"reason":"x"}}`, want: pdp(pdpAnswer{Decision: true})},
		{name: "budget_exceeded with retry_after 12", body: `{"decision":false,"context":{"reason":"budget_exceeded","retry_after":12}}`, want: pdp(pdpAnswer{Reason: "budget_exceeded", RetryAfter: 12})},
		{name: "budget_exceeded with retry_after 1", body: `{"decision":false,"context":{"reason":"budget_exceeded","retry_after":1}}`, want: pdp(pdpAnswer{Reason: "budget_exceeded", RetryAfter: 1})},
		{name: "limits of a false answer are ignored", body: `{"decision":false,"context":{"limits":{"max_object_size":-5}}}`, want: pdp(pdpAnswer{})},
		{name: "malformed limits of a false answer are ignored", body: `{"decision":false,"context":{"limits":"zzz"}}`, want: pdp(pdpAnswer{})},
		{name: "limits of a true answer", body: `{"decision":true,"context":{"limits":{"max_object_size":100,"other":0}}}`, want: pdp(pdpAnswer{Decision: true, Limits: map[string]int64{"max_object_size": 100, "other": 0}})},
		{name: "usage units 1 of a decide answer", body: `{"decision":true,"context":{"usage":{"units":1}}}`, want: pdp(pdpAnswer{Decision: true, Units: 1, HasUnits: true})},

		{name: "status 500", status: 500, want: failed},
		{name: "status 401", status: 401, want: failed},
		{name: "status 403", status: 403, want: failed},
		{name: "status 404", status: 404, want: failed},
		{name: "redirect 302", status: 302, want: failed},
		{name: "body of 300 KiB", body: pdpBigBody(), want: failed},
		{name: "empty body", body: ``, want: failed},
		{name: "bad JSON", body: `{"decision":`, want: failed},
		{name: "data after the document", body: `{"decision":true} {}`, want: failed},
		{name: "answer is an array", body: `[{"decision":true}]`, want: failed},
		{name: "answer is null", body: `null`, want: failed},
		{name: "decision is missing", body: `{"context":{}}`, want: failed},
		{name: "decision is a string", body: `{"decision":"true"}`, want: failed},
		{name: "decision is a number", body: `{"decision":1}`, want: failed},
		{name: "context is an array", body: `{"decision":true,"context":[]}`, want: failed},
		{name: "context is null", body: `{"decision":true,"context":null}`, want: failed},
		{name: "reason is a number", body: `{"decision":false,"context":{"reason":5}}`, want: failed},
		{name: "reason is null", body: `{"decision":false,"context":{"reason":null}}`, want: failed},
		{name: "reason of a true answer is a number", body: `{"decision":true,"context":{"reason":5}}`, want: failed},
		{name: "retry_after 0", body: `{"decision":false,"context":{"reason":"budget_exceeded","retry_after":0}}`, want: failed},
		{name: "retry_after 1.5", body: `{"decision":false,"context":{"reason":"budget_exceeded","retry_after":1.5}}`, want: failed},
		{name: "retry_after 12.0", body: `{"decision":false,"context":{"reason":"budget_exceeded","retry_after":12.0}}`, want: failed},
		{name: "retry_after 1e1", body: `{"decision":false,"context":{"reason":"budget_exceeded","retry_after":1e1}}`, want: failed},
		{name: "retry_after is a string", body: `{"decision":false,"context":{"reason":"budget_exceeded","retry_after":"12"}}`, want: failed},
		{name: "retry_after is negative", body: `{"decision":false,"context":{"reason":"budget_exceeded","retry_after":-3}}`, want: failed},
		{name: "retry_after is absent", body: `{"decision":false,"context":{"reason":"budget_exceeded"}}`, want: failed},
		{name: "limits with a negative value", body: `{"decision":true,"context":{"limits":{"max_object_size":-1}}}`, want: failed},
		{name: "limits with a fraction", body: `{"decision":true,"context":{"limits":{"max_object_size":1.5}}}`, want: failed},
		{name: "limits with a string", body: `{"decision":true,"context":{"limits":{"max_object_size":"7"}}}`, want: failed},
		{name: "limits is an array", body: `{"decision":true,"context":{"limits":[1]}}`, want: failed},
		{name: "limits is null", body: `{"decision":true,"context":{"limits":null}}`, want: failed},
		{name: "usage units 2", body: `{"decision":true,"context":{"usage":{"units":2}}}`, want: failed},
		{name: "usage units -1", body: `{"decision":true,"context":{"usage":{"units":-1}}}`, want: failed},
		{name: "usage units is a fraction", body: `{"decision":true,"context":{"usage":{"units":1.0}}}`, want: failed},
		{name: "usage units is a string", body: `{"decision":true,"context":{"usage":{"units":"1"}}}`, want: failed},
		{name: "usage is an array", body: `{"decision":true,"context":{"usage":[]}}`, want: failed},

		{name: "use: true with units 1", usage: true, body: `{"decision":true,"context":{"usage":{"units":1}}}`, want: pdp(pdpAnswer{Decision: true, Units: 1, HasUnits: true})},
		{name: "use: true with units 0", usage: true, body: `{"decision":true,"context":{"usage":{"units":0}}}`, want: pdp(pdpAnswer{Decision: true, Units: 0, HasUnits: true})},
		{name: "use: true with units and limits", usage: true, body: `{"decision":true,"context":{"usage":{"units":1},"limits":{"max_object_size":9}}}`, want: pdp(pdpAnswer{Decision: true, Units: 1, HasUnits: true, Limits: map[string]int64{"max_object_size": 9}})},
		{name: "use: true without units is false", usage: true, body: `{"decision":true}`, want: pdp(pdpAnswer{})},
		{name: "use: true with context and no units is false", usage: true, body: `{"decision":true,"context":{"limits":{"max_object_size":9}}}`, want: pdp(pdpAnswer{})},
		{name: "use: false", usage: true, body: `{"decision":false}`, want: pdp(pdpAnswer{})},
		{name: "use: budget_exceeded", usage: true, body: `{"decision":false,"context":{"reason":"budget_exceeded","retry_after":12}}`, want: pdp(pdpAnswer{Reason: "budget_exceeded", RetryAfter: 12})},
		{name: "use: budget_exceeded with retry_after 0", usage: true, body: `{"decision":false,"context":{"reason":"budget_exceeded","retry_after":0}}`, want: failed},
		{name: "use: units 2", usage: true, body: `{"decision":true,"context":{"usage":{"units":2}}}`, want: failed},
		{name: "use: status 503", usage: true, status: 503, want: failed},
		{name: "use: bad JSON", usage: true, body: `nope`, want: failed},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client, fake, _ := newPDPTestClient(t, nil)
			if tc.status != 0 {
				fake.SetStatus(tc.status)
			}
			if tc.status == 0 || tc.body != "" {
				fake.SetRaw([]byte(tc.body))
			}
			var got pdpAnswer
			if tc.usage {
				got = pdpUseOne(client, "alice", pdpWriteQuestion("a"))
			} else {
				got = pdpDecideOne(client, "alice", pdpReadQuestion("a"))
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("answer = %+v, want %+v", got, tc.want)
			}
			if fake.Requests() != 1 {
				t.Fatalf("requests = %d, want 1 (no retry, no redirect)", fake.Requests())
			}
			again := pdpDecideOne(client, "alice", pdpReadQuestion("zz"))
			_ = again
		})
	}
}

func TestPDPBatchAnswerRules(t *testing.T) {
	t.Parallel()
	questions := []pdpQuestion{pdpReadQuestion("a"), pdpReadQuestion("b"), pdpReadQuestion("c")}
	t.Run("a bad item fails only this item", func(t *testing.T) {
		t.Parallel()
		client, fake, _ := newPDPTestClient(t, nil)
		fake.SetRaw([]byte(`{"evaluations":[{"decision":true},{"decision":"true"},{"decision":false,"context":{"reason":"budget_exceeded","retry_after":3}}]}`))
		got := client.decide(context.Background(), "req-1", "alice", questions)
		want := []pdpAnswer{
			{Decision: true, Source: "pdp"},
			failedAnswer(),
			{Reason: "budget_exceeded", RetryAfter: 3, Source: "pdp"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("answers = %+v, want %+v", got, want)
		}
	})
	counts := map[string]string{
		"too few items":  `{"evaluations":[{"decision":true},{"decision":true}]}`,
		"too many items": `{"evaluations":[{"decision":true},{"decision":true},{"decision":true},{"decision":true}]}`,
		"no items":       `{"evaluations":[]}`,
		"no member":      `{"decision":true}`,
		"null member":    `{"evaluations":null}`,
	}
	for name, body := range counts {
		name, body := name, body
		t.Run("count mismatch: "+name, func(t *testing.T) {
			t.Parallel()
			client, fake, _ := newPDPTestClient(t, func(cfg *accessConfig) { cfg.PDPMaxEvaluations = 3 })
			fake.SetRaw([]byte(body))
			many := append(append([]pdpQuestion(nil), questions...), pdpReadQuestion("d"))
			got := client.decide(context.Background(), "req-1", "alice", many)
			for index, answer := range got {
				if !answer.Failed || answer.Decision || answer.Source != "error" {
					t.Fatalf("answer %d = %+v, want failed", index, answer)
				}
			}
			if fake.Requests() != 1 {
				t.Fatalf("requests = %d, want 1: a failed request stops the call", fake.Requests())
			}
		})
	}
}

func TestPDPSplitsAtMaxEvaluations(t *testing.T) {
	t.Parallel()
	client, fake, _ := newPDPTestClient(t, func(cfg *accessConfig) { cfg.PDPMaxEvaluations = 2 })
	fake.SetAnswer(func(q fakeQuestion) map[string]any { return map[string]any{"decision": q.ResourceID == "b"} })
	got := client.decide(context.Background(), "req-1", "alice", []pdpQuestion{pdpReadQuestion("a"), pdpReadQuestion("b"), pdpReadQuestion("c")})
	if decisions := []bool{got[0].Decision, got[1].Decision, got[2].Decision}; !reflect.DeepEqual(decisions, []bool{false, true, false}) {
		t.Fatalf("decisions = %v", decisions)
	}
	requests := fake.RawRequests()
	if len(requests) != 2 || requests[0].Path != pdpEvaluationsPath || requests[1].Path != pdpEvaluationPath {
		t.Fatalf("requests = %+v", requests)
	}
	questions := fake.Questions()
	if len(questions) != 3 || !questions[0].Batch || !questions[1].Batch || questions[2].Batch {
		t.Fatalf("questions = %+v", questions)
	}
}

func TestPDPRemovesDuplicateQuestions(t *testing.T) {
	t.Parallel()
	client, fake, _ := newPDPTestClient(t, nil)
	fake.SetAnswer(func(q fakeQuestion) map[string]any { return map[string]any{"decision": q.ResourceID == "a"} })
	got := client.decide(context.Background(), "req-1", "alice", []pdpQuestion{pdpReadQuestion("a"), pdpReadQuestion("b"), pdpReadQuestion("a"), pdpFolderQuestion(pdpTestRead, "a", true)})
	if decisions := []bool{got[0].Decision, got[1].Decision, got[2].Decision, got[3].Decision}; !reflect.DeepEqual(decisions, []bool{true, false, true, true}) {
		t.Fatalf("decisions = %v", decisions)
	}
	if got[2].Source != "pdp" {
		t.Fatalf("the copy has source %q", got[2].Source)
	}
	// The subtree question has another cache key: three questions go to the PDP, in one request.
	if questions := fake.Questions(); len(questions) != 3 || fake.Requests() != 1 {
		t.Fatalf("questions = %d, requests = %d", len(questions), fake.Requests())
	}
}

func TestPDPNoQuestionsNoRequest(t *testing.T) {
	t.Parallel()
	client, fake, _ := newPDPTestClient(t, nil)
	if got := client.decide(context.Background(), "req-1", "alice", nil); len(got) != 0 {
		t.Fatalf("answers = %v", got)
	}
	if fake.Requests() != 0 {
		t.Fatalf("requests = %d", fake.Requests())
	}
}

func TestPDPOneDeadlineForTheCall(t *testing.T) {
	t.Parallel()
	four := []pdpQuestion{pdpReadQuestion("a"), pdpReadQuestion("b"), pdpReadQuestion("c"), pdpReadQuestion("d")}
	t.Run("a slow PDP fails the call and the second chunk is never sent", func(t *testing.T) {
		t.Parallel()
		client, fake, _ := newPDPTestClient(t, func(cfg *accessConfig) {
			cfg.PDPMaxEvaluations = 2
			cfg.PDPTimeout = 200 * time.Millisecond
		})
		fake.SetDelay(5 * time.Second)
		started := time.Now()
		got := client.decide(context.Background(), "req-1", "alice", four)
		if time.Since(started) > 2*time.Second {
			t.Fatalf("the call took %s", time.Since(started))
		}
		for index, answer := range got {
			if !answer.Failed {
				t.Fatalf("answer %d = %+v, want failed", index, answer)
			}
		}
		if fake.Requests() != 1 {
			t.Fatalf("requests = %d, want 1", fake.Requests())
		}
	})
	t.Run("the deadline is for the whole call", func(t *testing.T) {
		t.Parallel()
		client, fake, _ := newPDPTestClient(t, func(cfg *accessConfig) {
			cfg.PDPMaxEvaluations = 2
			cfg.PDPTimeout = 700 * time.Millisecond
		})
		fake.SetDelay(400 * time.Millisecond)
		got := client.decide(context.Background(), "req-1", "alice", four)
		if got[0].Failed || got[1].Failed {
			t.Fatalf("the first chunk must have an answer: %+v", got[:2])
		}
		if !got[2].Failed || !got[3].Failed {
			t.Fatalf("the second chunk must fail at the deadline of the call: %+v", got[2:])
		}
		if fake.Requests() != 2 {
			t.Fatalf("requests = %d, want 2", fake.Requests())
		}
	})
	t.Run("a canceled context fails the call", func(t *testing.T) {
		t.Parallel()
		client, fake, _ := newPDPTestClient(t, nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		got := client.decide(ctx, "req-1", "alice", four[:1])
		if !got[0].Failed || fake.Requests() != 0 {
			t.Fatalf("answer = %+v, requests = %d", got[0], fake.Requests())
		}
	})
}

func TestPDPUseIsNeverSentAgain(t *testing.T) {
	t.Parallel()
	question := pdpWriteQuestion("a")
	t.Run("the PDP closes the connection after the request", func(t *testing.T) {
		t.Parallel()
		client, fake, _ := newPDPTestClient(t, nil)
		// A first question opens a connection that the client can use again.
		pdpDecideOne(client, "alice", pdpReadQuestion("a"))
		fake.Reset()
		fake.SetCloseConnection(true)
		got := pdpUseOne(client, "alice", question)
		if !got.Failed {
			t.Fatalf("answer = %+v, want failed", got)
		}
		if fake.Requests() != 1 {
			t.Fatalf("requests = %d, want 1: no second request after a lost answer", fake.Requests())
		}
	})
	t.Run("the PDP is slower than usage_timeout", func(t *testing.T) {
		t.Parallel()
		client, fake, _ := newPDPTestClient(t, func(cfg *accessConfig) { cfg.PDPUsageTimeout = 200 * time.Millisecond })
		fake.SetDelay(5 * time.Second)
		got := pdpUseOne(client, "alice", question)
		if !got.Failed {
			t.Fatalf("answer = %+v, want failed", got)
		}
		time.Sleep(300 * time.Millisecond)
		if fake.Requests() != 1 {
			t.Fatalf("requests = %d, want 1", fake.Requests())
		}
	})
	t.Run("the usage deadline is usage_timeout, not timeout", func(t *testing.T) {
		t.Parallel()
		client, fake, _ := newPDPTestClient(t, func(cfg *accessConfig) {
			cfg.PDPTimeout = 100 * time.Millisecond
			cfg.PDPUsageTimeout = 2 * time.Second
		})
		fake.SetDelay(300 * time.Millisecond)
		fake.SetAnswer(func(fakeQuestion) map[string]any {
			return map[string]any{"decision": true, "context": map[string]any{"usage": map[string]any{"units": 1}}}
		})
		if got := pdpUseOne(client, "alice", question); got.Failed || !got.Decision {
			t.Fatalf("answer = %+v", got)
		}
	})
	t.Run("a subtree question is a bug of the caller", func(t *testing.T) {
		t.Parallel()
		client, fake, _ := newPDPTestClient(t, nil)
		got := pdpUseOne(client, "alice", pdpFolderQuestion(pdpTestWrite, "a", true))
		if !got.Failed || fake.Requests() != 0 {
			t.Fatalf("answer = %+v, requests = %d", got, fake.Requests())
		}
	})
}

func TestPDPReusesTheConnection(t *testing.T) {
	t.Parallel()
	client, fake, _ := newPDPTestClient(t, func(cfg *accessConfig) { cfg.PDPCacheTTL = 0 })
	for index := 0; index < 5; index++ {
		pdpDecideOne(client, "alice", pdpReadQuestion("n"+strconv.Itoa(index)))
	}
	pdpUseOne(client, "alice", pdpWriteQuestion("u"))
	// A refused status and a body of 100 KiB must not break the connection either.
	fake.SetStatus(500)
	pdpDecideOne(client, "alice", pdpReadQuestion("x"))
	fake.SetStatus(0)
	fake.SetRaw([]byte(`{"decision":true,"context":{"pad":"` + strings.Repeat("y", 100<<10) + `"}}`))
	pdpDecideOne(client, "alice", pdpReadQuestion("y"))
	if fake.Requests() != 8 || fake.Connections() != 1 {
		t.Fatalf("requests = %d, connections = %d, want 8 and 1", fake.Requests(), fake.Connections())
	}
}

func TestPDPCacheHit(t *testing.T) {
	t.Parallel()
	client, fake, _ := newPDPTestClient(t, nil)
	fake.SetAnswer(func(fakeQuestion) map[string]any {
		return map[string]any{"decision": true, "context": map[string]any{"limits": map[string]any{"max_object_size": 100}}}
	})
	question := pdpReadQuestion("a")
	first := pdpDecideOne(client, "alice", question)
	second := pdpDecideOne(client, "alice", question)
	if first.Source != "pdp" || second.Source != "cache" {
		t.Fatalf("sources = %q, %q", first.Source, second.Source)
	}
	if !second.Decision || second.Limits["max_object_size"] != 100 {
		t.Fatalf("answer = %+v", second)
	}
	// A caller that changes the limits of an answer must not change the kept answer.
	second.Limits["max_object_size"] = 1
	if third := pdpDecideOne(client, "alice", question); third.Limits["max_object_size"] != 100 {
		t.Fatalf("the kept answer changed: %+v", third)
	}
	if fake.Requests() != 1 {
		t.Fatalf("requests = %d, want 1", fake.Requests())
	}
}

func TestPDPCacheKeepsFalseAnswerWithReason(t *testing.T) {
	t.Parallel()
	client, fake, _ := newPDPTestClient(t, nil)
	fake.SetAnswer(func(fakeQuestion) map[string]any {
		return map[string]any{"decision": false, "context": map[string]any{"reason": "budget_exceeded", "retry_after": 12}}
	})
	question := pdpReadQuestion("a")
	pdpDecideOne(client, "alice", question)
	hit := pdpDecideOne(client, "alice", question)
	want := pdpAnswer{Reason: "budget_exceeded", RetryAfter: 12, Source: "cache"}
	if !reflect.DeepEqual(hit, want) || fake.Requests() != 1 {
		t.Fatalf("hit = %+v, requests = %d", hit, fake.Requests())
	}
}

func TestPDPCacheKeyParts(t *testing.T) {
	t.Parallel()
	client, fake, _ := newPDPTestClient(t, nil)
	pdpDecideOne(client, "alice", pdpReadQuestion("a"))
	steps := []struct {
		name     string
		subject  string
		question pdpQuestion
	}{
		{"another subject", "bob", pdpReadQuestion("a")},
		{"another action", "alice", pdpWriteQuestion("a")},
		{"another resource", "alice", pdpReadQuestion("b")},
		{"another chain", "alice", pdpQuestion{Action: pdpTestRead, Resource: accessResource{Type: accessTypeFolder, ID: "a", Ancestors: []accessNode{{Type: accessTypeService, ID: "other"}, {Type: accessTypeBucket, ID: "logs"}}}}},
		{"another match", "alice", pdpFolderQuestion(pdpTestRead, "a", true)},
		{"a subject that looks like a key part", "alice\x1fobject.read", pdpReadQuestion("a")},
	}
	for index, step := range steps {
		answer := pdpDecideOne(client, step.subject, step.question)
		if answer.Source != "pdp" {
			t.Errorf("%s: source = %q, want pdp", step.name, answer.Source)
		}
		if fake.Requests() != index+2 {
			t.Errorf("%s: requests = %d, want %d", step.name, fake.Requests(), index+2)
		}
	}
}

func TestPDPCacheTTLZeroKeepsNothing(t *testing.T) {
	t.Parallel()
	client, fake, _ := newPDPTestClient(t, func(cfg *accessConfig) { cfg.PDPCacheTTL = 0 })
	question := pdpReadQuestion("a")
	pdpDecideOne(client, "alice", question)
	pdpDecideOne(client, "alice", question)
	pdpUseOne(client, "alice", pdpWriteQuestion("a"))
	fake.SetAnswer(func(fakeQuestion) map[string]any {
		return map[string]any{"decision": true, "context": map[string]any{"usage": map[string]any{"units": 0}}}
	})
	pdpUseOne(client, "alice", pdpWriteQuestion("b"))
	pdpUseOne(client, "alice", pdpWriteQuestion("b"))
	if fake.Requests() != 5 {
		t.Fatalf("requests = %d, want 5", fake.Requests())
	}
}

func TestPDPCacheExpiry(t *testing.T) {
	t.Parallel()
	client, fake, clock := newPDPTestClient(t, nil)
	question := pdpReadQuestion("a")
	pdpDecideOne(client, "alice", question)
	clock.Advance(29 * time.Second)
	if got := pdpDecideOne(client, "alice", question); got.Source != "cache" {
		t.Fatalf("after 29s: source = %q", got.Source)
	}
	clock.Advance(time.Second)
	if got := pdpDecideOne(client, "alice", question); got.Source != "pdp" {
		t.Fatalf("after 30s: source = %q", got.Source)
	}
	if fake.Requests() != 2 {
		t.Fatalf("requests = %d, want 2", fake.Requests())
	}
}

func TestPDPFailureIsNeverKept(t *testing.T) {
	t.Parallel()
	client, fake, _ := newPDPTestClient(t, nil)
	question := pdpReadQuestion("a")
	fake.SetStatus(500)
	if got := pdpDecideOne(client, "alice", question); !got.Failed {
		t.Fatalf("answer = %+v", got)
	}
	fake.Reset()
	if got := pdpDecideOne(client, "alice", question); got.Failed || got.Source != "pdp" || !got.Decision {
		t.Fatalf("the failure was kept: %+v", got)
	}
	// A failed item of a batch is not kept either.
	fake.SetRaw([]byte(`{"evaluations":[{"decision":true},{"decision":"x"}]}`))
	got := client.decide(context.Background(), "req-1", "alice", []pdpQuestion{pdpReadQuestion("b"), pdpReadQuestion("c")})
	if got[0].Failed || !got[1].Failed {
		t.Fatalf("answers = %+v", got)
	}
	fake.Reset()
	again := client.decide(context.Background(), "req-1", "alice", []pdpQuestion{pdpReadQuestion("b"), pdpReadQuestion("c")})
	if again[0].Source != "cache" || again[1].Source != "pdp" || again[1].Failed {
		t.Fatalf("answers = %+v", again)
	}
	// A failed use is not kept.
	fake.SetStatus(503)
	if got := pdpUseOne(client, "alice", pdpWriteQuestion("a")); !got.Failed {
		t.Fatalf("answer = %+v", got)
	}
	fake.Reset()
	fake.SetAnswer(func(fakeQuestion) map[string]any {
		return map[string]any{"decision": true, "context": map[string]any{"usage": map[string]any{"units": 0}}}
	})
	if got := pdpUseOne(client, "alice", pdpWriteQuestion("a")); got.Failed || got.Source != "pdp" {
		t.Fatalf("answer = %+v", got)
	}
}

func TestPDPUseCache(t *testing.T) {
	t.Parallel()
	unitsAnswer := func(units int) func(fakeQuestion) map[string]any {
		return func(fakeQuestion) map[string]any {
			return map[string]any{"decision": true, "context": map[string]any{"usage": map[string]any{"units": units}}}
		}
	}
	question := pdpWriteQuestion("a")

	t.Run("a true answer with units 1 is never kept", func(t *testing.T) {
		t.Parallel()
		client, fake, _ := newPDPTestClient(t, nil)
		fake.SetAnswer(unitsAnswer(1))
		for index := 0; index < 3; index++ {
			if got := pdpUseOne(client, "alice", question); got.Source != "pdp" || got.Units != 1 {
				t.Fatalf("answer = %+v", got)
			}
		}
		if fake.Requests() != 3 {
			t.Fatalf("requests = %d, want 3", fake.Requests())
		}
	})
	t.Run("a true answer with units 0 is kept for cache_ttl", func(t *testing.T) {
		t.Parallel()
		client, fake, clock := newPDPTestClient(t, nil)
		fake.SetAnswer(func(fakeQuestion) map[string]any {
			return map[string]any{"decision": true, "context": map[string]any{"usage": map[string]any{"units": 0}, "limits": map[string]any{"max_object_size": 5}}}
		})
		pdpUseOne(client, "alice", question)
		got := pdpUseOne(client, "alice", question)
		want := pdpAnswer{Decision: true, Units: 0, HasUnits: true, Limits: map[string]int64{"max_object_size": 5}, Source: "cache"}
		if !reflect.DeepEqual(got, want) || fake.Requests() != 1 {
			t.Fatalf("answer = %+v, requests = %d", got, fake.Requests())
		}
		clock.Advance(30 * time.Second)
		if got := pdpUseOne(client, "alice", question); got.Source != "pdp" {
			t.Fatalf("after cache_ttl: %+v", got)
		}
	})
	t.Run("another subject has no hit", func(t *testing.T) {
		t.Parallel()
		client, fake, _ := newPDPTestClient(t, nil)
		fake.SetAnswer(unitsAnswer(0))
		pdpUseOne(client, "alice", question)
		if got := pdpUseOne(client, "bob", question); got.Source != "pdp" || fake.Requests() != 2 {
			t.Fatalf("answer = %+v, requests = %d", got, fake.Requests())
		}
	})
	t.Run("a false answer without budget_exceeded is not kept", func(t *testing.T) {
		t.Parallel()
		client, fake, _ := newPDPTestClient(t, nil)
		fake.SetAnswer(func(fakeQuestion) map[string]any { return map[string]any{"decision": false} })
		pdpUseOne(client, "alice", question)
		pdpUseOne(client, "alice", question)
		if fake.Requests() != 2 {
			t.Fatalf("requests = %d, want 2", fake.Requests())
		}
	})
	t.Run("a true answer without units is not kept", func(t *testing.T) {
		t.Parallel()
		client, fake, _ := newPDPTestClient(t, nil)
		pdpUseOne(client, "alice", question)
		if got := pdpUseOne(client, "alice", question); got.Decision || got.Source != "pdp" || fake.Requests() != 2 {
			t.Fatalf("answer = %+v, requests = %d", got, fake.Requests())
		}
	})
	t.Run("budget_exceeded is kept for retry_after and gives the time that remains", func(t *testing.T) {
		t.Parallel()
		client, fake, clock := newPDPTestClient(t, nil)
		fake.SetAnswer(func(fakeQuestion) map[string]any {
			return map[string]any{"decision": false, "context": map[string]any{"reason": "budget_exceeded", "retry_after": 12}}
		})
		first := pdpUseOne(client, "alice", question)
		if first.RetryAfter != 12 || first.Source != "pdp" {
			t.Fatalf("first = %+v", first)
		}
		if got := pdpUseOne(client, "alice", question); got.Source != "cache" || got.RetryAfter != 12 || got.Reason != "budget_exceeded" || got.Decision {
			t.Fatalf("hit at 0s = %+v", got)
		}
		clock.Advance(5 * time.Second)
		if got := pdpUseOne(client, "alice", question); got.Source != "cache" || got.RetryAfter != 7 {
			t.Fatalf("hit at 5s = %+v", got)
		}
		clock.Advance(6*time.Second + 500*time.Millisecond)
		if got := pdpUseOne(client, "alice", question); got.Source != "cache" || got.RetryAfter != 1 {
			t.Fatalf("hit at 11.5s = %+v, want RetryAfter 1 (never less than 1)", got)
		}
		clock.Advance(500 * time.Millisecond)
		if got := pdpUseOne(client, "alice", question); got.Source != "pdp" {
			t.Fatalf("at 12s = %+v, want a new question", got)
		}
		if fake.Requests() != 2 {
			t.Fatalf("requests = %d, want 2", fake.Requests())
		}
	})
	t.Run("budget_exceeded is kept for cache_ttl when it is lower than retry_after", func(t *testing.T) {
		t.Parallel()
		client, fake, clock := newPDPTestClient(t, nil)
		fake.SetAnswer(func(fakeQuestion) map[string]any {
			return map[string]any{"decision": false, "context": map[string]any{"reason": "budget_exceeded", "retry_after": 100}}
		})
		pdpUseOne(client, "alice", question)
		if got := pdpUseOne(client, "alice", question); got.Source != "cache" || got.RetryAfter != 30 {
			t.Fatalf("hit at 0s = %+v, want RetryAfter 30", got)
		}
		clock.Advance(30 * time.Second)
		if got := pdpUseOne(client, "alice", question); got.Source != "pdp" {
			t.Fatalf("at 30s = %+v, want a new question", got)
		}
	})
	t.Run("a decide answer never answers a use question and the reverse", func(t *testing.T) {
		t.Parallel()
		client, fake, _ := newPDPTestClient(t, nil)
		fake.SetAnswer(func(q fakeQuestion) map[string]any {
			if q.Usage {
				return map[string]any{"decision": true, "context": map[string]any{"usage": map[string]any{"units": 0}}}
			}
			return map[string]any{"decision": true}
		})
		pdpDecideOne(client, "alice", question)
		if got := pdpUseOne(client, "alice", question); got.Source != "pdp" || !got.HasUnits {
			t.Fatalf("use after decide = %+v", got)
		}
		other := pdpWriteQuestion("b")
		pdpUseOne(client, "alice", other)
		if got := pdpDecideOne(client, "alice", other); got.Source != "pdp" {
			t.Fatalf("decide after use = %+v", got)
		}
		if fake.Requests() != 4 {
			t.Fatalf("requests = %d, want 4", fake.Requests())
		}
		if questions := fake.Questions(); questions[0].Usage || !questions[1].Usage || !questions[2].Usage || questions[3].Usage {
			t.Fatalf("questions = %+v", questions)
		}
	})
}

func TestPDPCacheIsBounded(t *testing.T) {
	t.Parallel()
	t.Run("when full, the client keeps no new answer", func(t *testing.T) {
		t.Parallel()
		client, fake, _ := newPDPTestClient(t, nil)
		client.maxEntries = 2
		pdpDecideOne(client, "alice", pdpReadQuestion("a"))
		pdpDecideOne(client, "alice", pdpReadQuestion("b"))
		pdpDecideOne(client, "alice", pdpReadQuestion("c"))
		if got := pdpDecideOne(client, "alice", pdpReadQuestion("c")); got.Source != "pdp" {
			t.Fatalf("c was kept in a full table: %+v", got)
		}
		if got := pdpDecideOne(client, "alice", pdpReadQuestion("a")); got.Source != "cache" {
			t.Fatalf("a was lost: %+v", got)
		}
		if fake.Requests() != 4 {
			t.Fatalf("requests = %d, want 4", fake.Requests())
		}
	})
	t.Run("the two tables count together", func(t *testing.T) {
		t.Parallel()
		client, fake, _ := newPDPTestClient(t, nil)
		client.maxEntries = 2
		fake.SetAnswer(func(fakeQuestion) map[string]any {
			return map[string]any{"decision": true, "context": map[string]any{"usage": map[string]any{"units": 0}}}
		})
		pdpDecideOne(client, "alice", pdpReadQuestion("a"))
		pdpUseOne(client, "alice", pdpWriteQuestion("a"))
		pdpUseOne(client, "alice", pdpWriteQuestion("b"))
		if got := pdpUseOne(client, "alice", pdpWriteQuestion("b")); got.Source != "pdp" {
			t.Fatalf("b was kept in a full table: %+v", got)
		}
	})
	t.Run("when full, the expired entries go first", func(t *testing.T) {
		t.Parallel()
		client, _, clock := newPDPTestClient(t, nil)
		client.maxEntries = 2
		pdpDecideOne(client, "alice", pdpReadQuestion("a"))
		pdpDecideOne(client, "alice", pdpReadQuestion("b"))
		clock.Advance(31 * time.Second)
		pdpDecideOne(client, "alice", pdpReadQuestion("c"))
		if got := pdpDecideOne(client, "alice", pdpReadQuestion("c")); got.Source != "cache" {
			t.Fatalf("c was not kept after the purge: %+v", got)
		}
		client.mu.Lock()
		entries, size := len(client.decideKept)+len(client.useKept), client.keptBytes
		client.mu.Unlock()
		want := len(pdpCacheKey("alice", pdpReadQuestion("c"))) + pdpCacheEntryOverhead
		if entries != 1 || size != want {
			t.Fatalf("entries = %d, bytes = %d, want 1 and %d", entries, size, want)
		}
	})
	t.Run("the bound of bytes", func(t *testing.T) {
		t.Parallel()
		client, _, _ := newPDPTestClient(t, nil)
		oneEntry := len(pdpCacheKey("alice", pdpReadQuestion("a"))) + pdpCacheEntryOverhead
		client.maxBytes = oneEntry + oneEntry/2
		pdpDecideOne(client, "alice", pdpReadQuestion("a"))
		pdpDecideOne(client, "alice", pdpReadQuestion("b"))
		if got := pdpDecideOne(client, "alice", pdpReadQuestion("b")); got.Source != "pdp" {
			t.Fatalf("b was kept above the bound of bytes: %+v", got)
		}
		if got := pdpDecideOne(client, "alice", pdpReadQuestion("a")); got.Source != "cache" {
			t.Fatalf("a was lost: %+v", got)
		}
	})
	t.Run("the default bounds", func(t *testing.T) {
		t.Parallel()
		client, _, _ := newPDPTestClient(t, nil)
		if client.maxEntries != 16384 || client.maxBytes != 8<<20 {
			t.Fatalf("bounds = %d, %d", client.maxEntries, client.maxBytes)
		}
	})
}

func TestPDPConcurrentUse(t *testing.T) {
	t.Parallel()
	client, fake, _ := newPDPTestClient(t, nil)
	fake.SetAnswer(func(q fakeQuestion) map[string]any {
		if q.Usage {
			return map[string]any{"decision": true, "context": map[string]any{"usage": map[string]any{"units": 0}}}
		}
		return map[string]any{"decision": true}
	})
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for round := 0; round < 20; round++ {
				name := strconv.Itoa((worker + round) % 5)
				client.decide(context.Background(), "req", "alice", []pdpQuestion{pdpReadQuestion(name), pdpReadQuestion("z" + name)})
				pdpUseOne(client, "alice", pdpWriteQuestion(name))
			}
		}(worker)
	}
	wg.Wait()
}

// The tests of the log change the output of the standard logger. They are not parallel.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var out bytes.Buffer
	original := log.Writer()
	log.SetOutput(&out)
	t.Cleanup(func() { log.SetOutput(original) })
	return &out
}

func TestPDPLogLines(t *testing.T) {
	out := captureLog(t)
	client, fake, _ := newPDPTestClient(t, nil)
	fake.SetAnswer(func(q fakeQuestion) map[string]any {
		if q.Usage {
			return map[string]any{"decision": false, "context": map[string]any{"reason": "budget_exceeded", "retry_after": 12}}
		}
		return map[string]any{"decision": true, "context": map[string]any{"usage": map[string]any{"units": 0}}}
	})
	client.decide(context.Background(), "req-1", "alice", []pdpQuestion{pdpReadQuestion("a")})
	client.decide(context.Background(), "req-2", "alice", []pdpQuestion{pdpReadQuestion("a")})
	pdpUseOne(client, "alice", pdpWriteQuestion("a"))
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("log =\n%s\nwant 2 lines: a cache hit writes no line without logCacheHits", out.String())
	}
	wantFirst := `access.decision request_id=req-1 subject="alice" action=object.read resource="service:svc/bucket:logs/folder:-" match=node usage=false decision=true units=0 retry_after=- reason=- source=pdp failed=false`
	if !strings.HasSuffix(lines[0], wantFirst) {
		t.Fatalf("line 1 = %q\nwant suffix %q", lines[0], wantFirst)
	}
	wantUse := `access.decision request_id=req-1 subject="alice" action=object.write resource="service:svc/bucket:logs/folder:-" match=node usage=true decision=false units=- retry_after=12 reason=budget_exceeded source=pdp failed=false`
	if !strings.HasSuffix(lines[1], wantUse) {
		t.Fatalf("line 2 = %q\nwant suffix %q", lines[1], wantUse)
	}

	out.Reset()
	client.logCacheHits = true
	client.decide(context.Background(), "req-3", "alice", []pdpQuestion{pdpReadQuestion("a")})
	if got := out.String(); !strings.Contains(got, "request_id=req-3 ") || !strings.Contains(got, "source=cache failed=false") {
		t.Fatalf("log = %q, want a cache line", got)
	}
	// The detailed log mode shows the folder ids.
	if got := out.String(); !strings.Contains(got, `resource="service:svc/bucket:logs/folder:a"`) {
		t.Fatalf("log = %q, want the folder id in the detailed mode", got)
	}
}

// The anonymous log mode keeps the folder ids and the subfolder ids out of the log, as it keeps the object keys out.
func TestLoggedResourcePathMasksTheFolderIDs(t *testing.T) {
	resource := keyResource("svc", "logs", "a/b/c.txt")
	if got := loggedResourcePath(resource, false); got != "service:svc/bucket:logs/folder:-/subfolder:-" {
		t.Fatalf("anonymous path = %q", got)
	}
	if got := loggedResourcePath(resource, true); got != "service:svc/bucket:logs/folder:a/subfolder:b" {
		t.Fatalf("detailed path = %q", got)
	}
	if got := loggedResourcePath(bucketResource("svc", "logs"), false); got != "service:svc/bucket:logs" {
		t.Fatalf("bucket path = %q", got)
	}
}

func TestPDPLogOfFailures(t *testing.T) {
	out := captureLog(t)
	client, fake, _ := newPDPTestClient(t, nil)
	fake.SetStatus(500)
	pdpDecideOne(client, "alice", pdpReadQuestion("a"))
	fake.SetStatus(401)
	pdpDecideOne(client, "alice", pdpReadQuestion("b"))
	fake.SetStatus(0)
	fake.SetRaw([]byte(`{"decision":false,"context":{"reason":"bad reason with spaces\nand a line"}}`))
	pdpDecideOne(client, "alice", pdpReadQuestion("c"))
	got := out.String()
	for _, want := range []string{
		`access.pdp_failed request_id=req-1 evaluations=1 error="HTTP status 500"`,
		`access.pdp_credential_refused request_id=req-1 status=401`,
		`resource="service:svc/bucket:logs/folder:-" match=node usage=false decision=false units=- retry_after=- reason=- source=error failed=true`,
		`reason="bad reason with spaces\nand a line" source=pdp failed=false`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log lacks %q\n%s", want, got)
		}
	}
	if strings.Contains(got, fakePDPToken) || strings.Contains(got, "Bearer") {
		t.Fatalf("the log holds the token or a header:\n%s", got)
	}
	if strings.Count(got, "\n") != strings.Count(strings.TrimRight(got, "\n"), "\n")+1 || strings.Contains(got, "and a line\n") {
		t.Fatalf("a value of the PDP added a line to the log:\n%s", got)
	}
}

func TestPDPLogTokenOfUnreachablePDP(t *testing.T) {
	out := captureLog(t)
	client, fake, _ := newPDPTestClient(t, nil)
	fake.Server.Close()
	if got := pdpDecideOne(client, "alice", pdpReadQuestion("a")); !got.Failed {
		t.Fatalf("answer = %+v", got)
	}
	if got := out.String(); !strings.Contains(got, "access.pdp_failed request_id=req-1 evaluations=1 error=") || strings.Contains(got, fakePDPToken) || strings.Contains(got, "http://") {
		t.Fatalf("log = %s", got)
	}
}
