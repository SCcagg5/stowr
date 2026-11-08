package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// keyForms tells the forms that a configuration key accepts.
type keyForms uint8

const (
	// formValue: the value is in the file (`<name> = "..."`).
	formValue keyForms = 1 << iota
	// formEnv: the value is in the environment variable that the file names (`<name>_env = "VAR"`).
	formEnv
	// formFile: the value is the content of the file that the file names (`<name>_file = "/path"`).
	formFile
)

// configKey is one key of the configuration. The decoders, the reference
// (docs/configuration-reference.md) and the effective configuration (print-config)
// all read this table. Thus they always agree.
type configKey struct {
	Block       string
	Name        string
	Type        string
	Default     string
	Range       string
	Forms       keyForms
	Secret      bool
	Description string
}

// configBlock describes one block of the configuration for the reference.
type configBlock struct {
	Path        string
	Syntax      string
	Description string
}

var configBlocks = []configBlock{
	{"server", "server { ... }", "Zero or one block. The listener, the logs and the limits of the process."},
	{"access", "access { ... }", "Exactly one block. Required. It sets the policy of access (docs/ACCESS.md). Without it, the start stops."},
	{"access.identity", "access { identity { ... } }", "One block with mode authzen. No block with mode open. The rules to check the identity token."},
	{"access.pdp", "access { pdp { ... } }", "One block with mode authzen. No block with mode open. The connection to the policy decision point (PDP)."},
	{"auth", `auth "<id>" { ... }`, "One or more blocks. A credential set and an endpoint that one or more buckets use."},
	{"auth.usage", `auth "<id>" { usage { ... } }`, "Zero or one block in an auth block with provider \"s3\". The admin API that gives the usage and the quota of the buckets of the auth block (docs/ARCHITECTURE.md, \"Usage and quota of a bucket\"). Without it, the interface shows no usage and no quota."},
	{"bucket", `bucket "<id>" { ... }`, "One or more blocks. One bucket that the browser shows. The label is the id of the bucket in the URL."},
	{"volume", `volume "<id>" { ... }`, "Zero or more blocks. One folder of the server that the browser shows as a bucket: a Docker volume, an NFS, CephFS or FUSE mount (docs/ARCHITECTURE.md, \"Volumes\"). The label is the id of the volume in the URL (/-/volume/<id>/). A file needs one bucket block or one volume block at least."},
}

