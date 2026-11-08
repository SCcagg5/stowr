package main

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The access block of the configuration (docs/ACCESS.md). The block is mandatory. It sets the policy of access:
// mode "authzen" (the server checks the identity token and asks the PDP) or mode "open" (no check).

const (
	accessModeAuthZEN = "authzen"
	accessModeOpen    = "open"

	defaultIdentityHeader = "X-Identity-Token"

	// The defaults of the durations, as text. The text is the form that the file uses.
	defaultIdentityMaxAgeText    = "5m"
	defaultIdentityClockSkewText = "60s"
	defaultPDPTimeoutText        = "500ms"
	defaultPDPUsageTimeoutText   = "2s"
	defaultPDPCacheTTLText       = "30s"

	defaultPDPMaxEvaluations = 64
	maxPDPEvaluations        = 1024

	maxIdentityHeaderChars = 64
	maxIssuerBytes         = 1024
	maxPDPTokenBytes       = 4096

	// maxAccessResources is the most bucket blocks and volume blocks together: the action document lists one
	// resource for each of them (ProcurAuth 5.3c).
	maxAccessResources = 256
)

// accessConfig is the typed form of the access block. The rest of the code reads only this structure.
type accessConfig struct {
	// Mode is accessModeAuthZEN or accessModeOpen.
	Mode     string
	Header   string
	Issuer   string
	Audience string
	Keys     []identityKey
	MaxAge   time.Duration
	// ClockSkew is the difference of clock that the server accepts for the times of a token.
	ClockSkew         time.Duration
	PDPURL            string
	PDPPlainHTTP      bool
	PDPToken          string
	PDPTimeout        time.Duration
	PDPUsageTimeout   time.Duration
	PDPCacheTTL       time.Duration
	PDPMaxEvaluations int
}

// enforced tells if the server checks the identity token and asks the PDP.
func (c accessConfig) enforced() bool { return c.Mode == accessModeAuthZEN }

// decodeAccessBlock reads the access block and its identity block and pdp block. It never changes a value.
func decodeAccessBlock(block hclBlock, cfg *appConfig, baseDir string) error {
	if len(block.Labels) != 0 {
		return block.errorf("access block does not accept labels")
	}
	if err := rejectUnknownAttrs(block, attributeNames("access")...); err != nil {
		return err
	}
	if err := requireAttrKinds(block, map[string]hclValueKind{"mode": hclString}); err != nil {
		return err
	}
	recordSources(cfg, "access", "access", block, baseDir)

	var identityBlock, pdpBlock *hclBlock
	for index := range block.Blocks {
		nested := &block.Blocks[index]
		switch nested.Type {
		case "identity":
			if identityBlock != nil {
				return nested.errorf("only one access.identity block is allowed")
			}
			identityBlock = nested
		case "pdp":
			if pdpBlock != nil {
				return nested.errorf("only one access.pdp block is allowed")
			}
			pdpBlock = nested
		default:
			return nested.errorf("unknown block %q in access; the blocks are identity and pdp", nested.Type)
		}
		if len(nested.Labels) != 0 {
			return nested.errorf("access.%s block does not accept labels", nested.Type)
		}
	}

	mode, ok := block.stringAttr("mode")
	if !ok {
		return block.errorf("access.mode is required: \"authzen\" or \"open\"")
	}
	if mode != accessModeAuthZEN && mode != accessModeOpen {
		return block.errorf("access.mode must be \"authzen\" or \"open\" (lower case), got %q", mode)
	}
	access := accessConfig{Mode: mode}

	if mode == accessModeOpen {
		for _, nested := range []*hclBlock{identityBlock, pdpBlock} {
			if nested != nil {
				return nested.errorf("access.mode = \"open\" contradicts the access.%s block; remove the block or set access.mode = \"authzen\"", nested.Type)
			}
		}
		cfg.Access = access
		return nil
	}
	if identityBlock == nil {
		return block.errorf("access.mode = \"authzen\" needs the access.identity block; add the block or set access.mode = \"open\"")
	}
	if pdpBlock == nil {
		return block.errorf("access.mode = \"authzen\" needs the access.pdp block; add the block or set access.mode = \"open\"")
	}
	if err := decodeIdentityBlock(*identityBlock, cfg, &access, baseDir); err != nil {
		return err
	}
	if err := decodePDPBlock(*pdpBlock, cfg, &access, baseDir); err != nil {
		return err
	}
	cfg.Access = access
	return nil
}

