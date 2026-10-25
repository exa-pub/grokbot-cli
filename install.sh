#!/bin/sh
# Installs the latest grokbot-cli release.
#
#   curl -fsSL https://raw.githubusercontent.com/exa-pub/grokbot-cli/main/install.sh | sh
#
# GROKBOT_VERSION    release tag to install, e.g. v0.1.0 (default: latest)
# GROKBOT_INSTALL_DIR  target directory (default: /usr/local/bin if writable
#                    or sudo is available, otherwise ~/.local/bin)
set -eu

REPO="exa-pub/grokbot-cli"
BINARY="grokbot-cli"

if [ -t 2 ]; then
  BOLD='\033[1m' RED='\033[0;31m' GREEN='\033[0;32m' YELLOW='\033[0;33m' RESET='\033[0m'
else
  BOLD='' RED='' GREEN='' YELLOW='' RESET=''
fi

info() { printf "${BOLD}${GREEN}==> %s${RESET}\n" "$*" >&2; }
warn() { printf "${BOLD}${YELLOW}==> %s${RESET}\n" "$*" >&2; }
fail() { printf "${BOLD}${RED}Error: %s${RESET}\n" "$*" >&2; exit 1; }

if command -v curl >/dev/null 2>&1; then
  download() { curl -fsSL -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
  download() { wget -q -O "$2" "$1"; }
else
  fail "curl or wget is required"
fi

[ "$(uname -s)" = Linux ] || fail "unsupported OS $(uname -s); only Linux is supported"

case "$(uname -m)" in
  x86_64 | amd64) ARCH=amd64 ;;
  aarch64 | arm64) ARCH=arm64 ;;
  *) fail "unsupported architecture $(uname -m)" ;;
esac

# Prompts go to stderr and read from the terminal, so they work with
# `curl | sh` and are not captured by $(...).
install_dir() {
  if [ -n "${GROKBOT_INSTALL_DIR:-}" ]; then
    echo "$GROKBOT_INSTALL_DIR"
  elif [ "$(id -u)" = 0 ] || [ -w /usr/local/bin ]; then
    echo /usr/local/bin
  elif command -v sudo >/dev/null 2>&1 && sudo -n true 2>/dev/null; then
    echo /usr/local/bin
  elif command -v sudo >/dev/null 2>&1 && [ -r /dev/tty ]; then
    warn "/usr/local/bin is not writable."
    printf "    [1] Use sudo to install to /usr/local/bin\n" >&2
    printf "    [2] Install to ~/.local/bin\n" >&2
    printf "    Choice [1]: " >&2
    # Without a usable terminal sudo could not ask for a password either.
    if ! { read -r choice </dev/tty; } 2>/dev/null; then
      echo "$HOME/.local/bin"
    elif [ "$choice" = 2 ]; then
      echo "$HOME/.local/bin"
    else
      echo /usr/local/bin
    fi
  else
    echo "$HOME/.local/bin"
  fi
}

INSTALL_DIR="$(install_dir)"

# sudo is needed if the nearest existing parent of INSTALL_DIR is not writable.
SUDO=
if [ "$(id -u)" != 0 ]; then
  dir="$INSTALL_DIR"
  while [ ! -d "$dir" ]; do dir="$(dirname "$dir")"; done
  [ -w "$dir" ] || SUDO=sudo
fi

ARCHIVE="${BINARY}-linux-${ARCH}.tar.gz"
if [ -n "${GROKBOT_VERSION:-}" ]; then
  BASE="https://github.com/${REPO}/releases/download/${GROKBOT_VERSION}"
else
  BASE="https://github.com/${REPO}/releases/latest/download"
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

info "Downloading ${BASE}/${ARCHIVE}"
download "${BASE}/${ARCHIVE}" "${TMP}/${ARCHIVE}" || fail "download failed; check GROKBOT_VERSION"

if download "${BASE}/checksums.txt" "${TMP}/checksums.txt" 2>/dev/null; then
  expected="$(awk -v f="$ARCHIVE" '$2 == f { print $1 }' "${TMP}/checksums.txt")"
  [ -n "$expected" ] || fail "${ARCHIVE} is missing from checksums.txt"
  if command -v sha256sum >/dev/null 2>&1; then
    actual="$(sha256sum "${TMP}/${ARCHIVE}" | awk '{ print $1 }')"
    [ "$actual" = "$expected" ] || fail "checksum mismatch: expected ${expected}, got ${actual}"
    info "Checksum verified"
  else
    warn "sha256sum not found; skipping checksum verification"
  fi
else
  warn "checksums.txt not found; skipping checksum verification"
fi

tar -xzf "${TMP}/${ARCHIVE}" -C "$TMP"
$SUDO mkdir -p "$INSTALL_DIR"
$SUDO install -m 755 "${TMP}/${BINARY}-linux-${ARCH}" "${INSTALL_DIR}/${BINARY}"

info "Installed $("${INSTALL_DIR}/${BINARY}" --version) to ${INSTALL_DIR}/${BINARY}"

case ":${PATH}:" in
  *":${INSTALL_DIR}:"*) ;;
  *)
    warn "${INSTALL_DIR} is not in your PATH. Add it, e.g. in ~/.bashrc or ~/.zshrc:"
    echo "    export PATH=\"${INSTALL_DIR}:\$PATH\"" >&2
    ;;
esac
