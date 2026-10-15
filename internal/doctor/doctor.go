// Package doctor checks that the server is ready to run deploys.
package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nikitakiselev/pushbot/internal/auth"
	"github.com/nikitakiselev/pushbot/internal/config"
	"github.com/nikitakiselev/pushbot/internal/state"
	"github.com/nikitakiselev/pushbot/internal/upgrade"
)

// Status of a check.
type Status int

const (
	OK Status = iota
	Warn
	Fail
)

// Result of one check. Hint says how to fix a warning or a failure.
type Result struct {
	Status Status
	Text   string
	Hint   string
}

// ErrNoDaemonUser is returned by Options.AsDaemonUser when the pushbot user does not exist.
var ErrNoDaemonUser = errors.New("user pushbot does not exist")

// Options for Run.
type Options struct {
	ConfigPath string
	URL        string // optional public address, checked through the reverse proxy
	Version    string // running pushbot version, e.g. "1.2.0"
	Repo       string // owner/name hosting the releases
	APIBase    string // default https://api.github.com
	Token      string // optional GitHub token for API requests
	Client     *http.Client
	// Systemctl runs systemctl and returns its trimmed stdout.
	Systemctl func(args ...string) (string, error)
	// AsDaemonUser runs a command as the pushbot user with the docker group, the
	// way pushbot.service runs deploy steps, and returns its trimmed output.
	AsDaemonUser func(name string, args ...string) (string, error)
	Now          func() time.Time
}

// Run runs the checks, reports each result as soon as it is known and returns
// the number of failures.
func Run(ctx context.Context, o Options, report func(Result)) int {
	if o.APIBase == "" {
		o.APIBase = "https://api.github.com"
	}
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 10 * time.Second}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	failures := 0
	add := func(r Result) {
		if r.Status == Fail {
			failures++
		}
		report(r)
	}

	cfg, err := config.Load(o.ConfigPath)
	if err != nil {
		add(Result{Fail, "config: " + err.Error(), "fix the file, then: sudo systemctl restart pushbot"})
	} else {
		add(Result{OK, "config " + o.ConfigPath + " is valid", ""})
	}
	service, running := checkService(o)
	add(service)
	// A stopped service would fail these too; its line already says what to do.
	if running && cfg != nil {
		add(checkDaemon(ctx, o, cfg.Listen))
		if cfg.MetricsListen != "" {
			add(checkMetrics(ctx, o, cfg.MetricsListen))
		}
	}
	if running && o.URL != "" {
		add(checkURL(ctx, o))
	}
	add(checkDocker(o))
	if cfg != nil {
		for _, r := range checkDirs(o, cfg.ProjectsRoot, cfg.StateDir) {
			add(r)
		}
	}
	if cfg != nil {
		oidcResult, offset, offsetOK := checkOIDC(ctx, o, cfg.OIDCIssuer)
		add(oidcResult)
		for _, r := range checkOwners(ctx, o, cfg) {
			add(r)
		}
		if offsetOK {
			add(checkClock(offset))
		}
	}
	add(checkVersion(ctx, o))
	return failures
}

// checkService also reports whether the service is running.
func checkService(o Options) (Result, bool) {
	enabled, err := o.Systemctl("is-enabled", "pushbot")
	if errors.Is(err, exec.ErrNotFound) {
		return Result{Fail, "systemctl not found: pushbot runs as a systemd service", ""}, false
	}
	if enabled == "" || enabled == "not-found" {
		return Result{Fail, "pushbot.service is not installed", "run the install command from the README"}, false
	}
	active, _ := o.Systemctl("is-active", "pushbot")
	switch {
	case active == "active" && enabled == "enabled":
		return Result{OK, "service pushbot is active and enabled", ""}, true
	case active == "active":
		return Result{Warn, "service pushbot is " + enabled + " and will not start after a reboot", "sudo systemctl enable pushbot"}, true
	case active == "inactive":
		return Result{Fail, "service pushbot is not running", "sudo systemctl start pushbot"}, false
	default:
		return Result{Fail, "service pushbot is " + active, "see the log: sudo journalctl -u pushbot -n 50"}, false
	}
}

