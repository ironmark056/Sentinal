#!/usr/bin/env bash
# Build sentinel + sentinel-server for every supported platform.
# Output: dist/<pkg>-<os>-<arch>[.exe]
#
# Usage:
#   ./scripts/build-release.sh                  # all platforms, both binaries
#   ./scripts/build-release.sh linux/amd64      # one platform, both binaries
set -euo pipefail

cd "$(dirname "$0")/.."

VERSION="${SENTINEL_VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
LDFLAGS="-s -w -X main.version=${VERSION}"
PACKAGES=(sentinel sentinel-server)

PLATFORMS=(
    linux/amd64
    linux/arm64
    darwin/amd64
    darwin/arm64
    windows/amd64
    windows/arm64
)

if [ $# -gt 0 ]; then
    PLATFORMS=("$@")
fi

mkdir -p dist
echo "Building sentinel + sentinel-server ${VERSION}"
echo

for p in "${PLATFORMS[@]}"; do
    os="${p%/*}"
    arch="${p#*/}"
    for pkg in "${PACKAGES[@]}"; do
        out="dist/${pkg}-${os}-${arch}"
        [ "$os" = "windows" ] && out="${out}.exe"

        echo "  → $out"
        GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 \
            go build -trimpath -ldflags "${LDFLAGS}" -o "$out" "./cmd/${pkg}"
    done
done

echo
echo "Done. Outputs:"
ls -lh dist/sentinel-* dist/sentinel-server-*
