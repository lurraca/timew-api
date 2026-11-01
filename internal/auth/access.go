// Package auth verifies the token Cloudflare Access adds to every request it
// lets through (header Cf-Access-Jwt-Assertion).
//
// Access already decides who gets in: an email one-time code for browsers, a
// service token for scripts and the tray app. Checking its signed token here
// too means a request that reaches the port some other way gets nothing.
// Adapted from splice's internal/web/auth.go: standard library only, since a
// JWT is three base64url parts and the signature is RSA PKCS#1 v1.5 over
// SHA-256 of "header.payload".
package auth

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

const Header = "Cf-Access-Jwt-Assertion"

// leeway allows for clock skew between kodama and Cloudflare.
const leeway = 60

// Identity says who made a request, or why it can't tell.
type Identity interface {
	Who(r *http.Request) (string, error)
}

// Dev is for local development only: every request is allowed.
type Dev struct{}

func (Dev) Who(*http.Request) (string, error) { return "local", nil }

type Access struct {
	TeamDomain string // e.g. "example.cloudflareaccess.com"
	Audience   string // the Access application's AUD tag
	Client     *http.Client
	CertsURL   string // default https://<team>/cdn-cgi/access/certs (overridable in tests)
	Now        func() time.Time

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

var ErrNoToken = errors.New("no Cloudflare Access token")

// Who returns the user's email for a browser login, or the client id for a
// service token, once the token checks out.
func (a *Access) Who(r *http.Request) (string, error) {
	tok := r.Header.Get(Header)
	if tok == "" {
		return "", ErrNoToken
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return "", errors.New("malformed token")
	}
	var hdr struct{ Alg, Kid string }
	if err := decodePart(parts[0], &hdr); err != nil {
		return "", errors.New("malformed token")
	}
	if hdr.Alg != "RS256" {
		return "", fmt.Errorf("unexpected algorithm %q", hdr.Alg)
	}
	key, err := a.key(hdr.Kid)
	if err != nil {
		return "", err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", errors.New("malformed token")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		return "", errors.New("bad token signature")
	}

	var claims struct {
		Email      string          `json:"email"`
		CommonName string          `json:"common_name"` // set for service tokens
		Iss        string          `json:"iss"`
		Exp        int64           `json:"exp"`
		Nbf        int64           `json:"nbf"`
		Aud        json.RawMessage `json:"aud"` // a string or a list of strings
	}
	if err := decodePart(parts[1], &claims); err != nil {
		return "", errors.New("malformed token")
	}
	now := time.Now
	if a.Now != nil {
		now = a.Now
	}
	t := now().Unix()
	switch {
	case claims.Iss != "https://"+a.TeamDomain:
		return "", fmt.Errorf("unexpected issuer %q", claims.Iss)
	case t >= claims.Exp+leeway:
		return "", errors.New("token expired")
	case claims.Nbf != 0 && t < claims.Nbf-leeway:
		return "", errors.New("token not yet valid")
	case !audienceHas(claims.Aud, a.Audience):
		return "", errors.New("token not for this application")
	}
	if claims.Email != "" {
		return strings.ToLower(claims.Email), nil
	}
	if claims.CommonName != "" {
		return claims.CommonName, nil
	}
	return "", errors.New("token names nobody")
}

// key returns the public key with this id, refreshing the team's key list
// when it's an hour old or a new kid appears, but at most every 10 minutes
// for unknown kids, so a junk token can't make us hammer Cloudflare.
func (a *Access) key(kid string) (*rsa.PublicKey, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if k, ok := a.keys[kid]; ok && time.Since(a.fetched) < time.Hour {
		return k, nil
	}
	if a.keys != nil && time.Since(a.fetched) < 10*time.Minute {
		if _, ok := a.keys[kid]; !ok {
			return nil, fmt.Errorf("unknown signing key %q", kid)
		}
	}
	url := a.CertsURL
	if url == "" {
		url = "https://" + a.TeamDomain + "/cdn-cgi/access/certs"
	}
	client := a.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetch Access keys: %w", err)
	}
	defer resp.Body.Close()
	var set struct {
		Keys []struct{ Kid, Kty, N, E string } `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return nil, fmt.Errorf("decode Access keys: %w", err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	a.keys, a.fetched = keys, time.Now()
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("unknown signing key %q", kid)
}

func decodePart(s string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func audienceHas(raw json.RawMessage, want string) bool {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one == want
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		for _, a := range many {
			if a == want {
				return true
			}
		}
	}
	return false
}
