package main

import (
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

type appConfig struct {
	Listen          string
	JobHistoryLimit int
	Runtime         runtimePolicy
	// Access is the access block: the policy of access (access_config.go). The block is mandatory.
	Access          accessConfig
	Authentications []authConfig
	Buckets         []bucketConfig
	// Volumes are the volume blocks: the folders of the server that the browser shows as buckets (volume_config.go).
	Volumes    []volumeConfig
	SourceDir  string
	SourceName string
	// Sources gives the source of each key that the file sets: "file",
	// "env:VAR" or "file:/path". A key that is not in it has its default.
	Sources map[string]string
	// DurationTexts gives the text of each duration key of the access block, as the file writes it or as the
	// default is: print-config shows this text. The key is the path of the key, for example "access.pdp.timeout".
	DurationTexts map[string]string
}

type authConfig struct {
	ID                 string
	Provider           string
	Mode               string
	Endpoint           string
	Region             string
	AccessKeyID        string
	SecretAccessKey    string
	SessionToken       string
	CredentialsFile    string
	InsecureSkipVerify bool
	// Usage is the usage block: the admin API that gives the usage and the quota of the buckets. Nil: no block.
	Usage *usageConfig
}

// usageConfig is the usage block of an auth block (refer to usage.go).
type usageConfig struct {
	// Source is usageSourceGarage, usageSourceSeaweedFS or usageSourceMinIO.
	Source string
	// Endpoint is the admin API of Garage, the master of SeaweedFS, or the S3 endpoint of MinIO.
	Endpoint string
	// FilerEndpoint is the filer of SeaweedFS. It is empty for the other sources.
	FilerEndpoint string
	// Token is the admin token of Garage. It is a secret. It is empty for the other sources.
	Token string
}

type bucketConfig struct {
	ID                 string
	Name               string
	AuthID             string
	Provider           string
	Bucket             string
	Region             string
	Permissions        []string
	PermissionsDefined bool
	RootPrefix         string
	MaxScanPages       int
	// Encryption is "" (no encryption header) or encryptionSSES3 (x-amz-server-side-encryption: AES256 on each write).
	Encryption string
	// QuotaBytes is the limit of the bucket that the operator sets. 0: no such limit.
	QuotaBytes int64
}

const (
	maxConfigurationBytes = int64(4 << 20)
	maxSecretFileBytes    = int64(1 << 20)
	// maxUsageTokenBytes is the longest admin token of a usage block.
	maxUsageTokenBytes = 4096
	// maxQuotaBytes is the largest value of bucket.quota_bytes (1 EiB).
	maxQuotaBytes = int64(1 << 60)
)

func loadConfig(filename string) (appConfig, error) {
	filename = strings.TrimSpace(filename)
	if filename == "" {
		return appConfig{}, fmt.Errorf("configuration path cannot be empty")
	}
	abs, err := filepath.Abs(filename)
	if err != nil {
		return appConfig{}, fmt.Errorf("resolve configuration path: %w", err)
	}
	data, err := readBoundedFile(abs, maxConfigurationBytes)
	if err != nil {
		return appConfig{}, fmt.Errorf("read configuration %q: %w", abs, err)
	}
	return decodeConfig(string(data), abs, filepath.Dir(abs))
}

func readBoundedFile(filename string, maximum int64) ([]byte, error) {
	if maximum < 1 {
		return nil, fmt.Errorf("file size limit must be positive")
	}
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, fmt.Errorf("file exceeds the %d-byte limit", maximum)
	}
	return data, nil
}

func loadRuntimeConfig(configPath string) (appConfig, error) {
	configPath = strings.TrimSpace(configPath)
	if configPath == "" {
		return appConfig{}, fmt.Errorf("configuration path is required; use -c <path> or --config <path>")
	}
	return loadConfig(configPath)
}

