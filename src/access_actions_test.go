package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// accessTestConfig gives a configuration in mode "authzen" with the audience "stowr-test".
func accessTestConfig(buckets []bucketConfig, volumes []volumeConfig) appConfig {
	return appConfig{
		Access:  accessConfig{Mode: accessModeAuthZEN, Audience: "stowr-test"},
		Buckets: buckets,
		Volumes: volumes,
	}
}

// testBucket gives a bucket block. A nil list of permissions means that the block has no permissions key.
func testBucket(id, name string, permissions []string) bucketConfig {
	return bucketConfig{ID: id, Name: name, Provider: "s3", Bucket: id, Permissions: permissions, PermissionsDefined: permissions != nil}
}

// testVolume gives a volume block. A nil list of permissions means that the block has no permissions key.
func testVolume(id, name string, permissions []string) volumeConfig {
	return volumeConfig{ID: id, Name: name, Path: "/data/" + id, Permissions: permissions, PermissionsDefined: permissions != nil}
}

var (
	lintTypeName   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	lintActionName = regexp.MustCompile(`^[a-z0-9_]+(\.[a-z0-9_]+)*$`)
	lintEncodedID  = regexp.MustCompile(`^(?:[A-Za-z0-9._~-]|%[0-9A-F]{2})+$`)
)

