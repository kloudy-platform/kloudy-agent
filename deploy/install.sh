#!/usr/bin/env bash
#
# Install or upgrade the Kloudy metrics agent.
#
# Idempotent: running it again with the same version is a no-op, and running it
# with a new one is an upgrade. Every value comes from the environment, so no
# caller-supplied string is ever interpolated into this source.
#
# Required:
#   KLOUDY_AGENT_VERSION   release to install, e.g. 1.4.0
#   KLOUDY_AGENT_SHA256    expected checksum of the binary for this architecture
#   KLOUDY_ENDPOINT        platform ingest URL, https only
#   KLOUDY_TOKEN           this server's token
#
# Optional:
#   KLOUDY_AGENT_BASE_URL  where releases are downloaded from
#
# The checksum is required and has no fallback. This binary runs on every
# customer server, which makes it the most attractive supply-chain target in the
# product, and a download nobody verified is an unauthenticated code path onto
# all of them. Kloudy passes the checksum over the SSH channel it has already
# authenticated, so an attacker would have to compromise both that channel and
# the download to be believed.
#
set -euo pipefail

BASE_URL="${KLOUDY_AGENT_BASE_URL:-https://github.com/kloudy-platform/kloudy-agent/releases/download}"

BINARY=/usr/local/bin/kloudy-agent
UNIT=/etc/systemd/system/kloudy-agent.service
CONFIG_DIR=/etc/kloudy
CONFIG="${CONFIG_DIR}/agent.env"
SERVICE_USER=kloudy

log() { printf '==> %s\n' "$1"; }
die() { printf 'error: %s\n' "$1" >&2; exit 1; }

require() {
    local name="$1"
    [ -n "${!name:-}" ] || die "${name} is required"
}

require KLOUDY_AGENT_VERSION
require KLOUDY_AGENT_SHA256
require KLOUDY_ENDPOINT
require KLOUDY_TOKEN

case "${KLOUDY_ENDPOINT}" in
    https://*) ;;
    *) die "KLOUDY_ENDPOINT must be https" ;;
esac

# The token is written into a systemd EnvironmentFile, which has no quoting
# rules worth relying on. Refusing anything outside this alphabet is cheaper
# than reasoning about how systemd would parse the rest.
case "${KLOUDY_TOKEN}" in
    *[!A-Za-z0-9_.-]*) die "KLOUDY_TOKEN contains characters that are not allowed" ;;
esac

case "$(uname -m)" in
    x86_64|amd64) ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    *) die "unsupported architecture: $(uname -m)" ;;
esac

[ "$(id -u)" -eq 0 ] || die "must run as root"

command -v systemctl >/dev/null 2>&1 || die "systemd is required"
command -v sha256sum >/dev/null 2>&1 || die "sha256sum is required"

download() {
    local url="$1" dest="$2"
    if command -v curl >/dev/null 2>&1; then
        curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 -o "${dest}" "${url}"
    elif command -v wget >/dev/null 2>&1; then
        wget --quiet --https-only -O "${dest}" "${url}"
    else
        die "curl or wget is required"
    fi
}

# --- Already at the requested version? -----------------------------------------
if [ -x "${BINARY}" ] && [ "$("${BINARY}" --version 2>/dev/null || true)" = "${KLOUDY_AGENT_VERSION}" ]; then
    log "agent ${KLOUDY_AGENT_VERSION} already installed, skipping download"
    INSTALL_BINARY=false
else
    INSTALL_BINARY=true
fi

# --- Service account -----------------------------------------------------------
# No shell and no home: this account exists to own a process and a spool
# directory, and must not be a way onto the machine.
if ! id -u "${SERVICE_USER}" >/dev/null 2>&1; then
    log "creating ${SERVICE_USER} service account"
    useradd --system --no-create-home --shell /usr/sbin/nologin "${SERVICE_USER}"
fi

