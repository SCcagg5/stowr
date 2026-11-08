package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"
)

// The tests of the display questions: the list of the instances, the access member of a listing and the navigation
// listing. They use the fake PDP and the helpers of access_test.go.

type adInstancesBody struct {
	Default   string `json:"default"`
	Instances []struct {
		ID           string                     `json:"id"`
		Capabilities map[string]json.RawMessage `json:"capabilities"`
		Operations   map[string]capabilityState `json:"operations"`
	} `json:"instances"`
}

func adDecodeInstances(t *testing.T, recorder *httptest.ResponseRecorder) adInstancesBody {
	t.Helper()
	var body adInstancesBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", recorder.Body.String(), err)
	}
	return body
}

func adCapability(t *testing.T, raw json.RawMessage) capabilityState {
	t.Helper()
	var state capabilityState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("capability %q: %v", raw, err)
	}
	return state
}

type adListBody struct {
	Items []struct {
		Type   string `json:"type"`
		Name   string `json:"name"`
		Prefix string `json:"prefix"`
		Key    string `json:"key"`
	} `json:"items"`
	Access *listAccessJSON `json:"access"`
}

func adDecodeList(t *testing.T, recorder *httptest.ResponseRecorder) adListBody {
	t.Helper()
	var body adListBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", recorder.Body.String(), err)
	}
	return body
}

// adAddRootObject adds an object at the root of the bucket rw, below the root prefix.
func adAddRootObject(env *accessTestEnv) {
	backend := env.app.instances["rw"].backend.(*memoryBackend)
	backend.mu.Lock()
	defer backend.mu.Unlock()
	backend.objects["tenant/root.txt"] = memoryObject{data: []byte("root"), contentType: "text/plain", modified: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
}

func TestInstancesShowOnlyTheBucketsWithOneTrueAnswer(t *testing.T) {
	env := newAccessTestEnv(t)
	env.fake.SetAnswer(func(q fakeQuestion) map[string]any {
		return map[string]any{"decision": q.Action == actionObjectRead && q.Match == "subtree" && q.Path == atBucket}
	})
	recorder := env.doToken(http.MethodGet, "/api/instances", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d, body %q", recorder.Code, recorder.Body.String())
	}
	body := adDecodeInstances(t, recorder)
	if len(body.Instances) != 1 || body.Instances[0].ID != "rw" || body.Default != "rw" {
		t.Fatalf("instances %+v, default %q", body.Instances, body.Default)
	}
	read := adCapability(t, body.Instances[0].Capabilities["read"])
	if read.Source == "access" || !read.Allowed {
		t.Fatalf("read = %+v", read)
	}
	for _, name := range []string{"write", "delete"} {
		state := adCapability(t, body.Instances[0].Capabilities[name])
		if state.State != capabilityDenied || state.Source != "access" || state.Allowed {
			t.Fatalf("%s = %+v", name, state)
		}
	}
	for _, name := range []string{"upload", "overwrite", "createFolder", "copy", "move", "rename", "delete"} {
		state := body.Instances[0].Operations[name]
		if state.State != capabilityDenied {
			t.Fatalf("operation %s = %+v", name, state)
		}
	}
	for _, name := range []string{"upload", "delete"} {
		if body.Instances[0].Operations[name].Source != "access" {
			t.Fatalf("operation %s = %+v", name, body.Instances[0].Operations[name])
		}
	}
	for _, name := range []string{"list", "preview", "download"} {
		if state := body.Instances[0].Operations[name]; state.State == capabilityDenied {
			t.Fatalf("operation %s = %+v", name, state)
		}
	}

	// The questions: the five actions on the bucket node of each instance, match subtree, no usage.
	questions := env.fake.Questions()
	if len(questions) != 10 {
		t.Fatalf("%d questions %+v, want 10", len(questions), questions)
	}
	seen := map[string]bool{}
	for _, question := range questions {
		if question.Match != "subtree" || question.Usage || question.Subject != atSubject || !question.Batch {
			t.Fatalf("question %+v", question)
		}
		if question.Path != atBucket && question.Path != "service:"+atServiceID+"/bucket:readonly" {
			t.Fatalf("question %+v is not on a bucket node", question)
		}
		seen[question.Action+" "+question.Path] = true
	}
	if len(seen) != 10 {
		t.Fatalf("the questions are not distinct: %+v", questions)
	}
	actions := map[string]bool{}
	for _, question := range questions {
		actions[question.Action] = true
	}
	var names []string
	for name := range actions {
		names = append(names, name)
	}
	sort.Strings(names)
	want := []string{actionBucketAdmin, actionObjectDelete, actionObjectLock, actionObjectRead, actionObjectWrite}
	for index := range want {
		if names[index] != want[index] {
			t.Fatalf("actions %v, want %v", names, want)
		}
	}
}

func TestInstancesGiveAnEmptyListWhenEveryAnswerIsFalse(t *testing.T) {
	env := newAccessTestEnv(t)
	env.fake.SetAnswer(atAnswerAll(false))
	recorder := env.doToken(http.MethodGet, "/api/instances", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d, body %q", recorder.Code, recorder.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["instances"]) != "[]" || string(raw["default"]) != `""` {
		t.Fatalf("instances %s, default %s", raw["instances"], raw["default"])
	}
}

func TestInstancesGiveTheFirstVisibleAsDefault(t *testing.T) {
	env := newAccessTestEnv(t)
	env.fake.SetAnswer(func(q fakeQuestion) map[string]any {
		return map[string]any{"decision": q.Action == actionObjectLock && q.Path == "service:"+atServiceID+"/bucket:readonly"}
	})
	recorder := env.doToken(http.MethodGet, "/api/instances", "")
	body := adDecodeInstances(t, recorder)
	if recorder.Code != http.StatusOK || len(body.Instances) != 1 || body.Instances[0].ID != "readonly" || body.Default != "readonly" {
		t.Fatalf("status %d, body %q", recorder.Code, recorder.Body.String())
	}
	// This person holds only object.lock: read, write and delete show the access source.
	for _, name := range []string{"read", "write", "delete"} {
		if state := adCapability(t, body.Instances[0].Capabilities[name]); state.State != capabilityDenied || state.Source != "access" {
			t.Fatalf("%s = %+v", name, state)
		}
	}
}

func TestInstancesGive503WhenThePDPFails(t *testing.T) {
	env := newAccessTestEnv(t)
	env.fake.SetStatus(http.StatusInternalServerError)
	recorder := env.doToken(http.MethodGet, "/api/instances", "")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, body %q", recorder.Code, recorder.Body.String())
	}
	if body := atDecodeError(t, recorder); body.Error.Code != "pdp_unavailable" || !body.Error.Retryable {
		t.Fatalf("error %+v", body.Error)
	}
}

