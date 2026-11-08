package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// The tests of the access middleware, of the route table and of authorize. They use the fake PDP and tokens that the
// tests sign with real times. The answer cache is off (cache_ttl 0), so each request asks the fake PDP again.

const (
	atSubject   = "user-1"
	atServiceID = idtAudience
	atBucket    = "service:" + atServiceID + "/bucket:rw"
	atFolder    = atBucket + "/folder:folder"
	atSubfolder = atFolder + "/subfolder:nested"
)

type accessTestEnv struct {
	t       *testing.T
	app     *application
	handler http.Handler
	fake    *fakePDP
	keys    idtKeys
}

// newAccessTestEnv builds an application in mode authzen on top of testApplication: memory backends, the fake PDP, a
// verifier with test keys and the action document of the configuration.
func newAccessTestEnv(t *testing.T) *accessTestEnv {
	t.Helper()
	app, _, _ := testApplication(t)
	app.config.Buckets = []bucketConfig{app.instances["readonly"].cfg, app.instances["rw"].cfg}
	app.publicFS = fstest.MapFS{
		"index.html":    &fstest.MapFile{Data: []byte("ok")},
		"assets/app.js": &fstest.MapFile{Data: []byte("console.log(1)")},
	}
	// The probe of the provider has not run: the versions need this fact.
	app.instances["rw"].versioningChecked, app.instances["rw"].versioningSupported = true, true

	fake, pdpConfig := newFakePDP(t)
	keys := newIdtKeys(t)
	cfg := idtConfig(keys.edKey("ed1"))
	cfg.Header = defaultIdentityHeader
	cfg.PDPURL, cfg.PDPPlainHTTP, cfg.PDPToken = pdpConfig.PDPURL, pdpConfig.PDPPlainHTTP, pdpConfig.PDPToken
	cfg.PDPTimeout, cfg.PDPUsageTimeout, cfg.PDPMaxEvaluations = pdpConfig.PDPTimeout, pdpConfig.PDPUsageTimeout, pdpConfig.PDPMaxEvaluations
	cfg.PDPCacheTTL = 0
	app.config.Access = cfg
	doc, err := buildAccessDocument(app.config)
	if err != nil {
		t.Fatal(err)
	}
	app.accessDoc = doc
	app.identity = newIdentityVerifier(cfg)
	// app.close closes the client.
	app.pdp = newPDPClient(cfg, false)
	return &accessTestEnv{t: t, app: app, handler: app.routes(), fake: fake, keys: keys}
}

func (e *accessTestEnv) tokenFor(subject string) string {
	now := time.Now().Unix()
	return idtSign(e.t, e.keys, "ed", idtHeader("Ed25519", "ed1"), idtClaims(idtMap{"sub": subject, "iat": now - 10, "exp": now + 290}))
}

func (e *accessTestEnv) token() string { return e.tokenFor(atSubject) }

// do sends a request. The header values replace the header of the request. A nil body is an empty body.
func (e *accessTestEnv) do(method, target, body string, header http.Header) *httptest.ResponseRecorder {
	e.t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, target, reader)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, values := range header {
		request.Header[http.CanonicalHeaderKey(name)] = values
	}
	recorder := httptest.NewRecorder()
	e.handler.ServeHTTP(recorder, request)
	return recorder
}

// doToken sends a request with a valid identity token.
func (e *accessTestEnv) doToken(method, target, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.do(method, target, body, http.Header{defaultIdentityHeader: {e.token()}})
}

type atErrorBody struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		Action    string `json:"action"`
		Retryable bool   `json:"retryable"`
	} `json:"error"`
}

func atDecodeError(t *testing.T, recorder *httptest.ResponseRecorder) atErrorBody {
	t.Helper()
	var body atErrorBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("status %d, body %q is not an error object: %v", recorder.Code, recorder.Body.String(), err)
	}
	return body
}

// atCaptureLog gives the text that the standard logger writes until the returned function runs.
func atCaptureLog(t *testing.T) func() string {
	t.Helper()
	var buffer bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buffer)
	var done bool
	stop := func() string {
		if !done {
			log.SetOutput(previous)
			done = true
		}
		return buffer.String()
	}
	t.Cleanup(func() { stop() })
	return stop
}

func atAnswerAll(decision bool) func(fakeQuestion) map[string]any {
	return func(fakeQuestion) map[string]any { return map[string]any{"decision": decision} }
}

// atUsageAnswer answers a usage question with units 1, and the other questions with the decision.
func atUsageAnswer(decision bool) func(fakeQuestion) map[string]any {
	return func(q fakeQuestion) map[string]any {
		if q.Usage && decision {
			return map[string]any{"decision": true, "context": map[string]any{"usage": map[string]any{"units": 1}}}
		}
		return map[string]any{"decision": decision}
	}
}

// ---- the middleware ----

