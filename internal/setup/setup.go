// Package setup writes the daemon config and starts the service: the work of
// pushbot init.
package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nikitakiselev/pushbot/internal/auth"
	"github.com/nikitakiselev/pushbot/internal/config"
)

// logHint ends errors about a daemon that did not start.
const logHint = "see the log: sudo journalctl -u pushbot -n 50"

// Lookup is the result of looking up an allow-list owner on GitHub.
type Lookup struct {
	Owner string
	ID    string
	Err   error // GitHub could not be asked; never auth.ErrOwnerNotFound
}

// Owners looks up allow-list owners on GitHub and remembers the answers, so
// the wizard and Run ask about each owner once per pushbot init.
type Owners struct {
	Resolver *auth.OwnerResolver

	mu    sync.Mutex
	known map[string]Lookup
}

// Check validates allow patterns and looks up their owners. It fails on a bad
// pattern or an owner GitHub does not know. Owners that could not be looked up
// are returned with Err set.
func (o *Owners) Check(ctx context.Context, patterns []string) ([]Lookup, error) {
	if len(patterns) == 0 {
		return nil, errors.New("allow must list at least one owner/repo pattern")
	}
	var owners []string
	seen := map[string]bool{}
	for _, p := range patterns {
		if err := config.ValidateAllow(p); err != nil {
			return nil, err
		}
		owner, _, _ := strings.Cut(strings.ToLower(p), "/")
		if !seen[owner] {
			seen[owner] = true
			owners = append(owners, owner)
		}
	}
	var lookups []Lookup
	for _, owner := range owners {
		l := o.lookup(ctx, owner)
		if errors.Is(l.Err, auth.ErrOwnerNotFound) {
			return nil, fmt.Errorf("GitHub has no user or organization %s", owner)
		}
		lookups = append(lookups, l)
	}
	return lookups, nil
}

func (o *Owners) lookup(ctx context.Context, owner string) Lookup {
	o.mu.Lock()
	defer o.mu.Unlock()
	if l, ok := o.known[owner]; ok {
		return l
	}
	// Failures are remembered too: a GitHub that does not answer would freeze
	// the wizard on every Enter. pushbot retries the lookup when it starts.
	id, err := o.Resolver.Resolve(ctx, owner)
	l := Lookup{Owner: owner, ID: id, Err: err}
	if o.known == nil {
		o.known = map[string]Lookup{}
	}
	o.known[owner] = l
	return l
}

// Options for Run.
type Options struct {
	Path   string      // config file to write
	File   config.File // the config to write
	Owners *Owners
	// Systemctl runs systemctl. Nil means the service is not managed: no unit
	// is installed, or it reads another config file.
	Systemctl func(args ...string) error
	// Chown hands a created directory to the pushbot user. Nil skips it.
	Chown         func(path string) error
	Client        *http.Client  // for /healthz, default 2s timeout
	HealthTimeout time.Duration // how long to wait for /healthz, default 75s
	Out           io.Writer
}

// Run validates the config and looks up its owners, writes it (the previous
// file is kept as .bak), creates projects_root, restarts the service and waits
// until the daemon answers.
func Run(ctx context.Context, o Options) error {
	data := o.File.Render()
	cfg, err := config.Parse(data)
	if err != nil {
		return err
	}
	lookups, err := o.Owners.Check(ctx, o.File.Allow)
	if err != nil {
		return fmt.Errorf("allow: %w", err)
	}
	for _, l := range lookups {
		if l.Err != nil {
			fmt.Fprintf(o.Out, "! cannot look up %s on GitHub, pushbot retries when it starts: %v\n", l.Owner, l.Err)
		} else {
			fmt.Fprintf(o.Out, "✓ %s → GitHub id %s\n", l.Owner, l.ID)
		}
	}

	backup, err := writeConfig(o.Path, data)
	if err != nil {
		return fmt.Errorf("write %s: %w", o.Path, err)
	}
	if backup != "" {
		fmt.Fprintf(o.Out, "✓ wrote %s, the previous one is %s\n", o.Path, backup)
	} else {
		fmt.Fprintf(o.Out, "✓ wrote %s\n", o.Path)
	}
	if err := makeProjectsRoot(cfg.ProjectsRoot, o); err != nil {
		return err
	}

	if o.Systemctl == nil {
		fmt.Fprintf(o.Out, "\nStart the daemon: pushbot serve --config %s\n", o.Path)
	} else {
		if err := o.Systemctl("enable", "--quiet", "pushbot"); err != nil {
			return err
		}
		fmt.Fprintln(o.Out, "Restarting pushbot (waits for running deploys to finish)...")
		if err := o.Systemctl("restart", "pushbot"); err != nil {
			return err
		}
		if err := waitHealthy(ctx, o, cfg.Listen); err != nil {
			return err
		}
		fmt.Fprintf(o.Out, "✓ pushbot is running on %s\n", cfg.Listen)
	}
	printNextSteps(o.Out, cfg.Listen)
	return nil
}

// writeConfig atomically replaces path with data. A previous file is copied to
// path.bak first; its name is returned.
func writeConfig(path string, data []byte) (backup string, err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if old, err := os.ReadFile(path); err == nil {
		backup = path + ".bak"
		if err := os.WriteFile(backup, old, 0o644); err != nil {
			return "", err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".new-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return "", err
	}
	// Without it a crash right after the rename can leave an empty config.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	return backup, os.Rename(tmp.Name(), path)
}

// makeProjectsRoot creates a missing projects_root for the pushbot user. An
// existing directory is left as it is.
func makeProjectsRoot(root string, o Options) error {
	if _, err := os.Stat(root); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	// Parents must stay traversable for the pushbot user, as install -d made them.
	if err := os.MkdirAll(filepath.Dir(root), 0o755); err != nil {
		return err
	}
	if err := os.Mkdir(root, 0o750); err != nil {
		return err
	}
	if err := os.Chmod(root, 0o750); err != nil {
		return err
	}
	fmt.Fprintf(o.Out, "✓ created %s\n", root)
	if o.Chown != nil {
		if err := o.Chown(root); err != nil {
			fmt.Fprintf(o.Out, "! cannot hand %s to user pushbot: %v\n", root, err)
		}
	}
	return nil
}

// waitHealthy polls /healthz until the daemon answers, the service stops or
// HealthTimeout passes.
func waitHealthy(ctx context.Context, o Options, listen string) error {
	client := o.Client
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second}
	}
	timeout := o.HealthTimeout
	if timeout == 0 {
		timeout = 75 * time.Second
	}
	u := "http://" + config.DialAddr(listen) + "/healthz"
	deadline := time.Now().Add(timeout)
	for {
		if healthy(ctx, client, u) {
			return nil
		}
		if err := o.Systemctl("is-active", "--quiet", "pushbot"); err != nil {
			return errors.New("pushbot stopped after the restart, " + logHint)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("pushbot does not answer %s after %s, %s", u, timeout, logHint)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func healthy(ctx context.Context, client *http.Client, u string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	return resp.StatusCode == http.StatusOK && strings.TrimSpace(string(body)) == "ok"
}

func printNextSteps(w io.Writer, listen string) {
	fmt.Fprintf(w, `
Next, expose pushbot over HTTPS with a reverse proxy. Caddy (Caddyfile):

  pushbot.example.com {
      reverse_proxy %s {
          flush_interval -1
      }
  }

nginx and proxies in Docker: https://github.com/nikitakiselev/pushbot#reverse-proxy
Then check everything: sudo pushbot doctor --url https://pushbot.example.com
`, config.DialAddr(listen))
}
