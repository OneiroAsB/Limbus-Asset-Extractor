#!/usr/bin/env bash
# Cross-compile into dist/. Requires Go 1.22+; no external dependencies.
set -euo pipefail
if [[ $# -gt 1 || ( $# -eq 1 && "${1:-}" != "--release" ) ]]; then
  echo "Usage: $0 [--release]" >&2
  exit 2
fi
cd "$(dirname "$0")"
mkdir -p dist
build() { # GOOS GOARCH output_file
  CGO_ENABLED=0 GOOS="$1" GOARCH="$2" go build -trimpath -ldflags "-s -w" -o "dist/$3" ./cmd/limbus-extractor
  echo "dist/$3"
}
build linux   amd64 limbus-extractor-linux-amd64
build linux   arm64 limbus-extractor-linux-arm64
build windows amd64 limbus-extractor-windows-amd64.exe
if [[ "${1:-}" == "--release" ]]; then
  go run ./tools/package-release
fi