func decodeConfig(data, sourceName, sourceDir string) (appConfig, error) {
	root, err := parseHCLSubset(data)
	if err != nil {
		return appConfig{}, fmt.Errorf("parse configuration %q: %w", sourceName, err)
	}

	cfg := appConfig{
		Listen:          ":8080",
		JobHistoryLimit: 10,
		Runtime:         defaultRuntimePolicy(),
		SourceDir:       sourceDir,
		SourceName:      sourceName,
		Sources:         make(map[string]string),
		DurationTexts:   make(map[string]string),
	}

	if len(root.Attrs) != 0 {
		for name, attr := range root.Attrs {
			return appConfig{}, fmt.Errorf("line %d:%d: root attribute %q is not supported; use a block", attr.Line, attr.Column, name)
		}
	}

	var serverSeen, accessSeen bool
	authByID := make(map[string]authConfig)
	bucketIDs := make(map[string]struct{})
	bucketBlocks := make([]hclBlock, 0)

	for _, block := range root.Blocks {
		switch block.Type {
		case "server":
			if serverSeen {
				return appConfig{}, fmt.Errorf("only one server block is allowed")
			}
			serverSeen = true
			if err := decodeServerBlock(block, &cfg); err != nil {
				return appConfig{}, err
			}
			recordSources(&cfg, "server", "server", block, sourceDir)
		case "access":
			if accessSeen {
				return appConfig{}, block.errorf("only one access block is allowed")
			}
			accessSeen = true
			if err := decodeAccessBlock(block, &cfg, sourceDir); err != nil {
				return appConfig{}, err
			}
		case "auth":
			auth, err := decodeAuthBlock(block, sourceDir)
			if err != nil {
				return appConfig{}, err
			}
			recordSources(&cfg, "auth "+strconv.Quote(auth.ID), "auth", block, sourceDir)
			if usageBlock, _ := authUsageBlock(block); usageBlock != nil {
				recordSources(&cfg, "auth "+strconv.Quote(auth.ID)+".usage", "auth.usage", *usageBlock, sourceDir)
			}
			if _, exists := authByID[auth.ID]; exists {
				return appConfig{}, block.errorf("duplicate auth id %q", auth.ID)
			}
			authByID[auth.ID] = auth
			cfg.Authentications = append(cfg.Authentications, auth)
		case "bucket":
			if len(block.Labels) != 1 {
				return appConfig{}, block.errorf("bucket block requires exactly one quoted identifier")
			}
			id := block.Labels[0]
			if _, exists := bucketIDs[id]; exists {
				return appConfig{}, block.errorf("duplicate bucket id %q", id)
			}
			bucketIDs[id] = struct{}{}
			bucketBlocks = append(bucketBlocks, block)
		case "volume":
			volume, err := decodeVolumeBlock(block)
			if err != nil {
				return appConfig{}, err
			}
			// The buckets and the volumes are the instances of the interface: one label names one instance.
			if _, exists := bucketIDs[volume.ID]; exists {
				return appConfig{}, block.errorf("duplicate bucket or volume id %q: a volume block and a bucket block cannot have the same label", volume.ID)
			}
			bucketIDs[volume.ID] = struct{}{}
			cfg.Volumes = append(cfg.Volumes, volume)
			recordSources(&cfg, "volume "+strconv.Quote(volume.ID), "volume", block, sourceDir)
		default:
			return appConfig{}, block.errorf("unknown block type %q; supported blocks are server, access, auth, bucket and volume", block.Type)
		}
	}

	if len(bucketBlocks) == 0 && len(cfg.Volumes) == 0 {
		return appConfig{}, fmt.Errorf("at least one bucket block or one volume block is required")
	}
	if len(bucketBlocks) > 0 && len(authByID) == 0 {
		return appConfig{}, fmt.Errorf("at least one auth block is required")
	}
	// The address of a bucket in the interface is /-/<auth>/<bucket>/, and the address of a volume is
	// /-/volume/<volume>/: an auth block with the label "volume" makes two instances with the same address.
	if _, exists := authByID[volumeHostID]; exists && len(cfg.Volumes) > 0 {
		return appConfig{}, fmt.Errorf("auth %q: the label %q is the address word of the volumes in the interface (/-/%s/<id>/), and the file has a volume block; give the auth block an other label", volumeHostID, volumeHostID, volumeHostID)
	}
	for _, block := range bucketBlocks {
		bucket, err := decodeBucketBlock(block, authByID)
		if err != nil {
			return appConfig{}, err
		}
		cfg.Buckets = append(cfg.Buckets, bucket)
		recordSources(&cfg, "bucket "+strconv.Quote(bucket.ID), "bucket", block, sourceDir)
	}
	if err := validateRuntimePolicy(&cfg); err != nil {
		return appConfig{}, err
	}
	if !accessSeen {
		return appConfig{}, fmt.Errorf("the access block is required: add `access { mode = \"open\" }` (no check), or an access block with mode = \"authzen\", an identity block and a pdp block")
	}
	if err := validateAccessResources(cfg); err != nil {
		return appConfig{}, err
	}

	return cfg, nil
}

func decodeServerBlock(block hclBlock, cfg *appConfig) error {
	if cfg == nil {
		return fmt.Errorf("configuration is nil")
	}
	if len(block.Labels) != 0 {
		return block.errorf("server block does not accept labels")
	}
	if len(block.Blocks) != 0 {
		return block.errorf("nested blocks are not supported inside server")
	}
	serverKinds := map[string]hclValueKind{
		"listen": hclString, "job_history_limit": hclNumber,
		"access_mode": hclString, "log_mode": hclString,
		"allow_full_object_fallback": hclBool,
		"memory_limit_bytes":         hclNumber, "max_storage_bytes_per_request": hclNumber,
		"max_storage_requests_per_request": hclNumber,
		"max_range_cache_bytes":            hclNumber, "max_concurrent_storage_requests": hclNumber,
		"max_concurrent_requests_per_storage": hclNumber, "session_ttl_seconds": hclNumber,
		"max_stats_folders": hclNumber, "max_archive_entries": hclNumber,
		"max_background_storage_bytes": hclNumber, "max_background_storage_requests": hclNumber,
	}
	if err := rejectUnknownAttrs(block, attributeNames("server")...); err != nil {
		return err
	}
	if err := requireAttrKinds(block, serverKinds); err != nil {
		return err
	}
	if value, ok := block.stringAttr("listen"); ok {
		listen, err := normalizeListenAddress(value)
		if err != nil {
			return block.errorf("server.listen: %v, got %q", err, value)
		}
		cfg.Listen = listen
	}
	if value, ok := block.intAttr("job_history_limit"); ok {
		if value < 1 || value > 20 {
			return block.errorf("server.job_history_limit must be between 1 and 20, got %d", value)
		}
		cfg.JobHistoryLimit = int(value)
	}
	if value, ok := block.stringAttr("access_mode"); ok {
		cfg.Runtime.AccessMode = value
	}
	if value, ok := block.stringAttr("log_mode"); ok {
		cfg.Runtime.LogMode = value
	}
	if value, ok := block.boolAttr("allow_full_object_fallback"); ok {
		cfg.Runtime.AllowFullObjectFallback = value
	}
	integerSettings := []struct {
		name        string
		destination *int64
	}{
		{"memory_limit_bytes", &cfg.Runtime.MemoryLimitBytes},
		{"max_storage_bytes_per_request", &cfg.Runtime.MaxStorageBytesPerRequest},
		{"max_storage_requests_per_request", &cfg.Runtime.MaxStorageRequestsPerRequest},
		{"max_range_cache_bytes", &cfg.Runtime.MaxRangeCacheBytes},
		{"max_background_storage_bytes", &cfg.Runtime.MaxBackgroundStorageBytes},
		{"max_background_storage_requests", &cfg.Runtime.MaxBackgroundStorageRequests},
	}
	for _, setting := range integerSettings {
		if value, ok := block.intAttr(setting.name); ok {
			*setting.destination = value
		}
	}
	if value, ok := block.intAttr("max_concurrent_storage_requests"); ok {
		cfg.Runtime.MaxConcurrentStorageRequests = int(value)
	}
	if value, ok := block.intAttr("max_concurrent_requests_per_storage"); ok {
		cfg.Runtime.MaxConcurrentRequestsPerStore = int(value)
	}
	if value, ok := block.intAttr("session_ttl_seconds"); ok {
		cfg.Runtime.SessionTTLSeconds = int(value)
	}
	if value, ok := block.intAttr("max_stats_folders"); ok {
		cfg.Runtime.MaxStatsFolders = int(value)
	}
	if value, ok := block.intAttr("max_archive_entries"); ok {
		cfg.Runtime.MaxArchiveEntries = int(value)
	}
	return nil
}

