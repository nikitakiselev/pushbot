# Troubleshoot a pushbot deploy

Find the first error, match it below, fix the cause, re-run. Do not change
several things at once.

## Where to look

**The Actions log of the pushbot step** comes first:

```sh
gh run view <run-id> --log-failed
```

- Errors from the action itself start with `pushbot:` (shown as an error
  annotation).
- A request rejected by the server prints the server's reason first (a line
  starting with `pushbot:`), then
  `the server rejected the deploy (HTTP <code>)`. If the line before is an HTML
  page instead, the reverse proxy rejected the request, not pushbot.
- A deploy that started prints lines starting with `==>` plus the steps' output,
  and ends with `PUSHBOT: success (…s)` or `PUSHBOT: failed: <reason>`.

**The server**, if the user can give you a shell there:

```sh
sudo journalctl -u pushbot --since "1 hour ago"   # rejected requests with the reason and repository
sudo ls -t /var/lib/pushbot/logs/<name>/          # deploy logs, newest first; secrets are masked
sudo pushbot doctor --url https://pushbot.example.com
```

## Before the request: action errors

| Message | Cause | Fix |
|---|---|---|
| `no OIDC token available: add 'permissions: id-token: write' ...` | The job has no `id-token: write`. A job-level `permissions:` block replaces the workflow-level one. Pull requests from forks never get tokens. | Add `id-token: write` to the job (or workflow) permissions. |
| `pushbot.yml not found` | No `actions/checkout` before the step, or the manifest is elsewhere. | Add `actions/checkout`, or set the `config` input to the manifest's path. |
| `yq is required on the runner` (or `jq`, `curl`, `tar`) | A self-hosted runner or job container without these tools. | Install them, or run the job on `ubuntu-latest`. |
| `tar: <path>: Cannot stat: No such file or directory` | A `files` entry does not exist in the checkout: a typo, a gitignored or generated file. | Fix the path, or generate the file in an earlier step. |
| `cannot get an OIDC token from GitHub` | GitHub's token service failed. | Re-run the job. |

## Connection errors

`request to <url> failed (curl exit code N)`:

