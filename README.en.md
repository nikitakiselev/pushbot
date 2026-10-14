# pushbot

[Русский](README.md) | **English**

A tiny CI/CD daemon for homelabs and personal servers. GitHub Actions builds an
image, pushes it to a registry and, as its last step, calls pushbot on the server.
pushbot puts the project files in place, writes `.env` from GitHub Secrets and runs
the steps from `pushbot.yml`. CI needs no SSH access and no auth secrets: requests
are verified with GitHub Actions OIDC tokens.

```mermaid
flowchart LR
    A[git push] --> B[GitHub Actions]
    B -->|HTTP| C[pushbot on the server]
    C --> D["runs the deploy commands<br/>from pushbot.yml"]
```

## Server installation

Requires Linux with systemd and Docker.

```sh
curl -fsSL https://raw.githubusercontent.com/nikitakiselev/pushbot/main/install.sh \
  | sudo PUSHBOT_ALLOW='your-github-login/*' sh
```

**`PUSHBOT_ALLOW`** — which GitHub repositories may deploy to this server. The
format is `owner/repo-pattern`: `your-github-login/*` allows any of your
repositories (including future ones), `your-github-login/homelab-*` only those with
that prefix. Without this restriction a token from any repository on GitHub would
pass: GitHub signs tokens for everyone.

