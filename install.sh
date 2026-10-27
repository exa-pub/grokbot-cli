#!/bin/bash
# Installs a grokbot-tunnel release into your home directory.
#
#   curl -fsSL https://raw.githubusercontent.com/exa-pub/grokbot-tunnel/main/install.sh | bash
#   curl -fsSL https://raw.githubusercontent.com/exa-pub/grokbot-tunnel/main/install.sh | bash -s v0.1.0
#
# The optional argument is "latest" (default) or a release tag such as v0.1.0.
#
# GROKBOT_VERSION              release tag to install if no argument is given
# GROKBOT_INSTALL_DIR          target directory (default: ~/.local/bin)
# GROKBOT_SKIP_CHECKSUM=1      install without verifying checksums.txt
# GROKBOT_INSTALL_ALLOW_SUDO=1 allow running under sudo (installs for root)
set -euo pipefail

REPO="exa-pub/grokbot-tunnel"
BINARY="grokbot-tunnel"
OLD_BINARY="grokbot-cli"

if [ -t 2 ]; then
    BOLD=$'\033[1m' RED=$'\033[0;31m' GREEN=$'\033[0;32m' YELLOW=$'\033[0;33m' RESET=$'\033[0m'
else
    BOLD='' RED='' GREEN='' YELLOW='' RESET=''
fi

info() { printf '%s==> %s%s\n' "${BOLD}${GREEN}" "$*" "$RESET" >&2; }
warn() { printf '%s==> %s%s\n' "${BOLD}${YELLOW}" "$*" "$RESET" >&2; }
fail() { printf '%sError: %s%s\n' "${BOLD}${RED}" "$*" "$RESET" >&2; exit 1; }

TARGET="${1:-${GROKBOT_VERSION:-latest}}"
if [[ ! "$TARGET" =~ ^(latest|v?[0-9]+\.[0-9]+\.[0-9]+(-[^[:space:]]+)?)$ ]]; then
    echo "Usage: $0 [latest|VERSION]" >&2
    exit 1
fi
if [ "$TARGET" != latest ] && [[ "$TARGET" != v* ]]; then
    TARGET="v$TARGET"
fi

# Under sudo, $HOME usually points at root's home, so the binary would land in
# /root/.local/bin and the command would be missing from the user's shell.
# Plain root (containers, CI) is fine.
if [ "$(id -u)" -eq 0 ] && [ -n "${SUDO_USER:-}" ] && [ "$SUDO_USER" != root ] &&
    [ -z "${GROKBOT_INSTALL_ALLOW_SUDO:-}" ]; then
    cat >&2 <<EOF
Error: do not run this installer with sudo.

${BINARY} installs into your home directory and does not need root access.
With sudo it would be installed into root's home directory instead of yours.

Re-run the same command without sudo:
    curl -fsSL https://raw.githubusercontent.com/${REPO}/main/install.sh | bash

To intentionally install for the root user, set GROKBOT_INSTALL_ALLOW_SUDO=1:
    curl -fsSL https://raw.githubusercontent.com/${REPO}/main/install.sh | sudo GROKBOT_INSTALL_ALLOW_SUDO=1 bash
EOF
    exit 1
fi

if command -v curl >/dev/null 2>&1; then
    DOWNLOADER=curl
elif command -v wget >/dev/null 2>&1; then
    DOWNLOADER=wget
else
    fail "curl or wget is required"
fi

download_file() {
    local url="$1" output="$2"
    if [ "$DOWNLOADER" = curl ]; then
        curl -fsSL -o "$output" "$url"
    else
        wget -q -O "$output" "$url"
    fi
}

# Prints the URL that $1 finally redirects to.
resolve_redirect() {
    if [ "$DOWNLOADER" = curl ]; then
        curl -fsSL -o /dev/null -w '%{url_effective}' "$1"
    else
        wget -q -S --spider "$1" 2>&1 | awk '$1 == "Location:" { l = $2 } END { print l }'
    fi
}

case "$(uname -s)" in
    Linux) ;;
    Darwin) fail "macOS is not supported; only Linux is supported" ;;
    MINGW* | MSYS* | CYGWIN*) fail "Windows is not supported; only Linux is supported" ;;
    *) fail "unsupported operating system $(uname -s); only Linux is supported" ;;
esac

case "$(uname -m)" in
    x86_64 | amd64) ARCH=amd64 ;;
    aarch64 | arm64) ARCH=arm64 ;;
    *) fail "unsupported architecture $(uname -m)" ;;
esac

if [ -n "${GROKBOT_INSTALL_DIR:-}" ]; then
    INSTALL_DIR="$GROKBOT_INSTALL_DIR"