func validateRuntimePolicy(cfg *appConfig) error {
	if cfg == nil {
		return fmt.Errorf("configuration is nil")
	}
	listen, err := normalizeListenAddress(cfg.Listen)
	if err != nil {
		return fmt.Errorf("server.listen: %w", err)
	}
	cfg.Listen = listen
	switch cfg.Runtime.AccessMode {
	case accessModeInheritCredentials, accessModeForceReadOnly:
	default:
		return fmt.Errorf("server.access_mode must be %q or %q, got %q", accessModeInheritCredentials, accessModeForceReadOnly, cfg.Runtime.AccessMode)
	}
	switch cfg.Runtime.LogMode {
	case logModeAnonymous, logModeDetailed:
	default:
		return fmt.Errorf("server.log_mode must be %q or %q, got %q", logModeAnonymous, logModeDetailed, cfg.Runtime.LogMode)
	}
	validateRange := func(name string, value, minimum, maximum int64) error {
		if value < minimum || value > maximum {
			return fmt.Errorf("server.%s must be between %d and %d, got %d", name, minimum, maximum, value)
		}
		return nil
	}
	if cfg.Runtime.MemoryLimitBytes != 0 {
		if err := validateRange("memory_limit_bytes", cfg.Runtime.MemoryLimitBytes, 32<<20, 1<<50); err != nil {
			return err
		}
	}
	for _, setting := range []struct {
		name            string
		value, min, max int64
	}{
		{"max_storage_bytes_per_request", cfg.Runtime.MaxStorageBytesPerRequest, 1 << 20, 1 << 50},
		{"max_storage_requests_per_request", cfg.Runtime.MaxStorageRequestsPerRequest, 1, 10_000_000},
		{"max_range_cache_bytes", cfg.Runtime.MaxRangeCacheBytes, 1 << 20, 4 << 30},
		{"max_background_storage_bytes", cfg.Runtime.MaxBackgroundStorageBytes, 1 << 20, 1 << 50},
		{"max_background_storage_requests", cfg.Runtime.MaxBackgroundStorageRequests, 1, 10_000_000},
	} {
		if err := validateRange(setting.name, setting.value, setting.min, setting.max); err != nil {
			return err
		}
	}
	if cfg.Runtime.MaxConcurrentStorageRequests < 1 || cfg.Runtime.MaxConcurrentStorageRequests > 1024 {
		return fmt.Errorf("server.max_concurrent_storage_requests must be between 1 and 1024, got %d", cfg.Runtime.MaxConcurrentStorageRequests)
	}
	if cfg.Runtime.MaxConcurrentRequestsPerStore < 1 || cfg.Runtime.MaxConcurrentRequestsPerStore > 256 {
		return fmt.Errorf("server.max_concurrent_requests_per_storage must be between 1 and 256, got %d", cfg.Runtime.MaxConcurrentRequestsPerStore)
	}
	if cfg.Runtime.MaxConcurrentRequestsPerStore > cfg.Runtime.MaxConcurrentStorageRequests {
		return fmt.Errorf("server.max_concurrent_requests_per_storage (%d) cannot exceed max_concurrent_storage_requests (%d)", cfg.Runtime.MaxConcurrentRequestsPerStore, cfg.Runtime.MaxConcurrentStorageRequests)
	}
	if cfg.Runtime.SessionTTLSeconds < 30 || cfg.Runtime.SessionTTLSeconds > 7*24*60*60 {
		return fmt.Errorf("server.session_ttl_seconds must be between 30 and 604800, got %d", cfg.Runtime.SessionTTLSeconds)
	}
	if cfg.Runtime.MaxStatsFolders < 100 || cfg.Runtime.MaxStatsFolders > 1_000_000 {
		return fmt.Errorf("server.max_stats_folders must be between 100 and 1000000, got %d", cfg.Runtime.MaxStatsFolders)
	}
	if cfg.Runtime.MaxArchiveEntries < 1 || cfg.Runtime.MaxArchiveEntries > 1_000_000 {
		return fmt.Errorf("server.max_archive_entries must be between 1 and 1000000, got %d", cfg.Runtime.MaxArchiveEntries)
	}
	return nil
}

var configIDPattern = regexp.MustCompile(`^(?:[A-Za-z0-9][A-Za-z0-9._-]*|-)$`)
var instanceIDPattern = regexp.MustCompile(`^(?:[A-Za-z0-9][A-Za-z0-9._-]*|-)$`)

