package config

import (
	"reflect"
	"testing"
	"time"
)

func TestParseMinimalUsesDefaults(t *testing.T) {
	c, err := Parse([]byte(`
projects_root: /srv/apps
allow:
  - nikitakiselev/*
`))
	if err != nil {
		t.Fatal(err)
	}
	want := &Config{
		Listen:          "127.0.0.1:9800",
		ProjectsRoot:    "/srv/apps",
		StateDir:        "/var/lib/pushbot",
		Audience:        "pushbot",
		DeployTimeout:   15 * time.Minute,
		MaxBundleSize:   50 << 20,
		MaxUnpackedSize: 200 << 20,
		LogRetention:    20,
		OIDCIssuer:      "https://token.actions.githubusercontent.com",
		Allow:           []AllowRule{{Owner: "nikitakiselev", RepoGlob: "*"}},
	}
	if !reflect.DeepEqual(c, want) {
		t.Errorf("got  %+v\nwant %+v", c, want)
	}
}

func TestParseOverrides(t *testing.T) {
	c, err := Parse([]byte(`
listen: 0.0.0.0:7000
metrics_listen: 192.168.1.10:9801
projects_root: /opt
state_dir: /tmp/state
audience: deploy
deploy_timeout: 90s
max_bundle_size: 1KB
max_unpacked_size: 2GB
log_retention: 3
oidc_issuer: http://127.0.0.1:1234
allow:
  - Org/homelab-*
  - me/site
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.MetricsListen != "192.168.1.10:9801" {
		t.Errorf("metrics_listen = %q", c.MetricsListen)
	}
	if c.Listen != "0.0.0.0:7000" || c.ProjectsRoot != "/opt" || c.StateDir != "/tmp/state" ||
		c.Audience != "deploy" || c.DeployTimeout != 90*time.Second || c.MaxBundleSize != 1024 ||
		c.MaxUnpackedSize != 2<<30 || c.LogRetention != 3 || c.OIDCIssuer != "http://127.0.0.1:1234" {
		t.Errorf("unexpected config %+v", c)
	}
	wantAllow := []AllowRule{{Owner: "org", RepoGlob: "homelab-*"}, {Owner: "me", RepoGlob: "site"}}
	if !reflect.DeepEqual(c.Allow, wantAllow) {
		t.Errorf("allow = %+v", c.Allow)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"no projects_root":   "allow: [a/*]",
		"relative root":      "projects_root: srv\nallow: [a/*]",
		"no allow":           "projects_root: /srv",
		"owner wildcard":     "projects_root: /srv\nallow: ['*/x']",
		"no slash":           "projects_root: /srv\nallow: [nikita]",
		"bad glob":           "projects_root: /srv\nallow: ['a/[']",
		"unknown key":        "projects_root: /srv\nallow: [a/*]\nlisten_addr: x",
		"bad duration":       "projects_root: /srv\nallow: [a/*]\ndeploy_timeout: soon",
		"bad size":           "projects_root: /srv\nallow: [a/*]\nmax_bundle_size: 10XB",
		"zero retention":     "projects_root: /srv\nallow: [a/*]\nlog_retention: 0",
		"relative state dir": "projects_root: /srv\nallow: [a/*]\nstate_dir: state",
		"bad metrics_listen": "projects_root: /srv\nallow: [a/*]\nmetrics_listen: 9801",
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(doc)); err == nil {
				t.Errorf("Parse succeeded, want error")
			}
		})
	}
}

func TestParseSize(t *testing.T) {
	cases := map[string]int64{"10": 10, "10B": 10, "2KB": 2048, "3MB": 3 << 20, "1GB": 1 << 30, "5mb": 5 << 20}
	for in, want := range cases {
		got, err := parseSize(in)
		if err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
}
