// Package deploy runs the deploy pipeline: checks that can still be reported
// as HTTP errors (Prepare), then lock → files → .env → steps (Job.Run).
package deploy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nikitakiselev/pushbot/internal/auth"
	"github.com/nikitakiselev/pushbot/internal/bundle"
	"github.com/nikitakiselev/pushbot/internal/envfile"
	"github.com/nikitakiselev/pushbot/internal/manifest"
	"github.com/nikitakiselev/pushbot/internal/runner"
	"github.com/nikitakiselev/pushbot/internal/state"
)

var (
	// ErrBadRequest: the request is invalid (HTTP 400).
	ErrBadRequest = errors.New("bad request")
	// ErrForbidden: the project name belongs to another repository (HTTP 403).
	ErrForbidden = errors.New("forbidden")
)

const stepPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// Config of the deployer.
type Config struct {
	ProjectsRoot string
	StateDir     string
	Timeout      time.Duration // whole deploy, including waiting for the project lock
	MaxUnpacked  int64
	MaxFiles     int // default 10000
	LogRetention int
	Runner       runner.Options
	Logger       *slog.Logger // one line per deploy; default slog.Default()
}

// Registry credentials for pulling private images from GHCR.
type Registry struct {
	User  string
	Token string
}

// Request is a parsed, authenticated deploy request.
type Request struct {
	Claims     *auth.Claims
	Manifest   *manifest.Manifest
	Env        []envfile.Var
	BundlePath string // "" when no files were sent
	Registry   *Registry
}

// Result of a deploy.
type Result struct {
	Success  bool
	Reason   string // why it failed
	Duration time.Duration
}

// Deployer prepares and runs deploys. Deploys of one project are serialized.
type Deployer struct {
	cfg      Config
	projects *state.Projects

	mu    sync.Mutex
	locks map[string]chan struct{}
}

// New creates the deployer and its directories.
func New(cfg Config) (*Deployer, error) {
	if cfg.MaxFiles == 0 {
		cfg.MaxFiles = 10000
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	for _, dir := range []string{cfg.ProjectsRoot, filepath.Join(cfg.StateDir, "tmp"), filepath.Join(cfg.StateDir, "logs")} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, err
		}
	}
	return &Deployer{
		cfg:      cfg,
		projects: state.NewProjects(filepath.Join(cfg.StateDir, "projects.json")),
		locks:    map[string]chan struct{}{},
	}, nil
}

// Job is a prepared deploy. Call Run once; Cleanup is safe to call any time.
type Job struct {
	d         *Deployer
	req       Request
	name      string
	dir       string
	tmpDir    string
	copies    []copyOp
	dirExists bool
	claimed   bool // the name was claimed by this deploy
	cleanOnce sync.Once
}

// Prepare validates the request, extracts the bundle and claims the project name.
// Errors wrap ErrBadRequest or ErrForbidden when caused by the request.
func (d *Deployer) Prepare(req Request) (*Job, error) {
	name := req.Manifest.Name
	if name == "" {
		name = manifest.DefaultName(req.Claims.Repository)
		if err := manifest.ValidateName(name); err != nil {
			return nil, fmt.Errorf("%w: %v; set name in pushbot.yml", ErrBadRequest, err)
		}
	}
	j := &Job{d: d, req: req, name: name, dir: filepath.Join(d.cfg.ProjectsRoot, name)}

	tmpDir, err := os.MkdirTemp(filepath.Join(d.cfg.StateDir, "tmp"), "deploy-*")
	if err != nil {
		return nil, err
	}
	j.tmpDir = tmpDir
	if err := j.extract(); err != nil {
		j.Cleanup()
		return nil, err
	}

	_, statErr := os.Stat(j.dir)
	j.dirExists = statErr == nil
	j.claimed, err = d.projects.Claim(name, req.Claims.RepositoryID, req.Claims.Repository, time.Now())
	if errors.Is(err, state.ErrNameTaken) {
		j.Cleanup()
		return nil, fmt.Errorf("%w: %v", ErrForbidden, err)
	}
	if err != nil {
		j.Cleanup()
		return nil, err
	}
	return j, nil
}

