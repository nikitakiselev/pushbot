// Package e2e runs the real action.sh against an in-process pushbot server and a
// fake OIDC issuer.
package e2e

import (
	"io"
	"net/http"
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
	for _, bin := range []string{"bash", "yq", "jq", "curl", "tar", "base64"} {
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

	out, err := h.action(t, "PUSHBOT_VERBOSE=true")
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
		".env":                 "# pushbot: env input\nIMAGE_TAG='v1'\nSECRET='hunter22'\n",
	} {
		if got, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", name, got, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "not-listed.txt")); err == nil {
		t.Error("a file not listed in files was deployed")
	}
}

// secretsJSON emulates ${{ toJSON(secrets) }}: GitHub upper-cases secret names.
const secretsJSON = `{"github_token": "ghs_job", "DB_PASSWORD": "hunter22", "STAGING_ONLY": "s3cret", "UNUSED": "x"}`

func TestActionEnvSources(t *testing.T) {
	h := newHarness(t)
	h.write(t, "pushbot.yml", `
files: [docker-compose.yml]
env:
  file: deploy/production.env
  secrets: [db_password]
steps:
  - run: echo "tag=$IMAGE_TAG"
`)
	h.write(t, "docker-compose.yml", "services: {}\n")
	h.write(t, "deploy/production.env", "APP_NAME=\"mBlog\"\n")

	out, err := h.action(t, "PUSHBOT_ENV=IMAGE_TAG=v1", "PUSHBOT_SECRETS="+secretsJSON)
	if err != nil {
		t.Fatalf("action failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "pushbot: secrets db_password\n") {
		t.Errorf("output lacks the secrets line:\n%s", out)
	}
	if strings.Contains(out, "pushbot: environment") {
		t.Errorf("output mentions an environment for a job without one:\n%s", out)
	}
	want := "# pushbot: env input\nIMAGE_TAG='v1'\n" +
		"# pushbot: secrets\ndb_password='hunter22'\n" +
		"# pushbot: deploy/production.env\nAPP_NAME=\"mBlog\"\n"
	if got, _ := os.ReadFile(filepath.Join(h.root, "my-service", ".env")); string(got) != want {
		t.Errorf(".env = %q, want %q", got, want)
	}
}

func TestActionEnvironment(t *testing.T) {
	h := newHarness(t)
	h.write(t, "pushbot.yml", `
files: [docker-compose.yml]
env:
  file: deploy/production.env
  secrets: [DB_PASSWORD]
steps:
  - run: echo "environment=$PUSHBOT_ENVIRONMENT"
environments:
  production: {}
  Staging:
    name: my-service-staging
    files: [compose.staging.yml]
    env:
      file: deploy/staging.env
      secrets: [STAGING_ONLY]
`)
	h.write(t, "compose.staging.yml", "services: {}\n")
	h.write(t, "deploy/staging.env", "APP_ENV=staging\n")
	h.issuer.SetActionsClaims(map[string]any{"environment": "staging"})

	out, err := h.action(t, "PUSHBOT_ENV=", "PUSHBOT_SECRETS="+secretsJSON, "PUSHBOT_VERBOSE=true")
	if err != nil {
		t.Fatalf("action failed: %v\n%s", err, out)
	}
	for _, want := range []string{"pushbot: environment staging\n", "pushbot: secrets STAGING_ONLY\n", "environment=staging\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	dir := filepath.Join(h.root, "my-service-staging")
	want := "# pushbot: secrets\nSTAGING_ONLY='s3cret'\n# pushbot: deploy/staging.env\nAPP_ENV=staging\n"
	if got, _ := os.ReadFile(filepath.Join(dir, ".env")); string(got) != want {
		t.Errorf(".env = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "compose.staging.yml")); err != nil {
		t.Errorf("staging files were not deployed: %v", err)
	}
}

const quietManifest = `
steps:
  - run: echo first-output
  - run: |
      echo second-output
      exit ${FAIL:-0}
`

func TestActionQuietLog(t *testing.T) {
	h := newHarness(t)
	h.write(t, "pushbot.yml", quietManifest)
	out, err := h.action(t)
	if err != nil {
		t.Fatalf("action failed: %v\n%s", err, out)
	}
	for _, want := range []string{"==> [1/2] echo first-output\n", "==> [2/2] echo second-output …\n", "PUSHBOT: success ("} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	for _, hidden := range []string{"first-output\n", "second-output\n"} {
		if strings.Contains(out, "\n"+hidden) {
			t.Errorf("output shows the step output %q:\n%s", hidden, out)
		}
	}
}

func TestActionQuietLogShowsFailedStep(t *testing.T) {
	h := newHarness(t)
	h.write(t, "pushbot.yml", quietManifest)
	out, err := h.action(t, "PUSHBOT_ENV=FAIL=3")
	if err == nil {
		t.Fatalf("action succeeded, want failure:\n%s", out)
	}
	if strings.Contains(out, "\nfirst-output\n") {
		t.Errorf("output shows the output of a step that passed:\n%s", out)
	}
	if want := "==> [2/2] echo second-output …\nsecond-output\nPUSHBOT: failed: step 2/2 exited with code 3\n"; !strings.Contains(out, want) {
		t.Errorf("output lacks %q:\n%s", want, out)
	}
}

func TestActionDebugLogIsVerbose(t *testing.T) {
	h := newHarness(t)
	h.write(t, "pushbot.yml", quietManifest)
	out, err := h.action(t, "RUNNER_DEBUG=1")
	if err != nil {
		t.Fatalf("action failed: %v\n%s", err, out)
	}
	for _, want := range []string{"\nfirst-output\n", "\nsecond-output\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestActionNumericEnvironmentName(t *testing.T) {
	h := newHarness(t)
	h.write(t, "pushbot.yml", "environments:\n  2024: {}\n  production: {}\nsteps:\n  - run: 'true'\n")
	h.issuer.SetActionsClaims(map[string]any{"environment": "production"})
	if out, err := h.action(t); err != nil {
		t.Fatalf("action failed: %v\n%s", err, out)
	}
}

func TestActionPacksEnvFileOnce(t *testing.T) {
	h := newHarness(t)
	h.write(t, "pushbot.yml", "files: [deploy/]\nenv:\n  file: deploy/production.env\nsteps:\n  - run: 'true'\n")
	h.write(t, "deploy/production.env", "A=1\n")
	out, err := h.action(t)
	if err != nil {
		t.Fatalf("action failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "pushbot: packing deploy/\n") {
		t.Errorf("env.file packed twice or not at all:\n%s", out)
	}
}

func TestActionOldServer(t *testing.T) {
	h := newHarness(t)
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		http.Error(w, `pushbot: unknown field "secrets"`, http.StatusBadRequest)
	}))
	t.Cleanup(old.Close)
	h.write(t, "pushbot.yml", "env:\n  secrets: [DB_PASSWORD]\n")
	out, err := h.action(t, "PUSHBOT_URL="+old.URL, "PUSHBOT_SECRETS="+secretsJSON)
	if err == nil {
		t.Fatalf("action succeeded:\n%s", out)
	}
	if want := "::error::pushbot: the server is too old for env.secrets: run sudo pushbot upgrade on it"; !strings.Contains(out, want) {
		t.Errorf("output lacks %q:\n%s", want, out)
	}
}

func TestActionWithoutFilesOrRegistry(t *testing.T) {
	h := newHarness(t)
	h.write(t, "pushbot.yml", "steps:\n  - run: echo \"docker_config=${DOCKER_CONFIG:-unset}\"\n")
	out, err := h.action(t, "PUSHBOT_REGISTRY_AUTH=false", "PUSHBOT_VERBOSE=true")
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
			want:     "repository stranger/x is not in the allow list",
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
		{
			name:     "env file missing",
			manifest: "env:\n  file: deploy/production.env\n",
			want:     "::error::pushbot: env.file deploy/production.env not found",
		},
		{
			name:     "no secrets input",
			manifest: "env:\n  secrets: [DB_PASSWORD]\n",
			want:     "add secrets: ${{ toJSON(secrets) }} to the pushbot step",
		},
		{
			name:     "secret not set",
			manifest: "env:\n  secrets: [DB_PASSWORD, MISSING]\n",
			env:      []string{"PUSHBOT_SECRETS=" + secretsJSON},
			want:     "::error::pushbot: secret MISSING is not set in GitHub: gh secret set MISSING\n",
		},
		{
			name:     "secret not set in environment",
			manifest: "env:\n  secrets: [MISSING]\n",
			claims:   map[string]any{"environment": "staging"},
			env:      []string{"PUSHBOT_SECRETS=" + secretsJSON},
			want:     "::error::pushbot: secret MISSING is not set in GitHub (environment staging): gh secret set MISSING --env staging\n",
		},
		{
			name:     "secrets input is not json",
			manifest: "env:\n  secrets: [DB_PASSWORD]\n",
			env:      []string{"PUSHBOT_SECRETS=nope"},
			want:     "the secrets input is not a JSON object",
		},
		{
			name:     "environment not listed",
			manifest: "environments:\n  production: {}\n",
			claims:   map[string]any{"environment": "stagng"},
			want:     `environment "stagng" is not listed`,
		},
		{
			// The job of a misspelled environment gets none of its secrets: name the
			// environment, not the secret.
			name:     "environment not listed, secrets declared",
			manifest: "env:\n  secrets: [DB_PASSWORD]\nenvironments:\n  Staging: {}\n  production: {}\n",
			claims:   map[string]any{"environment": "stagng"},
			env:      []string{"PUSHBOT_SECRETS={}"},
			want:     `::error::pushbot: environment "stagng" is not listed in environments of pushbot.yml (production, Staging)`,
		},
		{
			name:     "no environment, secrets declared",
			manifest: "env:\n  secrets: [DB_PASSWORD]\nenvironments:\n  production: {}\n",
			env:      []string{"PUSHBOT_SECRETS={}"},
			want:     "::error::pushbot: pushbot.yml lists environments, but the job has no environment: set environment: in the workflow",
		},
		{
			name:     "environments is a list",
			manifest: "environments: [production]\n",
			claims:   map[string]any{"environment": "production"},
			want:     "::error::pushbot: pushbot.yml: environments must be a map",
		},
		{
			// The daemon's check, made before the secret is looked up in GitHub.
			name:     "invalid secret name",
			manifest: "env:\n  secrets: [DB-PASSWORD]\n",
			env:      []string{"PUSHBOT_SECRETS={}"},
			want:     "::error::pushbot: pushbot.yml: env.secrets: invalid name \"DB-PASSWORD\": must match ^[A-Za-z_][A-Za-z0-9_]*$",
		},
		{
			name:     "environments is empty",
			manifest: "environments: {}\n",
			claims:   map[string]any{"environment": "production"},
			want:     "environments must list at least one environment",
		},
		{
			name:     "secrets is a string",
			manifest: "env:\n  secrets: DB_PASSWORD\n",
			want:     "::error::pushbot: pushbot.yml: env.secrets must be a list",
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
