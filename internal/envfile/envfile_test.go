package envfile

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	vars, err := Parse("IMAGE_TAG=abc123\n\n# comment\n  \nDB_URL=postgres://u:p@h/db?x=1\nEMPTY=\nSPACED= keep spaces \r\n")
	if err != nil {
		t.Fatal(err)
	}
	want := []Var{
		{Key: "IMAGE_TAG", Value: "abc123"},
		{Key: "DB_URL", Value: "postgres://u:p@h/db?x=1"},
		{Key: "EMPTY", Value: ""},
		{Key: "SPACED", Value: " keep spaces "},
	}
	if !reflect.DeepEqual(vars, want) {
		t.Errorf("got %+v", vars)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"no equals":     "JUSTKEY",
		"bad key":       "1KEY=x",
		"key with dash": "MY-KEY=x",
		"single quote":  "PASS=it's",
		"duplicate":     "A=1\nA=2",
		"empty key":     "=x",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(in); err == nil {
				t.Errorf("Parse(%q) succeeded, want error", in)
			}
		})
	}
}

func TestWrite(t *testing.T) {
	file := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(file, []byte("OLD=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Write(file, []Var{{Key: "A", Value: "x$y"}, {Key: "B", Value: ""}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "A='x$y'\nB=''\n"; got != want {
		t.Errorf("content = %q, want %q", got, want)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm = %o, want 600", perm)
	}
}

func TestWriteEmpty(t *testing.T) {
	file := filepath.Join(t.TempDir(), ".env")
	if err := Write(file, nil); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(file)
	if len(data) != 0 {
		t.Errorf("content = %q, want empty", data)
	}
}

func TestMasker(t *testing.T) {
	m := NewMasker([]Var{
		{Key: "SHORT", Value: "abc"},
		{Key: "PASS", Value: "hunter2"},
		{Key: "LONG", Value: "hunter2hunter2"},
		{Key: "EMPTY", Value: ""},
	})
	got := m.Mask("pass=hunter2 long=hunter2hunter2 short=abc")
	if want := "pass=*** long=*** short=abc"; got != want {
		t.Errorf("Mask = %q, want %q", got, want)
	}
	if got := NewMasker(nil).Mask("plain"); got != "plain" {
		t.Errorf("empty masker changed text: %q", got)
	}
}
