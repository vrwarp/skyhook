#!/usr/bin/env bash
# One command to try Skyhook on your own machine.
#
# Builds the client, starts the landside server in loopback demo mode, and
# prints the link to open. Demo mode is plain HTTP on 127.0.0.1: no TLS, no
# QUIC, no certificate to trust first — which is what lets Chrome register the
# service worker and offer the install prompt. It refuses to bind anything but
# loopback, and it stops on its own after ten minutes.
#
#   scripts/demo.sh              # ten minutes, then it cleans up
#   scripts/demo.sh 30m          # or as long as you like
#   SKYHOOK_DEMO_HOME=https://news.ycombinator.com/ scripts/demo.sh
set -euo pipefail
cd "$(dirname "$0")/.."

DURATION="${1:-10m}"
DATA_DIR="${SKYHOOK_DATA_DIR:-$PWD/.devdata/demo}"
HOME_URL="${SKYHOOK_DEMO_HOME:-https://news.ycombinator.com/}"

if ! command -v go >/dev/null; then echo "demo: go is not installed" >&2; exit 1; fi
if ! command -v npm >/dev/null; then echo "demo: npm is not installed" >&2; exit 1; fi

echo "==> building the client"
if [ ! -d client/node_modules ]; then
  ( cd client && npm ci ) >/dev/null
fi
( cd client && npm run build ) >/dev/null

echo "==> starting the landside server (Chromium runs here, not in your browser)"
mkdir -p "$DATA_DIR"
export SKYHOOK_DATA_DIR="$DATA_DIR"
export SKYHOOK_WEB_ROOT="$PWD/client/dist"
export SKYHOOK_TOKEN="${SKYHOOK_TOKEN:-demo-token}"
export SKYHOOK_LOG_LEVEL="${SKYHOOK_LOG_LEVEL:-info}"

cat >"$DATA_DIR/demo.json" <<JSON
{ "homeUrl": "$HOME_URL", "headless": true }
JSON

go run ./cmd/skyhookd -demo -demo-for "$DURATION" -config "$DATA_DIR/demo.json" 2>&1 |
  while IFS= read -r line; do
    printf '%s\n' "$line"
    case "$line" in
      *'open this link to start'*)
        # The token rides in the fragment, so it never reaches a server log.
        echo
        echo "  Open the URL above in Chrome. Press + for a tab, type a URL, and"
        echo "  watch a page you are not fetching. The HUD shows the transport,"
        echo "  the queue depth and the bytes actually spent."
        echo
        ;;
    esac
  done