func TestAccessMiddlewareRefusesBadTokens(t *testing.T) {
	env := newAccessTestEnv(t)
	valid := env.token()
	signedByOther := idtSign(t, env.keys, "other", idtHeader("Ed25519", "ed1"), idtClaims(idtMap{"iat": time.Now().Unix() - 10, "exp": time.Now().Unix() + 290}))
	target := "/api/list?instance=rw"
	tests := []struct {
		name   string
		target string
		header http.Header
	}{
		{"no token", target, nil},
		{"two copies of the header", target, http.Header{defaultIdentityHeader: {valid, valid}}},
		{"bad signature", target, http.Header{defaultIdentityHeader: {signedByOther}}},
		{"garbage", target, http.Header{defaultIdentityHeader: {"not-a-token"}}},
		{"token only in Authorization", target, http.Header{"Authorization": {"Bearer " + valid}}},
		{"token only in a cookie", target, http.Header{"Cookie": {defaultIdentityHeader + "=" + valid}}},
		{"token only in the query", target + "&" + url.QueryEscape(defaultIdentityHeader) + "=" + valid, nil},
		{"a page without token", "/", nil},
		{"a page of a bucket without token", "/-/test-auth/rw/", nil},
		{"build without token", "/api/build", nil},
		{"an asset without token", "/assets/app.js", nil},
		{"the gateway without token", "/s3?instance=rw&key=folder%2Fdirect.txt", nil},
		{"the old path of the action document", "/.well-known/access-actions", nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := env.do(http.MethodGet, test.target, "", test.header)
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body %q", recorder.Code, recorder.Body.String())
			}
			body := atDecodeError(t, recorder)
			if body.Error.Code != "unauthenticated" || body.Error.Message != "A valid identity token is required." {
				t.Fatalf("error = %+v", body.Error)
			}
		})
	}
	if count := env.fake.Requests(); count != 0 {
		t.Fatalf("the PDP got %d requests, want 0", count)
	}
}

func TestAccessMiddlewareClosesTheConnectionOfAnUnsafeRefusal(t *testing.T) {
	env := newAccessTestEnv(t)
	recorder := env.do(http.MethodPost, "/api/copy", `{"instance":"rw","src":"a","dst":"b"}`, nil)
	if recorder.Code != http.StatusUnauthorized || recorder.Header().Get("Connection") != "close" {
		t.Fatalf("POST: status %d, Connection %q", recorder.Code, recorder.Header().Get("Connection"))
	}
	recorder = env.do(http.MethodGet, "/api/list?instance=rw", "", nil)
	if recorder.Code != http.StatusUnauthorized || recorder.Header().Get("Connection") != "" {
		t.Fatalf("GET: status %d, Connection %q", recorder.Code, recorder.Header().Get("Connection"))
	}
}

func TestAccessMiddlewareExemptPaths(t *testing.T) {
	env := newAccessTestEnv(t)
	badToken := http.Header{defaultIdentityHeader: {"not-a-token"}}
	for _, header := range []http.Header{nil, badToken} {
		recorder := env.do(http.MethodGet, "/healthz", "", header)
		if recorder.Code != http.StatusOK {
			t.Fatalf("/healthz: status %d", recorder.Code)
		}
		recorder = env.do(http.MethodHead, "/healthz", "", header)
		if recorder.Code != http.StatusOK {
			t.Fatalf("HEAD /healthz: status %d", recorder.Code)
		}
		recorder = env.do(http.MethodGet, accessActionsPath, "", header)
		if recorder.Code != http.StatusOK || !bytes.Equal(recorder.Body.Bytes(), env.app.accessDoc.json()) {
			t.Fatalf("action document: status %d, body %q", recorder.Code, recorder.Body.String())
		}
		recorder = env.do(http.MethodHead, accessActionsPath, "", header)
		if recorder.Code != http.StatusOK || recorder.Body.Len() != 0 {
			t.Fatalf("HEAD action document: status %d, %d body bytes", recorder.Code, recorder.Body.Len())
		}
	}
	if count := env.fake.Requests(); count != 0 {
		t.Fatalf("the PDP got %d requests, want 0", count)
	}
	// The path of the old protocol is a plain unknown path: 404 after the token.
	recorder := env.doToken(http.MethodGet, "/.well-known/access-actions", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("old path: status %d, want 404", recorder.Code)
	}
}

func TestAccessMiddlewareRequestLogHasTheSubject(t *testing.T) {
	env := newAccessTestEnv(t)
	logs := atCaptureLog(t)
	header := http.Header{defaultIdentityHeader: {env.token()}, "X-Request-ID": {"req-log-1"}}
	if recorder := env.do(http.MethodGet, "/api/build", "", header); recorder.Code != http.StatusOK {
		t.Fatalf("status %d", recorder.Code)
	}
	env.do(http.MethodGet, "/api/build", "", nil)
	text := logs()
	if !strings.Contains(text, `subject="user-1" request_id=req-log-1`) {
		t.Fatalf("the request log has no subject:\n%s", text)
	}
	if !strings.Contains(text, "access.unauthenticated request_id=") || !strings.Contains(text, "reason=missing") {
		t.Fatalf("the refusal is not in the log:\n%s", text)
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "access.unauthenticated") && strings.Contains(line, "subject=") {
			t.Fatalf("a refusal line has a subject: %s", line)
		}
	}
}

func TestRequestIDFrom(t *testing.T) {
	valid := []string{"a", "req-1", "A.b_c:d-9", strings.Repeat("x", 128)}
	for _, value := range valid {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.Header.Set("X-Request-ID", value)
		if got := requestIDFrom(request); got != value {
			t.Errorf("requestIDFrom(%q) = %q", value, got)
		}
	}
	invalid := []string{"", "has space", strings.Repeat("x", 129), "semi;colon", "slash/x", "é"}
	for _, value := range invalid {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		if value != "" {
			request.Header.Set("X-Request-ID", value)
		}
		got := requestIDFrom(request)
		if got == value || len(got) != 32 || strings.Trim(got, "0123456789abcdef") != "" {
			t.Errorf("requestIDFrom(%q) = %q, want a random hex id", value, got)
		}
	}
}

// ---- the route table ----

