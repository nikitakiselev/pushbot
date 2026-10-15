package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nikitakiselev/pushbot/internal/auth"
	"github.com/nikitakiselev/pushbot/internal/config"
)

// fakeGitHub answers user lookups for ownerIDs and counts requests per login.
type fakeGitHub struct {
	*httptest.Server
	requests map[string]*atomic.Int32
}

func newFakeGitHub(t *testing.T, ownerIDs map[string]string) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{requests: map[string]*atomic.Int32{}}
	for login := range ownerIDs {
		f.requests[login] = &atomic.Int32{}
	}
	f.requests["nobody"] = &atomic.Int32{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		login := strings.TrimPrefix(r.URL.Path, "/users/")
		if n := f.requests[login]; n != nil {
			n.Add(1)
		}
		id, ok := ownerIDs[login]
		if !ok {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"id": %s}`, id)
	}))
	t.Cleanup(f.Close)
	return f
}

// downGitHub fails every request.
func downGitHub(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(s.Close)
	return s
}

// daemon answers /healthz with ok once healthy is set.
type daemon struct {
	*httptest.Server
	healthy atomic.Bool
}

func newDaemon(t *testing.T, healthy bool) *daemon {
	t.Helper()
	d := &daemon{}
	d.healthy.Store(healthy)
	d.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" || !d.healthy.Load() {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	}))
	t.Cleanup(d.Close)
	return d
}

type env struct {
	opts    Options
	dir     string
	calls   []string
	chowned []string
	active  bool // what systemctl is-active reports
	out     *bytes.Buffer
}

// newEnv describes a fresh server: no config, the GitHub owner alice exists,
// the daemon answers /healthz once started.
func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	gh := newFakeGitHub(t, map[string]string{"alice": "1001"})
	d := newDaemon(t, true)
	e := &env{dir: dir, active: true, out: &bytes.Buffer{}}
	e.opts = Options{
		Path: filepath.Join(dir, "etc", "config.yml"),
		File: config.File{
			Listen:       strings.TrimPrefix(d.URL, "http://"),
			ProjectsRoot: filepath.Join(dir, "apps"),
			Allow:        []string{"alice/*"},
		},
		Owners: &Owners{Resolver: &auth.OwnerResolver{APIBase: gh.URL}},
		Systemctl: func(args ...string) error {
			e.calls = append(e.calls, strings.Join(args, " "))
			if args[0] == "is-active" && !e.active {
				return errors.New("exit status 3")
			}
			return nil
		},
		Chown: func(path string) error {
			e.chowned = append(e.chowned, path)
			return nil
		},
		HealthTimeout: 5 * time.Second,
		Out:           e.out,
	}
	return e
}

func (e *env) run(t *testing.T) error {
	t.Helper()
	return Run(context.Background(), e.opts)
}

func TestRunWritesConfigAndStartsService(t *testing.T) {
	e := newEnv(t)
	if err := e.run(t); err != nil {
		t.Fatalf("%v\n%s", err, e.out)
	}
	cfg, err := config.Load(e.opts.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Allow, []config.AllowRule{{Owner: "alice", RepoGlob: "*"}}) {
		t.Errorf("allow = %+v", cfg.Allow)
	}
	if want := []string{"enable --quiet pushbot", "restart pushbot"}; !reflect.DeepEqual(e.calls, want) {
		t.Errorf("systemctl calls = %q, want %q", e.calls, want)
	}
	if info, err := os.Stat(e.opts.Path); err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("config mode: %v, %v", info, err)
	}
	if _, err := os.Stat(e.opts.Path + ".bak"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a new config must not leave a backup: %v", err)
	}
	if !strings.Contains(e.out.String(), "reverse_proxy "+e.opts.File.Listen) {
		t.Errorf("output has no reverse proxy example for %s:\n%s", e.opts.File.Listen, e.out)
	}
}

func TestRunCreatesProjectsRoot(t *testing.T) {
	e := newEnv(t)
	if err := e.run(t); err != nil {
		t.Fatal(err)
	}
	root := e.opts.File.ProjectsRoot
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o750 {
		t.Fatalf("projects_root: %v, %v", info, err)
	}
	if !reflect.DeepEqual(e.chowned, []string{root}) {
		t.Errorf("chowned %q, want %q", e.chowned, root)
	}
}

// Missing parents must stay traversable for the pushbot user.
func TestRunCreatesProjectsRootParents(t *testing.T) {
	e := newEnv(t)
	e.opts.File.ProjectsRoot = filepath.Join(e.dir, "opt", "pushbot", "apps")
	if err := e.run(t); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{filepath.Join(e.dir, "opt"), filepath.Join(e.dir, "opt", "pushbot")} {
		if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o755 {
			t.Errorf("%s: %v, %v; want mode 0755", dir, info, err)
		}
	}
	if info, err := os.Stat(e.opts.File.ProjectsRoot); err != nil || info.Mode().Perm() != 0o750 {
		t.Errorf("projects_root: %v, %v; want mode 0750", info, err)
	}
}

func TestRunKeepsExistingProjectsRoot(t *testing.T) {
	e := newEnv(t)
	os.MkdirAll(e.opts.File.ProjectsRoot, 0o755)
	if err := e.run(t); err != nil {
		t.Fatal(err)
	}
	if len(e.chowned) != 0 {
		t.Errorf("chowned %q, want nothing: the directory existed", e.chowned)
	}
}

func TestRunBacksUpExistingConfig(t *testing.T) {
	e := newEnv(t)
	os.MkdirAll(filepath.Dir(e.opts.Path), 0o755)
	os.WriteFile(e.opts.Path, []byte("old config\n"), 0o600)
	if err := e.run(t); err != nil {
		t.Fatal(err)
	}
	if bak, err := os.ReadFile(e.opts.Path + ".bak"); err != nil || string(bak) != "old config\n" {
		t.Errorf("backup = %q, %v", bak, err)
	}
	if _, err := config.Load(e.opts.Path); err != nil {
		t.Errorf("new config: %v", err)
	}
}

func TestRunRejectsInvalidConfig(t *testing.T) {
	e := newEnv(t)
	e.opts.File.ProjectsRoot = "apps"
	if err := e.run(t); err == nil || !strings.Contains(err.Error(), "projects_root") {
		t.Fatalf("err = %v, want a projects_root error", err)
	}
	e.assertNothingDone(t)
}

func TestRunRejectsUnknownOwner(t *testing.T) {
	e := newEnv(t)
	e.opts.File.Allow = []string{"alice/*", "nobody/site"}
	if err := e.run(t); err == nil || !strings.Contains(err.Error(), "nobody") {
		t.Fatalf("err = %v, want an error naming the owner", err)
	}
	e.assertNothingDone(t)
}

func (e *env) assertNothingDone(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(e.opts.Path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("config written: %v", err)
	}
	if len(e.calls) != 0 {
		t.Errorf("systemctl called: %q", e.calls)
	}
}

func TestRunWarnsWhenGitHubIsDown(t *testing.T) {
	e := newEnv(t)
	e.opts.Owners = &Owners{Resolver: &auth.OwnerResolver{APIBase: downGitHub(t).URL}}
	if err := e.run(t); err != nil {
		t.Fatalf("%v\n%s", err, e.out)
	}
	if !strings.Contains(e.out.String(), "cannot look up alice") {
		t.Errorf("no warning about alice in the output:\n%s", e.out)
	}
	if _, err := config.Load(e.opts.Path); err != nil {
		t.Errorf("config: %v", err)
	}
}

func TestRunWithoutService(t *testing.T) {
	e := newEnv(t)
	e.opts.Systemctl = nil
	e.opts.File.Listen = "127.0.0.1:1" // nothing listens: Run must not wait for it
	if err := e.run(t); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.out.String(), "pushbot serve --config "+e.opts.Path) {
		t.Errorf("output does not say how to start the daemon:\n%s", e.out)
	}
}

func TestRunFailsWhenDaemonDoesNotAnswer(t *testing.T) {
	e := newEnv(t)
	e.opts.File.Listen = strings.TrimPrefix(newDaemon(t, false).URL, "http://")
	e.opts.HealthTimeout = time.Second
	err := e.run(t)
	if err == nil || !strings.Contains(err.Error(), "journalctl") {
		t.Fatalf("err = %v, want a hint to read the log", err)
	}
}

func TestRunWaitsForDaemonToStart(t *testing.T) {
	e := newEnv(t)
	d := newDaemon(t, false)
	e.opts.File.Listen = strings.TrimPrefix(d.URL, "http://")
	time.AfterFunc(700*time.Millisecond, func() { d.healthy.Store(true) })
	if err := e.run(t); err != nil {
		t.Fatalf("%v\n%s", err, e.out)
	}
}

func TestRunStopsWaitingWhenServiceStops(t *testing.T) {
	e := newEnv(t)
	e.opts.File.Listen = strings.TrimPrefix(newDaemon(t, false).URL, "http://")
	e.opts.HealthTimeout = time.Minute
	e.active = false
	start := time.Now()
	err := e.run(t)
	if err == nil || !strings.Contains(err.Error(), "journalctl") {
		t.Fatalf("err = %v, want a hint to read the log", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("Run waited %s for a stopped service", elapsed)
	}
}

func TestOwnersCheck(t *testing.T) {
	gh := newFakeGitHub(t, map[string]string{"alice": "1001", "my-org": "2002"})
	owners := &Owners{Resolver: &auth.OwnerResolver{APIBase: gh.URL}}
	ctx := context.Background()

	got, err := owners.Check(ctx, []string{"alice/*", "My-Org/homelab-*", "my-org/site"})
	if err != nil {
		t.Fatal(err)
	}
	want := []Lookup{{Owner: "alice", ID: "1001"}, {Owner: "my-org", ID: "2002"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if _, err := owners.Check(ctx, []string{"alice/*", "my-org/site"}); err != nil {
		t.Fatal(err)
	}
	for login, n := range map[string]int32{"alice": 1, "my-org": 1} {
		if got := gh.requests[login].Load(); got != n {
			t.Errorf("%s looked up %d times, want %d", login, got, n)
		}
	}

	for _, patterns := range [][]string{{"alice"}, {"nobody/*"}, {}} {
		if _, err := owners.Check(ctx, patterns); err == nil {
			t.Errorf("Check(%q) succeeded, want error", patterns)
		}
	}
}

// A GitHub that does not answer is asked once: every wait freezes the wizard.
func TestOwnersCheckRemembersFailures(t *testing.T) {
	var requests atomic.Int32
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(gh.Close)
	owners := &Owners{Resolver: &auth.OwnerResolver{APIBase: gh.URL}}
	for range 2 {
		lookups, err := owners.Check(context.Background(), []string{"alice/*"})
		if err != nil || len(lookups) != 1 || lookups[0].Err == nil {
			t.Fatalf("lookups = %+v, err = %v; want one failed lookup", lookups, err)
		}
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("GitHub asked %d times, want 1", n)
	}
}
