package main

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	idtIssuer   = "https://edge.example"
	idtAudience = "svc-1"
	idtNow      = int64(1800000000)
)

// idtKeys holds the signing keys of the tests.
type idtKeys struct {
	edPriv ed25519.PrivateKey
	edPub  ed25519.PublicKey
	esPriv *ecdsa.PrivateKey
	other  ed25519.PrivateKey // a key that the set does not hold
}

func newIdtKeys(t testing.TB) idtKeys {
	t.Helper()
	edPub, edPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	esPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, other, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return idtKeys{edPriv: edPriv, edPub: edPub, esPriv: esPriv, other: other}
}

func (k idtKeys) edKey(kid string) identityKey {
	return identityKey{Alg: "Ed25519", KID: kid, Ed25519: k.edPub}
}

func (k idtKeys) esKey(kid string) identityKey {
	return identityKey{Alg: "ES256", KID: kid, P256: &k.esPriv.PublicKey}
}

func idtConfig(keys ...identityKey) accessConfig {
	return accessConfig{
		Mode:      accessModeAuthZEN,
		Issuer:    idtIssuer,
		Audience:  idtAudience,
		Keys:      keys,
		MaxAge:    5 * time.Minute,
		ClockSkew: 60 * time.Second,
	}
}

func idtB64(data []byte) string { return base64.RawURLEncoding.EncodeToString(data) }