func (j *Job) extract() error {
	bundleDir := filepath.Join(j.tmpDir, "bundle")
	if err := os.Mkdir(bundleDir, 0o700); err != nil {
		return err
	}
	if j.req.BundlePath != "" {
		f, err := os.Open(j.req.BundlePath)
		if err != nil {
			return err
		}
		defer f.Close()
		lim := bundle.Limits{MaxUnpacked: j.d.cfg.MaxUnpacked, MaxFiles: j.d.cfg.MaxFiles}
		if err := bundle.Extract(f, bundleDir, lim); err != nil {
			return fmt.Errorf("%w: %v", ErrBadRequest, err)
		}
	}
	copies, err := planCopies(bundleDir, j.req.Manifest.Files)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	j.copies = copies
	return nil
}

// Cleanup removes the job's temporary files (extracted bundle, docker config).
func (j *Job) Cleanup() {
	j.cleanOnce.Do(func() {
		if j.tmpDir != "" {
			_ = os.RemoveAll(j.tmpDir)
		}
	})
}

// Run executes the deploy, streaming the log to w. It runs to completion even if
// writes to w fail. The last line written is the result.
func (j *Job) Run(w io.Writer) Result {
	defer j.Cleanup()
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), j.d.cfg.Timeout)
	defer cancel()

	c := j.req.Claims
	sha7 := c.SHA
	if len(sha7) > 7 {
		sha7 = sha7[:7]
	}
	out := &output{client: w, clientOK: true, mask: envfile.NewMasker(j.req.Env)}
	logDir := filepath.Join(j.d.cfg.StateDir, "logs", j.name)
	if f, err := createLogFile(logDir, start, sha7); err != nil {
		j.d.cfg.Logger.Error("cannot create deploy log", "project", j.name, "err", err)
	} else {
		out.file = f
	}

	res := j.run(ctx, out, sha7)
	res.Duration = time.Since(start)
	if res.Success {
		out.linef("PUSHBOT: success (%.1fs)", res.Duration.Seconds())
	} else {
		out.linef("PUSHBOT: failed: %s", res.Reason)
	}
	out.close()

	result := "success"
	if !res.Success {
		result = "failed: " + res.Reason
	}
	j.d.cfg.Logger.Info("deploy finished", "project", j.name, "repository", c.Repository, "sha", sha7,
		"run_id", c.RunID, "result", result, "duration", res.Duration.Round(100*time.Millisecond))
	if err := rotateLogs(logDir, j.d.cfg.LogRetention); err != nil {
		j.d.cfg.Logger.Error("cannot rotate deploy logs", "project", j.name, "err", err)
	}
	return res
}

func (j *Job) run(ctx context.Context, out *output, sha7 string) Result {
	c := j.req.Claims
	out.linef("==> %s@%s (run #%s) → %s", c.Repository, sha7, c.RunID, j.dir)
	if j.claimed {
		out.linef("==> project name %q is now bound to %s", j.name, c.Repository)
		if j.dirExists {
			out.linef("==> warning: %s already existed and is now managed by pushbot", j.dir)
		}
	}

	unlock, err := j.d.lock(ctx, j.name, func() { out.line("==> waiting for previous deploy...") })
	if err != nil {
		return Result{Reason: "timeout"}
	}
	defer unlock()

	if err := os.MkdirAll(j.dir, 0o750); err != nil {
		return Result{Reason: "create project directory: " + err.Error()}
	}
	out.linef("==> files: %s", j.filesSummary())
	for _, op := range j.copies {
		if err := copyFile(op.from, filepath.Join(j.dir, filepath.FromSlash(op.to)), op.mode); err != nil {
			return Result{Reason: fmt.Sprintf("copy %s: %v", op.to, err)}
		}
	}

	if err := envfile.Write(filepath.Join(j.dir, ".env"), j.req.Env); err != nil {
		return Result{Reason: "write .env: " + err.Error()}
	}
	out.linef("==> .env: %s", j.envSummary())

	env, err := j.stepEnv()
	if err != nil {
		return Result{Reason: "registry auth: " + err.Error()}
	}
	steps := j.req.Manifest.Steps
	for i, s := range steps {
		label := fmt.Sprintf("[%d/%d]", i+1, len(steps))
		out.linef("==> %s %s", label, summary(s.Run))
		res, err := runner.Run(ctx, runner.Step{Script: s.Run, Dir: j.dir, Env: env, Label: label}, j.d.cfg.Runner, out.line)
		switch {
		case err != nil:
			return Result{Reason: fmt.Sprintf("step %d/%d: %v", i+1, len(steps), err)}
		case res.TimedOut:
			return Result{Reason: "timeout"}
		case res.ExitCode != 0:
			return Result{Reason: fmt.Sprintf("step %d/%d exited with code %d", i+1, len(steps), res.ExitCode)}
		}
	}
	return Result{Success: true}
}

