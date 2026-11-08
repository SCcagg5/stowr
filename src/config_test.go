package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The file docs/configuration-reference.md is the output of configReference: the program makes it from the table of
// keys. Run `go run . config-reference > ../docs/configuration-reference.md` in src after a change of the table.
func TestConfigurationReferenceIsCurrent(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("..", "docs", "configuration-reference.md"))
	if err != nil {
		t.Fatalf("read the reference: %v", err)
	}
	if string(data) != configReference() {
		t.Fatal("docs/configuration-reference.md is not the output of configReference: make it again with go run . config-reference")
	}
}

func TestOmittedPermissionsInheritCredentialCapabilities(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.hcl")
	if err := os.WriteFile(filepath.Join(dir, "secret"), []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, path, `
auth "shared" {
  provider          = "s3"
  mode              = "access_key"
  endpoint          = "http://localhost:9000"
  region            = "test"
  access_key_id          = "key"
  secret_access_key_file = "secret"
}

bucket "inherited" {
  auth   = "shared"
  bucket = "bucket"
}
`+testOpenAccess)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	bucket := cfg.Buckets[0]
	if bucket.PermissionsDefined || len(bucket.Permissions) != 0 {
		t.Fatalf("omitted permissions must not create an application ceiling: %#v", bucket)
	}
	caps := initialCapabilities(bucket)
	for name, state := range map[string]capabilityState{
		permissionRead: caps.Read, permissionWrite: caps.Write, permissionDelete: caps.Delete,
	} {
		if state.State != capabilityUnknown || !state.Allowed || state.Verified {
			t.Fatalf("%s capability = %#v, want tentable unknown", name, state)
		}
	}
}

func TestLoadConfigSharesAuthenticationAcrossBuckets(t *testing.T) {
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "s3-secret.txt")
	if err := os.WriteFile(secretPath, []byte("shared-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.hcl")
	writeConfig(t, configPath, `
server {
  listen            = ":9090"
  job_history_limit = 17
}

auth "production" {
  provider               = "s3"
  mode                   = "access_key"
  endpoint               = "http://127.0.0.1:9000"
  region                 = "us-east-1"
  access_key_id          = "shared-key"
  secret_access_key_file = "./s3-secret.txt"
}

bucket "documents" {
  name           = "Documents"
  auth           = "production"
  bucket         = "documents"
  permissions    = ["read", "write", "delete"]
  root_prefix    = "tenant-a/"
  max_scan_pages = 15
}

bucket "archive" {
  name        = "Archive"
  auth        = "production"
  bucket      = "archive"
  permissions = ["read"]
}
`+testOpenAccess)
	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if cfg.Listen != ":9090" || cfg.JobHistoryLimit != 17 {
		t.Fatalf("server config = listen:%q history:%d", cfg.Listen, cfg.JobHistoryLimit)
	}
	if len(cfg.Authentications) != 1 || len(cfg.Buckets) != 2 {
		t.Fatalf("auth=%d buckets=%d", len(cfg.Authentications), len(cfg.Buckets))
	}
	for _, bucket := range cfg.Buckets {
		if bucket.AuthID != "production" || bucket.Provider != "s3" || bucket.Region != "us-east-1" {
			t.Fatalf("bucket did not resolve shared auth identity: %+v", bucket)
		}
	}
	auth := cfg.Authentications[0]
	if auth.AccessKeyID != "shared-key" || auth.SecretAccessKey != "shared-secret" {
		t.Fatalf("shared authentication credentials were not loaded: %+v", auth)
	}
	if cfg.Buckets[0].RootPrefix != "tenant-a/" {
		t.Fatalf("RootPrefix = %q", cfg.Buckets[0].RootPrefix)
	}
	if cfg.Buckets[0].MaxScanPages != 15 || cfg.Buckets[1].MaxScanPages != 1 {
		t.Fatalf("max scan pages = documents:%d archive:%d", cfg.Buckets[0].MaxScanPages, cfg.Buckets[1].MaxScanPages)
	}
	if got := strings.Join(cfg.Buckets[0].Permissions, ","); got != "read,write,delete" {
		t.Fatalf("documents permissions = %q", got)
	}
}

