// Command pushbot is a small deploy daemon for GitHub Actions.
package main

import (
	"context"
	"os"
	"strings"

	"github.com/charmbracelet/fang"
	"github.com/spf13/cobra"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

// repo hosts the releases used by `pushbot upgrade`.
const repo = "nikitakiselev/pushbot"

func main() {
	root := &cobra.Command{
		Use:   "pushbot",
		Short: "Deploy daemon for GitHub Actions",
		Long: `pushbot receives deploy requests from GitHub Actions, verifies their OIDC
tokens and runs the deploy steps from the project's pushbot.yml.

Docs: https://github.com/` + repo,
	}
	root.AddCommand(serveCmd(), versionCmd())
	root.SetArgs(compatArgs(os.Args[1:]))
	if err := fang.Execute(context.Background(), root, fang.WithVersion(version)); err != nil {
		os.Exit(1)
	}
}

// compatArgs accepts the Go-style "-config" flag used by the systemd units of
// v1.0 and v1.1, which pflag would otherwise read as "-c onfig".
func compatArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if a == "-config" || strings.HasPrefix(a, "-config=") {
			a = "-" + a
		}
		out[i] = a
	}
	return out
}
