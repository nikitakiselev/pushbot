package manifest

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseFull(t *testing.T) {
	m, err := Parse([]byte(`
name: my-service
files:
  - docker-compose.yml
  - config/
  - deploy/compose.prod.yml: docker-compose.override.yml
  - deploy/: .
steps:
  - run: docker compose pull
  - run: |
      echo one
      echo two
`))
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "my-service" {
		t.Errorf("name = %q", m.Name)
	}
	wantFiles := []File{
		{Src: "docker-compose.yml", Dest: "docker-compose.yml"},
		{Src: "config", Dest: "config"},
		{Src: "deploy/compose.prod.yml", Dest: "docker-compose.override.yml"},
		{Src: "deploy", Dest: "."},
	}
	if !reflect.DeepEqual(m.Files, wantFiles) {
		t.Errorf("files = %+v", m.Files)
	}
	wantSteps := []Step{{Run: "docker compose pull"}, {Run: "echo one\necho two\n"}}
	if !reflect.DeepEqual(m.Steps, wantSteps) {
		t.Errorf("steps = %+v", m.Steps)
	}
}

func TestParseDefaults(t *testing.T) {
	m, err := Parse([]byte(`files: [docker-compose.yml]`))
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "" {
		t.Errorf("name = %q, want empty", m.Name)
	}
	if !reflect.DeepEqual(m.Steps, DefaultSteps) {
		t.Errorf("steps = %+v, want defaults", m.Steps)
	}
}

func TestParseEmptyDocument(t *testing.T) {
	m, err := Parse([]byte(""))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Files) != 0 || !reflect.DeepEqual(m.Steps, DefaultSteps) {
		t.Errorf("unexpected manifest %+v", m)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"bad name":             `name: My_Service!`,
		"name starts with dot": `name: .hidden`,
		"unknown top key":      `nmae: x`,
		"empty steps":          `steps: []`,
		"unknown step key":     "steps:\n  - exec: ls",
		"empty run":            "steps:\n  - run: \"\"",
		"two step keys":        "steps:\n  - run: ls\n    shell: bash",
		"absolute src":         `files: [/etc/passwd]`,
		"dotdot src":           `files: [../secret]`,
		"dotdot inside":        `files: [config/../../x]`,
		"dot src":              `files: [.]`,
		"absolute dest":        "files:\n  - a: /etc/a",
		"dotdot dest":          "files:\n  - a: ../a",
		"env dest":             `files: [.env]`,
		"env dest renamed":     "files:\n  - prod.env: .env",
		"map with two keys":    "files:\n  - {a: b, c: d}",
		"empty file entry":     `files: [""]`,
		"file entry is list":   "files:\n  - [a, b]",
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(doc)); err == nil {
				t.Errorf("Parse(%q) succeeded, want error", doc)
			}
		})
	}
}

func TestDefaultName(t *testing.T) {
	cases := map[string]string{
		"nikitakiselev/My-Service": "my-service",
		"owner/repo.name_1":        "repo.name_1",
		"no-slash":                 "no-slash",
	}
	for repo, want := range cases {
		if got := DefaultName(repo); got != want {
			t.Errorf("DefaultName(%q) = %q, want %q", repo, got, want)
		}
	}
}

