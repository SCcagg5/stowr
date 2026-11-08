package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The tests of the decisions of the mutation routes, of the jobs, of the uploads and of the SQLite sessions. They use
// newAccessTestEnv and the fake PDP of the access tests. The backends are the memory backends of testApplication. A
// test that needs a feature of a provider (tags, lifecycle, multiparts, Object Lock) puts the instance "rw" of another
// test application in its place.

// amAsk is one expected question: the action and the path of the resource.
type amAsk struct {
	Action string
	Path   string
}

// amFolder gives the path of a folder of the bucket "rw".
func amFolder(name string) string { return atBucket + "/folder:" + name }

// amRoute is one mutation route. others are the questions without usage, in order. use is the question with usage.
type amRoute struct {
	name    string
	method  string
	target  string
	body    string
	gateway bool
	others  []amAsk
	use     amAsk
	// strict is true when a PDP that says true must give a 2xx answer. A route that is not strict needs a feature that
	// the memory backend has not: the answer must only be no access error.
	strict bool
	// prepare changes the environment before the request. It can ask the PDP: the test resets the fake after it.
	prepare func(t *testing.T, env *accessTestEnv)
	// request gives the target and the body when they depend on the environment.
	request func(t *testing.T, env *accessTestEnv) (target, body string)
}

func amMemory(env *accessTestEnv) *memoryBackend {
	return env.app.instances["rw"].backend.(*memoryBackend)
}

func amUse(action, path string) amAsk { return amAsk{Action: action, Path: path} }

func amMarshal(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func amRoutes() []amRoute {
	other := amFolder("other")
	return []amRoute{
		{name: "s3 put", method: http.MethodPut, target: "/s3?instance=rw&key=other%2Fnew.txt", body: "hello", gateway: true, strict: true,
			use: amUse(actionObjectWrite, other)},
		{name: "s3 put of a folder", method: http.MethodPut, target: "/s3?instance=rw&key=other%2Fsub%2F", gateway: true, strict: true,
			use: amUse(actionObjectWrite, other+"/subfolder:sub")},
		{name: "s3 delete", method: http.MethodDelete, target: "/s3?instance=rw&key=folder%2Fdirect.txt", gateway: true, strict: true,
			use: amUse(actionObjectDelete, atFolder)},
		{name: "s3 delete of a version", method: http.MethodDelete, target: "/s3?instance=rw&key=folder%2Fdirect.txt&version=v1", gateway: true,
			use: amUse(actionObjectDelete, atFolder)},
		{name: "uploads create", method: http.MethodPost, target: "/api/uploads", body: `{"instance":"rw","key":"other/up.bin","size":3}`, strict: true,
			use: amUse(actionObjectWrite, other)},
		{name: "copy", method: http.MethodPost, target: "/api/copy", body: `{"instance":"rw","src":"folder/direct.txt","dst":"other/copy.txt"}`, strict: true,
			others: []amAsk{amUse(actionObjectRead, atFolder)}, use: amUse(actionObjectWrite, other)},
		{name: "copy of a prefix", method: http.MethodPost, target: "/api/copy", body: `{"instance":"rw","src":"folder/nested/","dst":"copied/","isPrefix":true}`, strict: true,
			others: []amAsk{amUse(actionObjectRead, atSubfolder)}, use: amUse(actionObjectWrite, amFolder("copied"))},
		{name: "rename", method: http.MethodPost, target: "/api/rename", body: `{"instance":"rw","src":"folder/direct.txt","dst":"other/moved.txt"}`, strict: true,
			others: []amAsk{amUse(actionObjectRead, atFolder), amUse(actionObjectWrite, other)}, use: amUse(actionObjectDelete, atFolder)},
		{name: "rename of a prefix", method: http.MethodPost, target: "/api/rename", body: `{"instance":"rw","src":"folder/nested/","dst":"moved/","isPrefix":true}`, strict: true,
			others: []amAsk{amUse(actionObjectRead, atSubfolder), amUse(actionObjectWrite, amFolder("moved"))}, use: amUse(actionObjectDelete, atSubfolder)},
		{name: "delete prefix", method: http.MethodPost, target: "/api/delete-prefix", body: `{"instance":"rw","prefix":"folder/nested/"}`, strict: true,
			others: []amAsk{amUse(actionObjectRead, atSubfolder)}, use: amUse(actionObjectDelete, atSubfolder)},
		{name: "versions delete", method: http.MethodDelete, target: "/api/versions?instance=rw&key=folder%2Fdirect.txt&version=v1",
			use: amUse(actionObjectDelete, atFolder)},
		{name: "versions restore", method: http.MethodPost, target: "/api/versions/restore", body: `{"instance":"rw","key":"folder/direct.txt","version":"v1"}`,
			others: []amAsk{amUse(actionObjectRead, atFolder)}, use: amUse(actionObjectWrite, atFolder)},
		{name: "tags put", method: http.MethodPut, target: "/api/tags", body: `{"instance":"rw","key":"folder/direct.txt","tags":[{"key":"a","value":"1"}]}`, strict: true,
			use: amUse(actionObjectWrite, atFolder), prepare: amTagging},
		{name: "tags bulk", method: http.MethodPost, target: "/api/tags/bulk", body: `{"instance":"rw","prefix":"folder/","add":[{"key":"a","value":"1"}]}`, strict: true,
			others: []amAsk{amUse(actionObjectRead, atFolder)}, use: amUse(actionObjectWrite, atFolder), prepare: amTagging},
		{name: "metadata put", method: http.MethodPut, target: "/api/metadata", body: `{"instance":"rw","key":"folder/direct.txt","etag":"abc"}`,
			use: amUse(actionObjectWrite, atFolder)},
		{name: "object lock put", method: http.MethodPut, strict: true, use: amUse(actionObjectLock, atFolder), request: amLockRequest},
		{name: "lifecycle put", method: http.MethodPut, strict: true, prepare: amLifecycle, request: amLifecycleRequest,
			others: []amAsk{amUse(actionObjectRead, atBucket), amUse(actionObjectDelete, atBucket)}, use: amUse(actionBucketAdmin, atBucket)},
		{name: "multiparts abort of a list", method: http.MethodPost, strict: true, prepare: amMultiparts, request: amAbortRequest,
			others: []amAsk{amUse(actionObjectWrite, other)}, use: amUse(actionObjectWrite, atFolder)},
		{name: "multiparts abort of the old uploads", method: http.MethodPost, target: "/api/multiparts/abort", strict: true, prepare: amMultiparts,
			body:   `{"instance":"rw","olderThanDays":7,"prefix":"folder/"}`,
			others: []amAsk{amUse(actionObjectRead, atFolder)}, use: amUse(actionObjectWrite, atFolder)},
		{name: "archive extract", method: http.MethodPost, target: "/api/archive-extract", strict: true,
			body:   `{"instance":"rw","key":"folder/a.zip","target":"extracted/","entries":["x.txt"]}`,
			others: []amAsk{amUse(actionObjectRead, atFolder), amUse(actionObjectRead, amFolder("extracted"))}, use: amUse(actionObjectWrite, amFolder("extracted"))},
	}
}

func amTagging(t *testing.T, env *accessTestEnv) {
	env.app.instances["rw"].backend = newTaggingMemory(amMemory(env))
}

func amLifecycle(t *testing.T, env *accessTestEnv) {
	other, _ := lifecycleApplication(t, "MinIO", `<LifecycleConfiguration>`+editableLogs+`</LifecycleConfiguration>`)
	env.app.instances["rw"] = other.instances["rw"]
}

func amLifecycleRequest(t *testing.T, env *accessTestEnv) (string, string) {
	env.fake.SetAnswer(atAnswerAll(true))
	recorder := env.doToken(http.MethodGet, "/api/lifecycle?instance=rw", "")
	var state lifecycleResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &state); err != nil || state.Editor == nil {
		t.Fatalf("lifecycle read: status %d, body %q, error %v", recorder.Code, recorder.Body.String(), err)
	}
	rules := []lifecycleEditRule{}
	for _, entry := range state.Editor.Entries {
		if entry.Rule != nil {
			rules = append(rules, *entry.Rule)
		}
	}
	return "/api/lifecycle", amMarshal(t, lifecycleRequest{Instance: "rw", Hash: state.Editor.Hash, Rules: rules})
}

