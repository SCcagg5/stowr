package main

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
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
		"not JSON, so not PEM":    {`not json`, "line 1 is not -----BEGIN PUBLIC KEY-----"},
		"empty text":              {``, "no key"},
		"array at the top":        {`[]`, "line 1 is not -----BEGIN PUBLIC KEY-----"},
		"JSON that is not valid":  {`{"keys":`, "not valid JSON"},
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
		"alg EdDSA for EC":        {keySet(ecJWK(`,"alg":"EdDSA"`)), `alg must be "ES256"`},
		"alg mismatch":            {keySet(edJWK(`,"alg":"ES256"`)), `alg must be "Ed25519"`},
		"alg mismatch for EC":     {keySet(ecJWK(`,"alg":"Ed25519"`)), `alg must be "ES256"`},
		"alg is not a string":     {keySet(edJWK(`,"alg":5`)), `"alg" must be a string`},
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
		"point not on the curve":  {keySet(`{"kty":"EC","crv":"P-256","x":"` + rfc7515X + `","y":"` + offCurveY + `"}`), "not a point of the curve P-256"},
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
	_, err := parseIdentityPublicKeys(keySet(edJWK(`,"kid":"a"`), ecJWK(`,"kid":"b"`), edJWK(`,"kid":"c","alg":"ES256"`)))
	if err == nil || !strings.HasPrefix(err.Error(), "key 3: ") {
		t.Fatalf("error = %v, want a prefix \"key 3: \"", err)
	}
}

// As in ClickHouse Dash: the alg EdDSA of an OKP key is accepted, and the members use and key_ops have no effect. The
// alg EdDSA of a token header is still refused (access_identity.go).
func TestParseIdentityPublicKeysJWKMembersWithoutEffect(t *testing.T) {
	t.Parallel()
	keys, err := parseIdentityPublicKeys(keySet(
		edJWK(`,"kid":"a","alg":"EdDSA"`),
		edJWK(`,"kid":"b","use":"enc"`),
		edJWK(`,"kid":"c","key_ops":["sign"]`),
		edJWK(`,"kid":"d","key_ops":"verify"`),
		edJWK(`,"kid":"e\u0085"`),
	))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		if key.Alg != identityAlgEd25519 {
			t.Fatalf("key %q has the alg %q, want Ed25519", key.KID, key.Alg)
		}
	}
}

// pemBlock gives one PEM block of a SubjectPublicKeyInfo.
func pemBlock(t *testing.T, public any) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// rfc8037PEM gives the Ed25519 key of RFC 8037, appendix A.3, as PEM.
func rfc8037PEM(t *testing.T) string {
	t.Helper()
	x, err := base64.RawURLEncoding.DecodeString(rfc8037X)
	if err != nil {
		t.Fatal(err)
	}
	return pemBlock(t, ed25519.PublicKey(x))
}

// rfc7515PEM gives the P-256 key of RFC 7515, appendix A.3, as PEM.
func rfc7515PEM(t *testing.T) string {
	t.Helper()
	x, _ := base64.RawURLEncoding.DecodeString(rfc7515X)
	y, _ := base64.RawURLEncoding.DecodeString(rfc7515Y)
	return pemBlock(t, &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)})
}

// A PEM key gets its RFC 7638 thumbprint as kid: the same kid as the JWK of the same key without kid.
func TestParseIdentityPublicKeysPEM(t *testing.T) {
	t.Parallel()
	keys, err := parseIdentityPublicKeys(rfc8037PEM(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].Alg != identityAlgEd25519 || keys[0].KID != rfc8037Thumbprint || len(keys[0].Ed25519) != 32 {
		t.Fatalf("Ed25519 PEM = %+v", keys)
	}
	if got := keys[0].display(); got != "Ed25519 kid="+rfc8037Thumbprint {
		t.Fatalf("display = %q", got)
	}
	keys, err = parseIdentityPublicKeys(rfc7515PEM(t))
	if err != nil {
		t.Fatal(err)
	}
	fromJWK, err := parseIdentityPublicKeys(keySet(ecJWK("")))
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].Alg != identityAlgES256 || keys[0].P256 == nil || keys[0].KID != fromJWK[0].KID {
		t.Fatalf("P-256 PEM = %+v, want the kid %q", keys, fromJWK[0].KID)
	}
	if got := keys[0].display(); got != "P-256 kid="+fromJWK[0].KID {
		t.Fatalf("display = %q", got)
	}
}

