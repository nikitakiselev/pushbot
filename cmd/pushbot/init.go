package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"charm.land/huh/v2"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/nikitakiselev/pushbot/internal/auth"
	"github.com/nikitakiselev/pushbot/internal/config"
	"github.com/nikitakiselev/pushbot/internal/setup"
)

func initCmd() *cobra.Command {
	var (
		configPath, listen, projectsRoot, metricsListen string
		allow                                           []string
		force                                           bool
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create the config and start pushbot",
		Long: `Create the config and start the pushbot service.

Without flags, a wizard asks for the settings, shows the file and asks to
confirm it. With any setting given as a flag, the config is written right
away. The owners in allow are checked on GitHub. An existing config is the
starting point: the wizard edits it, and flags change the settings they name
(--allow replaces the whole list) only with --force. The previous file is kept
as config.yml.bak.`,
		Example: `  sudo pushbot init
  sudo pushbot init --allow 'alice/*'
  sudo pushbot init --allow 'alice/*' --allow 'my-org/homelab-*' --listen 172.17.0.1:9800`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runtime.GOOS != "linux" {
				return errors.New("init works on Linux servers only")
			}
			if os.Geteuid() != 0 {
				return errors.New("root is required, run: sudo pushbot init")
			}
			out := cmd.OutOrStdout()
			ctx := cmd.Context()

			var given settingFlags
			flags := cmd.Flags()
			if flags.Changed("allow") {
				given.allow = &allow
			}
			if flags.Changed("listen") {
				given.listen = &listen
			}
			if flags.Changed("projects-root") {
				given.projectsRoot = &projectsRoot
			}
			if flags.Changed("metrics-listen") {
				given.metricsListen = &metricsListen
			}
			terminal := term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stdout.Fd())
			f, wizard, err := prepare(configPath, given, force, terminal, out)
			if err != nil {
				return err
			}

			owners := &setup.Owners{Resolver: &auth.OwnerResolver{Token: os.Getenv("GITHUB_TOKEN")}}
			if wizard {
				if f, err = runWizard(ctx, f, owners, configPath); err != nil {
					if errors.Is(err, huh.ErrUserAborted) {
						return errNothingWritten
					}
					return err
				}
			}

			opts := setup.Options{Path: configPath, File: f, Owners: owners, Chown: chownToDaemonUser, Out: out}
			// The unit reads the default config, so a config elsewhere is not its business.
			if _, err := os.Stat(unitPath); err == nil && filepath.Clean(configPath) == defaultConfigPath {
				opts.Systemctl = systemctl
			}
			return setup.Run(ctx, opts)
		},
	}
	fl := cmd.Flags()
	fl.StringVarP(&configPath, "config", "c", defaultConfigPath, "path to the config file")
	fl.StringSliceVar(&allow, "allow", nil, "who may deploy: owner/repo-pattern, e.g. 'alice/*'; repeat for more")
	fl.StringVar(&listen, "listen", config.DefaultListen, "address the daemon listens on, host:port")
	fl.StringVar(&projectsRoot, "projects-root", config.DefaultProjectsRoot, "directory with the deployed projects")
	fl.StringVar(&metricsListen, "metrics-listen", "", "address for Prometheus /metrics, empty disables metrics")
	fl.BoolVar(&force, "force", false, "change an existing config with the flags")
	return cmd
}

// useWizard decides where the settings come from: the wizard, or the flags
// when any setting is given.
func useWizard(path string, settings, force, exists, terminal bool) (bool, error) {
	switch {
	case settings && exists && !force:
		return false, fmt.Errorf("config %s exists: run pushbot init without flags to edit it, or pass --force to change it with the flags", path)
	case settings:
		return false, nil
	case !terminal:
		return false, errors.New("no terminal for the wizard: pass the settings as flags, e.g. --allow 'your-github-login/*'")
	default:
		return true, nil
	}
}

// settingFlags are the setting flags given on the command line; nil if not given.
type settingFlags struct {
	allow                               *[]string
	listen, projectsRoot, metricsListen *string
}

func (s settingFlags) given() bool {
	return s.allow != nil || s.listen != nil || s.projectsRoot != nil || s.metricsListen != nil
}