// decodeIdentityBlock reads the access.identity block: the rules to check the identity token.
func decodeIdentityBlock(block hclBlock, cfg *appConfig, access *accessConfig, baseDir string) error {
	const path = "access.identity"
	if len(block.Blocks) != 0 {
		return block.errorf("nested blocks are not supported inside %s", path)
	}
	if err := rejectUnknownAttrs(block, attributeNames(path)...); err != nil {
		return err
	}
	kinds := make(map[string]hclValueKind, len(block.Attrs))
	for name := range block.Attrs {
		kinds[name] = hclString
	}
	if err := requireAttrKinds(block, kinds); err != nil {
		return err
	}
	recordSources(cfg, path, path, block, baseDir)

	access.Header = defaultIdentityHeader
	if value, ok := block.stringAttr("header"); ok {
		if err := validateIdentityHeader(value); err != nil {
			return block.errorf("%s.header: %v, got %q", path, err, value)
		}
		access.Header = value
	}
	issuer, ok := block.stringAttr("issuer")
	if !ok {
		return block.errorf("%s.issuer is required: the iss of the identity tokens", path)
	}
	if issuer == "" || len(issuer) > maxIssuerBytes || !utf8.ValidString(issuer) || hasControlCharacter(issuer) {
		return block.errorf("%s.issuer must have 1 to %d bytes of UTF-8 without a control character, got %q", path, maxIssuerBytes, issuer)
	}
	access.Issuer = issuer
	audience, ok := block.stringAttr("audience")
	if !ok {
		return block.errorf("%s.audience is required: the id of this service", path)
	}
	if !validAccessID(audience) {
		return block.errorf("%s.audience must have 1 to %d bytes of UTF-8 without a control character, got %q", path, maxAccessIDBytes, audience)
	}
	access.Audience = audience

	keysText, defined, err := resolveValueSource(block, path, "public_keys", baseDir, formValue|formEnv|formFile)
	if err != nil {
		return err
	}
	if !defined {
		return block.errorf("%s.public_keys is required: give public_keys, public_keys_env or public_keys_file (a JWK Set)", path)
	}
	if access.Keys, err = parseIdentityPublicKeys(keysText); err != nil {
		return block.errorf("%s.public_keys: %v", path, err)
	}

	if access.MaxAge, err = decodeDuration(block, cfg, path, "max_age", defaultIdentityMaxAgeText, time.Second, 24*time.Hour); err != nil {
		return err
	}
	if access.ClockSkew, err = decodeDuration(block, cfg, path, "clock_skew", defaultIdentityClockSkewText, 0, 5*time.Minute); err != nil {
		return err
	}
	if access.ClockSkew%time.Second != 0 {
		return block.errorf("%s.clock_skew must be a whole number of seconds, got %q", path, cfg.DurationTexts[path+".clock_skew"])
	}
	return nil
}

