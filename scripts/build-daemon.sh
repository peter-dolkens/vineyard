#!/bin/sh
# Cross-compiles vineyardd for every fleet platform into bin/. Static, stripped, no cgo.
set -e
cd "$(dirname "$0")/../daemon"
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