func decodeAuthBlock(block hclBlock, baseDir string) (authConfig, error) {
	if len(block.Labels) != 1 {
		return authConfig{}, block.errorf("auth block requires exactly one quoted identifier")
	}
	if err := rejectClearSecrets(block, "auth "+strconv.Quote(block.Labels[0]), "auth"); err != nil {
		return authConfig{}, err
	}
	allowed := attributeNames("auth")
	if err := rejectUnknownAttrs(block, allowed...); err != nil {
		return authConfig{}, err
	}
	kinds := make(map[string]hclValueKind, len(allowed))
	for _, name := range allowed {
		kinds[name] = hclString
	}
	kinds["insecure_skip_verify"] = hclBool
	if err := requireAttrKinds(block, kinds); err != nil {
		return authConfig{}, err
	}
	usageBlock, err := authUsageBlock(block)
	if err != nil {
		return authConfig{}, err
	}

	auth := authConfig{ID: block.Labels[0]}
	if !configIDPattern.MatchString(auth.ID) {
		return authConfig{}, block.errorf("auth label must match %s, got %q", configIDPattern.String(), auth.ID)
	}
	path := "auth " + strconv.Quote(auth.ID)
	for _, field := range []struct {
		name string
		dst  *string
	}{
		{"provider", &auth.Provider},
		{"mode", &auth.Mode},
		{"endpoint", &auth.Endpoint},
		{"region", &auth.Region},
		{"credentials_file", &auth.CredentialsFile},
	} {
		value, _, err := textAttr(block, path, field.name)
		if err != nil {
			return authConfig{}, err
		}
		*field.dst = value
	}
	if value, ok := block.boolAttr("insecure_skip_verify"); ok {
		auth.InsecureSkipVerify = value
	}
	if strings.HasSuffix(auth.Endpoint, "/") {
		return authConfig{}, block.errorf("%s.endpoint must not end with \"/\", got %q", path, auth.Endpoint)
	}

	// access_key_id is an identifier, not a secret: it accepts the three forms.
	accessKey, accessDefined, err := resolveValueSource(block, path, "access_key_id", baseDir, formValue|formEnv|formFile)
	if err != nil {
		return authConfig{}, err
	}
	if accessDefined && (strings.TrimSpace(accessKey) != accessKey || hasControlCharacter(accessKey)) {
		return authConfig{}, block.errorf("%s.access_key_id must not have a space at the start or at the end or a control character, got %q", path, accessKey)
	}
	secretAccessKey, secretDefined, err := resolveSecretSource(block, path, "secret_access_key", baseDir)
	if err != nil {
		return authConfig{}, err
	}
	sessionToken, sessionDefined, err := resolveSecretSource(block, path, "session_token", baseDir)
	if err != nil {
		return authConfig{}, err
	}
	auth.AccessKeyID = accessKey
	auth.SecretAccessKey = secretAccessKey
	auth.SessionToken = sessionToken

	if auth.Provider == "" {
		return authConfig{}, block.errorf("%s.provider is required: \"s3\" or \"gcs\"", path)
	}
	switch auth.Provider {
	case "s3":
		if auth.CredentialsFile != "" {
			return authConfig{}, block.errorf("%s.credentials_file is only supported for provider \"gcs\"", path)
		}
		if auth.Endpoint == "" {
			return authConfig{}, block.errorf("%s.endpoint is required for provider \"s3\"", path)
		}
		if auth.Region == "" {
			return authConfig{}, block.errorf("%s.region is required for provider \"s3\"", path)
		}
		if auth.Mode == "" {
			if accessDefined || secretDefined || sessionDefined {
				auth.Mode = "access_key"
			} else {
				auth.Mode = "anonymous"
			}
		}
		switch auth.Mode {
		case "access_key":
			if !accessDefined || !secretDefined {
				return authConfig{}, block.errorf("%s: mode \"access_key\" requires access_key_id and secret_access_key (_env or _file)", path)
			}
		case "anonymous":
			if accessDefined || secretDefined || sessionDefined {
				return authConfig{}, block.errorf("%s: mode \"anonymous\" cannot define access credentials", path)
			}
		default:
			return authConfig{}, block.errorf("%s.mode must be \"access_key\" or \"anonymous\" for provider \"s3\", got %q", path, auth.Mode)
		}
	case "gcs":
		if accessDefined || secretDefined || sessionDefined {
			return authConfig{}, block.errorf("%s: the S3 credential attributes are only supported for provider \"s3\"", path)
		}
		if auth.Endpoint == "" {
			auth.Endpoint = "https://storage.googleapis.com"
		}
		if auth.Mode == "" {
			if auth.CredentialsFile != "" {
				auth.Mode = "service_account"
			} else {
				auth.Mode = "anonymous"
			}
		}
		switch auth.Mode {
		case "service_account":
			if auth.CredentialsFile == "" {
				return authConfig{}, block.errorf("%s: mode \"service_account\" requires credentials_file", path)
			}
		case "anonymous":
			if auth.CredentialsFile != "" {
				return authConfig{}, block.errorf("%s: mode \"anonymous\" cannot define credentials_file", path)
			}
		default:
			return authConfig{}, block.errorf("%s.mode must be \"service_account\" or \"anonymous\" for provider \"gcs\", got %q", path, auth.Mode)
		}
	default:
		return authConfig{}, block.errorf("%s.provider must be \"s3\" or \"gcs\", got %q", path, auth.Provider)
	}
	if auth.CredentialsFile != "" && !filepath.IsAbs(auth.CredentialsFile) {
		auth.CredentialsFile = filepath.Join(baseDir, auth.CredentialsFile)
	}
	if auth.CredentialsFile != "" {
		auth.CredentialsFile = filepath.Clean(auth.CredentialsFile)
	}
	if _, err := parseStorageEndpoint(auth.Provider, auth.Endpoint); err != nil {
		return authConfig{}, block.errorf("%s.endpoint: %v, got %q", path, err, auth.Endpoint)
	}
	if usageBlock != nil {
		if auth.Usage, err = decodeUsageBlock(*usageBlock, auth, baseDir); err != nil {
			return authConfig{}, err
		}
	}
	return auth, nil
}

// authUsageBlock gives the usage block of an auth block, or nil. The usage block is the only nested block of auth,
// and it can be there one time.
func authUsageBlock(block hclBlock) (*hclBlock, error) {
	var usage *hclBlock
	for index := range block.Blocks {
		nested := &block.Blocks[index]
		if nested.Type != "usage" {
			return nil, nested.errorf("unknown block %q in auth; the only block is usage", nested.Type)
		}
		if usage != nil {
			return nil, nested.errorf("only one usage block is allowed in auth %q", block.Labels[0])
		}
		usage = nested
	}
	return usage, nil
}