func TestRouteTableKinds(t *testing.T) {
	app, _, _ := testApplication(t)
	exempt := map[string]bool{}
	seen := map[string]bool{}
	for _, entry := range app.routeTable() {
		if entry.access == routeDenied {
			t.Errorf("route %q has no access kind", entry.pattern)
		}
		if entry.handler == nil {
			t.Errorf("route %q has no handler", entry.pattern)
		}
		if seen[entry.pattern] {
			t.Errorf("route %q is twice in the table", entry.pattern)
		}
		seen[entry.pattern] = true
		if entry.access == routeExempt {
			exempt[entry.pattern] = true
		}
		// The middleware exempts the concrete paths of the exempt routes, and no other path.
		if got := accessExemptPath(entry.pattern); got != (entry.access == routeExempt) {
			t.Errorf("accessExemptPath(%q) = %v, but the kind is %d", entry.pattern, got, entry.access)
		}
	}
	if len(exempt) != 2 || !exempt["/healthz"] || !exempt[accessActionsPath] {
		t.Errorf("exempt routes = %v, want /healthz and %s", exempt, accessActionsPath)
	}
	for _, path := range []string{"/", "/healthz/", "//healthz", "/healthz2", accessActionsPath + "/", "/api/build", "/.well-known/access-actions"} {
		if accessExemptPath(path) {
			t.Errorf("accessExemptPath(%q) = true", path)
		}
	}
	tokenOnly := map[string]bool{"/-/": true, "/api/instances": true, "/api/build": true, "/api/jobs": true, "/": true}
	for _, entry := range app.routeTable() {
		if (entry.access == routeTokenOnly) != tokenOnly[entry.pattern] {
			t.Errorf("route %q has the kind %d", entry.pattern, entry.access)
		}
	}
}

func TestRouteWithoutKindIsDenied(t *testing.T) {
	env := newAccessTestEnv(t)
	called := false
	handler := env.app.routesFor([]routeEntry{
		{"/api/unruled", routeDenied, func(http.ResponseWriter, *http.Request) { called = true }},
		{"/api/ruled", routeTokenOnly, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }},
	})
	logs := atCaptureLog(t)
	send := func(target string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		request.Header.Set(defaultIdentityHeader, env.token())
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	recorder := send("/api/unruled")
	if recorder.Code != http.StatusForbidden || called {
		t.Fatalf("status %d, handler called %v", recorder.Code, called)
	}
	body := atDecodeError(t, recorder)
	if body.Error.Code != "forbidden" || body.Error.Action != "unknown" {
		t.Fatalf("error = %+v", body.Error)
	}
	if recorder := send("/api/ruled"); recorder.Code != http.StatusNoContent {
		t.Fatalf("a ruled route gives %d", recorder.Code)
	}
	if text := logs(); !strings.Contains(text, "access.no_rule request_id=") {
		t.Fatalf("no access.no_rule line:\n%s", text)
	}
	if count := env.fake.Requests(); count != 0 {
		t.Fatalf("the PDP got %d requests", count)
	}
}

func TestTokenOnlyRoutesAskNothing(t *testing.T) {
	env := newAccessTestEnv(t)
	for _, target := range []string{"/", "/index.html", "/-/test-auth/rw/", "/api/build", "/assets/app.js", "/api/instances"} {
		recorder := env.doToken(http.MethodGet, target, "")
		if recorder.Code != http.StatusOK {
			t.Errorf("%s: status %d, body %q", target, recorder.Code, recorder.Body.String())
		}
	}
	if recorder := env.doToken(http.MethodGet, "/api/jobs", ""); recorder.Code != http.StatusNotFound {
		t.Errorf("/api/jobs: status %d, want 404", recorder.Code)
	}
	if count := env.fake.Requests(); count != 0 {
		t.Fatalf("the PDP got %d requests, want 0", count)
	}
}

func TestOptionsOnTheGatewayAsksNothing(t *testing.T) {
	env := newAccessTestEnv(t)
	recorder := env.doToken(http.MethodOptions, "/s3?instance=rw&key=folder%2Fdirect.txt", "")
	if recorder.Code != http.StatusNoContent || recorder.Header().Get("Allow") == "" {
		t.Fatalf("status %d, Allow %q", recorder.Code, recorder.Header().Get("Allow"))
	}
	if count := env.fake.Requests(); count != 0 {
		t.Fatalf("the PDP got %d requests, want 0", count)
	}
	if recorder := env.do(http.MethodOptions, "/s3?instance=rw", "", nil); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("OPTIONS without token: status %d, want 401", recorder.Code)
	}
}

func TestOpenModeDoesNothing(t *testing.T) {
	env := newAccessTestEnv(t)
	env.app.config.Access = accessConfig{Mode: accessModeOpen}
	env.app.accessDoc = accessDocument{}
	handler := env.app.routes()
	for _, target := range []string{"/api/list?instance=rw", "/api/build", "/s3?instance=rw&key=folder%2Fdirect.txt", "/"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
		if recorder.Code != http.StatusOK {
			t.Errorf("%s: status %d", target, recorder.Code)
		}
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, accessActionsPath, nil))
	if recorder.Code != http.StatusNotFound {
		t.Errorf("action document in open mode: status %d, want 404", recorder.Code)
	}
	if count := env.fake.Requests(); count != 0 {
		t.Fatalf("the PDP got %d requests", count)
	}
}

// ---- the read routes ----

type atReadRoute struct {
	name   string
	method string
	target string
	body   string
	// gateway is true for the routes that answer with a plain text error.
	gateway bool
	// resource is the path of the expected question. wants replaces it for a route that asks several questions.
	resource string
	wants    []string
}

func (r atReadRoute) expected() []string {
	if len(r.wants) > 0 {
		return r.wants
	}
	return []string{r.resource}
}

