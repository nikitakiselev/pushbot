// Package auth verifies GitHub Actions OIDC tokens and applies the allowlist.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

var (
	// ErrUnauthorized: the token is invalid (HTTP 401).
	ErrUnauthorized = errors.New("unauthorized")
	// ErrForbidden: the token is valid but the repository is not allowed (HTTP 403).
	ErrForbidden = errors.New("forbidden")
)

// clockSkew is tolerated on token expiry.
const clockSkew = 60 * time.Second

// Claims are the verified token claims pushbot uses.
type Claims struct {
	Repository        string // owner/repo
	RepositoryID      string
	RepositoryOwner   string
	RepositoryOwnerID string
	Ref               string
	SHA               string
	Actor             string
	RunID             string
	Environment       string // the job's environment; "" when it has none
	JTI               string
	Expiry            time.Time
}

// Rule allows repositories of the owner with OwnerID whose name matches RepoGlob.
type Rule struct {
	Owner    string // for messages only; matching uses OwnerID
	OwnerID  string
	RepoGlob string // lower-case path.Match pattern
}

// Verifier checks tokens. It is safe for concurrent use.
type Verifier struct {
	oidc  *oidc.IDTokenVerifier
	rules []Rule

	mu   sync.Mutex
	seen map[string]time.Time // jti → expiry
}

// NewVerifier creates a verifier for issuer (keys from <issuer>/.well-known/jwks,
// fetched lazily and cached) that requires audience in the aud claim.
func NewVerifier(issuer, audience string, rules []Rule) *Verifier {
	keys := oidc.NewRemoteKeySet(context.Background(), strings.TrimSuffix(issuer, "/")+"/.well-known/jwks")
	cfg := &oidc.Config{
		ClientID: audience,
		Now:      func() time.Time { return time.Now().Add(-clockSkew) },
	}
	return &Verifier{oidc: oidc.NewVerifier(issuer, keys, cfg), rules: rules, seen: map[string]time.Time{}}
}

type rawClaims struct {
	Repository        string     `json:"repository"`
	RepositoryID      flexString `json:"repository_id"`
	RepositoryOwner   string     `json:"repository_owner"`
	RepositoryOwnerID flexString `json:"repository_owner_id"`
	Ref               string     `json:"ref"`
	SHA               string     `json:"sha"`
	Actor             string     `json:"actor"`
	RunID             flexString `json:"run_id"`
	Environment       string     `json:"environment"`
	JTI               string     `json:"jti"`
}

// Verify checks signature, issuer, audience, expiry, replay and the allowlist.
func (v *Verifier) Verify(ctx context.Context, rawToken string) (*Claims, error) {
	tok, err := v.oidc.Verify(ctx, rawToken)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	var raw rawClaims
	if err := tok.Claims(&raw); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	c := &Claims{
		Repository:        raw.Repository,
		RepositoryID:      string(raw.RepositoryID),
		RepositoryOwner:   raw.RepositoryOwner,
		RepositoryOwnerID: string(raw.RepositoryOwnerID),
		Ref:               raw.Ref,
		SHA:               raw.SHA,
		Actor:             raw.Actor,
		RunID:             string(raw.RunID),
		Environment:       raw.Environment,
		JTI:               raw.JTI,
		Expiry:            tok.Expiry,
	}
	if c.JTI == "" {
		return nil, fmt.Errorf("%w: token has no jti", ErrUnauthorized)
	}
	if c.Repository == "" || c.RepositoryID == "" || c.RepositoryOwnerID == "" {
		return nil, fmt.Errorf("%w: token has no repository claims (is it a GitHub Actions token?)", ErrUnauthorized)
	}
	if !v.allowed(c) {
		return nil, fmt.Errorf("%w: repository %s is not in the allow list", ErrForbidden, c.Repository)
	}
	if !v.markSeen(c.JTI, c.Expiry) {
		return nil, fmt.Errorf("%w: token already used", ErrUnauthorized)
	}
	return c, nil
}

func (v *Verifier) allowed(c *Claims) bool {
	_, repo, _ := strings.Cut(strings.ToLower(c.Repository), "/")
	for _, r := range v.rules {
		if r.OwnerID != c.RepositoryOwnerID {
			continue
		}
		if ok, _ := path.Match(r.RepoGlob, repo); ok {
			return true
		}
	}
	return false
}

// markSeen records jti until its expiry and reports whether it was new.
func (v *Verifier) markSeen(jti string, expiry time.Time) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := time.Now()
	for id, exp := range v.seen {
		if now.After(exp.Add(clockSkew)) {
			delete(v.seen, id)
		}
	}
	if _, dup := v.seen[jti]; dup {
		return false
	}
	v.seen[jti] = expiry
	return true
}

// flexString accepts a JSON string or number (GitHub sends IDs as strings).
type flexString string

func (f *flexString) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*f = flexString(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(data, &n); err != nil {
		return err
	}
	*f = flexString(n.String())
	return nil
}
