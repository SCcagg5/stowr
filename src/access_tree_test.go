package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// The node of a key follows the table of docs/ACCESS.md, "From a key to a node".
func TestKeyResourceGivesTheDeepestFolderOfTheKey(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 257)
	for _, tc := range []struct {
		rel  string
		want string
	}{
		{"", "service:svc/bucket:b"},
		{"a.txt", "service:svc/bucket:b"},
		{"rep", "service:svc/bucket:b"},
		{"reports/", "service:svc/bucket:b/folder:reports"},
		{"reports/q1.csv", "service:svc/bucket:b/folder:reports"},
		{"reports/20", "service:svc/bucket:b/folder:reports"},
		{"reports/2026/", "service:svc/bucket:b/folder:reports/subfolder:2026"},
		{"reports/2026/q1.csv", "service:svc/bucket:b/folder:reports/subfolder:2026"},
		{"reports/2026/q1/x.csv", "service:svc/bucket:b/folder:reports/subfolder:2026"},
		{"reports/2026/q1/", "service:svc/bucket:b/folder:reports/subfolder:2026"},
		{"reports//x", "service:svc/bucket:b/folder:reports"},
		{"/x", "service:svc/bucket:b"},
		{"team a/x", "service:svc/bucket:b/folder:team%20a"},
		{long + "/x", "service:svc/bucket:b"},
		{"bad\x01/x", "service:svc/bucket:b"},
		{"\xff/x", "service:svc/bucket:b"},
		{"public/../secret/x", "service:svc/bucket:b/folder:public/subfolder:.."},
	} {
		got := keyResource("svc", "b", tc.rel)
		if got.path() != tc.want {
			t.Fatalf("keyResource(%q) = %s, want %s", tc.rel, got.path(), tc.want)
		}
	}
}

// A node below the service carries its chain in properties.ancestors, from the service. The service has none.
func TestAccessResourceJSONCarriesTheChain(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		resource accessResource
		want     string
	}{
		{serviceResource("svc"), `{"type":"service","id":"svc"}`},
		{bucketResource("svc", "b"), `{"type":"bucket","id":"b","properties":{"ancestors":[{"type":"service","id":"svc"}]}}`},
		{keyResource("svc", "b", "r/2/x"), `{"type":"subfolder","id":"2","properties":{"ancestors":[{"type":"service","id":"svc"},{"type":"bucket","id":"b"},{"type":"folder","id":"r"}]}}`},
	} {
		data, err := json.Marshal(tc.resource)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != tc.want {
			t.Fatalf("JSON = %s, want %s", data, tc.want)
		}
	}
}

func TestPercentEncodeIDKeepsOnlyTheUnreservedBytes(t *testing.T) {
	t.Parallel()
	if got := percentEncodeID("a/b c~é"); got != "a%2Fb%20c~%C3%A9" {
		t.Fatalf("percentEncodeID = %q", got)
	}
	if !validAccessID("é") || validAccessID("") || validAccessID(strings.Repeat("a", 257)) || validAccessID("a\x7f") {
		t.Fatal("validAccessID does not follow ProcurAuth 4.1")
	}
}
