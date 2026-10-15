package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nikitakiselev/pushbot/internal/config"
)

func TestUseWizard(t *testing.T) {
	cases := []struct {
		name                              string
		settings, force, exists, terminal bool
		wizard                            bool
		err                               string // part of the error, "" for none
	}{
		{name: "no flags in a terminal", terminal: true, wizard: true},
		{name: "no flags in a terminal, config exists", exists: true, terminal: true, wizard: true},
		{name: "no flags without a terminal", err: "no terminal"},
		{name: "flags", settings: true, terminal: true},
		{name: "flags without a terminal", settings: true},
		{name: "flags, config exists", settings: true, exists: true, terminal: true, err: "--force"},
		{name: "flags, config exists, force", settings: true, force: true, exists: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wizard, err := useWizard("/etc/pushbot/config.yml", tc.settings, tc.force, tc.exists, tc.terminal)
			switch {
			case tc.err == "" && err != nil:
				t.Fatalf("err = %v", err)
			case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
				t.Fatalf("err = %v, want one with %q", err, tc.err)
			case wizard != tc.wizard:
				t.Errorf("wizard = %v, want %v", wizard, tc.wizard)
			}
		})
	}
}

func TestSplitPatterns(t *testing.T) {
	cases := map[string][]string{
		"alice/*":                      {"alice/*"},
		" alice/*, my-org/homelab-* ":  {"alice/*", "my-org/homelab-*"},
		"alice/* my-org/site,,bob/x  ": {"alice/*", "my-org/site", "bob/x"},
		"  ":                           {},
	}
	for in, want := range cases {
		if got := splitPatterns(in); !reflect.DeepEqual(got, want) {
			t.Errorf("splitPatterns(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPreviewShowsSettingsOnly(t *testing.T) {
	got := preview([]byte("# pushbot configuration\nprojects_root: /srv\nallow:\n  - alice/* `x` \\n\n\n# metrics_listen: 127.0.0.1:9801\n"))
	want := "projects\\_root: /srv\nallow:\n  - alice/\\* \\`x\\` \\\\n"
	if got != want {
		t.Errorf("preview = %q, want %q", got, want)
	}
}

// Stray spaces would be quoted into the file and fail net.Listen, or make a
// directory whose name ends in a space.
func TestAnswersApplyTrims(t *testing.T) {
	a := answers{Allow: " alice/* ", Listen: " 127.0.0.1:9800 ", ProjectsRoot: "/srv/apps ", MetricsListen: " "}
	got := a.apply(config.File{Audience: "deploy"})
	want := config.File{Allow: []string{"alice/*"}, Listen: "127.0.0.1:9800", ProjectsRoot: "/srv/apps", Audience: "deploy"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

func strp(s string) *string { return &s }

func TestPrepare(t *testing.T) {
	const existing = "listen: 172.17.0.1:9800\nprojects_root: /opt/apps\nstate_dir: /data/pushbot\nallow:\n  - old/*\n"
	defaults := config.File{Listen: "127.0.0.1:9800", ProjectsRoot: "/srv/apps"}
	cases := []struct {
		name     string
		file     string // "" for no file
		flags    settingFlags
		force    bool
		terminal bool
		want     config.File
		wizard   bool
		err      string // part of the error
		warning  bool
	}{
		{name: "new, wizard", terminal: true, want: defaults, wizard: true},
		{name: "new, allow flag", flags: settingFlags{allow: &[]string{"a/*"}},
			want: config.File{Listen: "127.0.0.1:9800", ProjectsRoot: "/srv/apps", Allow: []string{"a/*"}}},
		{name: "new, no allow", flags: settingFlags{listen: strp("127.0.0.1:9900")}, err: "--allow"},
		{name: "existing, wizard", file: existing, terminal: true, wizard: true,
			want: config.File{Listen: "172.17.0.1:9800", ProjectsRoot: "/opt/apps", StateDir: "/data/pushbot", Allow: []string{"old/*"}}},
		{name: "existing, allow replaces the list, the rest is kept", file: existing, force: true,
			flags: settingFlags{allow: &[]string{"a/*", "b/*"}},
			want:  config.File{Listen: "172.17.0.1:9800", ProjectsRoot: "/opt/apps", StateDir: "/data/pushbot", Allow: []string{"a/*", "b/*"}}},
		{name: "existing, listen only keeps allow", file: existing, force: true,
			flags: settingFlags{listen: strp("127.0.0.1:9900")},
			want:  config.File{Listen: "127.0.0.1:9900", ProjectsRoot: "/opt/apps", StateDir: "/data/pushbot", Allow: []string{"old/*"}}},
		{name: "existing, no force", file: existing, flags: settingFlags{allow: &[]string{"a/*"}}, err: "--force"},
		{name: "broken, flags", file: "listen: [\n", force: true, flags: settingFlags{allow: &[]string{"a/*"}}, err: "cannot read"},
		{name: "empty, flags", file: "# nothing\n", force: true, flags: settingFlags{allow: &[]string{"a/*"}}, err: "cannot read"},
		{name: "broken, wizard", file: "listen: [\n", terminal: true, want: defaults, wizard: true, warning: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yml")
			if tc.file != "" {
				os.WriteFile(path, []byte(tc.file), 0o644)
			}
			var out bytes.Buffer
			f, wizard, err := prepare(path, tc.flags, tc.force, tc.terminal, &out)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want one with %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(f, tc.want) || wizard != tc.wizard {
				t.Errorf("got %+v wizard=%v\nwant %+v wizard=%v", f, wizard, tc.want, tc.wizard)
			}
			if warned := strings.Contains(out.String(), "cannot read"); warned != tc.warning {
				t.Errorf("warning printed: %v, want %v (%q)", warned, tc.warning, out.String())
			}
		})
	}
}
