#!/usr/bin/env bash
# Runs a server and prints the pairing file, for local development.
set -euo pipefail
cd "$(dirname "$0")/.."

DATA_DIR="${SKYHOOK_DATA_DIR:-$PWD/.devdata}"
mkdir -p "$DATA_DIR"

export SKYHOOK_DATA_DIR="$DATA_DIR"
export SKYHOOK_LOG_LEVEL="${SKYHOOK_LOG_LEVEL:-debug}"
export SKYHOOK_TOKEN="${SKYHOOK_TOKEN:-dev-token}"

go run ./cmd/skyhookd "$@" &
SERVER=$!
trap 'kill $SERVER 2>/dev/null || true' EXIT

# Wait for the pairing file, then show it: the client needs the token and the
# certificate fingerprint from it.
for _ in $(seq 1 50); do
  if [ -f "$DATA_DIR/pairing.json" ]; then
    echo "--- pairing ---"
    cat "$DATA_DIR/pairing.json"
    break
  fi
  sleep 0.2
done

wait $SERVER
