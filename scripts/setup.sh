#!/usr/bin/env bash
# The first thing to run in a fresh checkout.
#
#   scripts/setup.sh
#
# It asks what your deployment looks like, checks each answer while you are
# still here to fix it, and writes a configuration file. Nothing is written
# until it has shown you the whole plan.
#
# This is a two-line wrapper around `skyhookd -setup`; run that directly if you
# have already built the binary.
set -euo pipefail
cd "$(dirname "$0")/.."

if ! command -v go >/dev/null; then
  echo "setup: go is not installed; see https://go.dev/dl/" >&2
  exit 1
fi

exec go run ./cmd/skyhookd -setup "$@"
