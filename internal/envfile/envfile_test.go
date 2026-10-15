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
	err := Write(file, Content{
		Env:      []Var{{Key: "A", Value: "x$y"}, {Key: "B", Value: ""}},
		Secrets:  []Var{{Key: "DB_PASSWORD", Value: "hunter2"}},
		FileName: "deploy/production.env",
		File:     []byte("APP_NAME=\"mBlog\"\nURL=\"mysql://${DB_PASSWORD}@db\""),
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	want := "# pushbot: env input\nA='x$y'\nB=''\n" +
		"# pushbot: secrets\nDB_PASSWORD='hunter2'\n" +
		"# pushbot: deploy/production.env\nAPP_NAME=\"mBlog\"\nURL=\"mysql://${DB_PASSWORD}@db\"\n"
	if got := string(data); got != want {
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

func TestWriteSkipsEmptyParts(t *testing.T) {
	cases := map[string]struct {
		c    Content
		want string
	}{
		"nothing":    {Content{}, ""},
		"env only":   {Content{Env: []Var{{Key: "A", Value: "1"}}}, "# pushbot: env input\nA='1'\n"},
		"empty file": {Content{FileName: "x.env", File: []byte{}}, ""},
		"file only":  {Content{FileName: "x.env", File: []byte("A=1\n")}, "# pushbot: x.env\nA=1\n"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), ".env")
			if err := Write(file, c.c); err != nil {
				t.Fatal(err)
			}
			if data, _ := os.ReadFile(file); string(data) != c.want {
				t.Errorf("content = %q, want %q", data, c.want)
			}
		})
	}
}

func TestFileKeys(t *testing.T) {
	data := "# comment\n" +
		"\n" +
		"export A=1\n" +
		"  B = two words\n" +
		"C=\"multi\n" +
		"line \\\" still open\n" +
		"end\"\n" +
		"D='x\n" +
		"y'\n" +
		"E=\"one line\" # comment\n" +
		"F=\r\n" +
		"G=it's fine unquoted\n"
	keys, err := FileKeys([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"A", "B", "C", "D", "E", "F", "G"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("keys = %q, want %q", keys, want)
	}
}

func TestFileKeysErrors(t *testing.T) {
	cases := map[string]string{
		"A=1\nnot a pair\n": "line 2: expected KEY=VALUE",
		"1A=x":              "line 1: expected KEY=VALUE",
		"A=1\nA=2":          "line 2: duplicate key A",
		"A=\"open\nB=2\n":   `line 1: A: unterminated " quote`,
		"A='open":           "line 1: A: unterminated ' quote",
		"A=\"escaped \\\"":  `line 1: A: unterminated " quote`,
	}
	for in, want := range cases {
		_, err := FileKeys([]byte(in))
		if err == nil || err.Error() != want {
			t.Errorf("FileKeys(%q) error = %v, want %q", in, err, want)
		}
	}
}

func TestCheck(t *testing.T) {
	keys := []string{"APP_NAME", "DB_PASSWORD", "IMAGE_TAG"}
	warnings, err := Check([]Var{
		{Key: "IMAGE_TAG", Value: "v1"},
		{Key: "DB_PASSWORD", Value: "a$$b"},
		{Key: "QUOTE", Value: `"`},
		{Key: "HALF", Value: `"open`},
		{Key: "TEMPLATE", Value: "${UNKNOWN} and $APP_NAME"},
	}, keys)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`DB_PASSWORD contains "$$": env and secrets values are taken as is, so it stays "$$"`}
	if !reflect.DeepEqual(warnings, want) {
		t.Errorf("warnings = %q, want %q", warnings, want)
	}
}

func TestCheckErrors(t *testing.T) {
	keys := []string{"APP_NAME", "MAIL_FROM_NAME"}
	cases := map[string]struct {
		v    Var
		want string
	}{
		"wrapped in quotes": {Var{"APP_NAME", `"mBlog"`}, "APP_NAME: the value is wrapped in double quotes; env and secrets values are taken as is, remove the quotes"},
		"reference":         {Var{"MAIL_FROM_NAME", "${APP_NAME}"}, "MAIL_FROM_NAME: ${APP_NAME} is not expanded in env and secrets; move MAIL_FROM_NAME to the env file"},
		"default reference": {Var{"MAIL_FROM_NAME", "x ${APP_NAME:-blog}"}, "MAIL_FROM_NAME: ${APP_NAME} is not expanded in env and secrets; move MAIL_FROM_NAME to the env file"},
		"single quote":      {Var{"PASS", "it's"}, "PASS: values containing ' are not supported"},
		"newline":           {Var{"PASS", "a\nb"}, "PASS: multiline values are not supported"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Check([]Var{c.v}, keys)
			if err == nil || err.Error() != c.want {
				t.Errorf("error = %v, want %q", err, c.want)
			}
		})
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
