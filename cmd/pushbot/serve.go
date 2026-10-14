package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/nikitakiselev/pushbot/internal/auth"
	"github.com/nikitakiselev/pushbot/internal/config"
	"github.com/nikitakiselev/pushbot/internal/deploy"
	"github.com/nikitakiselev/pushbot/internal/metrics"
	"github.com/nikitakiselev/pushbot/internal/server"
)

const defaultConfigPath = "/etc/pushbot/config.yml"

func serveCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the deploy daemon",
		Long: `Run the deploy daemon. It listens for deploy requests from GitHub Actions,
checks their OIDC tokens against the allow list and runs the steps from the
project's pushbot.yml. Usually started by systemd (pushbot.service).`,
		Example: "  pushbot serve --config /etc/pushbot/config.yml",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return serve(configPath)
		},
	}
	cmd.Flags().StringVarP(&configPath, "config", "c", defaultConfigPath, "path to the config file")
	return cmd
}

func serve(configPath string) error {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	cfg, err := config.Load(configPath)
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
