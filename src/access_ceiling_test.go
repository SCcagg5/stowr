package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The tests of the ceiling max_object_size. They use newAccessTestEnv and the fake PDP. The backends are the memory
// backends of testApplication. A PDP answer gives the ceiling of object.read, of object.write, or of both.

// acNoCeiling is the value that gives no ceiling in the answers of acAnswer.
const acNoCeiling = int64(-1)

// acAnswer answers true to each question. It adds max_object_size to the answers of object.read and object.write when
// the value is not acNoCeiling. A question with usage gets one unit.
func acAnswer(read, write int64) func(fakeQuestion) map[string]any {
	return func(q fakeQuestion) map[string]any {
		answerContext := map[string]any{}
		if q.Usage {
			answerContext["usage"] = map[string]any{"units": 1}
		}
		switch {
		case q.Action == actionObjectRead && read != acNoCeiling:
			answerContext["limits"] = map[string]any{limitMaxObjectSize: read}
		case q.Action == actionObjectWrite && write != acNoCeiling:
			answerContext["limits"] = map[string]any{limitMaxObjectSize: write}
		}
		return map[string]any{"decision": true, "context": answerContext}
	}
}

// acSeed puts an object of the given size in the memory backend of the instance "rw". The key has no root prefix.
func acSeed(env *accessTestEnv, key string, size int) {
	acSeedData(env, key, bytes.Repeat([]byte("x"), size))
}

func acSeedData(env *accessTestEnv, key string, data []byte) {
	backend := amMemory(env)
	backend.mu.Lock()
	defer backend.mu.Unlock()
	backend.objects["tenant/"+key] = memoryObject{data: data, contentType: "application/octet-stream", modified: time.Now().UTC()}
}

func acHas(env *accessTestEnv, key string) bool {
	backend := amMemory(env)
	backend.mu.Lock()
	defer backend.mu.Unlock()
	_, found := backend.objects["tenant/"+key]
	return found
}

func acCountPrefix(env *accessTestEnv, prefix string) int {
	backend := amMemory(env)
	backend.mu.Lock()
	defer backend.mu.Unlock()
	count := 0
	for key := range backend.objects {
		if strings.HasPrefix(key, "tenant/"+prefix) {
			count++
		}
	}
	return count
}

func acGatewayGet(env *accessTestEnv, key string, header http.Header) *httptest.ResponseRecorder {
	if header == nil {
		header = http.Header{}
	}
	header.Set(defaultIdentityHeader, env.token())
	return env.do(http.MethodGet, "/s3?instance=rw&key="+key, "", header)
}

