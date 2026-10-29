#!/usr/bin/env bash
# Cross-compiles the Windows release into dist/ (works from Linux/WSL or Git Bash).
set -euo pipefail
cd "$(dirname "$0")"
VERSION="${VERSION:-0.1.0}"
mkdir -p dist
export GOOS=windows GOARCH=amd64 CGO_ENABLED=0
go build -trimpath -ldflags "-s -w" -o cmd/agbridge/shim.exe ./cmd/ls-shim
go build -trimpath -ldflags "-s -w -H windowsgui -X main.version=${VERSION}" -o dist/agbridge.exe ./cmd/agbridge
ls -l dist/agbridge.exe cmd/agbridge/shim.exe
