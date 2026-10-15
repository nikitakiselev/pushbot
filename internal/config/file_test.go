package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestRenderParses(t *testing.T) {
	f := File{Listen: "127.0.0.1:9800", ProjectsRoot: "/srv/apps", Allow: []string{"alice/*", "My-Org/homelab-*"}}
	c, err := Parse(f.Render())
	if err != nil {
		t.Fatalf("%v\n%s", err, f.Render())
	}
	want := &Config{
		Listen:          "127.0.0.1:9800",
		ProjectsRoot:    "/srv/apps",
		StateDir:        "/var/lib/pushbot",
		Audience:        "pushbot",
		DeployTimeout:   15 * time.Minute,
		MaxBundleSize:   50 << 20,
		MaxUnpackedSize: 200 << 20,
		LogRetention:    20,
		OIDCIssuer:      "https://token.actions.githubusercontent.com",
		Allow:           []AllowRule{{Owner: "alice", RepoGlob: "*"}, {Owner: "my-org", RepoGlob: "homelab-*"}},
	}
	if !reflect.DeepEqual(c, want) {
		t.Errorf("got  %+v\nwant %+v", c, want)
	}
}

func fullFile() File {
	retention := 5
	return File{
		Listen:          "172.17.0.1:9800",
		MetricsListen:   "127.0.0.1:9801",
		ProjectsRoot:    "/opt/apps",
		StateDir:        "/data/pushbot",
		Audience:        "deploy",
		DeployTimeout:   "20m",
		MaxBundleSize:   "1GB",
		MaxUnpackedSize: "2GB",
		LogRetention:    &retention,
		OIDCIssuer:      "http://127.0.0.1:1234",
		Allow:           []string{"alice/*"},
	}
}

func TestRenderKeepsOptionalSettings(t *testing.T) {
	c, err := Parse(fullFile().Render())
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "172.17.0.1:9800" || c.MetricsListen != "127.0.0.1:9801" || c.ProjectsRoot != "/opt/apps" ||
		c.StateDir != "/data/pushbot" || c.Audience != "deploy" || c.DeployTimeout != 20*time.Minute ||
		c.MaxBundleSize != 1<<30 || c.MaxUnpackedSize != 2<<30 || c.LogRetention != 5 ||
		c.OIDCIssuer != "http://127.0.0.1:1234" {
		t.Errorf("unexpected config %+v", c)
	}
}

func TestReadFileReadsRenderedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, fullFile().Render(), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, fullFile()) {
		t.Errorf("got  %+v\nwant %+v", got, fullFile())
	}
}

func TestRenderQuotesValues(t *testing.T) {
	f := File{Listen: "[::1]:9800", ProjectsRoot: "/srv/my apps #1", Allow: []string{"alice/*"}}
	c, err := Parse(f.Render())
	if err != nil {
		t.Fatalf("%v\n%s", err, f.Render())
	}
	if c.Listen != "[::1]:9800" || c.ProjectsRoot != "/srv/my apps #1" {
		t.Errorf("listen = %q, projects_root = %q", c.Listen, c.ProjectsRoot)
	}
}

// Every "# key: value" line of a rendered file must stay valid when uncommented,
// and for unset settings the value shown must be the default.
func TestRenderCommentedSettings(t *testing.T) {
	line := regexp.MustCompile(`(?m)^# ([a-z_]+): (.+)$`)
	minimal := File{Listen: "127.0.0.1:9800", ProjectsRoot: "/srv/apps", Allow: []string{"alice/*"}}
	cases := []struct {
		name string
		file File
		keys []string
	}{
		{"minimal", minimal, []string{"metrics_listen", "state_dir", "audience", "deploy_timeout", "max_bundle_size", "max_unpacked_size", "log_retention"}},
		{"full", fullFile(), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text := string(tc.file.Render())
			base, err := Parse([]byte(text))
			if err != nil {
				t.Fatal(err)
			}
			var keys []string
			for _, m := range line.FindAllStringSubmatch(text, -1) {
				keys = append(keys, m[1])
				uncommented := strings.Replace(text, m[0], m[1]+": "+m[2], 1)
				c, err := Parse([]byte(uncommented))
				if err != nil {
					t.Errorf("uncommenting %q: %v", m[0], err)
					continue
				}
				if m[1] == "metrics_listen" {
					c.MetricsListen = ""
				}
				if !reflect.DeepEqual(c, base) {
					t.Errorf("uncommenting %q changes the config: %+v", m[0], c)
				}
			}
			if !reflect.DeepEqual(keys, tc.keys) {
				t.Errorf("commented settings = %v, want %v", keys, tc.keys)
			}
		})
	}
}

func TestReadFileErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadFile(filepath.Join(dir, "missing.yml")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing file: err = %v, want fs.ErrNotExist", err)
	}
	// An unknown key would be dropped when the file is written back.
	unknown := filepath.Join(dir, "unknown.yml")
	os.WriteFile(unknown, []byte("projects_root: /srv\nallow: [a/*]\nlisten_addr: x\n"), 0o644)
	if _, err := ReadFile(unknown); err == nil {
		t.Error("unknown key: ReadFile succeeded, want error")
	}
}

func TestValidateAllow(t *testing.T) {
	cases := map[string]bool{
		"alice/*": true, "My-Org/homelab-*": true, "alice": false, "*/x": false, "a/[": false, "/x": false,
		// These would never match a repository name.
		"alice/* bob/*": false, "alice/a/b": false, "alice/ x": false,
	}
	for in, ok := range cases {
		if err := ValidateAllow(in); (err == nil) != ok {
			t.Errorf("ValidateAllow(%q) = %v, want ok=%v", in, err, ok)
		}
	}
}

func TestValidateAddress(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:9800": true, ":9800": true, "[::1]:9800": true, ":http": true,
		"9800": false, "localhost": false, "host:": false, "": false, ":99999": false, ":-1": false,
	}
	for in, ok := range cases {
		if err := ValidateAddress(in); (err == nil) != ok {
			t.Errorf("ValidateAddress(%q) = %v, want ok=%v", in, err, ok)
		}
	}
}

func TestDialAddr(t *testing.T) {
	cases := map[string]string{
		":9800":           "127.0.0.1:9800",
		"0.0.0.0:9800":    "127.0.0.1:9800",
		"[::]:9800":       "127.0.0.1:9800",
		"172.17.0.1:9800": "172.17.0.1:9800",
		"[::1]:9800":      "[::1]:9800",
	}
	for in, want := range cases {
		if got := DialAddr(in); got != want {
			t.Errorf("DialAddr(%q) = %q, want %q", in, got, want)
		}
	}
}
