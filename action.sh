#!/usr/bin/env bash
# pushbot GitHub Action: get an OIDC token, pack the files listed in pushbot.yml
# for the job's environment, POST everything with the declared secrets to the
# daemon and stream its log. Exits non-zero unless the daemon's last line is
# "PUSHBOT: success".
set -euo pipefail

die() {
  echo "::error::pushbot: $*" >&2
  exit 1
}

for bin in yq jq curl tar base64; do
  command -v "$bin" >/dev/null || die "$bin is required on the runner"
done
[ -n "${PUSHBOT_URL:-}" ] || die "the url input is required"
[ -n "${ACTIONS_ID_TOKEN_REQUEST_URL:-}" ] || die "no OIDC token available: add 'permissions: id-token: write' to the workflow or job"
[ -f "$PUSHBOT_CONFIG" ] || die "$PUSHBOT_CONFIG not found"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

oidc=$(curl -sSf -H "Authorization: bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN" \
  "$ACTIONS_ID_TOKEN_REQUEST_URL&audience=${PUSHBOT_AUDIENCE:-pushbot}" | jq -r .value) ||
  die "cannot get an OIDC token from GitHub"
[ -n "$oidc" ] && [ "$oidc" != null ] || die "GitHub returned an empty OIDC token"
# Keep the token off the command line (and out of ps).
printf 'Authorization: Bearer %s\n' "$oidc" >"$work/headers"

# The job's environment from the token payload (base64url JSON) picks the files
# and secrets to send. The daemon verifies the token and decides on its own.
payload=$(printf '%s' "$oidc" | cut -d. -f2 | tr '_-' '/+')
case $((${#payload} % 4)) in
2) payload+="==" ;;
3) payload+="=" ;;
esac
environment=$(printf '%s' "$payload" | base64 -d | jq -r '.environment // ""') ||
  die "cannot read the OIDC token"
if [ -n "$environment" ]; then echo "pushbot: environment $environment"; fi

# files, env.file and env.secrets for this environment: a key under
# environments.<name> replaces the top-level one. An unlisted environment gets
# the top level here; the daemon rejects it with a clear message. A files item
# is a path (string) or a "src: dest" map, whose key is the path.
PUSHBOT_ENVIRONMENT="$environment" yq -o=json '
  (.environments // {} | to_entries
    | map(select((.key | downcase) == (strenv(PUSHBOT_ENVIRONMENT) | downcase)))
    | .[0].value // {}) as $o
  | {"files": [($o.files // .files // [])[] | select(tag == "!!map") |= keys[0]],
     "env_file": ($o.env.file // .env.file // ""),
     "secrets": ($o.env.secrets // .env.secrets // [])}
' "$PUSHBOT_CONFIG" >"$work/manifest.json"

files=()
while IFS= read -r f; do
  if [ -n "$f" ]; then files+=("$f"); fi
done < <(jq -r '.files[]' "$work/manifest.json")
env_file=$(jq -r .env_file "$work/manifest.json")
if [ -n "$env_file" ]; then
  [ -f "$env_file" ] || die "env.file $env_file not found"
  files+=("$env_file")
fi

form=(-F "manifest=<$PUSHBOT_CONFIG")
if [ ${#files[@]} -gt 0 ]; then
  echo "pushbot: packing ${files[*]}"
  # COPYFILE_DISABLE keeps macOS tar from adding ._* AppleDouble files.
  COPYFILE_DISABLE=1 tar -czf "$work/bundle.tgz" -- "${files[@]}"
  form+=(-F "bundle=@$work/bundle.tgz;type=application/gzip")
fi

printf '%s' "${PUSHBOT_ENV:-}" >"$work/env"
form+=(-F "env=<$work/env")

# Send only the secrets pushbot.yml declares. GitHub secret names are
# case-insensitive; toJSON(secrets) has them upper-cased.
names=$(jq -c .secrets "$work/manifest.json")
if [ "$names" != "[]" ]; then
  [ -n "${PUSHBOT_SECRETS:-}" ] ||
    die "env.secrets is set in $PUSHBOT_CONFIG: add secrets: \${{ toJSON(secrets) }} to the pushbot step"
  printf '%s' "$PUSHBOT_SECRETS" >"$work/all-secrets"
  jq -e 'type == "object"' "$work/all-secrets" >/dev/null 2>&1 ||
    die "the secrets input is not a JSON object: pass secrets: \${{ toJSON(secrets) }}"
  missing=$(jq -r --argjson names "$names" \
    'with_entries(.key |= ascii_upcase) as $s | $names[] | select($s[ascii_upcase] == null)' "$work/all-secrets")
  if [ -n "$missing" ]; then
    where="" flag=""
    if [ -n "$environment" ]; then
      where=" (environment $environment)"
      flag=" --env $(printf '%q' "$environment")"
    fi
    while IFS= read -r name; do
      echo "::error::pushbot: secret $name is not set in GitHub$where: gh secret set $name$flag" >&2
    done <<<"$missing"
    exit 1
  fi
  jq --argjson names "$names" \
    'with_entries(.key |= ascii_upcase) as $s | reduce $names[] as $n ({}; .[$n] = $s[$n | ascii_upcase])' \
    "$work/all-secrets" >"$work/secrets"
  echo "pushbot: secrets $(jq -r 'join(", ")' <<<"$names")"
  form+=(-F "secrets=<$work/secrets")
fi

if [ "${PUSHBOT_REGISTRY_AUTH:-true}" = "true" ]; then
  printf '%s' "$PUSHBOT_REGISTRY_USER" >"$work/registry_user"
  printf '%s' "$PUSHBOT_REGISTRY_TOKEN" >"$work/registry_token"
  form+=(-F "registry_user=<$work/registry_user" -F "registry_token=<$work/registry_token")
fi

set +e
curl -sS -N -H @"$work/headers" -H 'Expect:' -D "$work/response-headers" \
  "${form[@]}" "${PUSHBOT_URL%/}/v1/deploy" | tee "$work/out"
rc=${PIPESTATUS[0]}
set -e
[ "$rc" -eq 0 ] || die "request to $PUSHBOT_URL failed (curl exit code $rc)"

# The last status line wins (a proxy may send "100 Continue" first).
code=$(grep -E '^HTTP/' "$work/response-headers" | tail -n1 | awk '{print $2}')
[ "$code" = 200 ] || die "the server rejected the deploy (HTTP $code)"

last=$(tail -n1 "$work/out")
case "$last" in
"PUSHBOT: success"*) exit 0 ;;
"PUSHBOT: failed"*) die "${last#PUSHBOT: }" ;;
*) die "the deploy log ended unexpectedly (connection lost?); check the server log" ;;
esac
