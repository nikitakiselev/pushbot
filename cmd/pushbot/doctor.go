package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/charmbracelet/colorprofile"
	"github.com/spf13/cobra"

	"github.com/nikitakiselev/pushbot/internal/deploy"
	"github.com/nikitakiselev/pushbot/internal/doctor"
)

func doctorCmd() *cobra.Command {
	var configPath, publicURL string
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check that the server is ready to deploy",
		Long: `Check the config, the service, docker for the pushbot user, directories,
access to GitHub, the clock and the pushbot version. Every problem comes with
a hint how to fix it. Exits with code 1 if something is broken.`,
		Example: "  sudo pushbot doctor\n  sudo pushbot doctor --url https://pushbot.example.com",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runtime.GOOS != "linux" {
				return errors.New("doctor works on Linux servers only")
			}
			if os.Geteuid() != 0 {
				return errors.New("root is required, run: sudo pushbot doctor")
			}
			w := colorprofile.NewWriter(cmd.OutOrStdout(), os.Environ())
			warnings := 0
			failures := doctor.Run(cmd.Context(), doctor.Options{
				ConfigPath:   configPath,
				URL:          publicURL,
				Version:      version,
				Repo:         repo,
				Token:        os.Getenv("GITHUB_TOKEN"),
				Systemctl:    systemctlOutput,
				AsDaemonUser: asDaemonUser,
			}, func(r doctor.Result) {
				if r.Status == doctor.Warn {
					warnings++
				}
				fmt.Fprintln(w, formatResult(r))
			})
			switch {
			case failures > 0:
				return fmt.Errorf("%d %s found", failures, plural(failures, "problem"))
			case warnings > 0:
				fmt.Fprintf(w, "\nNo problems, %d %s.\n", warnings, plural(warnings, "warning"))
			default:
				fmt.Fprintln(w, "\nNo problems found.")
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&configPath, "config", "c", defaultConfigPath, "path to the config file")
	cmd.Flags().StringVar(&publicURL, "url", "", "public pushbot address to check through the reverse proxy")
	return cmd
}

func formatResult(r doctor.Result) string {
	const (
		green, yellow, red, faint, reset = "\x1b[32m", "\x1b[33m", "\x1b[31m", "\x1b[2m", "\x1b[0m"
	)
	mark := green + "✓" + reset
	switch r.Status {
	case doctor.Warn:
		mark = yellow + "!" + reset
	case doctor.Fail:
		mark = red + "✗" + reset
	}
	line := mark + " " + r.Text
	if r.Hint != "" {
		line += faint + " → " + reset + r.Hint
	}
	return line
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

func systemctlOutput(args ...string) (string, error) {
	out, err := exec.Command("systemctl", args...).Output()
	return strings.TrimSpace(string(out)), err
}

// asDaemonUser runs a command like pushbot.service runs deploy steps: as user
// pushbot with its groups plus docker, PATH of the steps and HOME of the user.
func asDaemonUser(name string, args ...string) (string, error) {
	u, err := user.Lookup("pushbot")
	if err != nil {
		return "", doctor.ErrNoDaemonUser
	}
	uid, _ := strconv.ParseUint(u.Uid, 10, 32)
	gid, _ := strconv.ParseUint(u.Gid, 10, 32)
	groupIDs, _ := u.GroupIds()
	if g, err := user.LookupGroup("docker"); err == nil {
		groupIDs = append(groupIDs, g.Gid)
	}
	var groups []uint32
	for _, g := range groupIDs {
		if id, err := strconv.ParseUint(g, 10, 32); err == nil {
			groups = append(groups, uint32(id))
		}
	}
	c := exec.Command(name, args...)
	c.Dir = "/"
	c.Env = []string{"PATH=" + deploy.StepPath, "HOME=" + u.HomeDir}
	c.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: groups}}
	out, err := c.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}
