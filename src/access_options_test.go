package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// optionsService is the audience of the test documents: accessTestConfig sets it.
const optionsService = "stowr-test"

// optionsFixture is the options route with a fake PDP.
type optionsFixture struct {
	t    *testing.T
	fake *fakePDP
	src  accessOptionsSource
}

// optionsInstance gives a storage instance with the permissions read, write and delete.
func optionsInstance(id, name string, backend storageBackend, root string) *storageInstance {
	cfg := bucketConfig{
		ID: id, Name: name, AuthID: "test-auth", Provider: "s3", Bucket: id, Region: "test", RootPrefix: root,
		PermissionsDefined: true, Permissions: []string{permissionRead, permissionWrite, permissionDelete},
	}
	return &storageInstance{cfg: cfg, backend: backend, caps: initialCapabilities(cfg)}
}

// optionsBuckets gives n instances without a backend: the bucket children need no storage.
func optionsBuckets(n int) []*storageInstance {
	out := make([]*storageInstance, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("bucket%03d", i)
		out = append(out, optionsInstance(id, id, nil, ""))
	}
	return out
}

// newOptionsFixture builds the route for the instances. The action document publishes the five actions.
func newOptionsFixture(t *testing.T, instances []*storageInstance, mutate func(*accessConfig)) *optionsFixture {
	t.Helper()
	fake, cfg := newFakePDP(t)
	if mutate != nil {
		mutate(&cfg)
	}
	client := newPDPClient(cfg, false)
	t.Cleanup(client.close)
	var blocks []bucketConfig
	for _, instance := range instances {
		blocks = append(blocks, testBucket(instance.cfg.ID, instance.cfg.Name, nil))
	}
	doc, err := buildAccessDocument(accessTestConfig(blocks, nil))
	if err != nil {
		t.Fatal(err)
	}
	return &optionsFixture{t: t, fake: fake, src: accessOptionsSource{
		Service: optionsService, Doc: doc, PDP: client, Instances: instances, LogMode: logModeAnonymous,
	}}
}

// answerWhen makes the fake PDP say true to the questions for which the test function gives true.
func (f *optionsFixture) answerWhen(test func(fakeQuestion) bool) {
	f.fake.SetAnswer(func(q fakeQuestion) map[string]any { return map[string]any{"decision": test(q)} })
}

func (f *optionsFixture) get(rawQuery string) *httptest.ResponseRecorder {
	f.t.Helper()
	request := httptest.NewRequest(http.MethodGet, accessOptionsPath, nil)
	request.URL.RawQuery = rawQuery
	recorder := httptest.NewRecorder()
	serveAccessOptions(recorder, request, f.src, "alice", "req-1")
	return recorder
}

// page reads an answer of status 200.
func (f *optionsFixture) page(rawQuery string) ([]optionsItem, *string) {
	f.t.Helper()
	recorder := f.get(rawQuery)
	if recorder.Code != http.StatusOK {
		f.t.Fatalf("GET ?%s: status = %d, body = %s", rawQuery, recorder.Code, recorder.Body.String())
	}
	return decodeOptionsAnswer(f.t, recorder)
}

func decodeOptionsAnswer(t *testing.T, recorder *httptest.ResponseRecorder) ([]optionsItem, *string) {
	t.Helper()
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &raw); err != nil {
		t.Fatalf("body is not JSON: %v: %s", err, recorder.Body.String())
	}
	if len(raw) != 2 || raw["items"] == nil || raw["next"] == nil {
		t.Fatalf("members = %v, want items and next", raw)
	}
	if string(raw["items"]) == "null" {
		t.Fatalf("items is null")
	}
	var answer optionsAnswer
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	return answer.Items, answer.Next
}

func optionIDs(items []optionsItem) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

func optionActions(questions []fakeQuestion) []string {
	actions := make([]string, 0, len(questions))
	for _, question := range questions {
		actions = append(actions, question.Action)
	}
	return actions
}

// foldersBackend gives a memory backend with the folders f000/x to f<count-1>/x.
func foldersBackend(count int) *memoryBackend {
	objects := make(map[string]string, count)
	for i := 0; i < count; i++ {
		objects[fmt.Sprintf("f%03d/x", i)] = "x"
	}
	return newMemoryBackend(objects)
}

