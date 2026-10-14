// Package config loads the daemon configuration (/etc/pushbot/config.yml).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the validated daemon configuration.
type Config struct {
	Listen          string
	MetricsListen   string // "" disables /metrics
	ProjectsRoot    string
	StateDir        string
	Audience        string
	DeployTimeout   time.Duration
	MaxBundleSize   int64
	MaxUnpackedSize int64
	LogRetention    int
	OIDCIssuer      string
	Allow           []AllowRule
}

// AllowRule permits deploys from repositories of Owner whose name matches RepoGlob
// (path.Match syntax). Both are lower-cased.
type AllowRule struct {
	Owner    string
	RepoGlob string
}

type rawConfig struct {
	Listen          string   `yaml:"listen"`
	MetricsListen   string   `yaml:"metrics_listen"`
	ProjectsRoot    string   `yaml:"projects_root"`
	StateDir        string   `yaml:"state_dir"`
	Audience        string   `yaml:"audience"`
	DeployTimeout   string   `yaml:"deploy_timeout"`
	MaxBundleSize   string   `yaml:"max_bundle_size"`
	MaxUnpackedSize string   `yaml:"max_unpacked_size"`
	LogRetention    *int     `yaml:"log_retention"`
	OIDCIssuer      string   `yaml:"oidc_issuer"`
	Allow           []string `yaml:"allow"`
}

// Load reads and parses a config file.
func Load(file string) (*Config, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	c, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return c, nil
}

// Parse decodes config YAML, applies defaults and validates the result.
func Parse(data []byte) (*Config, error) {
	var raw rawConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}

	c := &Config{
		Listen:        or(raw.Listen, "127.0.0.1:9800"),
		MetricsListen: raw.MetricsListen,
		ProjectsRoot:  raw.ProjectsRoot,
		StateDir:      or(raw.StateDir, "/var/lib/pushbot"),
		Audience:      or(raw.Audience, "pushbot"),
		OIDCIssuer:    or(raw.OIDCIssuer, "https://token.actions.githubusercontent.com"),
		LogRetention:  20,
	}
	if c.ProjectsRoot == "" || !filepath.IsAbs(c.ProjectsRoot) {
		return nil, errors.New("projects_root must be an absolute path")
	}
	if c.MetricsListen != "" {
		if _, port, err := net.SplitHostPort(c.MetricsListen); err != nil || port == "" {
			return nil, fmt.Errorf("metrics_listen: expected host:port, got %q", c.MetricsListen)
		}
	}
	if !filepath.IsAbs(c.StateDir) {
		return nil, errors.New("state_dir must be an absolute path")
	}
	if raw.LogRetention != nil {
		if *raw.LogRetention < 1 {
			return nil, errors.New("log_retention must be at least 1")
		}
		c.LogRetention = *raw.LogRetention
	}

	var err error
	if c.DeployTimeout, err = time.ParseDuration(or(raw.DeployTimeout, "15m")); err != nil {
		return nil, fmt.Errorf("deploy_timeout: %w", err)
	}
	if c.MaxBundleSize, err = parseSize(or(raw.MaxBundleSize, "50MB")); err != nil {
		return nil, fmt.Errorf("max_bundle_size: %w", err)
	}
	if c.MaxUnpackedSize, err = parseSize(or(raw.MaxUnpackedSize, "200MB")); err != nil {
		return nil, fmt.Errorf("max_unpacked_size: %w", err)
	}

	if len(raw.Allow) == 0 {
		return nil, errors.New("allow must list at least one owner/repo pattern")
	}
	for _, s := range raw.Allow {
		rule, err := parseAllowRule(s)
		if err != nil {
			return nil, fmt.Errorf("allow: %w", err)
		}
		c.Allow = append(c.Allow, rule)
	}
	return c, nil
}

func parseAllowRule(s string) (AllowRule, error) {
	owner, glob, ok := strings.Cut(strings.ToLower(s), "/")
	if !ok || owner == "" || glob == "" {
		return AllowRule{}, fmt.Errorf("%q: expected owner/repo-pattern", s)
	}
	if strings.ContainsAny(owner, "*?[]\\") {
		return AllowRule{}, fmt.Errorf("%q: owner must be literal, only the repo part may use wildcards", s)
	}
	if _, err := path.Match(glob, ""); err != nil {
		return AllowRule{}, fmt.Errorf("%q: %w", s, err)
	}
	return AllowRule{Owner: owner, RepoGlob: glob}, nil
}

// parseSize parses sizes like "512", "10KB", "50MB", "1GB" (powers of 1024).
func parseSize(s string) (int64, error) {
	units := []struct {
		suffix string
		mult   int64
	}{{"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"B", 1}}
	num, mult := strings.ToUpper(strings.TrimSpace(s)), int64(1)
	for _, u := range units {
		if strings.HasSuffix(num, u.suffix) {
			num, mult = strings.TrimSpace(strings.TrimSuffix(num, u.suffix)), u.mult
			break
		}
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return n * mult, nil
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