var configKeys = []configKey{
	{"server", "listen", "text", `":8080"`, "a numeric host:port, not a URL", formValue, false, "The address and the port that the server listens on."},
	{"server", "job_history_limit", "integer", "10", "1 to 20", formValue, false, "The number of finished background jobs that the process keeps in memory."},
	{"server", "access_mode", "text", `"inherit_credentials"`, "inherit_credentials or force_read_only", formValue, false, "force_read_only refuses each write and each delete."},
	{"server", "log_mode", "text", `"anonymous"`, "anonymous or detailed", formValue, false, "anonymous keeps the object keys out of the request log. detailed writes them."},
	{"server", "allow_full_object_fallback", "boolean", "false", "", formValue, false, "Let a parser read a full object when a format has no range reader."},
	{"server", "memory_limit_bytes", "integer", "0", "0, or 33554432 to 1125899906842624", formValue, false, "The soft memory limit of the Go runtime. 0 keeps the default of the runtime."},
	{"server", "max_storage_bytes_per_request", "integer", "274877906944", "1048576 to 1125899906842624", formValue, false, "The provider bytes that one HTTP request can transfer."},
	{"server", "max_storage_requests_per_request", "integer", "4096", "1 to 10000000", formValue, false, "The provider calls that one HTTP request can make."},
	{"server", "max_range_cache_bytes", "integer", "8388608", "1048576 to 4294967296", formValue, false, "The memory cache of one range reader session."},
	{"server", "max_concurrent_storage_requests", "integer", "4", "1 to 1024", formValue, false, "The provider calls that run at the same time, for all the buckets."},
	{"server", "max_concurrent_requests_per_storage", "integer", "2", "1 to 256, and not more than max_concurrent_storage_requests", formValue, false, "The provider calls that run at the same time for one bucket."},
	{"server", "session_ttl_seconds", "integer", "1200", "30 to 604800", formValue, false, "The life of a preview session in memory."},
	{"server", "max_stats_folders", "integer", "1000", "100 to 1000000", formValue, false, "The folder totals that one Insights result keeps."},
	{"server", "max_archive_entries", "integer", "10000", "1 to 1000000", formValue, false, "The entries of a ZIP central directory that the server reads, and the members that a request can select."},
	{"server", "max_background_storage_bytes", "integer", "274877906944", "1048576 to 1125899906842624", formValue, false, "The provider bytes that one background job can transfer."},
	{"server", "max_background_storage_requests", "integer", "100000", "1 to 10000000", formValue, false, "The provider calls that one background job can make."},

	{"access", "mode", "text", "(required)", "authzen or open (lower case)", formValue, false, "authzen: the server checks the identity token and asks the PDP. open: the server makes no check and writes a warning at start."},

	{"access.identity", "header", "text", `"X-Identity-Token"`, "1 to 64 characters: letters, digits and \"-\". Not Cookie, Authorization, Host, Content-Type or Content-Length", formValue, false, "The name of the HTTP header that holds the identity token."},
	{"access.identity", "issuer", "text", "(required)", "1 to 1024 bytes of UTF-8, without a control character", formValue, false, "The iss of the identity tokens that the server accepts."},
	{"access.identity", "audience", "text", "(required)", "1 to 256 bytes of UTF-8, without a control character", formValue, false, "The id of this service. It is the aud of the tokens, the server of the action document and the root of each chain of resources."},
	{"access.identity", "public_keys", "text", "(required)", "PEM (1 to 16 -----BEGIN PUBLIC KEY----- blocks) or a JWK Set (RFC 7517) with 1 to 16 keys. Each key is Ed25519 or P-256", formValue | formEnv | formFile, false, "The public keys of the issuer. A text that starts with \"{\" is a JWK Set, other text is PEM. The server checks the signature of each identity token with them. It is not a secret: print-config shows the type and the kid of each key."},
	{"access.identity", "max_age", "duration", `"5m"`, "1s to 24h", formValue, false, "The longest life (exp minus iat) of an identity token."},
	{"access.identity", "clock_skew", "duration", `"60s"`, "0s to 5m, whole seconds", formValue, false, "The difference between the clock of the server and the clock of the issuer that the server accepts."},

	{"access.pdp", "url", "URL", "(required)", "http or https, with a host, without credentials, query or fragment, and without a \"/\" at the end", formValue | formEnv | formFile, false, "The base URL of the PDP. The client adds /access/v1/evaluation and /access/v1/evaluations to it."},
	{"access.pdp", "plain_http", "boolean", "false", "true with an http URL. false with an https URL", formValue, false, "true accepts an http URL: the server then sends the token and the questions without TLS. Use it only on a trusted network."},
	{"access.pdp", "token", "text", "(required)", "1 to 4096 bytes, no space and no control character", formEnv | formFile, true, "The bearer token that the server sends to the PDP. It is a secret: the form in clear text stops the start."},
	{"access.pdp", "timeout", "duration", `"500ms"`, "10ms to 1m", formValue, false, "The deadline of one request to the PDP, without context.usage."},
	{"access.pdp", "usage_timeout", "duration", `"2s"`, "100ms to 1m, and not less than timeout", formValue, false, "The deadline of one question with context.usage."},
	{"access.pdp", "cache_ttl", "duration", `"30s"`, "0s to 1h", formValue, false, "The life of an answer of the PDP in memory. 0s keeps no answer."},
	{"access.pdp", "max_evaluations", "integer", "64", "1 to 1024", formValue, false, "The most items in one Access Evaluations request."},

	{"auth", "provider", "text", "(required)", "s3 or gcs (lower case)", formValue, false, "The storage protocol."},
	{"auth", "mode", "text", "s3: access_key with credentials, else anonymous. gcs: service_account with credentials_file, else anonymous", "s3: access_key or anonymous. gcs: service_account or anonymous", formValue, false, "How the server signs the provider requests."},
	{"auth", "endpoint", "URL", "s3: (required). gcs: https://storage.googleapis.com", "http or https, without credentials, query or fragment, and without a \"/\" at the end", formValue, false, "The base URL of the provider."},
	{"auth", "region", "text", "s3: (required)", "", formValue, false, "The region that the S3 signature uses."},
	{"auth", "access_key_id", "text", "(none)", "s3 access_key mode only; no space at the start or at the end", formValue | formEnv | formFile, false, "The S3 access key id. It is an identifier, not a secret: print-config shows it."},
	{"auth", "secret_access_key", "text", "(none)", "s3 access_key mode only; no space and no control character", formEnv | formFile, true, "The S3 secret access key. It is a secret: the form in clear text stops the start."},
	{"auth", "session_token", "text", "(none)", "s3 only; no space and no control character", formEnv | formFile, true, "The S3 session token. It is a secret: the form in clear text stops the start."},
	{"auth", "credentials_file", "file path", "(none)", "gcs service_account mode only", formValue, false, "The JSON file of a GCS service account. A relative path starts at the directory of the configuration file."},
	{"auth", "insecure_skip_verify", "boolean", "false", "", formValue, false, "Do not verify the TLS certificate of the provider. Use it only for a test."},

	{"auth.usage", "source", "text", "(required)", "garage, seaweedfs or minio, exact (lower case)", formValue, false, "The reader of the usage and of the quota. The server never guesses it from the provider."},
	{"auth.usage", "endpoint", "URL", "garage: (required), the admin API. seaweedfs: (required), the master. minio: the endpoint of the auth block", "http or https, without credentials, query or fragment, and without a \"/\" at the end", formValue, false, "The base URL of the admin API. print-config and the effective configuration in the log show it. The answers of the server and the error messages never show it."},
	{"auth.usage", "filer_endpoint", "URL", "seaweedfs: (required). Other sources: (none)", "seaweedfs only. http or https, without credentials, query or fragment, and without a \"/\" at the end", formValue, false, "The base URL of the filer of SeaweedFS. The filer gives the quota of a bucket."},
	{"auth.usage", "token", "text", "garage: (required). Other sources: (none)", "garage only; 1 to 4096 bytes, no space and no control character", formEnv | formFile, true, "The admin token of Garage. It is a secret: the form in clear text stops the start. minio signs with the S3 credentials of the auth block. seaweedfs sends no credential."},

	{"bucket", "name", "text", "the label of the block", "", formValue, false, "The name that the user interface shows."},
	{"bucket", "auth", "text", "(required)", "the label of an auth block", formValue, false, "The auth block that this bucket uses."},
	{"bucket", "bucket", "text", "(required)", "", formValue, false, "The name of the bucket at the provider."},
	{"bucket", "permissions", "list of text", "(none: no ceiling)", "read, write, delete (lower case, each one time)", formValue, false, "The operations that the server can do on this bucket. It is a ceiling: the provider can refuse more."},
	{"bucket", "root_prefix", "text", `""`, "\"\", or a prefix that ends with \"/\" and does not start with \"/\"", formValue, false, "The part of the bucket that the browser shows. The keys are relative to it."},
	{"bucket", "max_scan_pages", "integer", "1", "0 to 1000000", formValue, false, "The 1000-entry provider pages that one listing combines. 0 = no limit."},
	{"bucket", "encryption", "text", `""`, "\"\" or sse-s3, exact (lower case). SSE-C and SSE-KMS are not supported. Only for an s3 auth block", formValue, false, "The server-side encryption that the server asks for each object that it writes: upload, copy, move, rename, metadata edit, restore of a version, extraction. sse-s3 sends x-amz-server-side-encryption: AES256. \"\" sends no encryption header: the default encryption of the bucket applies."},
	{"bucket", "quota_bytes", "integer", "(none: the limit of the provider, or no limit)", "1 to 1152921504606846976. Only with a usage block in the auth block", formValue, false, "The size limit of the bucket that the interface shows when the provider gives no limit. The server does not enforce it."},

	{"volume", "name", "text", "the label of the block", "", formValue, false, "The name that the user interface shows."},
	{"volume", "path", "file path", "(required)", "an absolute path, without \"/\" at the end and without an empty, \".\" or \"..\" part. The folder must exist at start, and the process must read it", formValue, false, "The folder of the volume. The server reads and writes its files with the identity of the process. With write or delete in permissions (or without permissions), the start writes and removes a test file: a folder that the process cannot write stops the start."},
	{"volume", "permissions", "list of text", "(none: read, write and delete)", "read, write, delete (lower case, each one time)", formValue, false, "The operations that the server can do in this volume. It is a ceiling: the file system can refuse more. Give [\"read\"] for a folder that the process cannot write."},
	{"volume", "root_prefix", "text", `""`, "\"\", or a relative folder path that ends with \"/\", without an empty, \".\" or \"..\" part. The folder must exist at start", formValue, false, "The folder of path that the browser shows. The keys are relative to it. A symbolic link that leaves it is not followed."},
	{"volume", "max_scan_pages", "integer", "1", "0 to 1000000", formValue, false, "The 1000-entry pages of a folder that one listing combines. 0 = no limit."},
}

