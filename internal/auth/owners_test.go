package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/nikitakiselev/pushbot/internal/state"
)

func githubAPI(t *testing.T, ids map[string]int, fail *atomic.Bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail != nil && fail.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		if r.Header.Get("Authorization") != "Bearer gh-token" {
			http.Error(w, "missing token", http.StatusUnauthorized)
			return
		}
		id, ok := ids[r.URL.Path[len("/users/"):]]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"login":"x","id":` + strconv.Itoa(id) + `}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func noLog(string, ...any) {}

func TestOwnerIDsResolvesAndCaches(t *testing.T) {
	api := githubAPI(t, map[string]int{"nikitakiselev": 42, "org": 77}, nil)
	cache := filepath.Join(t.TempDir(), "owners.json")
	r := &OwnerResolver{APIBase: api.URL, Token: "gh-token"}

	ids, err := OwnerIDs(context.Background(), r, []string{"nikitakiselev", "org"}, cache, 0, noLog)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"nikitakiselev": "42", "org": "77"}
	if !reflect.DeepEqual(ids, want) {
		t.Errorf("ids = %v", ids)
	}
	cached, _ := state.LoadOwners(cache)
	if !reflect.DeepEqual(cached, want) {
		t.Errorf("cache = %v", cached)
	}
}

func TestOwnerIDsFallsBackToCache(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	api := githubAPI(t, nil, &fail)
	cache := filepath.Join(t.TempDir(), "owners.json")
	r := &OwnerResolver{APIBase: api.URL, Token: "gh-token"}

	if _, err := OwnerIDs(context.Background(), r, []string{"nikitakiselev"}, cache, 0, noLog); err == nil {
		t.Fatal("no cache and API down: want error")
	}
	if err := state.SaveOwners(cache, map[string]string{"nikitakiselev": "42"}); err != nil {
		t.Fatal(err)
	}
	ids, err := OwnerIDs(context.Background(), r, []string{"nikitakiselev"}, cache, 0, noLog)
	if err != nil || ids["nikitakiselev"] != "42" {
		t.Fatalf("ids = %v, err = %v", ids, err)
	}
}

func TestOwnerIDsUnknownOwnerIsPermanent(t *testing.T) {
	api := githubAPI(t, map[string]int{}, nil)
	cache := filepath.Join(t.TempDir(), "owners.json")
	// Even a cached ID must not hide a deleted account.
	if err := state.SaveOwners(cache, map[string]string{"ghost": "13"}); err != nil {
		t.Fatal(err)
	}
	r := &OwnerResolver{APIBase: api.URL, Token: "gh-token"}
	if _, err := OwnerIDs(context.Background(), r, []string{"ghost"}, cache, 0, noLog); err == nil {
		t.Fatal("unknown owner: want error")
	}
}
