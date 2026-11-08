package main

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testPDPToken = "pdp-test-token-0123456789abcdef0123456789"

	accessTestStorage = `
auth "public" {
  provider = "s3"
  mode     = "anonymous"
  endpoint = "http://localhost:9000"
  region   = "test"
}

bucket "logs" {
  auth   = "public"
  bucket = "logs"
}
`

	goodIdentity = `
    issuer           = "https://edge.example"
    audience         = "infra-s3"
    public_keys_file = "jwks.json"
`
	goodPDP = `
    url        = "https://pdp.example:4182"
    token_file = "pdp-token"
`
)

// authzenText gives an access block with mode authzen. identity and pdp are the bodies of the two blocks.
func authzenText(identity, pdp string) string {
	return "access {\n  mode = \"authzen\"\n  identity {" + identity + "\n  }\n  pdp {" + pdp + "\n  }\n}\n"
}

// decodeAccessTestConfig writes the key set and the token file in a new directory, and decodes the storage blocks
// with the text. files gives more files of the directory.
func decodeAccessTestConfig(t *testing.T, text string, files map[string]string) (appConfig, string, error) {
	t.Helper()
	dir := t.TempDir()
	writeConfig(t, filepath.Join(dir, "jwks.json"), keySet(edJWK(`,"kid":"edge-1"`), ecJWK(`,"kid":"edge-2"`)))
	writeConfig(t, filepath.Join(dir, "pdp-token"), testPDPToken+"\n")
	for name, content := range files {
		writeConfig(t, filepath.Join(dir, name), content)
	}
	cfg, err := decodeConfig(accessTestStorage+text, filepath.Join(dir, "config.hcl"), dir)
	return cfg, dir, err
}

func TestAccessConfigAuthZENValuesAndDefaults(t *testing.T) {
	t.Parallel()
	cfg, _, err := decodeAccessTestConfig(t, authzenText(goodIdentity, goodPDP), nil)
	if err != nil {
		t.Fatal(err)
	}
	access := cfg.Access
	if !access.enforced() || access.Mode != accessModeAuthZEN {
		t.Fatalf("mode = %q", access.Mode)
	}
	if access.Header != "X-Identity-Token" || access.Issuer != "https://edge.example" || access.Audience != "infra-s3" {
		t.Fatalf("identity = %+v", access)
	}
	if len(access.Keys) != 2 || access.Keys[0].display() != "Ed25519 kid=edge-1" || access.Keys[1].display() != "P-256 kid=edge-2" {
		t.Fatalf("keys = %+v", access.Keys)
	}
	if access.MaxAge != 5*time.Minute || access.ClockSkew != 60*time.Second {
		t.Fatalf("max_age = %v, clock_skew = %v", access.MaxAge, access.ClockSkew)
	}
	if access.PDPURL != "https://pdp.example:4182" || access.PDPPlainHTTP || access.PDPToken != testPDPToken {
		t.Fatalf("pdp = %+v", access)
	}
	if access.PDPTimeout != 500*time.Millisecond || access.PDPUsageTimeout != 2*time.Second || access.PDPCacheTTL != 30*time.Second || access.PDPMaxEvaluations != 64 {
		t.Fatalf("pdp defaults = %+v", access)
	}
}

func TestAccessConfigAuthZENExplicitValues(t *testing.T) {
	t.Parallel()
	identity := goodIdentity + `
    header     = "X-Edge-Token"
    max_age    = "1h"
    clock_skew = "2m"
`
	pdp := `
    url             = "http://pdp.internal:4182/base"
    plain_http      = true
    token_file      = "pdp-token"
    timeout         = "1s"
    usage_timeout   = "1m"
    cache_ttl       = "0s"
    max_evaluations = 1024
`
	cfg, _, err := decodeAccessTestConfig(t, authzenText(identity, pdp), nil)
	if err != nil {
		t.Fatal(err)
	}
	access := cfg.Access
	if access.Header != "X-Edge-Token" || access.MaxAge != time.Hour || access.ClockSkew != 2*time.Minute {
		t.Fatalf("identity = %+v", access)
	}
	if access.PDPURL != "http://pdp.internal:4182/base" || !access.PDPPlainHTTP || access.PDPTimeout != time.Second ||
		access.PDPUsageTimeout != time.Minute || access.PDPCacheTTL != 0 || access.PDPMaxEvaluations != 1024 {
		t.Fatalf("pdp = %+v", access)
	}
}