// prepare reads the existing config and decides where the settings come from.
// It returns the config for the wizard to start from, or the config to write,
// with the flags applied on top of the existing one. Without the wizard, a
// config that cannot be read is an error: starting from the defaults would
// quietly reset every setting the flags do not name.
func prepare(path string, s settingFlags, force, terminal bool, out io.Writer) (config.File, bool, error) {
	f, err := config.ReadFile(path)
	exists := !errors.Is(err, fs.ErrNotExist)
	wizard, werr := useWizard(path, s.given(), force, exists, terminal)
	if werr != nil {
		return config.File{}, false, werr
	}
	defaults := config.File{Listen: config.DefaultListen, ProjectsRoot: config.DefaultProjectsRoot}
	switch {
	case err == nil:
	case !exists:
		f = defaults
	case wizard:
		fmt.Fprintf(out, "! cannot read %s, starting from the defaults: %v\n", path, err)
		f = defaults
	default:
		return config.File{}, false, fmt.Errorf("cannot read the config, fix it or run pushbot init without flags to start from the defaults: %w", err)
	}
	if wizard {
		return f, true, nil
	}
	if s.allow != nil {
		f.Allow = *s.allow
	}
	if s.listen != nil {
		f.Listen = *s.listen
	}
	if s.projectsRoot != nil {
		f.ProjectsRoot = *s.projectsRoot
	}
	if s.metricsListen != nil {
		f.MetricsListen = *s.metricsListen
	}
	if len(f.Allow) == 0 {
		return config.File{}, false, errors.New("pass --allow: who may deploy, e.g. --allow 'your-github-login/*'")
	}
	return f, false, nil
}

// answers holds the wizard's fields. The fields are exported so that the file
// preview notices when they change.
type answers struct{ Allow, Listen, ProjectsRoot, MetricsListen string }

func (a answers) apply(f config.File) config.File {
	f.Allow = splitPatterns(a.Allow)
	f.Listen = strings.TrimSpace(a.Listen)
	f.ProjectsRoot = strings.TrimSpace(a.ProjectsRoot)
	f.MetricsListen = strings.TrimSpace(a.MetricsListen)
	return f
}

// runWizard asks for the settings, shows the resulting file and asks to
// confirm it. It is a single form: running several in a row loses keys.
func runWizard(ctx context.Context, f config.File, owners *setup.Owners, path string) (config.File, error) {
	a := answers{Allow: strings.Join(f.Allow, ", "), Listen: f.Listen, ProjectsRoot: f.ProjectsRoot, MetricsListen: f.MetricsListen}
	if a.Listen == "" {
		a.Listen = config.DefaultListen
	}
	ok := true
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().Title("Who may deploy?").
				Description("GitHub owner/repo-pattern, comma-separated: alice/* or my-org/homelab-*").
				Placeholder("your-github-login/*").
				Value(&a.Allow).
				Validate(func(s string) error {
					_, err := owners.Check(ctx, splitPatterns(s))
					return err
				}),
			huh.NewInput().Title("Listen address").
				Description("host:port for the daemon; the reverse proxy connects here").
				Value(&a.Listen).
				Validate(func(s string) error { return config.ValidateAddress(strings.TrimSpace(s)) }),
			huh.NewInput().Title("Projects root").
				Description("Directory for the deployed projects").
				Value(&a.ProjectsRoot).
				Validate(func(s string) error {
					if !filepath.IsAbs(strings.TrimSpace(s)) {
						return errors.New("must be an absolute path")
					}
					return nil
				}),
			huh.NewInput().Title("Prometheus metrics address").
				Description("host:port for /metrics; leave empty to disable metrics").
				Placeholder("127.0.0.1:9801").
				Value(&a.MetricsListen).
				Validate(func(s string) error {
					if strings.TrimSpace(s) == "" {
						return nil
					}
					return config.ValidateAddress(strings.TrimSpace(s))
				}),
		),
		huh.NewGroup(
			huh.NewNote().Title(path+" (comments left out)").
				DescriptionFunc(func() string { return preview(a.apply(f).Render()) }, &a),
			huh.NewConfirm().Title("Correct?").
				Description("Yes writes the file and starts pushbot. To change a setting, go back with shift+tab.").
				Affirmative("Yes").Negative("No").
				Value(&ok),
		),
	)
	if err := form.RunWithContext(ctx); err != nil {
		return f, err
	}
	if !ok {
		return f, errNothingWritten
	}
	return a.apply(f), nil
}

var errNothingWritten = errors.New("cancelled, nothing was written")

// preview is the config without its comments, escaped for a huh note, which
// renders * _ and ` as markup.
func preview(file []byte) string {
	var lines []string
	for _, line := range strings.Split(string(file), "\n") {
		if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "#") {
			lines = append(lines, line)
		}
	}
	return strings.NewReplacer(`\`, `\\`, "*", `\*`, "_", `\_`, "`", "\\`").Replace(strings.Join(lines, "\n"))
}

// splitPatterns splits the wizard's allow answer on commas and spaces.
func splitPatterns(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
}

func chownToDaemonUser(path string) error {
	u, err := user.Lookup("pushbot")
	if err != nil {
		return errors.New("user pushbot does not exist, run the install command from the README")
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return os.Chown(path, uid, gid)
}
