#!/bin/sh
# AIHub VPS installer. Run as root on Debian/Ubuntu (other systemd Linux
# with Docker already installed works too). Safe to run again: it updates
# binaries and config files and never overwrites .env secrets.
#
#   sh install.sh --admin-email you@example.com (--domain aihub.example.com | --ip 203.0.113.7)
#                 [--version backend-vX.Y.Z | --binaries DIR] [--registration invite|open]
#                 [--https-port 443] [--enable-ufw] [--ssh-port 22]
#
# What it does:
#   * Docker Engine + compose plugin (Docker's apt repository) if missing;
#   * AIHub release binaries, checked against the release's SHA256SUMS
#     (or prebuilt ones from --binaries DIR with a SHA256SUMS file);
#   * OpenFlux node-install.sh at a pinned commit, checked against its
#     SHA-256 (the same pin as OpenFlux's provision/pin.go), into
#     /usr/local/lib/aihub;
#   * aihub-nodectl as a root systemd service on a unix socket that only
#     group "aihub" (the backend container) may use, no sudo, no Docker
#     socket for the backend;
#   * /opt/aihub/.env with a fresh AIHUB_MASTER_KEY (only if missing);
#   * Caddy (Let's Encrypt TLS) + the backend via docker compose;
#   * firewall rules (80, 443; channel ports are opened by node-install.sh);
#   * a daily database backup to /var/backups/aihub.
set -eu
umask 022

REPO="ItzChelove4ik/AIHub"
# OpenFlux node-install.sh, pinned like third_party/openflux/provision/pin.go
# (CI checks that these stay equal).
NODE_SCRIPT_REPO="meepo161/OpenFlux"
NODE_SCRIPT_COMMIT="544a514c819b4a4a75a6b2c0b302540cb8d2024c"
NODE_SCRIPT_SHA256="84afc2b39fe84e08305b33267800013c154622a3e2265520292e4f6d3d05e2b6"

APP_DIR=/opt/aihub
LIB_DIR=/usr/local/lib/aihub
BACKUP_DIR=/var/backups/aihub
SRC_DIR=$(cd "$(dirname "$0")" && pwd)

DOMAIN="" IP="" ADMIN_EMAIL="" AIHUB_VERSION="" BIN_DIR="" REGISTRATION="invite" ENABLE_UFW=0 SSH_PORT=22 HTTPS_PORT=443

say() { printf '\033[1m==> %s\033[0m\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

while [ $# -gt 0 ]; do
    case "$1" in
        --domain) DOMAIN=$2; shift 2 ;;
        --ip) IP=$2; shift 2 ;;
        --admin-email) ADMIN_EMAIL=$2; shift 2 ;;
        --version) AIHUB_VERSION=$2; shift 2 ;;
        --binaries) BIN_DIR=$2; shift 2 ;;
        --registration) REGISTRATION=$2; shift 2 ;;
        --enable-ufw) ENABLE_UFW=1; shift ;;
        --ssh-port) SSH_PORT=$2; shift 2 ;;
        --https-port) HTTPS_PORT=$2; shift 2 ;;
        -h|--help) sed -n '2,25p' "$0"; exit 0 ;;
        *) die "unknown option $1 (see --help)" ;;
    esac
done

[ "$(id -u)" = 0 ] || die "run as root"
[ -n "$ADMIN_EMAIL" ] || die "--admin-email is required (only this account may register without an invite)"
case "$REGISTRATION" in invite|open) ;; *) die "--registration: invite or open" ;; esac
case "$SSH_PORT" in ''|*[!0-9]*) die "--ssh-port must be a number" ;; esac
case "$HTTPS_PORT" in ''|*[!0-9]*) die "--https-port must be a number" ;; esac
if [ -z "$DOMAIN" ]; then
    [ -n "$IP" ] || die "give --domain or --ip (then <ip>.sslip.io is used)"
    printf '%s' "$IP" | grep -Eq '^([0-9]{1,3}\.){3}[0-9]{1,3}$' || die "--ip must be an IPv4 address"
    DOMAIN="$(printf '%s' "$IP" | tr . -).sslip.io"
fi
printf '%s' "$DOMAIN" | grep -Eq '^[A-Za-z0-9.-]+$' || die "bad domain"
[ -d /run/systemd/system ] || die "systemd is required"

# Ports 80 (Let's Encrypt) and HTTPS_PORT must be free, or already ours.
port_owner() { ss -Hltnp "sport = :$1" 2>/dev/null | head -n 1; }
for port in 80 "$HTTPS_PORT"; do
    owner=$(port_owner "$port")
    if [ -n "$owner" ] && ! docker ps --format '{{.Names}} {{.Ports}}' 2>/dev/null | grep -q "^aihub-caddy-1 .*:$port->"; then
        die "port $port is taken ($owner); pick another with --https-port (80 must stay free for Let's Encrypt)"
    fi
done
if [ "$HTTPS_PORT" = 443 ]; then PUBLIC_URL="https://$DOMAIN"; else PUBLIC_URL="https://$DOMAIN:$HTTPS_PORT"; fi