// lock serializes deploys per project. onWait is called if the lock is busy.
func (d *Deployer) lock(ctx context.Context, name string, onWait func()) (func(), error) {
	d.mu.Lock()
	ch, ok := d.locks[name]
	if !ok {
		ch = make(chan struct{}, 1)
		d.locks[name] = ch
	}
	d.mu.Unlock()

	select {
	case ch <- struct{}{}:
	default:
		onWait()
		select {
		case ch <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return func() { <-ch }, nil
}

// stepEnv builds the complete step environment; PUSHBOT_* come last so env
// input cannot override them.
func (j *Job) stepEnv() ([]string, error) {
	c := j.req.Claims
	env := []string{"PATH=" + stepPath, "HOME=" + j.d.cfg.StateDir}
	for _, v := range j.req.Env {
		env = append(env, v.Key+"="+v.Value)
	}
	env = append(env,
		"PUSHBOT_PROJECT="+j.name,
		"PUSHBOT_REPOSITORY="+c.Repository,
		"PUSHBOT_SHA="+c.SHA,
		"PUSHBOT_REF="+c.Ref,
		"PUSHBOT_ACTOR="+c.Actor,
		"PUSHBOT_RUN_ID="+c.RunID,
		"PUSHBOT_RUN_URL=https://github.com/"+c.Repository+"/actions/runs/"+c.RunID,
	)
	if r := j.req.Registry; r != nil {
		dir := filepath.Join(j.tmpDir, "docker")
		if err := writeDockerConfig(dir, r); err != nil {
			return nil, err
		}
		env = append(env, "DOCKER_CONFIG="+dir)
	}
	return env, nil
}

func writeDockerConfig(dir string, r *Registry) error {
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	auth := base64.StdEncoding.EncodeToString([]byte(r.User + ":" + r.Token))
	data, err := json.Marshal(map[string]any{"auths": map[string]any{"ghcr.io": map[string]string{"auth": auth}}})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "config.json"), data, 0o600)
}

func (j *Job) filesSummary() string {
	if len(j.req.Manifest.Files) == 0 {
		return "none"
	}
	var names []string
	for _, f := range j.req.Manifest.Files {
		name := f.Src
		if info, err := os.Stat(filepath.Join(j.tmpDir, "bundle", filepath.FromSlash(f.Src))); err == nil && info.IsDir() {
			name += "/"
		}
		if f.Dest != f.Src {
			name += " → " + f.Dest
		}
		names = append(names, name)
	}
	return fmt.Sprintf("%s (%d files)", strings.Join(names, ", "), len(j.copies))
}

func (j *Job) envSummary() string {
	if len(j.req.Env) == 0 {
		return "empty"
	}
	keys := make([]string, len(j.req.Env))
	for i, v := range j.req.Env {
		keys[i] = v.Key
	}
	return strings.Join(keys, ", ")
}

// summary is the first line of a script, marked if more lines follow.
func summary(script string) string {
	script = strings.TrimSpace(script)
	first, rest, _ := strings.Cut(script, "\n")
	if rest != "" {
		return first + " …"
	}
	return first
}