// decodeUsageBlock reads the usage block of an auth block. The source is explicit: the server never guesses it. Each
// source has its own keys, and a key of another source is a contradiction.
func decodeUsageBlock(block hclBlock, auth authConfig, baseDir string) (*usageConfig, error) {
	path := "auth " + strconv.Quote(auth.ID) + ".usage"
	if len(block.Labels) != 0 {
		return nil, block.errorf("%s block does not accept labels", path)
	}
	if len(block.Blocks) != 0 {
		return nil, block.errorf("nested blocks are not supported inside %s", path)
	}
	if err := rejectClearSecrets(block, path, "auth.usage"); err != nil {
		return nil, err
	}
	if err := rejectUnknownAttrs(block, attributeNames("auth.usage")...); err != nil {
		return nil, err
	}
	kinds := make(map[string]hclValueKind, len(block.Attrs))
	for name := range block.Attrs {
		kinds[name] = hclString
	}
	if err := requireAttrKinds(block, kinds); err != nil {
		return nil, err
	}
	usage := &usageConfig{}
	source, defined, err := textAttr(block, path, "source")
	if err != nil {
		return nil, err
	}
	if !defined {
		return nil, block.errorf("%s.source is required: %q, %q or %q", path, usageSourceGarage, usageSourceSeaweedFS, usageSourceMinIO)
	}
	switch source {
	case usageSourceGarage, usageSourceSeaweedFS, usageSourceMinIO:
	default:
		return nil, block.errorf("%s.source must be %q, %q or %q (lower case), got %q", path, usageSourceGarage, usageSourceSeaweedFS, usageSourceMinIO, source)
	}
	usage.Source = source
	if auth.Provider != "s3" {
		return nil, block.errorf("%s: a usage block needs provider \"s3\", but auth %q has provider %q; remove the block", path, auth.ID, auth.Provider)
	}
	endpoint, endpointDefined, err := textAttr(block, path, "endpoint")
	if err != nil {
		return nil, err
	}
	filer, filerDefined, err := textAttr(block, path, "filer_endpoint")
	if err != nil {
		return nil, err
	}
	for _, field := range []struct{ name, value string }{{"endpoint", endpoint}, {"filer_endpoint", filer}} {
		if field.value == "" {
			continue
		}
		if err := validateUsageEndpoint(field.value); err != nil {
			// A URL with user information can hold a password: the error does not show such a value.
			if parsed, parseErr := url.Parse(field.value); parseErr == nil && parsed.User != nil {
				return nil, block.errorf("%s.%s: %v", path, field.name, err)
			}
			return nil, block.errorf("%s.%s: %v, got %q", path, field.name, err, field.value)
		}
	}
	token, tokenDefined, err := resolveValueSource(block, path, "token", baseDir, formEnv|formFile)
	if err != nil {
		return nil, err
	}
	if tokenDefined && (len(token) > maxUsageTokenBytes || strings.ContainsFunc(token, func(r rune) bool { return r <= ' ' || r == 0x7f })) {
		// The error never shows the token.
		return nil, block.errorf("%s.token must have 1 to %d bytes without a space or a control character", path, maxUsageTokenBytes)
	}
	switch source {
	case usageSourceGarage:
		if !endpointDefined {
			return nil, block.errorf("%s.endpoint is required with source %q: the URL of the admin API of Garage (port 3903 by default)", path, source)
		}
		if !tokenDefined {
			return nil, block.errorf("%s.token is required with source %q: give token_env or token_file (the admin token of Garage)", path, source)
		}
		if filerDefined {
			return nil, block.errorf("%s.filer_endpoint is only for source %q; remove it for source %q", path, usageSourceSeaweedFS, source)
		}
	case usageSourceSeaweedFS:
		if !endpointDefined {
			return nil, block.errorf("%s.endpoint is required with source %q: the URL of the master of SeaweedFS (port 9333 by default)", path, source)
		}
		if !filerDefined {
			return nil, block.errorf("%s.filer_endpoint is required with source %q: the URL of the filer of SeaweedFS (port 8888 by default)", path, source)
		}
		if tokenDefined {
			return nil, block.errorf("%s.token is only for source %q; remove it for source %q", path, usageSourceGarage, source)
		}
	case usageSourceMinIO:
		if filerDefined {
			return nil, block.errorf("%s.filer_endpoint is only for source %q; remove it for source %q", path, usageSourceSeaweedFS, source)
		}
		if tokenDefined {
			return nil, block.errorf("%s.token is only for source %q; source %q signs with the S3 credentials of auth %q", path, usageSourceGarage, source, auth.ID)
		}
		if auth.Mode != "access_key" {
			return nil, block.errorf("%s: source %q needs auth %q with mode \"access_key\" (the admin API of MinIO refuses anonymous requests), got mode %q", path, source, auth.ID, auth.Mode)
		}
		if !endpointDefined {
			endpoint = auth.Endpoint
		}
	}
	usage.Endpoint, usage.FilerEndpoint, usage.Token = endpoint, filer, token
	return usage, nil
}

// validateUsageEndpoint checks the URL of an admin API: http or https, a host, no credentials, no query, no fragment,
// and no "/" at the end.
func validateUsageEndpoint(value string) error {
	if _, err := parseStorageEndpoint("usage", value); err != nil {
		return err
	}
	if strings.HasSuffix(value, "/") {
		return fmt.Errorf("must not end with \"/\"")
	}
	return nil
}