// decodePDPBlock reads the access.pdp block: the connection to the policy decision point.
func decodePDPBlock(block hclBlock, cfg *appConfig, access *accessConfig, baseDir string) error {
	const path = "access.pdp"
	if len(block.Blocks) != 0 {
		return block.errorf("nested blocks are not supported inside %s", path)
	}
	if err := rejectClearSecrets(block, path, path); err != nil {
		return err
	}
	if err := rejectUnknownAttrs(block, attributeNames(path)...); err != nil {
		return err
	}
	kinds := make(map[string]hclValueKind, len(block.Attrs))
	for name := range block.Attrs {
		kinds[name] = hclString
	}
	kinds["plain_http"] = hclBool
	kinds["max_evaluations"] = hclNumber
	if err := requireAttrKinds(block, kinds); err != nil {
		return err
	}
	recordSources(cfg, path, path, block, baseDir)

	rawURL, defined, err := resolveValueSource(block, path, "url", baseDir, formValue|formEnv|formFile)
	if err != nil {
		return err
	}
	if !defined {
		return block.errorf("%s.url is required: give url, url_env or url_file", path)
	}
	if err := validatePDPURL(rawURL); err != nil {
		// A URL with credentials or a query can hold a secret: the error does not show such a value.
		if parsed, parseErr := url.Parse(rawURL); parseErr == nil && (parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery) {
			return block.errorf("%s.url: %v", path, err)
		}
		return block.errorf("%s.url: %v, got %q", path, err, rawURL)
	}
	access.PDPURL = rawURL
	if value, ok := block.boolAttr("plain_http"); ok {
		access.PDPPlainHTTP = value
	}
	plain := strings.HasPrefix(rawURL, "http://")
	if plain && !access.PDPPlainHTTP {
		return block.errorf("%s.url is an http URL, but %s.plain_http = false; use an https URL, or set plain_http = true on a trusted network", path, path)
	}
	if !plain && access.PDPPlainHTTP {
		return block.errorf("%s.url is an https URL, but %s.plain_http = true; remove plain_http, or use an http URL", path, path)
	}

	token, defined, err := resolveValueSource(block, path, "token", baseDir, formEnv|formFile)
	if err != nil {
		return err
	}
	if !defined {
		return block.errorf("%s.token is required: give token_env or token_file", path)
	}
	if len(token) > maxPDPTokenBytes || hasControlCharacter(token) || strings.ContainsFunc(token, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		// The error never shows the token.
		return block.errorf("%s.token must have 1 to %d bytes without a space or a control character", path, maxPDPTokenBytes)
	}
	access.PDPToken = token

	if access.PDPTimeout, err = decodeDuration(block, cfg, path, "timeout", defaultPDPTimeoutText, 10*time.Millisecond, time.Minute); err != nil {
		return err
	}
	if access.PDPUsageTimeout, err = decodeDuration(block, cfg, path, "usage_timeout", defaultPDPUsageTimeoutText, 100*time.Millisecond, time.Minute); err != nil {
		return err
	}
	if access.PDPUsageTimeout < access.PDPTimeout {
		return block.errorf("%s.usage_timeout = %q is less than %s.timeout = %q; usage_timeout must not be less than timeout",
			path, cfg.DurationTexts[path+".usage_timeout"], path, cfg.DurationTexts[path+".timeout"])
	}
	if access.PDPCacheTTL, err = decodeDuration(block, cfg, path, "cache_ttl", defaultPDPCacheTTLText, 0, time.Hour); err != nil {
		return err
	}
	access.PDPMaxEvaluations = defaultPDPMaxEvaluations
	if value, ok := block.intAttr("max_evaluations"); ok {
		if value < 1 || value > maxPDPEvaluations {
			return block.errorf("%s.max_evaluations must be between 1 and %d, got %d", path, maxPDPEvaluations, value)
		}
		access.PDPMaxEvaluations = int(value)
	}
	return nil
}

// validateAccessResources checks the keys of the access block against the bucket blocks and the volume blocks. In
// mode authzen, each label is the id of a node, and the action document lists one resource for each label.
func validateAccessResources(cfg appConfig) error {
	if !cfg.Access.enforced() {
		return nil
	}
	labels := make([]string, 0, len(cfg.Buckets)+len(cfg.Volumes))
	for _, bucket := range cfg.Buckets {
		labels = append(labels, bucket.ID)
	}
	for _, volume := range cfg.Volumes {
		labels = append(labels, volume.ID)
	}
	for _, label := range labels {
		if len(label) > maxAccessIDBytes {
			shown := label[:32] + "..."
			return fmt.Errorf("access.mode = \"authzen\" contradicts the bucket or volume label %q (%d bytes): a label is the id of a node and has at most %d bytes", shown, len(label), maxAccessIDBytes)
		}
	}
	if len(labels) > maxAccessResources {
		return fmt.Errorf("access.mode = \"authzen\" contradicts the number of bucket and volume blocks (%d): the action document lists one resource for each, and the most is %d", len(labels), maxAccessResources)
	}
	return nil
}

// configWarnings gives the warnings that the server logs at start. A warning is not an error.
func configWarnings(cfg appConfig) []string {
	var warnings []string
	if !cfg.Access.enforced() {
		warnings = append(warnings, "access.mode = \"open\": the server checks no identity token and asks no PDP")
	}
	if cfg.Access.PDPPlainHTTP {
		warnings = append(warnings, "access.pdp.plain_http = true: the server sends the PDP token and the questions without TLS")
	}
	if cfg.Access.enforced() {
		host, _, err := net.SplitHostPort(cfg.Listen)
		if err == nil && (host == "" || host == "0.0.0.0" || host == "::") {
			warnings = append(warnings, fmt.Sprintf("server.listen = %q accepts requests on each network interface: the service must accept requests only through the edge, so limit the network", cfg.Listen))
		}
	}
	return warnings
}

