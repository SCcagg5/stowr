package main

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"strings"
)

// The public keys of the issuer of the identity tokens. The configuration gives them as PEM text or as a JWK Set
// (RFC 7517), with the rules of ClickHouse Dash: a text that starts with "{" after white space is a JWK Set, any other
// text is PEM. Both forms hold 1 to 16 keys, and each key is Ed25519 or P-256. The JWK parser also refuses a duplicate
// member and trailing data.

const (
	// maxIdentityKeys is the most keys in the text: PEM blocks or keys of the JWK Set.
	maxIdentityKeys = 16
	// maxPEMKeyBodyBytes is the longest base64 text of one PEM block. An Ed25519 or a P-256 public key is much shorter.
	maxPEMKeyBodyBytes = 4096
	// maxKIDBytes is the longest kid of a JWK.
	maxKIDBytes = 256

	identityAlgEd25519 = "Ed25519"
	identityAlgES256   = "ES256"
)

// identityKey is one public key of the issuer.
type identityKey struct {
	// Alg is the only header alg that this key verifies: "Ed25519" or "ES256".
	Alg string
	KID string
	// Ed25519 is the key of 32 bytes. It is nil for a P-256 key.
	Ed25519 ed25519.PublicKey
	// P256 is the key of a P-256 point. It is nil for an Ed25519 key.
	P256 *ecdsa.PublicKey
}

// display gives the short name of the key for print-config and for the log: the type and the kid.
func (k identityKey) display() string {
	if k.Ed25519 != nil {
		return "Ed25519 kid=" + k.KID
	}
	return "P-256 kid=" + k.KID
}

// jwkPrivateMembers are the members that a private or a secret key has. A public key set cannot have them.
var jwkPrivateMembers = []string{"d", "p", "q", "dp", "dq", "qi", "oth", "k"}

// The DER prefixes of a SubjectPublicKeyInfo: Ed25519 (RFC 8410), then P-256 with an uncompressed point (RFC 5480).
// The prefix of P-256 ends with the byte 0x04 of the uncompressed point.
var (
	ed25519SPKIPrefix = []byte{0x30, 0x2a, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x70, 0x03, 0x21, 0x00}
	p256SPKIPrefix    = []byte{0x30, 0x59, 0x30, 0x13, 0x06, 0x07, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x02, 0x01, 0x06,
		0x08, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07, 0x03, 0x42, 0x00, 0x04}
)

// parseIdentityPublicKeys reads the public keys of the issuer. A text that starts with "{" after spaces, tabs and line
// ends is a JWK Set. Any other text is PEM.
func parseIdentityPublicKeys(text string) ([]identityKey, error) {
	if strings.HasPrefix(strings.TrimLeft(text, " \t\r\n"), "{") {
		return parseIdentityJWKSet(text)
	}
	return parseIdentityPEM(text)
}

// parseIdentityPEM reads 1 to 16 "PUBLIC KEY" PEM blocks. Each block is the SubjectPublicKeyInfo of an Ed25519 key, or of
// a P-256 key with an uncompressed point. The reader cuts the text at each line end and removes the spaces, the tabs
// and the carriage returns at the two ends of a line. Outside a block, a line must be empty or the line of the start of
// a block: other text stops the start. The kid of a PEM key is its RFC 7638 thumbprint.
func parseIdentityPEM(text string) ([]identityKey, error) {
	keys := make([]identityKey, 0, 2)
	inside := false
	var body strings.Builder
	for index, raw := range strings.Split(text, "\n") {
		line := strings.Trim(raw, " \t\r")
		if !inside {
			if line == "" {
				continue
			}
			if line != "-----BEGIN PUBLIC KEY-----" {
				return nil, fmt.Errorf("line %d is not -----BEGIN PUBLIC KEY-----: give PEM public keys or a JWK Set", index+1)
			}
			inside = true
			body.Reset()
			continue
		}
		if line != "-----END PUBLIC KEY-----" {
			body.WriteString(line)
			if body.Len() > maxPEMKeyBodyBytes {
				return nil, fmt.Errorf("key %d is too long for an Ed25519 or a P-256 public key", len(keys)+1)
			}
			continue
		}
		inside = false
		key, err := pemIdentityKey(body.String())
		if err != nil {
			return nil, fmt.Errorf("key %d %v", len(keys)+1, err)
		}
		if len(keys) == maxIdentityKeys {
			return nil, fmt.Errorf("more than %d keys: give 1 to %d keys", maxIdentityKeys, maxIdentityKeys)
		}
		keys = append(keys, key)
	}
	if inside {
		return nil, fmt.Errorf("-----END PUBLIC KEY----- is missing")
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no key: give at least one -----BEGIN PUBLIC KEY----- block, or a JWK Set")
	}
	return keys, nil
}