// keysOfBlock gives the keys of one block, in the order of the table.
func keysOfBlock(block string) []configKey {
	out := make([]configKey, 0, 16)
	for _, key := range configKeys {
		if key.Block == block {
			out = append(out, key)
		}
	}
	return out
}

// attributeNames gives each attribute name that a block accepts: the name of
// each key, and its _env and _file forms when the key has them.
func attributeNames(block string) []string {
	names := make([]string, 0, 24)
	for _, key := range keysOfBlock(block) {
		if key.Forms&formValue != 0 {
			names = append(names, key.Name)
		}
		if key.Forms&formEnv != 0 {
			names = append(names, key.Name+"_env")
		}
		if key.Forms&formFile != 0 {
			names = append(names, key.Name+"_file")
		}
	}
	return names
}

// keyOfAttribute gives the key and the form of an attribute name of a block.
func keyOfAttribute(block, attribute string) (configKey, keyForms, bool) {
	for _, key := range keysOfBlock(block) {
		switch {
		case key.Forms&formValue != 0 && attribute == key.Name:
			return key, formValue, true
		case key.Forms&formEnv != 0 && attribute == key.Name+"_env":
			return key, formEnv, true
		case key.Forms&formFile != 0 && attribute == key.Name+"_file":
			return key, formFile, true
		}
	}
	return configKey{}, 0, false
}

