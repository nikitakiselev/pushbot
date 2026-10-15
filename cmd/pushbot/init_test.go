package main

import (
	"reflect"
	"strings"
	"testing"
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
