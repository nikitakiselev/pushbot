// Command pushbot is a small deploy daemon for GitHub Actions.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/nikitakiselev/pushbot/internal/auth"
	"github.com/nikitakiselev/pushbot/internal/config"
	"github.com/nikitakiselev/pushbot/internal/deploy"
	"github.com/nikitakiselev/pushbot/internal/metrics"
	"github.com/nikitakiselev/pushbot/internal/server"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage:
  pushbot serve [-config /etc/pushbot/config.yml]   run the deploy daemon
  pushbot version                                   print the version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		if err := serve(os.Args[2:]); err != nil {
			slog.Error("pushbot stopped", "err", err)
			os.Exit(1)
		}
	case "version", "--version":
		fmt.Println(version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}

func serve(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := flags.String("config", "/etc/pushbot/config.yml", "path to the config file")
	flags.Parse(args)

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.StateDir, 0o750); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	rules, err := resolveRules(ctx, cfg, logger)
	if err != nil {
		return err
	}
	m := metrics.New(version)
	d, err := deploy.New(deploy.Config{
		ProjectsRoot: cfg.ProjectsRoot,
		StateDir:     cfg.StateDir,
		Timeout:      cfg.DeployTimeout,
		MaxUnpacked:  cfg.MaxUnpackedSize,
		LogRetention: cfg.LogRetention,
		Logger:       logger,
		Observer:     m,
	})
	if err != nil {
		return err
	}
	verifier := auth.NewVerifier(cfg.OIDCIssuer, cfg.Audience, rules)
	s := server.New(verifier, d, cfg.MaxBundleSize, filepath.Join(cfg.StateDir, "tmp"), logger).WithObserver(m)

	// No read/write timeouts: deploys stream for up to deploy_timeout.
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	errCh := make(chan error, 2)
	go func() { errCh <- httpSrv.ListenAndServe() }()

	// Metrics get their own listener so they are not exposed through the deploy proxy.
	var metricsSrv *http.Server
	if cfg.MetricsListen != "" {
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", m.Handler())
		metricsSrv = &http.Server{Addr: cfg.MetricsListen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() { errCh <- metricsSrv.ListenAndServe() }()
	}
	logger.Info("pushbot started", "version", version, "listen", cfg.Listen,
		"metrics_listen", cfg.MetricsListen, "projects_root", cfg.ProjectsRoot)

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	logger.Info("shutting down, waiting for running deploys")
	s.Drain()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.DeployTimeout+time.Minute)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	running := 1
	if metricsSrv != nil {
		_ = metricsSrv.Close()
		running++
	}
	for range running {
		if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	logger.Info("pushbot stopped")
	return nil
}

// resolveRules turns allow patterns into rules bound to numeric owner IDs.
func resolveRules(ctx context.Context, cfg *config.Config, logger *slog.Logger) ([]auth.Rule, error) {
	var owners []string
	seen := map[string]bool{}
	for _, r := range cfg.Allow {
		if !seen[r.Owner] {
			seen[r.Owner] = true
			owners = append(owners, r.Owner)
		}
	}
	resolver := &auth.OwnerResolver{Token: os.Getenv("GITHUB_TOKEN")}
	logf := func(format string, args ...any) { logger.Warn(fmt.Sprintf(format, args...)) }
	ids, err := auth.OwnerIDs(ctx, resolver, owners, filepath.Join(cfg.StateDir, "owners.json"), time.Minute, logf)
	if err != nil {
		return nil, err
	}
	var rules []auth.Rule
	for _, r := range cfg.Allow {
		rules = append(rules, auth.Rule{Owner: r.Owner, OwnerID: ids[r.Owner], RepoGlob: r.RepoGlob})
		logger.Info("allow", "pattern", r.Owner+"/"+r.RepoGlob, "owner_id", ids[r.Owner])
	}
	return rules, nil
}
