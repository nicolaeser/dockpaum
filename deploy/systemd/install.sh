#!/bin/sh
set -eu

unit_url=https://raw.githubusercontent.com/nicolaeser/dockpaum/main/deploy/systemd/dockpaum.service
here=$(cd "$(dirname "$0")" && pwd)

die() {
    echo "install.sh: $*" >&2
    exit 1
}

[ "$(id -u)" -eq 0 ] || die "run as root"
command -v systemctl >/dev/null 2>&1 || die "systemd is required"
command -v docker >/dev/null 2>&1 || die "docker is required"

case "${1:-install}" in
install) ;;
uninstall)
    systemctl disable --now dockpaum 2>/dev/null || true
    rm -f /etc/systemd/system/dockpaum.service /usr/local/bin/dockpaum
    systemctl daemon-reload
    echo "dockpaum removed; /etc/dockpaum and /var/lib/dockpaum are kept"
    exit 0
    ;;
*) die "usage: install.sh [install|uninstall]" ;;
esac

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

if [ -n "${DOCKPAUM_BINARY:-}" ]; then
    cp "$DOCKPAUM_BINARY" "$tmp/dockpaum"
else
    docker pull -q ghcr.io/nicolaeser/dockpaum:latest >/dev/null
    cid=$(docker create ghcr.io/nicolaeser/dockpaum:latest)
    docker cp "$cid:/dockpaum" "$tmp/dockpaum"
    docker rm "$cid" >/dev/null
fi

if [ -f "$here/dockpaum.service" ]; then
    cp "$here/dockpaum.service" "$tmp/dockpaum.service"
else
    curl -fsSL "$unit_url" -o "$tmp/dockpaum.service"
fi

id dockpaum >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin --groups docker dockpaum
install -m 0755 "$tmp/dockpaum" /usr/local/bin/dockpaum
install -d -m 0750 -o dockpaum -g dockpaum /etc/dockpaum
install -m 0644 "$tmp/dockpaum.service" /etc/systemd/system/dockpaum.service
systemctl daemon-reload
systemctl enable dockpaum >/dev/null
systemctl restart dockpaum
echo "installed $(/usr/local/bin/dockpaum --version)"
