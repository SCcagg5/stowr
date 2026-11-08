package main

import (
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

// The public keys of the issuer of the identity tokens. The configuration gives them as a JWK Set (RFC 7517).
// The parser is strict: it refuses a duplicate member, trailing data, a private member and each key that is not
// Ed25519 or P-256.

const (
	// maxIdentityKeys is the most keys in the JWK Set.
	maxIdentityKeys = 16

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

// parseIdentityPublicKeys reads a JWK Set with 1 to 16 keys. Each key is Ed25519 (OKP) or P-256 (EC).
func parseIdentityPublicKeys(text string) ([]identityKey, error) {
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
	var thumbprint string
	switch {
	case kty == "OKP" && crv == "Ed25519":
		x, err := jwkCoordinate(text, "x")
		if err != nil {
			return identityKey{}, err
		}
		key = identityKey{Alg: identityAlgEd25519, Ed25519: ed25519.PublicKey(x)}
		thumbprint = `{"crv":"Ed25519","kty":"OKP","x":"` + base64.RawURLEncoding.EncodeToString(x) + `"}`
	case kty == "EC" && crv == "P-256":
		x, err := jwkCoordinate(text, "x")
		if err != nil {
			return identityKey{}, err
		}
		y, err := jwkCoordinate(text, "y")
		if err != nil {
			return identityKey{}, err
		}
		point := make([]byte, 0, 65)
		point = append(point, 4)
		point = append(point, x...)
		point = append(point, y...)
		if _, err := ecdh.P256().NewPublicKey(point); err != nil {
			return identityKey{}, fmt.Errorf("the point (x, y) is not on the curve P-256")
		}
		key = identityKey{Alg: identityAlgES256, P256: &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}}
		thumbprint = `{"crv":"P-256","kty":"EC","x":"` + base64.RawURLEncoding.EncodeToString(x) + `","y":"` + base64.RawURLEncoding.EncodeToString(y) + `"}`
	default:
		return identityKey{}, fmt.Errorf("kty %q with crv %q is not supported; use kty \"OKP\" with crv \"Ed25519\", or kty \"EC\" with crv \"P-256\"", kty, crv)
	}

	alg, hasAlg, err := text("alg")
	if err != nil {
		return identityKey{}, err
	}
	if hasAlg && alg != key.Alg {
		return identityKey{}, fmt.Errorf("alg must be %q for this key, got %q", key.Alg, alg)
	}
	use, hasUse, err := text("use")
	if err != nil {
		return identityKey{}, err
	}
	if hasUse && use != "sig" {
		return identityKey{}, fmt.Errorf("use must be \"sig\", got %q", use)
	}
	if rawOps, exists := members["key_ops"]; exists {
		var ops []string
		if len(rawOps) == 0 || rawOps[0] != '[' || json.Unmarshal(rawOps, &ops) != nil {
			return identityKey{}, fmt.Errorf("the member \"key_ops\" must be an array of strings")
		}
		if !containsString(ops, "verify") {
			return identityKey{}, fmt.Errorf("key_ops must contain \"verify\"")
		}
	}
	kid, hasKID, err := text("kid")
	if err != nil {
		return identityKey{}, err
	}
	if hasKID {
		if !validAccessID(kid) {
			return identityKey{}, fmt.Errorf("kid must have 1 to %d bytes of UTF-8 without a control character, got %q", maxAccessIDBytes, kid)
		}
		key.KID = kid
	} else {
		digest := sha256.Sum256([]byte(thumbprint))
		key.KID = base64.RawURLEncoding.EncodeToString(digest[:])
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