func TestAccessConfigBoundsAreAccepted(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct{ identity, pdp string }{
		"lower bounds": {
			goodIdentity + "max_age = \"1s\"\nclock_skew = \"0s\"\nheader = \"A\"",
			goodPDP + "timeout = \"10ms\"\nusage_timeout = \"100ms\"\ncache_ttl = \"0s\"\nmax_evaluations = 1",
		},
		"upper bounds": {
			goodIdentity + "max_age = \"24h\"\nclock_skew = \"5m\"",
			goodPDP + "timeout = \"1m\"\nusage_timeout = \"1m\"\ncache_ttl = \"1h\"",
		},
		"clock_skew in whole seconds with ms": {
			goodIdentity + "clock_skew = \"2000ms\"",
			goodPDP,
		},
		"usage_timeout equal to timeout": {
			goodIdentity,
			goodPDP + "timeout = \"2s\"",
		},
	} {
		if _, _, err := decodeAccessTestConfig(t, authzenText(test.identity, test.pdp), nil); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestAccessConfigOpenMode(t *testing.T) {
	t.Parallel()
	cfg, _, err := decodeAccessTestConfig(t, "access {\n  mode = \"open\"\n}\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Access.enforced() || cfg.Access.Mode != accessModeOpen || len(cfg.Access.Keys) != 0 || cfg.Access.PDPURL != "" {
		t.Fatalf("access = %+v", cfg.Access)
	}
}

func TestAccessConfigRefusals(t *testing.T) {
	t.Parallel()
	ident := func(old, new string) string { return strings.Replace(goodIdentity, old, new, 1) }
	pdpWith := func(old, new string) string { return strings.Replace(goodPDP, old, new, 1) }
	longText := func(n int) string { return strings.Repeat("a", n) }
	for name, test := range map[string]struct {
		text string
		want string
	}{
		// The blocks.
		"no access block":            {"", "the access block is required"},
		"two access blocks":          {"access { mode = \"open\" }\naccess { mode = \"open\" }\n", "only one access block is allowed"},
		"access with a label":        {"access \"x\" { mode = \"open\" }\n", "access block does not accept labels"},
		"no mode":                    {"access {\n}\n", "access.mode is required"},
		"mode AuthZEN":               {"access { mode = \"AuthZEN\" }\n", `access.mode must be "authzen" or "open" (lower case), got "AuthZEN"`},
		"mode none":                  {"access { mode = \"none\" }\n", `got "none"`},
		"mode empty":                 {"access { mode = \"\" }\n", `got ""`},
		"mode is a number":           {"access { mode = 1 }\n", `attribute "mode" must be a quoted string`},
		"unknown attribute":          {"access {\n  mode = \"open\"\n  modes = \"x\"\n}\n", `unknown attribute "modes" in access block`},
		"unknown nested block":       {"access {\n  mode = \"open\"\n  policy {}\n}\n", `unknown block "policy" in access`},
		"two identity blocks":        {"access {\n  mode = \"authzen\"\n  identity {}\n  identity {}\n}\n", "only one access.identity block is allowed"},
		"two pdp blocks":             {"access {\n  mode = \"authzen\"\n  pdp {}\n  pdp {}\n}\n", "only one access.pdp block is allowed"},
		"identity with label":        {"access {\n  mode = \"authzen\"\n  identity \"x\" {}\n}\n", "access.identity block does not accept labels"},
		"pdp with label":             {"access {\n  mode = \"authzen\"\n  pdp \"x\" {}\n}\n", "access.pdp block does not accept labels"},
		"block inside identity":      {authzenText(goodIdentity+"\n    keys {}", goodPDP), "nested blocks are not supported inside access.identity"},
		"block inside pdp":           {authzenText(goodIdentity, goodPDP+"\n    cache {}"), "nested blocks are not supported inside access.pdp"},
		"unknown identity attribute": {authzenText(goodIdentity+"\n    leeway = \"1s\"", goodPDP), `unknown attribute "leeway" in identity block`},
		"unknown pdp attribute":      {authzenText(goodIdentity, goodPDP+"\n    retries = 3"), `unknown attribute "retries" in pdp block`},

		// The contradictions between the mode and the blocks.
		"open with identity":  {"access {\n  mode = \"open\"\n  identity {" + goodIdentity + "}\n}\n", `access.mode = "open" contradicts the access.identity block`},
		"open with pdp":       {"access {\n  mode = \"open\"\n  pdp {" + goodPDP + "}\n}\n", `access.mode = "open" contradicts the access.pdp block`},
		"authzen no blocks":   {"access { mode = \"authzen\" }\n", `access.mode = "authzen" needs the access.identity block`},
		"authzen no pdp":      {"access {\n  mode = \"authzen\"\n  identity {" + goodIdentity + "}\n}\n", `access.mode = "authzen" needs the access.pdp block`},
		"authzen no identity": {"access {\n  mode = \"authzen\"\n  pdp {" + goodPDP + "}\n}\n", `access.mode = "authzen" needs the access.identity block`},

		// access.identity.
		"header Cookie":          {authzenText(goodIdentity+"\n    header = \"Cookie\"", goodPDP), "access.identity.header: must not be Cookie, Authorization"},
		"header authorization":   {authzenText(goodIdentity+"\n    header = \"authorization\"", goodPDP), `got "authorization"`},
		"header Host":            {authzenText(goodIdentity+"\n    header = \"HOST\"", goodPDP), "access.identity.header"},
		"header Content-Type":    {authzenText(goodIdentity+"\n    header = \"content-type\"", goodPDP), "access.identity.header"},
		"header Content-Length":  {authzenText(goodIdentity+"\n    header = \"Content-Length\"", goodPDP), "access.identity.header"},
		"header with underscore": {authzenText(goodIdentity+"\n    header = \"X_Identity\"", goodPDP), "must have only letters, digits"},
		"header with a space":    {authzenText(goodIdentity+"\n    header = \"X Identity\"", goodPDP), "must have only letters, digits"},
		"header empty":           {authzenText(goodIdentity+"\n    header = \"\"", goodPDP), "must have 1 to 64 characters"},
		"header 65 characters":   {authzenText(goodIdentity+"\n    header = \""+longText(65)+"\"", goodPDP), "must have 1 to 64 characters"},
		"no issuer":              {authzenText(ident(`issuer           = "https://edge.example"`, ""), goodPDP), "access.identity.issuer is required"},
		"issuer empty":           {authzenText(ident(`"https://edge.example"`, `""`), goodPDP), "access.identity.issuer must have 1 to 1024 bytes"},
		"issuer 1025 bytes":      {authzenText(ident(`"https://edge.example"`, `"`+longText(1025)+`"`), goodPDP), "access.identity.issuer must have 1 to 1024 bytes"},
		"issuer with control":    {authzenText(ident(`"https://edge.example"`, `"edge\u0001"`), goodPDP), "access.identity.issuer must have 1 to 1024 bytes"},
		"no audience":            {authzenText(ident(`audience         = "infra-s3"`, ""), goodPDP), "access.identity.audience is required"},
		"audience empty":         {authzenText(ident(`"infra-s3"`, `""`), goodPDP), "access.identity.audience must have 1 to 256 bytes"},
		"audience 257 bytes":     {authzenText(ident(`"infra-s3"`, `"`+longText(257)+`"`), goodPDP), "access.identity.audience must have 1 to 256 bytes"},
		"audience with control":  {authzenText(ident(`"infra-s3"`, `"a\u0007b"`), goodPDP), "access.identity.audience must have 1 to 256 bytes"},
		"no public keys":         {authzenText(ident(`public_keys_file = "jwks.json"`, ""), goodPDP), "access.identity.public_keys is required"},
		"public keys not a key":  {authzenText(ident(`public_keys_file = "jwks.json"`, `public_keys = "not a key"`), goodPDP), "access.identity.public_keys: line 1 is not -----BEGIN PUBLIC KEY-----"},
		"public keys bad JWK":    {authzenText(ident(`public_keys_file = "jwks.json"`, `public_keys = "{\"keys\":"`), goodPDP), "access.identity.public_keys: the JWK Set is not valid JSON"},
		"public keys file":       {authzenText(ident(`"jwks.json"`, `"missing.json"`), goodPDP), "public_keys_file: cannot read"},
		"public keys two forms":  {authzenText(ident(`public_keys_file = "jwks.json"`, "public_keys_file = \"jwks.json\"\n    public_keys_env = \"X\""), goodPDP), "give only one of public_keys, public_keys_env and public_keys_file"},
		"public keys bad key":    {authzenText(ident(`public_keys_file = "jwks.json"`, `public_keys = "{\"keys\":[{\"kty\":\"RSA\"}]}"`), goodPDP), "access.identity.public_keys: key 1: kty"},
		"issuer is a number":     {authzenText(ident(`"https://edge.example"`, `5`), goodPDP), `attribute "issuer" must be a quoted string`},

		// The durations: the form.
		"max_age 1.5s":       {authzenText(goodIdentity+"\n    max_age = \"1.5s\"", goodPDP), `access.identity.max_age is not a duration`},
		"max_age +5s":        {authzenText(goodIdentity+"\n    max_age = \"+5s\"", goodPDP), `got "+5s"`},
		"max_age -5s":        {authzenText(goodIdentity+"\n    max_age = \"-5s\"", goodPDP), `got "-5s"`},
		"max_age 1m30s":      {authzenText(goodIdentity+"\n    max_age = \"1m30s\"", goodPDP), `got "1m30s"`},
		"max_age no unit":    {authzenText(goodIdentity+"\n    max_age = \"5\"", goodPDP), `got "5"`},
		"max_age empty":      {authzenText(goodIdentity+"\n    max_age = \"\"", goodPDP), `got ""`},
		"max_age leading 0":  {authzenText(goodIdentity+"\n    max_age = \"05s\"", goodPDP), `got "05s"`},
		"max_age with space": {authzenText(goodIdentity+"\n    max_age = \"5 s\"", goodPDP), `got "5 s"`},
		"max_age upper case": {authzenText(goodIdentity+"\n    max_age = \"5M\"", goodPDP), `got "5M"`},
		"max_age day unit":   {authzenText(goodIdentity+"\n    max_age = \"1d\"", goodPDP), `got "1d"`},
		"max_age huge":       {authzenText(goodIdentity+"\n    max_age = \"99999999999999999999s\"", goodPDP), "is too large"},
		"max_age overflow":   {authzenText(goodIdentity+"\n    max_age = \"9223372036854775807h\"", goodPDP), "is too large"},
		"max_age a number":   {authzenText(goodIdentity+"\n    max_age = 300", goodPDP), `attribute "max_age" must be a quoted string`},

		// The durations: the ranges.
		"max_age 0s":               {authzenText(goodIdentity+"\n    max_age = \"0s\"", goodPDP), `access.identity.max_age must be from 1s to 24h, got "0s"`},
		"max_age 999ms":            {authzenText(goodIdentity+"\n    max_age = \"999ms\"", goodPDP), `access.identity.max_age must be from 1s to 24h`},
		"max_age 25h":              {authzenText(goodIdentity+"\n    max_age = \"25h\"", goodPDP), `access.identity.max_age must be from 1s to 24h, got "25h"`},
		"clock_skew 6m":            {authzenText(goodIdentity+"\n    clock_skew = \"6m\"", goodPDP), `access.identity.clock_skew must be from 0s to 5m, got "6m"`},
		"clock_skew 1500ms":        {authzenText(goodIdentity+"\n    clock_skew = \"1500ms\"", goodPDP), `access.identity.clock_skew must be a whole number of seconds, got "1500ms"`},
		"timeout 9ms":              {authzenText(goodIdentity, goodPDP+"\n    timeout = \"9ms\""), `access.pdp.timeout must be from 10ms to 1m, got "9ms"`},
		"timeout 61s":              {authzenText(goodIdentity, goodPDP+"\n    timeout = \"61s\""), `access.pdp.timeout must be from 10ms to 1m, got "61s"`},
		"timeout 0s":               {authzenText(goodIdentity, goodPDP+"\n    timeout = \"0s\""), `access.pdp.timeout must be from 10ms to 1m`},
		"usage_timeout 99ms":       {authzenText(goodIdentity, goodPDP+"\n    usage_timeout = \"99ms\""), `access.pdp.usage_timeout must be from 100ms to 1m, got "99ms"`},
		"usage_timeout 61s":        {authzenText(goodIdentity, goodPDP+"\n    usage_timeout = \"61s\""), `access.pdp.usage_timeout must be from 100ms to 1m, got "61s"`},
		"cache_ttl 2h":             {authzenText(goodIdentity, goodPDP+"\n    cache_ttl = \"2h\""), `access.pdp.cache_ttl must be from 0s to 1h, got "2h"`},
		"cache_ttl 1.5s":           {authzenText(goodIdentity, goodPDP+"\n    cache_ttl = \"1.5s\""), `access.pdp.cache_ttl is not a duration`},
		"max_evaluations 0":        {authzenText(goodIdentity, goodPDP+"\n    max_evaluations = 0"), "access.pdp.max_evaluations must be between 1 and 1024, got 0"},
		"max_evaluations 1025":     {authzenText(goodIdentity, goodPDP+"\n    max_evaluations = 1025"), "access.pdp.max_evaluations must be between 1 and 1024, got 1025"},
		"max_evaluations a string": {authzenText(goodIdentity, goodPDP+"\n    max_evaluations = \"64\""), `attribute "max_evaluations" must be an integer`},
		"plain_http a string":      {authzenText(goodIdentity, goodPDP+"\n    plain_http = \"false\""), `attribute "plain_http" must be a boolean`},

		// access.pdp.url.
		"no url":                  {authzenText(goodIdentity, pdpWith(`url        = "https://pdp.example:4182"`, "")), "access.pdp.url is required"},
		"url without scheme":      {authzenText(goodIdentity, pdpWith(`"https://pdp.example:4182"`, `"pdp.example:4182"`)), "access.pdp.url: must start with http:// or https://"},
		"url ftp":                 {authzenText(goodIdentity, pdpWith(`"https://pdp.example:4182"`, `"ftp://pdp.example"`)), "must start with http:// or https://"},
		"url upper case HTTP":     {authzenText(goodIdentity, pdpWith(`"https://pdp.example:4182"`, `"HTTPS://pdp.example"`)), "must start with http:// or https://"},
		"url no host":             {authzenText(goodIdentity, pdpWith(`"https://pdp.example:4182"`, `"https://"`)), "must have a host"},
		"url no host, port":       {authzenText(goodIdentity, pdpWith(`"https://pdp.example:4182"`, `"https://:4182"`)), "must have a host"},
		"url with user info":      {authzenText(goodIdentity, pdpWith(`"https://pdp.example:4182"`, `"https://user:secret-pass@pdp.example"`)), "must not have credentials"},
		"url with query":          {authzenText(goodIdentity, pdpWith(`"https://pdp.example:4182"`, `"https://pdp.example?key=secret-value"`)), "must not have a query"},
		"url with empty query":    {authzenText(goodIdentity, pdpWith(`"https://pdp.example:4182"`, `"https://pdp.example?"`)), "must not have a query"},
		"url with fragment":       {authzenText(goodIdentity, pdpWith(`"https://pdp.example:4182"`, `"https://pdp.example#top"`)), "must not have a fragment"},
		"url with empty fragment": {authzenText(goodIdentity, pdpWith(`"https://pdp.example:4182"`, `"https://pdp.example#"`)), "must not have a fragment"},
		"url trailing slash":      {authzenText(goodIdentity, pdpWith(`"https://pdp.example:4182"`, `"https://pdp.example:4182/"`)), `must not end with "/"`},
		"url path trailing slash": {authzenText(goodIdentity, pdpWith(`"https://pdp.example:4182"`, `"https://pdp.example/base/"`)), `must not end with "/"`},
		"url with a bad port":     {authzenText(goodIdentity, pdpWith(`"https://pdp.example:4182"`, `"https://pdp.example:port"`)), "is not a valid URL"},
		"url file missing":        {authzenText(goodIdentity, pdpWith(`url        = "https://pdp.example:4182"`, `url_file = "no-url"`)), "url_file: cannot read"},

		// access.pdp.plain_http.
		"http url, plain_http false":     {authzenText(goodIdentity, pdpWith(`"https://pdp.example:4182"`, `"http://pdp.example:4182"`)), "access.pdp.url is an http URL, but access.pdp.plain_http = false"},
		"http url, plain_http set false": {authzenText(goodIdentity, pdpWith(`"https://pdp.example:4182"`, `"http://pdp.example:4182"`)+"\n    plain_http = false"), "plain_http = false"},
		"https url, plain_http true":     {authzenText(goodIdentity, goodPDP+"\n    plain_http = true"), "access.pdp.url is an https URL, but access.pdp.plain_http = true"},

		// access.pdp.token.
		"clear text token":       {authzenText(goodIdentity, pdpWith(`token_file = "pdp-token"`, `token = "clear-text-token-value"`)), "access.pdp.token is a secret: use token_env or token_file"},
		"no token":               {authzenText(goodIdentity, pdpWith(`token_file = "pdp-token"`, "")), "access.pdp.token is required"},
		"token two forms":        {authzenText(goodIdentity, goodPDP+"\n    token_env = \"X\""), "give only one of token, token_env and token_file"},
		"token file missing":     {authzenText(goodIdentity, pdpWith(`"pdp-token"`, `"no-token"`)), "token_file: cannot read"},
		"token env missing":      {authzenText(goodIdentity, pdpWith(`token_file = "pdp-token"`, `token_env = "STOWR_TEST_NO_SUCH_VARIABLE"`)), `the environment variable "STOWR_TEST_NO_SUCH_VARIABLE" does not exist`},
		"token_file is a number": {authzenText(goodIdentity, pdpWith(`"pdp-token"`, `5`)), `attribute "token_file" must be a quoted string`},

		// The resources of the service.
		"label of 257 bytes": {"access {\n  mode = \"authzen\"\n  identity {" + goodIdentity + "}\n  pdp {" + goodPDP + "}\n}\nbucket \"" + longText(257) + "\" {\n  auth = \"public\"\n  bucket = \"b\"\n}\n",
			`access.mode = "authzen" contradicts the bucket or volume label`},
	} {
		_, _, err := decodeAccessTestConfig(t, test.text, nil)
		if err == nil {
			t.Errorf("%s: the configuration was accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error %q does not contain %q", name, err, test.want)
		}
		for _, secret := range []string{testPDPToken, "clear-text-token-value", "secret-pass", "secret-value"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s: the error shows a secret: %q", name, err)
			}
		}
	}
}

func TestAccessConfigTokenWithASpaceNeverAppearsInTheError(t *testing.T) {
	t.Parallel()
	for name, content := range map[string]string{
		"space":        "zzqx1 zzqx2\n",
		"tab":          "zzqx1\tzzqx2\n",
		"line end":     "zzqx1\nzzqx2\n",
		"control":      "zzqx1\x01zzqx2\n",
		"DEL":          "zzqx1\x7fzzqx2\n",
		"4097 bytes":   strings.Repeat("zzqx", 1025) + "\n",
		"only a space": " \n",
	} {
		_, _, err := decodeAccessTestConfig(t, authzenText(goodIdentity, strings.Replace(goodPDP, `"pdp-token"`, `"bad-token"`, 1)), map[string]string{"bad-token": content})
		if err == nil {
			t.Errorf("%s: the token was accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), "access.pdp.token must have 1 to 4096 bytes without a space or a control character") {
			t.Errorf("%s: error = %q", name, err)
		}
		if strings.Contains(err.Error(), "zzqx") {
			t.Errorf("%s: the error shows a part of the token: %q", name, err)
		}
	}
}

func TestAccessConfigTokenFileLosesOnlyItsLastLineEnd(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct{ content, want string }{
		"line end":    {"abc\n", "abc"},
		"CRLF":        {"abc\r\n", "abc"},
		"no line end": {"abc", "abc"},
		"4096 bytes":  {strings.Repeat("a", 4096), strings.Repeat("a", 4096)},
	} {
		cfg, _, err := decodeAccessTestConfig(t, authzenText(goodIdentity, strings.Replace(goodPDP, `"pdp-token"`, `"other-token"`, 1)), map[string]string{"other-token": test.content})
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if cfg.Access.PDPToken != test.want {
			t.Errorf("%s: token = %q, want %q", name, cfg.Access.PDPToken, test.want)
		}
	}
}

func TestAccessConfigContradictions(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		pdp  string
		want []string
	}{
		"usage_timeout below an explicit timeout": {
			goodPDP + "\n    timeout = \"500ms\"\n    usage_timeout = \"400ms\"",
			[]string{`access.pdp.usage_timeout = "400ms"`, `access.pdp.timeout = "500ms"`},
		},
		"timeout above the default usage_timeout": {
			goodPDP + "\n    timeout = \"5s\"",
			[]string{`access.pdp.usage_timeout = "2s"`, `access.pdp.timeout = "5s"`},
		},
		"http url without plain_http": {
			strings.Replace(goodPDP, "https://", "http://", 1),
			[]string{"access.pdp.url is an http URL", "access.pdp.plain_http = false"},
		},
		"https url with plain_http": {
			goodPDP + "\n    plain_http = true",
			[]string{"access.pdp.url is an https URL", "access.pdp.plain_http = true"},
		},
	} {
		_, _, err := decodeAccessTestConfig(t, authzenText(goodIdentity, test.pdp), nil)
		if err == nil {
			t.Errorf("%s: the configuration was accepted", name)
			continue
		}
		for _, want := range test.want {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error %q does not name %q", name, err, want)
			}
		}
	}
}

