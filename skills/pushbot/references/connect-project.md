# Connect a repository to pushbot

You are in the project's repository. The goal: every push to the main branch builds
the image, pushes it to GitHub Container Registry (GHCR) and deploys it through
pushbot. You are done when a deploy ends with `PUSHBOT: success` and the
application answers.

## 1. Check the server

Ask the user for the pushbot URL (for example `https://pushbot.example.com`),
unless the repository already uses one. Check it:

```sh
curl -fsS https://pushbot.example.com/healthz   # must print: ok
```

If there is no server yet, or this check fails, go to
[install-server.md](install-server.md) first. One exception: if the check fails
only from inside the server's own network (a home LAN), the router may not
support NAT loopback. Ask the user to try from another network, such as a phone
hotspot, before suspecting the server.

Ask the user whether the repository's owner is in the server's `allow` list
(`/etc/pushbot/config.yml`). If it is not, the first deploy fails with 403. Adding
it is one command on the server, see [install-server.md](install-server.md#maintenance).

## 2. Look at the project before changing anything

Find out:

- **How it runs.** Find the compose files (`compose.yaml`, `compose.yml`,
  `docker-compose.yml`, `compose.*.yml`) and Dockerfiles. With a Dockerfile but no
  compose file, write one. With neither, ask the user how the project should run.
- **Which services are built from source** (`build:`) and which use published
  images (`image: postgres:16`). Every built service needs its own image in the
  registry.
- **Which repository files the containers read:** bind mounts such as
  `./nginx.conf:/etc/nginx/nginx.conf:ro` or `./config:/app/config`, and
  `env_file:` entries other than `.env`. Each must be listed in `files`, or it
  will not exist on the server. `.env` is written by pushbot and cannot be listed.
  Named volumes and data directories (`./data:/var/lib/postgresql/data`) need
  nothing.
- **Which variables it needs:** `${VAR}` in compose files, `environment:` entries,
  `.env.example`, settings the application reads. Split them into secrets
  (passwords, API keys, tokens) and plain settings.
- **Existing workflows** in `.github/workflows/`. If one already builds and pushes
  the image, add the pushbot step to it. Otherwise create
  `.github/workflows/deploy.yml`.
- **The server's CPU architecture.** Ask the user (`uname -m` on the server):
  `x86_64` is amd64, `aarch64` is arm64 (Raspberry Pi, Ampere, Apple-silicon VMs).
  An image built for the wrong architecture fails with `no matching manifest` or
  `exec format error`.
- **Ports.** Other projects on the same server may already use the host ports in
  the compose file. Ask the user which ports are free, or publish on `127.0.0.1`
  only when the application sits behind their reverse proxy.
- **Whether it already runs on the server,** started by hand or by another deploy
  (an SSH step in a workflow, a script). Then follow
  [Moving an existing deployment](#moving-an-existing-deployment) before the
  first deploy.

## 3. Make the compose file deployable

On the server, compose runs with nothing but the files from `files` and the
`.env` that pushbot writes. So:

- **Replace `build:` with an image tagged by the commit:**

  ```yaml
  services:
    app:
      image: ghcr.io/<owner>/<repo>:${IMAGE_TAG}
  ```

  Write `<owner>/<repo>` in lower case. With several built services, give each its
  own image, such as `ghcr.io/<owner>/<repo>-worker:${IMAGE_TAG}`.
- **Keep the development setup working.** If the compose file is used locally
  (`build:`, source code mounts, dev-only services), do not rewrite it. Write a
  separate production file, for example `deploy/compose.yml`, and map it to
  `compose.yml` on the server in `pushbot.yml`.
- **Pass variables to the containers explicitly.** pushbot writes `.env` into the
  project directory, and compose uses it only to fill in `${NAME}` in the compose
  file. A container sees a variable only when the compose file passes it:
  `environment:` with `DB_PASSWORD: ${DB_PASSWORD}`, or `env_file: .env` to pass
  all of them.
- **Keep data in named volumes or under the project directory** (`./data`):
  pushbot never deletes them.

### Moving an existing deployment

If the application already runs on the server, plan the switch with the user
before the first deploy; the user runs the server commands or approves them.

- `sudo docker compose ls` on the server shows the running compose projects and
  their compose files. pushbot runs compose in `/srv/apps/<name>`, so the new
  compose project is called `<name>` unless the compose file sets a top-level
  `name:`.
- Named volumes belong to their compose project (`<project>_<volume>`). If the new
  project name differs from the old one, the first deploy starts with empty
  volumes. To keep them, use the old project name: a top-level `name:` in the
  compose file, or the same `name` in `pushbot.yml`. With the same project name,
  the first deploy replaces the old containers in place.
- With a different project name, the old containers keep running and holding
  their ports, and the first deploy fails with `port is already allocated`. The
  user stops the old stack first, with `docker compose down` in its directory and
  **without `-v`**, which would delete the volumes.
- Data in the old directory (bind mounts such as `./data`) does not move by
  itself: the user copies it into `/srv/apps/<name>/` while the old stack is
  stopped.
- Once pushbot deploys work, remove the old deploy from the workflow (SSH steps,
  their secrets).

## 4. Write `pushbot.yml`

In the repository root. List the compose file and everything the containers
mount from the repository:

```yaml
# pushbot.yml
files:
  - deploy/compose.yml: compose.yml   # "src: dest" renames on the server
  - deploy/nginx.conf: nginx.conf
  - config/                           # a whole directory
env:
  file: deploy/production.env         # plain settings, copied into .env as is
  secrets: [DB_PASSWORD]              # GitHub secrets, by name
```

- **`files`:** each entry is a `path` (same path on the server) or a single
  `src: dest` pair. `dir/` copies a directory; `dir/: .` copies its contents into
  the project root. Paths are relative, without `..`; symlinks are not supported;
  `.env` is not allowed. The packed files must stay under 50 MB.
- **`env`:** where `.env` comes from besides the action's `env` input (see
  [section 6](#6-secrets-and-settings)). `file` is a dotenv file in the repository,
  copied into `.env` as is: quotes, `${VAR}` and comments work as compose reads
  them. `secrets` lists GitHub secret names; the action sends only these.
- **`name`:** optional, defaults to the lower-cased repository name, which becomes
  `/srv/apps/<name>`. Set it only when the user wants another name or the
  repository name does not match `^[a-z0-9][a-z0-9._-]{0,62}$`.
- **`steps`:** omit them to get `docker compose pull` and
  `docker compose up -d --remove-orphans`. Steps you write replace the defaults
  entirely, so keep those two if you still need them:

  ```yaml
  # pushbot.yml
  files:
    - compose.yml
  steps:
    - run: docker compose pull
    - run: docker compose run --rm app ./migrate
    - run: docker compose up -d --remove-orphans
  ```

- **`environments`:** only for several GitHub environments (staging and
  production) from one manifest. The job's `environment:` picks the block, which
  can override `name`, `files`, `env.file` and `env.secrets`; steps are shared.
  With `environments` set, the job's environment must be listed
  (`production: {}` for one without overrides), and environments on one server
  need distinct `name`s:

  ```yaml
  # pushbot.yml
  name: my-app
  files: [compose.yml]
  env:
    file: deploy/production.env
    secrets: [DB_PASSWORD]
  environments:
    production: {}
    staging:
      name: my-app-staging
      env:
        file: deploy/staging.env
  ```

- **No other keys.** pushbot rejects unknown keys.

To deploy several projects from one repository, write one manifest per project
(`deploy/api.pushbot.yml`, ...) with distinct `name`s and point each pushbot step
at its manifest with the `config` input.

## 5. Write the workflow

```yaml
name: deploy

on:
  push:
    branches: [main]

permissions:
  contents: read
  packages: write     # push the image to GHCR; the server pulls it with the same token
  id-token: write     # OIDC token for pushbot

concurrency: deploy   # deploys go in push order; a newer pending run replaces an older one

jobs:
  deploy:
    runs-on: ubuntu-latest      # ubuntu-24.04-arm for an arm64 server
    environment: production
    steps:
      - uses: actions/checkout@v4
      - uses: docker/login-action@v3
        with: { registry: ghcr.io, username: "${{ github.actor }}", password: "${{ github.token }}" }
      - uses: docker/build-push-action@v6
        with:
          push: true
          tags: ghcr.io/<owner>/<repo>:${{ github.sha }}

      - uses: nikitakiselev/pushbot@v1
        with:
          url: https://pushbot.example.com
          secrets: ${{ toJSON(secrets) }}   # the action sends only those in env.secrets
          env: |
            IMAGE_TAG=${{ github.sha }}
```

Check every point:

- **`id-token: write`** is set. A `permissions:` block sets every permission it does
  not list to `none`, and a job-level block replaces the workflow-level one. So
  wherever you add `id-token: write`, list everything else the job needs too:
  `contents: read` for checkout, `packages: write` to push to GHCR.
- **The image tag is `${{ github.sha }}`** and `IMAGE_TAG` in `env` is the same
  value. Not `latest`: with the commit tag the server runs exactly this commit, and
  re-running an old workflow rolls back.
- **The image name is lower case.** `ghcr.io/${{ github.repository }}` works only
  when the owner and repository names have no capitals: GHCR rejects
  `ghcr.io/MyOrg/App`. Otherwise write the name out in lower case.
- **The architecture matches the server.** For arm64, `runs-on: ubuntu-24.04-arm`
  builds a native arm64 image (available for public and private repositories). To
  build both architectures on one runner, add `docker/setup-qemu-action@v3` and
  `docker/setup-buildx-action@v3` before the build and set
  `platforms: linux/amd64,linux/arm64`.
- **Every variable the compose file needs comes from exactly one source:**
  `env` (one `KEY=VALUE` per line, for values computed in CI), `env.secrets` or
  `env.file`. Together they replace `.env` completely. `env` and secret values are
  taken literally: a value wrapped in `"..."` and a `${VAR}` naming another
  variable from these sources are rejected, `$$` stays `$$` (with a warning in the
  log); no `'`, no line breaks. Store a multi-line secret (a PEM key) base64-encoded
  and decode it in the application.
- **Private images:** by default the action sends the job's `github.token`, and
  the server pulls from ghcr.io with it while the job runs. That needs
  `packages: read` or `write` in the job running the pushbot step. An image
  published from another repository is pullable only if its package grants this
  repository access (package settings → Manage Actions access). Images in other
  private registries: see [troubleshooting.md](troubleshooting.md#deploy-failed-pushbot-failed).
- **The pushbot step runs after `actions/checkout`** in the same job: it reads
  `pushbot.yml` and the files to send from the working tree. If building and
  deploying are separate jobs, the deploy job needs `actions/checkout`,
  `id-token: write` and `needs:` on the build job.
- **Several built services:** one `docker/build-push-action` step per image, with
  `context:` and `file:` set for each.
- **`environment: production` is optional** unless `pushbot.yml` has
  `environments`. It lets the user keep secrets in a GitHub environment with
  protection rules. Without it, secrets come from the repository.
- **Do not trigger the deploy on `pull_request`.** Every pull request would deploy
  to production.

## 6. Secrets and settings

Show the user every variable: its name, what it is, and whether it is a secret.

- **Secrets:** list their names in `env.secrets`, and pass
  `secrets: ${{ toJSON(secrets) }}` to the pushbot step. The user sets the values.
  Do not ask for them in the chat. They run
  `gh secret set DB_PASSWORD` in their own terminal, or use Settings → Secrets and
  variables → Actions. `gh` prompts for the value only in a terminal; without one
  it reads stdin and can store an empty value. Repository secrets work with or
  without `environment:`. To store a secret in the environment instead
  (`--env production`), the environment must exist first: Settings →
  Environments, or `gh api -X PUT repos/{owner}/{repo}/environments/production`.
- **Plain settings:** put them into the `env.file` dotenv file in the repository
  (`deploy/production.env`), where they get history and review. Keep secrets out
  of it: its values are not masked in the server logs. An existing
  `.env.example` is a good start; dotenv syntax (`APP_NAME="My app"`,
  `MAIL_FROM_NAME="${APP_NAME}"`) works there as locally. A file may refer to
  secrets and `env` values, which come first in `.env`.
- **Check that every secret exists** with `gh secret list` (and
  `gh secret list --env production`). The action fails before the deploy when a
  secret from `env.secrets` is missing, but a secret stored empty passes.

## 7. Deploy and verify

Ask the user before committing and pushing. After the push:

```sh
sha=$(git rev-parse HEAD)
workflow=deploy.yml                     # the workflow file with the pushbot step
for i in $(seq 1 12); do                # the run takes a few seconds to appear
  run_id=$(gh run list --commit "$sha" --workflow "$workflow" --limit 1 --json databaseId --jq '.[0].databaseId')
  [ -n "$run_id" ] && break
  sleep 5
done
gh run watch "$run_id" --exit-status --compact
gh run view "$run_id" --log-failed      # when it failed
```

A run takes minutes. If your command times out, run `gh run watch` again; it
picks up where the run is. The pushbot step's log ends with
`PUSHBOT: success (…s)`. Then check the application itself (its URL or port): a
successful `docker compose up -d` does not mean the application started. If the
user has access to the server, the commands in
[troubleshooting.md](troubleshooting.md#deployed-but-the-application-does-not-work)
show the containers.

When anything fails, go to [troubleshooting.md](troubleshooting.md).

## Checklist

- [ ] `curl -fsS <url>/healthz` prints `ok`
- [ ] the workflow (or the deploy job) has `id-token: write`
- [ ] images are tagged `${{ github.sha }}`, names are lower case, the architecture matches the server
- [ ] the compose file on the server has no `build:`; it uses `${IMAGE_TAG}`
- [ ] every bind-mounted repository file is in `files`
- [ ] every variable comes from one of `env`, `env.secrets`, `env.file` and reaches the containers (`environment:` or `env_file: .env`); every secret exists in GitHub
- [ ] an existing deployment on the server is stopped or taken over, its data kept
- [ ] the deploy ends with `PUSHBOT: success` and the application answers
