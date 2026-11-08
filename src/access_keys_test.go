package main

import (
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

const (
	// The Ed25519 key of RFC 8037, appendix A.3, and its thumbprint.
	rfc8037X          = "11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo"
	rfc8037Thumbprint = "kPrK_qmxVWaYVA9wwBF6Iuo3vVzz7TxHCTwXBygrS4k"
	// The P-256 key of RFC 7515, appendix A.3.
	rfc7515X = "f83OJ3D2xF1Bg8vub9tLe1gHMzV76e8Tus9uPHvRVEU"
	rfc7515Y = "x_FEzRu9m36HLN_tue659LNpXW6pCyStikYjKIWI5a0"
)

// edJWK gives an Ed25519 JWK. extra is more members, with a comma at the start.
func edJWK(extra string) string {
	return `{"kty":"OKP","crv":"Ed25519","x":"` + rfc8037X + `"` + extra + `}`
}

// ecJWK gives a P-256 JWK. extra is more members, with a comma at the start.
func ecJWK(extra string) string {
	return `{"kty":"EC","crv":"P-256","x":"` + rfc7515X + `","y":"` + rfc7515Y + `"` + extra + `}`
}

func keySet(keys ...string) string {
	return `{"keys":[` + strings.Join(keys, ",") + `]}`
}

func TestParseIdentityPublicKeysAcceptsEd25519AndP256(t *testing.T) {
	t.Parallel()
	keys, err := parseIdentityPublicKeys(keySet(edJWK(`,"kid":"ed-1","alg":"Ed25519","use":"sig","key_ops":["verify"],"x5t":"ignored"`), ecJWK(`,"kid":"ec-1","alg":"ES256"`)))
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("keys = %d, want 2", len(keys))
	}
	if keys[0].Alg != "Ed25519" || keys[0].KID != "ed-1" || len(keys[0].Ed25519) != 32 || keys[0].P256 != nil {
		t.Fatalf("key 1 = %+v", keys[0])
	}
	if keys[1].Alg != "ES256" || keys[1].KID != "ec-1" || keys[1].Ed25519 != nil || keys[1].P256 == nil {
		t.Fatalf("key 2 = %+v", keys[1])
	}
	if !keys[1].P256.Curve.IsOnCurve(keys[1].P256.X, keys[1].P256.Y) || keys[1].P256.Curve != elliptic.P256() {
		t.Fatal("the P-256 key is not on the curve P-256")
	}
	if got := keys[0].display(); got != "Ed25519 kid=ed-1" {
		t.Fatalf("display = %q", got)
	}
	if got := keys[1].display(); got != "P-256 kid=ec-1" {
		t.Fatalf("display = %q", got)
	}
}

func TestParseIdentityPublicKeysThumbprintIsTheDefaultKID(t *testing.T) {
	t.Parallel()
	keys, err := parseIdentityPublicKeys(keySet(edJWK(""), ecJWK("")))
	if err != nil {
		t.Fatal(err)
	}
	if keys[0].KID != rfc8037Thumbprint {
		t.Fatalf("Ed25519 kid = %q, want the thumbprint of RFC 8037 %q", keys[0].KID, rfc8037Thumbprint)
	}
	digest := sha256.Sum256([]byte(`{"crv":"P-256","kty":"EC","x":"` + rfc7515X + `","y":"` + rfc7515Y + `"}`))
	if want := base64.RawURLEncoding.EncodeToString(digest[:]); keys[1].KID != want {
		t.Fatalf("P-256 kid = %q, want %q", keys[1].KID, want)
	}
}

func TestParseIdentityPublicKeysAcceptsTheSixteenKeysAndTheUnknownMembersOfTheSet(t *testing.T) {
	t.Parallel()
	items := make([]string, 0, 16)
	for i := 0; i < 16; i++ {
		items = append(items, edJWK(fmt.Sprintf(`,"kid":"k%d"`, i)))
	}
	keys, err := parseIdentityPublicKeys(`{"extra":{"a":1},"keys":[` + strings.Join(items, ",") + `]}`)
	if err != nil || len(keys) != 16 {
		t.Fatalf("keys = %d, error = %v", len(keys), err)
	}
}