case "$(uname -m)" in
    x86_64|amd64) ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    *) die "unsupported architecture $(uname -m)" ;;
esac

# ---- packages and Docker ---------------------------------------------------

# Read os-release in a subshell: it defines VERSION, ID and more.
# shellcheck source=/dev/null
OS_ID=$(. /etc/os-release 2>/dev/null && echo "${ID:-}")
# shellcheck source=/dev/null
OS_CODENAME=$(. /etc/os-release 2>/dev/null && echo "${VERSION_CODENAME:-}")
if ! have curl || ! have openssl || ! have sha256sum; then
    have apt-get || die "install curl, openssl and coreutils first"
    say "Installing curl, openssl"
    apt-get update -qq
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq curl ca-certificates openssl coreutils >/dev/null
fi

if have docker && ! docker compose version >/dev/null 2>&1; then
    # Docker is already there and may run other containers (VPNs, bots):
    # never replace or restart the engine, only add the compose plugin
    # that matches the installed packages.
    have apt-get || die "install the docker compose plugin first"
    say "Adding the docker compose plugin (the Docker engine stays as it is)"
    apt-get update -qq
    if dpkg -s docker-ce >/dev/null 2>&1; then
        DEBIAN_FRONTEND=noninteractive apt-get install -y -qq docker-compose-plugin >/dev/null
    else
        DEBIAN_FRONTEND=noninteractive apt-get install -y -qq docker-compose-v2 >/dev/null
    fi
    docker compose version >/dev/null 2>&1 || die "could not add docker compose; install it by hand"
fi

if ! have docker; then
    case "$OS_ID" in
        ubuntu|debian) ;;
        *) die "install Docker Engine with the compose plugin first (https://docs.docker.com/engine/install/)" ;;
    esac
    say "Installing Docker Engine from Docker's apt repository"
    apt-get update -qq
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq ca-certificates curl >/dev/null
    install -m 0755 -d /etc/apt/keyrings
    curl -fsSL "https://download.docker.com/linux/$OS_ID/gpg" -o /etc/apt/keyrings/docker.asc
    chmod a+r /etc/apt/keyrings/docker.asc
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/$OS_ID ${OS_CODENAME:?} stable" \
        > /etc/apt/sources.list.d/docker.list
    apt-get update -qq
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin >/dev/null
    systemctl enable --now docker >/dev/null
fi

# ---- AIHub binaries --------------------------------------------------------

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

if [ -n "$BIN_DIR" ]; then
    say "Using prebuilt binaries from $BIN_DIR"
    for f in "aihub-backend-linux-$ARCH" "aihub-nodectl-linux-$ARCH" SHA256SUMS; do
        [ -f "$BIN_DIR/$f" ] || die "$BIN_DIR/$f missing"
        cp "$BIN_DIR/$f" "$work/"
    done
else
    if [ -z "$AIHUB_VERSION" ]; then
        AIHUB_VERSION=$(curl -fsSL "https://api.github.com/repos/$REPO/releases?per_page=30" |
            grep -o '"tag_name": *"backend-v[^"]*"' | head -n 1 | cut -d'"' -f4)
        [ -n "$AIHUB_VERSION" ] || die "no backend-v* release found; pass --version or --binaries"
    fi
    say "Downloading AIHub $AIHUB_VERSION ($ARCH)"
    base="https://github.com/$REPO/releases/download/$AIHUB_VERSION"
    for f in "aihub-backend-linux-$ARCH" "aihub-nodectl-linux-$ARCH" SHA256SUMS; do
        curl -fsSL --retry 3 -o "$work/$f" "$base/$f" || die "download of $f failed"
    done
fi
(cd "$work" && grep -E " \*?aihub-(backend|nodectl)-linux-$ARCH\$" SHA256SUMS | sha256sum -c --quiet -) \
    || die "binary checksum mismatch"

# ---- OpenFlux node-install.sh (pinned) -------------------------------------