// pemIdentityKey reads the base64 body of one PEM block.
func pemIdentityKey(body string) (identityKey, error) {
	der, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		return identityKey{}, fmt.Errorf("is not valid base64")
	}
	switch {
	case len(der) == len(ed25519SPKIPrefix)+ed25519.PublicKeySize && bytes.HasPrefix(der, ed25519SPKIPrefix):
		x := append([]byte(nil), der[len(ed25519SPKIPrefix):]...)
		return identityKey{Alg: identityAlgEd25519, KID: ed25519Thumbprint(x), Ed25519: ed25519.PublicKey(x)}, nil
	case len(der) == len(p256SPKIPrefix)+64 && bytes.HasPrefix(der, p256SPKIPrefix):
		point := der[len(p256SPKIPrefix):]
		return p256IdentityKey(point[:32], point[32:])
	default:
		return identityKey{}, fmt.Errorf("is not an Ed25519 public key or a P-256 public key with an uncompressed point (SubjectPublicKeyInfo)")
	}
}

// p256IdentityKey makes a P-256 key from its coordinates of 32 bytes each. The point must be on the curve.
func p256IdentityKey(x, y []byte) (identityKey, error) {
	point := make([]byte, 0, 65)
	point = append(point, 4)
	point = append(point, x...)
	point = append(point, y...)
	if _, err := ecdh.P256().NewPublicKey(point); err != nil {
		return identityKey{}, fmt.Errorf("is not a point of the curve P-256")
	}
	return identityKey{
		Alg:  identityAlgES256,
		KID:  p256Thumbprint(x, y),
		P256: &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)},
	}, nil
}

// ed25519Thumbprint gives the RFC 7638 thumbprint of an Ed25519 key: the SHA-256 of the canonical JWK, in base64url.
func ed25519Thumbprint(x []byte) string {
	return jwkThumbprint(`{"crv":"Ed25519","kty":"OKP","x":"` + base64.RawURLEncoding.EncodeToString(x) + `"}`)
}

// p256Thumbprint gives the RFC 7638 thumbprint of a P-256 key.
func p256Thumbprint(x, y []byte) string {
	return jwkThumbprint(`{"crv":"P-256","kty":"EC","x":"` + base64.RawURLEncoding.EncodeToString(x) + `","y":"` +
		base64.RawURLEncoding.EncodeToString(y) + `"}`)
}

func jwkThumbprint(canonical string) string {
	digest := sha256.Sum256([]byte(canonical))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// parseIdentityJWKSet reads a JWK Set with 1 to 16 keys. Each key is Ed25519 (OKP) or P-256 (EC).
func parseIdentityJWKSet(text string) ([]identityKey, error) {
	if err := rejectDuplicateMembers(text); err != nil {
		return nil, err
	}
	var set map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &set); err != nil {
		return nil, fmt.Errorf("the JWK Set must be a JSON object: %v", err)
	}
	rawKeys, ok := set["keys"]
	if !ok {
		return nil, fmt.Errorf("the JWK Set must have the member \"keys\"")
	}
	var items []json.RawMessage
	if len(rawKeys) == 0 || rawKeys[0] != '[' || json.Unmarshal(rawKeys, &items) != nil {
		return nil, fmt.Errorf("the member \"keys\" must be an array")
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("the member \"keys\" has no key: give 1 to %d keys", maxIdentityKeys)
	}
	if len(items) > maxIdentityKeys {
		return nil, fmt.Errorf("the member \"keys\" has %d keys: give 1 to %d keys", len(items), maxIdentityKeys)
	}
	keys := make([]identityKey, 0, len(items))
	kids := make(map[string]int, len(items))
	for index, item := range items {
		key, err := parseIdentityKey(item)
		if err != nil {
			return nil, fmt.Errorf("key %d: %v", index+1, err)
		}
		if first, exists := kids[key.KID]; exists {
			return nil, fmt.Errorf("key %d: the kid %q is also the kid of key %d; each kid must be unique", index+1, key.KID, first)
		}
		kids[key.KID] = index + 1
		keys = append(keys, key)
	}
	return keys, nil
}

// rejectDuplicateMembers walks the JSON text and refuses a duplicate member name at any level, and any data after
// the value. The standard decoder keeps the last duplicate member without an error.
func rejectDuplicateMembers(text string) error {
	decoder := json.NewDecoder(strings.NewReader(text))
	if err := walkJSONValue(decoder, "the JWK Set"); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("the JWK Set has data after the JSON value")
	}
	return nil
}

