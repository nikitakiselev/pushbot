#!/usr/bin/env bash
# pushbot GitHub Action: pack the files listed in pushbot.yml, get an OIDC token,
# POST everything to the daemon and stream its log. Exits non-zero unless the
# daemon's last line is "PUSHBOT: success".
set -euo pipefail

die() {
  echo "::error::pushbot: $*" >&2
  exit 1
}

for bin in yq jq curl tar; do
  command -v "$bin" >/dev/null || die "$bin is required on the runner"
done
[ -n "${PUSHBOT_URL:-}" ] || die "the url input is required"
[ -n "${ACTIONS_ID_TOKEN_REQUEST_URL:-}" ] || die "no OIDC token available: add 'permissions: id-token: write' to the workflow or job"
[ -f "$PUSHBOT_CONFIG" ] || die "$PUSHBOT_CONFIG not found"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# Sources from the files list: a string item is the path, a map item is its key.
files=()
while IFS= read -r f; do
  if [ -n "$f" ]; then files+=("$f"); fi
done < <(yq '(.files // [])[] | select(tag == "!!map") |= keys[0]' "$PUSHBOT_CONFIG")

form=(-F "manifest=<$PUSHBOT_CONFIG")
if [ ${#files[@]} -gt 0 ]; then
  echo "pushbot: packing ${files[*]}"
  # COPYFILE_DISABLE keeps macOS tar from adding ._* AppleDouble files.
  COPYFILE_DISABLE=1 tar -czf "$work/bundle.tgz" -- "${files[@]}"
  form+=(-F "bundle=@$work/bundle.tgz;type=application/gzip")
fi

printf '%s' "${PUSHBOT_ENV:-}" >"$work/env"
form+=(-F "env=<$work/env")

if [ "${PUSHBOT_REGISTRY_AUTH:-true}" = "true" ]; then
  printf '%s' "$PUSHBOT_REGISTRY_USER" >"$work/registry_user"
  printf '%s' "$PUSHBOT_REGISTRY_TOKEN" >"$work/registry_token"
  form+=(-F "registry_user=<$work/registry_user" -F "registry_token=<$work/registry_token")
fi

oidc=$(curl -sSf -H "Authorization: bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN" \
  "$ACTIONS_ID_TOKEN_REQUEST_URL&audience=${PUSHBOT_AUDIENCE:-pushbot}" | jq -r .value) ||
  die "cannot get an OIDC token from GitHub"
[ -n "$oidc" ] && [ "$oidc" != null ] || die "GitHub returned an empty OIDC token"
# Keep the token off the command line (and out of ps).
printf 'Authorization: Bearer %s\n' "$oidc" >"$work/headers"

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