func TestOptionsU05AskSubtreeQuestionsWithoutUsage(t *testing.T) {
	f := newOptionsFixture(t, optionsBuckets(3), nil)
	items, _ := f.page("type=bucket")
	if len(items) != 3 {
		t.Fatalf("items = %v", items)
	}
	questions := f.fake.Questions()
	if len(questions) == 0 {
		t.Fatal("the route asked no question")
	}
	for _, q := range questions {
		if q.Match != "subtree" || q.Usage {
			t.Fatalf("question = %+v, want match subtree and no usage", q)
		}
	}
}

func TestOptionsU70QuestionHasTheTypeOfTheChildAndTheChainOfTheService(t *testing.T) {
	f := newOptionsFixture(t, optionsBuckets(2), nil)
	f.page("type=bucket")
	questions := f.fake.Questions()
	if len(questions) != 2 {
		t.Fatalf("questions = %+v", questions)
	}
	for _, q := range questions {
		if q.ResourceType != "bucket" || q.Match != "subtree" ||
			!reflect.DeepEqual(q.Ancestors, []accessNode{{Type: "service", ID: optionsService}}) {
			t.Fatalf("question = %+v", q)
		}
	}
}

func TestOptionsU71NoChildThatThePDPRefuses(t *testing.T) {
	f := newOptionsFixture(t, optionsBuckets(3), nil)
	f.answerWhen(func(fakeQuestion) bool { return false })
	items, next := f.page("type=bucket")
	if len(items) != 0 || next != nil {
		t.Fatalf("items = %v, next = %v", items, next)
	}
	// Each child got each action of its type: five for a bucket.
	if got := len(f.fake.Questions()); got != 15 {
		t.Fatalf("questions = %d, want 15", got)
	}
}

