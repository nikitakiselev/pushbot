// Package oidctest provides a fake GitHub Actions OIDC issuer for tests.
package oidctest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

const keyID = "test-key"

// ActionsRequestToken is the bearer token the fake Actions token endpoint expects
// (the value of ACTIONS_ID_TOKEN_REQUEST_TOKEN).
const ActionsRequestToken = "test-request-token"

// Issuer serves a JWKS at /.well-known/jwks and an Actions-style token endpoint
// at /actions-token, and signs tokens with its key.
type Issuer struct {
	URL    string
	key    *rsa.PrivateKey
	server *httptest.Server

	mu            sync.Mutex
	actionsClaims map[string]any
}

// NewIssuer starts a fake issuer that is shut down when the test ends.
func NewIssuer(t testing.TB) *Issuer {
	t.Helper()
	i := &Issuer{key: newKey(t)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/jwks", func(w http.ResponseWriter, r *http.Request) {
		set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: &i.key.PublicKey, KeyID: keyID, Algorithm: string(jose.RS256), Use: "sig",
		}}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(set)
	})
	mux.HandleFunc("GET /actions-token", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "bearer "+ActionsRequestToken {
			http.Error(w, "bad request token", http.StatusUnauthorized)
			return
		}
		overrides := map[string]any{"aud": r.URL.Query().Get("audience")}
		i.mu.Lock()
		for k, v := range i.actionsClaims {
			overrides[k] = v
		}
		i.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"value": i.Token(t, overrides)})
	})
	i.server = httptest.NewServer(mux)
	i.URL = i.server.URL
	t.Cleanup(i.server.Close)
	return i
}

// SetActionsClaims sets claim overrides for tokens issued by the /actions-token endpoint.
func (i *Issuer) SetActionsClaims(overrides map[string]any) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.actionsClaims = overrides
}

// ActionsTokenURL emulates ACTIONS_ID_TOKEN_REQUEST_URL (the action appends "&audience=...").
func (i *Issuer) ActionsTokenURL() string {
	return i.URL + "/actions-token?api-version=2.0"
}

// Claims returns GitHub-like claims for nikitakiselev/my-service with the given
// overrides applied; an override with a nil value removes the claim.
func (i *Issuer) Claims(overrides map[string]any) map[string]any {
	now := time.Now()
	c := map[string]any{
		"iss":                 i.URL,
		"aud":                 "pushbot",
		"sub":                 "repo:nikitakiselev/my-service:ref:refs/heads/main",
		"repository":          "nikitakiselev/my-service",
		"repository_id":       "1001",
		"repository_owner":    "nikitakiselev",
		"repository_owner_id": "42",
		"ref":                 "refs/heads/main",
		"sha":                 "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678",
		"actor":               "nikitakiselev",
		"run_id":              "9876543210",
		"event_name":          "push",
		"jti":                 randomHex(),
		"iat":                 now.Unix(),
		"nbf":                 now.Unix(),
		"exp":                 now.Add(5 * time.Minute).Unix(),
	}
	for k, v := range overrides {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	return c
}

// Token signs Claims(overrides) with the issuer key.
func (i *Issuer) Token(t testing.TB, overrides map[string]any) string {
	return sign(t, i.key, i.Claims(overrides))
}

// ForeignToken signs Claims(overrides) with an unrelated key that has the same key ID.
func (i *Issuer) ForeignToken(t testing.TB, overrides map[string]any) string {
	return sign(t, newKey(t), i.Claims(overrides))
}

func sign(t testing.TB, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	opts := (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", keyID)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, opts)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jws.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func newKey(t testing.TB) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func randomHex() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
