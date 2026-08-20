#!/usr/bin/env python3
"""Capture a list of URLs as diagnostic bundles and triage the lot.

The parity corpus measures pages nobody minds committing; this measures
everything else. Given a file of URLs (one per line, # comments), each page
is fetched with curl, made locally servable, opened through a running
skyhookd by `skyhookctl capture`, and the resulting bundle triaged. The
output directory ends up holding one bundle and one triage report per URL,
plus a summary — a conformance set for the bundle tooling made of markup
nobody wrote to be measurable.

The bundles contain the pages themselves. Do not commit them; commit what
they taught (gaps.json entries, triage table fixes) and the summary.

Fetching is curl's job rather than the landside browser's on purpose: it
works in environments where the browser's egress is filtered (the proxy
this was written behind resets Chromium's TLS specifically), and a local
mirror makes the capture repeatable. The rewrite strips scripts — corpus
rules apply: a page that mutates under measurement measures the weather —
plus <base> and meta refresh; stylesheets and small images are localized;
other subresource references are absolutized so both halves miss them
identically.

Usage:
  scripts/bundle_sweep.py -urls urls.txt -out /tmp/sweep \
      -pairing ~/.skyhook/pairing.json [-skyhookctl bin/skyhookctl]
      [-settle 8] [-keep-scripts]

Needs: a running skyhookd, its pairing file, curl, and a free ephemeral
port for the local mirror server.
"""

import argparse
import html
import http.server
import json
import pathlib
import re
import socket
import subprocess
import sys
import threading
from urllib.parse import urljoin, urlparse

UA = ("Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 "
      "(KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36")
MAX_PAGE = 4 * 1024 * 1024
MAX_CSS = 2 * 1024 * 1024
MAX_ASSET = 512 * 1024
MAX_IMAGES = 20


def fetch(url, cap):
    try:
        p = subprocess.run(
            ["curl", "-sS", "-L", "--max-time", "30", "--max-filesize", str(cap),
             "-A", UA, "--compressed", "-w", "\n%{content_type}\t%{http_code}", url],
            capture_output=True, timeout=40)
    except subprocess.TimeoutExpired:
        return None, "timeout"
    raw = p.stdout
    nl = raw.rfind(b"\n")
    if nl < 0:
        return None, "no response"
    body, tail = raw[:nl], raw[nl + 1:].decode("utf-8", "replace")
    ctype, _, code = tail.partition("\t")
    if code.strip() != "200":
        return None, f"HTTP {code.strip() or '?'}"
    return body, ctype.split(";")[0].strip()