// idtJSON gives raw JSON for a string or a byte slice, and the encoding of any other value.
func idtJSON(t testing.TB, value any) []byte {
	t.Helper()
	switch typed := value.(type) {
	case string:
		return []byte(typed)
	case []byte:
		return typed
	}
	out, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// idtSign makes a token. signer is "ed" (Ed25519), "es" (ES256) or "other" (an Ed25519 key that is not in the set).
func idtSign(t testing.TB, keys idtKeys, signer string, header, claims any) string {
	t.Helper()
	input := idtB64(idtJSON(t, header)) + "." + idtB64(idtJSON(t, claims))
	var signature []byte
	switch signer {
	case "ed":
		signature = ed25519.Sign(keys.edPriv, []byte(input))
	case "other":
		signature = ed25519.Sign(keys.other, []byte(input))
	case "es":
		digest := sha256.Sum256([]byte(input))
		r, s, err := ecdsa.Sign(rand.Reader, keys.esPriv, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		signature = make([]byte, 64)
		r.FillBytes(signature[:32])
		s.FillBytes(signature[32:])
	default:
		t.Fatalf("unknown signer %q", signer)
	}
	return input + "." + idtB64(signature)
}

type idtMap = map[string]any

func idtHeader(alg, kid string) idtMap {
	header := idtMap{"alg": alg, "typ": "JWT"}
	if kid != "" {
		header["kid"] = kid
	}
	return header
}

func idtClaims(extra idtMap) idtMap {
	claims := idtMap{"iss": idtIssuer, "aud": idtAudience, "sub": "user-1", "iat": idtNow - 10, "exp": idtNow + 290}
	for name, value := range extra {
		if value == nil {
			delete(claims, name)
		} else {
			claims[name] = value
		}
	}
	return claims
}

func idtVerify(cfg accessConfig, token string, now int64) identityResult {
	return newIdentityVerifier(cfg).verify(token, time.Unix(now, 0))
}

func TestIdentityTokenTable(t *testing.T) {
	keys := newIdtKeys(t)
	both := idtConfig(keys.edKey("ed1"), keys.esKey("es1"))
	edOnly := idtConfig(keys.edKey("ed1"))
	esOnly := idtConfig(keys.esKey("es1"))

	type row struct {
		name    string
		cfg     accessConfig
		signer  string
		header  any
		claims  any
		token   func(string) string // changes the finished token
		now     int64
		reason  string
		subject string
		owner   string
	}
	longSub := strings.Repeat("a", 257)
	rows := []row{
		{name: "valid Ed25519 with kid", header: idtHeader("Ed25519", "ed1"), signer: "ed", subject: "user-1"},
		{name: "valid ES256 with kid", header: idtHeader("ES256", "es1"), signer: "es", subject: "user-1"},
		{name: "valid without kid, one Ed25519 key", cfg: edOnly, header: idtHeader("Ed25519", ""), signer: "ed", subject: "user-1"},
		{name: "valid without kid, one ES256 key", cfg: esOnly, header: idtHeader("ES256", ""), signer: "es", subject: "user-1"},
		{name: "valid with kid in portal form", header: `{"alg":"Ed25519","kid":"ed1","typ":"JWT"}`, signer: "ed",
			claims:  `{"aud":"svc-1","exp":1800000290,"iat":1799999990,"iss":"https://edge.example","owner":"u-9","sub":"key:abc"}`,
			subject: "key:abc", owner: "u-9"},

		{name: "alg EdDSA", header: idtHeader("EdDSA", "ed1"), signer: "ed", reason: "algorithm"},
		{name: "alg none", header: idtHeader("none", "ed1"), signer: "ed", reason: "algorithm"},
		{name: "alg HS256", header: idtHeader("HS256", "ed1"), signer: "ed", reason: "algorithm"},
		{name: "alg ed25519 lower case", header: idtHeader("ed25519", "ed1"), signer: "ed", reason: "algorithm"},
		{name: "alg es256 lower case", header: idtHeader("es256", "es1"), signer: "es", reason: "algorithm"},
		{name: "alg missing", header: idtMap{"typ": "JWT", "kid": "ed1"}, signer: "ed", reason: "algorithm"},
		{name: "alg not a string", header: idtMap{"alg": 5, "typ": "JWT", "kid": "ed1"}, signer: "ed", reason: "algorithm"},
		{name: "header is an array", header: `[]`, signer: "ed", reason: "malformed"},
		{name: "header is a string", header: `"x"`, signer: "ed", reason: "malformed"},
		{name: "header is not JSON", header: `{`, signer: "ed", reason: "malformed"},

		{name: "typ missing", header: idtMap{"alg": "Ed25519", "kid": "ed1"}, signer: "ed", reason: "type"},
		{name: "typ jwt lower case", header: idtMap{"alg": "Ed25519", "kid": "ed1", "typ": "jwt"}, signer: "ed", reason: "type"},
		{name: "typ at+jwt", header: idtMap{"alg": "Ed25519", "kid": "ed1", "typ": "at+jwt"}, signer: "ed", reason: "type"},
		{name: "typ JOSE", header: idtMap{"alg": "Ed25519", "kid": "ed1", "typ": "JOSE"}, signer: "ed", reason: "type"},
		{name: "typ not a string", header: idtMap{"alg": "Ed25519", "kid": "ed1", "typ": 1}, signer: "ed", reason: "type"},
		{name: "crit present", header: idtMap{"alg": "Ed25519", "kid": "ed1", "typ": "JWT", "crit": []string{"exp"}}, signer: "ed", reason: "malformed"},
		{name: "crit empty value", header: idtMap{"alg": "Ed25519", "kid": "ed1", "typ": "JWT", "crit": nil}, signer: "ed", reason: "malformed"},

		{name: "kid unknown", header: idtHeader("Ed25519", "nope"), signer: "ed", reason: "kid"},
		{name: "kid not a string", header: idtMap{"alg": "Ed25519", "kid": 7, "typ": "JWT"}, signer: "ed", reason: "malformed"},
		{name: "kid null", header: `{"alg":"Ed25519","kid":null,"typ":"JWT"}`, signer: "ed", reason: "malformed"},
		{name: "kid of an ES256 key on an Ed25519 token", header: idtHeader("Ed25519", "es1"), signer: "ed", reason: "kid"},
		{name: "kid of an Ed25519 key on an ES256 token", header: idtHeader("ES256", "ed1"), signer: "es", reason: "kid"},
		{name: "Ed25519 token, set with P-256 keys only, no kid", cfg: esOnly, header: idtHeader("Ed25519", ""), signer: "ed", reason: "kid"},
		{name: "Ed25519 token, set with P-256 keys only, with kid", cfg: esOnly, header: idtHeader("Ed25519", "es1"), signer: "ed", reason: "kid"},
		{name: "ES256 token, set with Ed25519 keys only", cfg: edOnly, header: idtHeader("ES256", ""), signer: "es", reason: "kid"},
		{name: "jku jwk x5u x5c ignored", header: idtMap{"alg": "Ed25519", "kid": "ed1", "typ": "JWT", "jku": "http://127.0.0.1:1/k",
			"x5u": "http://127.0.0.1:1/c", "jwk": idtMap{"kty": "OKP"}, "x5c": []string{"AAAA"}}, signer: "ed", subject: "user-1"},

		{name: "bad signature", header: idtHeader("Ed25519", "ed1"), signer: "other", reason: "signature"},
		{name: "signature of the wrong length", header: idtHeader("Ed25519", "ed1"), signer: "ed", reason: "signature",
			token: func(s string) string { return s[:strings.LastIndex(s, ".")+1] + idtB64(make([]byte, 63)) }},
		{name: "ES256 signature of the wrong length", header: idtHeader("ES256", "es1"), signer: "es", reason: "signature",
			token: func(s string) string { return s[:strings.LastIndex(s, ".")+1] + idtB64(make([]byte, 65)) }},
		{name: "ES256 signature of zeros", header: idtHeader("ES256", "es1"), signer: "es", reason: "signature",
			token: func(s string) string { return s[:strings.LastIndex(s, ".")+1] + idtB64(make([]byte, 64)) }},
		{name: "claims changed after signature", header: idtHeader("Ed25519", "ed1"), signer: "ed", reason: "signature",
			token: func(s string) string {
				parts := strings.Split(s, ".")
				parts[1] = idtB64([]byte(`{"iss":"https://edge.example","aud":"svc-1","sub":"admin","iat":1799999990,"exp":1800000290}`))
				return strings.Join(parts, ".")
			}},

		{name: "wrong iss", header: idtHeader("Ed25519", "ed1"), signer: "ed", claims: idtClaims(idtMap{"iss": "https://evil.example"}), reason: "issuer"},
		{name: "iss missing", header: idtHeader("Ed25519", "ed1"), signer: "ed", claims: idtClaims(idtMap{"iss": nil}), reason: "issuer"},
		{name: "iss not a string", header: idtHeader("Ed25519", "ed1"), signer: "ed", claims: idtClaims(idtMap{"iss": 5}), reason: "issuer"},
		{name: "aud string", header: idtHeader("Ed25519", "ed1"), signer: "ed", claims: idtClaims(idtMap{"aud": idtAudience}), subject: "user-1"},
		{name: "aud wrong string", header: idtHeader("Ed25519", "ed1"), signer: "ed", claims: idtClaims(idtMap{"aud": "other"}), reason: "audience"},
		{name: "aud array that holds the id", header: idtHeader("Ed25519", "ed1"), signer: "ed",
			claims: idtClaims(idtMap{"aud": []string{"other", idtAudience}}), subject: "user-1"},
		{name: "aud array without the id", header: idtHeader("Ed25519", "ed1"), signer: "ed",
			claims: idtClaims(idtMap{"aud": []string{"other", "more"}}), reason: "audience"},
		{name: "aud array with a number item", header: idtHeader("Ed25519", "ed1"), signer: "ed",
			claims: idtClaims(idtMap{"aud": []any{idtAudience, 5}}), reason: "audience"},
		{name: "aud missing", header: idtHeader("Ed25519", "ed1"), signer: "ed", claims: idtClaims(idtMap{"aud": nil}), reason: "audience"},
		{name: "aud empty array", header: idtHeader("Ed25519", "ed1"), signer: "ed", claims: idtClaims(idtMap{"aud": []string{}}), reason: "audience"},

		{name: "sub missing", header: idtHeader("Ed25519", "ed1"), signer: "ed", claims: idtClaims(idtMap{"sub": nil}), reason: "subject"},
		{name: "sub empty", header: idtHeader("Ed25519", "ed1"), signer: "ed", claims: idtClaims(idtMap{"sub": ""}), reason: "subject"},
		{name: "sub of 256 bytes", header: idtHeader("Ed25519", "ed1"), signer: "ed",
			claims: idtClaims(idtMap{"sub": strings.Repeat("a", 256)}), subject: strings.Repeat("a", 256)},
		{name: "sub of 257 bytes", header: idtHeader("Ed25519", "ed1"), signer: "ed", claims: idtClaims(idtMap{"sub": longSub}), reason: "subject"},
		{name: "sub with a control character", header: idtHeader("Ed25519", "ed1"), signer: "ed", claims: idtClaims(idtMap{"sub": "a\nb"}), reason: "subject"},
		{name: "sub not a string", header: idtHeader("Ed25519", "ed1"), signer: "ed", claims: idtClaims(idtMap{"sub": 12}), reason: "subject"},
		{name: "sub key:abc is opaque", header: idtHeader("Ed25519", "ed1"), signer: "ed", claims: idtClaims(idtMap{"sub": "key:abc"}), subject: "key:abc"},

		{name: "iat missing", header: idtHeader("Ed25519", "ed1"), signer: "ed", claims: idtClaims(idtMap{"iat": nil}), reason: "claims"},
		{name: "exp missing", header: idtHeader("Ed25519", "ed1"), signer: "ed", claims: idtClaims(idtMap{"exp": nil}), reason: "claims"},
		{name: "iat is a string", header: idtHeader("Ed25519", "ed1"), signer: "ed", claims: idtClaims(idtMap{"iat": "123"}), reason: "claims"},
		{name: "iat 1.5", header: idtHeader("Ed25519", "ed1"), signer: "ed",
			claims: `{"iss":"https://edge.example","aud":"svc-1","sub":"u","iat":1.5,"exp":1800000290}`, reason: "claims"},
		{name: "iat 1e9", header: idtHeader("Ed25519", "ed1"), signer: "ed",
			claims: `{"iss":"https://edge.example","aud":"svc-1","sub":"u","iat":1e9,"exp":1800000290}`, reason: "claims"},
		{name: "exp 1800000290.0", header: idtHeader("Ed25519", "ed1"), signer: "ed",
			claims: `{"iss":"https://edge.example","aud":"svc-1","sub":"u","iat":1799999990,"exp":1800000290.0}`, reason: "claims"},
		{name: "iat negative", header: idtHeader("Ed25519", "ed1"), signer: "ed", claims: idtClaims(idtMap{"iat": -1}), reason: "claims"},
		{name: "exp above the limit", header: idtHeader("Ed25519", "ed1"), signer: "ed", claims: idtClaims(idtMap{"exp": int64(9000000000000001)}), reason: "claims"},
		{name: "exp null", header: idtHeader("Ed25519", "ed1"), signer: "ed",
			claims: `{"iss":"https://edge.example","aud":"svc-1","sub":"u","iat":1799999990,"exp":null}`, reason: "claims"},
		{name: "claims not an object", header: idtHeader("Ed25519", "ed1"), signer: "ed", claims: `[]`, reason: "malformed"},

		{name: "expired beyond the skew", header: idtHeader("Ed25519", "ed1"), signer: "ed", now: idtNow + 290 + 61, reason: "expired"},
		{name: "expired inside the skew", header: idtHeader("Ed25519", "ed1"), signer: "ed", now: idtNow + 290 + 60, subject: "user-1"},
		{name: "iat in the future beyond the skew", header: idtHeader("Ed25519", "ed1"), signer: "ed",
			claims: idtClaims(idtMap{"iat": idtNow + 61, "exp": idtNow + 100}), reason: "not_yet_valid"},
		{name: "iat in the future inside the skew", header: idtHeader("Ed25519", "ed1"), signer: "ed",
			claims: idtClaims(idtMap{"iat": idtNow + 60, "exp": idtNow + 100}), subject: "user-1"},
		{name: "exp before iat", header: idtHeader("Ed25519", "ed1"), signer: "ed",
			claims: idtClaims(idtMap{"iat": idtNow - 5, "exp": idtNow - 10}), reason: "max_age"},
		{name: "life above max_age", header: idtHeader("Ed25519", "ed1"), signer: "ed",
			claims: idtClaims(idtMap{"iat": idtNow - 10, "exp": idtNow + 291}), reason: "max_age"},
		{name: "life equal to max_age", header: idtHeader("Ed25519", "ed1"), signer: "ed",
			claims: idtClaims(idtMap{"iat": idtNow - 10, "exp": idtNow + 290}), subject: "user-1"},
		{name: "nbf in the future is ignored", header: idtHeader("Ed25519", "ed1"), signer: "ed",
			claims: idtClaims(idtMap{"nbf": idtNow + 100000}), subject: "user-1"},

		{name: "duplicate member in the header", header: `{"alg":"Ed25519","typ":"JWT","kid":"ed1","kid":"ed1"}`, signer: "ed", reason: "malformed"},
		{name: "duplicate alg in the header", header: `{"alg":"ES256","alg":"Ed25519","typ":"JWT","kid":"ed1"}`, signer: "ed", reason: "malformed"},
		{name: "duplicate member in a nested header value", header: `{"alg":"Ed25519","typ":"JWT","kid":"ed1","x":{"a":1,"a":2}}`, signer: "ed", reason: "malformed"},
		{name: "duplicate member through an escape", header: `{"alg":"Ed25519","typ":"JWT","kid":"ed1","kid":"ed1"}`, signer: "ed", reason: "malformed"},
		{name: "duplicate member in the claims", header: idtHeader("Ed25519", "ed1"), signer: "ed",
			claims: `{"iss":"https://edge.example","aud":"svc-1","sub":"u","sub":"v","iat":1799999990,"exp":1800000290}`, reason: "malformed"},
		{name: "duplicate member in a nested claim", header: idtHeader("Ed25519", "ed1"), signer: "ed",
			claims: `{"iss":"https://edge.example","aud":"svc-1","sub":"u","iat":1799999990,"exp":1800000290,"x":[{"a":1,"a":1}]}`, reason: "malformed"},
		{name: "trailing data after the header", header: `{"alg":"Ed25519","typ":"JWT","kid":"ed1"}{}`, signer: "ed", reason: "malformed"},
		{name: "trailing garbage after the header", header: `{"alg":"Ed25519","typ":"JWT","kid":"ed1"} x`, signer: "ed", reason: "malformed"},
		{name: "trailing data after the claims", header: idtHeader("Ed25519", "ed1"), signer: "ed",
			claims: `{"iss":"https://edge.example","aud":"svc-1","sub":"u","iat":1799999990,"exp":1800000290}{}`, reason: "malformed"},

		{name: "four parts", header: idtHeader("Ed25519", "ed1"), signer: "ed", reason: "malformed", token: func(s string) string { return s + ".AAAA" }},
		{name: "two parts", header: idtHeader("Ed25519", "ed1"), signer: "ed", reason: "malformed",
			token: func(s string) string { return s[:strings.LastIndex(s, ".")] }},
		{name: "empty signature part", header: idtHeader("Ed25519", "ed1"), signer: "ed", reason: "malformed",
			token: func(s string) string { return s[:strings.LastIndex(s, ".")+1] }},
		{name: "padding in a part", header: idtHeader("Ed25519", "ed1"), signer: "ed", reason: "malformed", token: func(s string) string { return s + "=" }},
		{name: "standard base64 character", header: idtHeader("Ed25519", "ed1"), signer: "ed", reason: "malformed",
			token: func(s string) string { return "+" + s[1:] }},
		{name: "extra unknown claims", header: idtHeader("Ed25519", "ed1"), signer: "ed",
			claims: idtClaims(idtMap{"scope": "x", "act": idtMap{"sub": "y"}, "jti": 5}), subject: "user-1"},
		{name: "owner as a string is kept", header: idtHeader("Ed25519", "ed1"), signer: "ed",
			claims: idtClaims(idtMap{"owner": "boss"}), subject: "user-1", owner: "boss"},
		{name: "owner as a number is ignored", header: idtHeader("Ed25519", "ed1"), signer: "ed",
			claims: idtClaims(idtMap{"owner": 42}), subject: "user-1"},
	}

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			cfg := r.cfg
			if cfg.Issuer == "" {
				cfg = both
			}
			claims := r.claims
			if claims == nil {
				claims = idtClaims(nil)
			}
			token := idtSign(t, keys, r.signer, r.header, claims)
			if r.token != nil {
				token = r.token(token)
			}
			now := r.now
			if now == 0 {
				now = idtNow
			}
			got := idtVerify(cfg, token, now)
			if got.Reason != r.reason {
				t.Fatalf("reason = %q, want %q (result %+v)", got.Reason, r.reason, got)
			}
			if r.reason == "" {
				if !got.ok() || got.Subject != r.subject || got.Owner != r.owner {
					t.Fatalf("result = %+v, want subject %q owner %q", got, r.subject, r.owner)
				}
			} else if got.ok() || got.Subject != "" {
				t.Fatalf("a refused token gave a subject: %+v", got)
			}
		})
	}
}

