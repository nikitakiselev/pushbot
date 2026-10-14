package deploy

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nikitakiselev/pushbot/internal/auth"
	"github.com/nikitakiselev/pushbot/internal/envfile"
	"github.com/nikitakiselev/pushbot/internal/manifest"
	"github.com/nikitakiselev/pushbot/internal/runner"
)

type env struct {
	root, state string
	d           *Deployer
}

func newEnv(t *testing.T, mutate ...func(*Config)) *env {
	t.Helper()
	base := t.TempDir()
	cfg := Config{
		ProjectsRoot: filepath.Join(base, "apps"),
		StateDir:     filepath.Join(base, "state"),
		Timeout:      10 * time.Second,
		MaxUnpacked:  1 << 20,
		LogRetention: 20,
		Runner:       runner.Options{KillGrace: 100 * time.Millisecond},
	}
	for _, m := range mutate {
		m(&cfg)
	}
	d, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &env{root: cfg.ProjectsRoot, state: cfg.StateDir, d: d}
}

func claims(repo, repoID string) *auth.Claims {
	return &auth.Claims{
		Repository: repo, RepositoryID: repoID, RepositoryOwnerID: "42",
		Ref: "refs/heads/main", SHA: "a1b2c3d4e5f6", Actor: "me", RunID: "77",
	}
}

// writeBundle writes a tar.gz with the given files (name → content) and returns its path.
func writeBundle(t *testing.T, files map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	p := filepath.Join(t.TempDir(), "bundle.tgz")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func parse(t *testing.T, doc string) *manifest.Manifest {
	t.Helper()
	m, err := manifest.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func run(t *testing.T, e *env, req Request) (Result, string) {
	t.Helper()
	job, err := e.d.Prepare(req)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	res := job.Run(&buf)
	return res, buf.String()
}

func TestDeploySuccess(t *testing.T) {
	e := newEnv(t)
	bundle := writeBundle(t, map[string]string{
		"docker-compose.yml":      "services: {}\n",
		"config/app.conf":         "a=1\n",
		"config/nested/x.conf":    "x\n",
		"deploy/compose.prod.yml": "prod\n",
	})
	m := parse(t, `
files:
  - docker-compose.yml
  - config/
  - deploy/compose.prod.yml: compose.override.yml
steps:
  - run: echo "tag=$IMAGE_TAG secret=$DB_PASSWORD"
  - run: |
      test -f config/nested/x.conf
      echo "$PUSHBOT_PROJECT $PUSHBOT_SHA $PUSHBOT_RUN_URL"
      echo "docker_config=${DOCKER_CONFIG:-unset}"
`)
	vars := []envfile.Var{{Key: "IMAGE_TAG", Value: "v1"}, {Key: "DB_PASSWORD", Value: "hunter22"}}
	res, out := run(t, e, Request{Claims: claims("nikitakiselev/My-Service", "1001"), Manifest: m, Env: vars, BundlePath: bundle})

	if !res.Success {
		t.Fatalf("deploy failed: %s\n%s", res.Reason, out)
	}
	dir := filepath.Join(e.root, "my-service")
	for _, want := range []string{
		"==> nikitakiselev/My-Service@a1b2c3d (run #77) → " + dir,
		"==> files: docker-compose.yml, config/, deploy/compose.prod.yml → compose.override.yml (4 files)",
		"==> .env: IMAGE_TAG, DB_PASSWORD",
		`==> [1/2] echo "tag=$IMAGE_TAG secret=$DB_PASSWORD"`,
		"tag=v1 secret=hunter22",
		"==> [2/2] test -f config/nested/x.conf …",
		"my-service a1b2c3d4e5f6 https://github.com/nikitakiselev/My-Service/actions/runs/77",
		"docker_config=unset",
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if last := lines[len(lines)-1]; !strings.HasPrefix(last, "PUSHBOT: success (") {
		t.Errorf("last line = %q", last)
	}

	for name, want := range map[string]string{
		"docker-compose.yml":   "services: {}\n",
		"config/app.conf":      "a=1\n",
		"config/nested/x.conf": "x\n",
		"compose.override.yml": "prod\n",
		".env":                 "IMAGE_TAG='v1'\nDB_PASSWORD='hunter22'\n",
	} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", name, got, err, want)
		}
	}

	logs, _ := filepath.Glob(filepath.Join(e.state, "logs", "my-service", "*-a1b2c3d.log"))
	if len(logs) != 1 {
		t.Fatalf("log files = %v", logs)
	}
	logData, _ := os.ReadFile(logs[0])
	if strings.Contains(string(logData), "hunter22") || !strings.Contains(string(logData), "secret=***") {
		t.Errorf("server log is not masked:\n%s", logData)
	}
	if entries, _ := os.ReadDir(filepath.Join(e.state, "tmp")); len(entries) != 0 {
		t.Errorf("tmp not cleaned: %v", entries)
	}
}

func TestDeployDefaultSteps(t *testing.T) {
	m := parse(t, `name: x`)
	if len(m.Steps) != 2 || m.Steps[1].Run != "docker compose up -d --remove-orphans" {
		t.Fatalf("default steps = %+v", m.Steps)
	}
}

func TestDeployStepFailure(t *testing.T) {
	e := newEnv(t)
	m := parse(t, "steps:\n  - run: echo first\n  - run: exit 3\n  - run: echo never")
	res, out := run(t, e, Request{Claims: claims("me/app", "1"), Manifest: m})
	if res.Success || res.Reason != "step 2/3 exited with code 3" {
		t.Errorf("result = %+v", res)
	}
	if strings.Contains(out, "never") || !strings.HasSuffix(out, "PUSHBOT: failed: step 2/3 exited with code 3\n") {
		t.Errorf("output:\n%s", out)
	}
}

func TestDeployTimeout(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Timeout = 300 * time.Millisecond })
	m := parse(t, "steps:\n  - run: sleep 10")
	start := time.Now()
	res, out := run(t, e, Request{Claims: claims("me/app", "1"), Manifest: m})
	if res.Success || res.Reason != "timeout" || !strings.HasSuffix(out, "PUSHBOT: failed: timeout\n") {
		t.Errorf("result = %+v\n%s", res, out)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("timeout took %v", time.Since(start))
	}
}