// lintAccessDocument is a Go copy of the rules of access-lint (ProcurAuth 5.2, 5.3c and 6.1). It gives one text for
// each broken rule. An empty list means that the document is good.
func lintAccessDocument(raw []byte) []string {
	var problems []string
	fail := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	runes := utf8.RuneCountInString

	if len(raw) > 65536 {
		fail("document has %d bytes, the most is 65536", len(raw))
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var doc map[string]json.RawMessage
	if err := decoder.Decode(&doc); err != nil {
		return append(problems, "bad_json: "+err.Error())
	}
	for name := range doc {
		switch name {
		case "server", "resource_types", "actions", "resources", "limits":
		default:
			fail("unknown member %q", name)
		}
	}
	decodeMember := func(name string, out any) {
		value, ok := doc[name]
		if !ok {
			if name == "server" || name == "resource_types" || name == "actions" {
				fail("member %q is absent", name)
			}
			return
		}
		inner := json.NewDecoder(bytes.NewReader(value))
		inner.UseNumber()
		inner.DisallowUnknownFields()
		if err := inner.Decode(out); err != nil {
			fail("member %q: %v", name, err)
		}
	}

	var server string
	decodeMember("server", &server)
	if server == "" || len(server) > 256 || hasControlCharacter(server) {
		fail("bad_server: %q", server)
	}

	var types []struct {
		Name        string `json:"name"`
		Parent      string `json:"parent"`
		Description string `json:"description"`
	}
	decodeMember("resource_types", &types)
	parents := map[string]string{}
	for _, item := range types {
		switch {
		case item.Name == "service":
			fail("reserved_type_name")
		case !lintTypeName.MatchString(item.Name):
			fail("bad_type_name %q", item.Name)
		}
		if _, dup := parents[item.Name]; dup {
			fail("duplicate_type_name %q", item.Name)
		}
		parents[item.Name] = item.Parent
		if runes(item.Description) > 300 {
			fail("description of type %q is longer than 300 characters", item.Name)
		}
	}
	depth := map[string]int{"service": 0}
	for _, item := range types {
		if item.Parent != "service" {
			if _, ok := parents[item.Parent]; !ok {
				fail("undeclared_parent %q of %q", item.Parent, item.Name)
			}
		}
		steps, current := 0, item.Name
		for current != "service" {
			next, ok := parents[current]
			if !ok {
				break
			}
			steps++
			current = next
			if steps > len(types)+1 {
				fail("type_cycle at %q", item.Name)
				break
			}
		}
		depth[item.Name] = steps
		if steps > 4 {
			fail("type_too_deep %q", item.Name)
		}
	}
	if len(types) > 16 {
		fail("%d types, the most is 16", len(types))
	}

	var limits []struct {
		Name        string       `json:"name"`
		Label       string       `json:"label"`
		Description string       `json:"description"`
		Unit        string       `json:"unit"`
		Kind        string       `json:"kind"`
		Min         *json.Number `json:"min"`
		Max         *json.Number `json:"max"`
	}
	decodeMember("limits", &limits)
	kinds := map[string]string{}
	const two53 = int64(1) << 53
	for _, limit := range limits {
		if !lintTypeName.MatchString(limit.Name) {
			fail("bad_limit_name %q", limit.Name)
		}
		if _, dup := kinds[limit.Name]; dup {
			fail("duplicate_limit_name %q", limit.Name)
		}
		kinds[limit.Name] = limit.Kind
		if limit.Label == "" || runes(limit.Label) > 60 {
			fail("label of limit %q has %d characters", limit.Name, runes(limit.Label))
		}
		if limit.Description == "" || runes(limit.Description) > 300 {
			fail("description of limit %q has %d characters", limit.Name, runes(limit.Description))
		}
		if limit.Unit == "" || runes(limit.Unit) > 20 {
			fail("unit of limit %q has %d characters", limit.Name, runes(limit.Unit))
		}
		switch limit.Kind {
		case "budget":
			if limit.Min != nil || limit.Max != nil {
				fail("budget %q has min or max", limit.Name)
			}
			if strings.HasPrefix(limit.Name, "max_") {
				fail("budget_name_max %q", limit.Name)
			}
		case "ceiling":
			if !strings.HasPrefix(limit.Name, "max_") || len(limit.Name) == len("max_") {
				fail("ceiling_name %q", limit.Name)
			}
			if limit.Min == nil || limit.Max == nil {
				fail("ceiling %q without a bound", limit.Name)
				continue
			}
			low, errLow := strconv.ParseInt(limit.Min.String(), 10, 64)
			high, errHigh := strconv.ParseInt(limit.Max.String(), 10, 64)
			switch {
			case errLow != nil || errHigh != nil || low < 0 || high < 0:
				fail("bad_bound of %q", limit.Name)
			case low > high:
				fail("min_above_max of %q", limit.Name)
			case high >= two53:
				fail("max of %q is not below 2^53", limit.Name)
			}
		default:
			fail("bad_kind %q", limit.Kind)
		}
	}
	if len(limits) > 16 {
		fail("%d limits, the most is 16", len(limits))
	}

	var actions []struct {
		Name          string   `json:"name"`
		ResourceTypes []string `json:"resource_types"`
		Description   string   `json:"description"`
		Limits        []string `json:"limits"`
	}
	decodeMember("actions", &actions)
	actionNames := map[string]bool{}
	for _, action := range actions {
		if len(action.Name) < 1 || len(action.Name) > 64 || !lintActionName.MatchString(action.Name) {
			fail("bad_action_name %q", action.Name)
		}
		if action.Name == "access" {
			fail("access_in_list")
		}
		if actionNames[action.Name] {
			fail("duplicate_action_name %q", action.Name)
		}
		actionNames[action.Name] = true
		if runes(action.Description) > 300 || action.Description == "" {
			fail("description of action %q has %d characters", action.Name, runes(action.Description))
		}
		seen := map[string]bool{}
		for _, name := range action.ResourceTypes {
			if _, ok := parents[name]; !ok && name != "service" {
				fail("undeclared_type %q in action %q", name, action.Name)
			}
			if seen[name] {
				fail("duplicate_type %q in action %q", name, action.Name)
			}
			seen[name] = true
		}
		if len(action.ResourceTypes) == 0 {
			fail("action %q has no type", action.Name)
		}
		seen = map[string]bool{}
		for _, name := range action.Limits {
			if _, ok := kinds[name]; !ok {
				fail("undeclared_limit %q in action %q", name, action.Name)
			}
			if seen[name] {
				fail("duplicate_limit %q in action %q", name, action.Name)
			}
			seen[name] = true
		}
	}
	if len(actions) > 64 {
		fail("%d actions, the most is 64", len(actions))
	}

	var resources []struct {
		Path    string   `json:"path"`
		Label   *string  `json:"label"`
		Actions []string `json:"actions"`
	}
	decodeMember("resources", &resources)
	for _, resource := range resources {
		segments := strings.Split(resource.Path, "/")
		if len(segments) > 4 {
			fail("type_too_deep path %q", resource.Path)
		}
		previous := "service"
		for _, segment := range segments {
			kind, id, ok := strings.Cut(segment, ":")
			if !ok || !lintEncodedID.MatchString(id) {
				fail("bad_path %q", resource.Path)
				break
			}
			if _, declared := parents[kind]; !declared {
				fail("undeclared_type %q in path %q", kind, resource.Path)
				break
			}
			if parents[kind] != previous {
				fail("wrong_parent in path %q", resource.Path)
				break
			}
			previous = kind
			if len(id) == 0 {
				fail("bad_path %q: empty id", resource.Path)
			}
		}
		if resource.Label != nil && (runes(*resource.Label) < 1 || runes(*resource.Label) > 60) {
			fail("label of %q has %d characters", resource.Path, runes(*resource.Label))
		}
		for _, name := range resource.Actions {
			if !actionNames[name] {
				fail("undeclared_action %q in resource %q", name, resource.Path)
			}
		}
	}
	if len(resources) > 256 {
		fail("%d resources, the most is 256", len(resources))
	}
	return problems
}

// publishedActions gives the published actions in the order of the document.
func publishedActions(doc accessDocument) []string {
	var names []string
	for _, row := range accessActionRows {
		if doc.published(row.name) {
			names = append(names, row.name)
		}
	}
	return names
}

// documentLimitNames gives the names of the declared limits.
func documentLimitNames(t *testing.T, doc accessDocument) []string {
	t.Helper()
	var body struct {
		Limits []struct {
			Name string `json:"name"`
		} `json:"limits"`
	}
	if err := json.Unmarshal(doc.json(), &body); err != nil {
		t.Fatalf("the document is not JSON: %v", err)
	}
	var names []string
	for _, limit := range body.Limits {
		names = append(names, limit.Name)
	}
	return names
}

func mustBuildAccessDocument(t *testing.T, cfg appConfig) accessDocument {
	t.Helper()
	doc, err := buildAccessDocument(cfg)
	if err != nil {
		t.Fatalf("buildAccessDocument: %v", err)
	}
	if problems := lintAccessDocument(doc.json()); len(problems) != 0 {
		t.Fatalf("the document breaks the rules of the linter: %v\n%s", problems, doc.json())
	}
	return doc
}

func TestAccessDocumentPublishesOnlyTheActionsThatTheConfigurationServes(t *testing.T) {
	t.Parallel()
	all := []string{actionObjectRead, actionObjectWrite, actionObjectDelete, actionObjectLock, actionBucketAdmin}
	forceReadOnly := func(cfg appConfig) appConfig {
		cfg.Runtime.AccessMode = accessModeForceReadOnly
		return cfg
	}
	for _, tc := range []struct {
		name       string
		cfg        appConfig
		actions    []string
		limits     []string
		hasBudget  bool
		hasCeiling bool
	}{
		{"all permissions, one bucket and one volume",
			accessTestConfig([]bucketConfig{testBucket("docs", "Docs", nil)}, []volumeConfig{testVolume("scratch", "Scratch", nil)}),
			all, []string{limitWrites, limitMaxObjectSize}, true, true},
		{"force_read_only",
			forceReadOnly(accessTestConfig([]bucketConfig{testBucket("docs", "Docs", nil)}, []volumeConfig{testVolume("scratch", "Scratch", nil)})),
			[]string{actionObjectRead}, []string{limitMaxObjectSize}, false, true},
		{"one volume with read only",
			accessTestConfig(nil, []volumeConfig{testVolume("scratch", "Scratch", []string{"read"})}),
			[]string{actionObjectRead}, []string{limitMaxObjectSize}, false, true},
		{"one volume with read, write and delete, and no bucket",
			accessTestConfig(nil, []volumeConfig{testVolume("scratch", "Scratch", []string{"read", "write", "delete"})}),
			[]string{actionObjectRead, actionObjectWrite, actionObjectDelete}, []string{limitWrites, limitMaxObjectSize}, true, true},
		{"one bucket with read and delete",
			accessTestConfig([]bucketConfig{testBucket("docs", "Docs", []string{"read", "delete"})}, nil),
			[]string{actionObjectRead, actionObjectDelete}, []string{limitWrites, limitMaxObjectSize}, true, true},
		{"a bucket with read and a volume with write",
			accessTestConfig([]bucketConfig{testBucket("docs", "Docs", []string{"read"})}, []volumeConfig{testVolume("scratch", "Scratch", []string{"write"})}),
			[]string{actionObjectRead, actionObjectWrite}, []string{limitWrites, limitMaxObjectSize}, true, true},
		{"a bucket with write only: no Object Lock and no lifecycle, because both need read too",
			accessTestConfig([]bucketConfig{testBucket("docs", "Docs", []string{"write"})}, nil),
			[]string{actionObjectWrite}, []string{limitWrites, limitMaxObjectSize}, true, true},
		{"a bucket with read and write",
			accessTestConfig([]bucketConfig{testBucket("docs", "Docs", []string{"read", "write"})}, nil),
			[]string{actionObjectRead, actionObjectWrite, actionObjectLock, actionBucketAdmin}, []string{limitWrites, limitMaxObjectSize}, true, true},
		{"force_read_only with a bucket that has no permissions key",
			forceReadOnly(accessTestConfig([]bucketConfig{testBucket("docs", "Docs", nil)}, nil)),
			[]string{actionObjectRead}, []string{limitMaxObjectSize}, false, true},
		{"an empty list of permissions",
			accessTestConfig([]bucketConfig{testBucket("docs", "Docs", []string{})}, nil),
			nil, nil, false, false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			doc := mustBuildAccessDocument(t, tc.cfg)
			if got := publishedActions(doc); !reflect.DeepEqual(got, tc.actions) {
				t.Fatalf("published actions = %v, want %v", got, tc.actions)
			}
			if got := documentLimitNames(t, doc); !reflect.DeepEqual(got, tc.limits) {
				t.Fatalf("declared limits = %v, want %v", got, tc.limits)
			}
			for _, name := range tc.actions {
				if doc.listsBudget(name) != (name != actionObjectRead) {
					t.Fatalf("listsBudget(%q) = %v", name, doc.listsBudget(name))
				}
			}
			if doc.listsBudget(actionObjectRead) || doc.listsBudget("unknown.action") {
				t.Fatal("object.read and an unknown action list no budget")
			}
			wantCeiling := map[string]bool{actionObjectRead: true, actionObjectWrite: true}
			for _, name := range all {
				want := wantCeiling[name] && doc.published(name)
				if doc.listsCeiling(name, limitMaxObjectSize) != want {
					t.Fatalf("listsCeiling(%q, max_object_size) = %v, want %v", name, !want, want)
				}
				if doc.listsCeiling(name, limitWrites) {
					t.Fatalf("listsCeiling(%q, writes) is true: writes is a budget", name)
				}
			}
			if doc.isBudget(limitWrites) != tc.hasBudget || doc.isBudget(limitMaxObjectSize) || doc.isBudget("unknown") {
				t.Fatalf("isBudget(writes) = %v, want %v", doc.isBudget(limitWrites), tc.hasBudget)
			}
			// A published action lists only limits that the document declares and that its table gives.
			for _, row := range accessActionRows {
				if doc.published(row.name) && !reflect.DeepEqual(doc.actions[row.name].Limits, row.limits) {
					t.Fatalf("limits of %q = %v, want %v", row.name, doc.actions[row.name].Limits, row.limits)
				}
			}
		})
	}
}

