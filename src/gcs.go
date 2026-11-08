package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const gcsOAuthScope = "https://www.googleapis.com/auth/devstorage.full_control"

type gcsServiceAccount struct {
	Type         string `json:"type"`
	ProjectID    string `json:"project_id"`
	PrivateKeyID string `json:"private_key_id"`
	PrivateKey   string `json:"private_key"`
	ClientEmail  string `json:"client_email"`
	TokenURI     string `json:"token_uri"`
}

type gcsTokenSource struct {
	info   gcsServiceAccount
	key    *rsa.PrivateKey
	client *http.Client
	mu     sync.Mutex
	token  string
	expiry time.Time
}

type gcsBackend struct {
	cfg      bucketConfig
	endpoint *url.URL
	client   *http.Client
	tokens   *gcsTokenSource
}

func newGCSBackendWithAuthentication(cfg bucketConfig, auth *sharedAuthentication) (*gcsBackend, error) {
	if auth == nil || auth.cfg.Provider != "gcs" {
		return nil, fmt.Errorf("bucket %q requires a GCS authentication", cfg.ID)
	}
	return &gcsBackend{
		cfg:      cfg,
		endpoint: auth.endpoint,
		client:   auth.client,
		tokens:   auth.gcsToken,
	}, nil
}

const maxGCSCredentialsBytes = 4 << 20

func loadGCSTokenSource(filename string, client *http.Client) (*gcsTokenSource, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("open gcs credentials file %q: %w", filename, err)
	}
	defer file.Close()
	fileInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect gcs credentials file %q: %w", filename, err)
	}
	if fileInfo.Size() > maxGCSCredentialsBytes {
		return nil, fmt.Errorf("gcs credentials file %q exceeds the %d-byte limit", filename, maxGCSCredentialsBytes)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxGCSCredentialsBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read gcs credentials file %q: %w", filename, err)
	}
	if len(data) > maxGCSCredentialsBytes {
		return nil, fmt.Errorf("gcs credentials file %q exceeds the %d-byte limit", filename, maxGCSCredentialsBytes)
	}
	var info gcsServiceAccount
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("decode gcs credentials file %q: %w", filename, err)
	}
	if info.Type != "service_account" {
		return nil, fmt.Errorf("gcs credentials file %q is not a service_account key", filename)
	}
	if info.ClientEmail == "" || info.PrivateKey == "" || info.TokenURI == "" {
		return nil, fmt.Errorf("gcs credentials file %q is missing client_email, private_key or token_uri", filename)
	}
	block, _ := pem.Decode([]byte(info.PrivateKey))
	if block == nil {
		return nil, fmt.Errorf("gcs credentials file %q contains an invalid PEM private key", filename)
	}
	var privateKey *rsa.PrivateKey
	if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		var ok bool
		privateKey, ok = parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("gcs private key is not RSA")
		}
	} else if parsed, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		privateKey = parsed
	} else {
		return nil, fmt.Errorf("parse gcs RSA private key: unsupported key encoding")
	}
	return &gcsTokenSource{info: info, key: privateKey, client: client}, nil
}

func (t *gcsTokenSource) Token(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token != "" && time.Until(t.expiry) > time.Minute {
		return t.token, nil
	}

	now := time.Now().UTC()
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	if t.info.PrivateKeyID != "" {
		header["kid"] = t.info.PrivateKeyID
	}
	claims := map[string]any{
		"iss":   t.info.ClientEmail,
		"scope": gcsOAuthScope,
		"aud":   t.info.TokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}
	encodedHeader, err := encodeJWTPart(header)
	if err != nil {
		return "", err
	}
	encodedClaims, err := encodeJWTPart(claims)
	if err != nil {
		return "", err
	}
	unsigned := encodedHeader + "." + encodedClaims
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, t.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign gcs service account JWT: %w", err)
	}
	assertion := unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)

	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer")
	form.Set("assertion", assertion)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.info.TokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("create gcs token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := t.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("gcs token request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read gcs token response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("gcs token endpoint returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var tokenResponse struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &tokenResponse); err != nil {
		return "", fmt.Errorf("decode gcs token response: %w", err)
	}
	if tokenResponse.AccessToken == "" {
		return "", fmt.Errorf("gcs token response did not contain access_token")
	}
	if tokenResponse.ExpiresIn <= 0 {
		tokenResponse.ExpiresIn = 3600
	}
	t.token = tokenResponse.AccessToken
	t.expiry = now.Add(time.Duration(tokenResponse.ExpiresIn) * time.Second)
	return t.token, nil
}

