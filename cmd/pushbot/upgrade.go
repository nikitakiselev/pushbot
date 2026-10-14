package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/nikitakiselev/pushbot/internal/upgrade"
)

const unitPath = "/etc/systemd/system/pushbot.service"

func upgradeCmd() *cobra.Command {
	var target string
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade pushbot to the latest release",
		Long: `Download a pushbot release from GitHub, verify its checksum, replace this
binary and the systemd unit, and restart the service if it is running. The
restart waits for running deploys to finish. The config is not touched.`,
		Example: "  sudo pushbot upgrade\n  sudo pushbot upgrade --version v1.0.0",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runtime.GOOS != "linux" {
				return errors.New("upgrade works on Linux servers only")
			}
			if os.Geteuid() != 0 {
				return errors.New("root is required, run: sudo pushbot upgrade")
			}
			bin, err := os.Executable()
			if err != nil {
				return err
			}
			if bin, err = filepath.EvalSymlinks(bin); err != nil {
				return err
			}
			return upgrade.Run(cmd.Context(), upgrade.Options{
				Current:    version,
				Version:    target,
				Repo:       repo,
				Token:      os.Getenv("GITHUB_TOKEN"),
				Arch:       runtime.GOARCH,
				BinaryPath: bin,
				UnitPath:   unitPath,
				Systemctl:  systemctl,
				Out:        cmd.OutOrStdout(),
			})
		},
	}
	cmd.Flags().StringVar(&target, "version", "", "install this release instead of the latest, e.g. v1.0.0")
	return cmd
}

func systemctl(args ...string) error {
	c := exec.Command("systemctl", args...)
	c.Stdout, c.Stderr = os.Stderr, os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("systemctl %v: %w", args, err)
	}
	return nil
}