func atReadRoutes() []atReadRoute {
	key := "instance=rw&key=folder%2Fdirect.txt"
	object := func(name, path string) atReadRoute {
		return atReadRoute{name: name, method: http.MethodGet, target: path + "?" + key, resource: atFolder}
	}
	return []atReadRoute{
		{name: "s3 get", method: http.MethodGet, target: "/s3?" + key, gateway: true, resource: atFolder},
		{name: "s3 head", method: http.MethodHead, target: "/s3?" + key, gateway: true, resource: atFolder},
		{name: "s3 raw list", method: http.MethodGet, target: "/s3?instance=rw&prefix=folder%2F", gateway: true, resource: atFolder},
		{name: "s3 raw list of the root", method: http.MethodGet, target: "/s3?instance=rw", gateway: true, resource: atBucket},
		{name: "s3 raw list head", method: http.MethodHead, target: "/s3?instance=rw&prefix=folder%2Fnested%2F", gateway: true, resource: atSubfolder},
		{name: "open get", method: http.MethodGet, target: "/open/?" + key, gateway: true, resource: atFolder},
		{name: "open head", method: http.MethodHead, target: "/open/?" + key, gateway: true, resource: atFolder},
		{name: "list", method: http.MethodGet, target: "/api/list?instance=rw&prefix=folder%2Fnested%2F", resource: atSubfolder},
		{name: "list head", method: http.MethodHead, target: "/api/list?instance=rw&prefix=folder%2Fnested%2F", resource: atSubfolder},
		{name: "list of the root", method: http.MethodGet, target: "/api/list?instance=rw", resource: atBucket},
		{name: "stats", method: http.MethodGet, target: "/api/stats?instance=rw&prefix=folder%2F", resource: atFolder},
		{name: "archive", method: http.MethodGet, target: "/api/archive?instance=rw&prefix=folder%2F", resource: atFolder},
		object("spreadsheet", "/api/spreadsheet"),
		object("delimited", "/api/delimited"),
		object("document count", "/api/document-count"),
		object("word", "/api/document/word"),
		object("parquet", "/api/parquet"),
		object("json raw", "/api/json/raw"),
		object("json beautify", "/api/json/beautify"),
		object("json summary", "/api/json/summary"),
		object("json tree", "/api/json/tree"),
		{name: "search", method: http.MethodGet, target: "/api/search?" + key + "&q=direct", resource: atFolder},
		object("media info", "/api/media-info"),
		object("structured preview", "/api/structured-preview"),
		object("archive preview", "/api/archive-preview"),
		{name: "archive entry", method: http.MethodGet, target: "/api/archive-entry?" + key + "&entry=a.txt", resource: atFolder},
		{name: "archive entry head", method: http.MethodHead, target: "/api/archive-entry?" + key + "&entry=a.txt", resource: atFolder},
		object("image preview", "/api/image-preview"),
		object("inspect", "/api/inspect"),
		object("versions", "/api/versions"),
		{name: "version counts", method: http.MethodPost, target: "/api/version-counts",
			body:  `{"instance":"rw","keys":["folder/direct.txt","folder/nested/deep.txt","folder/direct.txt"]}`,
			wants: []string{atSubfolder, atFolder}},
		{name: "integrity", method: http.MethodPost, target: "/api/integrity", body: `{"instance":"rw","key":"folder/direct.txt"}`, resource: atFolder},
		{name: "archive entry integrity", method: http.MethodPost, target: "/api/archive-entry/integrity",
			body: `{"instance":"rw","key":"folder/direct.txt","entry":"a.txt"}`, resource: atFolder},
		object("tags", "/api/tags"),
		object("metadata", "/api/metadata"),
		object("object lock", "/api/object-lock"),
		{name: "encryption", method: http.MethodGet, target: "/api/encryption?instance=rw", resource: atBucket},
		{name: "usage", method: http.MethodGet, target: "/api/usage?instance=rw", resource: atBucket},
		{name: "lifecycle", method: http.MethodGet, target: "/api/lifecycle?instance=rw", resource: atBucket},
		{name: "permissions refresh", method: http.MethodPost, target: "/api/permissions/refresh?instance=rw", resource: atBucket},
		{name: "multiparts", method: http.MethodGet, target: "/api/multiparts?instance=rw&prefix=folder%2F", resource: atFolder},
	}
}

func TestReadRoutesAskObjectReadOnTheNode(t *testing.T) {
	for _, route := range atReadRoutes() {
		t.Run(route.name, func(t *testing.T) {
			env := newAccessTestEnv(t)

			// The PDP says false: 403 with the action.
			env.fake.SetAnswer(atAnswerAll(false))
			recorder := env.doToken(route.method, route.target, route.body)
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("false: status %d, body %q", recorder.Code, recorder.Body.String())
			}
			if route.gateway {
				if !strings.Contains(recorder.Body.String(), "The action object.read is not allowed.") && route.method != http.MethodHead {
					t.Fatalf("false: body %q", recorder.Body.String())
				}
			} else if route.method != http.MethodHead {
				body := atDecodeError(t, recorder)
				if body.Error.Code != "forbidden" || body.Error.Action != "object.read" {
					t.Fatalf("false: error %+v", body.Error)
				}
			}
			if questions := env.fake.Questions(); len(questions) == 0 || questions[0].Path != route.expected()[0] {
				t.Fatalf("false: questions %+v", questions)
			}

			// The PDP says true: the handler goes on, with exactly the expected questions.
			env.fake.Reset()
			env.fake.SetAnswer(atAnswerAll(true))
			recorder = env.doToken(route.method, route.target, route.body)
			switch recorder.Code {
			case http.StatusUnauthorized, http.StatusForbidden, http.StatusServiceUnavailable:
				t.Fatalf("true: status %d, body %q", recorder.Code, recorder.Body.String())
			}
			questions := env.fake.Questions()
			want := route.expected()
			if len(questions) != len(want) {
				t.Fatalf("true: %d questions %+v, want %d", len(questions), questions, len(want))
			}
			for index, question := range questions {
				if question.Action != actionObjectRead || question.Path != want[index] || question.Match != "node" || question.Usage || question.Subject != atSubject {
					t.Fatalf("true: question %d = %+v, want object.read on %s", index, question, want[index])
				}
			}

			// The PDP fails: 503 pdp_unavailable.
			env.fake.Reset()
			env.fake.SetStatus(http.StatusInternalServerError)
			recorder = env.doToken(route.method, route.target, route.body)
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("failure: status %d, body %q", recorder.Code, recorder.Body.String())
			}
			if !route.gateway && route.method != http.MethodHead {
				body := atDecodeError(t, recorder)
				if body.Error.Code != "pdp_unavailable" || body.Error.Action != "object.read" || !body.Error.Retryable {
					t.Fatalf("failure: error %+v", body.Error)
				}
			}

			// An unknown instance gives 404 and no question.
			env.fake.Reset()
			unknown := strings.Replace(route.target, "instance=rw", "instance=nope", 1)
			body := strings.Replace(route.body, `"instance":"rw"`, `"instance":"nope"`, 1)
			recorder = env.doToken(route.method, unknown, body)
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("unknown instance: status %d, body %q", recorder.Code, recorder.Body.String())
			}
			if count := len(env.fake.Questions()); count != 0 {
				t.Fatalf("unknown instance: %d questions", count)
			}
		})
	}
}

