package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type entry struct {
	hdr  tar.Header
	body string
}

func file(name, body string, mode int64) entry {
	return entry{hdr: tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode, Size: int64(len(body))}, body: body}
}

func dir(name string) entry {
	return entry{hdr: tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o755}}
}

func archive(t *testing.T, entries ...entry) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := e.hdr
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

var limits = Limits{MaxUnpacked: 1 << 20, MaxFiles: 100}

func TestExtract(t *testing.T) {
	dest := t.TempDir()
	buf := archive(t,
		file("docker-compose.yml", "services: {}\n", 0o644),
		dir("config/"),
		file("config/app.conf", "a=1\n", 0o600),
		file("./scripts/migrate.sh", "#!/bin/sh\n", 0o755),
		entry{hdr: tar.Header{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader}},
	)
	if err := Extract(buf, dest, limits); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"docker-compose.yml": "services: {}\n",
		"config/app.conf":    "a=1\n",
		"scripts/migrate.sh": "#!/bin/sh\n",
	} {
		got, err := os.ReadFile(filepath.Join(dest, name))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", name, got, err, want)
		}
	}
	info, err := os.Stat(filepath.Join(dest, "scripts/migrate.sh"))
	if err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Errorf("executable bit lost: %v %v", info.Mode(), err)
	}
	info, err = os.Stat(filepath.Join(dest, "docker-compose.yml"))
	if err != nil || info.Mode().Perm()&0o111 != 0 {
		t.Errorf("unexpected executable bit: %v %v", info.Mode(), err)
	}
}

func TestExtractRejects(t *testing.T) {
	cases := map[string][]entry{
		"dotdot":        {file("../evil", "x", 0o644)},
		"nested dotdot": {file("a/../../evil", "x", 0o644)},
		"absolute":      {file("/etc/evil", "x", 0o644)},
		"symlink":       {{hdr: tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}}},
		"hardlink":      {{hdr: tar.Header{Name: "link", Typeflag: tar.TypeLink, Linkname: "other"}}},
		"device":        {{hdr: tar.Header{Name: "dev", Typeflag: tar.TypeChar}}},
		"fifo":          {{hdr: tar.Header{Name: "fifo", Typeflag: tar.TypeFifo}}},
		"too big":       {file("big", strings.Repeat("x", 2<<20), 0o644)},
		"too big total": {
			file("a", strings.Repeat("x", 600<<10), 0o644),
			file("b", strings.Repeat("x", 600<<10), 0o644),
		},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			dest := t.TempDir()
			if err := Extract(archive(t, entries...), dest, limits); err == nil {
				t.Fatal("Extract succeeded, want error")
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(dest), "evil")); err == nil {
				t.Fatal("file written outside dest")
			}
		})
	}
}

func TestExtractTooManyFiles(t *testing.T) {
	var entries []entry
	for i := 0; i < 5; i++ {
		entries = append(entries, file(strings.Repeat("f", i+1), "x", 0o644))
	}
	if err := Extract(archive(t, entries...), t.TempDir(), Limits{MaxUnpacked: 1 << 20, MaxFiles: 4}); err == nil {
		t.Fatal("Extract succeeded, want error")
	}
}

func TestExtractNotGzip(t *testing.T) {
	if err := Extract(strings.NewReader("not an archive"), t.TempDir(), limits); err == nil {
		t.Fatal("Extract succeeded, want error")
	}
}