The value goes into `allow` of a new `/etc/pushbot/config.yml`. If the config
already exists, the variable is ignored. If you omit it, the script creates a config
with a placeholder and does not start the service until you fill in `allow`
yourself. The variable sets a single pattern; for more, add them to `allow` in the
config (see [Daemon config](#daemon-config)).

The script (worth reading, it is short) installs the binary to `/usr/local/bin`,
creates the `pushbot` user in the `docker` group, the `/srv/apps` and
`/var/lib/pushbot` directories, the `/etc/pushbot/config.yml` config and a systemd
service. Running it again upgrades pushbot and leaves the config alone.

### Reverse proxy

The daemon listens on `127.0.0.1:9800` over plain HTTP. A reverse proxy with TLS
exposes it: requests carry secrets from `env`, so plain HTTP must not face the
internet. The proxy has to do three things: accept request bodies up to ~50 MB (the
files archive), not buffer the response (otherwise the deploy log reaches Actions
only at the end), and keep the connection open while the deploy runs (up to 15
minutes).

**Caddy** (`Caddyfile`). Caddy gets the certificate itself; body size and response
time are unlimited by default:

```
deploy.example.com {
    reverse_proxy 127.0.0.1:9800 {
        flush_interval -1    # stream the log immediately, no buffering
    }
}
```

**nginx** (`/etc/nginx/sites-available/pushbot`, certificate e.g. from certbot):

```nginx
server {
    listen 443 ssl;
    server_name deploy.example.com;

    ssl_certificate     /etc/letsencrypt/live/deploy.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/deploy.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:9800;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;

        client_max_body_size 64m;   # default is 1m; the archive is up to 50 MB (max_bundle_size)
        proxy_buffering off;        # stream the log immediately, no buffering
        proxy_read_timeout 20m;     # longer than deploy_timeout (15m)
    }
}

server {
    listen 80;
    server_name deploy.example.com;
    return 301 https://$host$request_uri;
}
```

If the proxy itself runs in a Docker container, `127.0.0.1` inside the container is
the container, not the server. Either run the proxy with `network_mode: host`, or set
`listen: 172.17.0.1:9800` in `/etc/pushbot/config.yml` (the host address on the
`docker0` network, check with `ip addr show docker0`) and proxy to that.

Check: `curl https://deploy.example.com/healthz` should return `ok`.

## Adding a project

Nothing to do on the server. In the project repository:

**1. `pushbot.yml` in the root:**

```yaml
files:
  - docker-compose.yml
```

**2. A step in the workflow:**

```yaml
on:
  push:
    branches: [main]

permissions:
  contents: read
  packages: write     # push the image to GHCR
  id-token: write     # OIDC token for pushbot

jobs:
  deploy:
    runs-on: ubuntu-latest
    environment: production
    steps:
      - uses: actions/checkout@v4
      - uses: docker/login-action@v3
        with: { registry: ghcr.io, username: "${{ github.actor }}", password: "${{ github.token }}" }
      - uses: docker/build-push-action@v6
        with: { push: true, tags: "ghcr.io/${{ github.repository }}:${{ github.sha }}" }

      - uses: nikitakiselev/pushbot@v1
        with:
          url: https://deploy.example.com
          env: |
            IMAGE_TAG=${{ github.sha }}
            DB_PASSWORD=${{ secrets.DB_PASSWORD }}
```

In `docker-compose.yml` use `image: ghcr.io/<owner>/<repo>:${IMAGE_TAG}`.

**3. Push.** The project appears in `/srv/apps/<repository name>`.

## `pushbot.yml`

```yaml
name: my-service          # optional; defaults to the lower-cased repository name

files:                    # what to put into /srv/apps/<name>
  - docker-compose.yml
  - config/                                  # a whole directory
  - deploy/compose.prod.yml: compose.yml     # renamed
  - deploy/: .                               # directory contents into the project root

steps:                    # optional; defaults to:
  - run: docker compose pull
  - run: docker compose up -d --remove-orphans
```

Deploy order: files from `files` → `.env` → steps, one by one.

- **Files** are overwritten, but **nothing is deleted**: container data in the
  project directory (`./data` etc.) is safe. Paths are relative, without `..`;
  symlinks are not supported.
- **`.env`** is fully rewritten from the `env` input (mode `0600`, values in single
  quotes so compose does not expand `$`). `.env` cannot be listed in `files`.
- **Steps** run as `sh -e -c '<run>'` in the project directory; the first error stops
  the deploy. The environment contains the `env` variables plus `PUSHBOT_PROJECT`,
  `PUSHBOT_REPOSITORY`, `PUSHBOT_SHA`, `PUSHBOT_REF`, `PUSHBOT_ACTOR`,
  `PUSHBOT_RUN_ID`, `PUSHBOT_RUN_URL`. The daemon's environment is not inherited.

There is no automatic rollback: to roll back, re-run the workflow for the previous
commit.

## Action inputs

| Input | Default | |
|---|---|---|
| `url` | — | pushbot address, required |
| `env` | empty | `KEY=VALUE` lines for `.env` |
| `config` | `pushbot.yml` | manifest path relative to the repository root |
| `registry-auth` | `true` | send the temporary `github.token` so the server can pull a private image from ghcr.io |
| `audience` | `pushbot` | OIDC token audience |

## Daemon config

`/etc/pushbot/config.yml`; after changes run `systemctl restart pushbot`.

```yaml
listen: 127.0.0.1:9800
projects_root: /srv/apps
allow:
  - your-github-login/*       # owner is literal, the repository name is a pattern
  - my-org/homelab-*
# optional (defaults):
# state_dir: /var/lib/pushbot
# audience: pushbot
# deploy_timeout: 15m
# max_bundle_size: 50MB
# max_unpacked_size: 200MB
# log_retention: 20
```

On startup pushbot looks up the numeric GitHub IDs of the owners in `allow` and
matches tokens by ID rather than by name (an account name can be released and taken
by someone else). The IDs are cached in `owners.json` in case GitHub is unreachable.
If the `GITHUB_TOKEN` environment variable is set, API requests use it (higher rate
limit).

## Security

- Push access to an allowed repository = the right to run commands on the server as
  the `pushbot` user, and the `docker` group is effectively root. This is a
  deliberate trade-off for personal use; the protection is OIDC and `allow`.
- A token is checked for GitHub's signature, `iss`, `aud`, lifetime and single use
  (`jti`). Pull requests from forks do not get OIDC tokens.
- The first deploy binds the project name to the repository (by `repository_id`);
  another repository with the same `name` gets 403. To release a name, remove its
  entry from `/var/lib/pushbot/projects.json`.
- `env` values are masked as `***` in the server logs; GitHub masks secrets in the
  Actions log.

## Logs

- Each deploy: `/var/lib/pushbot/logs/<name>/<time>-<sha>.log` (the last 20 are kept).
- Summary and rejected requests: `journalctl -u pushbot`.

If the Actions job is cancelled, the deploy still runs to completion — check the log
on the server.

## Development

```sh
go test -race ./...
```

The tests in `internal/e2e` run the real `action.sh` (requires `bash`, `yq`, `jq`,
`curl`, `tar`). CI also deploys through the action with real GitHub OIDC tokens. To
release, push a `vX.Y.Z` tag: GoReleaser publishes the binaries and the `v1` tag is
moved to the new release.

Design (in Russian): [`docs/superpowers/specs/2026-10-14-pushbot-design.md`](docs/superpowers/specs/2026-10-14-pushbot-design.md).
