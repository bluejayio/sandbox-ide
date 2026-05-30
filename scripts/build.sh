#!/usr/bin/env bash
# Builds the scheduler and host-agent binaries from their respective modules.
# Run from anywhere — the script locates the repo root via its own path.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN_DIR="$REPO_ROOT/bin"
mkdir -p "$BIN_DIR"

echo "==> building scheduler"
(cd "$REPO_ROOT/scheduler" && go build -o "$BIN_DIR/scheduler" ./cmd/scheduler)

echo "==> building host-agent"
(cd "$REPO_ROOT/host-agent" && go build -o "$BIN_DIR/agent" ./cmd/agent)

echo "==> building vm-agent (Linux x86_64 — runs inside the guest)"
(cd "$REPO_ROOT/vm-agent" && GOOS=linux GOARCH=amd64 go build -o "$BIN_DIR/vm-agent" ./cmd/vm-agent)

echo "==> done"
ls -lh "$BIN_DIR"