func TestOptionsU72ChildrenHaveIDLabelAndHasChildren(t *testing.T) {
	f := newOptionsFixture(t, []*storageInstance{optionsInstance("docs", "Documents", nil, "")}, nil)
	recorder := f.get("type=bucket")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	var raw struct {
		Items []map[string]any `json:"items"`
		Next  any              `json:"next"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.Items) != 1 || len(raw.Items[0]) != 3 || raw.Items[0]["id"] != "docs" || raw.Items[0]["label"] != "Documents" || raw.Items[0]["has_children"] != true || raw.Next != nil {
		t.Fatalf("answer = %s", recorder.Body.String())
	}
}

func TestOptionsU73StaysWithinTheNumbersOfAProcurAuthCaller(t *testing.T) {
	var instances []*storageInstance
	long := strings.Repeat("é", 100)
	for i := 0; i < 70; i++ {
		id := fmt.Sprintf("%03d-%s", i, strings.Repeat("x", 252))
		instances = append(instances, optionsInstance(id, long, nil, ""))
	}
	f := newOptionsFixture(t, instances, nil)
	recorder := f.get("type=bucket&limit=999")
	items, next := decodeOptionsAnswer(t, recorder)
	if len(items) != 50 || next == nil {
		t.Fatalf("items = %d, next = %v", len(items), next)
	}
	if recorder.Body.Len() > 64<<10 {
		t.Fatalf("answer has %d bytes", recorder.Body.Len())
	}
	for _, item := range items {
		if len(item.ID) != 256 || utf8.RuneCountInString(item.Label) > 60 {
			t.Fatalf("item id = %d bytes, label = %d characters", len(item.ID), utf8.RuneCountInString(item.Label))
		}
	}
}

func TestOptionsU74PDPFailureGives503(t *testing.T) {
	cases := map[string]func(*fakePDP){
		"HTTP 500":      func(p *fakePDP) { p.SetStatus(500) },
		"not a boolean": func(p *fakePDP) { p.SetRaw([]byte(`{"decision":"yes"}`)) },
		"no answer":     func(p *fakePDP) { p.SetDelay(3 * time.Second) },
	}
	for name, inject := range cases {
		t.Run(name, func(t *testing.T) {
			f := newOptionsFixture(t, optionsBuckets(3), func(cfg *accessConfig) { cfg.PDPTimeout = 150 * time.Millisecond })
			inject(f.fake)
			recorder := f.get("type=bucket")
			if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), `"pdp_unavailable"`) {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestOptionsAFailedAnswerForFolderChildrenGives503(t *testing.T) {
	f := newOptionsFixture(t, []*storageInstance{optionsInstance("b", "b", foldersBackend(3), "")}, nil)
	f.fake.SetStatus(500)
	recorder := f.get("type=folder&parent=bucket%3Ab")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestOptionsU76TypeIsMandatory(t *testing.T) {
	f := newOptionsFixture(t, optionsBuckets(1), nil)
	for _, query := range []string{"", "limit=5", "type=", "parent=bucket%3Ab"} {
		if recorder := f.get(query); recorder.Code != http.StatusBadRequest {
			t.Fatalf("?%s: status = %d", query, recorder.Code)
		}
	}
	if got := len(f.fake.Questions()); got != 0 {
		t.Fatalf("questions = %d", got)
	}
}

func TestOptionsU77AParameterThatOccursTwoTimesGives400(t *testing.T) {
	f := newOptionsFixture(t, optionsBuckets(1), nil)
	for _, query := range []string{"type=bucket&type=bucket", "type=bucket&limit=2&limit=2", "type=bucket&q=a&q=b", "type=bucket&%74ype=bucket",
		"type=folder&parent=bucket%3Ab&parent=bucket%3Ab", "type=bucket&after=YQ&after=YQ"} {
		if recorder := f.get(query); recorder.Code != http.StatusBadRequest {
			t.Fatalf("?%s: status = %d", query, recorder.Code)
		}
	}
}

func TestOptionsU78LimitIsAWholeNumberFromOne(t *testing.T) {
	f := newOptionsFixture(t, optionsBuckets(60), nil)
	for _, limit := range []string{"0", "-1", "+5", "5x", "abc", "1.5", "", "00", " 5", "0x5"} {
		if recorder := f.get("type=bucket&limit=" + limit); recorder.Code != http.StatusBadRequest {
			t.Fatalf("limit=%q: status = %d", limit, recorder.Code)
		}
	}
	for limit, want := range map[string]int{"1": 1, "5": 5, "007": 7, "50": 50, "51": 50, "99": 50, "99999999999999999999999": 50} {
		items, _ := f.page("type=bucket&limit=" + limit)
		if len(items) != want {
			t.Fatalf("limit=%s: items = %d, want %d", limit, len(items), want)
		}
	}
}

func TestOptionsU79TypeMustBeTheChildTypeOfTheParent(t *testing.T) {
	f := newOptionsFixture(t, optionsBuckets(1), nil)
	for _, query := range []string{
		"parent=bucket%3Ax&type=bucket",
		"type=folder",
		"type=subfolder",
		"type=other",
		"parent=bucket%3Ax&type=subfolder",
		"parent=bucket%3Ax%2Ffolder%3Ay&type=folder",
		"parent=bucket%3Ax%2Ffolder%3Ay%2Fsubfolder%3Az&type=subfolder",
		"parent=bucket%3Ax%2Ffolder%3Ay&type=bucket",
	} {
		if recorder := f.get(query); recorder.Code != http.StatusBadRequest {
			t.Fatalf("?%s: status = %d", query, recorder.Code)
		}
	}
	if got := len(f.fake.Questions()); got != 0 {
		t.Fatalf("questions = %d", got)
	}
}

func TestOptionsU80BadEscapeGives400(t *testing.T) {
	f := newOptionsFixture(t, optionsBuckets(1), nil)
	for _, query := range []string{"parent=%zz&type=bucket", "type=bucket%", "type=bucket&q=%4", "type=bucket&limit=%3", "type=folder&parent=bucket%3Aa%25zz",
		"type=bucket&after=%zz"} {
		if recorder := f.get(query); recorder.Code != http.StatusBadRequest {
			t.Fatalf("?%s: status = %d", query, recorder.Code)
		}
	}
}

func TestOptionsU81UnknownParameterIsIgnored(t *testing.T) {
	f := newOptionsFixture(t, optionsBuckets(2), nil)
	items, _ := f.page("type=bucket&x_unknown=1&x_unknown=2&x_bad=%zz&&")
	if len(items) != 2 {
		t.Fatalf("items = %v", items)
	}
}

func TestOptionsU82NoMoreItemsThanTheLimit(t *testing.T) {
	f := newOptionsFixture(t, optionsBuckets(3), nil)
	items, next := f.page("type=bucket&limit=1")
	if len(items) != 1 || next == nil {
		t.Fatalf("items = %v, next = %v", items, next)
	}
}

func TestOptionsU83OnlyReadQuestionsWithoutUsageWhenTheReadIsTrue(t *testing.T) {
	f := newOptionsFixture(t, optionsBuckets(4), nil)
	f.page("type=bucket")
	for _, q := range f.fake.Questions() {
		if q.Usage || q.Match != "subtree" || q.Action != actionObjectRead {
			t.Fatalf("question = %+v", q)
		}
	}
	if got := len(f.fake.Questions()); got != 4 {
		t.Fatalf("questions = %d, want 4: the read is true, so no other action is asked", got)
	}
	if got := f.fake.Requests(); got != 1 {
		t.Fatalf("requests to the PDP = %d, want 1: one batch for the page", got)
	}
}

func TestOptionsAskTheNextActionsOnlyForTheChildrenStillFalse(t *testing.T) {
	f := newOptionsFixture(t, []*storageInstance{optionsInstance("b1", "b1", nil, ""), optionsInstance("b2", "b2", nil, "")}, nil)
	f.answerWhen(func(q fakeQuestion) bool {
		return q.ResourceID == "b2" && q.Action == actionObjectRead || q.ResourceID == "b1" && q.Action == actionBucketAdmin
	})
	items, _ := f.page("type=bucket")
	if want := []string{"b1", "b2"}; !reflect.DeepEqual(optionIDs(items), want) {
		t.Fatalf("items = %v", optionIDs(items))
	}
	var forB1, forB2 []string
	for _, q := range f.fake.Questions() {
		if q.ResourceID == "b1" {
			forB1 = append(forB1, q.Action)
		} else {
			forB2 = append(forB2, q.Action)
		}
	}
	if want := []string{"object.read", "object.write", "object.delete", "object.lock", "bucket.admin"}; !reflect.DeepEqual(forB1, want) {
		t.Fatalf("questions for b1 = %v", forB1)
	}
	if want := []string{"object.read"}; !reflect.DeepEqual(forB2, want) {
		t.Fatalf("questions for b2 = %v", forB2)
	}
	// The first batch holds the read of both buckets. Each later batch holds only b1.
	if got := f.fake.Requests(); got != 5 {
		t.Fatalf("requests = %d, want 5", got)
	}
}

func TestOptionsFolderChildrenNeverAskBucketAdmin(t *testing.T) {
	f := newOptionsFixture(t, []*storageInstance{optionsInstance("b", "b", foldersBackend(2), "")}, nil)
	f.answerWhen(func(q fakeQuestion) bool { return q.ResourceID == "f001" && q.Action == actionObjectWrite })
	items, _ := f.page("type=folder&parent=bucket%3Ab")
	if want := []string{"f001"}; !reflect.DeepEqual(optionIDs(items), want) {
		t.Fatalf("items = %v", optionIDs(items))
	}
	for _, q := range f.fake.Questions() {
		if q.Action == actionBucketAdmin {
			t.Fatalf("question = %+v", q)
		}
	}
	var forF001 []string
	for _, q := range f.fake.Questions() {
		if q.ResourceID == "f001" {
			forF001 = append(forF001, q.Action)
		}
	}
	if want := []string{"object.read", "object.write"}; !reflect.DeepEqual(forF001, want) {
		t.Fatalf("questions for f001 = %v", forF001)
	}
}

func TestOptionsAskOnlyThePublishedActions(t *testing.T) {
	f := newOptionsFixture(t, optionsBuckets(2), nil)
	doc, err := buildAccessDocument(accessTestConfig([]bucketConfig{testBucket("bucket000", "bucket000", []string{permissionRead}), testBucket("bucket001", "", []string{permissionRead})}, nil))
	if err != nil {
		t.Fatal(err)
	}
	f.src.Doc = doc
	f.answerWhen(func(fakeQuestion) bool { return false })
	items, _ := f.page("type=bucket")
	if len(items) != 0 {
		t.Fatalf("items = %v", items)
	}
	for _, q := range f.fake.Questions() {
		if q.Action != actionObjectRead {
			t.Fatalf("question = %+v: the document publishes only object.read", q)
		}
	}
	if got := len(f.fake.Questions()); got != 2 {
		t.Fatalf("questions = %d", got)
	}
}

func TestOptionsBucketsAreSortedAndSearchedWithoutCase(t *testing.T) {
	instances := []*storageInstance{
		optionsInstance("b", "Beta", nil, ""), optionsInstance("a", "a", nil, ""), optionsInstance("B", "Gamma files", nil, ""),
		optionsInstance("A", "", nil, ""), optionsInstance("c1", "Shared"+strings.Repeat("x", 60), nil, ""), optionsInstance("c2", "bad\nname", nil, ""),
	}
	f := newOptionsFixture(t, instances, nil)
	items, _ := f.page("type=bucket")
	if want := []string{"A", "a", "B", "b", "c1", "c2"}; !reflect.DeepEqual(optionIDs(items), want) {
		t.Fatalf("order = %v, want %v", optionIDs(items), want)
	}
	labels := map[string]string{}
	for _, item := range items {
		labels[item.ID] = item.Label
		if !item.HasChildren {
			t.Fatalf("item = %+v", item)
		}
	}
	// The label is the name, or the id when the name is empty, too long or has a control character.
	if labels["b"] != "Beta" || labels["A"] != "A" || labels["c1"] != "c1" || labels["c2"] != "c2" {
		t.Fatalf("labels = %v", labels)
	}
	for query, want := range map[string][]string{
		"q=GAMMA":    {"B"},
		"q=b":        {"B", "b"},
		"q=BET":      {"b"},
		"q=a":        {"A", "B", "a", "b"},
		"q=nothing":  {},
		"q=%20files": {"B"},
	} {
		items, _ := f.page("type=bucket&" + query)
		got := optionIDs(items)
		sort.Strings(got)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("?%s: items = %v, want %v", query, got, want)
		}
	}
}

func TestOptionsBucketPagesFollowTheCursor(t *testing.T) {
	f := newOptionsFixture(t, optionsBuckets(5), nil)
	var ids []string
	cursor := ""
	for pages := 0; pages < 10; pages++ {
		query := "type=bucket&limit=2"
		if cursor != "" {
			query += "&after=" + cursor
		}
		items, next := f.page(query)
		ids = append(ids, optionIDs(items)...)
		if next == nil {
			break
		}
		cursor = *next
	}
	want := []string{"bucket000", "bucket001", "bucket002", "bucket003", "bucket004"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids = %v", ids)
	}
}

func TestOptionsAFolderPageWith120FoldersGivesPagesOf50And50And20(t *testing.T) {
	f := newOptionsFixture(t, []*storageInstance{optionsInstance("b", "b", foldersBackend(120), "")}, nil)
	seen := map[string]bool{}
	var sizes []int
	cursor := ""
	for pages := 0; pages < 10; pages++ {
		f.fake.Reset()
		query := "type=folder&parent=bucket%3Ab"
		if cursor != "" {
			query += "&after=" + cursor
		}
		items, next := f.page(query)
		sizes = append(sizes, len(items))
		for _, item := range items {
			if seen[item.ID] {
				t.Fatalf("%s is in two pages", item.ID)
			}
			seen[item.ID] = true
			if !item.HasChildren || item.Label != item.ID {
				t.Fatalf("item = %+v", item)
			}
		}
		if got := len(f.fake.Questions()); got != len(items) {
			t.Fatalf("page %d: %d questions for %d items", pages, got, len(items))
		}
		if next == nil {
			break
		}
		cursor = *next
	}
	if !reflect.DeepEqual(sizes, []int{50, 50, 20}) || len(seen) != 120 {
		t.Fatalf("sizes = %v, folders = %d", sizes, len(seen))
	}
	if !seen["f000"] || !seen["f119"] {
		t.Fatalf("seen = %v", seen)
	}
}

func TestOptionsTheBoundOf200QuestionsGivesAnEmptyPageWithACursor(t *testing.T) {
	backend := foldersBackend(120)
	f := newOptionsFixture(t, []*storageInstance{optionsInstance("b", "b", backend, "")}, nil)
	f.answerWhen(func(q fakeQuestion) bool { return q.ResourceID == "f060" && q.Action == actionObjectDelete })
	first := ""
	var found []string
	var counts []int
	cursor := ""
	for pages := 0; pages < 10; pages++ {
		f.fake.Reset()
		query := "type=folder&parent=bucket%3Ab"
		if cursor != "" {
			query += "&after=" + cursor
		}
		items, next := f.page(query)
		counts = append(counts, len(f.fake.Questions()))
		if len(f.fake.Questions()) > 200 {
			t.Fatalf("page %d: %d questions", pages, len(f.fake.Questions()))
		}
		if pages == 0 {
			// 50 folders, four actions each: 200 questions, no item, and a cursor.
			if len(items) != 0 || next == nil || first != "" {
				t.Fatalf("first page: items = %v, next = %v", items, next)
			}
			first = *next
		}
		found = append(found, optionIDs(items)...)
		if next == nil {
			break
		}
		cursor = *next
	}
	if !reflect.DeepEqual(counts, []int{200, 199, 80}) {
		t.Fatalf("counts = %v", counts)
	}
	if !reflect.DeepEqual(found, []string{"f060"}) {
		t.Fatalf("found = %v", found)
	}
	// The first cursor continues after the folder f049.
	if payload, ok := decodeOptionsCursor(first, "folder"); !ok || payload != "f0490" {
		t.Fatalf("cursor payload = %q, %v", payload, ok)
	}
}

func TestOptionsTheBoundStopsABucketWalkToo(t *testing.T) {
	f := newOptionsFixture(t, optionsBuckets(60), nil)
	f.answerWhen(func(fakeQuestion) bool { return false })
	items, next := f.page("type=bucket")
	// Five actions: 40 buckets use the 200 questions.
	if len(items) != 0 || next == nil || len(f.fake.Questions()) != 200 {
		t.Fatalf("items = %v, next = %v, questions = %d", items, next, len(f.fake.Questions()))
	}
	if payload, ok := decodeOptionsCursor(*next, "bucket"); !ok || payload != "bucket039" {
		t.Fatalf("payload = %q, %v", payload, ok)
	}
}

func TestOptionsFolderSearchIsAPrefixOfTheID(t *testing.T) {
	backend := newMemoryBackend(map[string]string{"alpha/x": "x", "alps/x": "x", "beta/x": "x", "xalpha/x": "x", "alpha.txt": "x"})
	f := newOptionsFixture(t, []*storageInstance{optionsInstance("b", "b", backend, "")}, nil)
	items, _ := f.page("type=folder&parent=bucket%3Ab&q=alp")
	if want := []string{"alpha", "alps"}; !reflect.DeepEqual(optionIDs(items), want) {
		t.Fatalf("items = %v", optionIDs(items))
	}
	if backend.last.Prefix != "alp" || backend.last.Delimiter != "/" {
		t.Fatalf("listing = %+v", backend.last)
	}
	for _, query := range []string{"q=lph", "q=alpha%2Fx", "q=zzz"} {
		items, next := f.page("type=folder&parent=bucket%3Ab&" + query)
		if len(items) != 0 || next != nil {
			t.Fatalf("?%s: items = %v, next = %v", query, items, next)
		}
	}
}

func TestOptionsSubfoldersUseTheRootPrefixAndTheChainOfTheParent(t *testing.T) {
	backend := newMemoryBackend(map[string]string{
		"root/reports/2026/q1.csv": "x", "root/reports/2027/x": "x", "root/reports/a.txt": "x", "root/other/9/x": "x", "outside/reports/2099/x": "x",
	})
	f := newOptionsFixture(t, []*storageInstance{optionsInstance("b", "b", backend, "root/")}, nil)
	items, _ := f.page("type=subfolder&parent=bucket%3Ab%2Ffolder%3Areports")
	if want := []string{"2026", "2027"}; !reflect.DeepEqual(optionIDs(items), want) {
		t.Fatalf("items = %v", optionIDs(items))
	}
	for _, item := range items {
		if item.HasChildren {
			t.Fatalf("a subfolder has no children: %+v", item)
		}
	}
	if backend.last.Prefix != "root/reports/" {
		t.Fatalf("prefix = %q", backend.last.Prefix)
	}
	q := f.fake.Questions()[0]
	if q.ResourceType != "subfolder" || q.ResourceID != "2026" || q.Path != "service:stowr-test/bucket:b/folder:reports/subfolder:2026" || q.Match != "subtree" {
		t.Fatalf("question = %+v", q)
	}
	folders, _ := f.page("type=folder&parent=bucket%3Ab")
	if want := []string{"other", "reports"}; !reflect.DeepEqual(optionIDs(folders), want) {
		t.Fatalf("folders = %v", optionIDs(folders))
	}
	for _, item := range folders {
		if !item.HasChildren {
			t.Fatalf("a folder can have children: %+v", item)
		}
	}
}

func TestOptionsAnUnknownParentGivesNoItems(t *testing.T) {
	backend := newMemoryBackend(map[string]string{"a/x": "x"})
	f := newOptionsFixture(t, []*storageInstance{optionsInstance("b", "b", backend, "")}, nil)
	for _, query := range []string{
		"type=folder&parent=bucket%3Aunknown",
		"type=subfolder&parent=bucket%3Ab%2Ffolder%3Anope",
		"type=subfolder&parent=bucket%3Ab%2Ffolder%3Aa%252Fb",
	} {
		items, next := f.page(query)
		if len(items) != 0 || next != nil {
			t.Fatalf("?%s: items = %v, next = %v", query, items, next)
		}
	}
	if recorder := f.get("type=folder&parent=bucket%3Ab%2F"); recorder.Code != http.StatusBadRequest {
		t.Fatalf("a parent with an empty node: status = %d", recorder.Code)
	}
	if got := len(f.fake.Questions()); got != 0 {
		t.Fatalf("questions = %d: a node that does not exist needs no question", got)
	}
}

func TestOptionsTheParentIsDecodedTwoTimes(t *testing.T) {
	req, err := parseOptionsQuery("parent=bucket%3Aa%252Fb&type=folder")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(req.parent, []accessNode{{Type: "bucket", ID: "a/b"}}) || req.missingParent || req.childType != "folder" {
		t.Fatalf("request = %+v", req)
	}
	req, err = parseOptionsQuery("parent=bucket%3Ab%2Ffolder%3Ax%2By%252Fz&type=subfolder&q=a+b&limit=3")
	if err != nil {
		t.Fatal(err)
	}
	want := []accessNode{{Type: "bucket", ID: "b"}, {Type: "folder", ID: "x+y/z"}}
	if !reflect.DeepEqual(req.parent, want) || req.search != "a+b" || req.limit != 3 || !req.missingParent {
		t.Fatalf("request = %+v", req)
	}
	// The id of an instance can hold "/": the route finds it.
	backend := newMemoryBackend(map[string]string{"f1/x": "x"})
	f := newOptionsFixture(t, []*storageInstance{optionsInstance("a/b", "a/b", backend, "")}, nil)
	items, _ := f.page("type=folder&parent=bucket%3Aa%252Fb")
	if !reflect.DeepEqual(optionIDs(items), []string{"f1"}) {
		t.Fatalf("items = %v", items)
	}
	if got := f.fake.Questions()[0].Path; got != "service:stowr-test/bucket:a%2Fb/folder:f1" {
		t.Fatalf("path = %q", got)
	}
}

func TestOptionsBadCursorsGive400(t *testing.T) {
	f := newOptionsFixture(t, []*storageInstance{optionsInstance("b", "b", foldersBackend(3), "")}, nil)
	_, next := f.page("type=folder&parent=bucket%3Ab&limit=1")
	if next == nil {
		t.Fatal("no cursor")
	}
	for _, query := range []string{
		"type=bucket&after=" + *next, // a cursor of the folders
		"type=bucket&after=!!!",
		"type=bucket&after=",
		"type=bucket&after=YQ",                           // "a": no type
		"type=folder&parent=bucket%3Ab&after=Zm9sZGVyAA", // "folder" and an empty payload
	} {
		if recorder := f.get(query); recorder.Code != http.StatusBadRequest {
			t.Fatalf("?%s: status = %d", query, recorder.Code)
		}
	}
}

func TestOptionsAnInstanceThatDoesNotAllowReadHasNoFolders(t *testing.T) {
	backend := foldersBackend(3)
	instance := optionsInstance("b", "b", backend, "")
	instance.cfg.Permissions = []string{permissionWrite}
	instance.caps = initialCapabilities(instance.cfg)
	f := newOptionsFixture(t, []*storageInstance{instance}, nil)
	items, next := f.page("type=folder&parent=bucket%3Ab")
	if len(items) != 0 || next != nil || backend.listCalls != 0 || len(f.fake.Questions()) != 0 {
		t.Fatalf("items = %v, next = %v, list calls = %d, questions = %d", items, next, backend.listCalls, len(f.fake.Questions()))
	}
}

func TestOptionsTheScanOfTheStorageIsBoundedByMaxScanPages(t *testing.T) {
	backend := newMemoryBackend(nil)
	backend.listPages = []listPage{
		{Prefixes: []string{"f1/", "f2/"}, NextPageToken: "page-1"},
		{Prefixes: []string{"f3/"}, Objects: []objectInfo{{Key: "f3a"}}, NextPageToken: "page-2"},
		{Prefixes: []string{"f4/"}},
	}
	instance := optionsInstance("b", "b", backend, "")
	instance.cfg.MaxScanPages = 2
	f := newOptionsFixture(t, []*storageInstance{instance}, nil)
	items, next := f.page("type=folder&parent=bucket%3Ab")
	if want := []string{"f1", "f2", "f3"}; !reflect.DeepEqual(optionIDs(items), want) {
		t.Fatalf("items = %v", optionIDs(items))
	}
	if backend.listCalls != 2 || next == nil {
		t.Fatalf("list calls = %d, next = %v", backend.listCalls, next)
	}
	if payload, _ := decodeOptionsCursor(*next, "folder"); payload != "f3a" {
		t.Fatalf("payload = %q", payload)
	}

	// A page of objects only gives no folder: the cursor still moves forward.
	backend = newMemoryBackend(nil)
	backend.listPages = []listPage{
		{Objects: []objectInfo{{Key: "o1"}, {Key: "o2"}}, NextPageToken: "page-1"},
		{Prefixes: []string{"z/"}},
	}
	instance = optionsInstance("b", "b", backend, "")
	f = newOptionsFixture(t, []*storageInstance{instance}, nil)
	items, next = f.page("type=folder&parent=bucket%3Ab")
	if len(items) != 0 || next == nil {
		t.Fatalf("items = %v, next = %v", items, next)
	}
	if payload, _ := decodeOptionsCursor(*next, "folder"); payload != "o2" {
		t.Fatalf("payload = %q", payload)
	}
}

func TestOptionsAVolumeContinuesAfterTheCursor(t *testing.T) {
	root := volumeTree(t)
	app := volumeTestApplication(t, root, "")
	instance, _ := volumeInstance(t, app)
	f := newOptionsFixture(t, []*storageInstance{instance}, nil)
	var ids []string
	cursor := ""
	for pages := 0; pages < 10; pages++ {
		query := "type=folder&parent=bucket%3Avol&limit=1"
		if cursor != "" {
			query += "&after=" + cursor
		}
		items, next := f.page(query)
		ids = append(ids, optionIDs(items)...)
		if next == nil {
			break
		}
		cursor = *next
	}
	if want := []string{"a-b", "a", "empty", "link-in"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
}

func TestOptionsOtherMethodsGive405AndHeadGivesNoBody(t *testing.T) {
	f := newOptionsFixture(t, optionsBuckets(1), nil)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		request := httptest.NewRequest(method, accessOptionsPath+"?type=bucket", nil)
		recorder := httptest.NewRecorder()
		serveAccessOptions(recorder, request, f.src, "alice", "req-1")
		if recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != "GET, HEAD" {
			t.Fatalf("%s: status = %d, Allow = %q", method, recorder.Code, recorder.Header().Get("Allow"))
		}
	}
	if got := len(f.fake.Questions()); got != 0 {
		t.Fatalf("questions = %d", got)
	}
	request := httptest.NewRequest(http.MethodHead, accessOptionsPath+"?type=bucket", nil)
	recorder := httptest.NewRecorder()
	serveAccessOptions(recorder, request, f.src, "alice", "req-1")
	if recorder.Code != http.StatusOK {
		t.Fatalf("HEAD: status = %d", recorder.Code)
	}
}

func TestOptionsLogLineHasTheCounts(t *testing.T) {
	var out bytes.Buffer
	original := log.Writer()
	log.SetOutput(&out)
	t.Cleanup(func() { log.SetOutput(original) })

	f := newOptionsFixture(t, []*storageInstance{optionsInstance("b", "b", foldersBackend(3), "")}, nil)
	f.page("type=folder&parent=bucket%3Ab")
	line := out.String()
	if !strings.Contains(line, `access.options request_id=req-1 subject="alice" type=folder parent="bucket:b" items=3 questions=3`) {
		t.Fatalf("log = %q", line)
	}
	// The mode anonymous keeps the ids of the folders out of the log.
	out.Reset()
	f.fake.Reset()
	f.page("type=subfolder&parent=bucket%3Ab%2Ffolder%3Asecret")
	if line := out.String(); strings.Contains(line, "secret") || !strings.Contains(line, `parent="bucket:b/folder:-"`) {
		t.Fatalf("log = %q", line)
	}
	out.Reset()
	f.src.LogMode = logModeDetailed
	f.page("type=subfolder&parent=bucket%3Ab%2Ffolder%3Asecret")
	if line := out.String(); !strings.Contains(line, `parent="bucket:b/folder:secret"`) {
		t.Fatalf("log = %q", line)
	}
}
