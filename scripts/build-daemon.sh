#!/bin/sh
# Cross-compiles vineyardd for every fleet platform into bin/. Static, stripped, no cgo.
# The web app (vineyardd web) is bundled first so every binary embeds it; without node_modules the
# binaries still build and `vineyardd web` says the app is missing.
set -e
cd "$(dirname "$0")/.."
if [ -d node_modules/esbuild ]; then
  node esbuild.mjs --web --production
else
  echo "node_modules missing: building vineyardd without the web app (run npm install first)"
fi
cd daemon
VERSION="${VERSION:-$(git -C .. describe --tags --always 2>/dev/null || echo dev)}"
OUT="../bin"
mkdir -p "$OUT"
for target in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 linux/386 windows/amd64 windows/arm64 windows/386; do
  os="${target%/*}"; arch="${target#*/}"
  ext=""; [ "$os" = windows ] && ext=".exe"
  echo "building $os/$arch"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags="-s -w -X main.Version=$VERSION" -o "$OUT/vineyardd-$os-$arch$ext" ./cmd/vineyardd
done
ls -la "$OUT"