// The process ceiling comes before the PDP: a bucket without the permission read answers 403 permission_denied and
// asks nothing.
func TestProcessCeilingComesBeforeThePDP(t *testing.T) {
	env := newAccessTestEnv(t)
	env.app.instances["rw"].caps.set(permissionRead, deniedCapability("configuration", "off", true))
	recorder := env.doToken(http.MethodGet, "/api/list?instance=rw", "")
	if recorder.Code != http.StatusForbidden || atDecodeError(t, recorder).Error.Code != "permission_denied" {
		t.Fatalf("status %d, body %q", recorder.Code, recorder.Body.String())
	}
	if count := env.fake.Requests(); count != 0 {
		t.Fatalf("the PDP got %d requests", count)
	}
}

// ---- authorize ----

func (e *accessTestEnv) request(requestID string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/api/list?instance=rw", nil)
	ctx := context.WithValue(request.Context(), accessRequestKey{}, &accessRequest{Subject: atSubject, RequestID: requestID})
	return request.WithContext(ctx)
}

func (e *accessTestEnv) ask(action, rel string) accessAsk {
	return e.app.objectAsk(action, e.app.instances["rw"], rel)
}

func atAsAPIError(t *testing.T, err error) apiError {
	t.Helper()
	var apiErr apiError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %v is not an apiError", err)
	}
	return apiErr
}

type atQuestion struct {
	Action string
	Path   string
	Usage  bool
	Batch  bool
}

func atSummarize(questions []fakeQuestion) []atQuestion {
	out := make([]atQuestion, len(questions))
	for index, q := range questions {
		out[index] = atQuestion{q.Action, q.Path, q.Usage, q.Batch}
	}
	return out
}

func atSameSummary(got, want []atQuestion) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

func TestAuthorizeWithoutIdentityGives401(t *testing.T) {
	env := newAccessTestEnv(t)
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	_, err := env.app.authorize(request, accessOperation{Use: env.ask(actionObjectRead, "a.txt")})
	if apiErr := atAsAPIError(t, err); apiErr.Status != http.StatusUnauthorized || apiErr.Code != "unauthenticated" {
		t.Fatalf("error = %+v", apiErr)
	}
	if env.fake.Requests() != 0 {
		t.Fatal("authorize asked the PDP")
	}
}

func TestAuthorizeOpenModeAsksNothing(t *testing.T) {
	env := newAccessTestEnv(t)
	env.app.config.Access = accessConfig{Mode: accessModeOpen}
	grant, err := env.app.authorize(httptest.NewRequest(http.MethodGet, "/", nil), accessOperation{Use: env.ask(actionObjectWrite, "a.txt")})
	if err != nil || len(grant.Limits) != 0 {
		t.Fatalf("grant %+v, error %v", grant, err)
	}
	if env.fake.Requests() != 0 {
		t.Fatal("authorize asked the PDP")
	}
}

func TestAuthorizeAsksTheOthersBeforeTheUse(t *testing.T) {
	env := newAccessTestEnv(t)
	env.fake.SetAnswer(atUsageAnswer(true))
	folderA, folderB := env.ask(actionObjectRead, "a/x"), env.ask(actionObjectRead, "b/y")

	// A use without budget: one batch, the Others then the use.
	_, err := env.app.authorize(env.request("rid-1"), accessOperation{Others: []accessAsk{folderA, folderB}, Use: env.ask(actionObjectRead, "c/z")})
	if err != nil {
		t.Fatal(err)
	}
	want := []atQuestion{
		{actionObjectRead, atBucket + "/folder:a", false, true},
		{actionObjectRead, atBucket + "/folder:b", false, true},
		{actionObjectRead, atBucket + "/folder:c", false, true},
	}
	if got := atSummarize(env.fake.Questions()); !atSameSummary(got, want) {
		t.Fatalf("questions %+v, want %+v", got, want)
	}
	if count := env.fake.Requests(); count != 1 {
		t.Fatalf("%d HTTP requests, want 1", count)
	}
	for _, q := range env.fake.Questions() {
		if q.RequestID != "rid-1" || q.Subject != atSubject || q.Match != "node" {
			t.Fatalf("question %+v", q)
		}
	}

	// A use with a budget: the Others in one batch, then the use alone with usage.
	env.fake.Reset()
	_, err = env.app.authorize(env.request("rid-2"), accessOperation{Others: []accessAsk{folderA, folderB}, Use: env.ask(actionObjectWrite, "c/z")})
	if err != nil {
		t.Fatal(err)
	}
	want = []atQuestion{
		{actionObjectRead, atBucket + "/folder:a", false, true},
		{actionObjectRead, atBucket + "/folder:b", false, true},
		{actionObjectWrite, atBucket + "/folder:c", true, false},
	}
	if got := atSummarize(env.fake.Questions()); !atSameSummary(got, want) {
		t.Fatalf("questions %+v, want %+v", got, want)
	}
	if count := env.fake.Requests(); count != 2 {
		t.Fatalf("%d HTTP requests, want 2", count)
	}

	// One Other: one Access Evaluation, not a batch.
	env.fake.Reset()
	if _, err = env.app.authorize(env.request("rid-3"), accessOperation{Others: []accessAsk{folderA}, Use: env.ask(actionObjectDelete, "c/z")}); err != nil {
		t.Fatal(err)
	}
	want = []atQuestion{
		{actionObjectRead, atBucket + "/folder:a", false, false},
		{actionObjectDelete, atBucket + "/folder:c", true, false},
	}
	if got := atSummarize(env.fake.Questions()); !atSameSummary(got, want) {
		t.Fatalf("questions %+v, want %+v", got, want)
	}
}

