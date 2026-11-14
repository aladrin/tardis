#!/bin/sh
set -eu

export CGO_ENABLED=0
TARGET_OS="${GOOS:-$(go env GOOS)}"
TARGET_ARCH="${GOARCH:-$(go env GOARCH)}"

case "$TARGET_OS" in
	openbsd|linux)
		;;
	*)
		echo "Unsupported target GOOS=${TARGET_OS} (supported: openbsd, linux)" >&2
		exit 1
		;;
esac

export GOOS="$TARGET_OS"
export GOARCH="$TARGET_ARCH"

echo "[1/2] Resolving Go module dependencies (go mod tidy)..."
go mod tidy

echo "[2/2] Building kerr (${GOOS}/${GOARCH}, CGO_ENABLED=0)..."
go build -trimpath -ldflags="-s -w" -o kerr .
chmod 0755 kerr

echo "Build complete: ./kerr (${GOOS}/${GOARCH})"
