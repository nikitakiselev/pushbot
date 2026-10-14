// Package manifest parses and validates pushbot.yml, the per-project deploy description.
package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Manifest is a validated pushbot.yml.
type Manifest struct {
	// Name of the project directory; empty means "derive from the repository".
	Name  string
	Files []File
	Steps []Step
}

// File maps a path in the repository to a path in the project directory.
// Both are clean relative paths; Dest may be "." (the project directory itself).
type File struct {
	Src  string
	Dest string
}

// Step is a shell script executed in the project directory.
type Step struct {
	Run string
}

// DefaultSteps are used when the manifest has no steps key.
var DefaultSteps = []Step{
	{Run: "docker compose pull"},
	{Run: "docker compose up -d --remove-orphans"},
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

type rawManifest struct {
	Name  string  `yaml:"name"`
	Files []File  `yaml:"files"`
	Steps *[]Step `yaml:"steps"`
}

// Parse decodes and validates a manifest. Unknown keys are errors.
func Parse(data []byte) (*Manifest, error) {
	var raw rawManifest
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("pushbot.yml: %w", err)
	}

	m := &Manifest{Name: raw.Name, Files: raw.Files, Steps: DefaultSteps}
	if m.Name != "" {
		if err := ValidateName(m.Name); err != nil {
			return nil, fmt.Errorf("pushbot.yml: %w", err)
		}
	}
	if raw.Steps != nil {
		if len(*raw.Steps) == 0 {
			return nil, errors.New("pushbot.yml: steps must not be empty (omit the key to use the defaults)")
		}
		m.Steps = *raw.Steps
	}
	return m, nil
}

// ValidateName checks a project name.
func ValidateName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("invalid project name %q: must match %s", name, nameRe)
	}
	return nil
}

// DefaultName derives a project name from an "owner/repo" string.
// The result may still be invalid (e.g. ".github") and must be checked with ValidateName.
func DefaultName(repository string) string {
	_, repo, found := strings.Cut(repository, "/")
	if !found {
		repo = repository
	}
	return strings.ToLower(repo)
}

// UnmarshalYAML accepts either "path" or a single-pair map "src: dest".
func (f *File) UnmarshalYAML(node *yaml.Node) error {
	var src, dest string
	switch node.Kind {
	case yaml.ScalarNode:
		src, dest = node.Value, node.Value
	case yaml.MappingNode:
		if len(node.Content) != 2 {
			return fmt.Errorf("line %d: a files entry must be a path or a single \"src: dest\" pair", node.Line)
		}
		src, dest = node.Content[0].Value, node.Content[1].Value
	default:
		return fmt.Errorf("line %d: a files entry must be a path or a single \"src: dest\" pair", node.Line)
	}

	var err error
	if f.Src, err = cleanRel(src, false); err != nil {
		return fmt.Errorf("line %d: src: %w", node.Line, err)
	}
	if f.Dest, err = cleanRel(dest, true); err != nil {
		return fmt.Errorf("line %d: dest: %w", node.Line, err)
	}
	if f.Dest == ".env" {
		return fmt.Errorf("line %d: .env is written by pushbot from the env input and cannot be listed in files", node.Line)
	}
	return nil
}

// UnmarshalYAML accepts only {run: "<script>"}.
func (s *Step) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode || len(node.Content) != 2 || node.Content[0].Value != "run" {
		return fmt.Errorf("line %d: a step must be a map with a single \"run\" key", node.Line)
	}
	if err := node.Content[1].Decode(&s.Run); err != nil {
		return fmt.Errorf("line %d: run: %w", node.Line, err)
	}
	if strings.TrimSpace(s.Run) == "" {
		return fmt.Errorf("line %d: run must not be empty", node.Line)
	}
	return nil
}

// cleanRel validates a relative slash-separated path and returns it cleaned.
// "." is accepted only when allowDot is set.
func cleanRel(p string, allowDot bool) (string, error) {
	if p == "" {
		return "", errors.New("path must not be empty")
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("%q: path must be relative", p)
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return "", fmt.Errorf("%q: path must not contain \"..\"", p)
		}
	}
	clean := path.Clean(p)
	if clean == "." && !allowDot {
		return "", fmt.Errorf("%q: path must name a file or directory", p)
	}
	return clean, nil
}