func TestDeployKeepsUnlistedFiles(t *testing.T) {
	e := newEnv(t)
	dir := filepath.Join(e.root, "app")
	os.MkdirAll(filepath.Join(dir, "data"), 0o755)
	os.WriteFile(filepath.Join(dir, "data", "db.sqlite"), []byte("precious"), 0o644)
	os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte("old"), 0o644)

	m := parse(t, "files: [docker-compose.yml]\nsteps:\n  - run: 'true'")
	bundle := writeBundle(t, map[string]string{"docker-compose.yml": "new"})
	res, out := run(t, e, Request{Claims: claims("me/app", "1"), Manifest: m, BundlePath: bundle})
	if !res.Success {
		t.Fatalf("deploy failed: %s\n%s", res.Reason, out)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "data", "db.sqlite")); string(got) != "precious" {
		t.Errorf("unlisted file changed: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "docker-compose.yml")); string(got) != "new" {
		t.Errorf("listed file not replaced: %q", got)
	}
	if !strings.Contains(out, "already existed") {
		t.Errorf("no warning about existing directory:\n%s", out)
	}
}

func TestPrepareErrors(t *testing.T) {
	e := newEnv(t)
	bundle := writeBundle(t, map[string]string{"a.yml": "a", "deploy/.env": "X=1", "deploy/x": "x"})
	cases := map[string]Request{
		"missing file":     {Claims: claims("me/app", "1"), Manifest: parse(t, "files: [missing.yml]"), BundlePath: bundle},
		"no bundle":        {Claims: claims("me/app", "1"), Manifest: parse(t, "files: [a.yml]")},
		"file to dot":      {Claims: claims("me/app", "1"), Manifest: parse(t, "files:\n  - a.yml: ."), BundlePath: bundle},
		".env via dir":     {Claims: claims("me/app", "1"), Manifest: parse(t, "files:\n  - deploy: ."), BundlePath: bundle},
		"bad default name": {Claims: claims("me/.github", "1"), Manifest: parse(t, "")},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := e.d.Prepare(req)
			if !errors.Is(err, ErrBadRequest) {
				t.Errorf("err = %v, want ErrBadRequest", err)
			}
		})
	}
	// A rejected request must not claim the name.
	if p, _ := e.d.projects.Get("app"); p != nil {
		t.Errorf("name claimed by a rejected request: %+v", p)
	}
}

