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

ENV_FILE="$ROOT/.env"

# 1. Ensure .env exists (copy from .env.example)
if [ ! -f "$ENV_FILE" ] && [ -f "$ROOT/.env.example" ]; then
  cp "$ROOT/.env.example" "$ENV_FILE"
  echo "No .env found; created it from .env.example"
fi

# 2. If no token, offer to generate one (skipped when piped/CI)
if [ -f "$ENV_FILE" ] && ! grep -qE '^AUTH_TOKENS=[^[:space:]]' "$ENV_FILE"; then
  if [ -t 0 ]; then
    echo "No token found in .env"
    read -r -p "Generate one now? [Y/n] " ANS
    case "$ANS" in
      n|N|no|NO) echo "  Skipped; running in bridge mode (clients send their own token)." ;;
      *) "$ROOT/gen-freebuff-token.sh" --append --env "$ENV_FILE" ;;
    esac
  else
    echo "No token in AUTH_TOKENS - running in bridge mode (clients send their own token)."
  fi
fi

# 3. Banner with the real listen address
ADDR="127.0.0.1:3457"
if [ -f "$ENV_FILE" ]; then
  LINE=$(grep -E '^LISTEN_ADDR=' "$ENV_FILE" | head -1 | cut -d= -f2- | tr -d '[:space:]')
  [ -n "$LINE" ] && ADDR="$LINE"
fi
echo ""
echo "Starting freebuff-proxy from $ROOT"
echo "  OpenAI API:  http://$ADDR/v1"
echo "  Health:      http://$ADDR/healthz"
echo "  Stop:        Ctrl+C"
echo ""

exec "$ROOT/freebuff-proxy"
