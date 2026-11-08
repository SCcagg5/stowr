package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// usageAuthLines is the body of an auth block with the mode access_key. The lines of the usage block follow it.
const usageAuthLines = `
  provider               = "s3"
  mode                   = "access_key"
  endpoint               = "http://minio.local:9000"
  region                 = "test"
  access_key_id          = "key"
  secret_access_key_file = "./s3-secret"
`

// usageConfigText gives a configuration with the auth block "x" and the bucket "b". authLines is the body of the auth
// block, with the usage block. bucketLines are more lines of the bucket block.
func usageConfigText(authLines, bucketLines string) string {
	return "auth \"x\" {" + authLines + "}\nbucket \"b\" {\n  auth   = \"x\"\n  bucket = \"bucket\"\n" + bucketLines + "\n}\n"
}

// usageBlock gives an access_key auth body with a usage block that has these lines.
func usageBlock(lines string) string {
	return usageAuthLines + "  usage {\n" + lines + "\n  }\n"
}

// loadUsageConfig writes the files that the configuration needs and loads it. It gives the directory too.
func loadUsageConfig(t *testing.T, content string) (appConfig, string, error) {
	t.Helper()
	dir := t.TempDir()
	writeConfig(t, filepath.Join(dir, "s3-secret"), "s3-secret-value\n")
	writeConfig(t, filepath.Join(dir, "token"), "admin-token-value\n")
	path := filepath.Join(dir, "config.hcl")
	writeConfig(t, path, content+testOpenAccess)
	cfg, err := loadConfig(path)
	return cfg, dir, err
}