func TestPrepareNameTaken(t *testing.T) {
	e := newEnv(t)
	m := parse(t, "name: shared\nsteps:\n  - run: 'true'")
	if res, out := run(t, e, Request{Claims: claims("me/one", "1"), Manifest: m}); !res.Success {
		t.Fatalf("first deploy failed: %s", out)
	}
	_, err := e.d.Prepare(Request{Claims: claims("me/two", "2"), Manifest: m})
	if !errors.Is(err, ErrForbidden) {
		t.Errorf("err = %v, want ErrForbidden", err)
	}
}

func TestDeploySerializesProject(t *testing.T) {
	e := newEnv(t)
	trace := filepath.Join(t.TempDir(), "trace")
	m := parse(t, "steps:\n  - run: echo start >> "+trace+"; sleep 0.3; echo end >> "+trace)

	jobs := make([]*Job, 2)
	for i := range jobs {
		j, err := e.d.Prepare(Request{Claims: claims("me/app", "1"), Manifest: m})
		if err != nil {
			t.Fatal(err)
		}
		jobs[i] = j
	}
	outs := make([]bytes.Buffer, 2)
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func() { defer wg.Done(); j.Run(&outs[i]) }()
		time.Sleep(50 * time.Millisecond)
	}
	wg.Wait()

	got, _ := os.ReadFile(trace)
	if string(got) != "start\nend\nstart\nend\n" {
		t.Errorf("deploys overlapped: %q", got)
	}
	if !strings.Contains(outs[1].String(), "==> waiting for previous deploy...") {
		t.Errorf("second deploy did not report waiting:\n%s", outs[1].String())
	}
}

type brokenWriter struct{ n int }

func (b *brokenWriter) Write(p []byte) (int, error) {
	if b.n++; b.n > 1 {
		return 0, errors.New("client gone")
	}
	return len(p), nil
}

func TestDeployContinuesWhenClientDisconnects(t *testing.T) {
	e := newEnv(t)
	m := parse(t, "steps:\n  - run: echo working\n  - run: touch done")
	job, err := e.d.Prepare(Request{Claims: claims("me/app", "1"), Manifest: m})
	if err != nil {
		t.Fatal(err)
	}
	if res := job.Run(&brokenWriter{}); !res.Success {
		t.Fatalf("deploy failed: %s", res.Reason)
	}
	if _, err := os.Stat(filepath.Join(e.root, "app", "done")); err != nil {
		t.Error("deploy stopped after client disconnect")
	}
	logs, _ := filepath.Glob(filepath.Join(e.state, "logs", "app", "*.log"))
	data, _ := os.ReadFile(logs[0])
	if !strings.Contains(string(data), "working") || !strings.Contains(string(data), "PUSHBOT: success") {
		t.Errorf("server log incomplete:\n%s", data)
	}
}

func TestDeployRegistryAuth(t *testing.T) {
	e := newEnv(t)
	m := parse(t, `steps:
  - run: cat "$DOCKER_CONFIG/config.json"; echo; echo "$DOCKER_CONFIG" > dockerdir`)
	res, out := run(t, e, Request{Claims: claims("me/app", "1"), Manifest: m, Registry: &Registry{User: "me", Token: "ghs_x"}})
	if !res.Success {
		t.Fatalf("deploy failed: %s\n%s", res.Reason, out)
	}
	if want := base64.StdEncoding.EncodeToString([]byte("me:ghs_x")); !strings.Contains(out, want) {
		t.Errorf("docker config lacks auth:\n%s", out)
	}
	dockerDir, _ := os.ReadFile(filepath.Join(e.root, "app", "dockerdir"))
	if _, err := os.Stat(strings.TrimSpace(string(dockerDir))); err == nil {
		t.Error("temporary DOCKER_CONFIG was not removed")
	}
}

func TestDeployLogRetention(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.LogRetention = 2 })
	m := parse(t, "steps:\n  - run: 'true'")
	for i := 0; i < 3; i++ {
		run(t, e, Request{Claims: claims("me/app", "1"), Manifest: m})
	}
	logs, _ := filepath.Glob(filepath.Join(e.state, "logs", "app", "*.log"))
	if len(logs) != 2 {
		t.Errorf("log files = %d, want 2", len(logs))
	}
}