func TestListGivesTheAccessMemberWhenTheNodeIsReadable(t *testing.T) {
	env := newAccessTestEnv(t)
	env.fake.SetAnswer(func(q fakeQuestion) map[string]any {
		return map[string]any{"decision": q.Action != actionObjectDelete}
	})
	recorder := env.doToken(http.MethodGet, "/api/list?instance=rw&prefix=folder%2F", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d, body %q", recorder.Code, recorder.Body.String())
	}
	body := adDecodeList(t, recorder)
	if body.Access == nil || *body.Access != (listAccessJSON{Read: true, Write: true, Delete: false}) {
		t.Fatalf("access = %+v", body.Access)
	}
	var keys []string
	for _, item := range body.Items {
		keys = append(keys, item.Type+":"+item.Name)
	}
	if len(keys) != 2 || keys[0] != "prefix:nested/" || keys[1] != "content:direct.txt" {
		t.Fatalf("items %v", keys)
	}
	questions := env.fake.Questions()
	if len(questions) != 3 || env.fake.Requests() != 1 {
		t.Fatalf("%d questions %+v in %d requests, want 3 in 1", len(questions), questions, env.fake.Requests())
	}
	for index, action := range []string{actionObjectRead, actionObjectWrite, actionObjectDelete} {
		if questions[index].Action != action || questions[index].Path != atFolder || questions[index].Match != "node" || questions[index].Usage {
			t.Fatalf("question %d = %+v", index, questions[index])
		}
	}
}

func TestListShowsTheFoldersOnlyWhenThePersonCannotReadTheBucket(t *testing.T) {
	env := newAccessTestEnv(t)
	adAddRootObject(env)
	env.fake.SetAnswer(func(q fakeQuestion) map[string]any {
		switch {
		case q.Action == actionObjectRead && q.Match == "subtree":
			return map[string]any{"decision": q.Path == atFolder}
		case q.Action == actionObjectWrite && q.Match == "node":
			return map[string]any{"decision": true}
		}
		return map[string]any{"decision": false}
	})
	recorder := env.doToken(http.MethodGet, "/api/list?instance=rw", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d, body %q", recorder.Code, recorder.Body.String())
	}
	body := adDecodeList(t, recorder)
	if len(body.Items) != 1 || body.Items[0].Type != "prefix" || body.Items[0].Prefix != "folder/" {
		t.Fatalf("items %+v", body.Items)
	}
	if body.Access == nil || *body.Access != (listAccessJSON{Read: false, Write: true, Delete: false}) {
		t.Fatalf("access = %+v", body.Access)
	}

	// The questions: three on the node of the bucket, then one subtree question for each distinct folder item.
	questions := env.fake.Questions()
	if len(questions) != 5 {
		t.Fatalf("%d questions %+v, want 5", len(questions), questions)
	}
	for _, question := range questions[:3] {
		if question.Path != atBucket || question.Match != "node" {
			t.Fatalf("question %+v", question)
		}
	}
	subtree := map[string]bool{}
	for _, question := range questions[3:] {
		if question.Action != actionObjectRead || question.Match != "subtree" || question.Usage {
			t.Fatalf("question %+v", question)
		}
		subtree[question.Path] = true
	}
	if !subtree[atFolder] || !subtree[atBucket+"/folder:a"] || len(subtree) != 2 {
		t.Fatalf("subtree questions %v", subtree)
	}
	if env.fake.Requests() != 2 {
		t.Fatalf("%d requests, want 2", env.fake.Requests())
	}

	// HEAD gives the same status.
	recorder = env.doToken(http.MethodHead, "/api/list?instance=rw", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("HEAD status %d", recorder.Code)
	}
}

