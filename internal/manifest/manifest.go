// Package manifest parses and validates pushbot.yml, the per-project deploy description.
package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Manifest is a validated pushbot.yml.
type Manifest struct {
	// Name of the project directory; empty means "derive from the repository".
	Name  string
	Files []File
	Env   Env
	Steps []Step
	// Environments maps GitHub environment names to their overrides; nil when
	// pushbot.yml has no environments key. ForEnvironment applies them.
	Environments map[string]Environment
}

// Env says where .env values come from besides the action's env input.
type Env struct {
	File    string   // repository path of a dotenv file written into .env as is; "" for none
	Secrets []string // names of the GitHub secrets the action sends
}

// Environment overrides top-level keys for one GitHub environment. A nil field
// keeps the top-level value.
type Environment struct {
	Name    *string
	Files   *[]File
	EnvFile *string
	Secrets *[]string
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

var (
	nameRe       = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	secretNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

type rawManifest struct {
	Name         string                      `yaml:"name"`
	Files        []File                      `yaml:"files"`
	Env          *rawEnv                     `yaml:"env"`
	Steps        *[]Step                     `yaml:"steps"`
	Environments *map[string]*rawEnvironment `yaml:"environments"`
}

type rawEnv struct {
	File    *string   `yaml:"file"`
	Secrets *[]string `yaml:"secrets"`
}

type rawEnvironment struct {
	Name  *string `yaml:"name"`
	Files *[]File `yaml:"files"`
	Env   *rawEnv `yaml:"env"`
	// Steps is decoded only to explain why it cannot be overridden.
	Steps yaml.Node `yaml:"steps"`
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
	file, secrets, err := parseEnv(raw.Env)
	if err != nil {
		return nil, fmt.Errorf("pushbot.yml: %w", err)
	}
	if file != nil {
		m.Env.File = *file
	}
	if secrets != nil {
		m.Env.Secrets = *secrets
	}
	if raw.Environments != nil {
		if m.Environments, err = parseEnvironments(*raw.Environments); err != nil {
			return nil, fmt.Errorf("pushbot.yml: %w", err)
		}
	}
	return m, nil
}

// ForEnvironment returns the manifest for a deploy from the given GitHub
// environment ("" when the job has none) with its overrides applied.
func (m *Manifest) ForEnvironment(environment string) (*Manifest, error) {
	if m.Environments == nil {
		return m, nil
	}
	if environment == "" {
		return nil, errors.New("pushbot.yml lists environments, but the job has no environment: set environment: in the workflow")
	}
	var names []string
	for name, o := range m.Environments {
		if !strings.EqualFold(name, environment) {
			names = append(names, name)
			continue
		}
		r := &Manifest{Name: m.Name, Files: m.Files, Env: m.Env, Steps: m.Steps}
		if o.Name != nil {
			r.Name = *o.Name
		}
		if o.Files != nil {
			r.Files = *o.Files
		}
		if o.EnvFile != nil {
			r.Env.File = *o.EnvFile
		}
		if o.Secrets != nil {
			r.Env.Secrets = *o.Secrets
		}
		return r, nil
	}
	sort.Slice(names, func(i, j int) bool { return strings.ToLower(names[i]) < strings.ToLower(names[j]) })
	return nil, fmt.Errorf("environment %q is not listed in environments of pushbot.yml (%s)", environment, strings.Join(names, ", "))
}

// parseEnv validates an env block; nil results mean the key is absent.
func parseEnv(r *rawEnv) (file *string, secrets *[]string, err error) {
	if r == nil {
		return nil, nil, nil
	}
	if r.File != nil {
		clean, err := cleanRel(*r.File, false)
		if err != nil {
			return nil, nil, fmt.Errorf("env.file: %w", err)
		}
		file = &clean
	}
	if r.Secrets != nil {
		seen := map[string]bool{}
		for _, name := range *r.Secrets {
			upper := strings.ToUpper(name)
			switch {
			case !secretNameRe.MatchString(name):
				return nil, nil, fmt.Errorf("env.secrets: invalid name %q: must match %s", name, secretNameRe)
			case strings.HasPrefix(upper, "GITHUB_"):
				return nil, nil, fmt.Errorf("env.secrets: %s: names starting with GITHUB_ are reserved by GitHub", name)
			case seen[upper]:
				return nil, nil, fmt.Errorf("env.secrets: %s is listed twice (GitHub secret names are case-insensitive)", name)
			}
			seen[upper] = true
		}
		secrets = r.Secrets
	}
	return file, secrets, nil
}

func parseEnvironments(raw map[string]*rawEnvironment) (map[string]Environment, error) {
	if len(raw) == 0 {
		return nil, errors.New("environments must list at least one environment (omit the key to deploy the same way everywhere)")
	}
	envs := map[string]Environment{}
	seen := map[string]string{}
	for name, r := range raw {
		if name == "" {
			return nil, errors.New("environments: environment name must not be empty")
		}
		if other, ok := seen[strings.ToLower(name)]; ok {
			return nil, fmt.Errorf("environments: %q and %q name the same environment twice (GitHub environment names are case-insensitive)", other, name)
		}
		seen[strings.ToLower(name)] = name
		var e Environment
		if r != nil {
			if r.Steps.Kind != 0 {
				return nil, fmt.Errorf("line %d: environments.%s: steps cannot be overridden per environment, so that every environment runs the same deploy; check $PUSHBOT_ENVIRONMENT in a step instead", r.Steps.Line, name)
			}
			if r.Name != nil {
				if err := ValidateName(*r.Name); err != nil {
					return nil, fmt.Errorf("environments.%s: %w", name, err)
				}
			}
			file, secrets, err := parseEnv(r.Env)
			if err != nil {
				return nil, fmt.Errorf("environments.%s: %w", name, err)
			}
			e = Environment{Name: r.Name, Files: r.Files, EnvFile: file, Secrets: secrets}
		}
		envs[name] = e
	}
	return envs, nil
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
