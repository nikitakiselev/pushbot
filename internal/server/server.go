// Package server exposes the deploy API over HTTP.
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"sync/atomic"

	"github.com/nikitakiselev/pushbot/internal/auth"
	"github.com/nikitakiselev/pushbot/internal/deploy"
	"github.com/nikitakiselev/pushbot/internal/envfile"
	"github.com/nikitakiselev/pushbot/internal/manifest"
)

const (
	maxTextPart = 64 << 10 // manifest, env
	maxCredPart = 4 << 10  // registry_user, registry_token
	// formOverhead allows for multipart framing and the text parts on top of the bundle.
	formOverhead = 1 << 20
)

// TokenVerifier is satisfied by *auth.Verifier.
type TokenVerifier interface {
	Verify(ctx context.Context, rawToken string) (*auth.Claims, error)
}

// Server handles POST /v1/deploy and GET /healthz.
type Server struct {
	verifier  TokenVerifier
	deployer  *deploy.Deployer
	maxBundle int64
	tmpDir    string // where uploaded bundles are spooled
	logger    *slog.Logger
	draining  atomic.Bool
}

func New(v TokenVerifier, d *deploy.Deployer, maxBundle int64, tmpDir string, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{verifier: v, deployer: d, maxBundle: maxBundle, tmpDir: tmpDir, logger: logger}
}

// Drain makes new deploy requests fail with 503; running deploys continue.
func (s *Server) Drain() {
	s.draining.Store(true)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/deploy", s.handleDeploy)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok\n")
	})
	return mux
}

// httpError is a request failure reported before the deploy starts.
type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return e.msg }

func fail(code int, format string, args ...any) *httpError {
	return &httpError{code: code, msg: fmt.Sprintf(format, args...)}
}

func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	job, claims, herr := s.prepare(w, r)
	if herr != nil {
		attrs := []any{"code", herr.code, "err", herr.msg, "forwarded_for", r.Header.Get("X-Forwarded-For")}
		if claims != nil {
			attrs = append(attrs, "repository", claims.Repository)
		}
		s.logger.Warn("deploy request rejected", attrs...)
		http.Error(w, "pushbot: "+herr.msg, herr.code)
		return
	}
	defer job.Cleanup()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	job.Run(&flushWriter{w: w, rc: http.NewResponseController(w)})
}

func (s *Server) prepare(w http.ResponseWriter, r *http.Request) (*deploy.Job, *auth.Claims, *httpError) {
	if s.draining.Load() {
		return nil, nil, fail(http.StatusServiceUnavailable, "shutting down, retry later")
	}

	scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	if !strings.EqualFold(scheme, "Bearer") || token == "" {
		return nil, nil, fail(http.StatusUnauthorized, "missing bearer token")
	}
	claims, err := s.verifier.Verify(r.Context(), token)
	if errors.Is(err, auth.ErrForbidden) {
		return nil, nil, fail(http.StatusForbidden, "%v", err)
	}
	if err != nil {
		return nil, nil, fail(http.StatusUnauthorized, "%v", err)
	}

	r.Body = http.MaxBytesReader(w, r.Body, s.maxBundle+formOverhead)
	f, herr := s.readForm(r)
	if f != nil && f.bundlePath != "" {
		defer os.Remove(f.bundlePath)
	}
	if herr != nil {
		return nil, claims, herr
	}

	m, err := manifest.Parse(f.manifest)
	if err != nil {
		return nil, claims, fail(http.StatusBadRequest, "%v", err)
	}
	vars, err := envfile.Parse(f.env)
	if err != nil {
		return nil, claims, fail(http.StatusBadRequest, "%v", err)
	}
	var registry *deploy.Registry
	switch {
	case f.registryUser != "" && f.registryToken != "":
		registry = &deploy.Registry{User: f.registryUser, Token: f.registryToken}
	case f.registryUser != "" || f.registryToken != "":
		return nil, claims, fail(http.StatusBadRequest, "registry_user and registry_token must be sent together")
	}

	job, err := s.deployer.Prepare(deploy.Request{
		Claims: claims, Manifest: m, Env: vars, BundlePath: f.bundlePath, Registry: registry,
	})
	switch {
	case errors.Is(err, deploy.ErrBadRequest):
		return nil, claims, fail(http.StatusBadRequest, "%v", err)
	case errors.Is(err, deploy.ErrForbidden):
		return nil, claims, fail(http.StatusForbidden, "%v", err)
	case err != nil:
		s.logger.Error("prepare deploy", "err", err)
		return nil, claims, fail(http.StatusInternalServerError, "internal error, see the pushbot journal")
	}
	return job, claims, nil
}

type form struct {
	manifest      []byte
	env           string
	registryUser  string
	registryToken string
	bundlePath    string
}

func (s *Server) readForm(r *http.Request) (*form, *httpError) {
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, fail(http.StatusBadRequest, "expected multipart/form-data: %v", err)
	}
	f := &form{}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return f, bodyError(err)
		}
		if herr := s.readPart(f, part); herr != nil {
			return f, herr
		}
	}
	if f.manifest == nil {
		return f, fail(http.StatusBadRequest, "missing manifest field")
	}
	return f, nil
}

func (s *Server) readPart(f *form, part *multipart.Part) *httpError {
	defer part.Close()
	name := part.FormName()
	switch name {
	case "manifest":
		data, herr := readLimited(part, name, maxTextPart)
		f.manifest = data
		return herr
	case "env":
		data, herr := readLimited(part, name, maxTextPart)
		f.env = string(data)
		return herr
	case "registry_user", "registry_token":
		data, herr := readLimited(part, name, maxCredPart)
		if name == "registry_user" {
			f.registryUser = strings.TrimSpace(string(data))
		} else {
			f.registryToken = strings.TrimSpace(string(data))
		}
		return herr
	case "bundle":
		return s.spoolBundle(f, part)
	default:
		return fail(http.StatusBadRequest, "unknown field %q", name)
	}
}

func (s *Server) spoolBundle(f *form, part *multipart.Part) *httpError {
	if f.bundlePath != "" {
		return fail(http.StatusBadRequest, "bundle sent twice")
	}
	tmp, err := os.CreateTemp(s.tmpDir, "upload-*.tgz")
	if err != nil {
		s.logger.Error("spool bundle", "err", err)
		return fail(http.StatusInternalServerError, "internal error, see the pushbot journal")
	}
	f.bundlePath = tmp.Name()
	n, err := io.Copy(tmp, io.LimitReader(part, s.maxBundle+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return bodyError(err)
	}
	if n > s.maxBundle {
		return fail(http.StatusRequestEntityTooLarge, "bundle exceeds %d bytes", s.maxBundle)
	}
	return nil
}

func readLimited(r io.Reader, name string, limit int64) ([]byte, *httpError) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, bodyError(err)
	}
	if int64(len(data)) > limit {
		return nil, fail(http.StatusRequestEntityTooLarge, "%s exceeds %d bytes", name, limit)
	}
	return data, nil
}

func bodyError(err error) *httpError {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return fail(http.StatusRequestEntityTooLarge, "request body exceeds %d bytes", mbe.Limit)
	}
	return fail(http.StatusBadRequest, "read request: %v", err)
}

// flushWriter flushes after every write so log lines reach the client immediately.
type flushWriter struct {
	w  io.Writer
	rc *http.ResponseController
}

func (f *flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err == nil {
		err = f.rc.Flush()
	}
	return n, err
}