// recordSources notes the source of each attribute of a block: "file" for a
// value in the file, "env:VAR" or "file:/path" for an indirect form.
func recordSources(cfg *appConfig, path, blockType string, block hclBlock, baseDir string) {
	if cfg.Sources == nil {
		cfg.Sources = make(map[string]string)
	}
	for name, attr := range block.Attrs {
		key, form, ok := keyOfAttribute(blockType, name)
		if !ok {
			continue
		}
		source := "file"
		switch form {
		case formEnv:
			source = "env:" + attr.Value.String
		case formFile:
			source = "file:" + resolveConfigPath(attr.Value.String, baseDir)
		}
		cfg.Sources[path+"."+key.Name] = source
	}
}

// configReference gives the text of docs/configuration-reference.md.
func configReference() string {
	var b strings.Builder
	b.WriteString("# Configuration reference\n\n")
	b.WriteString("This document uses ASD-STE100 Simplified Technical English.\n\n")
	b.WriteString("Do not edit this file. The program makes it from the table of keys in `src/config_keys.go`.\n")
	b.WriteString("To make it again, run `go run . config-reference > ../docs/configuration-reference.md` in `src`.\n")
	b.WriteString("A test makes sure that this file and the table agree.\n\n")
	b.WriteString("## Rules\n\n")
	b.WriteString("- An unknown block, an unknown attribute or a duplicate attribute stops the start.\n")
	b.WriteString("- A value that is not in its range stops the start. The error gives the key and the line.\n")
	b.WriteString("- A key without a value in the file has its default value. A key with `(required)` must have a value.\n")
	b.WriteString("- A file needs one `access` block. The block has mode `authzen` or mode `open`.\n")
	b.WriteString("- A file needs one `bucket` block or one `volume` block at least. A `bucket` block needs an `auth` block.\n")
	b.WriteString("- A duration is one whole number and one unit: `ms`, `s`, `m` or `h`. For example: `500ms`, `30s`, `5m`, `1h`.\n")
	b.WriteString("  A sign, a fraction, a space or two units (`1m30s`) stops the start.\n")
	b.WriteString("- `stowr print-config -c <file>` checks the file, writes the value of each key and its source, and exits with status 0.\n")
	b.WriteString("  The source is `file`, `env:<VAR>`, `file:<path>` or `default`. The output hides each secret.\n")
	b.WriteString("- At start, the server writes the same values in its log.\n\n")
	b.WriteString("## Values from the environment or from a file\n\n")
	b.WriteString("The column *Forms* gives the forms of each key:\n\n")
	b.WriteString("- `value`: `<name> = \"...\"`. The value is in the file.\n")
	b.WriteString("- `_env`: `<name>_env = \"VAR\"`. The value is in the environment variable `VAR`.\n")
	b.WriteString("- `_file`: `<name>_file = \"/path\"`. The value is the content of the file, without the last line end.\n")
	b.WriteString("  A relative path starts at the directory of the configuration file.\n\n")
	b.WriteString("Give one form only. Two forms stop the start. A variable that does not exist, an empty value, or a file that the\n")
	b.WriteString("server cannot read stops the start. The server reads each variable and each file one time, at start. The server reads\n")
	b.WriteString("no environment variable that the file does not name.\n\n")
	b.WriteString("A secret key has *yes* in the column *Secret*. A secret accepts only `_env` and `_file`: a secret in clear text in\n")
	b.WriteString("the file stops the start. The output of `print-config` and the log show `<hidden>` for it.\n\n")
	b.WriteString("## No change of a value\n\n")
	b.WriteString("The value in the file is the value that applies. The server does not remove spaces, does not change the case, and does\n")
	b.WriteString("not add or remove a `/`. A value that does not fit stops the start. The error gives the key, the expected form and the\n")
	b.WriteString("value. An error about a secret does not show the value. A text value must not be empty and must not have a space at\n")
	b.WriteString("the start or at the end: to use the default of a key, remove the key.\n")
	for _, block := range configBlocks {
		fmt.Fprintf(&b, "\n## `%s`\n\n", block.Path)
		fmt.Fprintf(&b, "Syntax: `%s`. %s\n\n", block.Syntax, block.Description)
		b.WriteString("| Key | Type | Default | Range | Forms | Secret | Use |\n")
		b.WriteString("|---|---|---|---|---|---|---|\n")
		for _, key := range keysOfBlock(block.Path) {
			secret := "no"
			if key.Secret {
				secret = "yes"
			}
			fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s | %s | %s |\n",
				key.Name, key.Type, markdownCell(key.Default), markdownCell(key.Range), formsText(key.Forms), secret, markdownCell(key.Description))
		}
	}
	return b.String()
}

