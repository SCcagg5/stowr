package main

import (
	"path/filepath"
	"testing"
)

func TestDefaultRuntimeIsStatelessAndBounded(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg, err := decodeConfig(minimalTestConfig("stateless"), filepath.Join(root, "config.hcl"), root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runtime.AccessMode != accessModeInheritCredentials {
		t.Fatalf("access mode = %q", cfg.Runtime.AccessMode)
	}
	if cfg.JobHistoryLimit != 10 {
		t.Fatalf("job history limit = %d", cfg.JobHistoryLimit)
	}
	if cfg.Runtime.MaxRangeCacheBytes != 8<<20 {
		t.Fatalf("range cache = %d", cfg.Runtime.MaxRangeCacheBytes)
	}
	if cfg.Runtime.MaxConcurrentStorageRequests != 4 || cfg.Runtime.MaxConcurrentRequestsPerStore != 2 {
		t.Fatalf("concurrency = %d / %d", cfg.Runtime.MaxConcurrentStorageRequests, cfg.Runtime.MaxConcurrentRequestsPerStore)
	}
}

func TestJobAndUploadManagersAreMemoryOnly(t *testing.T) {
	app, _, _ := testApplication(t)
	if app.jobs == nil || app.uploads == nil {
		t.Fatal("in-memory managers were not initialized")
	}
	if app.jobs.jobs == nil || app.uploads.uploads == nil {
		t.Fatal("manager maps were not initialized")
	}
}

func TestForceReadOnlyIsAnAdministrativeCeiling(t *testing.T) {
	t.Parallel()
	cfg := bucketConfig{PermissionsDefined: false}
	caps := initialCapabilities(cfg, true)
	if caps.Read.State != capabilityUnknown || !caps.Read.Allowed {
		t.Fatalf("read = %#v, want tentable", caps.Read)
	}
	for name, state := range map[string]capabilityState{
		permissionWrite: caps.Write, permissionDelete: caps.Delete,
	} {
		if state.State != capabilityDenied || state.Allowed || state.Source != "runtime" || !state.Verified {
			t.Fatalf("%s = %#v, want runtime denial", name, state)
		}
	}
}

// testOpenAccess is the access block of a test configuration that checks no identity token. A test adds it at the end
// of its text: the line numbers of the other blocks do not change.
const testOpenAccess = "\naccess {\n  mode = \"open\"\n}\n"

func minimalTestConfig(id string) string {
	return `
access {
  mode = "open"
}

auth "public" {
  provider = "s3"
  mode     = "anonymous"
  endpoint = "http://localhost:9000"
  region   = "test"
}

bucket "` + id + `" {
  auth   = "public"
  bucket = "bucket"
}
`
}