// acWantCeiling checks the 403 answer of a JSON route: the code, the action, the ceiling and the limit.
func acWantCeiling(t *testing.T, recorder *httptest.ResponseRecorder, action string, limit int64) {
	t.Helper()
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status %d, body %q", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Action  string `json:"action"`
			Ceiling string `json:"ceiling"`
			Limit   int64  `json:"limit"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q is not an error object: %v", recorder.Body.String(), err)
	}
	got := body.Error
	if got.Code != codeCeilingExceeded || got.Action != action || got.Ceiling != limitMaxObjectSize || got.Limit != limit {
		t.Fatalf("error = %+v, want code %s, action %s, limit %d", got, codeCeilingExceeded, action, limit)
	}
}

func acWaitJob(t *testing.T, env *accessTestEnv, recorder *httptest.ResponseRecorder, status string) jobState {
	t.Helper()
	if recorder.Code != http.StatusAccepted && recorder.Code != http.StatusOK {
		t.Fatalf("job request: status %d, body %q", recorder.Code, recorder.Body.String())
	}
	var created publicJob
	if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	return waitForJobStatus(t, env.app.jobs, created.ID, status)
}

// ---- the read ceiling ----

func TestCeilingGatewayGet(t *testing.T) {
	env := newAccessTestEnv(t)
	acSeedData(env, "folder/twelve.txt", []byte("hello world!"))
	logs := atCaptureLog(t)

	env.fake.SetAnswer(acAnswer(10, acNoCeiling))
	recorder := acGatewayGet(env, "folder%2Ftwelve.txt", http.Header{"X-Request-Id": {"rid-get"}})
	if recorder.Code != http.StatusForbidden || strings.Contains(recorder.Body.String(), "hello") ||
		!strings.Contains(recorder.Body.String(), "max_object_size") {
		t.Fatalf("over the ceiling: status %d, body %q", recorder.Code, recorder.Body.String())
	}
	if text := logs(); !strings.Contains(text, "access.ceiling_refused request_id=rid-get action=object.read ceiling=max_object_size limit=10 size=12") {
		t.Fatalf("log:\n%s", text)
	}

	env.fake.SetAnswer(acAnswer(20, acNoCeiling))
	recorder = acGatewayGet(env, "folder%2Ftwelve.txt", nil)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "hello world!" {
		t.Fatalf("under the ceiling: status %d, body %q", recorder.Code, recorder.Body.String())
	}

	// A size equal to the ceiling is allowed.
	env.fake.SetAnswer(acAnswer(12, acNoCeiling))
	if recorder = acGatewayGet(env, "folder%2Ftwelve.txt", nil); recorder.Code != http.StatusOK {
		t.Fatalf("equal to the ceiling: status %d", recorder.Code)
	}
}

// The conformance case U37 reads reports/a.txt of 12 bytes with the ceiling 1048576: the answer is 2xx.
func TestCeilingConformanceCaseU37(t *testing.T) {
	env := newAccessTestEnv(t)
	acSeedData(env, "reports/a.txt", []byte("hello world!"))
	env.fake.SetAnswer(acAnswer(1048576, acNoCeiling))
	recorder := acGatewayGet(env, "reports%2Fa.txt", nil)
	if recorder.Code < 200 || recorder.Code > 299 || recorder.Body.String() != "hello world!" {
		t.Fatalf("status %d, body %q", recorder.Code, recorder.Body.String())
	}
}

func TestCeilingJSONErrorShape(t *testing.T) {
	env := newAccessTestEnv(t)
	acSeed(env, "folder/big.json", 40)
	env.fake.SetAnswer(acAnswer(10, acNoCeiling))
	recorder := env.doToken(http.MethodGet, "/api/json/raw?instance=rw&key=folder%2Fbig.json", "")
	acWantCeiling(t, recorder, actionObjectRead, 10)
}

func TestCeilingRangeReadOfABigObjectGivesNoData(t *testing.T) {
	env := newAccessTestEnv(t)
	acSeed(env, "folder/big.bin", 100)
	backend := amMemory(env)
	env.fake.SetAnswer(acAnswer(10, acNoCeiling))

	// A range of 5 bytes of an object of 100 bytes: the ceiling applies to the object, not to the range.
	recorder := acGatewayGet(env, "folder%2Fbig.bin&preview=1", http.Header{"Range": {"bytes=0-4"}})
	if recorder.Code != http.StatusForbidden || recorder.Body.Len() > 200 || strings.Contains(recorder.Body.String(), "xxxxx") {
		t.Fatalf("gateway range: status %d, body %q", recorder.Code, recorder.Body.String())
	}

	// A preview route of the API reads ranges through the range source.
	putTestZip(t, backend, "tenant/folder/big.zip", map[string][]byte{"a.txt": bytes.Repeat([]byte("a"), 100)})
	recorder = env.doToken(http.MethodGet, "/api/archive-preview?instance=rw&key=folder%2Fbig.zip", "")
	acWantCeiling(t, recorder, actionObjectRead, 10)

	backend.mu.Lock()
	read := backend.getBytesRead
	backend.mu.Unlock()
	if read != 0 {
		t.Fatalf("the service read %d bytes of the object before the refusal", read)
	}

	env.fake.SetAnswer(acAnswer(1<<20, acNoCeiling))
	recorder = acGatewayGet(env, "folder%2Fbig.bin&preview=1", http.Header{"Range": {"bytes=0-4"}})
	if recorder.Code != http.StatusPartialContent || recorder.Body.String() != "xxxxx" {
		t.Fatalf("under the ceiling: status %d, body %q", recorder.Code, recorder.Body.String())
	}
}

func TestCeilingRangeSourceChecksACachedSpan(t *testing.T) {
	env := newAccessTestEnv(t)
	acSeed(env, "folder/big.bin", 100)
	instance := env.app.instances["rw"]
	source, err := openObjectRangeSource(context.Background(), instance, "folder/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.ReadRange(0, 10); err != nil {
		t.Fatal(err)
	}
	// A request with a lower ceiling reads the same source: the cache gives no data.
	request := &accessRequest{Subject: atSubject, RequestID: "rid-cache"}
	request.recordCeiling(actionObjectRead, limitMaxObjectSize, 10)
	ctx := context.WithValue(context.Background(), accessRequestKey{}, request)
	if _, err := source.withContext(ctx).ReadRange(0, 5); err == nil || atAsAPIError(t, err).Code != codeCeilingExceeded {
		t.Fatalf("error = %v", err)
	}
}

// ---- the write ceiling ----

func TestCeilingGatewayPut(t *testing.T) {
	env := newAccessTestEnv(t)
	env.fake.SetAnswer(acAnswer(acNoCeiling, 10))
	put := func(key, body string, length int64) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPut, "/s3?instance=rw&key="+key, strings.NewReader(body))
		request.ContentLength = length
		request.Header.Set(defaultIdentityHeader, env.token())
		recorder := httptest.NewRecorder()
		env.handler.ServeHTTP(recorder, request)
		return recorder
	}

	recorder := put("other%2Fnew.txt", strings.Repeat("y", 20), 20)
	if recorder.Code != http.StatusForbidden || acHas(env, "other/new.txt") {
		t.Fatalf("over the ceiling: status %d, body %q, object stored %v", recorder.Code, recorder.Body.String(), acHas(env, "other/new.txt"))
	}
	// An unknown size has no check that can pass: the ceiling refuses it.
	recorder = put("other%2Fchunked.txt", "abc", -1)
	if recorder.Code != http.StatusForbidden || acHas(env, "other/chunked.txt") {
		t.Fatalf("unknown size: status %d, object stored %v", recorder.Code, acHas(env, "other/chunked.txt"))
	}
	recorder = put("other%2Ffits.txt", "0123456789", 10)
	if recorder.Code != http.StatusNoContent || !acHas(env, "other/fits.txt") {
		t.Fatalf("size equal to the ceiling: status %d, body %q", recorder.Code, recorder.Body.String())
	}
}

func TestCeilingStorageCopyChecksTheSourceWithAHead(t *testing.T) {
	env := newAccessTestEnv(t)
	acSeed(env, "folder/big.bin", 40)
	instance := env.app.instances["rw"]
	request := &accessRequest{Subject: atSubject, RequestID: "rid-meta"}
	request.recordCeiling(actionObjectWrite, limitMaxObjectSize, 10)
	ctx := context.WithValue(context.Background(), accessRequestKey{}, request)
	if err := instance.Copy(ctx, "tenant/folder/big.bin", "tenant/folder/copy.bin"); err == nil || atAsAPIError(t, err).Code != codeCeilingExceeded {
		t.Fatalf("copy error = %v", err)
	}
	if acHas(env, "folder/copy.bin") {
		t.Fatal("the copy ran")
	}
	// CopySized trusts the size that the caller gives and makes no HEAD request.
	backend := amMemory(env)
	backend.mu.Lock()
	before := backend.headCount
	backend.mu.Unlock()
	if err := instance.CopySized(ctx, "tenant/folder/big.bin", "tenant/folder/copy.bin", 40, nil); err == nil || atAsAPIError(t, err).Code != codeCeilingExceeded {
		t.Fatalf("CopySized error = %v", err)
	}
	backend.mu.Lock()
	after := backend.headCount
	backend.mu.Unlock()
	if after != before {
		t.Fatalf("CopySized made %d HEAD requests", after-before)
	}
}

// ---- copy and move ----

func TestCeilingCopy(t *testing.T) {
	env := newAccessTestEnv(t)
	acSeedData(env, "folder/twelve.txt", []byte("hello world!"))
	copyBody := `{"instance":"rw","src":"folder/twelve.txt","dst":"other/copy.txt"}`

	// The source is over the read ceiling. The destination ceiling is high.
	env.fake.SetAnswer(acAnswer(10, 1000))
	recorder := env.doToken(http.MethodPost, "/api/copy", copyBody)
	acWantCeiling(t, recorder, actionObjectRead, 10)
	if acHas(env, "other/copy.txt") {
		t.Fatal("the copy ran")
	}

	// The read ceiling is high. The destination ceiling is low.
	env.fake.SetAnswer(acAnswer(1000, 10))
	recorder = env.doToken(http.MethodPost, "/api/copy", copyBody)
	acWantCeiling(t, recorder, actionObjectWrite, 10)
	if acHas(env, "other/copy.txt") {
		t.Fatal("the copy ran")
	}

	env.fake.SetAnswer(acAnswer(1000, 1000))
	if recorder = env.doToken(http.MethodPost, "/api/copy", copyBody); recorder.Code != http.StatusOK || !acHas(env, "other/copy.txt") {
		t.Fatalf("under the ceilings: status %d, body %q", recorder.Code, recorder.Body.String())
	}
}

func TestCeilingRenameChecksTheSource(t *testing.T) {
	env := newAccessTestEnv(t)
	acSeedData(env, "folder/twelve.txt", []byte("hello world!"))
	env.fake.SetAnswer(acAnswer(1000, 10))
	recorder := env.doToken(http.MethodPost, "/api/rename", `{"instance":"rw","src":"folder/twelve.txt","dst":"other/moved.txt"}`)
	acWantCeiling(t, recorder, actionObjectWrite, 10)
	if !acHas(env, "folder/twelve.txt") || acHas(env, "other/moved.txt") {
		t.Fatal("the move ran")
	}
}

func TestCeilingPrefixCopyCopiesNothingWhenOneObjectIsOver(t *testing.T) {
	env := newAccessTestEnv(t)
	acSeed(env, "folder/set/a.txt", 3)
	acSeed(env, "folder/set/b.txt", 5)
	acSeed(env, "folder/set/z.txt", 30)
	env.fake.SetAnswer(acAnswer(10, 10))
	recorder := env.doToken(http.MethodPost, "/api/copy", `{"instance":"rw","src":"folder/set/","dst":"copied/","isPrefix":true}`)
	job := acWaitJob(t, env, recorder, jobStatusFailed)
	if !strings.Contains(job.Error, "max_object_size") {
		t.Fatalf("job error = %q", job.Error)
	}
	if got := acCountPrefix(env, "copied/"); got != 0 {
		t.Fatalf("%d objects copied before the refusal", got)
	}
	// The job keeps the ceilings of its request.
	if job.Ceilings[actionObjectRead][limitMaxObjectSize] != 10 || job.Ceilings[actionObjectWrite][limitMaxObjectSize] != 10 {
		t.Fatalf("job ceilings = %v", job.Ceilings)
	}

	// The same prefix with a high ceiling copies each object.
	env.fake.SetAnswer(acAnswer(100, 100))
	recorder = env.doToken(http.MethodPost, "/api/copy", `{"instance":"rw","src":"folder/set/","dst":"copied/","isPrefix":true}`)
	acWaitJob(t, env, recorder, jobStatusCompleted)
	if got := acCountPrefix(env, "copied/"); got != 3 {
		t.Fatalf("%d objects copied, want 3", got)
	}
}

func TestCeilingPrefixMoveMovesNothingWhenOneObjectIsOver(t *testing.T) {
	env := newAccessTestEnv(t)
	acSeed(env, "folder/set/a.txt", 3)
	acSeed(env, "folder/set/z.txt", 30)
	env.fake.SetAnswer(acAnswer(10, acNoCeiling))
	recorder := env.doToken(http.MethodPost, "/api/rename", `{"instance":"rw","src":"folder/set/","dst":"moved/","isPrefix":true}`)
	acWaitJob(t, env, recorder, jobStatusFailed)
	if acCountPrefix(env, "moved/") != 0 || !acHas(env, "folder/set/a.txt") {
		t.Fatal("a part of the prefix moved")
	}
}

func TestCeilingCopyOrMoveOfAPrefixChecksBeforeTheFirstCopy(t *testing.T) {
	env := newAccessTestEnv(t)
	acSeed(env, "folder/set/a.txt", 3)
	acSeed(env, "folder/set/z.txt", 30)
	request := &accessRequest{Subject: atSubject, RequestID: "rid-prefix"}
	request.recordCeiling(actionObjectRead, limitMaxObjectSize, 10)
	ctx := context.WithValue(context.Background(), accessRequestKey{}, request)
	if _, err := copyOrMove(ctx, env.app.instances["rw"], "folder/set/", "copied/", true, false); err == nil || atAsAPIError(t, err).Code != codeCeilingExceeded {
		t.Fatalf("error = %v", err)
	}
	if acCountPrefix(env, "copied/") != 0 {
		t.Fatal("a copy ran")
	}
}

// ---- the ZIP of a folder ----

func TestCeilingZipOfAFolderGivesNoByteWhenOneObjectIsOver(t *testing.T) {
	env := newAccessTestEnv(t)
	acSeed(env, "folder/zipme/small.txt", 3)
	acSeed(env, "folder/zipme/big.txt", 30)
	env.fake.SetAnswer(acAnswer(10, acNoCeiling))
	recorder := env.doToken(http.MethodGet, "/api/archive?instance=rw&prefix=folder%2Fzipme%2F", "")
	acWantCeiling(t, recorder, actionObjectRead, 10)
	if strings.Contains(recorder.Body.String(), "PK") || recorder.Header().Get("Content-Type") == "application/zip" {
		t.Fatalf("the answer has ZIP bytes: %q", recorder.Body.String())
	}

	env.fake.SetAnswer(acAnswer(100, acNoCeiling))
	recorder = env.doToken(http.MethodGet, "/api/archive?instance=rw&prefix=folder%2Fzipme%2F", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("under the ceiling: status %d", recorder.Code)
	}
	reader, err := zip.NewReader(bytes.NewReader(recorder.Body.Bytes()), int64(recorder.Body.Len()))
	if err != nil || len(reader.File) != 2 {
		t.Fatalf("zip: %v, %d files", err, len(reader.File))
	}
}

// ---- uploads ----

func TestCeilingUploadCreateAndChunk(t *testing.T) {
	env := newAccessTestEnv(t)
	backend := amMemory(env)

	env.fake.SetAnswer(acAnswer(acNoCeiling, 10))
	recorder := env.doToken(http.MethodPost, "/api/uploads", `{"instance":"rw","key":"other/up.bin","size":20}`)
	acWantCeiling(t, recorder, actionObjectWrite, 10)
	backend.mu.Lock()
	started := len(backend.multipart)
	backend.mu.Unlock()
	if started != 0 {
		t.Fatalf("%d multipart uploads started", started)
	}

	// An upload of 3 bytes fits. A later chunk gets its own ceiling: the answer of the continuation is the one that applies.
	env.fake.SetAnswer(acAnswer(acNoCeiling, 10))
	upload := amCreateUploadWith(t, env, "other/up.bin", 3)
	env.fake.SetAnswer(acAnswer(acNoCeiling, 2))
	recorder = env.uploadRequest(http.MethodPut, "/api/uploads", "abc", upload.token)
	acWantCeiling(t, recorder, actionObjectWrite, 2)
	backend.mu.Lock()
	for _, state := range backend.multipart {
		if len(state.parts) != 0 {
			t.Errorf("a part was stored: %d", len(state.parts))
		}
	}
	backend.mu.Unlock()

	env.fake.SetAnswer(acAnswer(acNoCeiling, 3))
	if recorder = env.uploadRequest(http.MethodPut, "/api/uploads", "abc", upload.token); recorder.Code != http.StatusOK {
		t.Fatalf("chunk under the ceiling: status %d, body %q", recorder.Code, recorder.Body.String())
	}
}

func amCreateUploadWith(t *testing.T, env *accessTestEnv, key string, size int) amUpload {
	t.Helper()
	recorder := env.doToken(http.MethodPost, "/api/uploads", fmt.Sprintf(`{"instance":"rw","key":%q,"size":%d}`, key, size))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create: status %d, body %q", recorder.Code, recorder.Body.String())
	}
	var created publicUpload
	if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil || created.ResumeToken == "" {
		t.Fatalf("create: body %q, error %v", recorder.Body.String(), err)
	}
	return amUpload{token: created.ResumeToken}
}

func TestCeilingStorageLevelUploadChecks(t *testing.T) {
	env := newAccessTestEnv(t)
	instance := env.app.instances["rw"]
	request := &accessRequest{Subject: atSubject, RequestID: "rid-up"}
	request.recordCeiling(actionObjectWrite, limitMaxObjectSize, 10)
	ctx := context.WithValue(context.Background(), accessRequestKey{}, request)
	if _, err := instance.UploadPart(ctx, "tenant/x", "upload-1", 1, strings.NewReader("0123456789ab"), 12); err == nil || atAsAPIError(t, err).Code != codeCeilingExceeded {
		t.Fatalf("UploadPart error = %v", err)
	}
	if _, err := instance.InitiateResumable(ctx, "tenant/x", 12, "text/plain"); err == nil || atAsAPIError(t, err).Code != codeCeilingExceeded {
		t.Fatalf("InitiateResumable error = %v", err)
	}
	if _, _, err := instance.UploadResumableChunk(ctx, "session", strings.NewReader("abc"), 0, 3, 12, "text/plain"); err == nil || atAsAPIError(t, err).Code != codeCeilingExceeded {
		t.Fatalf("UploadResumableChunk error = %v", err)
	}
	if err := instance.PutIfAbsent(ctx, "tenant/x", strings.NewReader("0123456789ab"), 12, "text/plain"); err == nil || atAsAPIError(t, err).Code != codeCeilingExceeded {
		t.Fatalf("PutIfAbsent error = %v", err)
	}
}

// ---- extraction of an archive ----

func TestCeilingExtractionChecksTheMembersBeforeTheFirstWrite(t *testing.T) {
	env := newAccessTestEnv(t)
	putTestZip(t, amMemory(env), "tenant/folder/a.zip", map[string][]byte{
		"a.txt": []byte("tiny"),
		"z.txt": bytes.Repeat([]byte("z"), 40),
	})
	body := `{"instance":"rw","key":"folder/a.zip","target":"extracted/","entries":["a.txt","z.txt"]}`

	env.fake.SetAnswer(acAnswer(acNoCeiling, 10))
	job := acWaitJob(t, env, env.doToken(http.MethodPost, "/api/archive-extract", body), jobStatusFailed)
	if !strings.Contains(job.Error, "max_object_size") {
		t.Fatalf("job error = %q", job.Error)
	}
	if got := acCountPrefix(env, "extracted/"); got != 0 {
		t.Fatalf("%d members written before the refusal", got)
	}

	// The archive is an object that the extraction reads: the read ceiling applies to it.
	env.fake.SetAnswer(acAnswer(10, acNoCeiling))
	job = acWaitJob(t, env, env.doToken(http.MethodPost, "/api/archive-extract", body), jobStatusFailed)
	if !strings.Contains(job.Error, "max_object_size") || acCountPrefix(env, "extracted/") != 0 {
		t.Fatalf("job error = %q", job.Error)
	}

	env.fake.SetAnswer(acAnswer(1<<20, 100))
	acWaitJob(t, env, env.doToken(http.MethodPost, "/api/archive-extract", body), jobStatusCompleted)
	if got := acCountPrefix(env, "extracted/"); got != 2 {
		t.Fatalf("%d members written, want 2", got)
	}
}

// ---- jobs ----

func TestCeilingJobKeepsTheCeilingOfItsRequest(t *testing.T) {
	env := newAccessTestEnv(t)
	acSeedData(env, "folder/twelve.txt", []byte("hello world!"))

	// The integrity job reads the bytes. The PDP is not asked again when the job runs: the job state carries the ceiling.
	env.fake.SetAnswer(acAnswer(10, acNoCeiling))
	recorder := env.doToken(http.MethodPost, "/api/integrity", `{"instance":"rw","key":"folder/twelve.txt"}`)
	acWantCeiling(t, recorder, actionObjectRead, 10)
	env.app.jobs.mu.RLock()
	var stored jobState
	for _, candidate := range env.app.jobs.jobs {
		stored = cloneJobState(*candidate)
	}
	env.app.jobs.mu.RUnlock()
	if stored.Status != jobStatusFailed || !strings.Contains(stored.Error, "max_object_size") || stored.Ceilings[actionObjectRead][limitMaxObjectSize] != 10 {
		t.Fatalf("job = status %s, error %q, ceilings %v", stored.Status, stored.Error, stored.Ceilings)
	}
	if questions := env.fake.Questions(); len(questions) != 1 {
		t.Fatalf("the PDP got %d questions, want 1", len(questions))
	}

	env.fake.SetAnswer(acAnswer(20, acNoCeiling))
	recorder = env.doToken(http.MethodPost, "/api/integrity", `{"instance":"rw","key":"folder/twelve.txt"}`)
	if recorder.Code != http.StatusOK && recorder.Code != http.StatusAccepted {
		t.Fatalf("under the ceiling: status %d, body %q", recorder.Code, recorder.Body.String())
	}
}

func TestCeilingJobContextCarriesTheCeilings(t *testing.T) {
	ceilings := accessCeilings{actionObjectRead: {limitMaxObjectSize: 7}}
	ctx := withAccessCeilings(context.Background(), ceilings)
	ceilings[actionObjectRead][limitMaxObjectSize] = 1000
	if limit, ok := accessCeiling(ctx, actionObjectRead); !ok || limit != 7 {
		t.Fatalf("ceiling = %d, %v", limit, ok)
	}
	if _, ok := accessCeiling(ctx, actionObjectWrite); ok {
		t.Fatal("a ceiling of another action applies")
	}
	if err := checkReadCeiling(ctx, 8); err == nil {
		t.Fatal("8 bytes pass a ceiling of 7")
	}
	if err := checkReadCeiling(ctx, 7); err != nil {
		t.Fatal(err)
	}
	// The lower value wins between the ceilings of the request and the ceilings of the context.
	request := &accessRequest{Subject: atSubject}
	request.recordCeiling(actionObjectRead, limitMaxObjectSize, 3)
	both := context.WithValue(ctx, accessRequestKey{}, request)
	if limit, _ := accessCeiling(both, actionObjectRead); limit != 3 {
		t.Fatalf("limit = %d, want 3", limit)
	}
	if merged := accessCeilingsOf(both); merged[actionObjectRead][limitMaxObjectSize] != 3 {
		t.Fatalf("merged = %v", merged)
	}
	if got := withAccessCeilings(context.Background(), nil); accessCeilingsOf(got) != nil {
		t.Fatal("an empty set gives a ceiling")
	}
}

func TestCeilingLargeCopyIsRefusedBeforeTheJobAndKeepsItsCeilingsAfter(t *testing.T) {
	threshold := multipartCopyThreshold
	multipartCopyThreshold = 1024
	t.Cleanup(func() { multipartCopyThreshold = threshold })
	root := volumeTempDir(t)
	content := strings.Repeat("0123456789abcdef", 256)
	writeVolumeFile(t, root, "big.bin", content)
	volume := volumeTestApplication(t, root, "")

	env := newAccessTestEnv(t)
	env.app.instances["vol"] = volume.instances["vol"]
	env.app.order = append(env.app.order, "vol")
	body := `{"instance":"vol","src":"big.bin","dst":"copy/big.bin"}`

	env.fake.SetAnswer(acAnswer(acNoCeiling, 1000))
	recorder := env.doToken(http.MethodPost, "/api/copy", body)
	acWantCeiling(t, recorder, actionObjectWrite, 1000)
	if _, err := volume.instances["vol"].Head(context.Background(), "copy/big.bin"); err == nil {
		t.Fatal("the copy ran")
	}

	env.fake.SetAnswer(acAnswer(1<<20, 1<<20))
	recorder = env.doToken(http.MethodPost, "/api/copy", body)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status %d, body %q", recorder.Code, recorder.Body.String())
	}
	var created publicJob
	if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	job := waitForJobStatus(t, env.app.jobs, created.ID, jobStatusCompleted)
	if job.Type != jobTypeCopyObject || job.Ceilings[actionObjectWrite][limitMaxObjectSize] != 1<<20 || job.Ceilings[actionObjectRead][limitMaxObjectSize] != 1<<20 {
		t.Fatalf("job = %+v", job)
	}
}

func TestCeilingJobWithItsOwnCeilingsRefusesAPrefixWithoutAPDPQuestion(t *testing.T) {
	env := newAccessTestEnv(t)
	acSeed(env, "folder/set/a.txt", 3)
	acSeed(env, "folder/set/z.txt", 30)
	job, err := env.app.jobs.create(jobState{
		Type: jobTypeCopyPrefix, Instance: "rw", Source: "folder/set/", Target: "copied/",
		Ceilings: accessCeilings{actionObjectWrite: {limitMaxObjectSize: 10}},
	})
	if err != nil {
		t.Fatal(err)
	}
	failed := waitForJobStatus(t, env.app.jobs, job.ID, jobStatusFailed)
	if !strings.Contains(failed.Error, "action object.write") || acCountPrefix(env, "copied/") != 0 {
		t.Fatalf("job error = %q", failed.Error)
	}
	if env.fake.Requests() != 0 {
		t.Fatalf("the PDP got %d requests", env.fake.Requests())
	}
}

func TestCeilingResumeKeepsTheLowerValue(t *testing.T) {
	env := newAccessTestEnv(t)
	job, err := env.app.jobs.create(jobState{
		Type: jobTypeStatsPrefix, Instance: "rw", Prefix: "folder/",
		Ceilings: accessCeilings{actionObjectRead: {limitMaxObjectSize: 50}},
	})
	if err != nil {
		t.Fatal(err)
	}
	env.app.jobs.lowerCeilings(job.ID, accessCeilings{actionObjectRead: {limitMaxObjectSize: 20}, actionObjectWrite: {limitMaxObjectSize: 9}})
	env.app.jobs.lowerCeilings(job.ID, accessCeilings{actionObjectRead: {limitMaxObjectSize: 60}})
	stored, _ := env.app.jobs.get(job.ID)
	if stored.Ceilings[actionObjectRead][limitMaxObjectSize] != 20 || stored.Ceilings[actionObjectWrite][limitMaxObjectSize] != 9 {
		t.Fatalf("ceilings = %v", stored.Ceilings)
	}
}

// ---- authorize and the request ----

func TestCeilingAuthorizeKeepsTheLowerValueInTheRequest(t *testing.T) {
	env := newAccessTestEnv(t)
	env.fake.SetAnswer(func(q fakeQuestion) map[string]any {
		size := 50
		if strings.HasSuffix(q.Path, "folder:b") {
			size = 5
		}
		return map[string]any{"decision": true, "context": map[string]any{"limits": map[string]any{limitMaxObjectSize: size}}}
	})
	request := env.request("rid-lower")
	if _, err := env.app.authorize(request, accessOperation{Use: env.ask(actionObjectRead, "a/x")}); err != nil {
		t.Fatal(err)
	}
	if limit, ok := accessCeiling(request.Context(), actionObjectRead); !ok || limit != 50 {
		t.Fatalf("limit = %d, %v", limit, ok)
	}
	if _, err := env.app.authorize(request, accessOperation{Use: env.ask(actionObjectRead, "b/y")}); err != nil {
		t.Fatal(err)
	}
	if limit, _ := accessCeiling(request.Context(), actionObjectRead); limit != 5 {
		t.Fatalf("limit = %d, want 5", limit)
	}
	if _, ok := accessCeiling(request.Context(), actionObjectWrite); ok {
		t.Fatal("a ceiling of object.write exists")
	}
}

func TestCeilingErrorTextOfTheGateway(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeGatewayError(recorder, errCeilingExceeded(actionObjectRead, 10, 12))
	if recorder.Code != http.StatusForbidden || strings.Contains(recorder.Body.String(), "{") {
		t.Fatalf("status %d, body %q", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	writeAPIError(recorder, errCeilingExceeded(actionObjectWrite, 10, -1))
	acWantCeiling(t, recorder, actionObjectWrite, 10)
}

// ---- open mode ----

func TestCeilingOpenModeHasNoCheck(t *testing.T) {
	app, _, backend := testApplication(t)
	backend.mu.Lock()
	backend.objects["tenant/folder/big.bin"] = memoryObject{data: bytes.Repeat([]byte("x"), 5000), contentType: "application/octet-stream", modified: time.Now().UTC()}
	backend.mu.Unlock()
	handler := app.routes()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/s3?instance=rw&key=folder%2Fbig.bin", nil))
	if recorder.Code != http.StatusOK || recorder.Body.Len() != 5000 {
		t.Fatalf("status %d, %d bytes", recorder.Code, recorder.Body.Len())
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/copy", strings.NewReader(`{"instance":"rw","src":"folder/big.bin","dst":"other/big.bin"}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("copy: status %d", recorder.Code)
	}
	backend.mu.Lock()
	heads := backend.headCount
	backend.mu.Unlock()
	if heads != 0 {
		t.Fatalf("open mode made %d HEAD requests for the check", heads)
	}
	if ceilingApplies(context.Background(), actionObjectRead, actionObjectWrite) {
		t.Fatal("a ceiling applies without a request")
	}
}