func decodeBucketBlock(block hclBlock, authByID map[string]authConfig) (bucketConfig, error) {
	if len(block.Labels) != 1 {
		return bucketConfig{}, block.errorf("bucket block requires exactly one quoted identifier")
	}
	if err := rejectUnknownAttrs(block, attributeNames("bucket")...); err != nil {
		return bucketConfig{}, err
	}
	kinds := map[string]hclValueKind{
		"name": hclString, "auth": hclString, "bucket": hclString,
		"permissions": hclList, "root_prefix": hclString, "max_scan_pages": hclNumber, "encryption": hclString,
		"quota_bytes": hclNumber,
	}
	if err := requireAttrKinds(block, kinds); err != nil {
		return bucketConfig{}, err
	}
	if len(block.Blocks) != 0 {
		return bucketConfig{}, block.errorf("nested blocks are not supported inside bucket")
	}

	bucket := bucketConfig{
		ID:           block.Labels[0],
		Name:         block.Labels[0],
		MaxScanPages: 1,
	}
	if !instanceIDPattern.MatchString(bucket.ID) {
		return bucketConfig{}, block.errorf("bucket label must match %s, got %q", instanceIDPattern.String(), bucket.ID)
	}
	path := "bucket " + strconv.Quote(bucket.ID)
	if value, ok, err := textAttr(block, path, "name"); err != nil {
		return bucketConfig{}, err
	} else if ok {
		bucket.Name = value
	}
	var err error
	if bucket.AuthID, _, err = textAttr(block, path, "auth"); err != nil {
		return bucketConfig{}, err
	}
	if bucket.Bucket, _, err = textAttr(block, path, "bucket"); err != nil {
		return bucketConfig{}, err
	}
	if value, ok := block.stringAttr("root_prefix"); ok {
		// The value applies as it is: "" (the whole bucket), or a key prefix that ends with "/".
		if value != "" && (strings.HasPrefix(value, "/") || !strings.HasSuffix(value, "/") || strings.Contains(value, "\x00")) {
			return bucketConfig{}, block.errorf("%s.root_prefix must be \"\" or a prefix that ends with \"/\" and does not start with \"/\" (for example \"published/\"), got %q", path, value)
		}
		bucket.RootPrefix = value
	}
	if value, ok := block.intAttr("max_scan_pages"); ok {
		if value < 0 || value > 1_000_000 {
			return bucketConfig{}, block.errorf("%s.max_scan_pages must be between 0 and 1000000, got %d", path, value)
		}
		bucket.MaxScanPages = int(value)
	}
	if value, ok := block.stringAttr("encryption"); ok {
		// The value applies as it is: no change of case, no space removed.
		if value != "" && value != encryptionSSES3 {
			return bucketConfig{}, block.errorf("%s.encryption must be \"\" (no encryption header) or %q (x-amz-server-side-encryption: AES256), got %q; SSE-C and SSE-KMS are not supported", path, encryptionSSES3, value)
		}
		bucket.Encryption = value
	}
	if value, ok := block.intAttr("quota_bytes"); ok {
		if value < 1 || value > maxQuotaBytes {
			return bucketConfig{}, block.errorf("%s.quota_bytes must be between 1 and %d, got %d; remove the key for no limit", path, int64(maxQuotaBytes), value)
		}
		bucket.QuotaBytes = value
	}
	if values, ok := block.stringListAttr("permissions"); ok {
		bucket.PermissionsDefined = true
		for _, value := range values {
			switch value {
			case permissionRead, permissionWrite, permissionDelete:
				if containsString(bucket.Permissions, value) {
					return bucketConfig{}, block.errorf("%s.permissions contains %q two times", path, value)
				}
				bucket.Permissions = append(bucket.Permissions, value)
			default:
				return bucketConfig{}, block.errorf("%s.permissions must contain only \"read\", \"write\" and \"delete\", got %q", path, value)
			}
		}
	}
	if bucket.AuthID == "" {
		return bucketConfig{}, block.errorf("%s.auth is required", path)
	}
	if bucket.Bucket == "" {
		return bucketConfig{}, block.errorf("%s.bucket is required", path)
	}
	auth, ok := authByID[bucket.AuthID]
	if !ok {
		return bucketConfig{}, block.errorf("%s.auth: no auth block has the label %q", path, bucket.AuthID)
	}
	bucket.Provider = auth.Provider
	bucket.Region = auth.Region
	// The GCS API has no x-amz-server-side-encryption header: the two keys contradict each other.
	if bucket.Encryption != "" && bucket.Provider != "s3" {
		return bucketConfig{}, block.errorf("%s.encryption = %q needs an auth block with provider \"s3\", but auth %q has provider %q; remove the key for this bucket", path, bucket.Encryption, bucket.AuthID, bucket.Provider)
	}
	// The limit of the operator needs the used bytes, and only a usage block gives them.
	if bucket.QuotaBytes != 0 && auth.Usage == nil {
		return bucketConfig{}, block.errorf("%s.quota_bytes needs a usage block in auth %q: without it, the server cannot read the used bytes; add the block or remove the key", path, bucket.AuthID)
	}
	return bucket, nil
}

// resolveSecretSource reads an S3 secret of an auth block. A secret accepts
// only the forms <name>_env and <name>_file: a secret in clear text in the file
// stops the start. The value applies as it is, without its last line end. A
// value with a space or a control character is an error, and the error does
// not show the value.
func resolveSecretSource(block hclBlock, path, name, baseDir string) (string, bool, error) {
	value, defined, err := resolveValueSource(block, path, name, baseDir, formEnv|formFile)
	if err != nil || !defined {
		return "", false, err
	}
	if strings.ContainsFunc(value, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return "", false, block.errorf("%s.%s must not contain a space or a control character", path, name)
	}
	return value, true, nil
}

// textAttr reads a text attribute as it is in the file. The server does not
// change the value: an empty value, or a value with a space at the start or at
// the end, is an error.
func textAttr(block hclBlock, path, name string) (string, bool, error) {
	value, ok := block.stringAttr(name)
	if !ok {
		return "", false, nil
	}
	if value == "" {
		return "", false, block.errorf("%s.%s must not be empty; remove the attribute to use its default", path, name)
	}
	if strings.TrimSpace(value) != value {
		return "", false, block.errorf("%s.%s must not have a space at the start or at the end, got %q", path, name, value)
	}
	return value, true, nil
}

// rejectClearSecrets stops the start when the block has a secret in clear
// text. The error names the key and never shows the value.
func rejectClearSecrets(block hclBlock, path, blockType string) error {
	for _, key := range keysOfBlock(blockType) {
		if !key.Secret || key.Forms&formValue != 0 {
			continue
		}
		if attr, ok := block.Attrs[key.Name]; ok {
			return fmt.Errorf("line %d:%d: %s.%s is a secret: use %s_env or %s_file, not the value in clear text", attr.Line, attr.Column, path, key.Name, key.Name, key.Name)
		}
	}
	return nil
}

func rejectUnknownAttrs(block hclBlock, allowed ...string) error {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		allowedSet[name] = struct{}{}
	}
	for name, attr := range block.Attrs {
		if _, ok := allowedSet[name]; !ok {
			return fmt.Errorf("line %d:%d: unknown attribute %q in %s block", attr.Line, attr.Column, name, block.Type)
		}
	}
	return nil
}

func requireAttrKinds(block hclBlock, kinds map[string]hclValueKind) error {
	for name, attr := range block.Attrs {
		kind, ok := kinds[name]
		if !ok || attr.Value.Kind == kind {
			continue
		}
		want := map[hclValueKind]string{hclString: "a quoted string", hclBool: "a boolean", hclList: "a string list", hclNumber: "an integer"}[kind]
		return fmt.Errorf("line %d:%d: attribute %q must be %s", attr.Line, attr.Column, name, want)
	}
	return nil
}

func normalizeListenAddress(value string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("cannot be empty")
	}
	if strings.ContainsFunc(value, func(r rune) bool { return r <= ' ' }) {
		return "", fmt.Errorf("must not contain a space or a control character")
	}
	if strings.Contains(value, "://") {
		return "", fmt.Errorf("must be a host:port address, not a URL")
	}
	_, port, err := net.SplitHostPort(value)
	if err != nil {
		return "", fmt.Errorf("must be a valid host:port address: %w", err)
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return "", fmt.Errorf("port must be an integer between 1 and 65535")
	}
	return value, nil
}