func amMultiparts(t *testing.T, env *accessTestEnv) {
	app, _ := fakeMultipartApplication(t, "aws",
		fakeMultipartUpload{key: "tenant/folder/a.bin", id: "u1", initiated: time.Now().Add(-48 * time.Hour)},
		fakeMultipartUpload{key: "tenant/other/b.bin", id: "u2", initiated: time.Now().Add(-48 * time.Hour)})
	env.app.instances["rw"] = app.instances["rw"]
}

func amAbortRequest(t *testing.T, env *accessTestEnv) (string, string) {
	return "/api/multiparts/abort", amMarshal(t, multipartAbortRequest{Instance: "rw", Uploads: []multipartAbortItem{
		{Key: "folder/a.bin", UploadID: "u1"}, {Key: "other/b.bin", UploadID: "u2"}}})
}

func amLockRequest(t *testing.T, env *accessTestEnv) (string, string) {
	app, fake := fakeLockApplication(t)
	env.app.instances["rw"] = app.instances["rw"]
	version := fake.put("tenant/folder/direct.txt", "data")
	on := true
	return "/api/object-lock", amMarshal(t, objectLockRequest{Instance: "rw", Key: "folder/direct.txt", Version: version, LegalHold: &on})
}

// amSend prepares the environment of a route, resets the fake PDP, and sends the request.
func amSend(t *testing.T, env *accessTestEnv, route amRoute, answer func(fakeQuestion) map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return amSendWith(t, env, route, func(fake *fakePDP) { fake.SetAnswer(answer) })
}

// amSendWith is amSend with a function that configures the fake PDP after its reset.
func amSendWith(t *testing.T, env *accessTestEnv, route amRoute, configure func(*fakePDP)) *httptest.ResponseRecorder {
	t.Helper()
	target, body := route.target, route.body
	if route.prepare != nil {
		route.prepare(t, env)
	}
	if route.request != nil {
		target, body = route.request(t, env)
	}
	env.fake.Reset()
	configure(env.fake)
	return env.doToken(route.method, target, body)
}

// amAccessErrorCode gives the code of an access error of a JSON answer, and "" for any other answer.
func amAccessErrorCode(recorder *httptest.ResponseRecorder, gateway bool) string {
	if gateway {
		text := recorder.Body.String()
		if strings.Contains(text, "The action ") || strings.Contains(text, "The budget of the action") ||
			strings.Contains(text, "The decision for the action") || strings.Contains(text, "A valid identity token") {
			return "access"
		}
		return ""
	}
	var body atErrorBody
	if json.Unmarshal(recorder.Body.Bytes(), &body) != nil {
		return ""
	}
	switch body.Error.Code {
	case codeForbidden, codeTooManyRequests, codePDPUnavailable, codeUnauthenticated:
		return body.Error.Code
	}
	return ""
}

// amWantQuestions checks the exact questions of a route: the Others without usage, in order, then one usage question.
func amWantQuestions(t *testing.T, got []fakeQuestion, route amRoute) {
	t.Helper()
	want := append(append([]amAsk(nil), route.others...), route.use)
	if len(got) != len(want) {
		t.Fatalf("%d questions %+v, want %d %+v", len(got), atSummarize(got), len(want), want)
	}
	for index, question := range got {
		isUse := index == len(route.others)
		batch := !isUse && len(route.others) > 1
		if question.Action != want[index].Action || question.Path != want[index].Path || question.Match != "node" ||
			question.Subject != atSubject || question.Usage != isUse || question.Batch != batch {
			t.Fatalf("question %d = %+v, want %+v with usage %v and batch %v", index, question, want[index], isUse, batch)
		}
	}
	// The question without usage for the action of the use never comes (ProcurAuth 10.1).
	for _, question := range got {
		if question.Action == route.use.Action && question.Path == route.use.Path && !question.Usage {
			t.Fatalf("a question without usage for the use: %+v", question)
		}
	}
}

func TestMutationRoutesAskTheirPermissions(t *testing.T) {
	for _, route := range amRoutes() {
		route := route
		t.Run(route.name, func(t *testing.T) {
			t.Run("true", func(t *testing.T) {
				env := newAccessTestEnv(t)
				recorder := amSend(t, env, route, atUsageAnswer(true))
				if route.strict && (recorder.Code < 200 || recorder.Code > 299) {
					t.Fatalf("status %d, body %q", recorder.Code, recorder.Body.String())
				}
				if code := amAccessErrorCode(recorder, route.gateway); code != "" {
					t.Fatalf("access error %q: status %d, body %q", code, recorder.Code, recorder.Body.String())
				}
				amWantQuestions(t, env.fake.Questions(), route)
				// The Others go in one request, and the use goes in its own request.
				if count, want := env.fake.Requests(), 1+boolInt(len(route.others) > 0); count != want {
					t.Fatalf("%d PDP requests, want %d", count, want)
				}
			})

			for index, refused := range route.others {
				index, refused := index, refused
				t.Run(fmt.Sprintf("other %d refused", index), func(t *testing.T) {
					env := newAccessTestEnv(t)
					recorder := amSend(t, env, route, func(q fakeQuestion) map[string]any {
						if q.Action == refused.Action && q.Path == refused.Path && !q.Usage {
							return map[string]any{"decision": false}
						}
						return atUsageAnswer(true)(q)
					})
					amWantRefusal(t, env, recorder, route, refused.Action, true)
				})
			}

			t.Run("false", func(t *testing.T) {
				env := newAccessTestEnv(t)
				recorder := amSend(t, env, route, atAnswerAll(false))
				action := route.use.Action
				if len(route.others) > 0 {
					action = route.others[0].Action
				}
				amWantRefusal(t, env, recorder, route, action, len(route.others) > 0)
			})

			t.Run("budget", func(t *testing.T) {
				env := newAccessTestEnv(t)
				recorder := amSend(t, env, route, func(q fakeQuestion) map[string]any {
					if q.Usage {
						return map[string]any{"decision": false, "context": map[string]any{"reason": "budget_exceeded", "retry_after": 7}}
					}
					return map[string]any{"decision": true}
				})
				if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") != "7" {
					t.Fatalf("status %d, Retry-After %q, body %q", recorder.Code, recorder.Header().Get("Retry-After"), recorder.Body.String())
				}
				if !route.gateway {
					body := atDecodeError(t, recorder)
					if body.Error.Code != "too_many_requests" || body.Error.Action != route.use.Action {
						t.Fatalf("error %+v", body.Error)
					}
				}
			})

			t.Run("failure", func(t *testing.T) {
				env := newAccessTestEnv(t)
				failed := amSendWith(t, env, route, func(fake *fakePDP) { fake.SetStatus(http.StatusInternalServerError) })
				if failed.Code != http.StatusServiceUnavailable {
					t.Fatalf("status %d, body %q", failed.Code, failed.Body.String())
				}
				if !route.gateway {
					parsed := atDecodeError(t, failed)
					if parsed.Error.Code != "pdp_unavailable" || !parsed.Error.Retryable {
						t.Fatalf("error %+v", parsed.Error)
					}
				}
				questions := env.fake.Questions()
				// A failure of the Others stops the operation before the usage question. Without Others, the usage
				// question is the one that fails, and nothing is sent again.
				wantCount := len(route.others)
				if wantCount == 0 {
					wantCount = 1
				}
				if len(questions) != wantCount {
					t.Fatalf("%d questions %+v, want %d", len(questions), atSummarize(questions), wantCount)
				}
				if len(route.others) > 0 {
					for _, question := range questions {
						if question.Usage {
							t.Fatalf("a usage question after a failure: %+v", question)
						}
					}
				}
			})
		})
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// amWantRefusal checks a 403 with the action, and that no usage question came.
// A refused Other stops the operation before the usage question: noUsage is true for such a refusal.
func amWantRefusal(t *testing.T, env *accessTestEnv, recorder *httptest.ResponseRecorder, route amRoute, action string, noUsage bool) {
	t.Helper()
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status %d, body %q", recorder.Code, recorder.Body.String())
	}
	if route.gateway {
		if want := "The action " + action + " is not allowed."; !strings.Contains(recorder.Body.String(), want) {
			t.Fatalf("body %q, want %q", recorder.Body.String(), want)
		}
	} else {
		body := atDecodeError(t, recorder)
		if body.Error.Code != "forbidden" || body.Error.Action != action {
			t.Fatalf("error %+v, want action %s", body.Error, action)
		}
	}
	for _, question := range env.fake.Questions() {
		if noUsage && question.Usage {
			t.Fatalf("a usage question after a refusal: %+v", question)
		}
	}
	if len(env.fake.Questions()) == 0 {
		t.Fatal("the PDP got no question")
	}
}