else
    [ -n "${HOME:-}" ] || fail "HOME is not set; set GROKBOT_INSTALL_DIR instead"
    INSTALL_DIR="$HOME/.local/bin"
fi

if [ "$TARGET" = latest ]; then
    url="$(resolve_redirect "https://github.com/${REPO}/releases/latest" || true)"
    VERSION="${url##*/}"
    # Reject anything that is not a tag, e.g. when there are no releases yet.
    [[ "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+ ]] ||
        fail "could not determine the latest release of ${REPO}"
else
    VERSION="$TARGET"
fi

ARCHIVE="${BINARY}-linux-${ARCH}.tar.gz"
BASE="https://github.com/${REPO}/releases/download/${VERSION}"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

info "Downloading ${BINARY} ${VERSION} (linux/${ARCH})"
download_file "${BASE}/${ARCHIVE}" "${TMP}/${ARCHIVE}" ||
    fail "download of ${BASE}/${ARCHIVE} failed; check that release ${VERSION} exists"

if [ "${GROKBOT_SKIP_CHECKSUM:-}" = 1 ]; then
    warn "GROKBOT_SKIP_CHECKSUM=1; skipping checksum verification"
else
    command -v sha256sum >/dev/null 2>&1 ||
        fail "sha256sum is required to verify the download (set GROKBOT_SKIP_CHECKSUM=1 to skip)"
    download_file "${BASE}/checksums.txt" "${TMP}/checksums.txt" ||
        fail "could not download checksums.txt (set GROKBOT_SKIP_CHECKSUM=1 to skip)"
    expected="$(awk -v f="$ARCHIVE" '$2 == f { print $1 }' "${TMP}/checksums.txt")"
    [[ "$expected" =~ ^[a-f0-9]{64}$ ]] || fail "${ARCHIVE} is missing from checksums.txt"
    actual="$(sha256sum "${TMP}/${ARCHIVE}" | cut -d' ' -f1)"
    [ "$actual" = "$expected" ] || fail "checksum mismatch: expected ${expected}, got ${actual}"
    info "Checksum verified"
fi

tar -xzf "${TMP}/${ARCHIVE}" -C "$TMP"
mkdir -p "$INSTALL_DIR" || fail "cannot create ${INSTALL_DIR}"
install -m 755 "${TMP}/${BINARY}-linux-${ARCH}" "${INSTALL_DIR}/${BINARY}" ||
    fail "cannot write ${INSTALL_DIR}/${BINARY}"

code=0
installed="$("${INSTALL_DIR}/${BINARY}" --version)" || code=$?
if [ "$code" -ne 0 ]; then
    # 128+N means the binary was killed by signal N before printing anything.
    if [ "$code" -ge 128 ]; then
        fail "${INSTALL_DIR}/${BINARY} was killed by signal $((code - 128)) while starting"
    fi
    fail "${INSTALL_DIR}/${BINARY} was installed but does not run on this system"
fi
info "Installed ${installed} to ${INSTALL_DIR}/${BINARY}"

# A running service keeps the old binary until it restarts. Root has a system
# service, other users a service of their user manager.
sctl() {
    if [ "$(id -u)" -eq 0 ]; then systemctl "$@"; else systemctl --user "$@"; fi
}
if command -v systemctl >/dev/null 2>&1 &&
    sctl is-active --quiet grokbot-tunnel 2>/dev/null &&
    sctl show --property=ExecStart --value grokbot-tunnel 2>/dev/null |
    grep -qF "path=${INSTALL_DIR}/${BINARY} "; then
    info "Restarting the ${BINARY} service"
    sctl restart --no-block grokbot-tunnel || warn "could not restart the service; restart it yourself"
fi

# The project used to be called grokbot-cli; an old copy may still be on PATH.
if old="$(command -v "$OLD_BINARY" 2>/dev/null)"; then
    warn "Found the old ${OLD_BINARY} binary at ${old}; it is no longer updated. Remove it with:"
    echo "    rm ${old}" >&2
fi

case ":${PATH}:" in
    *":${INSTALL_DIR}:"*) ;;
    *)
        warn "${INSTALL_DIR} is not in your PATH. Add it, e.g. in ~/.bashrc or ~/.zshrc:"
        echo "    export PATH=\"${INSTALL_DIR}:\$PATH\"" >&2
        ;;
esac

echo "" >&2
echo "✅ Installation complete! Run '${BINARY} login' to get started." >&2
echo "   Then run '${BINARY} connect', or '${BINARY} install --permission always' to run it as a service." >&2