func TestParseIdentityPublicKeysRefusals(t *testing.T) {
	t.Parallel()
	items := make([]string, 0, 17)
	for i := 0; i < 17; i++ {
		items = append(items, edJWK(fmt.Sprintf(`,"kid":"k%d"`, i)))
	}
	offCurveY := "y" + rfc7515Y[1:]
	for name, test := range map[string]struct {
		text string
		want string
	}{
		"not JSON":                {`not json`, "not valid JSON"},
		"empty text":              {``, "not valid JSON"},
		"array at the top":        {`[]`, "must be a JSON object"},
		"no keys member":          {`{}`, `must have the member "keys"`},
		"keys is not an array":    {`{"keys":{}}`, `"keys" must be an array`},
		"no keys":                 {`{"keys":[]}`, "has no key"},
		"17 keys":                 {`{"keys":[` + strings.Join(items, ",") + `]}`, "has 17 keys"},
		"key is not an object":    {`{"keys":["x"]}`, "key 1: must be a JSON object"},
		"duplicate kid":           {keySet(edJWK(`,"kid":"same"`), ecJWK(`,"kid":"same"`)), `key 2: the kid "same" is also the kid of key 1`},
		"duplicate member of key": {keySet(edJWK(`,"kid":"a","kid":"b"`)), `the member "kid" is two times`},
		"duplicate member of set": {`{"keys":[` + edJWK("") + `],"keys":[]}`, `the member "keys" is two times in the JWK Set`},
		"duplicate with escape":   {`{"keys":[` + edJWK(`,"kid":"a","kid":"b"`) + `]}`, `the member "kid" is two times`},
		"trailing data":           {keySet(edJWK("")) + ` {}`, "data after the JSON value"},
		"private member d":        {keySet(edJWK(`,"d":"` + rfc8037X + `"`)), `private member "d"`},
		"private member k":        {keySet(edJWK(`,"k":"AAAA"`)), `private member "k"`},
		"private member oth":      {keySet(ecJWK(`,"oth":[]`)), `private member "oth"`},
		"alg EdDSA":               {keySet(edJWK(`,"alg":"EdDSA"`)), `alg must be "Ed25519"`},
		"alg mismatch":            {keySet(edJWK(`,"alg":"ES256"`)), `alg must be "Ed25519"`},
		"alg mismatch for EC":     {keySet(ecJWK(`,"alg":"Ed25519"`)), `alg must be "ES256"`},
		"alg is not a string":     {keySet(edJWK(`,"alg":5`)), `"alg" must be a string`},
		"use enc":                 {keySet(edJWK(`,"use":"enc"`)), `use must be "sig"`},
		"key_ops without verify":  {keySet(edJWK(`,"key_ops":["sign"]`)), `key_ops must contain "verify"`},
		"key_ops is not an array": {keySet(edJWK(`,"key_ops":"verify"`)), `"key_ops" must be an array`},
		"empty kid":               {keySet(edJWK(`,"kid":""`)), "kid must have 1 to 256 bytes"},
		"long kid":                {keySet(edJWK(`,"kid":"` + strings.Repeat("k", 257) + `"`)), "kid must have 1 to 256 bytes"},
		"kid with a control":      {keySet(edJWK(`,"kid":"a\u0001b"`)), "kid must have 1 to 256 bytes"},
		"kty RSA":                 {keySet(`{"kty":"RSA","n":"AQAB","e":"AQAB"}`), `kty "RSA"`},
		"EC with crv P-384":       {keySet(`{"kty":"EC","crv":"P-384","x":"AA","y":"AA"}`), `crv "P-384"`},
		"OKP with crv Ed448":      {keySet(`{"kty":"OKP","crv":"Ed448","x":"AA"}`), `crv "Ed448"`},
		"no kty":                  {keySet(`{"crv":"Ed25519","x":"` + rfc8037X + `"}`), "is not supported"},
		"no x":                    {keySet(`{"kty":"OKP","crv":"Ed25519"}`), `the member "x" is required`},
		"no y":                    {keySet(`{"kty":"EC","crv":"P-256","x":"` + rfc7515X + `"}`), `the member "y" is required`},
		"x too short":             {keySet(`{"kty":"OKP","crv":"Ed25519","x":"` + strings.Repeat("A", 42) + `"}`), `"x" must hold 32 bytes, got 31`},
		"x too long":              {keySet(`{"kty":"OKP","crv":"Ed25519","x":"` + strings.Repeat("A", 46) + `"}`), `"x" must hold 32 bytes, got 34`},
		"x with padding":          {keySet(`{"kty":"OKP","crv":"Ed25519","x":"` + rfc8037X + `="}`), `"x" must be base64url without padding`},
		"x in base64 standard":    {keySet(`{"kty":"OKP","crv":"Ed25519","x":"` + strings.ReplaceAll(rfc8037X, "_", "/") + `"}`), `"x" must be base64url without padding`},
		"x is not a string":       {keySet(`{"kty":"OKP","crv":"Ed25519","x":1}`), `"x" must be a string`},
		"y too short":             {keySet(`{"kty":"EC","crv":"P-256","x":"` + rfc7515X + `","y":"` + strings.Repeat("A", 42) + `"}`), `"y" must hold 32 bytes, got 31`},
		"point not on the curve":  {keySet(`{"kty":"EC","crv":"P-256","x":"` + rfc7515X + `","y":"` + offCurveY + `"}`), "not on the curve P-256"},
		"kty is not a string":     {keySet(`{"kty":1}`), `"kty" must be a string`},
	} {
		keys, err := parseIdentityPublicKeys(test.text)
		if err == nil {
			t.Errorf("%s: the text was accepted (%d keys)", name, len(keys))
			continue
		}
		if !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error %q does not contain %q", name, err, test.want)
		}
	}
}

func TestParseIdentityPublicKeysNamesThePositionOfTheKey(t *testing.T) {
	t.Parallel()
	_, err := parseIdentityPublicKeys(keySet(edJWK(`,"kid":"a"`), ecJWK(`,"kid":"b"`), edJWK(`,"kid":"c","use":"enc"`)))
	if err == nil || !strings.HasPrefix(err.Error(), "key 3: ") {
		t.Fatalf("error = %v, want a prefix \"key 3: \"", err)
	}
}