// bucketBlocks gives count bucket blocks. The first label has the given prefix.
func bucketBlocks(count int, label func(int) string) string {
	var b strings.Builder
	for i := 0; i < count; i++ {
		fmt.Fprintf(&b, "bucket %q {\n  auth = \"public\"\n  bucket = \"b%d\"\n}\n", label(i), i)
	}
	return b.String()
}

func TestAccessConfigLimitsOfTheResources(t *testing.T) {
	t.Parallel()
	storage := "auth \"public\" {\n  provider = \"s3\"\n  mode = \"anonymous\"\n  endpoint = \"http://localhost:9000\"\n  region = \"test\"\n}\n"
	authzen := authzenText(goodIdentity, goodPDP)
	decode := func(t *testing.T, access, blocks string) error {
		t.Helper()
		dir := t.TempDir()
		writeConfig(t, filepath.Join(dir, "jwks.json"), keySet(edJWK("")))
		writeConfig(t, filepath.Join(dir, "pdp-token"), testPDPToken)
		_, err := decodeConfig(access+storage+blocks, filepath.Join(dir, "config.hcl"), dir)
		return err
	}
	number := func(i int) string { return fmt.Sprintf("bucket-%03d", i) }

	if err := decode(t, authzen, bucketBlocks(256, number)); err != nil {
		t.Fatalf("256 buckets: %v", err)
	}
	err := decode(t, authzen, bucketBlocks(257, number))
	if err == nil || !strings.Contains(err.Error(), `access.mode = "authzen"`) || !strings.Contains(err.Error(), "(257)") {
		t.Fatalf("257 buckets: error = %v", err)
	}
	exact := func(int) string { return strings.Repeat("a", 256) }
	if err := decode(t, authzen, bucketBlocks(1, exact)); err != nil {
		t.Fatalf("a label of 256 bytes: %v", err)
	}
	long := func(int) string { return strings.Repeat("a", 257) }
	err = decode(t, authzen, bucketBlocks(1, long))
	if err == nil || !strings.Contains(err.Error(), `access.mode = "authzen"`) || !strings.Contains(err.Error(), "(257 bytes)") {
		t.Fatalf("a label of 257 bytes: error = %v", err)
	}

	// The buckets and the volumes count together, and a volume label has the same limit.
	folder := t.TempDir()
	volumes := func(count int) string {
		var b strings.Builder
		for i := 0; i < count; i++ {
			fmt.Fprintf(&b, "volume \"volume-%03d\" {\n  path = %q\n  permissions = [\"read\"]\n}\n", i, folder)
		}
		return b.String()
	}
	if err := decode(t, authzen, bucketBlocks(200, number)+volumes(56)); err != nil {
		t.Fatalf("200 buckets and 56 volumes: %v", err)
	}
	err = decode(t, authzen, bucketBlocks(200, number)+volumes(57))
	if err == nil || !strings.Contains(err.Error(), "(257)") {
		t.Fatalf("200 buckets and 57 volumes: error = %v", err)
	}
	longVolume := fmt.Sprintf("volume %q {\n  path = %q\n  permissions = [\"read\"]\n}\n", strings.Repeat("v", 257), folder)
	err = decode(t, authzen, longVolume)
	if err == nil || !strings.Contains(err.Error(), "(257 bytes)") {
		t.Fatalf("a volume label of 257 bytes: error = %v", err)
	}

	// With mode open, the labels are not node ids: no limit applies.
	open := "access { mode = \"open\" }\n"
	if err := decode(t, open, bucketBlocks(300, number)); err != nil {
		t.Fatalf("300 buckets with mode open: %v", err)
	}
	if err := decode(t, open, bucketBlocks(1, long)); err != nil {
		t.Fatalf("a label of 257 bytes with mode open: %v", err)
	}
}