// ---- PUT and DELETE of the gateway: effects ----

type amCountingBody struct {
	reads atomic.Int64
}

func (b *amCountingBody) Read(buffer []byte) (int, error) {
	b.reads.Add(1)
	return 0, io.EOF
}

func TestGatewayPutRefusedReadsNoBodyAndWritesNothing(t *testing.T) {
	env := newAccessTestEnv(t)
	env.fake.SetAnswer(atAnswerAll(false))
	body := &amCountingBody{}
	request := httptest.NewRequest(http.MethodPut, "/s3?instance=rw&key=other%2Fnew.txt", body)
	request.ContentLength = 5
	request.Header.Set(defaultIdentityHeader, env.token())
	recorder := httptest.NewRecorder()
	env.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status %d, body %q", recorder.Code, recorder.Body.String())
	}
	if reads := body.reads.Load(); reads != 0 {
		t.Fatalf("the handler read the body %d times", reads)
	}
	if _, found := amMemory(env).objects["tenant/other/new.txt"]; found {
		t.Fatal("the object exists after a refusal")
	}

	// The same request with a PDP that says true writes the object.
	env.fake.SetAnswer(atUsageAnswer(true))
	if recorder := env.doToken(http.MethodPut, "/s3?instance=rw&key=other%2Fnew.txt", "hello"); recorder.Code != http.StatusNoContent {
		t.Fatalf("status %d, body %q", recorder.Code, recorder.Body.String())
	}
	if string(amMemory(env).objects["tenant/other/new.txt"].data) != "hello" {
		t.Fatal("the object is missing after an allowed write")
	}
}

func TestGatewayDeleteRefusedKeepsTheObject(t *testing.T) {
	env := newAccessTestEnv(t)
	env.fake.SetAnswer(atAnswerAll(false))
	if recorder := env.doToken(http.MethodDelete, "/s3?instance=rw&key=folder%2Fdirect.txt", ""); recorder.Code != http.StatusForbidden {
		t.Fatalf("status %d", recorder.Code)
	}
	if _, found := amMemory(env).objects["tenant/folder/direct.txt"]; !found {
		t.Fatal("a refused delete removed the object")
	}
	env.fake.SetAnswer(atUsageAnswer(true))
	if recorder := env.doToken(http.MethodDelete, "/s3?instance=rw&key=folder%2Fdirect.txt", ""); recorder.Code != http.StatusNoContent {
		t.Fatalf("status %d", recorder.Code)
	}
	if _, found := amMemory(env).objects["tenant/folder/direct.txt"]; found {
		t.Fatal("an allowed delete kept the object")
	}
}

// The process ceiling comes before the PDP for a write.
func TestProcessCeilingOfAWriteComesBeforeThePDP(t *testing.T) {
	env := newAccessTestEnv(t)
	recorder := env.doToken(http.MethodPut, "/s3?instance=readonly&key=x.txt", "data")
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status %d", recorder.Code)
	}
	if count := env.fake.Requests(); count != 0 {
		t.Fatalf("the PDP got %d requests", count)
	}
	for _, target := range []string{"/s3?instance=nope&key=x.txt", "/api/uploads"} {
		env.fake.Reset()
		body := ""
		method := http.MethodPut
		if target == "/api/uploads" {
			method, body = http.MethodPost, `{"instance":"nope","key":"x","size":1}`
		}
		if recorder := env.doToken(method, target, body); recorder.Code != http.StatusNotFound {
			t.Fatalf("%s: status %d, body %q", target, recorder.Code, recorder.Body.String())
		}
		if count := env.fake.Requests(); count != 0 {
			t.Fatalf("%s: the PDP got %d requests for an unknown instance", target, count)
		}
	}
}

// ---- continuation ----

func TestAuthorizeContinuationAsksTheUseWithoutUsage(t *testing.T) {
	env := newAccessTestEnv(t)
	if !env.app.accessDoc.listsBudget(actionObjectWrite) {
		t.Fatal("object.write does not list the budget")
	}
	env.fake.SetAnswer(atUsageAnswer(true))
	op := accessOperation{
		Others:       []accessAsk{env.ask(actionObjectRead, "a/x")},
		Use:          env.ask(actionObjectWrite, "b/y"),
		Continuation: true,
	}
	if _, err := env.app.authorize(env.request("rid-c1"), op); err != nil {
		t.Fatal(err)
	}
	want := []atQuestion{
		{actionObjectRead, atBucket + "/folder:a", false, true},
		{actionObjectWrite, atBucket + "/folder:b", false, true},
	}
	if got := atSummarize(env.fake.Questions()); !atSameSummary(got, want) {
		t.Fatalf("questions %+v, want %+v", got, want)
	}
	if count := env.fake.Requests(); count != 1 {
		t.Fatalf("%d HTTP requests, want 1", count)
	}

	// Without Others: one question, not a batch, no usage.
	env.fake.Reset()
	op.Others = nil
	if _, err := env.app.authorize(env.request("rid-c2"), op); err != nil {
		t.Fatal(err)
	}
	want = []atQuestion{{actionObjectWrite, atBucket + "/folder:b", false, false}}
	if got := atSummarize(env.fake.Questions()); !atSameSummary(got, want) {
		t.Fatalf("questions %+v, want %+v", got, want)
	}

	// A use that is not a continuation still asks usage.
	env.fake.Reset()
	op.Continuation = false
	if _, err := env.app.authorize(env.request("rid-c3"), op); err != nil {
		t.Fatal(err)
	}
	want = []atQuestion{{actionObjectWrite, atBucket + "/folder:b", true, false}}
	if got := atSummarize(env.fake.Questions()); !atSameSummary(got, want) {
		t.Fatalf("questions %+v, want %+v", got, want)
	}
}