say "Fetching OpenFlux node-install.sh at ${NODE_SCRIPT_COMMIT%"${NODE_SCRIPT_COMMIT#???????}"}"
curl -fsSL --retry 3 -o "$work/node-install.sh" \
    "https://raw.githubusercontent.com/$NODE_SCRIPT_REPO/$NODE_SCRIPT_COMMIT/deploy/node-install.sh"
echo "$NODE_SCRIPT_SHA256  $work/node-install.sh" | sha256sum -c --quiet - || die "node-install.sh checksum mismatch"

install -d -m 0755 "$LIB_DIR"
install -m 0755 -o root -g root "$work/node-install.sh" "$LIB_DIR/node-install.sh"

# ---- aihub-nodectl ---------------------------------------------------------

getent group aihub >/dev/null || groupadd --system aihub
AIHUB_GID=$(getent group aihub | cut -d: -f3)

install -m 0755 -o root -g root "$work/aihub-nodectl-linux-$ARCH" /usr/local/bin/aihub-nodectl
install -m 0644 "$SRC_DIR/systemd/aihub-nodectl.service" /etc/systemd/system/aihub-nodectl.service
systemctl daemon-reload
systemctl enable aihub-nodectl >/dev/null 2>&1
systemctl restart aihub-nodectl
sleep 1
[ -S /run/aihub-nodectl/nodectl.sock ] || die "aihub-nodectl did not start (journalctl -u aihub-nodectl)"

# ---- app directory ---------------------------------------------------------

say "Preparing $APP_DIR"
install -d -m 0750 "$APP_DIR" "$APP_DIR/build"
install -d -m 0755 "$APP_DIR/build/data"
install -m 0755 "$work/aihub-backend-linux-$ARCH" "$APP_DIR/build/aihub-backend"
install -m 0644 "$SRC_DIR/Dockerfile.runtime" "$APP_DIR/build/Dockerfile.runtime"
install -m 0644 "$SRC_DIR/docker-compose.yml" "$APP_DIR/docker-compose.yml"
install -m 0644 "$SRC_DIR/Caddyfile" "$APP_DIR/Caddyfile"
install -m 0755 "$SRC_DIR/backup.sh" "$APP_DIR/backup.sh"
install -d -m 0700 -o 65532 -g 65532 "$BACKUP_DIR"

env_file="$APP_DIR/.env"
if [ ! -f "$env_file" ]; then
    say "Creating $env_file with a new master key"
    (
        umask 077
        cat > "$env_file" <<EOF
# AIHub settings (secrets: keep this file private, never commit it).
# Losing AIHUB_MASTER_KEY makes stored provider keys and backups unreadable:
# save a copy somewhere safe and separate from the server.
AIHUB_MASTER_KEY=$(openssl rand -hex 32)
AIHUB_LOG_LEVEL=info
AIHUB_DEFAULT_DAILY_REQUESTS=0
AIHUB_DEFAULT_DAILY_TOKENS=0
TUNNEL_PORT_MIN=20000
TUNNEL_PORT_MAX=20999
TUNNEL_CUPSONLINE=true
EOF
    )
fi
# Settings install.sh owns are rewritten on every run.
set_env() {
    if grep -q "^$1=" "$env_file"; then
        sed -i "s|^$1=.*|$1=$2|" "$env_file"
    else
        printf '%s=%s\n' "$1" "$2" >> "$env_file"
    fi
}
set_env AIHUB_DOMAIN "$DOMAIN"
set_env AIHUB_PUBLIC_URL "$PUBLIC_URL"
set_env AIHUB_HTTPS_PORT "$HTTPS_PORT"
set_env TUNNEL_PUBLIC_HOST "$DOMAIN"
set_env ADMIN_EMAIL "$ADMIN_EMAIL"
set_env AIHUB_REGISTRATION "$REGISTRATION"
set_env AIHUB_GID "$AIHUB_GID"
chmod 0600 "$env_file"
# docker compose reads AIHUB_DOMAIN and AIHUB_GID for interpolation from .env.

# ---- firewall ----------------------------------------------------------------

if have ufw; then
    ufw allow "$SSH_PORT/tcp" >/dev/null
    ufw allow 80/tcp >/dev/null
    ufw allow "$HTTPS_PORT/tcp" >/dev/null
    ufw allow "$HTTPS_PORT/udp" >/dev/null
    if [ "$ENABLE_UFW" = 1 ] && ! ufw status | grep -q "Status: active"; then
        say "Enabling ufw (SSH port $SSH_PORT stays open)"
        ufw --force enable >/dev/null
    fi
elif have firewall-cmd && firewall-cmd --state >/dev/null 2>&1; then
    firewall-cmd --permanent --add-service=http --add-port="$HTTPS_PORT/tcp" --add-port="$HTTPS_PORT/udp" >/dev/null
    firewall-cmd --reload >/dev/null
fi
# Docker publishes 80 and the HTTPS port itself; the channel ports are opened per channel
# by node-install.sh (ufw/firewalld) when it installs them.

# ---- start -------------------------------------------------------------------

say "Starting AIHub (docker compose)"
cd "$APP_DIR"
docker compose pull -q caddy
docker compose build -q
docker compose up -d --remove-orphans

install -m 0644 "$SRC_DIR/systemd/aihub-backup.service" /etc/systemd/system/aihub-backup.service
install -m 0644 "$SRC_DIR/systemd/aihub-backup.timer" /etc/systemd/system/aihub-backup.timer
systemctl daemon-reload
systemctl enable --now aihub-backup.timer >/dev/null 2>&1

say "Waiting for $PUBLIC_URL/readyz (Let's Encrypt can take a minute)"
i=0
until curl -fsS --max-time 10 "$PUBLIC_URL/readyz" >/dev/null 2>&1; do
    i=$((i + 1))
    if [ $i -ge 30 ]; then
        echo "not ready yet; check: cd $APP_DIR && docker compose logs" >&2
        exit 1
    fi
    sleep 5
done
curl -fsS "$PUBLIC_URL/readyz"; echo
say "AIHub is up at $PUBLIC_URL"
echo "Next: register $ADMIN_EMAIL in the app (or POST /v1/auth/register), then add carriers and keys."
echo "Back up $env_file (AIHUB_MASTER_KEY) somewhere safe."