func TestAccessConfigReadsTheEnvironmentOnlyWhenTheFileNamesIt(t *testing.T) {
	jwks := keySet(edJWK(`,"kid":"env-key"`))
	t.Setenv("STOWR_TEST_PDP_TOKEN", "token-from-env")
	t.Setenv("STOWR_TEST_PDP_URL", "http://pdp.internal:4182/base")
	t.Setenv("STOWR_TEST_JWKS", jwks)
	identity := strings.Replace(goodIdentity, `public_keys_file = "jwks.json"`, `public_keys_env = "STOWR_TEST_JWKS"`, 1)
	pdp := `
    url_env    = "STOWR_TEST_PDP_URL"
    plain_http = true
    token_env  = "STOWR_TEST_PDP_TOKEN"
`
	cfg, _, err := decodeAccessTestConfig(t, authzenText(identity, pdp), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Access.PDPToken != "token-from-env" || cfg.Access.PDPURL != "http://pdp.internal:4182/base" || len(cfg.Access.Keys) != 1 || cfg.Access.Keys[0].KID != "env-key" {
		t.Fatalf("access = %+v", cfg.Access)
	}
	printed := formatEffectiveConfiguration(cfg)
	for _, want := range []string{
		`access.pdp.token = <hidden>  # env:STOWR_TEST_PDP_TOKEN`,
		`access.pdp.url = "http://pdp.internal:4182/base"  # env:STOWR_TEST_PDP_URL`,
		`access.identity.public_keys = ["Ed25519 kid=env-key"]  # env:STOWR_TEST_JWKS`,
	} {
		if !strings.Contains(printed, want) {
			t.Errorf("print-config misses %q:\n%s", want, printed)
		}
	}
	if strings.Contains(printed, "token-from-env") {
		t.Fatal("print-config shows the token")
	}

	// An empty variable and a variable that does not exist stop the start.
	t.Setenv("STOWR_TEST_EMPTY", "")
	for name, test := range map[string]struct{ pdp, want string }{
		"empty variable":   {strings.Replace(pdp, "STOWR_TEST_PDP_TOKEN", "STOWR_TEST_EMPTY", 1), `the environment variable "STOWR_TEST_EMPTY" is empty`},
		"missing variable": {strings.Replace(pdp, "STOWR_TEST_PDP_URL", "STOWR_TEST_NO_SUCH_VARIABLE", 1), `the environment variable "STOWR_TEST_NO_SUCH_VARIABLE" does not exist`},
	} {
		_, _, err := decodeAccessTestConfig(t, authzenText(identity, test.pdp), nil)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error = %v, want %q", name, err, test.want)
		}
	}
}