func TestIdentityEmptyAndLongToken(t *testing.T) {
	keys := newIdtKeys(t)
	cfg := idtConfig(keys.edKey("ed1"))
	if got := idtVerify(cfg, "", idtNow); got.Reason != "missing" {
		t.Fatalf("empty token: %+v", got)
	}
	if got := idtVerify(cfg, strings.Repeat("a", 16385), idtNow); got.Reason != "malformed" {
		t.Fatalf("token of 16385 bytes: %+v", got)
	}
	// A valid token of exactly 16384 bytes is accepted: the limit is on the length only.
	claims := idtClaims(idtMap{"pad": ""})
	base := idtSign(t, keys, "ed", idtHeader("Ed25519", "ed1"), claims)
	// Each 3 bytes of padding make 4 characters. Search the pad that gives the exact length.
	for pad := 0; pad < 20000; pad += 3 {
		claims["pad"] = strings.Repeat("p", pad)
		token := idtSign(t, keys, "ed", idtHeader("Ed25519", "ed1"), claims)
		if len(token) > maxIdentityTokenBytes {
			break
		}
		if len(token) > len(base) {
			base = token
		}
	}
	if len(base) > maxIdentityTokenBytes || len(base) < maxIdentityTokenBytes-8 {
		t.Fatalf("test token has %d bytes", len(base))
	}
	if got := idtVerify(cfg, base, idtNow); !got.ok() {
		t.Fatalf("token of %d bytes: %+v", len(base), got)
	}
}

