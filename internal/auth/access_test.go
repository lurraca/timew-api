package auth

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	team = "example.cloudflareaccess.com"
	aud  = "aud-tag-123"
)

// Keys is a throwaway Access signing key, shared with the server tests.
type Keys struct {
	Kid  string
	Priv *rsa.PrivateKey
}

func NewKeys(t testing.TB, kid string) *Keys {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &Keys{kid, k}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (k *Keys) JWK() map[string]string {
	return map[string]string{"kid": k.Kid, "kty": "RSA", "alg": "RS256",
		"n": b64(k.Priv.N.Bytes()), "e": b64(big.NewInt(int64(k.Priv.E)).Bytes())}
}

// Token signs claims on top of a valid browser login; nil deletes a claim.
func (k *Keys) Token(alg string, over map[string]any) string {
	claims := map[string]any{"aud": []string{aud}, "iss": "https://" + team,
		"exp": time.Now().Add(10 * time.Minute).Unix(), "email": "Me@Example.com"}
	for key, v := range over {
		if v == nil {
			delete(claims, key)
		} else {
			claims[key] = v
		}
	}
	h, _ := json.Marshal(map[string]string{"alg": alg, "kid": k.Kid})
	p, _ := json.Marshal(claims)
	signing := b64(h) + "." + b64(p)
	digest := sha256.Sum256([]byte(signing))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, k.Priv, crypto.SHA256, digest[:])
	return signing + "." + b64(sig)
}

// Certs serves a JWKS for whatever *current points at, counting fetches.
func Certs(t testing.TB, current **Keys, fetches *atomic.Int32) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetches.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"keys": []any{(*current).JWK()}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func NewAccess(certsURL string) *Access {
	return &Access{TeamDomain: team, Audience: aud, CertsURL: certsURL}
}

func req(token string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	if token != "" {
		r.Header.Set(Header, token)
	}
	return r
}

func TestValidTokens(t *testing.T) {
	keys := NewKeys(t, "k1")
	var n atomic.Int32
	a := NewAccess(Certs(t, &keys, &n).URL)

	if who, err := a.Who(req(keys.Token("RS256", nil))); err != nil || who != "me@example.com" {
		t.Fatal(who, err)
	}
	svc := keys.Token("RS256", map[string]any{"email": nil, "common_name": "abc.access"})
	if who, err := a.Who(req(svc)); err != nil || who != "abc.access" {
		t.Fatal(who, err)
	}
	if n.Load() != 1 {
		t.Fatalf("keys fetched %d times, want 1", n.Load())
	}
}

func TestRejections(t *testing.T) {
	keys, other := NewKeys(t, "k1"), NewKeys(t, "k1")
	var n atomic.Int32
	a := NewAccess(Certs(t, &keys, &n).URL)
	now := time.Now()

	cases := map[string]string{
		"missing":               "",
		"garbage":               "a.b",
		"wrong audience":        keys.Token("RS256", map[string]any{"aud": []string{"someone-else"}}),
		"wrong issuer":          keys.Token("RS256", map[string]any{"iss": "https://evil.cloudflareaccess.com"}),
		"expired":               keys.Token("RS256", map[string]any{"exp": now.Add(-time.Hour).Unix()}),
		"not yet valid":         keys.Token("RS256", map[string]any{"nbf": now.Add(time.Hour).Unix()}),
		"signed by another key": other.Token("RS256", nil),
		"alg none":              keys.Token("none", nil),
		"names nobody":          keys.Token("RS256", map[string]any{"email": nil}),
	}
	for name, tok := range cases {
		if _, err := a.Who(req(tok)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestTamperedPayload(t *testing.T) {
	keys := NewKeys(t, "k1")
	var n atomic.Int32
	a := NewAccess(Certs(t, &keys, &n).URL)
	good := keys.Token("RS256", nil)
	forged, _ := json.Marshal(map[string]any{"aud": aud, "iss": "https://" + team, "exp": time.Now().Add(time.Hour).Unix(), "email": "evil@example.com"})
	parts := strings.Split(good, ".")
	if _, err := a.Who(req(parts[0] + "." + b64(forged) + "." + parts[2])); err == nil {
		t.Fatal("accepted a forged payload")
	}
}

func TestKeyRotationRefetchesOnce(t *testing.T) {
	keys := NewKeys(t, "k1")
	var n atomic.Int32
	a := NewAccess(Certs(t, &keys, &n).URL)
	if _, err := a.Who(req(keys.Token("RS256", nil))); err != nil {
		t.Fatal(err)
	}
	// Access rotated: a new kid appears. Within 10 minutes of the last fetch
	// an unknown kid is refused rather than refetched, so junk tokens can't
	// make us hammer Cloudflare; after that it's fetched once.
	keys = NewKeys(t, "k2")
	if _, err := a.Who(req(keys.Token("RS256", nil))); err == nil {
		t.Fatal("accepted an unknown kid inside the 10-minute window")
	}
	a.fetched = time.Now().Add(-11 * time.Minute)
	if _, err := a.Who(req(keys.Token("RS256", nil))); err != nil {
		t.Fatal(err)
	}
	if n.Load() != 2 {
		t.Fatalf("keys fetched %d times, want 2", n.Load())
	}
}