// A PEM file in public_keys_file gives the same keys and the same print-config form as a JWK Set: the type and the
// RFC 7638 thumbprint.
func TestAccessConfigPEMPublicKeys(t *testing.T) {
	t.Parallel()
	identity := strings.Replace(goodIdentity, `public_keys_file = "jwks.json"`, `public_keys_file = "edge.pem"`, 1)
	cfg, dir, err := decodeAccessTestConfig(t, authzenText(identity, goodPDP), map[string]string{"edge.pem": rfc8037PEM(t) + rfc7515PEM(t)})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Access.Keys) != 2 || cfg.Access.Keys[0].Alg != identityAlgEd25519 || cfg.Access.Keys[1].Alg != identityAlgES256 {
		t.Fatalf("keys = %+v", cfg.Access.Keys)
	}
	want := `access.identity.public_keys = ["Ed25519 kid=` + rfc8037Thumbprint + `", "P-256 kid=` + cfg.Access.Keys[1].KID + `"]  # file:` + filepath.Join(dir, "edge.pem")
	if printed := formatEffectiveConfiguration(cfg); !strings.Contains(printed, want+"\n") {
		t.Fatalf("print-config misses the line %q:\n%s", want, printed)
	}
}

func TestAccessPrintConfigAuthZEN(t *testing.T) {
	t.Parallel()
	identity := goodIdentity + "\n    max_age = \"300s\"\n"
	pdp := goodPDP + "\n    timeout = \"1s\"\n    plain_http = false\n"
	cfg, dir, err := decodeAccessTestConfig(t, authzenText(identity, pdp), nil)
	if err != nil {
		t.Fatal(err)
	}
	printed := formatEffectiveConfiguration(cfg)
	for _, want := range []string{
		`access.mode = "authzen"  # file`,
		`access.identity.header = "X-Identity-Token"  # default`,
		`access.identity.issuer = "https://edge.example"  # file`,
		`access.identity.audience = "infra-s3"  # file`,
		`access.identity.public_keys = ["Ed25519 kid=edge-1", "P-256 kid=edge-2"]  # file:` + filepath.Join(dir, "jwks.json"),
		`access.identity.max_age = "300s"  # file`,
		`access.identity.clock_skew = "60s"  # default`,
		`access.pdp.url = "https://pdp.example:4182"  # file`,
		`access.pdp.plain_http = false  # file`,
		`access.pdp.token = <hidden>  # file:` + filepath.Join(dir, "pdp-token"),
		`access.pdp.timeout = "1s"  # file`,
		`access.pdp.usage_timeout = "2s"  # default`,
		`access.pdp.cache_ttl = "30s"  # default`,
		`access.pdp.max_evaluations = 64  # default`,
	} {
		if !strings.Contains(printed, want+"\n") {
			t.Errorf("print-config misses the line %q:\n%s", want, printed)
		}
	}
	if strings.Contains(printed, testPDPToken) {
		t.Fatal("print-config shows the token")
	}
	count := 0
	for _, line := range strings.Split(printed, "\n") {
		if strings.HasPrefix(line, "access.") {
			count++
		}
	}
	if count != 14 {
		t.Fatalf("print-config has %d access lines, want 14:\n%s", count, printed)
	}
}