func TestAuthorizeReadNeverAsksUsage(t *testing.T) {
	env := newAccessTestEnv(t)
	if _, err := env.app.authorize(env.request("rid"), accessOperation{Use: env.ask(actionObjectRead, "a/x")}); err != nil {
		t.Fatal(err)
	}
	questions := env.fake.Questions()
	if len(questions) != 1 || questions[0].Usage || questions[0].Batch {
		t.Fatalf("questions %+v", questions)
	}
}

func TestAuthorizeRefusedOtherStopsBeforeTheUse(t *testing.T) {
	env := newAccessTestEnv(t)
	env.fake.SetAnswer(func(q fakeQuestion) map[string]any {
		return map[string]any{"decision": q.Action != actionObjectRead, "context": map[string]any{"usage": map[string]any{"units": 1}}}
	})
	_, err := env.app.authorize(env.request("rid"), accessOperation{
		Others: []accessAsk{env.ask(actionObjectRead, "a/x")},
		Use:    env.ask(actionObjectWrite, "a/x"),
	})
	apiErr := atAsAPIError(t, err)
	if apiErr.Status != http.StatusForbidden || apiErr.Code != "forbidden" || apiErr.Action != actionObjectRead || apiErr.Message != "The action object.read is not allowed." {
		t.Fatalf("error = %+v", apiErr)
	}
	questions := env.fake.Questions()
	if len(questions) != 1 || questions[0].Usage {
		t.Fatalf("questions %+v, want only the Other", questions)
	}
}

func TestAuthorizeFailedOtherStopsBeforeTheUse(t *testing.T) {
	env := newAccessTestEnv(t)
	env.fake.SetStatus(http.StatusInternalServerError)
	_, err := env.app.authorize(env.request("rid"), accessOperation{
		Others: []accessAsk{env.ask(actionObjectRead, "a/x")},
		Use:    env.ask(actionObjectWrite, "a/x"),
	})
	apiErr := atAsAPIError(t, err)
	if apiErr.Status != http.StatusServiceUnavailable || apiErr.Code != "pdp_unavailable" || apiErr.Action != actionObjectRead {
		t.Fatalf("error = %+v", apiErr)
	}
	if count := len(env.fake.Questions()); count != 1 {
		t.Fatalf("%d questions, want 1", count)
	}
}