func encodeJWTPart(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode JWT: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func (g *gcsBackend) List(ctx context.Context, options listOptions) (listPage, error) {
	query := url.Values{}
	if options.Prefix != "" {
		query.Set("prefix", options.Prefix)
	}
	if options.Delimiter != "" {
		query.Set("delimiter", options.Delimiter)
		query.Set("includeTrailingDelimiter", "false")
	}
	if options.MaxResults > 0 {
		query.Set("maxResults", strconv.Itoa(options.MaxResults))
	}
	if options.PageToken != "" {
		query.Set("pageToken", options.PageToken)
	}
	if options.StartAfter != "" {
		query.Set("startOffset", options.StartAfter)
	}
	query.Set("fields", "items(name,size,updated,etag,contentType),prefixes,nextPageToken")

	u := g.apiURL("storage/v1/b/"+pathSegment(g.cfg.Bucket)+"/o", query)
	resp, err := g.do(ctx, http.MethodGet, u, nil, 0, nil)
	if err != nil {
		return listPage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return listPage{}, gcsResponseError(resp)
	}
	var result struct {
		Prefixes      []string `json:"prefixes"`
		NextPageToken string   `json:"nextPageToken"`
		Items         []struct {
			Name        string `json:"name"`
			Size        string `json:"size"`
			Updated     string `json:"updated"`
			ETag        string `json:"etag"`
			ContentType string `json:"contentType"`
		} `json:"items"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&result); err != nil {
		return listPage{}, fmt.Errorf("decode gcs list response: %w", err)
	}
	page := listPage{Prefixes: result.Prefixes, NextPageToken: result.NextPageToken}
	for _, object := range result.Items {
		size, _ := strconv.ParseInt(object.Size, 10, 64)
		updated, _ := time.Parse(time.RFC3339Nano, object.Updated)
		page.Objects = append(page.Objects, objectInfo{
			Key:          object.Name,
			Size:         size,
			LastModified: updated,
			ETag:         object.ETag,
			ContentType:  object.ContentType,
		})
	}
	return page, nil
}

func (g *gcsBackend) Head(ctx context.Context, key string) (objectResponse, error) {
	return g.headWithGeneration(ctx, key, "")
}

func (g *gcsBackend) HeadObjectVersion(ctx context.Context, key, generation string) (objectResponse, error) {
	return g.headWithGeneration(ctx, key, generation)
}

func (g *gcsBackend) headWithGeneration(ctx context.Context, key, generation string) (objectResponse, error) {
	query := url.Values{}
	query.Set("fields", "name,size,updated,timeCreated,timeDeleted,etag,contentType,cacheControl,contentDisposition,contentEncoding,generation,metageneration,md5Hash,crc32c,metadata")
	if generation != "" {
		query.Set("generation", generation)
	}
	u := g.objectMetadataURL(key, query)
	resp, err := g.do(ctx, http.MethodGet, u, nil, 0, nil)
	if err != nil {
		return objectResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return objectResponse{}, gcsResponseError(resp)
	}
	var metadata struct {
		Name               string            `json:"name"`
		Size               string            `json:"size"`
		Updated            string            `json:"updated"`
		ETag               string            `json:"etag"`
		ContentType        string            `json:"contentType"`
		CacheControl       string            `json:"cacheControl"`
		ContentDisposition string            `json:"contentDisposition"`
		ContentEncoding    string            `json:"contentEncoding"`
		Generation         string            `json:"generation"`
		Metageneration     string            `json:"metageneration"`
		MD5Hash            string            `json:"md5Hash"`
		CRC32C             string            `json:"crc32c"`
		Metadata           map[string]string `json:"metadata"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&metadata); err != nil {
		return objectResponse{}, fmt.Errorf("decode gcs object metadata: %w", err)
	}
	headers := make(http.Header)
	headers.Set("Content-Length", metadata.Size)
	if metadata.ContentType != "" {
		headers.Set("Content-Type", metadata.ContentType)
	}
	if metadata.ETag != "" {
		headers.Set("ETag", metadata.ETag)
	}
	if updated, err := time.Parse(time.RFC3339Nano, metadata.Updated); err == nil {
		headers.Set("Last-Modified", updated.UTC().Format(http.TimeFormat))
	}
	if metadata.CacheControl != "" {
		headers.Set("Cache-Control", metadata.CacheControl)
	}
	if metadata.ContentDisposition != "" {
		headers.Set("Content-Disposition", metadata.ContentDisposition)
	}
	if metadata.ContentEncoding != "" {
		headers.Set("Content-Encoding", metadata.ContentEncoding)
	}
	if metadata.Generation != "" {
		headers.Set("x-goog-generation", metadata.Generation)
	}
	if metadata.Metageneration != "" {
		headers.Set("x-goog-metageneration", metadata.Metageneration)
	}
	if metadata.MD5Hash != "" || metadata.CRC32C != "" {
		values := make([]string, 0, 2)
		if metadata.CRC32C != "" {
			values = append(values, "crc32c="+metadata.CRC32C)
		}
		if metadata.MD5Hash != "" {
			values = append(values, "md5="+metadata.MD5Hash)
		}
		headers.Set("x-goog-hash", strings.Join(values, ","))
	}
	for name, value := range metadata.Metadata {
		headers.Set("x-goog-meta-"+name, value)
	}
	return objectResponse{StatusCode: http.StatusOK, Header: headers}, nil
}

func (g *gcsBackend) Get(ctx context.Context, key string, requestHeaders http.Header) (objectResponse, error) {
	return g.getWithGeneration(ctx, key, "", requestHeaders)
}

func (g *gcsBackend) GetObjectVersion(ctx context.Context, key, generation string, requestHeaders http.Header) (objectResponse, error) {
	return g.getWithGeneration(ctx, key, generation, requestHeaders)
}

func (g *gcsBackend) getWithGeneration(ctx context.Context, key, generation string, requestHeaders http.Header) (objectResponse, error) {
	query := url.Values{}
	query.Set("alt", "media")
	if generation != "" {
		query.Set("generation", generation)
	}
	u := g.objectMetadataURL(key, query)
	headers := make(http.Header)
	for _, name := range []string{"Range", "If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since", "If-Range"} {
		if value := requestHeaders.Get(name); value != "" {
			headers.Set(name, value)
		}
	}
	// With a ceiling object.read, the answer must tell the real size of the object: no transparent decompression.
	if headers.Get("Range") != "" || ceilingApplies(ctx, actionObjectRead) {
		headers.Set("Accept-Encoding", "identity")
	}
	resp, err := g.do(ctx, http.MethodGet, u, nil, 0, headers)
	if err != nil {
		return objectResponse{}, err
	}
	if !isSuccessfulObjectReadStatus(resp.StatusCode) {
		defer resp.Body.Close()
		return objectResponse{}, gcsResponseError(resp)
	}
	return objectResponse{StatusCode: resp.StatusCode, Header: resp.Header.Clone(), Body: resp.Body}, nil
}

func (g *gcsBackend) ListObjectVersions(ctx context.Context, key, pageToken string, maximum int) (objectVersionPage, error) {
	query := url.Values{}
	query.Set("prefix", key)
	query.Set("versions", "true")
	if maximum <= 0 || maximum > 1000 {
		maximum = 1000
	}
	query.Set("maxResults", strconv.Itoa(maximum))
	if pageToken != "" {
		query.Set("pageToken", pageToken)
	}
	query.Set("fields", "items(name,size,updated,timeCreated,timeDeleted,etag,contentType,generation,metageneration,md5Hash,crc32c),nextPageToken")
	u := g.apiURL("storage/v1/b/"+pathSegment(g.cfg.Bucket)+"/o", query)
	resp, err := g.do(ctx, http.MethodGet, u, nil, 0, nil)
	if err != nil {
		return objectVersionPage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return objectVersionPage{}, gcsResponseError(resp)
	}
	var result struct {
		NextPageToken string `json:"nextPageToken"`
		Items         []struct {
			Name           string `json:"name"`
			Size           string `json:"size"`
			Updated        string `json:"updated"`
			TimeCreated    string `json:"timeCreated"`
			TimeDeleted    string `json:"timeDeleted"`
			ETag           string `json:"etag"`
			ContentType    string `json:"contentType"`
			Generation     string `json:"generation"`
			Metageneration string `json:"metageneration"`
			MD5Hash        string `json:"md5Hash"`
			CRC32C         string `json:"crc32c"`
		} `json:"items"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&result); err != nil {
		return objectVersionPage{}, fmt.Errorf("decode gcs version response: %w", err)
	}
	page := objectVersionPage{}
	lastExactKey := false
	for _, item := range result.Items {
		lastExactKey = item.Name == key
		if item.Name != key {
			continue
		}
		size, _ := strconv.ParseInt(item.Size, 10, 64)
		modified, _ := time.Parse(time.RFC3339Nano, item.Updated)
		checksums := make(map[string]string)
		if item.MD5Hash != "" {
			checksums["md5"] = item.MD5Hash
		}
		if item.CRC32C != "" {
			checksums["crc32c"] = item.CRC32C
		}
		if len(checksums) == 0 {
			checksums = nil
		}
		page.Versions = append(page.Versions, storedObjectVersion{
			Version: item.Generation, IsCurrent: item.TimeDeleted == "", Size: size,
			LastModified: modified, ETag: item.ETag, ContentType: item.ContentType, Checksums: checksums,
		})
	}
	sortedVersions(page.Versions)
	// Exactly one generation can be current. Some compatible APIs omit or
	// inconsistently populate timeDeleted for archived generations, so keep
	// only the newest live candidate on the first page and never mark a
	// continuation-page generation as current.
	currentKept := false
	for index := range page.Versions {
		if pageToken == "" && page.Versions[index].IsCurrent && !currentKept {
			currentKept = true
			continue
		}
		page.Versions[index].IsCurrent = false
	}
	// GCS prefix listing may continue with another object. Do not carry a
	// cursor into a neighboring key when the caller requested one exact key.
	if result.NextPageToken != "" && lastExactKey {
		page.NextPageToken = result.NextPageToken
	}
	return page, nil
}

func (g *gcsBackend) Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) error {
	return g.put(ctx, key, body, size, contentType, false)
}

func (g *gcsBackend) PutIfAbsent(ctx context.Context, key string, body io.Reader, size int64, contentType string) error {
	return g.put(ctx, key, body, size, contentType, true)
}

func (g *gcsBackend) put(ctx context.Context, key string, body io.Reader, size int64, contentType string, ifAbsent bool) error {
	query := url.Values{}
	query.Set("uploadType", "media")
	query.Set("name", key)
	if ifAbsent {
		query.Set("ifGenerationMatch", "0")
	}
	u := g.apiURL("upload/storage/v1/b/"+pathSegment(g.cfg.Bucket)+"/o", query)
	headers := make(http.Header)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	headers.Set("Content-Type", contentType)
	resp, err := g.do(ctx, http.MethodPost, u, body, size, headers)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if ifAbsent && resp.StatusCode == http.StatusPreconditionFailed {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return apiError{Status: http.StatusConflict, Code: "object_exists", Message: "an object already exists at the extraction destination"}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return gcsResponseError(resp)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return nil
}

func (g *gcsBackend) InitiateResumable(ctx context.Context, key string, size int64, contentType string) (string, error) {
	query := url.Values{}
	query.Set("uploadType", "resumable")
	query.Set("name", key)
	u := g.apiURL("upload/storage/v1/b/"+pathSegment(g.cfg.Bucket)+"/o", query)
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json; charset=utf-8")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	headers.Set("X-Upload-Content-Type", contentType)
	if size >= 0 {
		headers.Set("X-Upload-Content-Length", strconv.FormatInt(size, 10))
	}
	resp, err := g.do(ctx, http.MethodPost, u, strings.NewReader("{}"), 2, headers)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", gcsResponseError(resp)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	location := strings.TrimSpace(resp.Header.Get("Location"))
	if location == "" {
		return "", fmt.Errorf("gcs resumable upload initiation did not return a session URL")
	}
	resolved, err := g.validateResumableURL(location)
	if err != nil {
		return "", err
	}
	return resolved.String(), nil
}

func (g *gcsBackend) UploadResumableChunk(ctx context.Context, sessionURL string, body io.Reader, start, size, total int64, contentType string) (int64, bool, error) {
	if size <= 0 || start < 0 || total <= 0 || start+size > total {
		return start, false, fmt.Errorf("invalid gcs resumable upload range")
	}
	u, err := g.validateResumableURL(sessionURL)
	if err != nil {
		return start, false, err
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	headers := make(http.Header)
	headers.Set("Content-Type", contentType)
	headers.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, start+size-1, total))
	resp, err := g.doSession(ctx, http.MethodPut, u, body, size, headers)
	if err != nil {
		return start, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusPermanentRedirect { // 308 Resume Incomplete
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return resumableNextOffset(resp.Header.Get("Range")), false, nil
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return total, true, nil
	}
	return start, false, gcsResponseError(resp)
}

func (g *gcsBackend) QueryResumable(ctx context.Context, sessionURL string, total int64) (int64, bool, error) {
	u, err := g.validateResumableURL(sessionURL)
	if err != nil {
		return 0, false, err
	}
	headers := make(http.Header)
	headers.Set("Content-Length", "0")
	headers.Set("Content-Range", fmt.Sprintf("bytes */%d", total))
	resp, err := g.doSession(ctx, http.MethodPut, u, nil, 0, headers)
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusPermanentRedirect {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return resumableNextOffset(resp.Header.Get("Range")), false, nil
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return total, true, nil
	}
	return 0, false, gcsResponseError(resp)
}

func (g *gcsBackend) AbortResumable(ctx context.Context, sessionURL string) error {
	u, err := g.validateResumableURL(sessionURL)
	if err != nil {
		return err
	}
	resp, err := g.doSession(ctx, http.MethodDelete, u, nil, 0, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if (resp.StatusCode >= 200 && resp.StatusCode < 300) || resp.StatusCode == http.StatusNotFound || resp.StatusCode == 499 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	return gcsResponseError(resp)
}

func resumableNextOffset(value string) int64 {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	parts := strings.SplitN(strings.TrimPrefix(value, "bytes="), "-", 2)
	if len(parts) != 2 {
		return 0
	}
	last, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if err != nil || last < 0 {
		return 0
	}
	return last + 1
}

func (g *gcsBackend) validateResumableURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("parse gcs resumable session URL: %w", err)
	}
	if !u.IsAbs() {
		u = g.endpoint.ResolveReference(u)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("gcs resumable session URL has an unsupported scheme")
	}
	if u.Host == "" || u.User != nil || u.Fragment != "" {
		return nil, fmt.Errorf("gcs resumable session URL is invalid")
	}
	sameOrigin := strings.EqualFold(u.Scheme, g.endpoint.Scheme) && strings.EqualFold(u.Host, g.endpoint.Host)
	officialEndpoint := strings.EqualFold(g.endpoint.Scheme, "https") &&
		(strings.EqualFold(g.endpoint.Hostname(), "storage.googleapis.com") || strings.HasSuffix(strings.ToLower(g.endpoint.Hostname()), ".googleapis.com"))
	officialSession := strings.EqualFold(u.Scheme, "https") &&
		(strings.EqualFold(u.Hostname(), "storage.googleapis.com") || strings.HasSuffix(strings.ToLower(u.Hostname()), ".googleapis.com"))
	if !sameOrigin && !(officialEndpoint && officialSession) {
		return nil, fmt.Errorf("gcs resumable session URL points to an unexpected host")
	}
	return u, nil
}

func (g *gcsBackend) doSession(ctx context.Context, method string, u *url.URL, body io.Reader, size int64, headers http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, fmt.Errorf("create gcs resumable request: %w", err)
	}
	if headers != nil {
		req.Header = headers.Clone()
	}
	if body != nil || size == 0 {
		req.ContentLength = size
	}
	if g.tokens != nil {
		token, err := g.tokens.Token(ctx)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gcs resumable request failed: %w", err)
	}
	return resp, nil
}

func (g *gcsBackend) Delete(ctx context.Context, key string) error {
	resp, err := g.do(ctx, http.MethodDelete, g.objectMetadataURL(key, nil), nil, 0, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return gcsResponseError(resp)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return nil
}

func (g *gcsBackend) DeleteObjectVersion(ctx context.Context, key, generation string) error {
	query := url.Values{}
	query.Set("generation", generation)
	resp, err := g.do(ctx, http.MethodDelete, g.objectMetadataURL(key, query), nil, 0, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return gcsResponseError(resp)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return nil
}

func (g *gcsBackend) RestoreObjectVersion(ctx context.Context, key, generation string) error {
	return g.rewriteObject(ctx, key, key, generation)
}

func (g *gcsBackend) Copy(ctx context.Context, sourceKey, destinationKey string) error {
	return g.rewriteObject(ctx, sourceKey, destinationKey, "")
}

func (g *gcsBackend) rewriteObject(ctx context.Context, sourceKey, destinationKey, sourceGeneration string) error {
	objectPath := "storage/v1/b/" + pathSegment(g.cfg.Bucket) + "/o/" + pathSegment(sourceKey) +
		"/rewriteTo/b/" + pathSegment(g.cfg.Bucket) + "/o/" + pathSegment(destinationKey)
	var rewriteToken string
	for attempts := 0; attempts < 10000; attempts++ {
		query := url.Values{}
		if sourceGeneration != "" {
			query.Set("sourceGeneration", sourceGeneration)
		}
		if rewriteToken != "" {
			query.Set("rewriteToken", rewriteToken)
		}
		u := g.apiURL(objectPath, query)
		headers := make(http.Header)
		headers.Set("Content-Type", "application/json")
		resp, err := g.do(ctx, http.MethodPost, u, strings.NewReader("{}"), 2, headers)
		if err != nil {
			return err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			err := gcsResponseError(resp)
			resp.Body.Close()
			return err
		}
		var result struct {
			Done         bool   `json:"done"`
			RewriteToken string `json:"rewriteToken"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result)
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("decode gcs rewrite response: %w", err)
		}
		if result.Done {
			return nil
		}
		if result.RewriteToken == "" || result.RewriteToken == rewriteToken {
			return fmt.Errorf("gcs rewrite did not complete and returned no new rewrite token")
		}
		rewriteToken = result.RewriteToken
	}
	return fmt.Errorf("gcs rewrite exceeded maximum number of iterations")
}

