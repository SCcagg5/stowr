package main

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The check of the identity token (ProcurAuth 9). The token is a JWT that the edge signs with Ed25519 or ES256.
// The functions here check the token only. The HTTP side (the header, the 401 answer) is in another file.

const (
	// maxIdentityTokenBytes is the longest token that the verifier reads.
	maxIdentityTokenBytes = 16384
	// identityCacheMaxEntries is the most results of valid tokens that the verifier keeps.
	identityCacheMaxEntries = 4096
	// maxIdentityTime is the largest value of iat and exp that the verifier accepts.
	maxIdentityTime = 9000000000000000
	// maxIdentityJSONDepth is the deepest nesting that the strict JSON reader accepts.
	maxIdentityJSONDepth = 32
)

// identityResult is the result of the check of one token. Reason is empty for a valid token. Else it is one of:
// missing, malformed, algorithm, type, kid, signature, issuer, audience, expired, not_yet_valid, max_age,
// subject, claims.
type identityResult struct {
	Subject string
	// Owner is the claim "owner" when it is a string. The server writes it in the log only.
	Owner  string
	Reason string
}

// ok tells if the token is valid.
func (r identityResult) ok() bool { return r.Reason == "" }

// cachedIdentity is the kept result of a valid token.
type cachedIdentity struct {
	subject string
	owner   string
	exp     int64
}

// identityVerifier checks identity tokens. It keeps the result of each valid token until the token expires.
// It is safe for concurrent use.
type identityVerifier struct {
	issuer   string
	audience string
	maxAge   int64 // seconds
	skew     int64 // seconds
	keys     []identityKey

	mu    sync.Mutex
	cache map[[32]byte]cachedIdentity
}

// newIdentityVerifier makes a verifier from the access block.
func newIdentityVerifier(cfg accessConfig) *identityVerifier {
	return &identityVerifier{
		issuer:   cfg.Issuer,
		audience: cfg.Audience,
		maxAge:   int64(cfg.MaxAge / time.Second),
		skew:     int64(cfg.ClockSkew / time.Second),
		keys:     cfg.Keys,
		cache:    make(map[[32]byte]cachedIdentity),
	}
}

// verify checks one token at the time now. It compares whole seconds.
func (v *identityVerifier) verify(token string, now time.Time) identityResult {
	if token == "" {
		return identityResult{Reason: "missing"}
	}
	if len(token) > maxIdentityTokenBytes {
		return identityResult{Reason: "malformed"}
	}
	seconds := now.Unix()
	digest := sha256.Sum256([]byte(token))

	v.mu.Lock()
	if entry, found := v.cache[digest]; found {
		if seconds <= entry.exp+v.skew {
			v.mu.Unlock()
			return identityResult{Subject: entry.subject, Owner: entry.owner}
		}
		delete(v.cache, digest)
		v.mu.Unlock()
		return identityResult{Reason: "expired"}
	}
	v.mu.Unlock()

	result, exp := v.check(token, seconds)
	if !result.ok() {
		return result
	}
	v.mu.Lock()
	if len(v.cache) >= identityCacheMaxEntries {
		for key, entry := range v.cache {
			if seconds > entry.exp+v.skew {
				delete(v.cache, key)
			}
		}
	}
	if len(v.cache) < identityCacheMaxEntries {
		v.cache[digest] = cachedIdentity{subject: result.Subject, owner: result.Owner, exp: exp}
	}
	v.mu.Unlock()
	return result
}

// check does the full check of a token that is not in the cache. It gives the exp of a valid token.
func (v *identityVerifier) check(token string, now int64) (identityResult, int64) {
	refused := func(reason string) (identityResult, int64) { return identityResult{Reason: reason}, 0 }

	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return refused("malformed")
	}
	decode := base64.RawURLEncoding.Strict()
	headerBytes, err := decode.DecodeString(parts[0])
	if err != nil {
		return refused("malformed")
	}
	claimBytes, err := decode.DecodeString(parts[1])
	if err != nil {
		return refused("malformed")
	}
	signature, err := decode.DecodeString(parts[2])
	if err != nil {
		return refused("malformed")
	}

	header, err := parseStrictIdentityObject(headerBytes)
	if err != nil {
		return refused("malformed")
	}
	alg, isString := identityJSONString(header["alg"])
	if !isString || (alg != identityAlgEd25519 && alg != identityAlgES256) {
		return refused("algorithm")
	}
	if _, exists := header["crit"]; exists {
		return refused("malformed")
	}
	if typ, isString := identityJSONString(header["typ"]); !isString || typ != "JWT" {
		return refused("type")
	}
	candidates, reason := v.keysFor(alg, header)
	if reason != "" {
		return refused(reason)
	}

	signingInput := []byte(token[:len(parts[0])+1+len(parts[1])])
	if !identitySignatureValid(alg, candidates, signingInput, signature) {
		return refused("signature")
	}

	claims, err := parseStrictIdentityObject(claimBytes)
	if err != nil {
		return refused("malformed")
	}
	if iss, isString := identityJSONString(claims["iss"]); !isString || iss != v.issuer {
		return refused("issuer")
	}
	if !identityAudienceHolds(claims["aud"], v.audience) {
		return refused("audience")
	}
	subject, isString := identityJSONString(claims["sub"])
	if !isString || !validAccessID(subject) {
		return refused("subject")
	}
	iat, iatOK := identityJSONTime(claims["iat"])
	exp, expOK := identityJSONTime(claims["exp"])
	if !iatOK || !expOK {
		return refused("claims")
	}

	if now > exp+v.skew {
		return refused("expired")
	}
	if iat > now+v.skew {
		return refused("not_yet_valid")
	}
	if exp < iat || exp-iat > v.maxAge {
		return refused("max_age")
	}

	// The owner is for the log. A value that is not a string is ignored.
	owner, _ := identityJSONString(claims["owner"])
	return identityResult{Subject: subject, Owner: owner}, exp
}

