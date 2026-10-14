// Package docs_test checks the READMEs and the agent skill in skills/: the
// pushbot.yml examples must parse, links must resolve, and SKILL.md must follow
// the Agent Skills format.
package docs_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/nikitakiselev/pushbot/internal/manifest"
)

const root = "../.."

// manifestMarker is the first line of every pushbot.yml example in the docs.
const manifestMarker = "# pushbot.yml"

// mustHaveExamples guards against the marker being dropped and the test passing on nothing.
var mustHaveExamples = []string{
	"README.md",
	"README.ru.md",
	"skills/pushbot/references/connect-project.md",
}

func TestManifestExamplesParse(t *testing.T) {
	files := append([]string{"README.md", "README.ru.md"}, skillFiles(t)...)
	found := map[string]int{}
	for _, file := range files {
		for _, b := range codeBlocks(readFile(t, file)) {
			if b.lang != "yaml" || !strings.HasPrefix(b.text, manifestMarker+"\n") {
				continue
			}
			found[file]++
			if _, err := manifest.Parse([]byte(b.text)); err != nil {
				t.Errorf("%s:%d: %v", file, b.line, err)
			}
		}
	}
	for _, file := range mustHaveExamples {
		if found[file] == 0 {
			t.Errorf("%s: no yaml block starting with %q", file, manifestMarker)
		}
	}
}

func TestSkillFrontmatter(t *testing.T) {
	text := readFile(t, "skills/pushbot/SKILL.md")
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		t.Fatal("SKILL.md must start with a --- frontmatter block")
	}
	front, _, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		t.Fatal("SKILL.md frontmatter is not closed with ---")
	}
	var meta struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal([]byte(front), &meta); err != nil {
		t.Fatalf("SKILL.md frontmatter: %v", err)
	}
	// https://agentskills.io/specification: the name matches the directory.
	if meta.Name != "pushbot" {
		t.Errorf("name = %q, want the directory name pushbot", meta.Name)
	}
	if n := len(meta.Description); n == 0 || n > 1024 {
		t.Errorf("description is %d characters, want 1..1024", n)
	}
}

var linkRe = regexp.MustCompile(`\]\(([^)\s]+)\)`)

func TestLinks(t *testing.T) {
	for _, file := range append([]string{"README.md", "README.ru.md"}, skillFiles(t)...) {
		for i, line := range proseLines(readFile(t, file)) {
			for _, m := range linkRe.FindAllStringSubmatch(line, -1) {
				target := m[1]
				if strings.HasPrefix(target, "https://") || strings.HasPrefix(target, "http://") {
					continue
				}
				path, anchor, _ := strings.Cut(target, "#")
				dest := file
				if path != "" {
					dest = filepath.ToSlash(filepath.Join(filepath.Dir(file), path))
				}
				if _, err := os.Stat(filepath.Join(root, dest)); err != nil {
					t.Errorf("%s:%d: link %s: %v", file, i+1, target, err)
					continue
				}
				if anchor != "" && !hasHeading(readFile(t, dest), anchor) {
					t.Errorf("%s:%d: link %s: no heading #%s in %s", file, i+1, target, anchor, dest)
				}
			}
		}
	}
}

// skillFiles lists the Markdown files under skills/, relative to the repository root.
func skillFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(filepath.Join(root, "skills"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".md") {
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no Markdown files under skills/")
	}
	return files
}

func readFile(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

type codeBlock struct {
	lang string
	text string
	line int // line of the opening fence, 1-based
}

// codeBlocks returns the fenced code blocks of a Markdown file. A fence may be
// indented (inside a list item); that indentation is removed from the block's lines.
func codeBlocks(md string) []codeBlock {
	var blocks []codeBlock
	var cur *codeBlock
	var indent int
	for i, line := range strings.Split(md, "\n") {
		trimmed := strings.TrimLeft(line, " ")
		switch {
		case cur == nil && strings.HasPrefix(trimmed, "```"):
			cur = &codeBlock{lang: strings.TrimPrefix(trimmed, "```"), line: i + 1}
			indent = len(line) - len(trimmed)
		case cur != nil && strings.TrimSpace(line) == "```":
			blocks = append(blocks, *cur)
			cur = nil
		case cur != nil:
			cur.text += line[min(indent, len(line)-len(trimmed)):] + "\n"
		}
	}
	return blocks
}

// proseLines returns the file's lines with the contents of code blocks blanked
// out, so line numbers stay correct.
func proseLines(md string) []string {
	lines := strings.Split(md, "\n")
	inCode := false
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimLeft(line, " "), "```") {
			inCode = !inCode
			lines[i] = ""
		} else if inCode {
			lines[i] = ""
		}
	}
	return lines
}

func hasHeading(md, anchor string) bool {
	for _, line := range proseLines(md) {
		if text, ok := strings.CutPrefix(strings.TrimLeft(line, "#"), " "); ok && strings.HasPrefix(line, "#") {
			if slug(text) == anchor {
				return true
			}
		}
	}
	return false
}

func TestSlug(t *testing.T) {
	for heading, want := range map[string]string{
		"3. Reverse proxy":                 "3-reverse-proxy",
		"4. Write `pushbot.yml`":           "4-write-pushbotyml",
		"Deploy failed: `PUSHBOT: failed`": "deploy-failed-pushbot-failed",
		"Deployed, but it does not work":   "deployed-but-it-does-not-work",
		"Конфиг демона":                    "конфиг-демона",
	} {
		if got := slug(heading); got != want {
			t.Errorf("slug(%q) = %q, want %q", heading, got, want)
		}
	}
}

var slugDropRe = regexp.MustCompile(`[^\p{L}\p{N} _-]`)

// slug turns a heading into its anchor the way GitHub does.
func slug(heading string) string {
	s := slugDropRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(heading)), "")
	return strings.ReplaceAll(s, " ", "-")
}