func TestIdentityCacheUsesStoredResult(t *testing.T) {
	keys := newIdtKeys(t)
	verifier := newIdentityVerifier(idtConfig(keys.edKey("ed1")))
	token := idtSign(t, keys, "ed", idtHeader("Ed25519", "ed1"), idtClaims(idtMap{"owner": "boss"}))

	first := verifier.verify(token, time.Unix(idtNow, 0))
	if !first.ok() || first.Subject != "user-1" || first.Owner != "boss" {
		t.Fatalf("first: %+v", first)
	}
	second := verifier.verify(token, time.Unix(idtNow+1, 0))
	if second != first {
		t.Fatalf("second = %+v, want %+v", second, first)
	}
	if len(verifier.cache) != 1 {
		t.Fatalf("cache has %d entries", len(verifier.cache))
	}
	// The last second that the skew accepts still hits the cache.
	if got := verifier.verify(token, time.Unix(idtNow+290+60, 0)); !got.ok() {
		t.Fatalf("inside the skew: %+v", got)
	}
	// Later than exp + skew: the cache entry goes and the token is expired.
	if got := verifier.verify(token, time.Unix(idtNow+290+61, 0)); got.Reason != "expired" {
		t.Fatalf("after the skew: %+v", got)
	}
	if len(verifier.cache) != 0 {
		t.Fatalf("expired entry stayed: %d entries", len(verifier.cache))
	}
}