func normalizePrefix(value string) string {
	value = strings.TrimLeft(value, "/")
	if value == "" {
		return ""
	}
	return strings.TrimRight(value, "/") + "/"
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// The parser below intentionally supports the small, documented HCL subset used
// by this project: blocks, quoted labels, attributes, strings, integers,
// booleans and string lists. Keeping it local avoids pulling a full HCL dependency into the
// single static binary.

type hclValueKind int

const (
	hclString hclValueKind = iota
	hclBool
	hclList
	hclNumber
)

type hclValue struct {
	Kind   hclValueKind
	String string
	Bool   bool
	List   []hclValue
	Number int64
	Line   int
	Column int
}

type hclAttr struct {
	Name   string
	Value  hclValue
	Line   int
	Column int
}

type hclBlock struct {
	Type   string
	Labels []string
	Attrs  map[string]hclAttr
	Blocks []hclBlock
	Line   int
	Column int
}

func (b hclBlock) errorf(format string, args ...any) error {
	return fmt.Errorf("line %d:%d: %s", b.Line, b.Column, fmt.Sprintf(format, args...))
}

func (b hclBlock) stringAttr(name string) (string, bool) {
	attr, ok := b.Attrs[name]
	if !ok {
		return "", false
	}
	if attr.Value.Kind != hclString {
		return "", false
	}
	return attr.Value.String, true
}

func (b hclBlock) boolAttr(name string) (bool, bool) {
	attr, ok := b.Attrs[name]
	if !ok || attr.Value.Kind != hclBool {
		return false, false
	}
	return attr.Value.Bool, true
}

func (b hclBlock) intAttr(name string) (int64, bool) {
	attr, ok := b.Attrs[name]
	if !ok || attr.Value.Kind != hclNumber {
		return 0, false
	}
	return attr.Value.Number, true
}

func (b hclBlock) stringListAttr(name string) ([]string, bool) {
	attr, ok := b.Attrs[name]
	if !ok || attr.Value.Kind != hclList {
		return nil, false
	}
	out := make([]string, 0, len(attr.Value.List))
	for _, value := range attr.Value.List {
		if value.Kind != hclString {
			return nil, false
		}
		out = append(out, value.String)
	}
	return out, true
}

type hclTokenKind int

const (
	tokenEOF hclTokenKind = iota
	tokenIdent
	tokenString
	tokenNumber
	tokenLBrace
	tokenRBrace
	tokenLBracket
	tokenRBracket
	tokenEqual
	tokenComma
)

type hclToken struct {
	Kind   hclTokenKind
	Text   string
	Line   int
	Column int
}

type hclLexer struct {
	source string
	pos    int
	line   int
	column int
}

func newHCLLexer(source string) *hclLexer {
	return &hclLexer{source: source, line: 1, column: 1}
}

func (l *hclLexer) nextToken() (hclToken, error) {
	if err := l.skipSpaceAndComments(); err != nil {
		return hclToken{}, err
	}
	if l.pos >= len(l.source) {
		return hclToken{Kind: tokenEOF, Line: l.line, Column: l.column}, nil
	}
	line, column := l.line, l.column
	ch := l.source[l.pos]
	switch ch {
	case '{':
		l.advanceByte()
		return hclToken{Kind: tokenLBrace, Text: "{", Line: line, Column: column}, nil
	case '}':
		l.advanceByte()
		return hclToken{Kind: tokenRBrace, Text: "}", Line: line, Column: column}, nil
	case '[':
		l.advanceByte()
		return hclToken{Kind: tokenLBracket, Text: "[", Line: line, Column: column}, nil
	case ']':
		l.advanceByte()
		return hclToken{Kind: tokenRBracket, Text: "]", Line: line, Column: column}, nil
	case '=':
		l.advanceByte()
		return hclToken{Kind: tokenEqual, Text: "=", Line: line, Column: column}, nil
	case ',':
		l.advanceByte()
		return hclToken{Kind: tokenComma, Text: ",", Line: line, Column: column}, nil
	case '"':
		return l.scanString()
	default:
		if ch >= '0' && ch <= '9' {
			start := l.pos
			for l.pos < len(l.source) && l.source[l.pos] >= '0' && l.source[l.pos] <= '9' {
				l.advanceByte()
			}
			return hclToken{Kind: tokenNumber, Text: l.source[start:l.pos], Line: line, Column: column}, nil
		}
		if isIdentStart(rune(ch)) {
			start := l.pos
			for l.pos < len(l.source) && isIdentPart(rune(l.source[l.pos])) {
				l.advanceByte()
			}
			return hclToken{Kind: tokenIdent, Text: l.source[start:l.pos], Line: line, Column: column}, nil
		}
		return hclToken{}, fmt.Errorf("line %d:%d: unexpected character %q", line, column, ch)
	}
}

func (l *hclLexer) skipSpaceAndComments() error {
	for l.pos < len(l.source) {
		ch := l.source[l.pos]
		if unicode.IsSpace(rune(ch)) {
			l.advanceByte()
			continue
		}
		if ch == '#' {
			l.skipLine()
			continue
		}
		if ch == '/' && l.pos+1 < len(l.source) {
			next := l.source[l.pos+1]
			if next == '/' {
				l.skipLine()
				continue
			}
			if next == '*' {
				startLine, startColumn := l.line, l.column
				l.advanceByte()
				l.advanceByte()
				for l.pos+1 < len(l.source) && !(l.source[l.pos] == '*' && l.source[l.pos+1] == '/') {
					l.advanceByte()
				}
				if l.pos+1 >= len(l.source) {
					return fmt.Errorf("line %d:%d: unterminated block comment", startLine, startColumn)
				}
				l.advanceByte()
				l.advanceByte()
				continue
			}
		}
		break
	}
	return nil
}

func (l *hclLexer) skipLine() {
	for l.pos < len(l.source) {
		ch := l.source[l.pos]
		l.advanceByte()
		if ch == '\n' {
			return
		}
	}
}

func (l *hclLexer) scanString() (hclToken, error) {
	line, column := l.line, l.column
	start := l.pos
	l.advanceByte()
	for l.pos < len(l.source) {
		ch := l.source[l.pos]
		if ch == '\n' || ch == '\r' {
			return hclToken{}, fmt.Errorf("line %d:%d: quoted strings cannot contain a raw newline", line, column)
		}
		if ch == '\\' {
			l.advanceByte()
			if l.pos >= len(l.source) {
				break
			}
			l.advanceByte()
			continue
		}
		l.advanceByte()
		if ch == '"' {
			raw := l.source[start:l.pos]
			value, err := strconv.Unquote(raw)
			if err != nil {
				return hclToken{}, fmt.Errorf("line %d:%d: invalid string: %w", line, column, err)
			}
			return hclToken{Kind: tokenString, Text: value, Line: line, Column: column}, nil
		}
	}
	return hclToken{}, fmt.Errorf("line %d:%d: unterminated string", line, column)
}

func (l *hclLexer) advanceByte() {
	if l.pos >= len(l.source) {
		return
	}
	if l.source[l.pos] == '\n' {
		l.line++
		l.column = 1
	} else {
		l.column++
	}
	l.pos++
}

func isIdentStart(r rune) bool {
	return unicode.IsLetter(r) || r == '_'
}

func isIdentPart(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-'
}

type hclParser struct {
	lexer  *hclLexer
	peeked *hclToken
}

func parseHCLSubset(source string) (hclBlock, error) {
	p := &hclParser{lexer: newHCLLexer(source)}
	root := hclBlock{Type: "root", Attrs: map[string]hclAttr{}, Line: 1, Column: 1}
	if err := p.parseBody(&root, false); err != nil {
		return hclBlock{}, err
	}
	return root, nil
}

func (p *hclParser) parseBody(target *hclBlock, stopAtBrace bool) error {
	for {
		tok, err := p.peek()
		if err != nil {
			return err
		}
		if tok.Kind == tokenEOF {
			if stopAtBrace {
				return fmt.Errorf("line %d:%d: expected closing brace", target.Line, target.Column)
			}
			return nil
		}
		if tok.Kind == tokenRBrace {
			if !stopAtBrace {
				return fmt.Errorf("line %d:%d: unexpected closing brace", tok.Line, tok.Column)
			}
			_, _ = p.next()
			return nil
		}
		if tok.Kind != tokenIdent {
			return fmt.Errorf("line %d:%d: expected attribute or block name", tok.Line, tok.Column)
		}
		name, _ := p.next()
		next, err := p.peek()
		if err != nil {
			return err
		}
		if next.Kind == tokenEqual {
			_, _ = p.next()
			value, err := p.parseValue()
			if err != nil {
				return err
			}
			if _, exists := target.Attrs[name.Text]; exists {
				return fmt.Errorf("line %d:%d: duplicate attribute %q", name.Line, name.Column, name.Text)
			}
			target.Attrs[name.Text] = hclAttr{Name: name.Text, Value: value, Line: name.Line, Column: name.Column}
			continue
		}

		block := hclBlock{Type: name.Text, Attrs: map[string]hclAttr{}, Line: name.Line, Column: name.Column}
		for next.Kind == tokenString || next.Kind == tokenIdent {
			label, _ := p.next()
			block.Labels = append(block.Labels, label.Text)
			next, err = p.peek()
			if err != nil {
				return err
			}
		}
		if next.Kind != tokenLBrace {
			return fmt.Errorf("line %d:%d: expected '=' or '{' after %q", next.Line, next.Column, name.Text)
		}
		_, _ = p.next()
		if err := p.parseBody(&block, true); err != nil {
			return err
		}
		target.Blocks = append(target.Blocks, block)
	}
}

func (p *hclParser) parseValue() (hclValue, error) {
	tok, err := p.next()
	if err != nil {
		return hclValue{}, err
	}
	switch tok.Kind {
	case tokenString:
		return hclValue{Kind: hclString, String: tok.Text, Line: tok.Line, Column: tok.Column}, nil
	case tokenNumber:
		value, parseErr := strconv.ParseInt(tok.Text, 10, 64)
		if parseErr != nil {
			return hclValue{}, fmt.Errorf("line %d:%d: invalid integer %q", tok.Line, tok.Column, tok.Text)
		}
		return hclValue{Kind: hclNumber, Number: value, Line: tok.Line, Column: tok.Column}, nil
	case tokenIdent:
		switch tok.Text {
		case "true":
			return hclValue{Kind: hclBool, Bool: true, Line: tok.Line, Column: tok.Column}, nil
		case "false":
			return hclValue{Kind: hclBool, Bool: false, Line: tok.Line, Column: tok.Column}, nil
		default:
			return hclValue{}, fmt.Errorf("line %d:%d: bare value %q is not supported; quote strings", tok.Line, tok.Column, tok.Text)
		}
	case tokenLBracket:
		list := hclValue{Kind: hclList, Line: tok.Line, Column: tok.Column}
		for {
			next, err := p.peek()
			if err != nil {
				return hclValue{}, err
			}
			if next.Kind == tokenRBracket {
				_, _ = p.next()
				return list, nil
			}
			value, err := p.parseValue()
			if err != nil {
				return hclValue{}, err
			}
			if value.Kind != hclString {
				return hclValue{}, fmt.Errorf("line %d:%d: lists may only contain strings", value.Line, value.Column)
			}
			list.List = append(list.List, value)
			next, err = p.peek()
			if err != nil {
				return hclValue{}, err
			}
			if next.Kind == tokenComma {
				_, _ = p.next()
				continue
			}
			if next.Kind != tokenRBracket {
				return hclValue{}, fmt.Errorf("line %d:%d: expected ',' or ']'", next.Line, next.Column)
			}
		}
	default:
		return hclValue{}, fmt.Errorf("line %d:%d: expected a value", tok.Line, tok.Column)
	}
}

func (p *hclParser) peek() (hclToken, error) {
	if p.peeked != nil {
		return *p.peeked, nil
	}
	tok, err := p.lexer.nextToken()
	if err != nil {
		return hclToken{}, err
	}
	p.peeked = &tok
	return tok, nil
}

func (p *hclParser) next() (hclToken, error) {
	if p.peeked != nil {
		tok := *p.peeked
		p.peeked = nil
		return tok, nil
	}
	return p.lexer.nextToken()
}

// hasControlCharacter tells if the value has a control character (C0, DEL or C1).
func hasControlCharacter(value string) bool {
	for _, r := range value {
		if r < 0x20 || (r >= 0x7f && r < 0xa0) {
			return true
		}
	}
	return false
}