func TestAccessDocumentTableMatchesTheContract(t *testing.T) {
	t.Parallel()
	for _, row := range accessActionRows {
		wantTypes := accessAllTypes
		if row.name == actionBucketAdmin {
			wantTypes = []string{accessTypeService, accessTypeBucket}
		}
		if !reflect.DeepEqual(row.types, wantTypes) {
			t.Fatalf("types of %q = %v, want %v", row.name, row.types, wantTypes)
		}
		for _, name := range row.limits {
			if name == "max_objects" {
				t.Fatalf("action %q lists max_objects: Stowr has no such ceiling", row.name)
			}
		}
	}
	doc := mustBuildAccessDocument(t, accessTestConfig([]bucketConfig{testBucket("docs", "Docs", nil)}, nil))
	for _, name := range []string{actionObjectWrite, actionObjectDelete, actionObjectLock, actionBucketAdmin} {
		if !doc.listsBudget(name) {
			t.Fatalf("%q lists no budget", name)
		}
	}
	if !doc.listsCeiling(actionObjectRead, limitMaxObjectSize) || !doc.listsCeiling(actionObjectWrite, limitMaxObjectSize) {
		t.Fatal("object.read and object.write list max_object_size")
	}
	if doc.listsCeiling(actionObjectDelete, limitMaxObjectSize) {
		t.Fatal("object.delete does not list max_object_size")
	}
	// Each declared limit is listed by a published action, and each limit of an action is declared.
	declared := map[string]bool{}
	for _, name := range documentLimitNames(t, doc) {
		declared[name] = true
	}
	listed := map[string]bool{}
	for _, name := range publishedActions(doc) {
		for _, limit := range doc.actions[name].Limits {
			listed[limit] = true
			if !declared[limit] {
				t.Fatalf("action %q lists the undeclared limit %q", name, limit)
			}
		}
	}
	if !reflect.DeepEqual(declared, listed) {
		t.Fatalf("declared limits %v, listed limits %v", declared, listed)
	}
}