func TestListNavigationGivesAnEmptyListWhenNoFolderIsReadable(t *testing.T) {
	env := newAccessTestEnv(t)
	adAddRootObject(env)
	env.fake.SetAnswer(atAnswerAll(false))
	recorder := env.doToken(http.MethodGet, "/api/list?instance=rw", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d, body %q", recorder.Code, recorder.Body.String())
	}
	body := adDecodeList(t, recorder)
	if len(body.Items) != 0 || body.Access == nil || body.Access.Read {
		t.Fatalf("items %+v, access %+v", body.Items, body.Access)
	}
}

func TestListGives403AtASubfolderThatThePersonCannotRead(t *testing.T) {
	env := newAccessTestEnv(t)
	env.fake.SetAnswer(atAnswerAll(false))
	recorder := env.doToken(http.MethodGet, "/api/list?instance=rw&prefix=folder%2Fnested%2F", "")
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status %d, body %q", recorder.Code, recorder.Body.String())
	}
	if body := atDecodeError(t, recorder); body.Error.Code != "forbidden" || body.Error.Action != actionObjectRead {
		t.Fatalf("error %+v", body.Error)
	}
	for _, question := range env.fake.Questions() {
		if question.Match != "node" {
			t.Fatalf("question %+v is not a node question", question)
		}
	}
}

func TestListGivesANavigationListingAtAFolder(t *testing.T) {
	// A folder has a deeper type. A false answer gives a navigation listing, not a 403.
	env := newAccessTestEnv(t)
	env.fake.SetAnswer(atAnswerAll(false))
	recorder := env.doToken(http.MethodGet, "/api/list?instance=rw&prefix=folder%2F", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d, body %q", recorder.Code, recorder.Body.String())
	}
	if body := adDecodeList(t, recorder); len(body.Items) != 0 || body.Access == nil || body.Access.Read {
		t.Fatalf("body %+v", body)
	}
}

func TestListGives503WhenThePDPFails(t *testing.T) {
	env := newAccessTestEnv(t)
	env.fake.SetStatus(http.StatusInternalServerError)
	recorder := env.doToken(http.MethodGet, "/api/list?instance=rw", "")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, body %q", recorder.Code, recorder.Body.String())
	}
	if body := atDecodeError(t, recorder); body.Error.Code != "pdp_unavailable" {
		t.Fatalf("error %+v", body.Error)
	}
}

func TestListNavigationGives503WhenTheSubtreeQuestionsFail(t *testing.T) {
	env := newAccessTestEnv(t)
	env.fake.SetAnswer(func(q fakeQuestion) map[string]any {
		if q.Match == "node" {
			// The batch of the node is answered: the next request fails.
			env.fake.SetStatus(http.StatusInternalServerError)
		}
		return map[string]any{"decision": false}
	})
	recorder := env.doToken(http.MethodGet, "/api/list?instance=rw", "")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, body %q", recorder.Code, recorder.Body.String())
	}
	if body := atDecodeError(t, recorder); body.Error.Code != "pdp_unavailable" {
		t.Fatalf("error %+v", body.Error)
	}
}

func TestListAndInstancesKeepTheOpenModeAnswers(t *testing.T) {
	app, _, _ := testApplication(t)
	handler := app.routes()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/list?instance=rw", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d, body %q", recorder.Code, recorder.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if _, found := raw["access"]; found {
		t.Fatalf("the answer has an access member: %q", recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/instances", nil))
	body := adDecodeInstances(t, recorder)
	if recorder.Code != http.StatusOK || len(body.Instances) != 2 || body.Default != "readonly" {
		t.Fatalf("status %d, body %q", recorder.Code, recorder.Body.String())
	}
	for _, instance := range body.Instances {
		for name, raw := range instance.Capabilities {
			if name == "read" || name == "write" || name == "delete" {
				if adCapability(t, raw).Source == "access" {
					t.Fatalf("%s: capability %s has the source access", instance.ID, name)
				}
			}
		}
	}
	if app.pdp != nil {
		t.Fatal("the open mode has a PDP client")
	}
}