// validateIdentityHeader checks the name of the header of the identity token.
func validateIdentityHeader(value string) error {
	if value == "" || len(value) > maxIdentityHeaderChars {
		return fmt.Errorf("must have 1 to %d characters", maxIdentityHeaderChars)
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			return fmt.Errorf("must have only letters, digits and \"-\"")
		}
	}
	switch http.CanonicalHeaderKey(value) {
	case "Cookie", "Authorization", "Host", "Content-Type", "Content-Length":
		return fmt.Errorf("must not be Cookie, Authorization, Host, Content-Type or Content-Length")
	}
	return nil
}

// validatePDPURL checks the base URL of the PDP: http or https, a host, no credentials, no query, no fragment, and
// no "/" at the end. The value is not changed.
func validatePDPURL(raw string) error {
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return fmt.Errorf("must start with http:// or https://")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("is not a valid URL")
	}
	if parsed.Hostname() == "" {
		return fmt.Errorf("must have a host")
	}
	if parsed.User != nil {
		return fmt.Errorf("must not have credentials")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery {
		return fmt.Errorf("must not have a query")
	}
	if parsed.Fragment != "" || strings.Contains(raw, "#") {
		return fmt.Errorf("must not have a fragment")
	}
	if strings.HasSuffix(raw, "/") {
		return fmt.Errorf("must not end with \"/\"")
	}
	return nil
}

// durationPattern is the strict form of a duration: one whole number without sign and without fraction, and one
// unit.
var durationPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)(ms|s|m|h)$`)

// parseDurationText reads a duration in the strict form, for example "500ms", "30s", "5m" or "1h".
func parseDurationText(text string) (time.Duration, error) {
	match := durationPattern.FindStringSubmatch(text)
	if match == nil {
		return 0, fmt.Errorf("is not a duration: use one whole number and one unit (ms, s, m or h), for example \"500ms\" or \"5m\"")
	}
	number, err := strconv.ParseUint(match[1], 10, 63)
	if err != nil {
		return 0, fmt.Errorf("is too large")
	}
	unit := map[string]time.Duration{"ms": time.Millisecond, "s": time.Second, "m": time.Minute, "h": time.Hour}[match[2]]
	if number > uint64((1<<63-1)/int64(unit)) {
		return 0, fmt.Errorf("is too large")
	}
	return time.Duration(number) * unit, nil
}

// decodeDuration reads a duration key of a block. The text that applies (the text of the file, or the default
// text) goes to cfg.DurationTexts for print-config.
func decodeDuration(block hclBlock, cfg *appConfig, path, name, defaultText string, minimum, maximum time.Duration) (time.Duration, error) {
	text := defaultText
	if value, ok := block.stringAttr(name); ok {
		text = value
	}
	if cfg.DurationTexts == nil {
		cfg.DurationTexts = make(map[string]string)
	}
	cfg.DurationTexts[path+"."+name] = text
	value, err := parseDurationText(text)
	if err != nil {
		return 0, block.errorf("%s.%s %v, got %q", path, name, err, text)
	}
	if value < minimum || value > maximum {
		return 0, block.errorf("%s.%s must be from %s to %s, got %q", path, name, durationText(minimum), durationText(maximum), text)
	}
	return value, nil
}

// durationText gives the shortest strict text of a duration, in the largest unit that divides it without a rest.
func durationText(value time.Duration) string {
	switch {
	case value == 0:
		return "0s"
	case value%time.Hour == 0:
		return strconv.FormatInt(int64(value/time.Hour), 10) + "h"
	case value%time.Minute == 0:
		return strconv.FormatInt(int64(value/time.Minute), 10) + "m"
	case value%time.Second == 0:
		return strconv.FormatInt(int64(value/time.Second), 10) + "s"
	}
	return strconv.FormatInt(int64(value/time.Millisecond), 10) + "ms"
}