func TestAccessDocumentIsDeterministic(t *testing.T) {
	t.Parallel()
	cfg := accessTestConfig([]bucketConfig{testBucket("docs", "Docs", nil), testBucket("logs", "Logs", []string{"read"})},
		[]volumeConfig{testVolume("scratch", "Scratch", nil)})
	first := mustBuildAccessDocument(t, cfg)
	second := mustBuildAccessDocument(t, cfg)
	if !bytes.Equal(first.json(), second.json()) {
		t.Fatal("two builds of one configuration give two documents")
	}
}

func TestAccessDocumentGolden(t *testing.T) {
	t.Parallel()
	cfg := accessTestConfig([]bucketConfig{testBucket("docs", "Documents", nil)}, []volumeConfig{testVolume("scratch", "Scratch folder", []string{"read"})})
	doc := mustBuildAccessDocument(t, cfg)
	const allTypes = `["service","bucket","folder","subfolder"]`
	want := `{"server":"stowr-test",` +
		`"resource_types":[` +
		`{"name":"bucket","parent":"service","description":"One bucket block or one volume block of the configuration. The id is the label of the block."},` +
		`{"name":"folder","parent":"bucket","description":"One top-level folder of a bucket or a volume, below its root prefix. The id is the first part of the object key."},` +
		`{"name":"subfolder","parent":"folder","description":"One folder in a top-level folder. The id is the second part of the object key."}],` +
		`"actions":[` +
		`{"name":"object.read","resource_types":` + allTypes + `,"description":"List, preview, inspect and download objects. Show versions, tags, metadata, usage and archive entries. Check the integrity of an object.","limits":["max_object_size"]},` +
		`{"name":"object.write","resource_types":` + allTypes + `,"description":"Upload objects, make folders, restore versions, and change tags and metadata. Be the destination of a copy, a move or an extraction.","limits":["writes","max_object_size"]},` +
		`{"name":"object.delete","resource_types":` + allTypes + `,"description":"Delete objects, versions and folders. Be the source of a move or a rename.","limits":["writes"]},` +
		`{"name":"object.lock","resource_types":` + allTypes + `,"description":"Set or remove a legal hold. Set, extend or shorten the retention of an object version.","limits":["writes"]},` +
		`{"name":"bucket.admin","resource_types":["service","bucket"],"description":"Change the lifecycle rules of a bucket.","limits":["writes"]}],` +
		`"resources":[{"path":"bucket:docs","label":"Documents"},{"path":"bucket:scratch","label":"Scratch folder"}],` +
		`"limits":[` +
		`{"name":"writes","label":"Writes","description":"Operations that change objects or bucket settings.","unit":"operation","kind":"budget"},` +
		`{"name":"max_object_size","label":"Largest object","description":"The largest object that one use reads or writes, in bytes.","unit":"byte","kind":"ceiling","min":1,"max":5497558138880}]}`
	if got := string(doc.json()); got != want {
		t.Fatalf("golden document differs.\n got: %s\nwant: %s", got, want)
	}
}

