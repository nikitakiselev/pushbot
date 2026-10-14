#!/bin/sh
# Install or upgrade pushbot as a systemd service.
#
#   curl -fsSL https://raw.githubusercontent.com/nikitakiselev/pushbot/main/install.sh \
#     | sudo PUSHBOT_ALLOW='your-github-login/*' sh
#
# PUSHBOT_ALLOW   allow pattern for a new /etc/pushbot/config.yml (ignored if the file exists)
# PUSHBOT_VERSION release tag to install, default: latest
set -eu

REPO=nikitakiselev/pushbot
VERSION=${PUSHBOT_VERSION:-latest}

die() { echo "install.sh: $*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run as root (sudo)"
command -v systemctl >/dev/null || die "systemd is required"
command -v docker >/dev/null || die "docker is required"
getent group docker >/dev/null || die "the docker group does not exist"

case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) die "unsupported architecture $(uname -m)" ;;
esac
if [ "$VERSION" = latest ]; then
  url="https://github.com/$REPO/releases/latest/download/pushbot_linux_$arch.tar.gz"
else
  url="https://github.com/$REPO/releases/download/$VERSION/pushbot_linux_$arch.tar.gz"
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
echo "Downloading $url"
curl -fsSL "$url" | tar -xz -C "$tmp"
install -m 0755 "$tmp/pushbot" /usr/local/bin/pushbot
install -m 0644 "$tmp/pushbot.service" /etc/systemd/system/pushbot.service

if ! id pushbot >/dev/null 2>&1; then
  useradd --system --home-dir /var/lib/pushbot --shell /usr/sbin/nologin pushbot
fi
usermod -aG docker pushbot
install -d -o pushbot -g pushbot -m 0750 /var/lib/pushbot /srv/apps
install -d -m 0755 /etc/pushbot

start=yes
if [ ! -f /etc/pushbot/config.yml ]; then
  allow=${PUSHBOT_ALLOW:-}
  if [ -z "$allow" ]; then
    allow='YOUR-GITHUB-LOGIN/*'
    start=no
  fi
  cat >/etc/pushbot/config.yml <<EOF
# pushbot configuration. Restart after changes: systemctl restart pushbot
listen: 127.0.0.1:9800
projects_root: /srv/apps
allow:
  - $allow
# Prometheus metrics at /metrics on a separate address (disabled by default):
# metrics_listen: 127.0.0.1:9801
EOF
  echo "Created /etc/pushbot/config.yml"
fi

systemctl daemon-reload
systemctl enable pushbot >/dev/null
if [ "$start" = yes ]; then
  systemctl restart pushbot
  echo "pushbot $(/usr/local/bin/pushbot version) is running: journalctl -u pushbot -f"
else
  echo "Edit the allow list in /etc/pushbot/config.yml, then run: systemctl start pushbot"
fi

cat <<'EOF'

Expose it with Caddy (Caddyfile):

  deploy.example.com {
      reverse_proxy 127.0.0.1:9800 {
          flush_interval -1
      }
  }
EOF