func TestValidateName(t *testing.T) {
	for _, ok := range []string{"a", "my-service", "app.v2", "x_1"} {
		if err := ValidateName(ok); err != nil {
			t.Errorf("ValidateName(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", ".github", "-x", "UPPER", "a/b", strings.Repeat("a", 64)} {
		if err := ValidateName(bad); err == nil {
			t.Errorf("ValidateName(%q) succeeded, want error", bad)
		}
	}
}

func TestParseEnv(t *testing.T) {
	m, err := Parse([]byte(`
env:
  file: deploy/production.env
  secrets: [APP_KEY, db_password]
`))
	if err != nil {
		t.Fatal(err)
	}
	want := Env{File: "deploy/production.env", Secrets: []string{"APP_KEY", "db_password"}}
	if !reflect.DeepEqual(m.Env, want) {
		t.Errorf("env = %+v, want %+v", m.Env, want)
	}
}

func TestParseEnvEmpty(t *testing.T) {
	for _, doc := range []string{"env: {}", "env:", ""} {
		m, err := Parse([]byte(doc))
		if err != nil {
			t.Fatalf("Parse(%q): %v", doc, err)
		}
		if m.Env.File != "" || len(m.Env.Secrets) != 0 {
			t.Errorf("Parse(%q): env = %+v, want empty", doc, m.Env)
		}
	}
}

const environmentsDoc = `
name: blog
files:
  - deploy/compose.yml: compose.yml
env:
  file: deploy/production.env
  secrets: [APP_KEY, DB_PASSWORD]
steps:
  - run: docker compose up -d
environments:
  production: {}
  Staging:
    name: blog-staging
    files:
      - deploy/compose.yml: compose.yml
      - deploy/Caddyfile.staging: Caddyfile
    env:
      file: deploy/staging.env
  preview:
    env:
      secrets: []
`

func TestForEnvironmentOverrides(t *testing.T) {
	m, err := Parse([]byte(environmentsDoc))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]*Manifest{
		"production": {
			Name:  "blog",
			Files: []File{{Src: "deploy/compose.yml", Dest: "compose.yml"}},
			Env:   Env{File: "deploy/production.env", Secrets: []string{"APP_KEY", "DB_PASSWORD"}},
			Steps: []Step{{Run: "docker compose up -d"}},
		},
		"staging": {
			Name: "blog-staging",
			Files: []File{
				{Src: "deploy/compose.yml", Dest: "compose.yml"},
				{Src: "deploy/Caddyfile.staging", Dest: "Caddyfile"},
			},
			Env:   Env{File: "deploy/staging.env", Secrets: []string{"APP_KEY", "DB_PASSWORD"}},
			Steps: []Step{{Run: "docker compose up -d"}},
		},
		"PREVIEW": {
			Name:  "blog",
			Files: []File{{Src: "deploy/compose.yml", Dest: "compose.yml"}},
			Env:   Env{File: "deploy/production.env", Secrets: []string{}},
			Steps: []Step{{Run: "docker compose up -d"}},
		},
	}
	for environment, want := range cases {
		t.Run(environment, func(t *testing.T) {
			got, err := m.ForEnvironment(environment)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("got  %+v\nwant %+v", got, want)
			}
		})
	}
}

func TestForEnvironmentWithoutEnvironments(t *testing.T) {
	m, err := Parse([]byte("name: blog\nenv:\n  secrets: [A]"))
	if err != nil {
		t.Fatal(err)
	}
	for _, environment := range []string{"", "production", "anything"} {
		got, err := m.ForEnvironment(environment)
		if err != nil {
			t.Fatalf("ForEnvironment(%q): %v", environment, err)
		}
		if !reflect.DeepEqual(got, m) {
			t.Errorf("ForEnvironment(%q) = %+v, want the manifest itself", environment, got)
		}
	}
}

func TestForEnvironmentErrors(t *testing.T) {
	m, err := Parse([]byte(environmentsDoc))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"stagng": `environment "stagng" is not listed in environments of pushbot.yml (preview, production, Staging)`,
		"":       "pushbot.yml lists environments, but the job has no environment: set environment: in the workflow",
	}
	for environment, want := range cases {
		_, err := m.ForEnvironment(environment)
		if err == nil || err.Error() != want {
			t.Errorf("ForEnvironment(%q) error = %v, want %q", environment, err, want)
		}
	}
}

func TestParseEnvErrors(t *testing.T) {
	cases := map[string]struct{ doc, want string }{
		"unknown env key":       {"env:\n  files: x", "files"},
		"absolute env file":     {"env:\n  file: /etc/env", "relative"},
		"dotdot env file":       {"env:\n  file: ../x.env", ".."},
		"empty env file":        {"env:\n  file: \"\"", "empty"},
		"bad secret name":       {"env:\n  secrets: [DB-PASSWORD]", "DB-PASSWORD"},
		"github secret":         {"env:\n  secrets: [github_token]", "GITHUB_"},
		"duplicate secret":      {"env:\n  secrets: [A, a]", "twice"},
		"steps in environment":  {"environments:\n  staging:\n    steps:\n      - run: ls", "PUSHBOT_ENVIRONMENT"},
		"unknown env override":  {"environments:\n  staging:\n    nmae: x", "nmae"},
		"duplicate environment": {"environments:\n  staging: {}\n  STAGING: {}", "twice"},
		"empty environments":    {"environments: {}", "at least one"},
		"bad override name":     {"environments:\n  staging:\n    name: Bad!", "Bad!"},
		"bad override secret":   {"environments:\n  staging:\n    env:\n      secrets: [1X]", "1X"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(c.doc))
			if err == nil {
				t.Fatalf("Parse(%q) succeeded, want error", c.doc)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q lacks %q", err, c.want)
			}
		})
	}
}
