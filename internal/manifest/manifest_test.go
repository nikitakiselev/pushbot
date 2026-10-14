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
		"bad name":            `name: My_Service!`,
		"name starts with dot": `name: .hidden`,
		"unknown top key":     `nmae: x`,
		"empty steps":         `steps: []`,
		"unknown step key":    "steps:\n  - exec: ls",
		"empty run":           "steps:\n  - run: \"\"",
		"two step keys":       "steps:\n  - run: ls\n    shell: bash",
		"absolute src":        `files: [/etc/passwd]`,
		"dotdot src":          `files: [../secret]`,
		"dotdot inside":       `files: [config/../../x]`,
		"dot src":             `files: [.]`,
		"absolute dest":       "files:\n  - a: /etc/a",
		"dotdot dest":         "files:\n  - a: ../a",
		"env dest":            `files: [.env]`,
		"env dest renamed":    "files:\n  - prod.env: .env",
		"map with two keys":   "files:\n  - {a: b, c: d}",
		"empty file entry":    `files: [""]`,
		"file entry is list":  "files:\n  - [a, b]",
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
