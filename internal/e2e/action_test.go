// Package e2e runs the real action.sh against an in-process pushbot server and a
// fake OIDC issuer.
package e2e

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nikitakiselev/pushbot/internal/auth"
	"github.com/nikitakiselev/pushbot/internal/deploy"
	"github.com/nikitakiselev/pushbot/internal/oidctest"
	"github.com/nikitakiselev/pushbot/internal/server"
)

type harness struct {
	issuer    *oidctest.Issuer
	url       string
	root      string
	workspace string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	for _, bin := range []string{"bash", "yq", "jq", "curl", "tar"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
	iss := oidctest.NewIssuer(t)
	base := t.TempDir()
	d, err := deploy.New(deploy.Config{
		ProjectsRoot: filepath.Join(base, "apps"),
		StateDir:     filepath.Join(base, "state"),
		Timeout:      30 * time.Second,
		MaxUnpacked:  10 << 20,
		LogRetention: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	v := auth.NewVerifier(iss.URL, "pushbot", []auth.Rule{{Owner: "nikitakiselev", OwnerID: "42", RepoGlob: "*"}})
	ts := httptest.NewServer(server.New(v, d, 10<<20, filepath.Join(base, "state", "tmp"), nil).Handler())
	t.Cleanup(ts.Close)
	return &harness{issuer: iss, url: ts.URL, root: filepath.Join(base, "apps"), workspace: t.TempDir()}
}

func (h *harness) write(t *testing.T, name, content string) {
	t.Helper()
	p := filepath.Join(h.workspace, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// action runs action.sh like the composite action does and returns its output.
func (h *harness) action(t *testing.T, extraEnv ...string) (string, error) {
	t.Helper()
	script, err := filepath.Abs("../../action.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", script)
	cmd.Dir = h.workspace
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"PUSHBOT_URL=" + h.url + "/",
		"PUSHBOT_ENV=IMAGE_TAG=v1\nSECRET=hunter22",
		"PUSHBOT_CONFIG=pushbot.yml",
		"PUSHBOT_REGISTRY_AUTH=true",
		"PUSHBOT_REGISTRY_USER=nikitakiselev",
		"PUSHBOT_REGISTRY_TOKEN=ghs_testtoken",
		"PUSHBOT_AUDIENCE=pushbot",
		"ACTIONS_ID_TOKEN_REQUEST_URL=" + h.issuer.ActionsTokenURL(),
		"ACTIONS_ID_TOKEN_REQUEST_TOKEN=" + oidctest.ActionsRequestToken,
	}, extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestActionDeploys(t *testing.T) {
	h := newHarness(t)
	h.write(t, "pushbot.yml", `
files:
  - docker-compose.yml
  - config/
  - deploy/prod with space.yml: compose.override.yml
steps:
  - run: echo "tag=$IMAGE_TAG"
  - run: test -s "$DOCKER_CONFIG/config.json" && echo registry-ok
`)
	h.write(t, "docker-compose.yml", "services: {}\n")
	h.write(t, "config/app.conf", "a=1\n")
	h.write(t, "deploy/prod with space.yml", "prod\n")
	h.write(t, "not-listed.txt", "must stay on the runner\n")

	out, err := h.action(t)
	if err != nil {
		t.Fatalf("action failed: %v\n%s", err, out)
	}
	t.Logf("action output:\n%s", out)
	for _, want := range []string{"(3 files)", "tag=v1\n", "registry-ok\n", "PUSHBOT: success ("} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	dir := filepath.Join(h.root, "my-service")
	for name, want := range map[string]string{
		"docker-compose.yml":   "services: {}\n",
		"config/app.conf":      "a=1\n",
		"compose.override.yml": "prod\n",
		".env":                 "IMAGE_TAG='v1'\nSECRET='hunter22'\n",
	} {
		if got, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", name, got, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "not-listed.txt")); err == nil {
		t.Error("a file not listed in files was deployed")
	}
}

func TestActionWithoutFilesOrRegistry(t *testing.T) {
	h := newHarness(t)
	h.write(t, "pushbot.yml", "steps:\n  - run: echo \"docker_config=${DOCKER_CONFIG:-unset}\"\n")
	out, err := h.action(t, "PUSHBOT_REGISTRY_AUTH=false")
	if err != nil {
		t.Fatalf("action failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "docker_config=unset") {
		t.Errorf("output:\n%s", out)
	}
}

func TestActionFails(t *testing.T) {
	cases := []struct {
		name     string
		manifest string
		claims   map[string]any
		env      []string
		want     string
	}{
		{
			name:     "step fails",
			manifest: "steps:\n  - run: exit 4\n",
			want:     "::error::pushbot: failed: step 1/1 exited with code 4",
		},
		{
			name:     "repository not allowed",
			manifest: "steps:\n  - run: 'true'\n",
			claims:   map[string]any{"repository": "stranger/x", "repository_owner_id": "9"},
			want:     "HTTP 403",
		},
		{
			name:     "listed file missing",
			manifest: "files: [missing.yml]\n",
			want:     "missing.yml",
		},
		{
			name:     "no id-token permission",
			manifest: "steps:\n  - run: 'true'\n",
			env:      []string{"ACTIONS_ID_TOKEN_REQUEST_URL="},
			want:     "id-token: write",
		},
		{
			name:     "invalid manifest",
			manifest: "steps: []\n",
			want:     "HTTP 400",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.write(t, "pushbot.yml", c.manifest)
			h.issuer.SetActionsClaims(c.claims)
			out, err := h.action(t, c.env...)
			if err == nil {
				t.Fatalf("action succeeded, want failure:\n%s", out)
			}
			if !strings.Contains(out, c.want) {
				t.Errorf("output lacks %q:\n%s", c.want, out)
			}
		})
	}
}
