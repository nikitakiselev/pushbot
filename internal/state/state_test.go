package state

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestClaim(t *testing.T) {
	file := filepath.Join(t.TempDir(), "projects.json")
	p := NewProjects(file)
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

	isNew, err := p.Claim("my-service", "111", "me/my-service", now)
	if err != nil || !isNew {
		t.Fatalf("first claim: new=%v err=%v", isNew, err)
	}
	isNew, err = p.Claim("my-service", "111", "me/my-service-renamed", now.Add(time.Hour))
	if err != nil || isNew {
		t.Fatalf("repeat claim by same repo: new=%v err=%v", isNew, err)
	}
	_, err = p.Claim("my-service", "222", "someone/my-service", now)
	if !errors.Is(err, ErrNameTaken) {
		t.Fatalf("claim by other repo: err=%v, want ErrNameTaken", err)
	}

	// State survives a new instance (i.e. a daemon restart).
	_, err = NewProjects(file).Claim("my-service", "222", "someone/my-service", now)
	if !errors.Is(err, ErrNameTaken) {
		t.Fatalf("after reload: err=%v, want ErrNameTaken", err)
	}
	got, err := NewProjects(file).Get("my-service")
	if err != nil {
		t.Fatal(err)
	}
	want := Project{RepositoryID: "111", Repository: "me/my-service", ClaimedAt: now}
	if !reflect.DeepEqual(got, &want) {
		t.Errorf("Get = %+v, want %+v", got, want)
	}
}

func TestClaimCorruptFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "projects.json")
	if err := os.WriteFile(file, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewProjects(file).Claim("x", "1", "a/x", time.Now()); err == nil {
		t.Fatal("Claim on corrupt file succeeded, want error")
	}
}

func TestOwners(t *testing.T) {
	file := filepath.Join(t.TempDir(), "owners.json")
	got, err := LoadOwners(file)
	if err != nil || len(got) != 0 {
		t.Fatalf("missing file: %v %v", got, err)
	}
	want := map[string]string{"nikitakiselev": "123", "org": "456"}
	if err := SaveOwners(file, want); err != nil {
		t.Fatal(err)
	}
	got, err = LoadOwners(file)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("LoadOwners = %v, %v", got, err)
	}
}