// Two blocks, spaces and line ends around them, CRLF line ends, and 16 blocks are accepted.
func TestParseIdentityPublicKeysPEMForms(t *testing.T) {
	t.Parallel()
	two := "\n  " + rfc8037PEM(t) + "\n\n" + rfc7515PEM(t) + "\n"
	keys, err := parseIdentityPublicKeys(two)
	if err != nil || len(keys) != 2 || keys[0].Alg != identityAlgEd25519 || keys[1].Alg != identityAlgES256 {
		t.Fatalf("two blocks: keys = %+v, error = %v", keys, err)
	}
	crlf := strings.ReplaceAll(rfc8037PEM(t), "\n", "\r\n")
	if keys, err := parseIdentityPublicKeys(crlf); err != nil || len(keys) != 1 {
		t.Fatalf("CRLF: keys = %d, error = %v", len(keys), err)
	}
	// Two identical blocks give one kid two times. As in ClickHouse Dash, only a JWK Set refuses a repeated kid.
	if keys, err := parseIdentityPublicKeys(rfc8037PEM(t) + rfc8037PEM(t)); err != nil || len(keys) != 2 {
		t.Fatalf("two identical blocks: keys = %d, error = %v", len(keys), err)
	}
	sixteen := ""
	for i := 0; i < 16; i++ {
		public, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		sixteen += pemBlock(t, public)
	}
	if keys, err := parseIdentityPublicKeys(sixteen); err != nil || len(keys) != 16 {
		t.Fatalf("16 blocks: keys = %d, error = %v", len(keys), err)
	}
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseIdentityPublicKeys(sixteen + pemBlock(t, public)); err == nil || !strings.Contains(err.Error(), "more than 16 keys") {
		t.Fatalf("17 blocks: error = %v", err)
	}
}

func TestParseIdentityPublicKeysPEMRefusals(t *testing.T) {
	t.Parallel()
	ed := rfc8037PEM(t)
	body := strings.TrimSuffix(strings.TrimPrefix(ed, "-----BEGIN PUBLIC KEY-----\n"), "-----END PUBLIC KEY-----\n")
	rsaKey, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// A compressed P-256 point: the prefix of a SubjectPublicKeyInfo with a point of 33 bytes.
	compressedDER := []byte{0x30, 0x39, 0x30, 0x13, 0x06, 0x07, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x02, 0x01, 0x06,
		0x08, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07, 0x03, 0x22, 0x00, 0x02}
	compressedDER = append(compressedDER, make([]byte, 32)...)
	compressed := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: compressedDER}))
	// A P-256 point that is not on the curve: the y of RFC 7515 with its first byte changed.
	x, _ := base64.RawURLEncoding.DecodeString(rfc7515X)
	y, _ := base64.RawURLEncoding.DecodeString(rfc7515Y)
	y[0] ^= 0xff
	offCurve := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: append(append(append([]byte(nil), p256SPKIPrefix...), x...), y...)}))
	for name, test := range map[string]struct {
		text string
		want string
	}{
		"text before the block":  {"keys:\n" + ed, "line 1 is not -----BEGIN PUBLIC KEY-----"},
		"text after the block":   {ed + "end\n", "line 4 is not -----BEGIN PUBLIC KEY-----"},
		"private key":            {"-----BEGIN PRIVATE KEY-----\n" + body + "-----END PRIVATE KEY-----\n", "line 1 is not -----BEGIN PUBLIC KEY-----"},
		"PEM header":             {"-----BEGIN PUBLIC KEY-----\nProc-Type: 4,ENCRYPTED\n" + body + "-----END PUBLIC KEY-----\n", "key 1 is not valid base64"},
		"no end line":            {strings.TrimSuffix(ed, "-----END PUBLIC KEY-----\n"), "-----END PUBLIC KEY----- is missing"},
		"bad base64":             {"-----BEGIN PUBLIC KEY-----\n%%%%\n-----END PUBLIC KEY-----\n", "key 1 is not valid base64"},
		"base64 without padding": {"-----BEGIN PUBLIC KEY-----\n" + strings.TrimRight(strings.TrimSpace(body), "=") + "\n-----END PUBLIC KEY-----\n", "key 1 is not valid base64"},
		"empty block":            {"-----BEGIN PUBLIC KEY-----\n-----END PUBLIC KEY-----\n", "key 1 is not an Ed25519 public key"},
		"RSA key":                {pemBlock(t, &rsaKey.PublicKey), "key 1 is not an Ed25519 public key or a P-256 public key"},
		"P-384 key":              {pemBlock(t, &p384.PublicKey), "key 1 is not an Ed25519 public key or a P-256 public key"},
		"compressed P-256 point": {compressed, "key 1 is not an Ed25519 public key or a P-256 public key with an uncompressed point"},
		"point not on the curve": {offCurve, "key 1 is not a point of the curve P-256"},
		"second block is bad":    {ed + "-----BEGIN PUBLIC KEY-----\nAAAA\n-----END PUBLIC KEY-----\n", "key 2 is not an Ed25519 public key"},
		"block too long":         {"-----BEGIN PUBLIC KEY-----\n" + strings.Repeat("A", 4097) + "\n-----END PUBLIC KEY-----\n", "key 1 is too long"},
		"only spaces":            {" \n\t\n", "no key"},
		"a JWK Set and PEM":      {keySet(edJWK("")) + "\n" + ed, "data after the JSON value"},
		"PEM then a JWK Set":     {ed + keySet(edJWK("")), "line 4 is not -----BEGIN PUBLIC KEY-----"},
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
