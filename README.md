# Skyhook

A split-architecture browser for links that are slow, lossy and far away —
in-flight wifi, satellite, anywhere the round trip is measured in seconds.

A real Chromium runs **landside** on a VPS with a fast path to the open
internet. It executes all page JavaScript and performs all origin fanout. The
**plane-side** client executes zero page JavaScript and makes zero requests to
the internet: it receives a compressed, incrementally-updated mirror of the
rendered document over a single QUIC connection, and sends back semantic input
events. Every interaction costs at most one round trip over the bad link; most
cost none.

Fidelity is traded for usability, deliberately. See [docs/DESIGN.md](docs/DESIGN.md)
for the full product requirements and technical design, and
[docs/IMPLEMENTATION.md](docs/IMPLEMENTATION.md) for what is built, what is
partial, and where the implementation diverges from the design (with reasons).
[docs/PRIOR-ART.md](docs/PRIOR-ART.md) is what rrweb, Blimp, OBML and the rest
knew that this did not, and which of their lessons changed the code.

```
PLANE SIDE (a browser)                    LANDSIDE (VPS)
┌────────────────────────┐                ┌──────────────────────────────┐
│ Skyhook PWA            │                │ skyhookd (Go)                │
│  ├ chrome UI + HUD     │   one QUIC /   │  ├ real Chromium via CDP     │
│  ├ sandboxed mirror    │◄──WebTransport─┤  ├ injected mirror agent     │
│  │   frames (no JS)    │   connection   │  ├ used-CSS extraction       │
│  ├ network worker      │                │  ├ image transcoder          │
│  ├ service worker      │                │  ├ session manager (12h TTL) │
│  └ IndexedDB + caches  │                │  ├ per-app adapters          │
└────────────────────────┘                │  └ serves the client app     │
                                          └──────────────────────────────┘
```

The client is a Chrome-targeted progressive web app, served by the server
itself. Mirrored pages render inside iframes carrying `sandbox="allow-same-origin"`
and no `allow-scripts`, so the browser itself guarantees that page JavaScript
never runs plane-side.

On a phone it draws a different chrome for the same browser: one row instead of
two, the tab strip as a list behind a count, the panels and menus as sheets, and
the HUD as a coloured dot that spells itself out when the answer is *offline* or
*slow*. It also tells the server it is a phone, which is what gets a site's own
mobile layout rather than its desktop one squeezed into 393 pixels.

## Try it in one command

```sh
scripts/demo.sh          # builds the client, runs both halves, stops after 10 minutes
```

It prints a link to open in Chrome. Press **+** for a tab, type a URL, and watch
a page arrive that your browser never fetched; the HUD shows the transport, the
queue depth and the bytes actually spent. Middle-click a link for a background
tab, and right-click anywhere for Skyhook's own menu — the browser's would act
on the sandboxed frame rather than on the page it is showing.

A page here is minutes wide, so the two ways of calling one off are worth
knowing. The reload button becomes **stop** while a page is on its way (so does
**Escape**), and stopping keeps the tab and whatever of it had arrived. Closing the tab is the
harder version: the tab's landside page is torn down, the call it was waiting on
is cancelled, and every byte it still had queued is dropped rather than spent —
which on this link is the difference between a link you get back now and one you
get back in two minutes. A page loading in a background tab cannot starve the
one you are reading either way; the scheduler rotates between tabs and puts the
one on screen first.

Press **★** (or Ctrl/⌘+D) to keep a page. Saved pages are one of the two ways of
getting somewhere here that do not spend the link: the list is on your device,
so it opens instantly, searches for free and stays readable through an outage —
it is what a new tab shows, what Ctrl/⌘+B opens beside the page, and what the
address bar completes from as you type.

The other is where you have already been. The address bar finishes an address
out of the pages this device has visited, offering the ones you typed yourself
ahead of the ones you merely landed on — three keystrokes instead of forty, and
no round trip until Enter. A row you would rather not be offered again has an
**✕** on it (Shift+Delete from the keyboard), and *Clear history* in the
right-click menu empties the lot; both say what they did and both offer the way
back. That list never leaves the device, and no server is asked about it.