// ---- uploads ----

type amUpload struct {
	token string
}

// amCreateUpload creates an upload of 3 bytes with a PDP that says true.
func amCreateUpload(t *testing.T, env *accessTestEnv, key string) amUpload {
	t.Helper()
	env.fake.SetAnswer(atUsageAnswer(true))
	recorder := env.doToken(http.MethodPost, "/api/uploads", fmt.Sprintf(`{"instance":"rw","key":%q,"size":3}`, key))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create: status %d, body %q", recorder.Code, recorder.Body.String())
	}
	var created publicUpload
	if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil || created.ResumeToken == "" {
		t.Fatalf("create: body %q, error %v", recorder.Body.String(), err)
	}
	return amUpload{token: created.ResumeToken}
}

func (e *accessTestEnv) uploadRequest(method, target, body, token string) *httptest.ResponseRecorder {
	header := http.Header{defaultIdentityHeader: {e.token()}, "X-Stowr-Resume-Token": {token}}
	if method == http.MethodPut {
		header.Set("Content-Range", "bytes 0-2/3")
	}
	return e.do(method, target, body, header)
}

func TestUploadCreateAsksOneUsageQuestionAndChunksAskNone(t *testing.T) {
	env := newAccessTestEnv(t)
	env.fake.SetAnswer(atUsageAnswer(true))
	recorder := env.doToken(http.MethodPost, "/api/uploads", `{"instance":"rw","key":"other/up.bin","size":3}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status %d, body %q", recorder.Code, recorder.Body.String())
	}
	want := []atQuestion{{actionObjectWrite, amFolder("other"), true, false}}
	if got := atSummarize(env.fake.Questions()); !atSameSummary(got, want) {
		t.Fatalf("create questions %+v, want %+v", got, want)
	}
	var created publicUpload
	if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	// A chunk, a status, a resume and a cancel ask object.write on the key of the token, without usage.
	wantContinuation := []atQuestion{{actionObjectWrite, amFolder("other"), false, false}}
	for _, request := range []struct {
		name, method, target, body string
		status                     int
	}{
		{"status", http.MethodGet, "/api/uploads", "", http.StatusOK},
		{"resume", http.MethodPost, "/api/uploads/resume", fmt.Sprintf(`{"resumeToken":%q}`, created.ResumeToken), http.StatusOK},
		{"chunk", http.MethodPut, "/api/uploads", "abc", http.StatusOK},
	} {
		env.fake.Reset()
		var recorder *httptest.ResponseRecorder
		if request.method == http.MethodPost {
			recorder = env.doToken(request.method, request.target, request.body)
		} else {
			recorder = env.uploadRequest(request.method, request.target, request.body, created.ResumeToken)
		}
		if recorder.Code != request.status {
			t.Fatalf("%s: status %d, body %q", request.name, recorder.Code, recorder.Body.String())
		}
		if got := atSummarize(env.fake.Questions()); !atSameSummary(got, wantContinuation) {
			t.Fatalf("%s: questions %+v, want %+v", request.name, got, wantContinuation)
		}
	}
	if string(amMemory(env).objects["tenant/other/up.bin"].data) != "abc" {
		t.Fatal("the chunk did not complete the upload")
	}

	// A cancel of another upload.
	second := amCreateUpload(t, env, "other/second.bin")
	env.fake.Reset()
	if recorder := env.uploadRequest(http.MethodDelete, "/api/uploads", "", second.token); recorder.Code != http.StatusOK {
		t.Fatalf("cancel: status %d, body %q", recorder.Code, recorder.Body.String())
	}
	if got := atSummarize(env.fake.Questions()); !atSameSummary(got, wantContinuation) {
		t.Fatalf("cancel: questions %+v, want %+v", got, wantContinuation)
	}
}

func TestUploadContinuationKeepsTheStatusOfARefusal(t *testing.T) {
	env := newAccessTestEnv(t)
	upload := amCreateUpload(t, env, "other/up.bin")
	for _, test := range []struct {
		name   string
		answer func(fakeQuestion) map[string]any
		status int
		code   string
	}{
		{"refusal", atAnswerAll(false), http.StatusForbidden, "forbidden"},
		{"budget", func(fakeQuestion) map[string]any {
			return map[string]any{"decision": false, "context": map[string]any{"reason": "budget_exceeded", "retry_after": 7}}
		}, http.StatusTooManyRequests, "too_many_requests"},
	} {
		env.fake.Reset()
		env.fake.SetAnswer(test.answer)
		for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
			recorder := env.uploadRequest(method, "/api/uploads", "abc", upload.token)
			if recorder.Code != test.status {
				t.Fatalf("%s %s: status %d, body %q", test.name, method, recorder.Code, recorder.Body.String())
			}
			if body := atDecodeError(t, recorder); body.Error.Code != test.code || body.Error.Action != actionObjectWrite {
				t.Fatalf("%s %s: error %+v", test.name, method, body.Error)
			}
		}
		recorder := env.doToken(http.MethodPost, "/api/uploads/resume", fmt.Sprintf(`{"resumeToken":%q}`, upload.token))
		if recorder.Code != test.status {
			t.Fatalf("%s resume: status %d", test.name, recorder.Code)
		}
		for _, question := range env.fake.Questions() {
			if question.Usage {
				t.Fatalf("%s: a usage question %+v", test.name, question)
			}
		}
	}

	// A failing PDP gives 503.
	env.fake.Reset()
	env.fake.SetStatus(http.StatusInternalServerError)
	if recorder := env.uploadRequest(http.MethodPut, "/api/uploads", "abc", upload.token); recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("failure: status %d", recorder.Code)
	}

	// The refusals did not change the upload: the PDP that says true lets the chunk through.
	env.fake.Reset()
	env.fake.SetAnswer(atUsageAnswer(true))
	if recorder := env.uploadRequest(http.MethodPut, "/api/uploads", "abc", upload.token); recorder.Code != http.StatusOK {
		t.Fatalf("chunk: status %d, body %q", recorder.Code, recorder.Body.String())
	}
}

func TestUploadChunkAfterTheWriteIsRefused(t *testing.T) {
	env := newAccessTestEnv(t)
	upload := amCreateUpload(t, env, "other/up.bin")
	env.fake.Reset()
	env.fake.SetAnswer(atAnswerAll(false))
	recorder := env.uploadRequest(http.MethodPut, "/api/uploads", "abc", upload.token)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status %d, body %q", recorder.Code, recorder.Body.String())
	}
	if _, found := amMemory(env).objects["tenant/other/up.bin"]; found {
		t.Fatal("a refused chunk wrote the object")
	}
}

// ---- jobs ----

// amStartCopyJob starts a copy job of a prefix with a PDP that says true and gives its id.
func amStartCopyJob(t *testing.T, env *accessTestEnv) string {
	t.Helper()
	env.fake.SetAnswer(atUsageAnswer(true))
	recorder := env.doToken(http.MethodPost, "/api/copy", `{"instance":"rw","src":"folder/nested/","dst":"copied/","isPrefix":true}`)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("copy: status %d, body %q", recorder.Code, recorder.Body.String())
	}
	var job publicJob
	if err := json.Unmarshal(recorder.Body.Bytes(), &job); err != nil || job.ID == "" {
		t.Fatalf("copy: body %q, error %v", recorder.Body.String(), err)
	}
	return job.ID
}

func TestJobReadAndControlAskTheOperationWithoutUsage(t *testing.T) {
	env := newAccessTestEnv(t)
	id := amStartCopyJob(t, env)
	wantCreate := []atQuestion{
		{actionObjectRead, atSubfolder, false, false},
		{actionObjectWrite, amFolder("copied"), true, false},
	}
	if got := atSummarize(env.fake.Questions()); !atSameSummary(got, wantCreate) {
		t.Fatalf("create questions %+v, want %+v", got, wantCreate)
	}

	// A read of the job asks the same operation again, without usage, in one batch.
	wantRead := []atQuestion{
		{actionObjectRead, atSubfolder, false, true},
		{actionObjectWrite, amFolder("copied"), false, true},
	}
	env.fake.Reset()
	if recorder := env.doToken(http.MethodGet, "/api/jobs/"+id, ""); recorder.Code != http.StatusOK {
		t.Fatalf("read: status %d, body %q", recorder.Code, recorder.Body.String())
	}
	if got := atSummarize(env.fake.Questions()); !atSameSummary(got, wantRead) {
		t.Fatalf("read questions %+v, want %+v", got, wantRead)
	}
	if count := env.fake.Requests(); count != 1 {
		t.Fatalf("%d HTTP requests, want 1", count)
	}

	// A control asks the same questions.
	env.fake.Reset()
	if recorder := env.doToken(http.MethodPost, "/api/jobs/"+id+"/cancel", ""); recorder.Code != http.StatusOK {
		t.Fatalf("cancel: status %d, body %q", recorder.Code, recorder.Body.String())
	}
	if got := atSummarize(env.fake.Questions()); !atSameSummary(got, wantRead) {
		t.Fatalf("cancel questions %+v, want %+v", got, wantRead)
	}
}

func TestJobRefusedAnswersLikeAnUnknownJob(t *testing.T) {
	env := newAccessTestEnv(t)
	id := amStartCopyJob(t, env)

	unknown := env.doToken(http.MethodGet, "/api/jobs/job_nothing", "")
	if unknown.Code != http.StatusNotFound || atDecodeError(t, unknown).Error.Code != "unknown_job" {
		t.Fatalf("unknown job: status %d, body %q", unknown.Code, unknown.Body.String())
	}

	env.fake.Reset()
	env.fake.SetAnswer(atAnswerAll(false))
	for _, request := range []struct{ method, target string }{
		{http.MethodGet, "/api/jobs/" + id},
		{http.MethodPost, "/api/jobs/" + id + "/pause"},
		{http.MethodPost, "/api/jobs/" + id + "/resume"},
		{http.MethodPost, "/api/jobs/" + id + "/cancel"},
	} {
		recorder := env.doToken(request.method, request.target, "")
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("%s %s: status %d, body %q", request.method, request.target, recorder.Code, recorder.Body.String())
		}
		got, want := atDecodeError(t, recorder), atDecodeError(t, unknown)
		if got.Error.Code != "unknown_job" || got.Error.Message != want.Error.Message || got.Error.Action != "" {
			t.Fatalf("%s %s: error %+v, want the answer of an unknown job %+v", request.method, request.target, got.Error, want.Error)
		}
	}
	for _, question := range env.fake.Questions() {
		if question.Usage {
			t.Fatalf("a usage question for a job: %+v", question)
		}
	}

	// An unknown job asks no question.
	env.fake.Reset()
	env.doToken(http.MethodGet, "/api/jobs/job_nothing", "")
	if count := len(env.fake.Questions()); count != 0 {
		t.Fatalf("an unknown job gave %d questions", count)
	}

	// A failure of the PDP stays 503. A budget answer stays 429.
	env.fake.SetStatus(http.StatusInternalServerError)
	if recorder := env.doToken(http.MethodGet, "/api/jobs/"+id, ""); recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("failure: status %d", recorder.Code)
	}
	env.fake.Reset()
	env.fake.SetAnswer(func(fakeQuestion) map[string]any {
		return map[string]any{"decision": false, "context": map[string]any{"reason": "budget_exceeded", "retry_after": 7}}
	})
	if recorder := env.doToken(http.MethodGet, "/api/jobs/"+id, ""); recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") != "7" {
		t.Fatalf("budget: status %d, Retry-After %q", recorder.Code, recorder.Header().Get("Retry-After"))
	}
}

func TestJobOfAnotherSubjectIsRefused(t *testing.T) {
	env := newAccessTestEnv(t)
	id := amStartCopyJob(t, env)
	// The PDP knows the subject of the job and refuses each other subject.
	env.fake.SetAnswer(func(q fakeQuestion) map[string]any { return map[string]any{"decision": q.Subject == atSubject} })
	other := env.do(http.MethodGet, "/api/jobs/"+id, "", http.Header{defaultIdentityHeader: {env.tokenFor("user-2")}})
	if other.Code != http.StatusNotFound || atDecodeError(t, other).Error.Code != "unknown_job" {
		t.Fatalf("other subject: status %d, body %q", other.Code, other.Body.String())
	}
	if recorder := env.doToken(http.MethodGet, "/api/jobs/"+id, ""); recorder.Code != http.StatusOK {
		t.Fatalf("owner: status %d, body %q", recorder.Code, recorder.Body.String())
	}
	cancel := env.do(http.MethodPost, "/api/jobs/"+id+"/cancel", "", http.Header{defaultIdentityHeader: {env.tokenFor("user-2")}})
	if cancel.Code != http.StatusNotFound {
		t.Fatalf("other subject cancel: status %d", cancel.Code)
	}
	if job, _ := env.app.jobs.get(id); job.Status == jobStatusCanceled {
		t.Fatal("a refused subject canceled the job")
	}
}

func TestJobWithoutARuleIsRefused(t *testing.T) {
	env := newAccessTestEnv(t)
	now := time.Now().UTC()
	for _, job := range []jobState{
		{ID: "job_mystery", Type: "mystery", Instance: "rw", Status: jobStatusCompleted, CreatedAt: now, UpdatedAt: now},
		{ID: "job_noinstance", Type: jobTypeStatsPrefix, Instance: "gone", Status: jobStatusCompleted, CreatedAt: now, UpdatedAt: now},
		{ID: "job_norequest", Type: jobTypeIntegrity, Instance: "rw", Status: jobStatusCompleted, CreatedAt: now, UpdatedAt: now},
	} {
		if err := env.app.jobs.put(job); err != nil {
			t.Fatal(err)
		}
		env.fake.Reset()
		env.fake.SetAnswer(atAnswerAll(true))
		recorder := env.doToken(http.MethodGet, "/api/jobs/"+job.ID, "")
		if recorder.Code != http.StatusNotFound || atDecodeError(t, recorder).Error.Code != "unknown_job" {
			t.Fatalf("%s: status %d, body %q", job.ID, recorder.Code, recorder.Body.String())
		}
		if count := len(env.fake.Questions()); count != 0 {
			t.Fatalf("%s: %d questions", job.ID, count)
		}
	}
}

func TestJobOperationOfEachJobType(t *testing.T) {
	env := newAccessTestEnv(t)
	app := env.app
	readonlyFolder := func(name string) string {
		return "service:" + atServiceID + "/bucket:readonly/folder:" + name
	}
	type want struct {
		others []amAsk
		use    amAsk
	}
	tests := []struct {
		name string
		job  jobState
		want want
	}{
		{"copy prefix", jobState{Type: jobTypeCopyPrefix, Instance: "rw", Source: "folder/nested/", Target: "copied/"},
			want{[]amAsk{{actionObjectRead, atSubfolder}}, amAsk{actionObjectWrite, amFolder("copied")}}},
		{"copy object", jobState{Type: jobTypeCopyObject, Instance: "rw", Source: "folder/a.bin", Target: "copied/a.bin"},
			want{[]amAsk{{actionObjectRead, atFolder}}, amAsk{actionObjectWrite, amFolder("copied")}}},
		{"move prefix", jobState{Type: jobTypeMovePrefix, Instance: "rw", Source: "folder/nested/", Target: "moved/"},
			want{[]amAsk{{actionObjectRead, atSubfolder}, {actionObjectWrite, amFolder("moved")}}, amAsk{actionObjectDelete, atSubfolder}}},
		{"move object", jobState{Type: jobTypeMoveObject, Instance: "rw", Source: "folder/a.bin", Target: "moved/a.bin"},
			want{[]amAsk{{actionObjectRead, atFolder}, {actionObjectWrite, amFolder("moved")}}, amAsk{actionObjectDelete, atFolder}}},
		{"delete prefix", jobState{Type: jobTypeDeletePrefix, Instance: "rw", Prefix: "folder/"},
			want{[]amAsk{{actionObjectRead, atFolder}}, amAsk{actionObjectDelete, atFolder}}},
		{"bulk tags", jobState{Type: jobTypeTagPrefix, Instance: "rw", Prefix: "folder/"},
			want{[]amAsk{{actionObjectRead, atFolder}}, amAsk{actionObjectWrite, atFolder}}},
		{"abort of incomplete uploads", jobState{Type: jobTypeAbortMultiparts, Instance: "rw", Prefix: "folder/"},
			want{[]amAsk{{actionObjectRead, atFolder}}, amAsk{actionObjectWrite, atFolder}}},
		{"stats of the root", jobState{Type: jobTypeStatsPrefix, Instance: "rw"},
			want{nil, amAsk{actionObjectRead, atBucket}}},
		{"integrity", jobState{Type: jobTypeIntegrity, Instance: "rw", IntegrityRequest: &integrityRequest{Key: "folder/direct.txt"}},
			want{nil, amAsk{actionObjectRead, atFolder}}},
		{"archive entry integrity", jobState{Type: jobTypeArchiveEntryIntegrity, Instance: "rw", ArchiveEntryIntegrityRequest: &archiveEntryIntegrityRequest{Key: "folder/a.zip", Entry: "x"}},
			want{nil, amAsk{actionObjectRead, atFolder}}},
		{"archive extraction to another instance", jobState{Type: jobTypeExtractArchive, Instance: "rw", TargetInstance: "readonly", Source: "folder/a.zip", Target: "out/"},
			want{[]amAsk{{actionObjectRead, atFolder}, {actionObjectRead, readonlyFolder("out")}}, amAsk{actionObjectWrite, readonlyFolder("out")}}},
	}
	for _, test := range tests {
		op, ok := app.jobOperation(test.job)
		if !ok {
			t.Fatalf("%s: no rule", test.name)
		}
		if len(op.Others) != len(test.want.others) {
			t.Fatalf("%s: others %+v, want %+v", test.name, op.Others, test.want.others)
		}
		for index, other := range op.Others {
			if other.Action != test.want.others[index].Action || other.Resource.path() != test.want.others[index].Path {
				t.Fatalf("%s: other %d = %s %s, want %+v", test.name, index, other.Action, other.Resource.path(), test.want.others[index])
			}
		}
		if op.Use.Action != test.want.use.Action || op.Use.Resource.path() != test.want.use.Path {
			t.Fatalf("%s: use = %s %s, want %+v", test.name, op.Use.Action, op.Use.Resource.path(), test.want.use)
		}
	}
	if _, ok := app.jobOperation(jobState{Type: "unknown", Instance: "rw"}); ok {
		t.Fatal("a job of an unknown type has a rule")
	}
}

// The Insights route can reuse a finished job of another request. It asks object.read for each new request.
func TestStatsAsksEachRequestEvenWhenItReusesAJob(t *testing.T) {
	env := newAccessTestEnv(t)
	env.fake.SetAnswer(atAnswerAll(true))
	for round := 0; round < 2; round++ {
		recorder := env.doToken(http.MethodGet, "/api/stats?instance=rw&prefix=folder%2F", "")
		if recorder.Code != http.StatusOK && recorder.Code != http.StatusAccepted {
			t.Fatalf("round %d: status %d, body %q", round, recorder.Code, recorder.Body.String())
		}
	}
	questions := env.fake.Questions()
	if len(questions) != 2 {
		t.Fatalf("%d questions, want 2", len(questions))
	}
	for _, question := range questions {
		if question.Action != actionObjectRead || question.Path != atFolder || question.Usage {
			t.Fatalf("question %+v", question)
		}
	}
}

// ---- SQLite sessions ----

func amSQLiteEnv(t *testing.T) *accessTestEnv {
	t.Helper()
	env := newAccessTestEnv(t)
	data, err := base64.StdEncoding.DecodeString(sqlitePreviewFixtureBase64)
	if err != nil {
		t.Fatal(err)
	}
	backend := amMemory(env)
	backend.mu.Lock()
	backend.objects["tenant/folder/fixture.sqlite"] = memoryObject{data: data, contentType: "application/octet-stream", modified: time.Now().UTC()}
	backend.mu.Unlock()
	return env
}

func amCreateSQLiteSession(t *testing.T, env *accessTestEnv) string {
	t.Helper()
	env.fake.SetAnswer(atAnswerAll(true))
	recorder := env.doToken(http.MethodPost, "/api/sqlite/sessions", `{"instance":"rw","key":"folder/fixture.sqlite"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create: status %d, body %q", recorder.Code, recorder.Body.String())
	}
	var session sqliteSessionResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &session); err != nil || session.ID == "" {
		t.Fatalf("create: body %q, error %v", recorder.Body.String(), err)
	}
	return session.ID
}