func TestAccessDocumentResourcesEncodeIDsAndKeepTheOrder(t *testing.T) {
	t.Parallel()
	cfg := accessTestConfig(
		[]bucketConfig{testBucket("b-2", "Second", nil), testBucket("a b/é", "Third", nil)},
		[]volumeConfig{testVolume("vol", "Volume", nil)})
	doc := mustBuildAccessDocument(t, cfg)
	var body struct {
		Resources []map[string]string `json:"resources"`
	}
	if err := json.Unmarshal(doc.json(), &body); err != nil {
		t.Fatal(err)
	}
	want := []map[string]string{
		{"path": "bucket:b-2", "label": "Second"},
		{"path": "bucket:a%20b%2F%C3%A9", "label": "Third"},
		{"path": "bucket:vol", "label": "Volume"},
	}
	if !reflect.DeepEqual(body.Resources, want) {
		t.Fatalf("resources = %v, want %v", body.Resources, want)
	}
	for _, resource := range body.Resources {
		if _, ok := resource["actions"]; ok {
			t.Fatal("a resource has an actions member")
		}
	}
}

func TestAccessDocumentLabelFollowsTheNameRules(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		wantLabel bool
	}{
		{strings.Repeat("a", 60), true},
		{strings.Repeat("a", 61), false},
		{strings.Repeat("é", 60), true},
		{strings.Repeat("é", 61), false},
		{strings.Repeat("日", 60), true},
		{"", false},
		{"two\nlines", false},
		{"tab\there", false},
		{"delete\x7fhere", false},
		{"bad\xffutf8", false},
		{"Plain name", true},
	} {
		doc := mustBuildAccessDocument(t, accessTestConfig([]bucketConfig{testBucket("docs", tc.name, nil)}, nil))
		var body struct {
			Resources []map[string]any `json:"resources"`
		}
		if err := json.Unmarshal(doc.json(), &body); err != nil {
			t.Fatal(err)
		}
		label, has := body.Resources[0]["label"]
		if has != tc.wantLabel {
			t.Fatalf("name %q: label present = %v, want %v", tc.name, has, tc.wantLabel)
		}
		if has && label != tc.name {
			t.Fatalf("name %q: label = %v", tc.name, label)
		}
	}
}

