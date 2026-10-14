# Install pushbot on a server

You have a shell on the server, directly or over SSH, and the user can use sudo.
Show every command that changes the system and run it only after the user agrees.
You are done when `sudo pushbot doctor --url https://<host>` reports no failures.

## 1. Gather what you need

Ask the user:

- **Who may deploy.** A GitHub login or organization, optionally with a
  repository name pattern: `alice/*` (all of alice's repositories, including
  future ones) or `alice/homelab-*`. This is required: without it, a token from
  any repository on GitHub would pass, because GitHub signs tokens for everyone.
- **The public hostname** for pushbot, such as `pushbot.example.com`, with a DNS
  record pointing at this server. GitHub-hosted runners connect from the internet,
  so pushbot needs a public address with HTTPS.

Then look at the server. These commands change nothing:

```sh
uname -m                          # x86_64 or aarch64; nothing else is supported
systemctl --version | head -n1    # systemd is required
docker version --format '{{.Server.Version}}'
docker compose version            # the Compose plugin is required
sudo ss -ltnp | grep -E ':(80|443|9800) '
systemctl is-active caddy nginx
docker ps --format '{{.Names}}\t{{.Image}}\t{{.Ports}}'
```

If Docker or the Compose plugin is missing, the user installs them first
(<https://docs.docker.com/engine/install/>); ask before doing it yourself.

The port check tells you which reverse proxy to use. Something already listening
on 80/443 (Caddy, nginx, Traefik, nginx-proxy-manager, as a service or in a
container) is the user's proxy: add pushbot to it. If nothing listens there,
suggest Caddy, which gets certificates by itself.

## 2. Install

```sh
curl -fsSL https://raw.githubusercontent.com/nikitakiselev/pushbot/main/install.sh \
  | sudo PUSHBOT_ALLOW='alice/*' sh
```

The script installs the binary to `/usr/local/bin`, creates the `pushbot` user in
the `docker` group, `/srv/apps`, `/var/lib/pushbot`, the config
`/etc/pushbot/config.yml` and the systemd service, and starts it. It is short; the
user may want to read it first.

- `PUSHBOT_ALLOW` takes one pattern. For more, add them to `allow` in the config
  afterwards (see [Maintenance](#maintenance)).
- If `/etc/pushbot/config.yml` already exists, the script keeps it and ignores
  `PUSHBOT_ALLOW`. Running the script again upgrades pushbot.
- Without `PUSHBOT_ALLOW`, the script writes a placeholder and does not start the
  service.

Check that the daemon answers locally:

```sh
curl -fsS http://127.0.0.1:9800/healthz   # prints: ok
```

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
# in /etc/pushbot/config.yml:  listen: 172.17.0.1:9800
sudo systemctl restart pushbot
```

If the container still cannot connect, a host firewall (ufw, firewalld) may be
blocking traffic from Docker networks to the host.

## 4. Verify

```sh
sudo pushbot doctor --url https://pushbot.example.com
```

`doctor` checks the config, the service, Docker access for the `pushbot` user,
the directories, the server clock, access to GitHub's OIDC keys, the `allow`
owners and the public URL through the proxy. Every failure or warning comes with
a fix on the next line. Apply the fixes (with the user's agreement) and run it
again until there are no failures. `doctor` exists since v1.3.0; on an older
version, upgrade first.

From outside the server, `curl -fsS https://pushbot.example.com/healthz` must print
`ok`.

## 5. Hand over

Tell the user the URL to put into workflows (`url: https://pushbot.example.com`)
and which repositories `allow` admits. To connect a repository, follow
[connect-project.md](connect-project.md) in that repository.

## Maintenance

- **Allow more repositories:** add a pattern to `allow` in
  `/etc/pushbot/config.yml`, then `sudo systemctl restart pushbot`. Patterns are
  `owner/repo-glob`; the owner is literal, the repository name is a glob. On
  startup pushbot resolves each owner to its numeric GitHub ID, so run
  `sudo pushbot doctor` after editing to catch a misspelled owner.
- **Upgrade:** `sudo pushbot upgrade` (latest release) or
  `sudo pushbot upgrade --version v1.3.0` (a specific one, e.g. to roll back). It
  keeps the config, and the restart waits for running deploys. Versions before
  v1.2.0 have no `upgrade`: run the install command again.
- **Restart after any config change:** `sudo systemctl restart pushbot`. It waits
  for running deploys (up to 16 minutes).
- **Other settings** in `/etc/pushbot/config.yml`, with their defaults:
  `deploy_timeout: 15m`, `max_bundle_size: 50MB`, `max_unpacked_size: 200MB`,
  `log_retention: 20`, `audience: pushbot`, `metrics_listen` (Prometheus, off by
  default). Raising `deploy_timeout` means raising the proxy timeout too.
- **Release a project name** held by another repository: remove its entry from
  `/var/lib/pushbot/projects.json`. No restart needed.
- **Logs:** `sudo journalctl -u pushbot` (daemon, rejected requests, deploy
  summaries) and `/var/lib/pushbot/logs/<name>/` (one file per deploy, the last
  20 kept; `sudo ls -t` lists the newest first).