func checkDaemon(ctx context.Context, o Options, listen string) Result {
	body, err := get(ctx, o.Client, "http://"+config.DialAddr(listen)+"/healthz")
	if err == nil && strings.TrimSpace(body) != "ok" {
		err = fmt.Errorf("unexpected response %q", body)
	}
	if err != nil {
		return Result{Fail, "daemon on " + listen + " does not answer /healthz: " + err.Error(), "see the log: sudo journalctl -u pushbot -n 50"}
	}
	return Result{OK, "daemon on " + listen + " answers /healthz", ""}
}

func checkMetrics(ctx context.Context, o Options, listen string) Result {
	if _, err := get(ctx, o.Client, "http://"+config.DialAddr(listen)+"/metrics"); err != nil {
		return Result{Fail, "metrics on " + listen + " do not answer: " + err.Error(), "see the log: sudo journalctl -u pushbot -n 50"}
	}
	return Result{OK, "metrics on " + listen + " answer /metrics", ""}
}

func checkURL(ctx context.Context, o Options) Result {
	u := strings.TrimSuffix(o.URL, "/")
	if !strings.Contains(u, "://") {
		u = "https://" + u
	}
	u += "/healthz"
	body, err := get(ctx, o.Client, u)
	if err == nil && strings.TrimSpace(body) != "ok" {
		err = fmt.Errorf("unexpected response %q", body)
	}
	if err != nil {
		return Result{Fail, u + ": " + err.Error(), "check the reverse proxy, see \"Reverse proxy\" in the README"}
	}
	return Result{OK, u + " answers through the proxy", ""}
}

func checkDocker(o Options) Result {
	server, err := o.AsDaemonUser("docker", "version", "--format", "{{.Server.Version}}")
	if err != nil {
		return dockerFailure(server, err)
	}
	compose, err := o.AsDaemonUser("docker", "compose", "version", "--short")
	if err != nil {
		return Result{Fail, "docker compose does not work for user pushbot: " + lastLine(compose, err), "install the Docker Compose plugin (docker-compose-plugin)"}
	}
	return Result{OK, fmt.Sprintf("user pushbot can run docker (Docker %s, Compose %s)", server, compose), ""}
}

func dockerFailure(out string, err error) Result {
	msg := lastLine(out, err)
	switch {
	case errors.Is(err, ErrNoDaemonUser):
		return Result{Fail, "user pushbot does not exist", "run the install command from the README"}
	case errors.Is(err, exec.ErrNotFound):
		return Result{Fail, "docker is not installed", "https://docs.docker.com/engine/install/"}
	case strings.Contains(msg, "permission denied"):
		return Result{Fail, "user pushbot has no access to the Docker socket", "the socket must belong to the docker group: ls -l /var/run/docker.sock"}
	case strings.Contains(msg, "Cannot connect to the Docker daemon"):
		return Result{Fail, "the Docker daemon is not running", "sudo systemctl start docker"}
	default:
		return Result{Fail, "docker does not work for user pushbot: " + msg, ""}
	}
}

func lastLine(out string, err error) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if l := strings.TrimSpace(lines[len(lines)-1]); l != "" {
		return l
	}
	return err.Error()
}

func checkDirs(o Options, dirs ...string) []Result {
	var problems []Result
	for _, dir := range dirs {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			problems = append(problems, Result{Fail, dir + " does not exist", "sudo install -d -o pushbot -g pushbot -m 0750 " + dir})
			continue
		}
		if _, err := o.AsDaemonUser("test", "-w", dir); err != nil {
			if errors.Is(err, ErrNoDaemonUser) {
				return nil // already reported by the docker check
			}
			problems = append(problems, Result{Fail, dir + " is not writable by user pushbot", "sudo chown pushbot:pushbot " + dir})
		}
	}
	if len(problems) > 0 {
		return problems
	}
	return []Result{{OK, strings.Join(dirs, " and ") + " are writable by user pushbot", ""}}
}