func walkJSONValue(decoder *json.Decoder, where string) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("the JWK Set is not valid JSON: %v", err)
	}
	delim, isDelim := token.(json.Delim)
	if !isDelim {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			nameToken, err := decoder.Token()
			if err != nil {
				return fmt.Errorf("the JWK Set is not valid JSON: %v", err)
			}
			name, _ := nameToken.(string)
			if _, exists := seen[name]; exists {
				return fmt.Errorf("the member %q is two times in %s", name, where)
			}
			seen[name] = struct{}{}
			if err := walkJSONValue(decoder, where+" member "+fmt.Sprintf("%q", name)); err != nil {
				return err
			}
		}
	case '[':
		for index := 0; decoder.More(); index++ {
			if err := walkJSONValue(decoder, fmt.Sprintf("%s item %d", where, index+1)); err != nil {
				return err
			}
		}
	}
	if _, err := decoder.Token(); err != nil {
		return fmt.Errorf("the JWK Set is not valid JSON: %v", err)
	}
	return nil
}

// parseIdentityKey reads one JWK. The caller adds the position of the key to the error.
func parseIdentityKey(raw json.RawMessage) (identityKey, error) {
	if len(raw) == 0 || raw[0] != '{' {
		return identityKey{}, fmt.Errorf("must be a JSON object")
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return identityKey{}, fmt.Errorf("must be a JSON object: %v", err)
	}
	for _, name := range jwkPrivateMembers {
		if _, exists := members[name]; exists {
			return identityKey{}, fmt.Errorf("has the private member %q; give public keys only", name)
		}
	}
	text := func(name string) (string, bool, error) {
		value, exists := members[name]
		if !exists {
			return "", false, nil
		}
		if len(value) == 0 || value[0] != '"' {
			return "", true, fmt.Errorf("the member %q must be a string", name)
		}
		var out string
		if err := json.Unmarshal(value, &out); err != nil {
			return "", true, fmt.Errorf("the member %q must be a string", name)
		}
		return out, true, nil
	}
	kty, _, err := text("kty")
	if err != nil {
		return identityKey{}, err
	}
	crv, _, err := text("crv")
	if err != nil {
		return identityKey{}, err
	}
	var key identityKey
	switch {
	case kty == "OKP" && crv == "Ed25519":
		x, err := jwkCoordinate(text, "x")
		if err != nil {
			return identityKey{}, err
		}
		key = identityKey{Alg: identityAlgEd25519, KID: ed25519Thumbprint(x), Ed25519: ed25519.PublicKey(x)}
	case kty == "EC" && crv == "P-256":
		x, err := jwkCoordinate(text, "x")
		if err != nil {
			return identityKey{}, err
		}
		y, err := jwkCoordinate(text, "y")
		if err != nil {
			return identityKey{}, err
		}
		if key, err = p256IdentityKey(x, y); err != nil {
			return identityKey{}, fmt.Errorf("the point (x, y) %v", err)
		}
	default:
		return identityKey{}, fmt.Errorf("kty %q with crv %q is not supported; use kty \"OKP\" with crv \"Ed25519\", or kty \"EC\" with crv \"P-256\"", kty, crv)
	}

	// alg is optional. When it is present, it must fit the key: "Ed25519" or "EdDSA" for OKP, "ES256" for EC. The name
	// EdDSA is accepted here, as ClickHouse Dash accepts it. A token header with the alg EdDSA is still refused. The
	// members use and key_ops have no effect.
	alg, hasAlg, err := text("alg")
	if err != nil {
		return identityKey{}, err
	}
	if hasAlg && alg != key.Alg && !(key.Alg == identityAlgEd25519 && alg == "EdDSA") {
		return identityKey{}, fmt.Errorf("alg must be %q for this key, got %q", key.Alg, alg)
	}
	// The declared kid replaces the thumbprint: 1 to 256 bytes without a control character (a byte below 0x20, or 0x7f).
	kid, hasKID, err := text("kid")
	if err != nil {
		return identityKey{}, err
	}
	if hasKID {
		if kid == "" || len(kid) > maxKIDBytes || strings.ContainsFunc(kid, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return identityKey{}, fmt.Errorf("kid must have 1 to %d bytes without a control character, got %q", maxKIDBytes, kid)
		}
		key.KID = kid
	}
	return key, nil
}

// jwkCoordinate reads a member that holds 32 bytes in base64url without padding.
func jwkCoordinate(text func(string) (string, bool, error), name string) ([]byte, error) {
	value, exists, err := text(name)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("the member %q is required", name)
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("the member %q must be base64url without padding: %v", name, err)
	}
	if len(decoded) != 32 {
		return nil, fmt.Errorf("the member %q must hold 32 bytes, got %d", name, len(decoded))
	}
	return decoded, nil
}
