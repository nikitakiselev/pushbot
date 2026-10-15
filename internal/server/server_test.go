package server

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nikitakiselev/pushbot/internal/auth"
	"github.com/nikitakiselev/pushbot/internal/deploy"
	"github.com/nikitakiselev/pushbot/internal/oidctest"
)

type testServer struct {
	*httptest.Server
	srv    *Server
	issuer *oidctest.Issuer
	root   string
}

func newTestServer(t *testing.T, maxBundle int64) *testServer {
	t.Helper()
	iss := oidctest.NewIssuer(t)
	base := t.TempDir()
	d, err := deploy.New(deploy.Config{
		ProjectsRoot: filepath.Join(base, "apps"),
		StateDir:     filepath.Join(base, "state"),
		Timeout:      10 * time.Second,
		MaxUnpacked:  1 << 20,
		LogRetention: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	v := auth.NewVerifier(iss.URL, "pushbot", []auth.Rule{{Owner: "nikitakiselev", OwnerID: "42", RepoGlob: "*"}})
	s := New(v, d, maxBundle, filepath.Join(base, "state", "tmp"), nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return &testServer{Server: ts, srv: s, issuer: iss, root: filepath.Join(base, "apps")}
}

type field struct{ name, value string }

func tgz(t *testing.T, files map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.String()
}

func (ts *testServer) post(t *testing.T, token string, fields ...field) *http.Response {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, f := range fields {
		if f.name == "bundle" {
			w, _ := mw.CreateFormFile("bundle", "bundle.tgz")
			w.Write([]byte(f.value))
		} else {
			mw.WriteField(f.name, f.value)
		}
	}
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/deploy", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDeployOK(t *testing.T) {
	ts := newTestServer(t, 1<<20)
	resp := ts.post(t, ts.issuer.Token(t, nil),
		field{"manifest", "files: [docker-compose.yml]\nsteps:\n  - run: echo \"tag=$IMAGE_TAG\""},
		field{"env", "IMAGE_TAG=abc\n"},
		field{"bundle", tgz(t, map[string]string{"docker-compose.yml": "services: {}\n"})},
	)
	out := readAll(t, resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, out)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("content-type = %q", ct)
	}
	if !strings.Contains(out, "tag=abc\n") || !strings.Contains(out, "\nPUSHBOT: success (") {
		t.Errorf("output:\n%s", out)
	}
	if got, _ := os.ReadFile(filepath.Join(ts.root, "my-service", "docker-compose.yml")); string(got) != "services: {}\n" {
		t.Errorf("compose file = %q", got)
	}
}

func TestDeploySecrets(t *testing.T) {
	ts := newTestServer(t, 1<<20)
	resp := ts.post(t, ts.issuer.Token(t, nil),
		field{"manifest", "env:\n  secrets: [DB_PASSWORD]\nsteps:\n  - run: 'true'"},
		field{"secrets", `{"DB_PASSWORD": "it is hunter2"}`},
	)
	out := readAll(t, resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(out, "\nPUSHBOT: success (") {
		t.Fatalf("status %d, output:\n%s", resp.StatusCode, out)
	}
	got, _ := os.ReadFile(filepath.Join(ts.root, "my-service", ".env"))
	if want := "# pushbot: secrets\nDB_PASSWORD='it is hunter2'\n"; string(got) != want {
		t.Errorf(".env = %q, want %q", got, want)
	}
}

func TestDeployFailureIsReportedInBody(t *testing.T) {
	ts := newTestServer(t, 1<<20)
	resp := ts.post(t, ts.issuer.Token(t, nil), field{"manifest", "steps:\n  - run: exit 7"})
	out := readAll(t, resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.HasSuffix(out, "PUSHBOT: failed: step 1/1 exited with code 7\n") {
		t.Errorf("status %d, output:\n%s", resp.StatusCode, out)
	}
}

func TestDeployStreams(t *testing.T) {
	ts := newTestServer(t, 1<<20)
	start := time.Now()
	resp := ts.post(t, ts.issuer.Token(t, nil), field{"manifest", "steps:\n  - run: echo first; sleep 1; echo second"})
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if sc.Text() == "first" {
			break
		}
	}
	if elapsed := time.Since(start); elapsed > 800*time.Millisecond {
		t.Errorf("first line arrived after %v: output is buffered", elapsed)
	}
	rest := readAll(t, resp.Body)
	if !strings.Contains(rest, "second") {
		t.Errorf("rest of output: %s", rest)
	}
}

func TestDeployRejections(t *testing.T) {
	ts := newTestServer(t, 2048)
	random := make([]byte, 8192)
	rand.Read(random)
	m := field{"manifest", "steps:\n  - run: 'true'"}

	cases := []struct {
		name   string
		token  string
		fields []field
		code   int
	}{
		{"no token", "", []field{m}, 401},
		{"bad token", "garbage", []field{m}, 401},
		{"wrong audience", ts.issuer.Token(t, map[string]any{"aud": "other"}), []field{m}, 401},
		{"foreign repo", ts.issuer.Token(t, map[string]any{"repository": "x/y", "repository_owner_id": "9"}), []field{m}, 403},
		{"no manifest", ts.issuer.Token(t, nil), []field{{"env", "A=1"}}, 400},
		{"bad manifest", ts.issuer.Token(t, nil), []field{{"manifest", "nmae: x"}}, 400},
		{"bad env", ts.issuer.Token(t, nil), []field{m, {"env", "not env"}}, 400},
		{"unknown field", ts.issuer.Token(t, nil), []field{m, {"nonsense", "x"}}, 400},
		{"secrets not json", ts.issuer.Token(t, nil), []field{m, {"secrets", "A=1"}}, 400},
		{"secret not a string", ts.issuer.Token(t, nil), []field{m, {"secrets", `{"A": 1}`}}, 400},
		{"secret not declared", ts.issuer.Token(t, nil), []field{m, {"secrets", `{"A": "1"}`}}, 400},
		{"half registry", ts.issuer.Token(t, nil), []field{m, {"registry_user", "me"}}, 400},
		{"missing file", ts.issuer.Token(t, nil), []field{{"manifest", "files: [a.yml]"}}, 400},
		{"bundle too big", ts.issuer.Token(t, nil), []field{m, {"bundle", string(random)}}, 413},
		{"bad bundle", ts.issuer.Token(t, nil), []field{{"manifest", "files: [a]"}, {"bundle", "not gzip"}}, 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := ts.post(t, c.token, c.fields...)
			body := readAll(t, resp.Body)
			if resp.StatusCode != c.code {
				t.Errorf("status = %d, want %d (%s)", resp.StatusCode, c.code, body)
			}
			if !strings.HasPrefix(body, "pushbot: ") {
				t.Errorf("body = %q", body)
			}
		})
	}
}

func TestDeployNameTaken(t *testing.T) {
	ts := newTestServer(t, 1<<20)
	m := field{"manifest", "name: shared\nsteps:\n  - run: 'true'"}
	resp := ts.post(t, ts.issuer.Token(t, nil), m)
	readAll(t, resp.Body)
	other := map[string]any{"repository": "nikitakiselev/other", "repository_id": "2002"}
	resp = ts.post(t, ts.issuer.Token(t, other), m)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403: %s", resp.StatusCode, readAll(t, resp.Body))
	}
}

func TestDrain(t *testing.T) {
	ts := newTestServer(t, 1<<20)
	ts.srv.Drain()
	resp := ts.post(t, ts.issuer.Token(t, nil), field{"manifest", ""})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

type rejections struct {
	mu    sync.Mutex
	codes []int
}

func (r *rejections) RequestRejected(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.codes = append(r.codes, code)
}

func TestRejectionObserver(t *testing.T) {
	ts := newTestServer(t, 1<<20)
	obs := &rejections{}
	ts.srv.WithObserver(obs)
	readAll(t, ts.post(t, "", field{"manifest", ""}).Body)
	readAll(t, ts.post(t, ts.issuer.Token(t, nil), field{"manifest", "nmae: x"}).Body)
	readAll(t, ts.post(t, ts.issuer.Token(t, nil), field{"manifest", "steps:\n  - run: 'true'"}).Body)
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if !reflect.DeepEqual(obs.codes, []int{401, 400}) {
		t.Errorf("rejections = %v, want [401 400]", obs.codes)
	}
}

func TestHealthz(t *testing.T) {
	ts := newTestServer(t, 1<<20)
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if body := readAll(t, resp.Body); resp.StatusCode != 200 || body != "ok\n" {
		t.Errorf("healthz = %d %q", resp.StatusCode, body)
	}
}