func TestSQLiteSessionAsksObjectReadOnTheKeyAtEachRequest(t *testing.T) {
	env := amSQLiteEnv(t)
	id := amCreateSQLiteSession(t, env)
	want := []atQuestion{{actionObjectRead, atFolder, false, false}}
	if got := atSummarize(env.fake.Questions()); !atSameSummary(got, want) {
		t.Fatalf("create questions %+v, want %+v", got, want)
	}
	for _, request := range []struct{ name, method, target string }{
		{"get", http.MethodGet, "/api/sqlite/sessions/" + id},
		{"table", http.MethodGet, "/api/sqlite/sessions/" + id + "/table?table=items"},
	} {
		env.fake.Reset()
		if recorder := env.doToken(request.method, request.target, ""); recorder.Code != http.StatusOK {
			t.Fatalf("%s: status %d, body %q", request.name, recorder.Code, recorder.Body.String())
		}
		if got := atSummarize(env.fake.Questions()); !atSameSummary(got, want) {
			t.Fatalf("%s: questions %+v, want %+v", request.name, got, want)
		}
	}
}

func TestSQLiteSessionOfARefusedKeyGives404(t *testing.T) {
	env := amSQLiteEnv(t)
	id := amCreateSQLiteSession(t, env)
	env.fake.Reset()
	env.fake.SetAnswer(atAnswerAll(false))
	for _, request := range []struct{ method, target string }{
		{http.MethodGet, "/api/sqlite/sessions/" + id},
		{http.MethodGet, "/api/sqlite/sessions/" + id + "/table?table=items"},
		{http.MethodDelete, "/api/sqlite/sessions/" + id},
	} {
		recorder := env.doToken(request.method, request.target, "")
		if recorder.Code != http.StatusNotFound || atDecodeError(t, recorder).Error.Code != "sqlite_session_not_found" {
			t.Fatalf("%s %s: status %d, body %q", request.method, request.target, recorder.Code, recorder.Body.String())
		}
	}
	// The refused delete kept the session.
	env.fake.SetAnswer(atAnswerAll(true))
	if recorder := env.doToken(http.MethodGet, "/api/sqlite/sessions/"+id, ""); recorder.Code != http.StatusOK {
		t.Fatalf("session after a refused delete: status %d", recorder.Code)
	}
	// A failure of the PDP stays 503.
	env.fake.SetStatus(http.StatusInternalServerError)
	if recorder := env.doToken(http.MethodGet, "/api/sqlite/sessions/"+id, ""); recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("failure: status %d", recorder.Code)
	}
	// An unknown session asks no question.
	env.fake.Reset()
	if recorder := env.doToken(http.MethodGet, "/api/sqlite/sessions/nothing", ""); recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown session: status %d", recorder.Code)
	}
	if count := len(env.fake.Questions()); count != 0 {
		t.Fatalf("an unknown session gave %d questions", count)
	}
}

