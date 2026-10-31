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
- A deploy that started prints lines starting with `==>`, one per step, and ends
  with `PUSHBOT: success (…s)` or `PUSHBOT: failed: <reason>`. Only a failed step's
  output is shown, right above `PUSHBOT: failed`; for the output of every step set
  `verbose: true` on the pushbot step, or re-run the job with debug logging.

**The server**, if the user can give you a shell there:

```sh
sudo journalctl -u pushbot --since "1 hour ago"   # rejected requests with the reason and repository
sudo ls -t /var/lib/pushbot/logs/<name>/          # deploy logs, newest first; env and secret values of 4+ characters are masked
sudo pushbot doctor --url https://pushbot.example.com
```

## Before the request: action errors

| Message | Cause | Fix |
|---|---|---|
| `no OIDC token available: add 'permissions: id-token: write' ...` | The job has no `id-token: write`. A job-level `permissions:` block replaces the workflow-level one. Pull requests from forks never get tokens. | Add `id-token: write` to the job (or workflow) permissions. A `permissions:` block sets everything it does not list to `none`, so list the rest too: `contents: read`, `packages: write`. |
| `pushbot.yml not found` | No `actions/checkout` before the step, or the manifest is elsewhere. | Add `actions/checkout`, or set the `config` input to the manifest's path. |
| `yq is required on the runner` (or `jq`, `curl`, `tar`, `base64`) | A self-hosted runner or job container without these tools. | Install them, or run the job on `ubuntu-latest`. |
| `tar: <path>: Cannot stat: No such file or directory` | A `files` entry does not exist in the checkout: a typo, a gitignored or generated file. | Fix the path, or generate the file in an earlier step. |
| `cannot get an OIDC token from GitHub` | GitHub's token service failed. | Re-run the job. |
| `env.file <path> not found` | The `env.file` from `pushbot.yml` does not exist in the checkout. | Fix the path or commit the file. |
| `env.secrets is set in pushbot.yml: add secrets: ${{ toJSON(secrets) }} to the pushbot step` | The step has no `secrets` input. | Add it as shown. |
| `secret <NAME> is not set in GitHub ...: gh secret set <NAME> ...` | A secret from `env.secrets` does not exist for this job. Environment secrets reach only jobs with that `environment:`; a job without one sees repository secrets only. | If the job should run in an environment, add `environment:` to it. Otherwise the user runs the command shown. |
| `environment "<x>" is not listed in environments of pushbot.yml (...)` | `pushbot.yml` has `environments`, and the job's `environment:` is not among them (names compare case-insensitively). | Fix `environment:` in the workflow, or add `<x>: {}` to `environments`. |
| `pushbot.yml lists environments, but the job has no environment: ...` | `pushbot.yml` has `environments`, the job has no `environment:`. | Add `environment: <name>` to the job. |
| `pushbot.yml: <key> must be a map` / `must be a list`, `pushbot.yml: env.secrets: invalid name ...`, `cannot parse pushbot.yml` | The action reads `files`, `env` and `environments` itself, before the server checks the rest. | Fix `pushbot.yml` (see [connect-project.md](connect-project.md#4-write-pushbotyml)). |
| `the server is too old for env.secrets: run sudo pushbot upgrade on it` | The daemon predates `env.secrets` and answered `unknown field "secrets"`. | On the server: `sudo pushbot upgrade`. |

## Connection errors

`request to <url> failed (curl exit code N)`:

| Code | Meaning | Fix |
|---|---|---|
| 6 | The hostname does not resolve. | Check `url` in the workflow and the DNS record. |
| 7 | Connection refused: the proxy is not running or the port is closed. | Check the proxy on the server and the firewall for 443. |
| 28 | Timed out: a firewall drops the traffic, or the address is not reachable from the internet (a LAN-only IP). | GitHub-hosted runners need a public address. |
| 35, 60 | TLS failed: no certificate, an expired one or one for another name. | Fix the certificate on the proxy. |
| 18, 52, 55, 56, 92 | The connection broke mid-request: a proxy limit on body size or time. | Check the proxy against the requirements in [install-server.md](install-server.md#3-reverse-proxy). |

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
| 403 | `forbidden: repository <owner>/<repo> is not in the allow list` | The server does not admit this repository. | On the server, add a pattern to `allow` with `sudo pushbot init --force`, passing every pattern because `--allow` replaces the list (see [install-server.md](install-server.md#maintenance)). |
| 403 | `forbidden: project name is taken by another repository: "<name>" belongs to <owner>/<repo>` | Another repository deployed under this name first. | Set another `name` in `pushbot.yml`. If the old repository is gone, release the name on the server (see [install-server.md](install-server.md#maintenance)). |
| 401 | `unauthorized: oidc: expected audience "<a>" got ["<b>"]` | The action's `audience` input differs from the daemon's `audience` setting. | Make them equal; the default is `pushbot` on both sides. |
| 401 | `unauthorized: oidc: token is expired ...` or `... before the nbf (not before) time ...` | The server clock is wrong. | Enable time sync on the server: `sudo timedatectl set-ntp true`. `doctor` checks the clock. |
| 401 | `unauthorized: failed to verify signature: ...` | The server cannot fetch GitHub's OIDC keys (DNS, outbound HTTPS). | Run `sudo pushbot doctor`; fix outbound access to `token.actions.githubusercontent.com`. |
| 401 | `unauthorized: token already used` | The same token arrived twice, usually a proxy retrying the request. | Re-run the job; turn off retries to pushbot in the proxy. |
| 401 | `missing bearer token` | The proxy strips the `Authorization` header, or the request did not come from the action. | Pass `Authorization` through the proxy. |
| 400 | `pushbot.yml: ...` (with a line number) | Invalid manifest: an unknown key, a bad `files` or `env` entry, an empty `run`, empty `steps`, `steps` under `environments`. | Fix it; the keys are `name`, `files`, `env`, `steps`, `environments` (see [connect-project.md](connect-project.md#4-write-pushbotyml)). On an older server the new keys are unknown: `sudo pushbot upgrade`. |
| 400 | `bad request: <KEY>: the value is wrapped in double quotes ...` | A value in `env` or a secret was written dotenv-style, `"..."`. | Remove the quotes, or move the variable to `env.file`. |
| 400 | `bad request: <KEY>: ${<OTHER>} is not expanded in env and secrets ...` | An `env` value or a secret refers to another variable. | Move `<KEY>` to `env.file`, where `${...}` works. |
| 400 | `bad request: <KEY> is set both in <a> and in <b>; keep one` | The same variable comes from two of `env`, the secrets and `env.file`. | Remove it from one. |
| 400 | `bad request: <file> line N: ...`, `bad request: env.file <file>: ...` | `env.file` has a line that is not `KEY=VALUE`, a duplicate key or an unclosed quote; or it is missing from the bundle or over 64 KB. | Fix the file. |
| 400 | `bad request: secret <NAME> is declared in pushbot.yml but was not sent` | The pushbot step has no `secrets` input that reached the server. | Pass `secrets: ${{ toJSON(secrets) }}`. |
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
| `unauthorized`, `denied` while pulling from ghcr.io | The server has no credentials for a private image, or the image belongs to another repository's package. | Keep `registry-auth` at its default `true`, and give the job `packages: read` (or `write`). For another repository's image, grant this repository access in the package settings (Manage Actions access). |
| `unauthorized`, `denied` from another registry (Docker Hub, a private one) | With `registry-auth: true`, steps use only the ghcr.io credentials from the job, not the server's own `docker login`. | Set `registry-auth: "false"` and log the `pushbot` user in on the server: `sudo -u pushbot -H docker login <registry>`. This drops the job's ghcr.io credentials too, so private ghcr.io images then need `docker login ghcr.io` with a personal access token as well. |
| `toomanyrequests` | Docker Hub's rate limit for anonymous pulls. With `registry-auth: true` the server's own Docker Hub login is not used. | Re-run later, or set `registry-auth: "false"` and `docker login` on the server as above. |
| `invalid reference format` | `IMAGE_TAG` is missing from `env`, so the image becomes `ghcr.io/<owner>/<repo>:`; or the name has capitals (`repository name must be lowercase`). | Add `IMAGE_TAG=${{ github.sha }}` to `env`; write the image name in lower case. |
| `manifest unknown`, `not found` | The image or tag was never pushed: the name in the compose file differs from the one built (owner, repository, suffix). | Make the names match. |
| `no matching manifest for linux/arm64` (or `amd64`); `exec format error` in container logs | The image was built for another CPU architecture. | Build on `ubuntu-24.04-arm` for arm64 servers, or build multi-platform (see [connect-project.md](connect-project.md#5-write-the-workflow)). |
| `variable is not set. Defaulting to a blank string` | A variable used in the compose file is missing from `.env`. | Add it to `env`, `env.secrets` or `env.file`. |
| `no configuration file provided: not found` | No compose file in the project directory under a default name. | List it in `files`, mapped to `compose.yml` if it has another name. |
| `port is already allocated`, `address already in use` | Another project or service uses the host port. | Pick a free port in the compose file. |
| `not a directory`, `Are you trying to mount a directory onto a file` | A bind-mounted file is not in `files`, so Docker created a directory in its place. | Add the file to `files`. Before the next deploy, remove the empty directory Docker created (`sudo rmdir /srv/apps/<name>/<file>`), or the copy fails with `is a directory`. |
| `permission denied ... docker.sock`, `docker: not found` | The `pushbot` user cannot use Docker. | `sudo pushbot doctor` names the fix. |

**`timeout`:** the deploy, including waiting for the previous deploy of the same
project, took longer than `deploy_timeout` (15 minutes), and the running step was
stopped. Usually slow image pulls or a step that never finishes, such as a
foreground `docker compose up` without `-d`. Fix the step, or raise
`deploy_timeout` on the server (and the proxy timeout with it).

**`copy <file>: ...`, `write .env: ...`, `create project directory: ...`:** pushbot
writes each file to a temporary file next to it and renames it into place, so it
needs write access to the directory that holds the file: `/srv/apps/<name>` or a
subdirectory of it. The usual cause is a directory created by root or by a
container. Fix the owner of that directory only:
`sudo chown pushbot:pushbot /srv/apps/<name>/<dir>`. Do not `chown -R` the project
directory: container data under it has its own owners. `... is a directory` means
a directory sits where the file should go (see the bind mount row above).

## Deployed, but the application does not work

On the server (`/srv/apps` is readable only by root and `pushbot`, hence `sudo sh -c`):

```sh
sudo sh -c 'cd /srv/apps/<name> && docker compose ps && docker compose logs --tail 100'
sudo grep -v '^#' /srv/apps/<name>/.env | cut -d= -f1   # which variables were written, without their values
```

If a variable is in `.env` but the application does not see it, the compose file
does not pass it to the container: see `environment:` / `env_file: .env` in
[connect-project.md](connect-project.md#3-make-the-compose-file-deployable).

Do not print `.env` itself: it holds the secrets.

If the Actions job was cancelled, the deploy still ran to completion on the
server; its log is in `/var/lib/pushbot/logs/<name>/`.
