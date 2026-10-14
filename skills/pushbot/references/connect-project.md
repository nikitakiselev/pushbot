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
[install-server.md](install-server.md) first.

Ask the user whether the repository's owner is in the server's `allow` list
(`/etc/pushbot/config.yml`). If it is not, the first deploy fails with 403. Adding
it is one line on the server, see [install-server.md](install-server.md#maintenance).

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
  `env_file:` entries. Each must be listed in `files`, or it will not exist on the
  server. Named volumes and data directories (`./data:/var/lib/postgresql/data`)
  need nothing.
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
- **Take variables from `.env`:** write `${NAME}` in the compose file. Compose reads
  `.env` from the project directory by itself.
- **Keep data in named volumes or under the project directory** (`./data`):
  pushbot never deletes them.

## 4. Write `pushbot.yml`

In the repository root. List the compose file and everything the containers
mount from the repository:

```yaml
# pushbot.yml
files:
  - deploy/compose.yml: compose.yml   # "src: dest" renames on the server
  - deploy/nginx.conf: nginx.conf
  - config/                           # a whole directory
```

- **`files`:** each entry is a `path` (same path on the server) or a single
  `src: dest` pair. `dir/` copies a directory; `dir/: .` copies its contents into
  the project root. Paths are relative, without `..`; symlinks are not supported;
  `.env` is not allowed. The packed files must stay under 50 MB.
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
          env: |
            IMAGE_TAG=${{ github.sha }}
            DB_PASSWORD=${{ secrets.DB_PASSWORD }}
```

Check every point:

- **`id-token: write`** is set. Permissions set on a job replace the workflow-level
  ones entirely, so a job with its own `permissions:` needs `id-token: write`
  there too.
- **The image tag is `${{ github.sha }}`** and `IMAGE_TAG` in `env` is the same
  value. Not `latest`: with the commit tag the server runs exactly this commit, and
  re-running an old workflow rolls back.
- **The image name is lower case**, written out literally. Do not use
  `ghcr.io/${{ github.repository }}`: it keeps capital letters (`MyOrg/App`), and
  GHCR rejects them.
- **The architecture matches the server.** For arm64, `runs-on: ubuntu-24.04-arm`
  builds a native arm64 image (available for public and private repositories). To
  build both architectures on one runner, add `docker/setup-qemu-action@v3` and
  `docker/setup-buildx-action@v3` before the build and set
  `platforms: linux/amd64,linux/arm64`.
- **`env` lists every variable the compose file needs,** one `KEY=VALUE` per line.
  It replaces `.env` completely. Values cannot contain `'`. A value cannot span
  lines: each line is a separate entry, so a multi-line secret (a PEM key)
  breaks the deploy. Store it base64-encoded and decode it in the application.
- **Private images:** by default the action sends the job's `github.token`, and
  the server pulls from ghcr.io with it while the job runs. That needs
  `packages: read` or `write` in the job running the pushbot step. Images in other
  private registries: see [troubleshooting.md](troubleshooting.md#deploy-failed-pushbot-failed).
- **The pushbot step runs after `actions/checkout`** in the same job: it reads
  `pushbot.yml` and the files to send from the working tree. If building and
  deploying are separate jobs, the deploy job needs `actions/checkout`,
  `id-token: write` and `needs:` on the build job.
- **Several built services:** one `docker/build-push-action` step per image, with
  `context:` and `file:` set for each.
- **`environment: production` is optional.** It lets the user keep secrets in a
  GitHub environment with protection rules. Without it, secrets come from the
  repository.
- **Do not trigger the deploy on `pull_request`.** Every pull request would deploy
  to production.

## 6. Secrets and settings

Show the user every variable: its name, what it is, and whether it is a secret.

- **Secrets:** the user sets the values. Do not ask for them in the chat. They run
  `gh secret set DB_PASSWORD` (it prompts for the value; add
  `--env production` to store it in the environment) or use Settings → Secrets and
  variables → Actions. In Claude Code they can type `! gh secret set DB_PASSWORD`.
- **Plain settings:** write them into `env` directly (`LOG_LEVEL=info`) or keep
  them in repository variables (`LOG_LEVEL=${{ vars.LOG_LEVEL }}`).
- **Check that every secret exists** with `gh secret list` (and
  `gh secret list --env production`). A missing secret silently becomes an empty
  value.

## 7. Deploy and verify

Ask the user before committing and pushing. After the push:

```sh
sha=$(git rev-parse HEAD)
run_id=$(gh run list --commit "$sha" --workflow deploy.yml --limit 1 --json databaseId --jq '.[0].databaseId')
gh run watch "$run_id" --exit-status
gh run view "$run_id" --log-failed      # when it failed
```

The run may need a few seconds to appear. The pushbot step's log ends with
`PUSHBOT: success (…s)`. Then check the application itself (its URL or port): a
successful `docker compose up -d` does not mean the application started. If the
user has access to the server, `docker compose ps` and `docker compose logs` in
`/srv/apps/<name>` show the containers.

When anything fails, go to [troubleshooting.md](troubleshooting.md).

## Checklist

- [ ] `curl -fsS <url>/healthz` prints `ok`
- [ ] the workflow (or the deploy job) has `id-token: write`
- [ ] images are tagged `${{ github.sha }}`, names are lower case, the architecture matches the server
- [ ] the compose file on the server has no `build:`; it uses `${IMAGE_TAG}`
- [ ] every bind-mounted repository file is in `files`
- [ ] every variable the compose file uses is in `env`; every secret exists in GitHub
- [ ] the deploy ends with `PUSHBOT: success` and the application answers