// ---- the document and the code ----

// acCeilingChecks is the table of the checks of this file. The key is the action and the ceiling name. The test of the
// document runs each check that the document needs, and it fails for a ceiling that the table does not have.
var acCeilingChecks = map[string]func(t *testing.T){
	actionObjectRead + "/" + limitMaxObjectSize: func(t *testing.T) {
		env := newAccessTestEnv(t)
		acSeedData(env, "folder/twelve.txt", []byte("hello world!"))
		env.fake.SetAnswer(acAnswer(10, acNoCeiling))
		if recorder := acGatewayGet(env, "folder%2Ftwelve.txt", nil); recorder.Code != http.StatusForbidden {
			t.Fatalf("status %d", recorder.Code)
		}
	},
	actionObjectWrite + "/" + limitMaxObjectSize: func(t *testing.T) {
		env := newAccessTestEnv(t)
		env.fake.SetAnswer(acAnswer(acNoCeiling, 10))
		request := httptest.NewRequest(http.MethodPut, "/s3?instance=rw&key=other%2Fnew.txt", strings.NewReader(strings.Repeat("y", 20)))
		request.ContentLength = 20
		request.Header.Set(defaultIdentityHeader, env.token())
		recorder := httptest.NewRecorder()
		env.handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusForbidden || acHas(env, "other/new.txt") {
			t.Fatalf("status %d", recorder.Code)
		}
	},
}

func TestCeilingsOfTheDocumentHaveACheck(t *testing.T) {
	env := newAccessTestEnv(t)
	var document documentBody
	if err := json.Unmarshal(env.app.accessDoc.json(), &document); err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, limit := range document.Limits {
		kinds[limit.Name] = limit.Kind
	}
	listed := 0
	for _, action := range document.Actions {
		for _, name := range action.Limits {
			if kinds[name] != "ceiling" {
				continue
			}
			listed++
			key := action.Name + "/" + name
			check, found := acCeilingChecks[key]
			if !found {
				t.Errorf("the document lists the ceiling %s on %s and this file has no check for it", name, action.Name)
				continue
			}
			t.Run(key, check)
		}
	}
	if listed == 0 {
		t.Fatal("the document lists no ceiling")
	}
	// The table has no check for a ceiling that the document does not list.
	for key := range acCeilingChecks {
		action, name, _ := strings.Cut(key, "/")
		if !env.app.accessDoc.listsCeiling(action, name) {
			t.Errorf("the table has the check %s, and the document does not list it", key)
		}
	}
}
