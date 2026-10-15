package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nikitakiselev/pushbot/internal/oidctest"
)

var rules = []Rule{
	{Owner: "nikitakiselev", OwnerID: "42", RepoGlob: "*"},
	{Owner: "org", OwnerID: "77", RepoGlob: "homelab-*"},
}

func TestVerifyValid(t *testing.T) {
	iss := oidctest.NewIssuer(t)
	v := NewVerifier(iss.URL, "pushbot", rules)

	c, err := v.Verify(context.Background(), iss.Token(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Repository != "nikitakiselev/my-service" || c.RepositoryID != "1001" || c.RepositoryOwnerID != "42" ||
		c.Ref != "refs/heads/main" || c.SHA != "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678" ||
		c.Actor != "nikitakiselev" || c.RunID != "9876543210" {
		t.Errorf("unexpected claims %+v", c)
	}
}

func TestVerifyEnvironment(t *testing.T) {
	iss := oidctest.NewIssuer(t)
	v := NewVerifier(iss.URL, "pushbot", rules)

	c, err := v.Verify(context.Background(), iss.Token(t, map[string]any{"environment": "staging"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Environment != "staging" {
		t.Errorf("environment = %q, want staging", c.Environment)
	}

	c, err = v.Verify(context.Background(), iss.Token(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Environment != "" {
		t.Errorf("environment = %q for a job without one", c.Environment)
	}
}

func TestVerifyNumericClaims(t *testing.T) {
	iss := oidctest.NewIssuer(t)
	v := NewVerifier(iss.URL, "pushbot", rules)
	c, err := v.Verify(context.Background(), iss.Token(t, map[string]any{"repository_owner_id": 42, "repository_id": 1001}))
	if err != nil {
		t.Fatal(err)
	}
	if c.RepositoryOwnerID != "42" || c.RepositoryID != "1001" {
		t.Errorf("unexpected claims %+v", c)
	}
}

func TestVerifyUnauthorized(t *testing.T) {
	iss := oidctest.NewIssuer(t)
	other := oidctest.NewIssuer(t)
	v := NewVerifier(iss.URL, "pushbot", rules)
	cases := map[string]string{
		"garbage":        "not-a-jwt",
		"wrong audience": iss.Token(t, map[string]any{"aud": "sts.amazonaws.com"}),
		"wrong issuer":   iss.Token(t, map[string]any{"iss": other.URL}),
		"expired":        iss.Token(t, map[string]any{"exp": time.Now().Add(-2 * time.Minute).Unix()}),
		"foreign key":    iss.ForeignToken(t, nil),
		"other issuer":   other.Token(t, map[string]any{"iss": iss.URL}),
		"no jti":         iss.Token(t, map[string]any{"jti": nil}),
		"no repo id":     iss.Token(t, map[string]any{"repository_id": nil}),
		"no owner id":    iss.Token(t, map[string]any{"repository_owner_id": nil}),
		"no repository":  iss.Token(t, map[string]any{"repository": nil}),
	}
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := v.Verify(context.Background(), token)
			if !errors.Is(err, ErrUnauthorized) {
				t.Errorf("err = %v, want ErrUnauthorized", err)
			}
		})
	}
}

func TestVerifyClockSkew(t *testing.T) {
	iss := oidctest.NewIssuer(t)
	v := NewVerifier(iss.URL, "pushbot", rules)
	token := iss.Token(t, map[string]any{"exp": time.Now().Add(-20 * time.Second).Unix()})
	if _, err := v.Verify(context.Background(), token); err != nil {
		t.Errorf("token expired 20s ago rejected: %v", err)
	}
}

func TestVerifyReplay(t *testing.T) {
	iss := oidctest.NewIssuer(t)
	v := NewVerifier(iss.URL, "pushbot", rules)
	token := iss.Token(t, nil)
	if _, err := v.Verify(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	_, err := v.Verify(context.Background(), token)
	if !errors.Is(err, ErrUnauthorized) || !strings.Contains(err.Error(), "already used") {
		t.Errorf("replay err = %v, want ErrUnauthorized (already used)", err)
	}
}

func TestVerifyForbidden(t *testing.T) {
	iss := oidctest.NewIssuer(t)
	v := NewVerifier(iss.URL, "pushbot", rules)
	cases := map[string]map[string]any{
		// Same login, different account (e.g. the name was released and re-registered).
		"owner id mismatch": {"repository_owner_id": "999"},
		"glob mismatch":     {"repository": "org/website", "repository_owner": "org", "repository_owner_id": "77"},
		"unknown owner":     {"repository": "stranger/x", "repository_owner": "stranger", "repository_owner_id": "5"},
	}
	for name, overrides := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := v.Verify(context.Background(), iss.Token(t, overrides))
			if !errors.Is(err, ErrForbidden) {
				t.Errorf("err = %v, want ErrForbidden", err)
			}
		})
	}

	ok := map[string]any{"repository": "org/Homelab-DNS", "repository_owner": "org", "repository_owner_id": "77"}
	if _, err := v.Verify(context.Background(), iss.Token(t, ok)); err != nil {
		t.Errorf("glob match (case-insensitive) rejected: %v", err)
	}
}