func TestIdentityCacheKeepsValidOnly(t *testing.T) {
	keys := newIdtKeys(t)
	verifier := newIdentityVerifier(idtConfig(keys.edKey("ed1")))
	bad := idtSign(t, keys, "other", idtHeader("Ed25519", "ed1"), idtClaims(nil))
	for i := 0; i < 3; i++ {
		if got := verifier.verify(bad, time.Unix(idtNow, 0)); got.Reason != "signature" {
			t.Fatalf("bad token: %+v", got)
		}
	}
	expired := idtSign(t, keys, "ed", idtHeader("Ed25519", "ed1"), idtClaims(nil))
	if got := verifier.verify(expired, time.Unix(idtNow+1000, 0)); got.Reason != "expired" {
		t.Fatalf("expired token: %+v", got)
	}
	if len(verifier.cache) != 0 {
		t.Fatalf("refused tokens were kept: %d entries", len(verifier.cache))
	}
}

func TestIdentityCacheBound(t *testing.T) {
	keys := newIdtKeys(t)
	verifier := newIdentityVerifier(idtConfig(keys.edKey("ed1")))
	now := time.Unix(idtNow, 0)
	for i := 0; i < identityCacheMaxEntries; i++ {
		token := idtSign(t, keys, "ed", idtHeader("Ed25519", "ed1"), idtClaims(idtMap{"sub": fmt.Sprintf("user-%d", i)}))
		if got := verifier.verify(token, now); !got.ok() {
			t.Fatalf("token %d: %+v", i, got)
		}
	}
	if len(verifier.cache) != identityCacheMaxEntries {
		t.Fatalf("cache has %d entries, want %d", len(verifier.cache), identityCacheMaxEntries)
	}
	extra := idtSign(t, keys, "ed", idtHeader("Ed25519", "ed1"), idtClaims(idtMap{"sub": "extra"}))
	if got := verifier.verify(extra, now); !got.ok() || got.Subject != "extra" {
		t.Fatalf("extra token: %+v", got)
	}
	if len(verifier.cache) != identityCacheMaxEntries {
		t.Fatalf("cache grew to %d entries", len(verifier.cache))
	}
	if _, kept := verifier.cache[sha256.Sum256([]byte(extra))]; kept {
		t.Fatal("the extra token was kept in a full cache")
	}
	// The extra token still verifies, without the cache.
	if got := verifier.verify(extra, now); !got.ok() {
		t.Fatalf("extra token, second time: %+v", got)
	}

	// Later, the first entries are expired: the next valid token takes a place after the clean.
	later := time.Unix(idtNow+290+61, 0)
	fresh := idtSign(t, keys, "ed", idtHeader("Ed25519", "ed1"), idtClaims(idtMap{"sub": "fresh", "iat": idtNow + 300, "exp": idtNow + 500}))
	if got := verifier.verify(fresh, later); !got.ok() {
		t.Fatalf("fresh token: %+v", got)
	}
	if len(verifier.cache) != 1 {
		t.Fatalf("cache has %d entries after the clean, want 1", len(verifier.cache))
	}
}

func TestIdentityVerifyConcurrent(t *testing.T) {
	keys := newIdtKeys(t)
	verifier := newIdentityVerifier(idtConfig(keys.edKey("ed1"), keys.esKey("es1")))
	tokens := make([]string, 8)
	for i := range tokens {
		claims := idtClaims(idtMap{"sub": fmt.Sprintf("user-%d", i)})
		if i%2 == 0 {
			tokens[i] = idtSign(t, keys, "ed", idtHeader("Ed25519", "ed1"), claims)
		} else {
			tokens[i] = idtSign(t, keys, "es", idtHeader("ES256", "es1"), claims)
		}
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for n := 0; n < 200; n++ {
				i := (g + n) % len(tokens)
				now := time.Unix(idtNow+int64(n%5), 0)
				got := verifier.verify(tokens[i], now)
				if !got.ok() || got.Subject != fmt.Sprintf("user-%d", i) {
					t.Errorf("goroutine %d token %d: %+v", g, i, got)
					return
				}
				if n%50 == 0 {
					verifier.verify(tokens[i]+"x", now)
					verifier.verify(tokens[i], time.Unix(idtNow+100000, 0))
				}
			}
		}(g)
	}
	wg.Wait()
}