| Code | Meaning | Fix |
|---|---|---|
| 6 | The hostname does not resolve. | Check `url` in the workflow and the DNS record. |
| 7 | Connection refused: the proxy is not running or the port is closed. | Check the proxy on the server and the firewall for 443. |
| 28 | Timed out: a firewall drops the traffic, or the address is not reachable from the internet (a LAN-only IP). | GitHub-hosted runners need a public address. |
| 35, 60 | TLS failed: no certificate, an expired one or one for another name. | Fix the certificate on the proxy. |
| 18, 52, 56, 92 | The connection broke mid-request: a proxy limit on body size or time. | Check the proxy against the requirements in [install-server.md](install-server.md#3-reverse-proxy). |

`the deploy log ended unexpectedly (connection lost?)`: the proxy closed the
stream, usually because its timeout is shorter than the deploy. The deploy keeps
running on the server; read its log there, then raise the proxy timeout (nginx
`proxy_read_timeout 20m`).

**The log arrives all at once at the end:** the proxy buffers responses. Caddy:
`flush_interval -1`; nginx: `proxy_buffering off`.

## Rejected by the server

`the server rejected the deploy (HTTP <code>)`, with pushbot's reason on the line
before:

| Code | Reason | Cause | Fix |
|---|---|---|---|
| 403 | `forbidden: repository <owner>/<repo> is not in the allow list` | The server does not admit this repository. | On the server, add a pattern to `allow` in `/etc/pushbot/config.yml` and `sudo systemctl restart pushbot`. |
| 403 | `forbidden: project name is taken by another repository: "<name>" belongs to <owner>/<repo>` | Another repository deployed under this name first. | Set another `name` in `pushbot.yml`. If the old repository is gone, release the name: remove its entry from `/var/lib/pushbot/projects.json` (no restart needed). |
| 401 | `unauthorized: oidc: expected audience "<a>" got ["<b>"]` | The action's `audience` input differs from the daemon's `audience` setting. | Make them equal; the default is `pushbot` on both sides. |
| 401 | `unauthorized: oidc: token is expired ...` or `... before the nbf (not before) time ...` | The server clock is wrong. | Enable time sync on the server: `sudo timedatectl set-ntp true`. `doctor` checks the clock. |
| 401 | `unauthorized: failed to verify signature: ...` | The server cannot fetch GitHub's OIDC keys (DNS, outbound HTTPS). | Run `sudo pushbot doctor`; fix outbound access to `token.actions.githubusercontent.com`. |
| 401 | `unauthorized: token already used` | The same token arrived twice, usually a proxy retrying the request. | Re-run the job; turn off retries to pushbot in the proxy. |
| 401 | `missing bearer token` | The proxy strips the `Authorization` header, or the request did not come from the action. | Pass `Authorization` through the proxy. |
| 400 | `pushbot.yml: ...` (with a line number) | Invalid manifest: an unknown key, a bad `files` entry, an empty `run`, empty `steps`. | Fix it; the keys are `name`, `files`, `steps` only (see [connect-project.md](connect-project.md#4-write-pushbotyml)). |
| 400 | `env line N: expected KEY=VALUE` | A line of `env` without `=`, often from a multi-line secret (N counts lines after secrets are substituted). | One `KEY=VALUE` per line; store multi-line values base64-encoded. |
| 400 | `env line N: invalid key ...`, `... values containing ' are not supported`, `duplicate key ...` | As it says. | Keys match `[A-Za-z_][A-Za-z0-9_]*`; no `'` in values; each key once. |
| 400 | `bad request: invalid project name ...; set name in pushbot.yml` | The repository name is not a valid project name. | Set `name:` matching `^[a-z0-9][a-z0-9._-]{0,62}$`. |
| 400 | `bad request: files: <path> is a file, give it a destination name instead of "."` | A file mapped as `<file>: .`. | Use `<file>: <name>` or just `<file>`; `: .` is for directories. |
| 400 | `bad request: bundle: ...` | A listed path contains a symlink, more files than allowed, or the files unpack to more than 200 MB. | Do not ship symlinks or large files; images belong in the registry. |
| 413 | `bundle exceeds N bytes` or `request body exceeds N bytes` | The packed files exceed `max_bundle_size` (50 MB). | Ship fewer files, or raise `max_bundle_size` on the server. |
| 413 | an HTML page, no `pushbot:` | The proxy's body limit (nginx defaults to 1 MB). | nginx: `client_max_body_size 64m;`. |
| 502, 504 | an HTML page | The proxy cannot reach pushbot: the service is down, or the proxy runs in Docker and targets `127.0.0.1`. | `sudo systemctl status pushbot`; see the Docker note in [install-server.md](install-server.md#3-reverse-proxy). |
| 503 | `shutting down, retry later` | The service was restarting. | Re-run the job. |
| 500 | `internal error, see the pushbot journal` | A server-side problem (disk, permissions). | `sudo journalctl -u pushbot -n 50`. |

## Deploy failed: `PUSHBOT: failed`

The deploy started; the reason follows `PUSHBOT: failed:`.

**`step N/M exited with code X`:** the lines above it are that step's output.
Common causes:

| Output | Cause | Fix |
|---|---|---|
| `unauthorized`, `denied` while pulling from ghcr.io | The server has no credentials for a private image. | Keep `registry-auth` at its default `true`, and give the job `packages: read` (or `write`). |
| `unauthorized`, `denied` from another registry (Docker Hub, a private one) | With `registry-auth: true`, steps use only the ghcr.io credentials from the job, not the server's own `docker login`. | Set `registry-auth: "false"` and log the `pushbot` user in on the server: `sudo -u pushbot -H docker login <registry>`. |
| `manifest unknown`, `not found` | The image or tag was never pushed: the name in the compose file differs from the one built (case, owner, suffix), or `IMAGE_TAG` is missing from `env`. | Make the names match; add `IMAGE_TAG=${{ github.sha }}` to `env`. |
| `no matching manifest for linux/arm64` (or `amd64`); `exec format error` in container logs | The image was built for another CPU architecture. | Build on `ubuntu-24.04-arm` for arm64 servers, or build multi-platform (see [connect-project.md](connect-project.md#5-write-the-workflow)). |
| `variable is not set. Defaulting to a blank string` | A variable used in the compose file is missing from `env`. | Add it to `env`. |
| `no configuration file provided: not found` | No compose file in the project directory under a default name. | List it in `files`, mapped to `compose.yml` if it has another name. |
| `port is already allocated`, `address already in use` | Another project or service uses the host port. | Pick a free port in the compose file. |
| `not a directory`, `Are you trying to mount a directory onto a file` | A bind-mounted file is not in `files`, so Docker created a directory in its place. | Add the file to `files`; on the server, remove the empty directory Docker created. |
| `permission denied ... docker.sock`, `docker: not found` | The `pushbot` user cannot use Docker. | `sudo pushbot doctor` names the fix. |

**`timeout`:** the deploy, including waiting for the previous deploy of the same
project, took longer than `deploy_timeout` (15 minutes), and the running step was
stopped. Usually slow image pulls or a step that never finishes, such as a
foreground `docker compose up` without `-d`. Fix the step, or raise
`deploy_timeout` on the server (and the proxy timeout with it).

**`copy <file>: ...`, `write .env: ...`, `create project directory: ...`:** pushbot
cannot write into `/srv/apps/<name>`, usually because a file there was created by
root. Fix the ownership of the files named in the error only:
`sudo chown pushbot:pushbot /srv/apps/<name>/<file>`. Do not `chown -R` the
project directory: container data under it has its own owners.

## Deployed, but the application does not work

On the server:

```sh
cd /srv/apps/<name>
sudo docker compose ps
sudo docker compose logs --tail 100
sudo cut -d= -f1 .env      # which variables were written, without their values
```

Do not print `.env` itself: it holds the secrets.

If the Actions job was cancelled, the deploy still ran to completion on the
server; its log is in `/var/lib/pushbot/logs/<name>/`.