func TestLoadConfigAllowsDashInstanceID(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.hcl")
	writeConfig(t, path, minimalTestConfig("-"))
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Buckets) != 1 || cfg.Buckets[0].ID != "-" {
		t.Fatalf("buckets = %#v", cfg.Buckets)
	}
}

func TestLoadConfigBackgroundBudgets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.hcl")
	writeConfig(t, path, `
server {
  max_background_storage_bytes    = 274877906944
  max_background_storage_requests = 12345
}

`+minimalTestConfig("primary"))
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runtime.MaxBackgroundStorageBytes != 274877906944 {
		t.Fatalf("MaxBackgroundStorageBytes = %d", cfg.Runtime.MaxBackgroundStorageBytes)
	}
	if cfg.Runtime.MaxBackgroundStorageRequests != 12345 {
		t.Fatalf("MaxBackgroundStorageRequests = %d", cfg.Runtime.MaxBackgroundStorageRequests)
	}
}

func TestLoadConfigRejectsInvalidBackgroundBudgets(t *testing.T) {
	for _, test := range []struct {
		name      string
		attribute string
		value     string
	}{
		{name: "background bytes too small", attribute: "max_background_storage_bytes", value: "1024"},
		{name: "background requests zero", attribute: "max_background_storage_requests", value: "0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.hcl")
			writeConfig(t, path, "server { "+test.attribute+" = "+test.value+" }\n"+minimalTestConfig("primary"))
			if _, err := loadConfig(path); err == nil || !strings.Contains(err.Error(), test.attribute) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestLoadConfigUsesLowMemoryDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.hcl")
	writeConfig(t, path, minimalTestConfig("primary"))
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.JobHistoryLimit != 10 || cfg.Runtime.MaxRangeCacheBytes != 8<<20 {
		t.Fatalf("defaults = history:%d cache:%d", cfg.JobHistoryLimit, cfg.Runtime.MaxRangeCacheBytes)
	}
	if cfg.Runtime.MaxConcurrentStorageRequests != 4 || cfg.Runtime.MaxConcurrentRequestsPerStore != 2 {
		t.Fatalf("concurrency defaults = global:%d bucket:%d", cfg.Runtime.MaxConcurrentStorageRequests, cfg.Runtime.MaxConcurrentRequestsPerStore)
	}
	if cfg.Runtime.MaxStorageBytesPerRequest < 100<<30 || cfg.Runtime.MaxBackgroundStorageBytes < 100<<30 {
		t.Fatalf("streaming byte budgets must support 100 GiB objects: request:%d background:%d", cfg.Runtime.MaxStorageBytesPerRequest, cfg.Runtime.MaxBackgroundStorageBytes)
	}
}

func TestLoadConfigSupportsGCSAuthentication(t *testing.T) {
	dir := t.TempDir()
	credentials := filepath.Join(dir, "gcs.json")
	if err := os.WriteFile(credentials, []byte(`{"type":"service_account"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.hcl")
	writeConfig(t, configPath, `
auth "gcs" {
  provider         = "gcs"
  mode             = "service_account"
  credentials_file = "./gcs.json"
}

bucket "archive" {
  auth   = "gcs"
  bucket = "archive-bucket"
}
`+testOpenAccess)
	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	bucket := cfg.Buckets[0]
	if bucket.Provider != "gcs" || bucket.AuthID != "gcs" {
		t.Fatalf("resolved GCS bucket = %+v", bucket)
	}
	auth := cfg.Authentications[0]
	if auth.Endpoint != "https://storage.googleapis.com" || auth.CredentialsFile != credentials {
		t.Fatalf("resolved GCS authentication = %+v", auth)
	}
}

func TestLoadConfigRejectsUnknownAuthenticationAttributes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.hcl")
	writeConfig(t, path, `
auth "broken" {
  provider           = "s3"
  endpoint           = "http://localhost:9000"
  region             = "test"
  unsupported_option = "value"
}

bucket "bucket" { auth = "broken" bucket = "bucket" }
`+testOpenAccess)
	_, err := loadConfig(path)
	if err == nil || !strings.Contains(err.Error(), `unknown attribute "unsupported_option"`) {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadConfigRejectsUnknownAuthentication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.hcl")
	writeConfig(t, path, minimalTestConfig("known")+`
bucket "broken" { auth = "missing" bucket = "bucket" }
`)
	_, err := loadConfig(path)
	if err == nil || !strings.Contains(err.Error(), `bucket "broken".auth: no auth block has the label "missing"`) {
		t.Fatalf("error = %v", err)
	}
}
func TestLoadConfigRejectsUnknownServerAttribute(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.hcl")
	writeConfig(t, path, `server { public_prefix = "/browser/" }
`+minimalTestConfig("bucket"))
	_, err := loadConfig(path)
	if err == nil || !strings.Contains(err.Error(), "public_prefix") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadConfigValidatesMaxScanPages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.hcl")
	writeConfig(t, path, `
auth "public" {
  provider = "s3"
  mode     = "anonymous"
  endpoint = "http://localhost:9000"
  region   = "test"
}

bucket "bucket" {
  auth           = "public"
  bucket         = "bucket"
  max_scan_pages = 1000001
}
`+testOpenAccess)
	if _, err := loadConfig(path); err == nil || !strings.Contains(err.Error(), "max_scan_pages") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadConfigRejectsOversizedConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.hcl")
	data := strings.Repeat("# padding\n", int(maxConfigurationBytes/10)+2)
	if int64(len(data)) <= maxConfigurationBytes {
		data += strings.Repeat("x", int(maxConfigurationBytes)-len(data)+1)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := loadConfig(path)
	if err == nil || !strings.Contains(err.Error(), "exceeds the") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadConfigRejectsOversizedSecretFile(t *testing.T) {
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(secretPath, bytes.Repeat([]byte{'x'}, int(maxSecretFileBytes)+1), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.hcl")
	writeConfig(t, configPath, `
auth "shared" {
  provider               = "s3"
  mode                   = "access_key"
  endpoint               = "http://localhost:9000"
  region                 = "test"
  access_key_id          = "key"
  secret_access_key_file = "./secret.txt"
}

bucket "bucket" { auth = "shared" bucket = "bucket" }
`+testOpenAccess)
	_, err := loadConfig(configPath)
	if err == nil || !strings.Contains(err.Error(), "exceeds the") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadRuntimeConfigRequiresPath(t *testing.T) {
	_, err := loadRuntimeConfig("")
	if err == nil || !strings.Contains(err.Error(), "configuration path is required") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadConfigRejectsWrongAttributeType(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.hcl")
	writeConfig(t, path, `
auth "public" {
  provider = "s3"
  mode     = "anonymous"
  endpoint = "http://localhost:9000"
  region   = "test"
}

bucket "broken" {
  auth        = "public"
  bucket      = "bucket"
  permissions = true
}
`+testOpenAccess)
	_, err := loadConfig(path)
	if err == nil || !strings.Contains(err.Error(), `attribute "permissions" must be a string list`) {
		t.Fatalf("error = %v", err)
	}
}

func TestParseHCLSubsetCommentsAndEscapes(t *testing.T) {
	t.Parallel()
	root, err := parseHCLSubset(`
// line comment
server { listen = ":8080" }
/* block comment */
bucket "one" {
  name = "A\nB"
  permissions = ["read", "write",]
}
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(root.Blocks) != 2 {
		t.Fatalf("blocks = %d", len(root.Blocks))
	}
	if got, _ := root.Blocks[1].stringAttr("name"); got != "A\nB" {
		t.Fatalf("escaped string = %q", got)
	}
}

func TestLoadConfigJobHistoryLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.hcl")
	writeConfig(t, path, `server { job_history_limit = 12 }
`+minimalTestConfig("primary"))
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.JobHistoryLimit != 12 {
		t.Fatalf("JobHistoryLimit = %d, want 12", cfg.JobHistoryLimit)
	}
}

func TestLoadConfigRejectsInvalidJobHistoryLimit(t *testing.T) {
	for _, value := range []string{"0", "21", `"100"`} {
		t.Run(value, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.hcl")
			writeConfig(t, path, "server { job_history_limit = "+value+" }\n"+minimalTestConfig("primary"))
			if _, err := loadConfig(path); err == nil {
				t.Fatal("expected invalid job history limit to fail")
			}
		})
	}
}

func writeConfig(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestStrictValuesStopTheStart: the server never changes a value to make it
// fit. A value that does not fit stops the start, and the error names the key
// and the value (but never the value of a secret).
func TestStrictValuesStopTheStart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "spaced"), []byte(" secret-with-space\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "good"), []byte("good-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	auth := func(lines string) string {
		return "auth \"s3\" {\n" + lines + "\n}\nbucket \"b\" {\n  auth = \"s3\"\n  bucket = \"b\"\n}\n"
	}
	base := "provider = \"s3\"\nendpoint = \"http://localhost:9000\"\nregion = \"test\"\n"
	withBucket := func(lines string) string {
		return "auth \"s3\" {\n" + base + "}\nbucket \"b\" {\n  auth = \"s3\"\n" + lines + "\n}\n"
	}
	for name, test := range map[string]struct {
		config string
		want   string
		hidden string
	}{
		"upper-case provider":   {auth(strings.Replace(base, `"s3"`, `"S3"`, 1)), `auth "s3".provider must be "s3" or "gcs", got "S3"`, ""},
		"space in mode":         {auth(base + `mode = " anonymous"`), `auth "s3".mode must not have a space at the start or at the end, got " anonymous"`, ""},
		"endpoint slash":        {auth(strings.Replace(base, `9000"`, `9000/"`, 1)), `auth "s3".endpoint must not end with "/", got "http://localhost:9000/"`, ""},
		"region space":          {auth(strings.Replace(base, `"test"`, `"test "`, 1)), `auth "s3".region must not have a space`, ""},
		"clear secret":          {auth(base + "access_key_id = \"k\"\nsecret_access_key = \"top-secret-value\""), `auth "s3".secret_access_key is a secret: use secret_access_key_env or secret_access_key_file`, "top-secret-value"},
		"clear session token":   {auth(base + "session_token = \"top-secret-value\""), `auth "s3".session_token is a secret`, "top-secret-value"},
		"secret with space":     {auth(base + "access_key_id = \"k\"\nsecret_access_key_file = \"spaced\""), `auth "s3".secret_access_key must not contain a space or a control character`, "secret-with-space"},
		"two secret forms":      {auth(base + "access_key_id = \"k\"\nsecret_access_key_file = \"good\"\nsecret_access_key_env = \"X\""), "give only one of secret_access_key, secret_access_key_env and secret_access_key_file", "good-secret"},
		"root prefix slash":     {withBucket(`  bucket = "b"` + "\n" + `  root_prefix = "/tenant"`), `bucket "b".root_prefix must be "" or a prefix that ends with "/"`, ""},
		"root prefix no end":    {withBucket(`  bucket = "b"` + "\n" + `  root_prefix = "tenant"`), `got "tenant"`, ""},
		"upper-case permission": {withBucket(`  bucket = "b"` + "\n" + `  permissions = ["Read"]`), `bucket "b".permissions must contain only "read", "write" and "delete", got "Read"`, ""},
		"duplicate permission":  {withBucket(`  bucket = "b"` + "\n" + `  permissions = ["read", "read"]`), `bucket "b".permissions contains "read" two times`, ""},
		"empty name":            {withBucket(`  bucket = "b"` + "\n" + `  name = ""`), `bucket "b".name must not be empty`, ""},
		"bucket name space":     {withBucket(`  bucket = " b"`), `bucket "b".bucket must not have a space at the start or at the end, got " b"`, ""},
		"upper-case log mode":   {"server { log_mode = \"Detailed\" }\n" + withBucket(`  bucket = "b"`), `server.log_mode must be "anonymous" or "detailed", got "Detailed"`, ""},
		"listen space":          {"server { listen = \" :8080\" }\n" + withBucket(`  bucket = "b"`), `server.listen`, ""},
	} {
		_, err := decodeConfig(test.config, filepath.Join(dir, "config.hcl"), dir)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("%s: error = %v, want %q", name, err, test.want)
		}
		if test.hidden != "" && strings.Contains(err.Error(), test.hidden) {
			t.Fatalf("%s: the error shows the secret: %v", name, err)
		}
	}
}
