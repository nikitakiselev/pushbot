# Install pushbot on a server

You have a shell on the server, directly or over SSH, and the user can use sudo.
Show every command that changes the system and run it only after the user agrees.
You are done when `sudo pushbot doctor --url https://<host>` reports no failures
(with one exception on home networks, see [Verify](#4-verify)).

## 1. Gather what you need

Ask the user:

- **Who may deploy.** A GitHub login or organization, optionally with a
  repository name pattern: `alice/*` (all of alice's repositories, including
  future ones) or `alice/homelab-*`. pushbot does not start without it: GitHub
  signs tokens for every repository on GitHub, and `allow` is what keeps other
  people's repositories out.
- **The public hostname** for pushbot, such as `pushbot.example.com`, with a DNS
  record pointing at this server. GitHub-hosted runners connect from the internet,
  so pushbot needs a public address with HTTPS.

Then look at the server. These commands change nothing:

```sh
uname -m                          # x86_64 or aarch64; nothing else is supported
systemctl --version | head -n1    # systemd is required
sudo docker version --format '{{.Server.Version}}'
sudo docker compose version       # the Compose plugin is required
sudo ss -ltnp | grep -E ':(80|443|9800) '
systemctl is-active caddy nginx
sudo docker ps --format '{{.Names}}\t{{.Image}}\t{{.Ports}}'
```

If Docker or the Compose plugin is missing, the user installs them first
(<https://docs.docker.com/engine/install/>); ask before doing it yourself.

The port check tells you which reverse proxy to use. Something already listening
on 80/443 (Caddy, nginx, Traefik, nginx-proxy-manager, as a service or in a
container) is the user's proxy: add pushbot to it. If nothing listens there,
suggest Caddy, which gets certificates by itself.

## 2. Install

```sh
curl -fsSL https://raw.githubusercontent.com/nikitakiselev/pushbot/main/install.sh | sudo sh
sudo pushbot init --allow 'alice/*'
```

The script installs the binary to `/usr/local/bin`, creates the `pushbot` user in
the `docker` group, `/var/lib/pushbot` and the systemd service. It is short; the
user may want to read it first. Running it again upgrades pushbot and keeps the
config.

`pushbot init` writes `/etc/pushbot/config.yml`, creates `/srv/apps`, starts the
service and waits until the daemon answers `/healthz`. Before writing anything it
looks up every `allow` owner on GitHub, so a misspelled owner is an error here,
not a service that fails to start.

- Always pass flags. Without them `init` runs an interactive wizard, which needs
  a terminal you do not have.
- Repeat `--allow` for more patterns: `--allow 'alice/*' --allow 'my-org/homelab-*'`.
- Other flags: `--listen` (default `127.0.0.1:9800`), `--projects-root` (default
  `/srv/apps`), `--metrics-listen` (Prometheus, off by default).
- If the config already exists, `init` with flags refuses without `--force`. See
  [Maintenance](#maintenance) before changing it.
- If `init` fails after the restart, read `sudo journalctl -u pushbot -n 50`.
- `init` exists since v1.5.0. If the server has an older pushbot, run
  `sudo pushbot upgrade` first.

## 3. Reverse proxy

pushbot listens on `127.0.0.1:9800` over plain HTTP. Whatever proxy you use must:

1. **Terminate TLS.** Never expose port 9800 to the internet: requests carry the
   application's secrets.
2. **Accept request bodies up to 64 MB** (the files archive is up to 50 MB).
3. **Not buffer the response,** or the deploy log reaches Actions only at the end.
4. **Keep the connection open longer than 15 minutes** (the deploy timeout).

Before changing an existing proxy, back up its config, and validate the new
config before reloading.

**Caddy** (`/etc/caddy/Caddyfile`; certificates, body size and timeouts need no
setup):

```
pushbot.example.com {
    reverse_proxy 127.0.0.1:9800 {
        flush_interval -1    # stream the log immediately, no buffering
    }
}
```

```sh
sudo caddy validate --config /etc/caddy/Caddyfile && sudo systemctl reload caddy
```

**nginx** (`/etc/nginx/sites-available/pushbot`, linked into `sites-enabled`). If
there is no certificate for the hostname yet, get one the way this server already
does, usually `sudo certbot certonly --nginx -d pushbot.example.com`.

```nginx
server {
    listen 443 ssl;
    server_name pushbot.example.com;

    ssl_certificate     /etc/letsencrypt/live/pushbot.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/pushbot.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:9800;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;

        client_max_body_size 64m;   # default is 1m
        proxy_buffering off;        # stream the log immediately
        proxy_read_timeout 20m;     # longer than deploy_timeout (15m)
    }
}

server {
    listen 80;
    server_name pushbot.example.com;
    return 301 https://$host$request_uri;
}
```

```sh
sudo nginx -t && sudo systemctl reload nginx
```

**Other proxies** (Traefik, nginx-proxy-manager, HAProxy): route the hostname to
pushbot and check the proxy's own body size limit, buffering and timeouts against
the four requirements above.

**A proxy running in a Docker container** cannot reach `127.0.0.1:9800`: inside the
container that address is the container itself. Either run the proxy with
`network_mode: host`, or make pushbot listen on the host's `docker0` address and
proxy to that:

```sh
ip -4 addr show docker0           # usually 172.17.0.1
sudo pushbot init --force --listen 172.17.0.1:9800
```

If the container still cannot connect, a host firewall (ufw, firewalld) may be
blocking traffic from Docker networks to the host.

## 4. Verify

```sh
sudo pushbot doctor --url https://pushbot.example.com
```

`doctor` checks the config, the service, Docker access for the `pushbot` user,
the directories, the server clock, access to GitHub's OIDC keys, the `allow`
owners and the public URL through the proxy. Most failures and warnings end with
`→` and the fix. Apply the fixes (with the user's agreement) and run it again
until there are no failures. `doctor` exists since v1.3.0; on an older version,
upgrade first.

From outside the server, `curl -fsS https://pushbot.example.com/healthz` must print
`ok`.

The public URL check runs from the server itself. On a home network whose router
has no NAT loopback (hairpinning), it fails although GitHub reaches the server
fine. If the `curl` above works from another network (a phone hotspot), that one
failure is not a problem; do not change the proxy because of it.

## 5. Hand over

Tell the user the URL to put into workflows (`url: https://pushbot.example.com`)
and which repositories `allow` admits. To connect a repository, follow
[connect-project.md](connect-project.md) in that repository.

## Maintenance

- **Change settings** with `sudo pushbot init --force` and the flags to change.
  Settings you do not name are kept. `init` checks the owners on GitHub, keeps the
  previous file as `config.yml.bak` and restarts the service.
- **Allow more repositories:** `--allow` replaces the whole list, so read the
  current `allow` in `/etc/pushbot/config.yml` and pass every pattern, old and new:
  `sudo pushbot init --force --allow 'alice/*' --allow 'my-org/homelab-*'`.
  Patterns are `owner/repo-glob`; the owner is literal, the repository name is a
  glob.
- **Upgrade:** `sudo pushbot upgrade` (latest release) or
  `sudo pushbot upgrade --version v1.3.0` (a specific one, e.g. to roll back). It
  keeps the config, and the restart waits for running deploys. Versions before
  v1.2.0 have no `upgrade`: run the install command again.
- **Restarts wait for running deploys,** up to 16 minutes, both
  `sudo systemctl restart pushbot` and `pushbot init`. If your command times out
  meanwhile, check `systemctl status pushbot` instead of restarting again.
- **Editing the file by hand** works too. Run `sudo pushbot doctor` before
  restarting: it reads the edited file and catches a misspelled owner, which would
  stop the service on restart (and fail every project's deploys). Then
  `sudo systemctl restart pushbot` and check `systemctl is-active pushbot`.
- **Other settings** (no flags for them, edit the file), with their defaults:
  `deploy_timeout: 15m`, `max_bundle_size: 50MB`, `max_unpacked_size: 200MB`,
  `log_retention: 20`, `audience: pushbot`, `metrics_listen` (Prometheus, off by
  default). Raising `deploy_timeout` means raising the proxy timeout too.
- **Release a project name** held by another repository: back up
  `/var/lib/pushbot/projects.json`, remove the name's entry and check that the file
  is still valid JSON (`sudo jq . /var/lib/pushbot/projects.json` if `jq` is
  installed): a broken file fails every deploy with 500. No restart needed.
- **Logs:** `sudo journalctl -u pushbot` (daemon, rejected requests, deploy
  summaries) and `/var/lib/pushbot/logs/<name>/` (one file per deploy, the last
  20 kept; `sudo ls -t` lists the newest first).
