package doctor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeGitHub serves OIDC keys, user lookups and the latest release.
func fakeGitHub(t *testing.T, ownerIDs map[string]string, latest string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/jwks", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"keys": [{"kty": "RSA", "kid": "1"}]}`)
	})
	mux.HandleFunc("GET /users/{login}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := ownerIDs[r.PathValue("login")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"id": %s}`, id)
	})
	mux.HandleFunc("GET /repos/me/pushbot/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name": %q}`, latest)
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// fakeDaemon answers /healthz and /metrics like a running pushbot.
func fakeDaemon(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "pushbot_build_info 1") })
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

type setup struct {
	opts       Options
	stateDir   string
	projects   string
	service    map[string]string // systemctl verb → output
	daemonUser func(name string, args ...string) (string, error)
}

// newSetup describes a healthy server: every check passes.
func newSetup(t *testing.T) *setup {
	t.Helper()
	gh := fakeGitHub(t, map[string]string{"me": "100"}, "v1.2.0")
	daemon := fakeDaemon(t)
	dir := t.TempDir()
	s := &setup{
		stateDir: filepath.Join(dir, "state"),
		projects: filepath.Join(dir, "apps"),
		service:  map[string]string{"is-enabled": "enabled", "is-active": "active"},
		daemonUser: func(name string, args ...string) (string, error) {
			switch strings.Join(append([]string{name}, args...), " ") {
			case "docker version --format {{.Server.Version}}":
				return "27.3.1", nil
			case "docker compose version --short":
				return "2.29.7", nil
			}
			if name == "test" {
				return "", nil
			}
			return "", fmt.Errorf("unexpected command %s %q", name, args)
		},
	}
	os.MkdirAll(s.stateDir, 0o750)
	os.MkdirAll(s.projects, 0o750)
	configPath := filepath.Join(dir, "config.yml")
	config := fmt.Sprintf("listen: %s\nmetrics_listen: %s\nprojects_root: %s\nstate_dir: %s\noidc_issuer: %s\nallow:\n  - me/*\n  - me/homelab-*\n",
		strings.TrimPrefix(daemon.URL, "http://"), strings.TrimPrefix(daemon.URL, "http://"), s.projects, s.stateDir, gh.URL)
	if err := os.WriteFile(configPath, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	s.opts = Options{
		ConfigPath: configPath,
		URL:        daemon.URL,
		Version:    "1.2.0",
		Repo:       "me/pushbot",
		APIBase:    gh.URL,
		Systemctl: func(args ...string) (string, error) {
			return s.service[args[0]], nil
		},
		AsDaemonUser: func(name string, args ...string) (string, error) { return s.daemonUser(name, args...) },
	}
	return s
}

func (s *setup) run(t *testing.T) ([]Result, int) {
	t.Helper()
	var results []Result
	failures := Run(context.Background(), s.opts, func(r Result) { results = append(results, r) })
	for _, r := range results {
		t.Logf("%d %s | %s", r.Status, r.Text, r.Hint)
	}
	return results, failures
}

// find returns the result whose text contains substr.
func find(t *testing.T, results []Result, substr string) Result {
	t.Helper()
	for _, r := range results {
		if strings.Contains(r.Text, substr) {
			return r
		}
	}
	t.Fatalf("no result mentions %q", substr)
	return Result{}
}

func TestHealthyServer(t *testing.T) {
	s := newSetup(t)
	results, failures := s.run(t)
	if failures != 0 {
		t.Errorf("failures = %d", failures)
	}
	for _, r := range results {
		if r.Status != OK {
			t.Errorf("not OK: %s", r.Text)
		}
	}
	for _, want := range []string{
		"config " + s.opts.ConfigPath + " is valid",
		"service pushbot is active and enabled",
		"answers /healthz",
		"answer /metrics",
		"/healthz answers through the proxy",
		"user pushbot can run docker (Docker 27.3.1, Compose 2.29.7)",
		s.projects + " and " + s.stateDir + " are writable by user pushbot",
		"GitHub OIDC keys are reachable",
		"allow me/*, me/homelab-* → owner id 100",
		"clock matches GitHub",
		"pushbot 1.2.0 is the latest release",
	} {
		find(t, results, want)
	}
}

func TestServiceStates(t *testing.T) {
	for _, tc := range []struct {
		enabled, active string
		status          Status
		text, hint      string
	}{
		{"enabled", "inactive", Fail, "is not running", "sudo systemctl start pushbot"},
		{"enabled", "failed", Fail, "service pushbot is failed", "journalctl -u pushbot"},
		{"disabled", "active", Warn, "will not start after a reboot", "sudo systemctl enable pushbot"},
		{"", "inactive", Fail, "pushbot.service is not installed", "install command"},
	} {
		s := newSetup(t)
		s.service = map[string]string{"is-enabled": tc.enabled, "is-active": tc.active}
		results, failures := s.run(t)
		r := find(t, results, tc.text)
		if r.Status != tc.status || !strings.Contains(r.Hint, tc.hint) {
			t.Errorf("%s/%s: got %+v", tc.enabled, tc.active, r)
		}
		// A stopped service is one problem, not three: the daemon checks are skipped.
		if tc.status == Fail && failures != 1 {
			t.Errorf("%s/%s: failures = %d, want 1", tc.enabled, tc.active, failures)
		}
	}
}

func TestDaemonDown(t *testing.T) {
	s := newSetup(t)
	s.opts.URL = "http://127.0.0.1:1"
	config, _ := os.ReadFile(s.opts.ConfigPath)
	lines := strings.Split(string(config), "\n")
	lines[0], lines[1] = "listen: 127.0.0.1:1", ""
	os.WriteFile(s.opts.ConfigPath, []byte(strings.Join(lines, "\n")), 0o644)

	results, failures := s.run(t)
	if failures != 2 {
		t.Errorf("failures = %d, want 2 (daemon and URL)", failures)
	}
	if r := find(t, results, "daemon on 127.0.0.1:1 does not answer /healthz"); !strings.Contains(r.Text, "connection refused") {
		t.Errorf("no cause in %q", r.Text)
	}
	if r := find(t, results, "http://127.0.0.1:1/healthz"); r.Status != Fail || !strings.Contains(r.Hint, "Reverse proxy") {
		t.Errorf("got %+v", r)
	}
}

func TestDockerProblems(t *testing.T) {
	for _, tc := range []struct {
		out, text, hint string
		err             error
	}{
		{"", "docker is not installed", "docs.docker.com", exec.ErrNotFound},
		{"", "user pushbot does not exist", "install command", ErrNoDaemonUser},
		{"permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock",
			"user pushbot has no access to the Docker socket", "docker group", errors.New("exit status 1")},
		{"Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?",
			"the Docker daemon is not running", "systemctl start docker", errors.New("exit status 1")},
	} {
		s := newSetup(t)
		s.daemonUser = func(name string, args ...string) (string, error) { return tc.out, tc.err }
		results, _ := s.run(t)
		if r := find(t, results, tc.text); r.Status != Fail || !strings.Contains(r.Hint, tc.hint) {
			t.Errorf("%v: got %+v", tc.err, r)
		}
	}
}

func TestComposeMissing(t *testing.T) {
	s := newSetup(t)
	healthy := s.daemonUser
	s.daemonUser = func(name string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "compose" {
			return "docker: unknown command: docker compose", errors.New("exit status 1")
		}
		return healthy(name, args...)
	}
	results, _ := s.run(t)
	if r := find(t, results, "docker compose does not work"); r.Status != Fail || !strings.Contains(r.Hint, "docker-compose-plugin") {
		t.Errorf("got %+v", r)
	}
}

func TestDirectories(t *testing.T) {
	s := newSetup(t)
	os.Remove(s.projects)
	healthy := s.daemonUser
	s.daemonUser = func(name string, args ...string) (string, error) {
		if name == "test" && args[1] == s.stateDir {
			return "", errors.New("exit status 1")
		}
		return healthy(name, args...)
	}
	results, failures := s.run(t)
	if failures != 2 {
		t.Errorf("failures = %d, want 2", failures)
	}
	if r := find(t, results, s.projects+" does not exist"); !strings.Contains(r.Hint, "install -d -o pushbot") {
		t.Errorf("got %+v", r)
	}
	if r := find(t, results, s.stateDir+" is not writable"); !strings.Contains(r.Hint, "chown pushbot:pushbot "+s.stateDir) {
		t.Errorf("got %+v", r)
	}
}

func TestOwners(t *testing.T) {
	s := newSetup(t)
	gh := fakeGitHub(t, map[string]string{"me": "100", "org": "300"}, "v1.2.0")
	s.opts.APIBase = gh.URL
	config, _ := os.ReadFile(s.opts.ConfigPath)
	config = []byte(strings.Replace(string(config), "  - me/homelab-*\n", "  - org/*\n  - ghost/*\n", 1))
	os.WriteFile(s.opts.ConfigPath, config, 0o644)
	// The daemon saved org with another ID: the login changed hands since.
	os.WriteFile(filepath.Join(s.stateDir, "owners.json"), []byte(`{"me": "100", "org": "200"}`), 0o600)

	results, failures := s.run(t)
	if failures != 2 {
		t.Errorf("failures = %d, want 2", failures)
	}
	if r := find(t, results, "allow me/*"); r.Status != OK {
		t.Errorf("got %+v", r)
	}
	if r := find(t, results, "allow org/*"); r.Status != Fail || !strings.Contains(r.Text, "owner id is now 300, pushbot trusts 200") {
		t.Errorf("got %+v", r)
	}
	if r := find(t, results, "allow ghost/*"); r.Status != Fail || !strings.Contains(r.Text, "no user or organization ghost") {
		t.Errorf("got %+v", r)
	}
}

func TestClock(t *testing.T) {
	for _, tc := range []struct {
		skew   time.Duration
		status Status
		text   string
	}{
		{0, OK, "clock matches GitHub"},
		{30 * time.Second, Warn, "clock is 30s ahead of GitHub"},
		{-3 * time.Minute, Fail, "clock is 3m0s behind GitHub, deploy tokens will be rejected"},
	} {
		s := newSetup(t)
		s.opts.Now = func() time.Time { return time.Now().Add(tc.skew) }
		results, _ := s.run(t)
		if r := find(t, results, "clock"); r.Status != tc.status || r.Text != tc.text {
			t.Errorf("skew %s: got %+v", tc.skew, r)
		}
	}
}

func TestOutdatedVersion(t *testing.T) {
	s := newSetup(t)
	s.opts.Version = "1.1.0"
	results, failures := s.run(t)
	if failures != 0 {
		t.Errorf("an old version is a warning, got %d failures", failures)
	}
	if r := find(t, results, "pushbot 1.1.0, latest is 1.2.0"); r.Status != Warn || r.Hint != "sudo pushbot upgrade" {
		t.Errorf("got %+v", r)
	}
}

func TestBrokenConfig(t *testing.T) {
	s := newSetup(t)
	os.WriteFile(s.opts.ConfigPath, []byte("listen: [\n"), 0o644)
	s.opts.URL = ""
	results, failures := s.run(t)
	if failures != 1 || results[0].Status != Fail || !strings.HasPrefix(results[0].Text, "config: ") {
		t.Errorf("failures = %d, first = %+v", failures, results[0])
	}
	// Checks that do not need the config still run.
	find(t, results, "service pushbot is active")
	find(t, results, "user pushbot can run docker")
}