func TestSQLiteSessionCreationOfARefusedKeyOpensNothing(t *testing.T) {
	env := amSQLiteEnv(t)
	env.fake.SetAnswer(atAnswerAll(false))
	recorder := env.doToken(http.MethodPost, "/api/sqlite/sessions", `{"instance":"rw","key":"folder/fixture.sqlite"}`)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status %d, body %q", recorder.Code, recorder.Body.String())
	}
	if body := atDecodeError(t, recorder); body.Error.Action != actionObjectRead {
		t.Fatalf("error %+v", body.Error)
	}
	if count := amMemory(env).getCount; count != 0 {
		t.Fatalf("the backend got %d reads", count)
	}
	env.fake.Reset()
	if recorder := env.doToken(http.MethodPost, "/api/sqlite/sessions", `{"instance":"nope","key":"x"}`); recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown instance: status %d", recorder.Code)
	}
	if count := len(env.fake.Questions()); count != 0 {
		t.Fatalf("an unknown instance gave %d questions", count)
	}
}

// ---- the archive extraction in another instance ----

func TestArchiveExtractResolvesBothInstancesBeforeTheDecision(t *testing.T) {
	env := newAccessTestEnv(t)
	cfg := env.app.instances["rw"].cfg
	cfg.ID, cfg.Name, cfg.Bucket = "rw2", "rw2", "rw2-bucket"
	env.app.instances["rw2"] = &storageInstance{cfg: cfg, backend: newMemoryBackend(nil), caps: initialCapabilities(cfg)}
	env.app.order = append(env.app.order, "rw2")

	env.fake.SetAnswer(atUsageAnswer(true))
	recorder := env.doToken(http.MethodPost, "/api/archive-extract", `{"instance":"rw","targetInstance":"rw2","key":"folder/a.zip","target":"out/","entries":["x.txt"]}`)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status %d, body %q", recorder.Code, recorder.Body.String())
	}
	targetFolder := "service:" + atServiceID + "/bucket:rw2/folder:out"
	want := []atQuestion{
		{actionObjectRead, atFolder, false, true},
		{actionObjectRead, targetFolder, false, true},
		{actionObjectWrite, targetFolder, true, false},
	}
	if got := atSummarize(env.fake.Questions()); !atSameSummary(got, want) {
		t.Fatalf("questions %+v, want %+v", got, want)
	}

	// An unknown target gives 404 and no question.
	env.fake.Reset()
	recorder = env.doToken(http.MethodPost, "/api/archive-extract", `{"instance":"rw","targetInstance":"nope","key":"folder/a.zip","target":"out/","entries":["x.txt"]}`)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown target: status %d, body %q", recorder.Code, recorder.Body.String())
	}
	if count := len(env.fake.Questions()); count != 0 {
		t.Fatalf("an unknown target gave %d questions", count)
	}
}

