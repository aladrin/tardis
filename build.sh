#!/bin/sh
set -eu

export CGO_ENABLED=0
export GOOS="${GOOS:-openbsd}"
if [ -n "${GOARCH:-}" ]; then
	export GOARCH
fi

TARGET_ARCH="${GOARCH:-$(go env GOARCH)}"

echo "[1/2] Resolving Go module dependencies (go mod tidy)..."
go mod tidy

echo "[2/2] Building tardis (${GOOS}/${TARGET_ARCH}, CGO_ENABLED=0)..."
go build -trimpath -ldflags="-s -w" -o tardis .
chmod 0755 tardis

echo "Build complete: ./tardis (${GOOS}/${TARGET_ARCH})"