// checkOIDC fetches the token signing keys the daemon uses and measures the
// clock offset against the issuer's Date header.
func checkOIDC(ctx context.Context, o Options, issuer string) (r Result, offset time.Duration, offsetOK bool) {
	keysURL := strings.TrimSuffix(issuer, "/") + "/.well-known/jwks"
	host := issuer
	if u, err := url.Parse(issuer); err == nil && u.Host != "" {
		host = u.Host
	}
	fail := func(err error) (Result, time.Duration, bool) {
		return Result{Fail, "cannot fetch OIDC keys from " + keysURL + ": " + err.Error(), "check DNS and outbound HTTPS on the server"}, 0, false
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, keysURL, nil)
	if err != nil {
		return fail(err)
	}
	start := o.Now()
	resp, err := o.Client.Do(req)
	if err != nil {
		return fail(short(err))
	}
	defer resp.Body.Close()
	end := o.Now()
	if resp.StatusCode != http.StatusOK {
		return fail(errors.New(resp.Status))
	}
	var keys struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&keys); err != nil || len(keys.Keys) == 0 {
		return fail(errors.New("no keys in the response"))
	}
	r = Result{OK, "GitHub OIDC keys are reachable (" + host + ")", ""}

	date, err := http.ParseTime(resp.Header.Get("Date"))
	if err != nil {
		return r, 0, false
	}
	// Date has a one-second resolution; compare its middle with the middle of the request.
	local := start.Add(end.Sub(start) / 2)
	return r, date.Add(500 * time.Millisecond).Sub(local), true
}

// checkClock reports how far the local clock is from GitHub's. Tokens are
// checked with a 60s margin.
func checkClock(offset time.Duration) Result {
	abs := offset.Abs().Round(time.Second)
	if abs < 2*time.Second {
		return Result{OK, "clock matches GitHub", ""}
	}
	dir := "behind"
	if offset < 0 {
		dir = "ahead of"
	}
	text := fmt.Sprintf("clock is %s %s GitHub", abs, dir)
	hint := "turn on time sync: sudo timedatectl set-ntp true"
	switch {
	case abs >= 60*time.Second:
		return Result{Fail, text + ", deploy tokens will be rejected", hint}
	case abs >= 10*time.Second:
		return Result{Warn, text, hint}
	default:
		return Result{OK, text, ""}
	}
}

// checkOwners resolves the allow-list owners and compares their IDs with the
// ones the daemon saved at start (owners.json).
func checkOwners(ctx context.Context, o Options, cfg *config.Config) []Result {
	var owners []string
	patterns := map[string][]string{}
	for _, rule := range cfg.Allow {
		if patterns[rule.Owner] == nil {
			owners = append(owners, rule.Owner)
		}
		patterns[rule.Owner] = append(patterns[rule.Owner], rule.Owner+"/"+rule.RepoGlob)
	}
	saved, _ := state.LoadOwners(filepath.Join(cfg.StateDir, "owners.json"))
	resolver := &auth.OwnerResolver{APIBase: o.APIBase, Client: o.Client, Token: o.Token}

	var results []Result
	for _, owner := range owners {
		label := "allow " + strings.Join(patterns[owner], ", ")
		id, err := resolver.Resolve(ctx, owner)
		switch {
		case errors.Is(err, auth.ErrOwnerNotFound):
			results = append(results, Result{Fail, label + ": GitHub has no user or organization " + owner, "fix allow in " + o.ConfigPath})
		case err != nil:
			results = append(results, Result{Warn, label + ": cannot resolve the owner: " + err.Error(), "if GitHub limits requests, set GITHUB_TOKEN"})
		case saved[owner] != "" && saved[owner] != id:
			results = append(results, Result{Fail,
				fmt.Sprintf("%s: owner id is now %s, pushbot trusts %s: the login belongs to another account", label, id, saved[owner]),
				"if that is expected: sudo systemctl restart pushbot, otherwise remove it from allow"})
		default:
			results = append(results, Result{OK, label + " → owner id " + id, ""})
		}
	}
	return results
}

func checkVersion(ctx context.Context, o Options) Result {
	tag, err := upgrade.LatestTag(ctx, o.Client, o.APIBase, o.Repo, o.Token)
	if err != nil {
		return Result{Warn, "cannot check for a newer pushbot: " + err.Error(), ""}
	}
	latest := strings.TrimPrefix(tag, "v")
	if latest == o.Version {
		return Result{OK, "pushbot " + o.Version + " is the latest release", ""}
	}
	return Result{Warn, "pushbot " + o.Version + ", latest is " + latest, "sudo pushbot upgrade"}
}

// get returns the body of a 200 response.
func get(ctx context.Context, client *http.Client, u string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", short(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", errors.New(resp.Status)
	}
	return string(body), nil
}

// short drops the method and URL that net/http puts in front of the cause.
func short(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