def localize(url, outdir, keep_scripts):
    out = pathlib.Path(outdir)
    out.mkdir(parents=True, exist_ok=True)
    body, ctype = fetch(url, MAX_PAGE)
    if body is None:
        return {"ok": False, "why": ctype}
    if "html" not in ctype:
        return {"ok": False, "why": f"content-type {ctype}"}
    doc = body.decode("utf-8", "replace")
    base = url
    m = re.search(r'<base[^>]+href="([^"]+)"', doc)
    if m:
        base = urljoin(url, html.unescape(m.group(1)))

    if not keep_scripts:
        doc = re.sub(r"<script\b[^>]*>.*?</script>", "", doc, flags=re.S | re.I)
        doc = re.sub(r"<script\b[^>]*/>", "", doc, flags=re.I)
    doc = re.sub(r'<meta[^>]+http-equiv=["\']?refresh[^>]*>', "", doc, flags=re.I)
    doc = re.sub(r"<base\b[^>]*>", "", doc, flags=re.I)

    state = {"n": 0, "misses": []}

    def grab(ref, cap, ext):
        absu = urljoin(base, html.unescape(ref))
        if not absu.startswith("http"):
            return None
        data, why = fetch(absu, cap)
        if data is None:
            state["misses"].append(f"{absu[:90]}: {why}")
            return None
        state["n"] += 1
        name = f"a{state['n']:02d}{ext}"
        (out / name).write_bytes(data)
        return name

    def css_repl(m):
        tag, href = m.group(0), m.group(1)
        if "stylesheet" not in tag.lower():
            return tag
        name = grab(href, MAX_CSS, ".css")
        if not name:
            return tag
        raw = (out / name).read_text("utf-8", errors="replace")
        cssbase = urljoin(base, html.unescape(href))

        def urlfix(mm):
            ref = mm.group(1).strip("'\" ")
            if ref.startswith(("data:", "#")):
                return mm.group(0)
            return 'url("%s")' % urljoin(cssbase, ref)

        (out / name).write_text(re.sub(r"url\(([^)]*)\)", urlfix, raw))
        return tag.replace(href, name)

    doc = re.sub(r'<link\b[^>]*href="([^"]+)"[^>]*>', css_repl, doc, flags=re.I)

    seen = {}

    def img_repl(m):
        ref = m.group(1)
        if ref.startswith("data:"):
            return m.group(0)
        if ref in seen:
            new = seen[ref]
        elif len(seen) >= MAX_IMAGES:
            new = urljoin(base, html.unescape(ref))
        else:
            ext = pathlib.Path(urlparse(urljoin(base, ref)).path).suffix[:5] or ".img"
            name = grab(ref, MAX_ASSET, ext)
            new = name if name else urljoin(base, html.unescape(ref))
            seen[ref] = new
        return m.group(0).replace(ref, new)

    doc = re.sub(r'<img\b[^>]*?src="([^"]+)"', img_repl, doc, flags=re.I)
    doc = re.sub(r'\s+srcset="[^"]*"', "", doc)

    (out / "index.html").write_text(doc)
    return {"ok": True, "assets": state["n"], "misses": state["misses"][:6]}


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("-urls", required=True, help="file of URLs, one per line")
    ap.add_argument("-out", required=True, help="output directory")
    ap.add_argument("-pairing", required=True, help="skyhookd pairing file")
    ap.add_argument("-skyhookctl", default="bin/skyhookctl")
    ap.add_argument("-settle", type=int, default=8, help="seconds to let each page settle")
    ap.add_argument("-keep-scripts", action="store_true",
                    help="leave page scripts in place (pages may mutate under capture)")
    args = ap.parse_args()

    urls = [l.strip() for l in open(args.urls)
            if l.strip() and not l.strip().startswith("#")]
    out = pathlib.Path(args.out)
    mirror = out / "mirror"
    bundles = out / "bundles"
    bundles.mkdir(parents=True, exist_ok=True)

    # Serve the localized pages, quietly: the sweep's own lines are the log.
    class Handler(http.server.SimpleHTTPRequestHandler):
        def __init__(self, *a, **kw):
            super().__init__(*a, directory=str(mirror), **kw)

        def log_message(self, *a):
            pass

    with socket.socket() as probe:
        probe.bind(("127.0.0.1", 0))
        port = probe.getsockname()[1]
    httpd = http.server.ThreadingHTTPServer(("127.0.0.1", port), Handler)
    threading.Thread(target=httpd.serve_forever, daemon=True).start()

    summary = []
    for i, url in enumerate(urls):
        tag = f"{i:02d}"
        row = {"i": tag, "url": url}
        loc = localize(url, mirror / tag, args.keep_scripts)
        row.update(loc)
        if loc["ok"]:
            p = subprocess.run(
                [args.skyhookctl, "capture", "-pairing", args.pairing,
                 "-url", f"http://127.0.0.1:{port}/{tag}/",
                 "-note", f"bundle sweep {tag}: {url}",
                 "-settle", f"{args.settle}s"],
                capture_output=True, text=True)
            m = re.search(r"written on the server: (\S+\.zip)", p.stdout)
            if m:
                zip_path = pathlib.Path(m.group(1))
                dest = bundles / f"{tag}.zip"
                zip_path.rename(dest)
                t = subprocess.run(
                    [args.skyhookctl, "bundle", "triage", "-json", str(dest)],
                    capture_output=True, text=True)
                (bundles / f"{tag}.triage.json").write_text(t.stdout)
                try:
                    row["verdict"] = json.loads(t.stdout)["verdict"]
                except (json.JSONDecodeError, KeyError):
                    row["verdict"] = f"triage exit {t.returncode}"
            else:
                row["verdict"] = "capture failed: " + (p.stdout or p.stderr).strip()[-120:]
        print(f"{tag} {row.get('verdict', row.get('why'))}  {url[:80]}", flush=True)
        summary.append(row)

    httpd.shutdown()
    (out / "summary.json").write_text(json.dumps(summary, indent=1))
    verdicts = [r.get("verdict") for r in summary]
    print(f"\n{verdicts.count('clean')} clean, {verdicts.count('diverged')} diverged, "
          f"{sum(1 for r in summary if not r.get('ok'))} unfetchable — {out}/summary.json")
    return 0


if __name__ == "__main__":
    sys.exit(main())