func TestAccessDocumentKeepsHTMLCharactersAsTheyAre(t *testing.T) {
	t.Parallel()
	doc := mustBuildAccessDocument(t, accessTestConfig([]bucketConfig{testBucket("docs", "R&D <team>", nil)}, nil))
	if !bytes.Contains(doc.json(), []byte(`"label":"R&D <team>"`)) {
		t.Fatalf("the label is not in the document as it is: %s", doc.json())
	}
}

func TestAccessDocumentAbove64KiBStopsTheStart(t *testing.T) {
	t.Parallel()
	var buckets []bucketConfig
	for index := 0; index < 250; index++ {
		id := fmt.Sprintf("%03d", index) + strings.Repeat("x", 253)
		buckets = append(buckets, testBucket(id, strings.Repeat("n", 60), nil))
	}
	_, err := buildAccessDocument(accessTestConfig(buckets, nil))
	if err == nil {
		t.Fatal("a document above 64 KiB gives no error")
	}
	for _, part := range []string{"bytes", "65536", "5.3c"} {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("the error %q does not name %q", err, part)
		}
	}
	// The same configuration in mode "open" builds nothing and gives no error.
	cfg := accessTestConfig(buckets, nil)
	cfg.Access = accessConfig{Mode: accessModeOpen}
	if _, err := buildAccessDocument(cfg); err != nil {
		t.Fatalf("mode open: %v", err)
	}
}

func TestAccessDocumentAtTheReaderMinimumIsGood(t *testing.T) {
	t.Parallel()
	var buckets []bucketConfig
	for index := 0; index < 256; index++ {
		buckets = append(buckets, testBucket(fmt.Sprintf("bucket-%03d", index), strings.Repeat("é", 60), nil))
	}
	doc := mustBuildAccessDocument(t, accessTestConfig(buckets, nil))
	if len(doc.json()) > 65536 {
		t.Fatalf("document has %d bytes", len(doc.json()))
	}
}

func TestAccessDocumentOfModeOpenIsEmpty(t *testing.T) {
	t.Parallel()
	cfg := accessTestConfig([]bucketConfig{testBucket("docs", "Docs", nil)}, nil)
	cfg.Access = accessConfig{Mode: accessModeOpen}
	doc, err := buildAccessDocument(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.json()) != 0 {
		t.Fatalf("document of mode open = %q", doc.json())
	}
	for _, row := range accessActionRows {
		if doc.published(row.name) || doc.listsBudget(row.name) || doc.listsCeiling(row.name, limitMaxObjectSize) {
			t.Fatalf("mode open publishes %q", row.name)
		}
	}
	if doc.isBudget(limitWrites) {
		t.Fatal("mode open declares the budget writes")
	}
}

