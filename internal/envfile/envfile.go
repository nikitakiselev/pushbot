// Package envfile parses the action's env input, writes .env files and masks
// env values in logs.
package envfile

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Var is a single KEY=VALUE pair.
type Var struct {
	Key   string
	Value string
}

var keyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Parse reads KEY=VALUE lines. Blank lines and lines starting with # are skipped.
// The value is everything after the first "=", verbatim.
func Parse(text string) ([]Var, error) {
	var vars []Var
	seen := map[string]bool{}
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSuffix(line, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok {
			return nil, fmt.Errorf("env line %d: expected KEY=VALUE", i+1)
		}
		if !keyRe.MatchString(key) {
			return nil, fmt.Errorf("env line %d: invalid key %q", i+1, key)
		}
		if strings.Contains(value, "'") {
			return nil, fmt.Errorf("env line %d: %s: values containing ' are not supported", i+1, key)
		}
		if seen[key] {
			return nil, fmt.Errorf("env line %d: duplicate key %s", i+1, key)
		}
		seen[key] = true
		vars = append(vars, Var{Key: key, Value: value})
	}
	return vars, nil
}

// Write atomically replaces file with KEY='VALUE' lines and mode 0600.
// Single quotes keep docker compose from interpolating $ in values.
func Write(file string, vars []Var) error {
	var b strings.Builder
	for _, v := range vars {
		fmt.Fprintf(&b, "%s='%s'\n", v.Key, v.Value)
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), ".env.tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), file)
}

// Masker replaces env values in text with ***.
type Masker struct {
	replacer *strings.Replacer
}

// minMaskLen avoids masking short values like "1" or "on", which would garble logs.
const minMaskLen = 4

// NewMasker masks every value at least minMaskLen characters long.
func NewMasker(vars []Var) *Masker {
	var values []string
	for _, v := range vars {
		if len(v.Value) >= minMaskLen {
			values = append(values, v.Value)
		}
	}
	// Longest first, so a value containing another is masked as a whole.
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	var pairs []string
	for _, v := range values {
		pairs = append(pairs, v, "***")
	}
	return &Masker{replacer: strings.NewReplacer(pairs...)}
}

// Mask returns s with all known values replaced.
func (m *Masker) Mask(s string) string {
	return m.replacer.Replace(s)
}