The demo runs in loopback mode — plain HTTP on `127.0.0.1`, no TLS, no QUIC.
That is deliberate rather than lazy: Chrome will not register a service worker
behind a self-signed certificate, so a local demo over TLS could not install or
start offline, while `127.0.0.1` is a secure origin whatever the scheme. The
server refuses to bind anything but loopback in this mode.

## Quick start

### Set it up by answering questions

```sh
scripts/setup.sh          # or: skyhookd -setup
```

It asks what your deployment looks like — which browser, how the plane side
reaches it, which certificate — and **checks each answer while you are still
there to fix it**: that the browser you want to attach to is actually listening,
that the name resolves, that the port is free, that your DNS hook really
publishes a record. Then it writes a configuration file and prints the pairing
link. Nothing is written until it has shown you the whole plan.

It is the fastest way to a working server, and the fastest way to find out which
of the four deployments you actually want, since each one says what it costs
before you pick it.

### Or by hand

```sh
# With Docker
docker compose -f deploy/docker-compose.yml up -d
docker compose -f deploy/docker-compose.yml exec skyhook \
  skyhookctl pairing -file /data/pairing.json

# Or from source
go build -o skyhookd ./cmd/skyhookd
SKYHOOK_DATA_DIR=~/.skyhook ./skyhookd
```

Run from a checkout, the server finds `client/dist` by itself, so a built client
needs no configuration at all.

By default the server launches Chromium itself and owns it. To drive a browser
that is already running instead, point the server at its DevTools endpoint:

```sh
google-chrome --remote-debugging-port=9222 --remote-allow-origins='*'   # yours
SKYHOOK_CHROME_ATTACH=http://127.0.0.1:9222 ./skyhookd
```