// ---- the whole route table ----

type amSurface struct {
	method string
	target string
	body   string
	header http.Header
}

// amSurfaces gives, for each route of the table that asks the PDP, one request of a valid shape for each method that
// the route serves. The requests of a job, of an upload and of a SQLite session use state that the function creates
// with a PDP that says true.
func amSurfaces(t *testing.T, env *accessTestEnv) map[string][]amSurface {
	t.Helper()
	jobID := amStartCopyJob(t, env)
	upload := amCreateUpload(t, env, "other/surface.bin")
	sessionID := amCreateSQLiteSession(t, env)
	uploadHeader := http.Header{"X-Stowr-Resume-Token": {upload.token}}
	chunkHeader := http.Header{"X-Stowr-Resume-Token": {upload.token}, "Content-Range": {"bytes 0-2/3"}}
	key := "instance=rw&key=folder%2Fdirect.txt"
	object := func(path string) []amSurface { return []amSurface{{method: http.MethodGet, target: path + "?" + key}} }
	surfaces := map[string][]amSurface{
		"/s3": {
			{method: http.MethodGet, target: "/s3?" + key}, {method: http.MethodHead, target: "/s3?" + key},
			{method: http.MethodGet, target: "/s3?instance=rw&prefix=folder%2F"},
			{method: http.MethodPut, target: "/s3?instance=rw&key=other%2Fnew.txt", body: "hello"},
			{method: http.MethodDelete, target: "/s3?" + key}, {method: http.MethodDelete, target: "/s3?" + key + "&version=v1"},
		},
		"/api/permissions/refresh": {{method: http.MethodPost, target: "/api/permissions/refresh?instance=rw"}},
		// A subfolder: a PDP that says false gives 403. The root gives a navigation listing (access_display_test.go).
		"/api/list":      {{method: http.MethodGet, target: "/api/list?instance=rw&prefix=folder%2Fnested%2F"}, {method: http.MethodHead, target: "/api/list?instance=rw&prefix=folder%2Fnested%2F"}},
		"/api/instances": {{method: http.MethodGet, target: "/api/instances"}},
		"/api/stats":     {{method: http.MethodGet, target: "/api/stats?instance=rw&prefix=folder%2F"}},
		"/api/archive":   {{method: http.MethodGet, target: "/api/archive?instance=rw&prefix=folder%2F"}},
		"/api/copy": {
			{method: http.MethodPost, target: "/api/copy", body: `{"instance":"rw","src":"folder/direct.txt","dst":"other/copy.txt"}`},
			{method: http.MethodPost, target: "/api/copy", body: `{"instance":"rw","src":"folder/nested/","dst":"copied2/","isPrefix":true}`},
		},
		"/api/rename": {
			{method: http.MethodPost, target: "/api/rename", body: `{"instance":"rw","src":"folder/direct.txt","dst":"other/moved.txt"}`},
			{method: http.MethodPost, target: "/api/rename", body: `{"instance":"rw","src":"folder/nested/","dst":"moved2/","isPrefix":true}`},
		},
		"/api/delete-prefix": {{method: http.MethodPost, target: "/api/delete-prefix", body: `{"instance":"rw","prefix":"folder/nested/"}`}},
		"/api/tags": {
			{method: http.MethodGet, target: "/api/tags?" + key},
			{method: http.MethodPut, target: "/api/tags", body: `{"instance":"rw","key":"folder/direct.txt","tags":[{"key":"a","value":"1"}]}`},
		},
		"/api/tags/bulk": {{method: http.MethodPost, target: "/api/tags/bulk", body: `{"instance":"rw","prefix":"folder/","add":[{"key":"a","value":"1"}]}`}},
		"/api/metadata": {
			{method: http.MethodGet, target: "/api/metadata?" + key},
			{method: http.MethodPut, target: "/api/metadata", body: `{"instance":"rw","key":"folder/direct.txt","etag":"abc"}`},
		},
		"/api/lifecycle": {
			{method: http.MethodGet, target: "/api/lifecycle?instance=rw"},
			{method: http.MethodPut, target: "/api/lifecycle", body: `{"instance":"rw","hash":"` + strings.Repeat("a", 64) + `","rules":[]}`},
		},
		"/api/encryption": {{method: http.MethodGet, target: "/api/encryption?instance=rw"}},
		"/api/usage":      {{method: http.MethodGet, target: "/api/usage?instance=rw"}},
		"/api/object-lock": {
			{method: http.MethodGet, target: "/api/object-lock?" + key},
			{method: http.MethodPut, target: "/api/object-lock", body: `{"instance":"rw","key":"folder/direct.txt","version":"v1","legalHold":true}`},
		},
		"/api/multiparts": {{method: http.MethodGet, target: "/api/multiparts?instance=rw&prefix=folder%2F"}},
		"/api/multiparts/abort": {
			{method: http.MethodPost, target: "/api/multiparts/abort", body: `{"instance":"rw","uploads":[{"key":"folder/a.bin","uploadId":"u1"}]}`},
			{method: http.MethodPost, target: "/api/multiparts/abort", body: `{"instance":"rw","olderThanDays":7,"prefix":"folder/"}`},
		},
		"/api/jobs/": {
			{method: http.MethodGet, target: "/api/jobs/" + jobID},
			{method: http.MethodPost, target: "/api/jobs/" + jobID + "/pause"},
			{method: http.MethodPost, target: "/api/jobs/" + jobID + "/resume"},
			{method: http.MethodPost, target: "/api/jobs/" + jobID + "/cancel"},
		},
		"/api/uploads/resume": {{method: http.MethodPost, target: "/api/uploads/resume", body: fmt.Sprintf(`{"resumeToken":%q}`, upload.token)}},
		"/api/uploads": {
			{method: http.MethodPost, target: "/api/uploads", body: `{"instance":"rw","key":"other/new.bin","size":3}`},
			{method: http.MethodGet, target: "/api/uploads", header: uploadHeader},
			{method: http.MethodPut, target: "/api/uploads", body: "abc", header: chunkHeader},
			{method: http.MethodDelete, target: "/api/uploads", header: uploadHeader},
		},
		"/api/spreadsheet":    object("/api/spreadsheet"),
		"/api/delimited":      object("/api/delimited"),
		"/api/document-count": object("/api/document-count"),
		"/api/document/word":  object("/api/document/word"),
		"/api/parquet":        object("/api/parquet"),
		"/api/sqlite/sessions": {
			{method: http.MethodPost, target: "/api/sqlite/sessions", body: `{"instance":"rw","key":"folder/fixture.sqlite"}`},
		},
		"/api/sqlite/sessions/": {
			{method: http.MethodGet, target: "/api/sqlite/sessions/" + sessionID},
			{method: http.MethodGet, target: "/api/sqlite/sessions/" + sessionID + "/table?table=items"},
			{method: http.MethodDelete, target: "/api/sqlite/sessions/" + sessionID},
		},
		"/api/json/raw":           object("/api/json/raw"),
		"/api/json/beautify":      object("/api/json/beautify"),
		"/api/json/summary":       object("/api/json/summary"),
		"/api/json/tree":          object("/api/json/tree"),
		"/api/search":             {{method: http.MethodGet, target: "/api/search?" + key + "&q=direct"}},
		"/api/media-info":         object("/api/media-info"),
		"/api/structured-preview": object("/api/structured-preview"),
		"/api/archive-preview":    object("/api/archive-preview"),
		"/api/archive-entry": {
			{method: http.MethodGet, target: "/api/archive-entry?" + key + "&entry=a.txt"},
			{method: http.MethodHead, target: "/api/archive-entry?" + key + "&entry=a.txt"},
		},
		"/api/archive-entry/integrity": {{method: http.MethodPost, target: "/api/archive-entry/integrity", body: `{"instance":"rw","key":"folder/direct.txt","entry":"a.txt"}`}},
		"/api/archive-extract": {{method: http.MethodPost, target: "/api/archive-extract",
			body: `{"instance":"rw","key":"folder/a.zip","target":"extracted/","entries":["x.txt"]}`}},
		"/api/versions": {
			{method: http.MethodGet, target: "/api/versions?" + key},
			{method: http.MethodDelete, target: "/api/versions?" + key + "&version=v1"},
		},
		"/api/version-counts":   {{method: http.MethodPost, target: "/api/version-counts", body: `{"instance":"rw","keys":["folder/direct.txt"]}`}},
		"/api/versions/restore": {{method: http.MethodPost, target: "/api/versions/restore", body: `{"instance":"rw","key":"folder/direct.txt","version":"v1"}`}},
		"/api/integrity":        {{method: http.MethodPost, target: "/api/integrity", body: `{"instance":"rw","key":"folder/direct.txt"}`}},
		"/api/inspect":          object("/api/inspect"),
		"/api/image-preview":    object("/api/image-preview"),
		accessOptionsPath:       {{method: http.MethodGet, target: accessOptionsPath + "?type=bucket"}},
		"/open/":                {{method: http.MethodGet, target: "/open/?" + key}, {method: http.MethodHead, target: "/open/?" + key}},
	}
	return surfaces
}

