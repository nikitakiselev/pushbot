#!/bin/sh
# Install or upgrade pushbot as a systemd service.
#
#   curl -fsSL https://raw.githubusercontent.com/nikitakiselev/pushbot/main/install.sh | sudo sh
#
# Then create the config and start the service: sudo pushbot init
#
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
install -d -o pushbot -g pushbot -m 0750 /var/lib/pushbot
install -d -m 0755 /etc/pushbot
systemctl daemon-reload

if [ -f /etc/pushbot/config.yml ]; then
  systemctl enable pushbot >/dev/null
  systemctl restart pushbot
  echo "pushbot $(/usr/local/bin/pushbot version) is running: journalctl -u pushbot -f"
  exit 0
fi

# install.sh comes from main, the release may predate pushbot init.
if ! /usr/local/bin/pushbot init --help >/dev/null 2>&1; then
  die "pushbot $(/usr/local/bin/pushbot version) is too old to create its config (no pushbot init), install a newer release"
fi
# PUSHBOT_ALLOW keeps the install command of earlier versions working.
if [ -n "${PUSHBOT_ALLOW:-}" ]; then
  /usr/local/bin/pushbot init --allow "$PUSHBOT_ALLOW"
  exit
fi
cat <<EOF
pushbot $(/usr/local/bin/pushbot version) is installed. Create the config and start it:

  sudo pushbot init
EOF
