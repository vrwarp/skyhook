#!/bin/sh
# Runs the server, optionally under Xvfb for headful operation. Sites with
# aggressive bot detection do better headful; everything else is fine headless.
set -eu

if [ "${SKYHOOK_HEADFUL:-0}" = "1" ]; then
  echo "starting Xvfb for headful Chromium"
  Xvfb :99 -screen 0 1600x1200x24 -nolisten tcp &
  export DISPLAY=:99
  export SKYHOOK_HEADLESS=0
  # Give the display a moment before Chromium reaches for it.
  sleep 1
fi

exec /usr/local/bin/skyhookd "$@"