func (g *gcsBackend) DiscoverCapabilities(ctx context.Context) (discoveredCapabilities, error) {
	requested := []string{
		"storage.objects.list",
		"storage.objects.get",
		"storage.objects.create",
		"storage.objects.delete",
	}
	query := url.Values{}
	for _, permission := range requested {
		query.Add("permissions", permission)
	}
	u := g.apiURL("storage/v1/b/"+pathSegment(g.cfg.Bucket)+"/iam/testPermissions", query)
	resp, err := g.do(ctx, http.MethodGet, u, nil, 0, nil)
	if err != nil {
		return discoveredCapabilities{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return discoveredCapabilities{}, gcsResponseError(resp)
	}
	var result struct {
		Permissions []string `json:"permissions"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return discoveredCapabilities{}, fmt.Errorf("decode gcs permission response: %w", err)
	}
	has := make(map[string]bool, len(result.Permissions))
	for _, permission := range result.Permissions {
		has[permission] = true
	}
	sort.Strings(result.Permissions)
	states := map[string]capabilityState{
		permissionRead: {
			Allowed:  has["storage.objects.list"] && has["storage.objects.get"],
			Verified: true,
			Source:   "gcs-test-iam-permissions",
			Reason:   "requires storage.objects.list and storage.objects.get",
		},
		permissionWrite: {
			Allowed:  has["storage.objects.create"],
			Verified: true,
			Source:   "gcs-test-iam-permissions",
			Reason:   "requires storage.objects.create",
		},
		permissionDelete: {
			Allowed:  has["storage.objects.delete"],
			Verified: true,
			Source:   "gcs-test-iam-permissions",
			Reason:   "requires storage.objects.delete",
		},
	}
	return discoveredCapabilities{States: states, Permissions: result.Permissions}, nil
}

func (g *gcsBackend) do(
	ctx context.Context,
	method string,
	u *url.URL,
	body io.Reader,
	size int64,
	headers http.Header,
) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, fmt.Errorf("create gcs request: %w", err)
	}
	if headers != nil {
		req.Header = headers.Clone()
	}
	if body != nil {
		req.ContentLength = size
	}
	if g.tokens != nil {
		token, err := g.tokens.Token(ctx)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gcs request failed: %w", err)
	}
	return resp, nil
}

func (g *gcsBackend) objectMetadataURL(key string, query url.Values) *url.URL {
	return g.apiURL("storage/v1/b/"+pathSegment(g.cfg.Bucket)+"/o/"+pathSegment(key), query)
}

func (g *gcsBackend) apiURL(relativeRawPath string, query url.Values) *url.URL {
	u := *g.endpoint
	decodedBase := strings.TrimRight(u.Path, "/")
	escapedBase := strings.TrimRight(u.EscapedPath(), "/")
	rawPath := strings.TrimLeft(relativeRawPath, "/")
	decodedRelative, err := url.PathUnescape(rawPath)
	if err != nil {
		decodedRelative = rawPath
	}
	u.Path = decodedBase + "/" + decodedRelative
	u.RawPath = escapedBase + "/" + rawPath
	if len(query) > 0 {
		u.RawQuery = query.Encode()
	} else {
		u.RawQuery = ""
	}
	return &u
}

func pathSegment(value string) string {
	return awsURIEncode(value, true)
}

func gcsResponseError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	var parsed struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &parsed)
	message := strings.TrimSpace(parsed.Error.Message)
	if message == "" {
		message = strings.TrimSpace(string(body))
	}
	if message == "" {
		message = resp.Status
	}
	return &upstreamError{StatusCode: resp.StatusCode, Code: parsed.Error.Status, Message: message}
}