func TestEveryRouteThatAsksTheHandlerRefusesWithAPDPThatSaysFalse(t *testing.T) {
	env := amSQLiteEnv(t)
	surfaces := amSurfaces(t, env)

	// A route of the table needs a request here, and a request here needs a route of the table.
	asking := map[string]bool{}
	for _, entry := range env.app.routeTable() {
		if entry.access != routeHandlerAsks {
			continue
		}
		asking[entry.pattern] = true
		if len(surfaces[entry.pattern]) == 0 {
			t.Errorf("the route %q asks the PDP and has no request in this test", entry.pattern)
		}
	}
	for pattern := range surfaces {
		if !asking[pattern] {
			t.Errorf("the test has requests for %q, and the route table has no such route that asks the PDP", pattern)
		}
	}

	env.fake.Reset()
	env.fake.SetAnswer(atAnswerAll(false))
	for pattern, requests := range surfaces {
		for _, request := range requests {
			header := http.Header{defaultIdentityHeader: {env.token()}}
			for name, values := range request.header {
				header[name] = values
			}
			recorder := env.do(request.method, request.target, request.body, header)
			if pattern == accessOptionsPath {
				// The options route shows only what the PDP allows. It answers 200 and shows nothing here.
				if strings.Contains(recorder.Body.String(), `"rw"`) {
					t.Errorf("the options route shows the bucket %q with a PDP that says false", "rw")
				}
				continue
			}
			if pattern == "/api/instances" {
				// The list of the instances shows only what the PDP allows. It answers 200 and shows nothing here.
				if strings.Contains(recorder.Body.String(), `"id":"rw"`) {
					t.Errorf("the list of the instances shows the bucket %q with a PDP that says false", "rw")
				}
				continue
			}
			if recorder.Code >= 200 && recorder.Code <= 299 {
				t.Errorf("%s %s (route %q): status %d with a PDP that says false", request.method, request.target, pattern, recorder.Code)
			}
		}
	}
	if len(env.fake.Questions()) == 0 {
		t.Fatal("no request reached the PDP")
	}
	if _, found := amMemory(env).objects["tenant/other/new.txt"]; found {
		t.Error("a refused write created an object")
	}
	if _, found := amMemory(env).objects["tenant/folder/direct.txt"]; !found {
		t.Error("a refused delete removed an object")
	}
}