func TestAuthorizeRemovesRepeatedOthersAndTheOtherEqualToTheUse(t *testing.T) {
	env := newAccessTestEnv(t)
	env.fake.SetAnswer(atUsageAnswer(true))
	readA := env.ask(actionObjectRead, "a/x")
	// The nodes of "a/x" and "a/y" are the same node: folder:a.
	sameNode := env.ask(actionObjectRead, "a/y")

	// A use with a budget: the Other that equals the use never comes before the usage question.
	_, err := env.app.authorize(env.request("rid"), accessOperation{
		Others: []accessAsk{env.ask(actionObjectWrite, "a/q"), readA, sameNode, readA},
		Use:    env.ask(actionObjectWrite, "a/x"),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []atQuestion{
		{actionObjectRead, atBucket + "/folder:a", false, false},
		{actionObjectWrite, atBucket + "/folder:a", true, false},
	}
	if got := atSummarize(env.fake.Questions()); !atSameSummary(got, want) {
		t.Fatalf("questions %+v, want %+v", got, want)
	}

	// A use without budget: the question appears one time.
	env.fake.Reset()
	if _, err = env.app.authorize(env.request("rid"), accessOperation{Others: []accessAsk{readA, sameNode}, Use: readA}); err != nil {
		t.Fatal(err)
	}
	want = []atQuestion{{actionObjectRead, atBucket + "/folder:a", false, false}}
	if got := atSummarize(env.fake.Questions()); !atSameSummary(got, want) {
		t.Fatalf("questions %+v, want %+v", got, want)
	}
}

func TestAuthorizeBudgetUsesTheUsageQuestion(t *testing.T) {
	env := newAccessTestEnv(t)
	env.fake.SetAnswer(atUsageAnswer(true))
	for _, action := range []string{actionObjectWrite, actionObjectDelete, actionObjectLock, actionBucketAdmin} {
		env.fake.Reset()
		if _, err := env.app.authorize(env.request("rid"), accessOperation{Use: env.app.bucketAsk(action, env.app.instances["rw"])}); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		want := []atQuestion{{action, atBucket, true, false}}
		if got := atSummarize(env.fake.Questions()); !atSameSummary(got, want) {
			t.Fatalf("%s: questions %+v, want %+v", action, got, want)
		}
		raw := env.fake.RawRequests()
		if len(raw) != 1 || raw[0].Path != pdpEvaluationPath {
			t.Fatalf("%s: requests %+v", action, raw)
		}
		if !strings.Contains(string(raw[0].Body), `"context":{"usage":{"units":1}}`) {
			t.Fatalf("%s: body %s", action, raw[0].Body)
		}
	}
}

func TestAuthorizeBudgetRefusalGives429(t *testing.T) {
	env := newAccessTestEnv(t)
	env.fake.SetAnswer(func(q fakeQuestion) map[string]any {
		return map[string]any{"decision": false, "context": map[string]any{"reason": "budget_exceeded", "retry_after": 7}}
	})
	_, err := env.app.authorize(env.request("rid"), accessOperation{Use: env.ask(actionObjectWrite, "a/x")})
	apiErr := atAsAPIError(t, err)
	if apiErr.Status != http.StatusTooManyRequests || apiErr.Code != "too_many_requests" || apiErr.Action != actionObjectWrite || apiErr.RetryAfter != 7 {
		t.Fatalf("error = %+v", apiErr)
	}

	recorder := httptest.NewRecorder()
	writeAPIError(recorder, err)
	if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") != "7" {
		t.Fatalf("JSON writer: status %d, Retry-After %q", recorder.Code, recorder.Header().Get("Retry-After"))
	}
	body := atDecodeError(t, recorder)
	if body.Error.Code != "too_many_requests" || body.Error.Action != actionObjectWrite || !body.Error.Retryable {
		t.Fatalf("error = %+v", body.Error)
	}
	recorder = httptest.NewRecorder()
	writeGatewayError(recorder, err)
	if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") != "7" || strings.HasPrefix(recorder.Body.String(), "{") {
		t.Fatalf("gateway writer: status %d, Retry-After %q, body %q", recorder.Code, recorder.Header().Get("Retry-After"), recorder.Body.String())
	}
}

func TestAPIErrorWritersKeepTheOldShapes(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeAPIError(recorder, apiError{Status: http.StatusBadRequest, Code: "invalid_key", Message: "bad"})
	if recorder.Header().Get("Retry-After") != "" || strings.Contains(recorder.Body.String(), "action") || strings.Contains(recorder.Body.String(), "retryable") {
		t.Fatalf("header %v, body %q", recorder.Header(), recorder.Body.String())
	}
	// The resource budget error keeps its code and has no Retry-After.
	recorder = httptest.NewRecorder()
	writeAPIError(recorder, resourceLimitError{Kind: "storage_requests", Limit: 1, Used: 2})
	body := atDecodeError(t, recorder)
	if recorder.Code != http.StatusTooManyRequests || body.Error.Code != "resource_budget_exceeded" || recorder.Header().Get("Retry-After") != "" || body.Error.Retryable {
		t.Fatalf("status %d, header %v, error %+v", recorder.Code, recorder.Header(), body.Error)
	}
	recorder = httptest.NewRecorder()
	writeAPIError(recorder, errPDPUnavailable(actionObjectRead))
	if body := atDecodeError(t, recorder); recorder.Code != http.StatusServiceUnavailable || body.Error.Code != "pdp_unavailable" || !body.Error.Retryable {
		t.Fatalf("status %d, error %+v", recorder.Code, body.Error)
	}
}

func TestAuthorizeCeilings(t *testing.T) {
	env := newAccessTestEnv(t)
	limits := func(values map[string]any) func(fakeQuestion) map[string]any {
		return func(fakeQuestion) map[string]any {
			return map[string]any{"decision": true, "context": map[string]any{"limits": values}}
		}
	}

	t.Run("a listed ceiling stays in the grant", func(t *testing.T) {
		env.fake.SetAnswer(limits(map[string]any{"max_object_size": 10}))
		grant, err := env.app.authorize(env.request("rid"), accessOperation{Use: env.ask(actionObjectRead, "a/x")})
		if err != nil {
			t.Fatal(err)
		}
		if got := grant.Limits[actionObjectRead][limitMaxObjectSize]; got != 10 || len(grant.Limits) != 1 {
			t.Fatalf("grant = %+v", grant)
		}
	})
	t.Run("the lower value stays", func(t *testing.T) {
		env.fake.SetAnswer(func(q fakeQuestion) map[string]any {
			size := 50
			if strings.HasSuffix(q.Path, "folder:b") {
				size = 5
			}
			return map[string]any{"decision": true, "context": map[string]any{"limits": map[string]any{"max_object_size": size}}}
		})
		grant, err := env.app.authorize(env.request("rid"), accessOperation{
			Others: []accessAsk{env.ask(actionObjectRead, "b/y")},
			Use:    env.ask(actionObjectRead, "a/x"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := grant.Limits[actionObjectRead][limitMaxObjectSize]; got != 5 {
			t.Fatalf("grant = %+v", grant)
		}
	})
	t.Run("a ceiling that the action does not list gives 403", func(t *testing.T) {
		env.fake.SetAnswer(limits(map[string]any{"max_rows": 5}))
		logs := atCaptureLog(t)
		_, err := env.app.authorize(env.request("rid-c"), accessOperation{Use: env.ask(actionObjectRead, "a/x")})
		apiErr := atAsAPIError(t, err)
		if apiErr.Status != http.StatusForbidden || apiErr.Code != "forbidden" || apiErr.Action != actionObjectRead {
			t.Fatalf("error = %+v", apiErr)
		}
		if text := logs(); !strings.Contains(text, `access.ceiling_unenforceable request_id=rid-c action=object.read ceiling="max_rows"`) {
			t.Fatalf("log:\n%s", text)
		}
	})
	t.Run("a ceiling of another action gives 403", func(t *testing.T) {
		env.fake.SetAnswer(limits(map[string]any{"max_object_size": 5}))
		_, err := env.app.authorize(env.request("rid"), accessOperation{Use: env.ask(actionObjectDelete, "a/x")})
		if apiErr := atAsAPIError(t, err); apiErr.Status != http.StatusForbidden {
			t.Fatalf("error = %+v", apiErr)
		}
	})
	t.Run("a budget as a limit gives 503", func(t *testing.T) {
		env.fake.SetAnswer(limits(map[string]any{"writes": 1}))
		logs := atCaptureLog(t)
		_, err := env.app.authorize(env.request("rid-b"), accessOperation{Use: env.ask(actionObjectRead, "a/x")})
		apiErr := atAsAPIError(t, err)
		if apiErr.Status != http.StatusServiceUnavailable || apiErr.Code != "pdp_unavailable" || apiErr.Action != actionObjectRead {
			t.Fatalf("error = %+v", apiErr)
		}
		if text := logs(); !strings.Contains(text, "access.pdp_invalid_answer request_id=rid-b") {
			t.Fatalf("log:\n%s", text)
		}
	})
	t.Run("a ceiling in the answer to a usage question", func(t *testing.T) {
		env.fake.SetAnswer(func(q fakeQuestion) map[string]any {
			return map[string]any{"decision": true, "context": map[string]any{
				"usage": map[string]any{"units": 1}, "limits": map[string]any{"max_object_size": 99}}}
		})
		grant, err := env.app.authorize(env.request("rid"), accessOperation{Use: env.ask(actionObjectWrite, "a/x")})
		if err != nil {
			t.Fatal(err)
		}
		if got := grant.Limits[actionObjectWrite][limitMaxObjectSize]; got != 99 {
			t.Fatalf("grant = %+v", grant)
		}
	})
}

func TestAuthorizeUnpublishedActionGives403WithoutQuestion(t *testing.T) {
	env := newAccessTestEnv(t)
	// An action that the document never has.
	_, err := env.app.authorize(env.request("rid"), accessOperation{Use: accessAsk{Action: "object.bogus", Resource: bucketResource(atServiceID, "rw")}})
	apiErr := atAsAPIError(t, err)
	if apiErr.Status != http.StatusForbidden || apiErr.Action != "object.bogus" {
		t.Fatalf("error = %+v", apiErr)
	}
	// A write action in a service with the policy force_read_only.
	env.app.config.Runtime.AccessMode = accessModeForceReadOnly
	doc, docErr := buildAccessDocument(env.app.config)
	if docErr != nil {
		t.Fatal(docErr)
	}
	env.app.accessDoc = doc
	_, err = env.app.authorize(env.request("rid"), accessOperation{Use: env.ask(actionObjectWrite, "a/x")})
	if apiErr := atAsAPIError(t, err); apiErr.Status != http.StatusForbidden || apiErr.Action != actionObjectWrite {
		t.Fatalf("error = %+v", apiErr)
	}
	// An unpublished Other refuses the operation too.
	_, err = env.app.authorize(env.request("rid"), accessOperation{
		Others: []accessAsk{env.ask(actionObjectDelete, "a/x")},
		Use:    env.ask(actionObjectRead, "a/x"),
	})
	if apiErr := atAsAPIError(t, err); apiErr.Status != http.StatusForbidden || apiErr.Action != actionObjectDelete {
		t.Fatalf("error = %+v", apiErr)
	}
	if count := len(env.fake.Questions()); count != 0 {
		t.Fatalf("%d questions, want 0", count)
	}
}

func TestObjectAndBucketAsks(t *testing.T) {
	env := newAccessTestEnv(t)
	instance := env.app.instances["rw"]
	ask := env.app.objectAsk(actionObjectRead, instance, "folder/nested/deep.txt")
	if ask.Action != actionObjectRead || ask.Resource.path() != atSubfolder {
		t.Fatalf("ask = %s", ask.Resource.path())
	}
	if ask := env.app.bucketAsk(actionBucketAdmin, instance); ask.Action != actionBucketAdmin || ask.Resource.path() != atBucket {
		t.Fatalf("ask = %s", ask.Resource.path())
	}
}

func TestNewApplicationBuildsTheAccessParts(t *testing.T) {
	cfg := appConfig{JobHistoryLimit: 10, Runtime: defaultRuntimePolicy(), Access: accessConfig{Mode: accessModeOpen}}
	app, err := newApplication(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if app.identity != nil || app.pdp != nil || len(app.accessDoc.json()) != 0 {
		t.Fatalf("open mode: identity %v, pdp %v, document %d bytes", app.identity, app.pdp, len(app.accessDoc.json()))
	}
	app.close()

	fake, pdpConfig := newFakePDP(t)
	keys := newIdtKeys(t)
	access := idtConfig(keys.edKey("ed1"))
	access.Header = defaultIdentityHeader
	access.PDPURL, access.PDPPlainHTTP, access.PDPToken = pdpConfig.PDPURL, pdpConfig.PDPPlainHTTP, pdpConfig.PDPToken
	access.PDPTimeout, access.PDPUsageTimeout, access.PDPMaxEvaluations = pdpConfig.PDPTimeout, pdpConfig.PDPUsageTimeout, pdpConfig.PDPMaxEvaluations
	cfg.Access = access
	cfg.Buckets = nil
	app, err = newApplication(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer app.close()
	if app.identity == nil || app.pdp == nil || len(app.accessDoc.json()) == 0 {
		t.Fatalf("authzen mode: identity %v, pdp %v, document %d bytes", app.identity, app.pdp, len(app.accessDoc.json()))
	}
	if fake.Requests() != 0 {
		t.Fatal("the start asked the PDP")
	}
}
