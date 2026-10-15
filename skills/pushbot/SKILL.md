---
name: pushbot
description: Connect a repository to a pushbot server, install or configure the pushbot daemon, or debug a failed pushbot deploy. Use when the user mentions pushbot, the repository has a pushbot.yml or a workflow that uses nikitakiselev/pushbot, or the user wants GitHub Actions to deploy docker compose to their own server without SSH.
---

# pushbot

pushbot deploys docker compose projects from GitHub Actions to a personal server.
The workflow builds an image and pushes it to a registry. Its last step, the
`nikitakiselev/pushbot@v1` action, sends `pushbot.yml`, the files it lists, the
secrets it names and the `env` input to the pushbot daemon over HTTPS. The daemon
checks the job's GitHub OIDC token against its `allow` list, puts the files into
`/srv/apps/<name>`, writes `.env` and runs the steps (by default `docker compose pull` and
`docker compose up -d --remove-orphans`). The deploy log streams back into the
Actions log.

These instructions are for pushbot v1 and the action `nikitakiselev/pushbot@v1`.

## What to read next

| Task | Read | Where you work |
|---|---|---|
| Deploy a repository with pushbot: `pushbot.yml`, workflow, compose file, secrets | [references/connect-project.md](references/connect-project.md) | the project repository |
| Install, configure or upgrade the daemon; set up the reverse proxy | [references/install-server.md](references/install-server.md) | a shell on the server, with sudo |
| A deploy failed, or the server rejects requests | [references/troubleshooting.md](references/troubleshooting.md) | the Actions log, then the server |

If you are reading this from a URL rather than an installed skill, the files are
next to this one:
`https://raw.githubusercontent.com/nikitakiselev/pushbot/v1/skills/pushbot/references/<file>`.
Download them in full (for example with `curl -fsSL`) rather than through a tool
that summarizes pages: the exact commands and error texts matter.

## How pushbot behaves

Most mistakes come from assuming pushbot works like a CI runner or an SSH deploy.
It does not:

- **The server builds nothing and clones nothing.** Images come from a registry.
  Only the files listed under `files` in `pushbot.yml` reach the server; the action
  packs them from the checked-out repository.
- **`.env` is rewritten on every deploy** from three sources and nothing else: the
  action's `env` input (values computed in CI, such as `IMAGE_TAG`), the GitHub
  secrets named in `env.secrets` of `pushbot.yml`, and the dotenv file `env.file`
  from the repository (plain settings, copied as is). A variable in none of them is
  gone after the next deploy; a key in two of them is an error. `env` and secret
  values are taken literally: no quotes, no `${VAR}`. `.env` cannot be listed in
  `files`.
- **Nothing on the server is ever deleted.** Files are overwritten; a file dropped
  from `files` stays on the server; container data such as `./data` is safe.
- **CI needs no SSH key and no pushbot token.** Authentication is the job's OIDC
  token, which requires `permissions: id-token: write`. The only secrets are the
  application's own, listed by name in `env.secrets`.
- **`pushbot.yml` knows five keys, all optional: `name`, `files`, `env`, `steps`,
  `environments`.** Any other key is an error. A step is `- run: <shell script>`
  and nothing else. `environments.<GitHub environment>` overrides `name`, `files`
  and `env` for staging, production and so on; steps are shared.
- **Steps run in `/srv/apps/<name>` as the `pushbot` user** with `sh -e`, without
  the daemon's environment. They get the variables from `env` and the secrets plus
  `PUSHBOT_PROJECT`, `PUSHBOT_ENVIRONMENT`, `PUSHBOT_REPOSITORY`, `PUSHBOT_SHA`,
  `PUSHBOT_REF`, `PUSHBOT_ACTOR`, `PUSHBOT_RUN_ID` and `PUSHBOT_RUN_URL`. Variables
  from `env.file` reach them only through `.env`.
- **The first deploy binds the project name to the repository.** The name defaults
  to the lower-cased repository name; another repository with the same name gets
  403.
- **Deploys of one project run one at a time.** A deploy, including the wait for
  the previous one, is limited to 15 minutes by default. There is no automatic
  rollback: re-running the workflow for an older commit rolls back.

## Rules

- **Ask, don't guess,** what you cannot see: the server's URL, the GitHub owner to
  allow, the hostname, the server's CPU architecture. A wrong guess here fails
  late and confusingly.
- **Never write secret values** into the repository, `pushbot.yml`, `env.file` or
  the workflow. Secrets live in GitHub and are named in `env.secrets`; the user
  enters the values.
- **On the server, show a command before running anything that changes the
  system** (installing, editing configs, restarting services) and run it only
  after the user agrees. Read-only checks need no confirmation.
- **Prove it works.** A connected project is done when a deploy ends with
  `PUSHBOT: success` and the application answers. A server is done when
  `sudo pushbot doctor --url <public url>` reports no failures (with one exception
  on home networks, see [install-server.md](references/install-server.md#4-verify)).