# --- Binary --------------------------------------------------------------------
if [ "${INSTALL_BINARY}" = true ]; then
    NAME="kloudy-agent-linux-${ARCH}"
    URL="${BASE_URL}/v${KLOUDY_AGENT_VERSION}/${NAME}"

    TMP="$(mktemp -d)"
    trap 'rm -rf "${TMP}"' EXIT

    log "downloading ${NAME} ${KLOUDY_AGENT_VERSION}"
    download "${URL}" "${TMP}/agent"

    # Verified before it is made executable and before it is moved anywhere it
    # could be run from. Nothing downloaded executes until this passes.
    log "verifying checksum"
    printf '%s  %s\n' "${KLOUDY_AGENT_SHA256}" "${TMP}/agent" > "${TMP}/sum"
    sha256sum --check --status "${TMP}/sum" \
        || die "checksum mismatch, refusing to install (expected ${KLOUDY_AGENT_SHA256})"

    chmod 0755 "${TMP}/agent"
    # Rename is atomic, and the running process keeps its own inode alive, so an
    # upgrade never leaves a half-written binary on disk.
    mv "${TMP}/agent" "${BINARY}"
    log "installed ${BINARY}"
fi

# --- Configuration -------------------------------------------------------------
# Written every run so a rotated token or a moved endpoint takes effect on
# upgrade. Created with restrictive permissions before the token is written to
# it, never widened afterwards.
log "writing ${CONFIG}"
install -d -m 0755 "${CONFIG_DIR}"
touch "${CONFIG}"
chmod 0600 "${CONFIG}"
chown "${SERVICE_USER}:${SERVICE_USER}" "${CONFIG}"
{
    printf 'KLOUDY_ENDPOINT=%s\n' "${KLOUDY_ENDPOINT}"
    printf 'KLOUDY_TOKEN=%s\n' "${KLOUDY_TOKEN}"
} > "${CONFIG}"

# --- Service -------------------------------------------------------------------
log "installing systemd unit"
cat > "${UNIT}" <<'UNITFILE'
[Unit]
Description=Kloudy metrics agent
Documentation=https://github.com/kloudy-platform/kloudy-agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=kloudy
Group=kloudy

# Holds KLOUDY_ENDPOINT and KLOUDY_TOKEN. Kept out of the command line so the
# token never appears in ps output, where any local user could read it.
EnvironmentFile=/etc/kloudy/agent.env
ExecStart=/usr/local/bin/kloudy-agent

Restart=always
RestartSec=5s

# /var/lib/kloudy-agent, created and owned by the service user. It is the only
# path the agent can write to, and it holds the spool of undelivered windows.
StateDirectory=kloudy-agent
StateDirectoryMode=0700

# The agent reads /proc, calls statfs, and makes outbound HTTPS requests. It
# needs nothing else, so it is granted nothing else. This block is also the
# answer to the reasonable question of what this binary can do on the machine.
NoNewPrivileges=true
CapabilityBoundingSet=
AmbientCapabilities=
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectKernelLogs=true
ProtectControlGroups=true
ProtectClock=true
ProtectHostname=true
ProtectProc=default
RestrictNamespaces=true
RestrictRealtime=true
RestrictSUIDSGID=true
LockPersonality=true
MemoryDenyWriteExecute=true
SystemCallFilter=@system-service
SystemCallErrorNumber=EPERM

# AF_UNIX is kept alongside the IP families because name resolution on some
# distributions goes through a local socket. Removing it would harden the unit
# slightly and break DNS on those machines.
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6

[Install]
WantedBy=multi-user.target
UNITFILE

systemctl daemon-reload
systemctl enable kloudy-agent >/dev/null 2>&1 || true
systemctl restart kloudy-agent

# --- Verify --------------------------------------------------------------------
# A unit that fails on start would otherwise leave this script reporting success
# while nothing is running.
sleep 2
if ! systemctl is-active --quiet kloudy-agent; then
    printf 'error: agent failed to start\n' >&2
    journalctl -u kloudy-agent --no-pager --lines 20 >&2 || true
    exit 1
fi

log "kloudy-agent ${KLOUDY_AGENT_VERSION} running"