func TestUsageConfigGarage(t *testing.T) {
	t.Parallel()
	cfg, _, err := loadUsageConfig(t, usageConfigText(usageBlock(`
    source     = "garage"
    endpoint   = "http://garage.local:3903"
    token_file = "./token"`), ""))
	if err != nil {
		t.Fatal(err)
	}
	want := usageConfig{Source: "garage", Endpoint: "http://garage.local:3903", Token: "admin-token-value"}
	if got := cfg.Authentications[0].Usage; got == nil || *got != want {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
	if cfg.Buckets[0].QuotaBytes != 0 {
		t.Fatalf("quota_bytes = %d", cfg.Buckets[0].QuotaBytes)
	}
}

func TestUsageConfigGarageTokenFromTheEnvironment(t *testing.T) {
	t.Setenv("USAGE_TEST_GARAGE_TOKEN", "token-from-the-environment")
	cfg, _, err := loadUsageConfig(t, usageConfigText(usageBlock(`
    source    = "garage"
    endpoint  = "http://garage.local:3903"
    token_env = "USAGE_TEST_GARAGE_TOKEN"`), ""))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Authentications[0].Usage; got == nil || got.Token != "token-from-the-environment" {
		t.Fatalf("usage = %+v", got)
	}
	text := formatEffectiveConfiguration(cfg)
	if !strings.Contains(text, `auth "x".usage.token = <hidden>  # env:USAGE_TEST_GARAGE_TOKEN`) || strings.Contains(text, "token-from-the-environment") {
		t.Fatalf("print-config:\n%s", text)
	}
}

func TestUsageConfigSeaweedFS(t *testing.T) {
	t.Parallel()
	cfg, _, err := loadUsageConfig(t, usageConfigText(usageBlock(`
    source         = "seaweedfs"
    endpoint       = "http://master.local:9333"
    filer_endpoint = "https://filer.local:8888"`), ""))
	if err != nil {
		t.Fatal(err)
	}
	want := usageConfig{Source: "seaweedfs", Endpoint: "http://master.local:9333", FilerEndpoint: "https://filer.local:8888"}
	if got := cfg.Authentications[0].Usage; got == nil || *got != want {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
}

// The endpoint of the MinIO source is the endpoint of the auth block, unless the block sets it.
func TestUsageConfigMinIO(t *testing.T) {
	t.Parallel()
	cfg, _, err := loadUsageConfig(t, usageConfigText(usageBlock(`    source = "minio"`), ""))
	if err != nil {
		t.Fatal(err)
	}
	want := usageConfig{Source: "minio", Endpoint: "http://minio.local:9000"}
	if got := cfg.Authentications[0].Usage; got == nil || *got != want {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
	cfg, _, err = loadUsageConfig(t, usageConfigText(usageBlock(`
    source   = "minio"
    endpoint = "https://console.local:9001"`), ""))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Authentications[0].Usage; got == nil || got.Endpoint != "https://console.local:9001" {
		t.Fatalf("usage = %+v", got)
	}
}

func TestUsageConfigNoBlockMeansNoUsage(t *testing.T) {
	t.Parallel()
	cfg, _, err := loadUsageConfig(t, usageConfigText(usageAuthLines, ""))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Authentications[0].Usage != nil {
		t.Fatalf("usage = %+v", cfg.Authentications[0].Usage)
	}
	if text := formatEffectiveConfiguration(cfg); strings.Contains(text, ".usage.") {
		t.Fatalf("print-config shows a usage key without a block:\n%s", text)
	}
}

// Each error stops the start. The message names the key and the value. A secret never shows.
func TestUsageConfigErrors(t *testing.T) {
	t.Parallel()
	garage := "    source = \"garage\"\n    endpoint = \"http://garage.local:3903\"\n    token_file = \"./token\"\n"
	gcs := "\n  provider = \"gcs\"\n  mode = \"service_account\"\n  credentials_file = \"./gcs.json\"\n  usage {\n    source = \"garage\"\n    endpoint = \"http://g:3903\"\n    token_file = \"./token\"\n  }\n"
	anonymous := "\n  provider = \"s3\"\n  mode = \"anonymous\"\n  endpoint = \"http://minio.local:9000\"\n  region = \"test\"\n  usage {\n    source = \"minio\"\n  }\n"
	for name, test := range map[string]struct {
		config string
		want   []string
		hidden string
	}{
		"missing source": {usageConfigText(usageBlock(`    endpoint = "http://g:3903"`), ""),
			[]string{`auth "x".usage.source is required`, `"garage"`, `"seaweedfs"`, `"minio"`}, ""},
		"unknown source": {usageConfigText(usageBlock(`    source = "ceph"`), ""),
			[]string{`auth "x".usage.source must be`, `got "ceph"`}, ""},
		"upper-case source": {usageConfigText(usageBlock(`    source = "Garage"`), ""),
			[]string{`auth "x".usage.source must be`, "lower case", `got "Garage"`}, ""},
		"source with a space": {usageConfigText(usageBlock(`    source = "garage "`), ""),
			[]string{`auth "x".usage.source`}, ""},
		"unknown attribute": {usageConfigText(usageBlock(garage+`    timeout = "5s"`), ""),
			[]string{`unknown attribute "timeout" in usage block`}, ""},
		"duplicate attribute": {usageConfigText(usageBlock(garage+`    endpoint = "http://other:3903"`), ""),
			[]string{"endpoint"}, ""},
		"duplicate usage block": {usageConfigText(usageBlock(garage)+"  usage {\n    source = \"minio\"\n  }\n", ""),
			[]string{`only one usage block is allowed in auth "x"`}, ""},
		"unknown nested block": {usageConfigText(usageAuthLines+"  other {\n    a = \"b\"\n  }\n", ""),
			[]string{`unknown block "other" in auth; the only block is usage`}, ""},
		"nested block in usage": {usageConfigText(usageBlock(garage+"    sub {\n      a = \"b\"\n    }"), ""),
			[]string{`nested blocks are not supported inside auth "x".usage`}, ""},
		"label on usage": {usageConfigText(usageAuthLines+"  usage \"y\" {\n    source = \"minio\"\n  }\n", ""),
			[]string{`auth "x".usage block does not accept labels`}, ""},
		"token in clear text": {usageConfigText(usageBlock("    source = \"garage\"\n    endpoint = \"http://g:3903\"\n    token = \"x-clear-token\""), ""),
			[]string{`auth "x".usage.token is a secret`, "token_env", "token_file"}, "x-clear-token"},
		"token env variable is missing": {usageConfigText(usageBlock("    source = \"garage\"\n    endpoint = \"http://g:3903\"\n    token_env = \"USAGE_TEST_NO_SUCH_VARIABLE\""), ""),
			[]string{"token", "USAGE_TEST_NO_SUCH_VARIABLE"}, ""},
		"token file is missing": {usageConfigText(usageBlock("    source = \"garage\"\n    endpoint = \"http://g:3903\"\n    token_file = \"./no-such-file\""), ""),
			[]string{"token", "no-such-file"}, ""},
		"two token forms": {usageConfigText(usageBlock(garage+"    token_env = \"X\""), ""),
			[]string{"token"}, "admin-token-value"},
		"garage without token": {usageConfigText(usageBlock("    source = \"garage\"\n    endpoint = \"http://g:3903\""), ""),
			[]string{`auth "x".usage.token is required with source "garage"`, "token_env", "token_file"}, ""},
		"garage without endpoint": {usageConfigText(usageBlock("    source = \"garage\"\n    token_file = \"./token\""), ""),
			[]string{`auth "x".usage.endpoint is required with source "garage"`}, ""},
		"garage with filer": {usageConfigText(usageBlock(garage+`    filer_endpoint = "http://f:8888"`), ""),
			[]string{`auth "x".usage.filer_endpoint is only for source "seaweedfs"`}, ""},
		"seaweedfs without endpoint": {usageConfigText(usageBlock("    source = \"seaweedfs\"\n    filer_endpoint = \"http://f:8888\""), ""),
			[]string{`auth "x".usage.endpoint is required with source "seaweedfs"`}, ""},
		"seaweedfs without filer": {usageConfigText(usageBlock("    source = \"seaweedfs\"\n    endpoint = \"http://m:9333\""), ""),
			[]string{`auth "x".usage.filer_endpoint is required with source "seaweedfs"`}, ""},
		"seaweedfs with token": {usageConfigText(usageBlock("    source = \"seaweedfs\"\n    endpoint = \"http://m:9333\"\n    filer_endpoint = \"http://f:8888\"\n    token_file = \"./token\""), ""),
			[]string{`auth "x".usage.token is only for source "garage"`}, "admin-token-value"},
		"minio with token": {usageConfigText(usageBlock("    source = \"minio\"\n    token_file = \"./token\""), ""),
			[]string{`auth "x".usage.token is only for source "garage"`}, "admin-token-value"},
		"minio with filer": {usageConfigText(usageBlock("    source = \"minio\"\n    filer_endpoint = \"http://f:8888\""), ""),
			[]string{`auth "x".usage.filer_endpoint is only for source "seaweedfs"`}, ""},
		"minio with anonymous mode": {usageConfigText(anonymous, ""),
			[]string{`source "minio" needs auth "x" with mode "access_key"`, `got mode "anonymous"`}, ""},
		"usage on a gcs auth": {usageConfigText(gcs, ""),
			[]string{`a usage block needs provider "s3"`, `auth "x" has provider "gcs"`}, ""},
		"endpoint with a slash at the end": {usageConfigText(usageBlock("    source = \"garage\"\n    endpoint = \"http://g:3903/\"\n    token_file = \"./token\""), ""),
			[]string{`auth "x".usage.endpoint`, `must not end with "/"`, `got "http://g:3903/"`}, ""},
		"endpoint with a query": {usageConfigText(usageBlock("    source = \"garage\"\n    endpoint = \"http://g:3903?a=b\"\n    token_file = \"./token\""), ""),
			[]string{`auth "x".usage.endpoint`, "query string or fragment", `got "http://g:3903?a=b"`}, ""},
		"endpoint with a fragment": {usageConfigText(usageBlock("    source = \"garage\"\n    endpoint = \"http://g:3903#a\"\n    token_file = \"./token\""), ""),
			[]string{`auth "x".usage.endpoint`, "query string or fragment"}, ""},
		"endpoint with a user": {usageConfigText(usageBlock("    source = \"garage\"\n    endpoint = \"http://user:pass@g:3903\"\n    token_file = \"./token\""), ""),
			[]string{`auth "x".usage.endpoint`, "user information"}, ""},
		"endpoint with a wrong scheme": {usageConfigText(usageBlock("    source = \"garage\"\n    endpoint = \"ftp://g:3903\"\n    token_file = \"./token\""), ""),
			[]string{`auth "x".usage.endpoint`, "http or https"}, ""},
		"filer endpoint with a slash at the end": {usageConfigText(usageBlock("    source = \"seaweedfs\"\n    endpoint = \"http://m:9333\"\n    filer_endpoint = \"http://f:8888/\""), ""),
			[]string{`auth "x".usage.filer_endpoint`, `must not end with "/"`}, ""},
		"wrong type": {usageConfigText(usageBlock("    source = \"garage\"\n    endpoint = 3903"), ""),
			[]string{"endpoint"}, ""},
		"quota 0": {usageConfigText(usageBlock(garage), `quota_bytes = 0`),
			[]string{`bucket "b".quota_bytes must be between 1 and 1152921504606846976, got 0`}, ""},
		"quota negative": {usageConfigText(usageBlock(garage), `quota_bytes = -5`),
			[]string{"unexpected character '-'"}, ""},
		"quota too large": {usageConfigText(usageBlock(garage), `quota_bytes = 1152921504606846977`),
			[]string{`bucket "b".quota_bytes must be between 1 and 1152921504606846976`}, ""},
		"quota is text": {usageConfigText(usageBlock(garage), `quota_bytes = "10"`),
			[]string{"quota_bytes"}, ""},
		"quota without a usage block": {usageConfigText(usageAuthLines, `quota_bytes = 1000`),
			[]string{`bucket "b".quota_bytes needs a usage block in auth "x"`}, ""},
	} {
		dir := t.TempDir()
		writeConfig(t, filepath.Join(dir, "s3-secret"), "s3-secret-value\n")
		writeConfig(t, filepath.Join(dir, "token"), "admin-token-value\n")
		writeConfig(t, filepath.Join(dir, "gcs.json"), `{"type":"service_account"}`)
		_, err := decodeConfig(test.config+testOpenAccess, filepath.Join(dir, "config.hcl"), dir)
		if err == nil {
			t.Errorf("%s: the start must stop", name)
			continue
		}
		for _, want := range test.want {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error = %v, want %q", name, err, want)
			}
		}
		if test.hidden != "" && strings.Contains(err.Error(), test.hidden) {
			t.Errorf("%s: the error shows the secret: %v", name, err)
		}
	}
}

func TestUsageConfigQuotaBytes(t *testing.T) {
	t.Parallel()
	garage := "    source = \"garage\"\n    endpoint = \"http://g:3903\"\n    token_file = \"./token\""
	for name, test := range map[string]struct {
		line string
		want int64
	}{
		"smallest":   {`quota_bytes = 1`, 1},
		"ten GiB":    {`quota_bytes = 10737418240`, 10737418240},
		"largest":    {`quota_bytes = 1152921504606846976`, 1 << 60},
		"not in use": {``, 0},
	} {
		cfg, _, err := loadUsageConfig(t, usageConfigText(usageBlock(garage), test.line))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := cfg.Buckets[0].QuotaBytes; got != test.want {
			t.Fatalf("%s: quota_bytes = %d, want %d", name, got, test.want)
		}
	}
}

// print-config shows the block, the source of each value and the key of the bucket. It hides the token.
func TestUsageConfigPrintConfig(t *testing.T) {
	t.Parallel()
	cfg, dir, err := loadUsageConfig(t, usageConfigText(usageBlock(`
    source     = "garage"
    endpoint   = "http://garage.local:3903"
    token_file = "./token"`), "quota_bytes = 1048576"))
	if err != nil {
		t.Fatal(err)
	}
	text := formatEffectiveConfiguration(cfg)
	for _, want := range []string{
		`auth "x".usage.source = "garage"  # file`,
		`auth "x".usage.endpoint = "http://garage.local:3903"  # file`,
		`auth "x".usage.filer_endpoint = <none>`,
		`auth "x".usage.token = <hidden>  # file:` + filepath.Join(dir, "token"),
		`bucket "b".quota_bytes = 1048576  # file`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("print-config does not show %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "admin-token-value") {
		t.Fatalf("print-config shows the token:\n%s", text)
	}

	// Without the key, the bucket shows the default.
	cfg, _, err = loadUsageConfig(t, usageConfigText(usageBlock(`    source = "minio"`), ""))
	if err != nil {
		t.Fatal(err)
	}
	text = formatEffectiveConfiguration(cfg)
	for _, want := range []string{`auth "x".usage.source = "minio"  # file`, `auth "x".usage.endpoint = "http://minio.local:9000"`, `bucket "b".quota_bytes = `} {
		if !strings.Contains(text, want) {
			t.Errorf("print-config does not show %q:\n%s", want, text)
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, `bucket "b".quota_bytes = `) && !strings.HasSuffix(line, "# default") {
			t.Errorf("quota_bytes without the key is not a default: %q", line)
		}
		if strings.HasPrefix(line, `auth "x".usage.token = `) && !strings.Contains(line, "<none>") {
			t.Errorf("token of the minio source: %q", line)
		}
	}
}

// The reference documents the keys of the block, and it tells which key takes _env and _file.
func TestUsageConfigIsInTheKeyTable(t *testing.T) {
	t.Parallel()
	reference := configReference()
	for _, want := range []string{"auth.usage", "`source`", "`filer_endpoint`", "`token`", "`quota_bytes`"} {
		if !strings.Contains(reference, want) {
			t.Errorf("the configuration reference does not contain %q", want)
		}
	}
	var token *configKey
	for index := range configKeys {
		if configKeys[index].Block == "auth.usage" && configKeys[index].Name == "token" {
			token = &configKeys[index]
		}
	}
	if token == nil || !token.Secret || token.Forms != formEnv|formFile {
		t.Fatalf("key token of auth.usage = %+v", token)
	}
}

// The directory of the configuration file is not needed when the token comes from a file with an absolute path.
func TestUsageConfigTokenFileWithAnAbsolutePath(t *testing.T) {
	t.Parallel()
	tokenDir := t.TempDir()
	tokenPath := filepath.Join(tokenDir, "t")
	if err := os.WriteFile(tokenPath, []byte("absolute-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := loadUsageConfig(t, usageConfigText(usageBlock(`
    source     = "garage"
    endpoint   = "http://garage.local:3903"
    token_file = "`+tokenPath+`"`), ""))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Authentications[0].Usage.Token; got != "absolute-token" {
		t.Fatalf("token = %q", got)
	}
}
