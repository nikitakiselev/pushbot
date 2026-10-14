package upgrade

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

type release struct {
	binary, unit string
	badChecksum  bool
}

type fakeGitHub struct {
	*httptest.Server
	downloads atomic.Int32
}

func newFakeGitHub(t *testing.T, latest string, releases map[string]release) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/me/pushbot/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name": %q}`, latest)
	})
	for tag, rel := range releases {
		archive := tgz(t, map[string]string{"pushbot": rel.binary, "pushbot.service": rel.unit})
		sum := sha256.Sum256(archive)
		if rel.badChecksum {
			sum[0] ^= 0xff
		}
		base := "/me/pushbot/releases/download/" + tag + "/"
		mux.HandleFunc("GET "+base+"pushbot_linux_amd64.tar.gz", func(w http.ResponseWriter, r *http.Request) {
			f.downloads.Add(1)
			w.Write(archive)
		})
		mux.HandleFunc("GET "+base+"checksums.txt", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "%s  pushbot_linux_arm64.tar.gz\n%s  pushbot_linux_amd64.tar.gz\n",
				strings.Repeat("0", 64), hex.EncodeToString(sum[:]))
		})
	}
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func tgz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body))})
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

type env struct {
	opts  Options
	calls []string
	out   *bytes.Buffer
}

// newEnv installs "old-binary" and "unit v1" and returns options pointing at gh.
func newEnv(t *testing.T, gh *fakeGitHub, current string, inactive bool) *env {
	t.Helper()
	dir := t.TempDir()
	e := &env{out: &bytes.Buffer{}}
	e.opts = Options{
		Current:      current,
		Repo:         "me/pushbot",
		APIBase:      gh.URL,
		DownloadBase: gh.URL,
		Arch:         "amd64",
		BinaryPath:   filepath.Join(dir, "pushbot"),
		UnitPath:     filepath.Join(dir, "pushbot.service"),
		Out:          e.out,
		Systemctl: func(args ...string) error {
			e.calls = append(e.calls, strings.Join(args, " "))
			if inactive && args[0] == "is-active" {
				return errors.New("exit status 3")
			}
			return nil
		},
	}
	os.WriteFile(e.opts.BinaryPath, []byte("old-binary"), 0o755)
	os.WriteFile(e.opts.UnitPath, []byte("unit v1"), 0o644)
	return e
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestUpgradeToLatest(t *testing.T) {
	gh := newFakeGitHub(t, "v1.2.0", map[string]release{"v1.2.0": {binary: "new-binary", unit: "unit v2"}})
	e := newEnv(t, gh, "1.1.0", false)

	if err := Run(context.Background(), e.opts); err != nil {
		t.Fatal(err)
	}
	if got := read(t, e.opts.BinaryPath); got != "new-binary" {
		t.Errorf("binary = %q", got)
	}
	if info, _ := os.Stat(e.opts.BinaryPath); info.Mode().Perm() != 0o755 {
		t.Errorf("binary mode = %v", info.Mode())
	}
	if got := read(t, e.opts.UnitPath); got != "unit v2" {
		t.Errorf("unit = %q", got)
	}
	if want := []string{"daemon-reload", "is-active --quiet pushbot", "restart pushbot"}; !reflect.DeepEqual(e.calls, want) {
		t.Errorf("systemctl calls = %q, want %q", e.calls, want)
	}
	for _, want := range []string{"1.1.0 → 1.2.0", "Checksum OK", "waits for running deploys"} {
		if !strings.Contains(e.out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, e.out.String())
		}
	}
}

func TestUpgradeAlreadyUpToDate(t *testing.T) {
	gh := newFakeGitHub(t, "v1.2.0", map[string]release{"v1.2.0": {binary: "new-binary", unit: "unit v2"}})
	e := newEnv(t, gh, "1.2.0", false)

	if err := Run(context.Background(), e.opts); err != nil {
		t.Fatal(err)
	}
	if gh.downloads.Load() != 0 || len(e.calls) != 0 || read(t, e.opts.BinaryPath) != "old-binary" {
		t.Errorf("up-to-date install was touched: downloads=%d calls=%q", gh.downloads.Load(), e.calls)
	}
	if !strings.Contains(e.out.String(), "already up to date") {
		t.Errorf("output:\n%s", e.out.String())
	}
}

func TestUpgradeToSpecificVersion(t *testing.T) {
	gh := newFakeGitHub(t, "v1.2.0", map[string]release{
		"v1.2.0": {binary: "new-binary", unit: "unit v2"},
		"v1.0.0": {binary: "older-binary", unit: "unit v1"},
	})
	for _, v := range []string{"v1.0.0", "1.0.0"} {
		e := newEnv(t, gh, "1.1.0", false)
		e.opts.Version = v
		if err := Run(context.Background(), e.opts); err != nil {
			t.Fatal(err)
		}
		if got := read(t, e.opts.BinaryPath); got != "older-binary" {
			t.Errorf("--version %s: binary = %q", v, got)
		}
		// The unit did not change, so no daemon-reload.
		if want := []string{"is-active --quiet pushbot", "restart pushbot"}; !reflect.DeepEqual(e.calls, want) {
			t.Errorf("--version %s: systemctl calls = %q, want %q", v, e.calls, want)
		}
	}
}

func TestUpgradeChecksumMismatch(t *testing.T) {
	gh := newFakeGitHub(t, "v1.2.0", map[string]release{"v1.2.0": {binary: "evil", unit: "unit v2", badChecksum: true}})
	e := newEnv(t, gh, "1.1.0", false)

	err := Run(context.Background(), e.opts)
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("err = %v, want checksum error", err)
	}
	if read(t, e.opts.BinaryPath) != "old-binary" || read(t, e.opts.UnitPath) != "unit v1" || len(e.calls) != 0 {
		t.Errorf("files or service changed after a checksum mismatch: calls=%q", e.calls)
	}
}

func TestUpgradeServiceNotRunning(t *testing.T) {
	gh := newFakeGitHub(t, "v1.2.0", map[string]release{"v1.2.0": {binary: "new-binary", unit: "unit v1"}})
	e := newEnv(t, gh, "1.1.0", true)

	if err := Run(context.Background(), e.opts); err != nil {
		t.Fatal(err)
	}
	if want := []string{"is-active --quiet pushbot"}; !reflect.DeepEqual(e.calls, want) {
		t.Errorf("systemctl calls = %q, want %q", e.calls, want)
	}
	if !strings.Contains(e.out.String(), "systemctl start pushbot") {
		t.Errorf("no hint to start the service:\n%s", e.out.String())
	}
}

func TestUpgradeUnknownVersion(t *testing.T) {
	gh := newFakeGitHub(t, "v1.2.0", map[string]release{})
	e := newEnv(t, gh, "1.1.0", false)
	e.opts.Version = "v9.9.9"

	err := Run(context.Background(), e.opts)
	if err == nil || !strings.Contains(err.Error(), "v9.9.9") {
		t.Fatalf("err = %v, want an error naming the version", err)
	}
	if read(t, e.opts.BinaryPath) != "old-binary" {
		t.Error("binary changed")
	}
}