func TestAccessPrintConfigOpen(t *testing.T) {
	t.Parallel()
	cfg, _, err := decodeAccessTestConfig(t, "access { mode = \"open\" }\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	printed := formatEffectiveConfiguration(cfg)
	if !strings.Contains(printed, "access.mode = \"open\"  # file\n") {
		t.Fatalf("print-config misses access.mode:\n%s", printed)
	}
	if strings.Contains(printed, "access.identity") || strings.Contains(printed, "access.pdp") {
		t.Fatalf("print-config shows keys that mode open does not have:\n%s", printed)
	}
}

func TestAccessPrintConfigDurationWithoutText(t *testing.T) {
	t.Parallel()
	// A configuration that no file made has no written text: print-config then gives the shortest strict text.
	cfg := appConfig{Listen: ":8080", Access: accessConfig{
		Mode: accessModeAuthZEN, Header: "X-Identity-Token", Issuer: "i", Audience: "a", PDPURL: "https://p",
		MaxAge: 90 * time.Second, PDPTimeout: 1500 * time.Millisecond, PDPCacheTTL: time.Hour,
	}}
	printed := formatEffectiveConfiguration(cfg)
	for _, want := range []string{`access.identity.max_age = "90s"`, `access.pdp.timeout = "1500ms"`, `access.pdp.cache_ttl = "1h"`, `access.identity.clock_skew = "0s"`} {
		if !strings.Contains(printed, want) {
			t.Errorf("print-config misses %q:\n%s", want, printed)
		}
	}
}

func TestConfigWarnings(t *testing.T) {
	t.Parallel()
	authzen := accessConfig{Mode: accessModeAuthZEN, PDPURL: "https://pdp"}
	plain := accessConfig{Mode: accessModeAuthZEN, PDPURL: "http://pdp", PDPPlainHTTP: true}
	for name, test := range map[string]struct {
		cfg  appConfig
		want []string
	}{
		"open":                          {appConfig{Listen: "127.0.0.1:8080", Access: accessConfig{Mode: accessModeOpen}}, []string{"the server checks no identity token and asks no PDP"}},
		"open on all interfaces":        {appConfig{Listen: ":8080", Access: accessConfig{Mode: accessModeOpen}}, []string{"checks no identity token"}},
		"authzen on a loopback address": {appConfig{Listen: "127.0.0.1:8080", Access: authzen}, nil},
		"authzen on a private address":  {appConfig{Listen: "10.0.0.5:8080", Access: authzen}, nil},
		"authzen without a host":        {appConfig{Listen: ":8080", Access: authzen}, []string{`server.listen = ":8080"`}},
		"authzen on 0.0.0.0":            {appConfig{Listen: "0.0.0.0:8080", Access: authzen}, []string{`server.listen = "0.0.0.0:8080"`}},
		"authzen on ::":                 {appConfig{Listen: "[::]:8080", Access: authzen}, []string{`server.listen = "[::]:8080"`}},
		"plain http on a loopback":      {appConfig{Listen: "127.0.0.1:8080", Access: plain}, []string{"access.pdp.plain_http = true"}},
		"plain http and no host":        {appConfig{Listen: ":8080", Access: plain}, []string{"access.pdp.plain_http = true", "only through the edge"}},
	} {
		got := configWarnings(test.cfg)
		if len(got) != len(test.want) {
			t.Errorf("%s: warnings = %q, want %d", name, got, len(test.want))
			continue
		}
		for index, want := range test.want {
			if !strings.Contains(got[index], want) {
				t.Errorf("%s: warning %d = %q, want %q", name, index, got[index], want)
			}
		}
	}
}

func TestConfigWarningsOfADecodedFile(t *testing.T) {
	t.Parallel()
	cfg, _, err := decodeAccessTestConfig(t, "access { mode = \"open\" }\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := configWarnings(cfg); len(got) != 1 || !strings.Contains(got[0], `access.mode = "open"`) {
		t.Fatalf("warnings = %q", got)
	}
	cfg, _, err = decodeAccessTestConfig(t, authzenText(goodIdentity, strings.Replace(goodPDP, "https://", "http://", 1)+"\n    plain_http = true"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := configWarnings(cfg); len(got) != 2 || !strings.Contains(got[0], "plain_http") || !strings.Contains(got[1], "only through the edge") {
		t.Fatalf("warnings = %q", got)
	}
	// print-config does not print the warnings.
	if strings.Contains(formatEffectiveConfiguration(cfg), "warning") {
		t.Fatal("print-config shows a warning")
	}
}

func TestDurationText(t *testing.T) {
	t.Parallel()
	for text, want := range map[string]time.Duration{
		"0s": 0, "0ms": 0, "1ms": time.Millisecond, "500ms": 500 * time.Millisecond, "60s": time.Minute, "5m": 5 * time.Minute,
		"24h": 24 * time.Hour, "100h": 100 * time.Hour, "10s": 10 * time.Second,
	} {
		got, err := parseDurationText(text)
		if err != nil || got != want {
			t.Errorf("parseDurationText(%q) = %v, %v; want %v", text, got, err, want)
		}
	}
	for _, text := range []string{"", "5", "s", "ms", "1.5s", ".5s", "+5s", "-5s", "1m30s", "05s", "00s", "5 s", " 5s", "5s ", "5S", "5d", "5us", "5µs", "0x10s", "1e3s", "5\n"} {
		if got, err := parseDurationText(text); err == nil {
			t.Errorf("parseDurationText(%q) = %v, want an error", text, got)
		}
	}
	for value, want := range map[time.Duration]string{
		0: "0s", time.Millisecond: "1ms", 1500 * time.Millisecond: "1500ms", time.Second: "1s", 90 * time.Second: "90s", time.Minute: "1m", time.Hour: "1h", 36 * time.Hour: "36h",
	} {
		if got := durationText(value); got != want {
			t.Errorf("durationText(%v) = %q, want %q", value, got, want)
		}
	}
}

func TestAccessKeyTable(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, key := range configKeys {
		name := key.Block + "." + key.Name
		if seen[name] {
			t.Fatalf("duplicate key %s", name)
		}
		seen[name] = true
		if key.Description == "" || key.Type == "" || key.Forms == 0 || key.Default == "" {
			t.Fatalf("key %s is not complete", name)
		}
		if key.Secret && key.Forms&formValue != 0 {
			t.Fatalf("the secret %s must accept only _env and _file", name)
		}
	}
	blocks := map[string]bool{}
	for _, block := range configBlocks {
		blocks[block.Path] = true
	}
	for _, key := range configKeys {
		if !blocks[key.Block] {
			t.Fatalf("key %s.%s has no block", key.Block, key.Name)
		}
	}
	for _, block := range []string{"access", "access.identity", "access.pdp"} {
		if !blocks[block] || len(keysOfBlock(block)) == 0 {
			t.Fatalf("the block %s has no entry or no key in the table", block)
		}
	}
	token, _, _ := keyOfAttribute("access.pdp", "token_env")
	if !token.Secret || token.Forms != formEnv|formFile {
		t.Fatalf("access.pdp.token = %+v", token)
	}
	if got := attributeNames("access.pdp"); containsString(got, "token") || !containsString(got, "token_env") || !containsString(got, "token_file") {
		t.Fatalf("attributes of access.pdp = %v", got)
	}
	// The default of each duration in the table is the text that the decoder uses.
	for path, text := range map[string]string{
		"access.identity.max_age":    defaultIdentityMaxAgeText,
		"access.identity.clock_skew": defaultIdentityClockSkewText,
		"access.pdp.timeout":         defaultPDPTimeoutText,
		"access.pdp.usage_timeout":   defaultPDPUsageTimeoutText,
		"access.pdp.cache_ttl":       defaultPDPCacheTTLText,
	} {
		index := strings.LastIndexByte(path, '.')
		var found configKey
		for _, key := range keysOfBlock(path[:index]) {
			if key.Name == path[index+1:] {
				found = key
			}
		}
		if found.Default != `"`+text+`"` || found.Type != "duration" {
			t.Errorf("%s: table default = %s (type %s), decoder default = %q", path, found.Default, found.Type, text)
		}
		if _, err := parseDurationText(text); err != nil {
			t.Errorf("%s: the default %q is not a duration: %v", path, text, err)
		}
	}
}

// The reference shows the access block. The old well-known path of the list of actions has no alias.
func TestAccessIsInTheReferenceAndTheOldPathIsGone(t *testing.T) {
	t.Parallel()
	reference := configReference()
	for _, want := range []string{
		"## `access`", "## `access.identity`", "## `access.pdp`",
		"| `mode` |", "| `public_keys` |", "| `max_evaluations` |", "| `token` | text | (required) |",
		"Exactly one block. Required.", "One block with mode authzen. No block with mode open.",
	} {
		if !strings.Contains(reference, want) {
			t.Errorf("the configuration reference misses %q", want)
		}
	}
	app, _, _ := testApplication(t)
	app.publicFS = publicTree(t)
	recorder := fetchStatic(t, app, "/.well-known/access-actions", nil)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("/.well-known/access-actions: status %d, want 404", recorder.Code)
	}
}

// A file without an access block gives an error that tells how to fix it.
func TestMissingAccessBlockTellsHowToFixIt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.hcl")
	writeConfig(t, path, strings.Replace(minimalTestConfig("bucket"), "access {\n  mode = \"open\"\n}\n", "", 1))
	_, err := loadConfig(path)
	if err == nil {
		t.Fatal("a file without an access block was accepted")
	}
	for _, want := range []string{"the access block is required", `access { mode = "open" }`, `mode = "authzen"`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
}
