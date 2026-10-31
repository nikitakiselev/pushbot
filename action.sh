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
# environments.<name> replaces the top-level one. The environment checks repeat
# the daemon's, because a job with the wrong environment also lacks that
# environment's secrets and would fail on them with a misleading message. Other
# checks are left to the daemon; only the types used here are checked.
yq -o=json . "$PUSHBOT_CONFIG" >"$work/pushbot.json" || die "cannot parse $PUSHBOT_CONFIG"
jq --arg env "$environment" --arg cfg "$PUSHBOT_CONFIG" --arg name '^[A-Za-z_][A-Za-z0-9_]*$' '
  def fail(msg): {error: "\($cfg): \(msg)"};
  (. // {}) as $m
  | if ($m | type) != "object" then fail("expected a map")
    elif ($m.environments // {} | type) != "object" then fail("environments must be a map")
    # An empty map is an error the daemon reports.
    elif ($m.environments // {} | length) > 0 and $env == "" then
      {error: "\($cfg) lists environments, but the job has no environment: set environment: in the workflow"}
    else
      ($m.environments // {} | to_entries
        | map(select((.key | ascii_downcase) == ($env | ascii_downcase))) | first) as $found
      | if ($m.environments // {} | length) > 0 and $found == null then
          {error: "environment \"\($env)\" is not listed in environments of \($cfg) (\($m.environments | keys | sort_by(ascii_downcase) | join(", ")))"}
        else
          ($found.value // {}) as $o
          | if ($o | type) != "object" then fail("environments.\($found.key) must be a map")
            elif ($m.env // {} | type) != "object" or ($o.env // {} | type) != "object" then fail("env must be a map")
            else
              {files: ($o.files // $m.files // []), env_file: ($o.env.file // $m.env.file // ""),
               secrets: ($o.env.secrets // $m.env.secrets // [])}
              | if (.files | type) != "array" then fail("files must be a list")
                elif (.secrets | type) != "array" then fail("env.secrets must be a list")
                elif (.env_file | type) != "string" then fail("env.file must be a path")
                else
                  # Checked here too: an invalid name is never set in GitHub either.
                  (.secrets | map(select(type != "string" or (test($name) | not)) | tostring) | first) as $bad
                  | if $bad then fail("env.secrets: invalid name \($bad | tojson): must match \($name)")
                    # A files item is a path or a "src: dest" map, whose key is the path.
                    else .files |= map(if type == "object" then keys_unsorted[0] else tostring end)
                    end
                end
            end
        end
    end
' "$work/pushbot.json" >"$work/manifest.json" || die "cannot read $PUSHBOT_CONFIG"
error=$(jq -r '.error // empty' "$work/manifest.json")
[ -z "$error" ] || die "$error"

files=()
while IFS= read -r f; do
  if [ -n "$f" ]; then files+=("$f"); fi
done < <(jq -r '.files[]' "$work/manifest.json")
env_file=$(jq -r .env_file "$work/manifest.json")
if [ -n "$env_file" ]; then
  [ -f "$env_file" ] || die "env.file $env_file not found"
  # Pack it once, also when it is listed in files or inside a listed directory.
  covered=false
  for f in "${files[@]}"; do
    f=${f%/}
    if [ "$env_file" = "$f" ] || [[ "$env_file" == "$f"/* ]]; then covered=true; fi
  done
  if [ "$covered" = false ]; then files+=("$env_file"); fi
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

# The job log gets the daemon's own lines (==>, PUSHBOT:) and the output of a
# failed step; the verbose input or a re-run with debug logging shows it all.
verbose=false
if [ "${PUSHBOT_VERBOSE:-false}" = true ] || [ "${RUNNER_DEBUG:-}" = 1 ]; then verbose=true; fi

# quiet holds back the output of the current step in a file and prints it only
# before "PUSHBOT: failed", or at the end when the log has no result line (a
# rejected request, a lost connection).
quiet() {
  local line done=false
  : >"$work/step"
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in
    "==> ["[0-9]*/[0-9]*"] "*) : >"$work/step" ;;
    "==> "*) ;;
    "PUSHBOT: success"*) done=true ;;
    "PUSHBOT: failed"*)
      cat "$work/step"
      done=true
      ;;
    *)
      printf '%s\n' "$line" >>"$work/step"
      continue
      ;;
    esac
    printf '%s\n' "$line"
  done
  if [ "$done" = false ]; then cat "$work/step"; fi
}

set +e
curl -sS -N -H @"$work/headers" -H 'Expect:' -D "$work/response-headers" \
  "${form[@]}" "${PUSHBOT_URL%/}/v1/deploy" | tee "$work/out" |
  if [ "$verbose" = true ]; then cat; else quiet; fi
rc=${PIPESTATUS[0]}
set -e
[ "$rc" -eq 0 ] || die "request to $PUSHBOT_URL failed (curl exit code $rc)"

# The last status line wins (a proxy may send "100 Continue" first).
code=$(grep -E '^HTTP/' "$work/response-headers" | tail -n1 | awk '{print $2}')
if [ "$code" != 200 ]; then
  # Daemons before env.secrets reject the field before reading the manifest.
  if grep -q '^pushbot: unknown field "secrets"' "$work/out"; then
    die "the server is too old for env.secrets: run sudo pushbot upgrade on it"
  fi
  die "the server rejected the deploy (HTTP $code)"
fi

last=$(tail -n1 "$work/out")
case "$last" in
"PUSHBOT: success"*) exit 0 ;;
"PUSHBOT: failed"*) die "${last#PUSHBOT: }" ;;
*) die "the deploy log ended unexpectedly (connection lost?); check the server log" ;;
esac
