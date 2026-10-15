// Package envfile parses the action's env input, checks values taken as is,
// reads keys of dotenv files, writes .env files and masks env values in logs.
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

var (
	keyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// fileLineRe matches "[export ]KEY=VALUE" lines of a dotenv file.
	fileLineRe = regexp.MustCompile(`^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*)$`)
	// refRe matches ${KEY}, ${KEY:-x}, ${KEY-x}, ${KEY:?x} and the like.
	refRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?:\}|:?[-?+])`)
)

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

// Check validates values taken as is (the env input and secrets). keys are the
// keys of all .env sources. It returns warnings for the deploy log.
func Check(raw []Var, keys []string) (warnings []string, err error) {
	known := map[string]bool{}
	for _, k := range keys {
		known[k] = true
	}
	for _, v := range raw {
		switch {
		case strings.Contains(v.Value, "'"):
			return nil, fmt.Errorf("%s: values containing ' are not supported", v.Key)
		case strings.ContainsAny(v.Value, "\r\n"):
			return nil, fmt.Errorf("%s: multiline values are not supported", v.Key)
		case strings.ContainsRune(v.Value, 0):
			return nil, fmt.Errorf("%s: values containing NUL bytes are not supported", v.Key)
		case len(v.Value) >= 2 && strings.HasPrefix(v.Value, `"`) && strings.HasSuffix(v.Value, `"`):
			return nil, fmt.Errorf("%s: the value is wrapped in double quotes; env and secrets values are taken as is, remove the quotes", v.Key)
		}
		for _, m := range refRe.FindAllStringSubmatch(v.Value, -1) {
			if known[m[1]] {
				return nil, fmt.Errorf("%s: ${%s} is not expanded in env and secrets; move %s to the env file", v.Key, m[1], v.Key)
			}
		}
		if strings.Contains(v.Value, "$$") {
			warnings = append(warnings, fmt.Sprintf(`%s contains "$$": env and secrets values are taken as is, so it stays "$$"`, v.Key))
		}
	}
	return warnings, nil
}

// FileKeys returns the keys a dotenv file defines, in order. Values are not
// interpreted, only skipped: a value opening a quote that is not closed on its
// line continues up to the closing quote.
func FileKeys(data []byte) ([]string, error) {
	lines := strings.Split(string(data), "\n")
	var keys []string
	seen := map[string]bool{}
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSuffix(lines[i], "\r")
		if trimmed := strings.TrimSpace(line); trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		m := fileLineRe.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("line %d: expected KEY=VALUE", i+1)
		}
		key, value := m[1], m[2]
		if seen[key] {
			return nil, fmt.Errorf("line %d: duplicate key %s", i+1, key)
		}
		seen[key] = true
		keys = append(keys, key)

		if value == "" || (value[0] != '"' && value[0] != '\'') {
			continue
		}
		start, quote, rest := i, value[0], value[1:]
		for !closesQuote(rest, quote) {
			if i++; i == len(lines) {
				return nil, fmt.Errorf("line %d: %s: unterminated %c quote", start+1, key, quote)
			}
			rest = lines[i]
		}
	}
	return keys, nil
}

// closesQuote reports whether s contains the closing quote. A backslash
// escapes the next character in both kinds of quotes, as docker compose reads them.
func closesQuote(s string, quote byte) bool {
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\\':
			i++
		case s[i] == quote:
			return true
		}
	}
	return false
}

// Content of a .env file.
type Content struct {
	Env      []Var  // the action's env input
	Secrets  []Var  // GitHub secrets
	FileName string // repository path of File, for the comment
	File     []byte // a dotenv file from the repository, written as is
}

// Write atomically replaces file with c and mode 0600. Env and secrets become
// KEY='VALUE' lines: single quotes keep docker compose from interpolating $ in
// them. They come before File, so File may refer to them. Each non-empty part
// starts with a comment.
func Write(file string, c Content) error {
	var b strings.Builder
	for _, part := range []struct {
		title string
		vars  []Var
	}{{"env input", c.Env}, {"secrets", c.Secrets}} {
		if len(part.vars) == 0 {
			continue
		}
		fmt.Fprintf(&b, "# pushbot: %s\n", part.title)
		for _, v := range part.vars {
			fmt.Fprintf(&b, "%s='%s'\n", v.Key, v.Value)
		}
	}
	if len(c.File) > 0 {
		fmt.Fprintf(&b, "# pushbot: %s\n", c.FileName)
		b.Write(c.File)
		if c.File[len(c.File)-1] != '\n' {
			b.WriteByte('\n')
		}
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