func TestAccessLintCatchesTheBrokenRules(t *testing.T) {
	t.Parallel()
	good := string(mustBuildAccessDocument(t, accessTestConfig([]bucketConfig{testBucket("docs", "Docs", nil)}, nil)).json())
	for _, tc := range []struct{ name, from, to, want string }{
		{"action access", `"name":"object.read"`, `"name":"access"`, "access_in_list"},
		{"cycle", `{"name":"bucket","parent":"service"`, `{"name":"bucket","parent":"subfolder"`, "type_cycle"},
		{"type service", `{"name":"bucket","parent":"service"`, `{"name":"service","parent":"service"`, "reserved_type_name"},
		{"budget with min", `"unit":"operation","kind":"budget"`, `"unit":"operation","kind":"budget","min":1`, "has min or max"},
		{"ceiling above 2^53", `"max":5497558138880`, `"max":9007199254740992`, "2^53"},
		{"undeclared limit", `"limits":["writes"]`, `"limits":["writes","max_rows"]`, "undeclared_limit"},
		{"bad path", `"path":"bucket:docs"`, `"path":"bucket:do cs"`, "bad_path"},
		{"wrong parent", `"path":"bucket:docs"`, `"path":"folder:docs"`, "wrong_parent"},
		{"unknown member", `{"server":`, `{"extra":1,"server":`, "unknown member"},
		{"lower case hex", `"path":"bucket:docs"`, `"path":"bucket:a%2f"`, "bad_path"},
	} {
		if !strings.Contains(good, tc.from) {
			t.Fatalf("%s: the good document has no %q", tc.name, tc.from)
		}
		bad := strings.Replace(good, tc.from, tc.to, 1)
		problems := lintAccessDocument([]byte(bad))
		if len(problems) == 0 || !strings.Contains(strings.Join(problems, "\n"), tc.want) {
			t.Fatalf("%s: problems = %v, want one with %q", tc.name, problems, tc.want)
		}
	}
	if problems := lintAccessDocument([]byte(good)); len(problems) != 0 {
		t.Fatalf("the good document has problems: %v", problems)
	}
}

func TestServeAccessActions(t *testing.T) {
	t.Parallel()
	cfg := accessTestConfig([]bucketConfig{testBucket("docs", "Docs", nil)}, nil)
	doc := mustBuildAccessDocument(t, cfg)

	t.Run("GET gives the document", func(t *testing.T) {
		t.Parallel()
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, accessActionsPath, nil)
		// The route never reads an identity header: a bad value changes nothing.
		request.Header.Set("X-Identity-Token", "not a token")
		serveAccessActions(recorder, request, cfg.Access, doc)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d", recorder.Code)
		}
		if got := recorder.Header().Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q", got)
		}
		if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("Cache-Control = %q", got)
		}
		if !bytes.Equal(recorder.Body.Bytes(), doc.json()) {
			t.Fatal("the body is not the bytes of the document")
		}
	})
	t.Run("HEAD gives the headers and no body", func(t *testing.T) {
		t.Parallel()
		recorder := httptest.NewRecorder()
		serveAccessActions(recorder, httptest.NewRequest(http.MethodHead, accessActionsPath, nil), cfg.Access, doc)
		if recorder.Code != http.StatusOK || recorder.Body.Len() != 0 {
			t.Fatalf("status = %d, body = %d bytes", recorder.Code, recorder.Body.Len())
		}
		if got := recorder.Header().Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q", got)
		}
		if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("Cache-Control = %q", got)
		}
		if got := recorder.Header().Get("Content-Length"); got != strconv.Itoa(len(doc.json())) {
			t.Fatalf("Content-Length = %q", got)
		}
	})
	t.Run("other methods give 405", func(t *testing.T) {
		t.Parallel()
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodOptions} {
			recorder := httptest.NewRecorder()
			serveAccessActions(recorder, httptest.NewRequest(method, accessActionsPath, nil), cfg.Access, doc)
			if recorder.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s: status = %d", method, recorder.Code)
			}
			if got := recorder.Header().Get("Allow"); got != "GET, HEAD" {
				t.Fatalf("%s: Allow = %q", method, got)
			}
		}
	})
	t.Run("mode open gives 404", func(t *testing.T) {
		t.Parallel()
		open := accessConfig{Mode: accessModeOpen}
		openDoc, err := buildAccessDocument(appConfig{Access: open})
		if err != nil {
			t.Fatal(err)
		}
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			recorder := httptest.NewRecorder()
			serveAccessActions(recorder, httptest.NewRequest(method, accessActionsPath, nil), open, openDoc)
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("%s: status = %d", method, recorder.Code)
			}
		}
	})
}
