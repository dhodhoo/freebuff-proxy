#!/usr/bin/env bash
# start-proxy.sh - Launch freebuff-proxy from the extracted folder.
# Right-click this folder -> "Open in Terminal" -> ./start-proxy.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

if [ ! -f "$ROOT/freebuff-proxy" ]; then
  echo "freebuff-proxy not found next to this script." >&2
  exit 1
fi

if [ ! -f "$ROOT/.env" ] && [ -f "$ROOT/.env.example" ]; then
  cp "$ROOT/.env.example" "$ROOT/.env"
  echo "No .env found; created it from .env.example"
fi

echo "Starting freebuff-proxy from $ROOT"
exec "$ROOT/freebuff-proxy"