func markdownCell(value string) string {
	if value == "" {
		return " "
	}
	return strings.ReplaceAll(value, "|", "\\|")
}

func formsText(forms keyForms) string {
	parts := make([]string, 0, 3)
	if forms&formValue != 0 {
		parts = append(parts, "value")
	}
	if forms&formEnv != 0 {
		parts = append(parts, "`_env`")
	}
	if forms&formFile != 0 {
		parts = append(parts, "`_file`")
	}
	return strings.Join(parts, ", ")
}

// effectiveSetting is one line of the effective configuration.
type effectiveSetting struct {
	Name   string
	Value  string
	Source string
}

// effectiveConfiguration gives the value that applies for each key, and its
// source. It hides each secret.
func effectiveConfiguration(cfg appConfig) []effectiveSetting {
	out := make([]effectiveSetting, 0, 96)
	addBlock := func(path, blockType string, values map[string]string) {
		for _, key := range keysOfBlock(blockType) {
			value, ok := values[key.Name]
			if !ok {
				value = "<none>"
			}
			source, found := cfg.Sources[path+"."+key.Name]
			if !found {
				source = "default"
			}
			out = append(out, effectiveSetting{Name: path + "." + key.Name, Value: value, Source: source})
		}
	}
	add := func(path string, values map[string]string) {
		blockType := path
		if index := strings.IndexByte(path, ' '); index >= 0 {
			blockType = path[:index]
		}
		addBlock(path, blockType, values)
	}
	runtime := cfg.Runtime
	add("server", map[string]string{
		"listen":                              strconv.Quote(cfg.Listen),
		"job_history_limit":                   strconv.Itoa(cfg.JobHistoryLimit),
		"access_mode":                         strconv.Quote(runtime.AccessMode),
		"log_mode":                            strconv.Quote(runtime.LogMode),
		"allow_full_object_fallback":          strconv.FormatBool(runtime.AllowFullObjectFallback),
		"memory_limit_bytes":                  strconv.FormatInt(runtime.MemoryLimitBytes, 10),
		"max_storage_bytes_per_request":       strconv.FormatInt(runtime.MaxStorageBytesPerRequest, 10),
		"max_storage_requests_per_request":    strconv.FormatInt(runtime.MaxStorageRequestsPerRequest, 10),
		"max_range_cache_bytes":               strconv.FormatInt(runtime.MaxRangeCacheBytes, 10),
		"max_concurrent_storage_requests":     strconv.Itoa(runtime.MaxConcurrentStorageRequests),
		"max_concurrent_requests_per_storage": strconv.Itoa(runtime.MaxConcurrentRequestsPerStore),
		"session_ttl_seconds":                 strconv.Itoa(runtime.SessionTTLSeconds),
		"max_stats_folders":                   strconv.Itoa(runtime.MaxStatsFolders),
		"max_archive_entries":                 strconv.Itoa(runtime.MaxArchiveEntries),
		"max_background_storage_bytes":        strconv.FormatInt(runtime.MaxBackgroundStorageBytes, 10),
		"max_background_storage_requests":     strconv.FormatInt(runtime.MaxBackgroundStorageRequests, 10),
	})
	addAccess := func() {
		access := cfg.Access
		addBlock("access", "access", map[string]string{"mode": strconv.Quote(access.Mode)})
		if !access.enforced() {
			return
		}
		keyNames := make([]string, 0, len(access.Keys))
		for _, key := range access.Keys {
			keyNames = append(keyNames, key.display())
		}
		addBlock("access.identity", "access.identity", map[string]string{
			"header":      strconv.Quote(access.Header),
			"issuer":      strconv.Quote(access.Issuer),
			"audience":    strconv.Quote(access.Audience),
			"public_keys": quotedList(keyNames),
			"max_age":     strconv.Quote(accessDurationText(cfg, "access.identity.max_age", access.MaxAge)),
			"clock_skew":  strconv.Quote(accessDurationText(cfg, "access.identity.clock_skew", access.ClockSkew)),
		})
		addBlock("access.pdp", "access.pdp", map[string]string{
			"url":             strconv.Quote(access.PDPURL),
			"plain_http":      strconv.FormatBool(access.PDPPlainHTTP),
			"token":           "<hidden>",
			"timeout":         strconv.Quote(accessDurationText(cfg, "access.pdp.timeout", access.PDPTimeout)),
			"usage_timeout":   strconv.Quote(accessDurationText(cfg, "access.pdp.usage_timeout", access.PDPUsageTimeout)),
			"cache_ttl":       strconv.Quote(accessDurationText(cfg, "access.pdp.cache_ttl", access.PDPCacheTTL)),
			"max_evaluations": strconv.Itoa(access.PDPMaxEvaluations),
		})
	}
	addAccess()
	hidden := func(value string) string {
		if value == "" {
			return "<none>"
		}
		return "<hidden>"
	}
	for _, auth := range cfg.Authentications {
		values := map[string]string{
			"provider":             strconv.Quote(auth.Provider),
			"mode":                 strconv.Quote(auth.Mode),
			"endpoint":             strconv.Quote(auth.Endpoint),
			"access_key_id":        quotedOrNone(auth.AccessKeyID),
			"secret_access_key":    hidden(auth.SecretAccessKey),
			"session_token":        hidden(auth.SessionToken),
			"insecure_skip_verify": strconv.FormatBool(auth.InsecureSkipVerify),
		}
		if auth.Region != "" {
			values["region"] = strconv.Quote(auth.Region)
		}
		if auth.CredentialsFile != "" {
			values["credentials_file"] = strconv.Quote(auth.CredentialsFile)
		}
		add("auth "+strconv.Quote(auth.ID), values)
		if usage := auth.Usage; usage != nil {
			addBlock("auth "+strconv.Quote(auth.ID)+".usage", "auth.usage", map[string]string{
				"source":         strconv.Quote(usage.Source),
				"endpoint":       strconv.Quote(usage.Endpoint),
				"filer_endpoint": quotedOrNone(usage.FilerEndpoint),
				"token":          hidden(usage.Token),
			})
		}
	}
	for _, bucket := range cfg.Buckets {
		values := map[string]string{
			"name":           strconv.Quote(bucket.Name),
			"auth":           strconv.Quote(bucket.AuthID),
			"bucket":         strconv.Quote(bucket.Bucket),
			"root_prefix":    strconv.Quote(bucket.RootPrefix),
			"max_scan_pages": strconv.Itoa(bucket.MaxScanPages),
			"encryption":     strconv.Quote(bucket.Encryption),
		}
		if bucket.PermissionsDefined {
			values["permissions"] = quotedList(bucket.Permissions)
		}
		if bucket.QuotaBytes != 0 {
			values["quota_bytes"] = strconv.FormatInt(bucket.QuotaBytes, 10)
		}
		add("bucket "+strconv.Quote(bucket.ID), values)
	}
	for _, volume := range cfg.Volumes {
		values := map[string]string{
			"name":           strconv.Quote(volume.Name),
			"path":           strconv.Quote(volume.Path),
			"root_prefix":    strconv.Quote(volume.RootPrefix),
			"max_scan_pages": strconv.Itoa(volume.MaxScanPages),
		}
		if volume.PermissionsDefined {
			values["permissions"] = quotedList(volume.Permissions)
		}
		add("volume "+strconv.Quote(volume.ID), values)
	}
	return out
}

// accessDurationText gives the text of a duration key of the access block: the text of the file or the default text.
// A configuration that was not made from a file has no such text: the function then gives the shortest strict text.
func accessDurationText(cfg appConfig, path string, value time.Duration) string {
	if text, ok := cfg.DurationTexts[path]; ok {
		return text
	}
	return durationText(value)
}

func quotedOrNone(value string) string {
	if value == "" {
		return "<none>"
	}
	return strconv.Quote(value)
}

func quotedList(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, strconv.Quote(value))
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// formatEffectiveConfiguration gives one line for each key: `name = value  # source`.
func formatEffectiveConfiguration(cfg appConfig) string {
	var b strings.Builder
	for _, setting := range effectiveConfiguration(cfg) {
		fmt.Fprintf(&b, "%s = %s  # %s\n", setting.Name, setting.Value, setting.Source)
	}
	return b.String()
}
