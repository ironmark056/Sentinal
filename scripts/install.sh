#!/usr/bin/env bash
# Install the latest sentinel release for the current platform.
#
# Usage:
#   curl -sSL https://raw.githubusercontent.com/your-org/sentinel/main/scripts/install.sh | sh
#
# Env vars:
#   SENTINEL_VERSION   pin a specific version (e.g. v0.1.0); default: latest
#   SENTINEL_BIN_DIR   install prefix; default: /usr/local/bin if writable else $HOME/.local/bin
#   SENTINEL_REPO      GitHub repo; default: your-org/sentinel
set -euo pipefail

REPO="${SENTINEL_REPO:-your-org/sentinel}"
VERSION="${SENTINEL_VERSION:-latest}"

# Detect OS / arch.
uname_os="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$uname_os" in
    linux)  os="linux" ;;
    darwin) os="darwin" ;;
    *) echo "Unsupported OS: $uname_os" >&2; exit 1 ;;
esac

uname_arch="$(uname -m)"
case "$uname_arch" in
    x86_64|amd64)  arch="amd64" ;;
    aarch64|arm64) arch="arm64" ;;
    *) echo "Unsupported architecture: $uname_arch" >&2; exit 1 ;;
esac

# Resolve install dir.
if [ -n "${SENTINEL_BIN_DIR:-}" ]; then
    bindir="$SENTINEL_BIN_DIR"
elif [ -w /usr/local/bin ]; then
    bindir="/usr/local/bin"
else
    bindir="$HOME/.local/bin"
fi
mkdir -p "$bindir"

# Resolve download URL.
if [ "$VERSION" = "latest" ]; then
    url="https://github.com/$REPO/releases/latest/download/sentinel-$os-$arch"
else
    url="https://github.com/$REPO/releases/download/$VERSION/sentinel-$os-$arch"
fi

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

echo "Downloading $url"
if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$url" -o "$tmp"
elif command -v wget >/dev/null 2>&1; then
    wget -qO "$tmp" "$url"
else
    echo "Need curl or wget on PATH." >&2
    exit 1
fi

chmod +x "$tmp"
target="$bindir/sentinel"
mv "$tmp" "$target"

echo "Installed: $target"
echo
echo "Next:"
echo "  $target init       # writes ~/.sentinel/sentinel.yaml"
echo "  $target dashboard  # opens http://127.0.0.1:7842"
