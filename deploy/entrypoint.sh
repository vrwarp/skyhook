#!/bin/sh
# Runs the server under Xvfb, which is how Chromium runs headful in a container.
# That is the default: headless Chromium says so in its own user agent and in
# navigator.webdriver, and a browser being used as a browser has no reason to.
# SKYHOOK_HEADFUL=0 opts out.
set -eu

if [ "${SKYHOOK_HEADFUL:-1}" = "1" ]; then
  echo "starting Xvfb for headful Chromium"
  Xvfb :99 -screen 0 1600x1200x24 -nolisten tcp &
  export DISPLAY=:99
  export SKYHOOK_HEADLESS=0
  # Give the display a moment before Chromium reaches for it.
  sleep 1
fi

# Chromium sandboxes itself with user namespaces. Plenty of container runtimes
# refuse those, and then Chromium does not start at all — which surfaces as the
# server timing out on a devtools port and tells the operator nothing.
#
# So: try it. If a throwaway Chromium cannot start, run without its internal
# sandbox and say so. The container and the dedicated uid are still doing their
# job; this only gives up the layer the runtime already took away.
if [ -z "${SKYHOOK_CHROME_ARGS:-}" ]; then
  probe_dir="$(mktemp -d)"
  if ! "${SKYHOOK_CHROME:-/usr/bin/chromium}" \
        --headless=new --no-first-run --disable-gpu \
        --user-data-dir="$probe_dir" --dump-dom about:blank >/dev/null 2>&1; then
    echo "chromium cannot use its own sandbox here (user namespaces blocked?);"
    echo "continuing with --no-sandbox: the container and its uid are the boundary"
    export SKYHOOK_CHROME_ARGS="--no-sandbox --disable-dev-shm-usage"
  fi
  rm -rf "$probe_dir"
fi

exec /usr/local/bin/skyhookd "$@"