That browser's profile is shared, so whatever it is logged into is what
mirrored pages see. Because it is your browser and not the server's, Skyhook
keeps to itself: its tabs go in a window of its own, it never attaches to,
navigates, closes or lists a tab it did not open, and stopping the server
closes that window and leaves the browser running. Keep the debugging port on
loopback — it is unauthenticated control of the whole browser.
[docs/OPERATIONS.md](docs/OPERATIONS.md#driving-a-browser-that-is-already-running)
has the rest.

On first run the server generates a pairing token and a short-lived,
self-signed ECDSA certificate, and writes `pairing.json` into its data
directory. That file is what the client needs: host, port, token, and the
certificate fingerprint it will pin.

If the box has a name, give it a real certificate instead — the server gets one
from Let's Encrypt and keeps it renewed:

```sh
SKYHOOK_ACME_DOMAINS=skyhook.example.com SKYHOOK_ACME_EMAIL=you@example.com \
  docker compose -f deploy/docker-compose.acme.yml up -d
```

That is the deployment to want, because it is the only one that keeps both
halves of what makes the client work. TLS terminates here, so WebTransport
survives; the certificate is one Chrome already trusts, so the app installs and
starts with no network — which a self-signed certificate never allows, however
firmly the client pins it. Renewal needs no restart and no re-pairing.

If ports 80 and 443 are not available — a machine behind a NAT, a link that
filters them, or something else already listening — answer in DNS instead, which
needs no inbound port at all:

```json
"acme": { "enabled": true, "agreeTos": true, "challenge": "dns-01",
          "dns": { "command": ["/usr/local/bin/skyhook-dns-hook"] } }
```

The hook is a command you supply, because every DNS provider has its own API and
none of them belong in here; `deploy/acme-dns-hook.example.sh` is a working one
for Cloudflare. [docs/OPERATIONS.md](docs/OPERATIONS.md#a-certificate-of-its-own)
has the DNS record, the three challenges and how to rehearse against staging.

Behind a reverse proxy, tell the server the address the proxy answers on —
everything it hands the client is built from it, and it cannot infer it:

```sh
SKYHOOK_PUBLIC_URL=https://skyhook.example.com \
  docker compose -f deploy/docker-compose.proxy.yml up -d
```

That deployment trades WebTransport for the WebSocket fallback, because no HTTP
proxy forwards HTTP/3, and trades the certificate pin for the proxy's real
certificate. [docs/OPERATIONS.md](docs/OPERATIONS.md#behind-a-reverse-proxy)
has the details, with worked nginx and Caddy configurations in `deploy/`.

### Plane side (the laptop)

The server serves the client. Build it once, point the server at it, and open
the pairing link the server logs on startup:

```sh
cd client
npm ci
npm run build      # -> client/dist
```

Running from a checkout, that is all: the server finds `client/dist` in the
repository it came out of and serves it. Elsewhere, set
`"webRoot": "/path/to/client/dist"` (the container image builds and ships it
already). Either way, open the link from the server's log:

```
level=INFO msg="pair the client by opening this link once" url="https://vps:4434/#token=…"
```

The token rides in the URL fragment, which browsers never send to a server. The
app stores it, strips it from the address bar, connects, and offers itself for
install. After that it starts from its own cache with no network at all — which
is the point, since you will be opening it at 35,000 feet. You can also paste
`pairing.json` into the app's pairing dialog if you would rather not use a link.

Because it runs from that cache, a deploy landside changes nothing in a browser
that already holds a copy, and no reload can see past it. So both halves say
which build they are on every connection: right-click anywhere for *Skyhook
versions…* to see the app's build, the server's version and the build the server
is serving. When they differ the entry becomes *Update Skyhook…*, and pressing
it fetches the new app and reloads onto it. Nothing updates by itself — it is a
download over a link that charges seconds for one.

### Diagnostics from anywhere

`skyhookctl` speaks the real protocol, so it is both a debugging tool and the
end-to-end probe CI uses:

```sh
skyhookctl probe -pairing ~/.skyhook/pairing.json \
  -url https://news.ycombinator.com/ -expect "comments" -json
```

## Testing against the link that matters

Every milestone is measured against an emulated 1.2 s RTT / 250 kbps / 2% loss
link, not against a LAN:

```sh
make test-slow            # shape eight lanes, run the suite eight-wide, unshape
```

which is this, and `LANES=4 make test-slow` on a smaller box:

```sh
sudo scripts/netem.sh lanes 21123 8 1200 250 2   # shape only the Skyhook ports
SKYHOOK_E2E=1 SKYHOOK_SLOW_LINK=1 SKYHOOK_TEST_PORTS=21123-21130 \
  go test ./test -parallel 8 -v
sudo scripts/netem.sh down
```

Eight *lanes*, not one shaped port shared eight ways. A netem qdisc's rate is a
budget for everything queued into it, so tests sharing a port would divide the
250 kbit between them and finish no sooner than they would have one at a time.
Each lane is a netem qdisc of its own, and a test leases one for its lifetime —
which is also what bounds how many run at once, so `-parallel` and the lane
count want to match.

Running them together is worth it because of what the suite spends its time on.
A test here is almost entirely waiting on a 1.2 s round trip, using neither the
link nor the CPU while it does, and tests that are waiting can wait together.
Every test already builds its own Chromium, fixture servers, manager and
temporary directories, so the shaped port was the only thing they ever shared.

That the lanes are independent rather than one link divided up is measurable:
against the same tests run alone, the median test took 1.01x as long at four
lanes and 1.02x at eight. The CI step went from 37m06s to 9m46s to 5m34s.

What is left is the longest single test, at around 98s: no number of lanes puts
the suite below that, so shortening it is the next thing to do rather than
adding lanes.

`sudo scripts/netem.sh outage 60` drops the link entirely for a minute, which
is how the reconnect-and-resync path is exercised. It replaces the whole qdisc,
lanes included, so it is a thing to do to one session rather than underneath a
suite.

### Reading the output

A test that fails prints its own server log, in full and down to DEBUG,
underneath its own name — and a test that passes prints nothing. That is the
only way the log is readable with eight tests running at once: written straight
to stderr, eight interleaved streams with nothing to say which test a line came
from is not a log, it is a haystack.

While the suite runs, stderr carries WARN and worse only, tagged with the test
it came from. To watch one test in detail, ask for it:

```sh
SKYHOOK_TEST_LOG=debug go test ./test -run TestPWAReadsAnAggregator -v
```

`debug`, `info`, `warn` (the default) and `error` all work. It sets what reaches
stderr live; the failure dump is always the full DEBUG record either way.

## When the mirror looks wrong

The split renderer's awkward failure mode is that both halves look fine on their
own: Chromium rendered the page and the agent serialised it, the patcher applied
every frame and reported no error, and the difference between them is on a
device you cannot reach, in a tab that has since moved on.

So Skyhook can take a **capture**: both halves frozen at the same instant and
zipped up landside, in `<dataDir>/captures`. Right-click a mirrored page and
choose *Report a rendering problem…*, or press Ctrl/⌘+Shift+D.

Nothing is captured unless somebody asks. While chasing a bug, `captureOnDivergence`
lets the server take one by itself the moment its integrity check catches the
two sides holding different documents — which is the moment nobody is ever
present for, and also a page written to disk that nobody chose to write, so it
is off until you turn it on.

A bundle holds the real page and the mirrored one, a screenshot from each side,
the wire frames actually sent, the document those frames add up to, both halves'
document hashes and the node-by-node fingerprints behind them, the session's
timeline, and both logs. Which pair you diff says where the bug is — the agent,
the frames, the patcher, or the CSS.

```sh
skyhookctl capture -pairing ~/.skyhook/pairing.json \
  -url https://news.ycombinator.com/ -note "the comment tree renders empty"
```

Typed text is redacted to a length and a digest by default: the keystrokes are
the reproduction steps and worth keeping, but they are also sometimes a
password. [docs/OPERATIONS.md](docs/OPERATIONS.md#diagnosing-the-mirror) has the
full contents of a bundle and how to read one.

## Repository layout

| Path | What lives there |
|---|---|
| `cmd/skyhookd` | The landside server binary |
| `cmd/skyhookctl` | Headless client: probe, pairing, kill switch, chat, capture |
| `internal/protocol` | Wire format: CBOR frames, zstd codec, dictionaries |
| `internal/transport` | WebTransport/QUIC and the WebSocket fallback |
| `internal/cdp` | A small Chrome DevTools Protocol client; launches or attaches |
| `internal/mirror` | The injected agent, snapshot/mutation pipeline, input replay |
| `internal/imgproc` | Image transcoding, blurhash, the landside cache |
| `internal/session` | Sessions, replay ring, resync, captures |
| `internal/diag` | Diagnostic bundles: the zip writer and the server's log ring |
| `internal/adapter` | Adapter framework and the Google Chat adapter |
| `internal/client` | A headless Go client, used by tests and `skyhookctl` |
| `client/` | The PWA: app shell, sandboxed mirror host, patcher, local echo |
| `client/src/mirror` | Patcher, echo engine and the sandboxed-frame host |
| `client/src/worker` | The network worker that owns the connection |
| `client/src/sw` | Service worker: offline shell, image cache, egress denial |
| `test/` | End-to-end tests against a real Chromium |
| `deploy/`, `scripts/` | Container, systemd unit, link emulation |

## Testing

```sh
go test ./...                    # unit tests; e2e skips without a browser
SKYHOOK_E2E=1 go test ./test -v  # end-to-end against real Chromium
cd client && npm test            # patcher, echo, codec, encoding, conformance
```

The wire format is pinned by cross-language fixtures in `testdata/`, in both
directions: Go writes `conformance.json` and the client's test suite decodes it;
the client writes `client-frames.json` and the Go suite decodes that. CI fails if
the two ever disagree.

The end-to-end suite includes tests that drive the real PWA in a real browser
against the real server — service worker, sandboxed frame, input path and all.
One of them takes a capture through the client's own UI and asserts the
screenshot that comes back up decodes and is not a blank rectangle, which is
what every failure of the SVG rasterisation path otherwise looks like.

## Security posture

The security model is **"the VPS is me."** All cookies, passwords and sessions
live landside; typed passwords transit (encrypted) to the VPS. This is
acceptable for a personal, self-hosted deployment and unacceptable to offer to
anyone else. The mitigations, the kill switch, and the residual risks are in
[docs/OPERATIONS.md](docs/OPERATIONS.md).

## Licence

MIT. See [LICENSE](LICENSE).