// keysFor gives the keys that can check a token with this alg and this header. The reason is not empty when none can.
func (v *identityVerifier) keysFor(alg string, header map[string]json.RawMessage) ([]identityKey, string) {
	if rawKID, exists := header["kid"]; exists {
		kid, isString := identityJSONString(rawKID)
		if !isString {
			return nil, "malformed"
		}
		for _, key := range v.keys {
			if key.KID == kid {
				if key.Alg != alg {
					return nil, "kid"
				}
				return []identityKey{key}, ""
			}
		}
		return nil, "kid"
	}
	var found []identityKey
	for _, key := range v.keys {
		if key.Alg == alg {
			found = append(found, key)
		}
	}
	if len(found) == 0 {
		return nil, "kid"
	}
	return found, ""
}

// identitySignatureValid tells if one of the keys signed the input. The keys all have the alg of the token.
func identitySignatureValid(alg string, keys []identityKey, signingInput, signature []byte) bool {
	if len(signature) != ed25519.SignatureSize {
		// Ed25519 and ES256 both give a signature of exactly 64 bytes.
		return false
	}
	switch alg {
	case identityAlgEd25519:
		for _, key := range keys {
			if len(key.Ed25519) == ed25519.PublicKeySize && ed25519.Verify(key.Ed25519, signingInput, signature) {
				return true
			}
		}
	case identityAlgES256:
		digest := sha256.Sum256(signingInput)
		r := new(big.Int).SetBytes(signature[:32])
		s := new(big.Int).SetBytes(signature[32:])
		for _, key := range keys {
			if key.P256 != nil && ecdsa.Verify(key.P256, digest[:], r, s) {
				return true
			}
		}
	}
	return false
}

// identityJSONString reads a raw JSON value as a string. It gives false for a missing value and for any other type.
func identityJSONString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var out string
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", false
	}
	return out, true
}

// identityJSONTime reads a raw JSON integer from 0 to maxIdentityTime. It accepts digits only: no sign, no
// fraction, no exponent.
func identityJSONTime(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 || len(raw) > 16 {
		return 0, false
	}
	for _, c := range raw {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	value, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || value > maxIdentityTime {
		return 0, false
	}
	return value, true
}

// identityAudienceHolds tells if the claim aud is the audience, or an array of strings that holds it.
func identityAudienceHolds(raw json.RawMessage, audience string) bool {
	if len(raw) == 0 {
		return false
	}
	switch raw[0] {
	case '"':
		value, isString := identityJSONString(raw)
		return isString && value == audience
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return false
		}
		holds := false
		for _, item := range items {
			value, isString := identityJSONString(item)
			if !isString {
				return false
			}
			if value == audience {
				holds = true
			}
		}
		return holds
	}
	return false
}

// parseStrictIdentityObject reads a JSON object. It refuses a duplicate member at any level, data after the
// object and a value that is not an object. The standard decoder keeps the last duplicate member.
func parseStrictIdentityObject(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	first, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := first.(json.Delim); !ok || delim != '{' {
		return nil, io.ErrUnexpectedEOF
	}
	// The first token is read: walk the members of the object.
	if err := walkIdentityMembers(decoder, 1); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, io.ErrUnexpectedEOF
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil {
		return nil, err
	}
	return members, nil
}

// walkIdentityMembers reads the members of an object after its "{" and the closing "}".
func walkIdentityMembers(decoder *json.Decoder, depth int) error {
	if depth > maxIdentityJSONDepth {
		return io.ErrUnexpectedEOF
	}
	seen := make(map[string]struct{})
	for decoder.More() {
		nameToken, err := decoder.Token()
		if err != nil {
			return err
		}
		name, _ := nameToken.(string)
		if _, exists := seen[name]; exists {
			return io.ErrUnexpectedEOF
		}
		seen[name] = struct{}{}
		if err := walkIdentityValue(decoder, depth); err != nil {
			return err
		}
	}
	_, err := decoder.Token()
	return err
}

// walkIdentityValue reads one value, with its nested values.
func walkIdentityValue(decoder *json.Decoder, depth int) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, isDelim := token.(json.Delim)
	if !isDelim {
		return nil
	}
	switch delim {
	case '{':
		return walkIdentityMembers(decoder, depth+1)
	case '[':
		if depth+1 > maxIdentityJSONDepth {
			return io.ErrUnexpectedEOF
		}
		for decoder.More() {
			if err := walkIdentityValue(decoder, depth+1); err != nil {
				return err
			}
		}
		_, err := decoder.Token()
		return err
	}
	return io.ErrUnexpectedEOF
}
