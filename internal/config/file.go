package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ReadFile reads a config file as written, without defaults or validation.
// Unknown keys are an error: writing the file back would drop them.
func ReadFile(file string) (File, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return File{}, err
	}
	f, err := decode(data)
	if err != nil {
		return File{}, fmt.Errorf("%s: %w", file, err)
	}
	return f, nil
}

// Render formats f as a commented config file. Settings that are not set are
// shown commented out with their defaults.
func (f File) Render() []byte {
	var b strings.Builder
	setting := func(key, value, def string) {
		switch {
		case value != "":
			fmt.Fprintf(&b, "%s: %s\n", key, scalar(value))
		case def != "":
			fmt.Fprintf(&b, "# %s: %s\n", key, def)
		}
	}

	b.WriteString("# pushbot configuration, written by pushbot init.\n")
	b.WriteString("# After changes: sudo systemctl restart pushbot\n")
	b.WriteString("# Docs: https://github.com/nikitakiselev/pushbot#daemon-config\n")
	setting("listen", f.Listen, DefaultListen)
	setting("projects_root", f.ProjectsRoot, "")
	b.WriteString("# Repositories that may deploy, owner/repo-pattern: the owner is literal,\n")
	b.WriteString("# the repository name is a pattern.\n")
	b.WriteString("allow:\n")
	for _, pattern := range f.Allow {
		fmt.Fprintf(&b, "  - %s\n", scalar(pattern))
	}
	b.WriteString("# Prometheus metrics at /metrics on a separate address (disabled when empty):\n")
	setting("metrics_listen", f.MetricsListen, "127.0.0.1:9801")
	b.WriteString("\n# Optional settings; a commented-out line shows the default:\n")
	setting("state_dir", f.StateDir, defaultStateDir)
	setting("audience", f.Audience, defaultAudience)
	setting("deploy_timeout", f.DeployTimeout, defaultDeployTimeout)
	setting("max_bundle_size", f.MaxBundleSize, defaultMaxBundleSize)
	setting("max_unpacked_size", f.MaxUnpackedSize, defaultMaxUnpackedSize)
	if f.LogRetention != nil {
		fmt.Fprintf(&b, "log_retention: %d\n", *f.LogRetention)
	} else {
		fmt.Fprintf(&b, "# log_retention: %d\n", defaultLogRetention)
	}
	setting("oidc_issuer", f.OIDCIssuer, "")
	return []byte(b.String())
}

// scalar formats s as a YAML scalar, quoted only when it has to be.
func scalar(s string) string {
	out, err := yaml.Marshal(s)
	if err != nil {
		return strconv.Quote(s)
	}
	return strings.TrimSuffix(string(out), "\n")
}
