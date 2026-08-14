# Implementation status and deviations

This document tracks what is built against [DESIGN.md](DESIGN.md), and — more
usefully — every place the implementation deliberately differs from the design,
with the reason. A design document written before the code is a hypothesis;
this is what survived contact.

## Milestone status

| Milestone | Design exit criteria | State |
|---|---|---|
| **M1 — Wire a mirror** | CDP snapshot → CBOR/zstd → patcher; click and type forwarding | **Done.** Verified end-to-end against real Chromium in `test/e2e_test.go`. |
| **M2 — Feel** | Local echo, ghost-send, scroll telemetry, images with blurhash, used-CSS | **Done.** Echo and reconciliation are unit-tested; used-CSS filtering and image transcoding are covered end-to-end. |
| **M3 — Survive** | Reconnect, resync, offline mode | **Done for reconnect/resync/offline queueing.** 0-RTT resumption is enabled in the QUIC config; it is not separately asserted by a test. FEC is not implemented — see below. |
| **M4 — Chat adapter** | Warm open ≤ 3 s, offline history, outbox | **Framework and adapter are built** (append-log, outbox, backlog replay, client archive and UI). The Google Chat selectors are a starting point, not a validated set: they need a session against the real app to tune. |
| **M5 — Polish** | Speculative prefetch, per-origin dictionaries, tabs/bookmarks, metrics HUD | **Prefetch, tabs, bookmarks and the HUD are built.** Dictionary training is implemented and tested server-side but is not enabled on the wire — see below. |

The client is a Chrome-targeted PWA served by the server itself; the Electron
shell the design called for was built first and then pivoted away from
(deviation 6).

## Deviations from the design, and why

### 1. No mutual TLS; the client pins the server certificate instead

The design specifies QUIC with mutual TLS and a pinned client certificate.
Chromium's WebTransport API cannot present a client certificate, and the client
is an Electron app that must use that API. The implementation instead:

- pins the **server** certificate by SHA-256 (`serverCertificateHashes`), which
  is strictly stronger than trusting the public CA set, and
- authenticates the **client** with a 256-bit token from the pairing file,
  compared in constant time.

The server generates its own short-lived ECDSA P-256 certificate (≤ 13 days,
what Chromium requires for hash pinning) and rotates it. Firewalling the QUIC
port and SSH remains the operator's job, as designed.

### 2. Forward error correction is not implemented

The design proposes XOR parity on the `dom` stream. In WebTransport, `dom` is a
reliable QUIC stream: QUIC already retransmits, and application-level parity on
top of a reliable stream cannot prevent the retransmit round trip — the stream
will not deliver out of order regardless. Spending 25% of a 250 kbps link on
parity that changes nothing is the wrong trade.

What is implemented instead, for the same goal (loss tolerance without a
round trip):

- input events and telemetry ride **datagrams** with a reliable mirror, so a
  lost keystroke does not wait for retransmission;
- images ride **their own streams**, so a lost image packet blocks nothing else;
- the replay ring buffer turns any detected gap into a single targeted replay
  rather than a stall.

Real FEC would need an unreliable DOM channel with application sequencing. That
is a plausible future change; it is not free, and the ring buffer covers the
observed failure modes.

### 3. Compression dictionaries are trained but not used on the wire

Per-origin zstd dictionary training is implemented (`internal/protocol/dict.go`),
tested, and runs hourly landside, writing dictionaries to disk. It is **not**
enabled on the wire, because the client's zstd decoder (`fzstd`, pure
JavaScript) has no dictionary support, and shipping a WASM zstd build into an
Electron app for it is a larger change than the win justifies today.

The protocol carries everything needed to turn it on: a `zstd-dict` capability
in `Hello`, a `DictUpdate` frame on the bulk channel, and a dictionary id in the
message header. When the client gains a dictionary-capable decoder, the server
side is already there. Note also that the intern table already captures much of
the same redundancy: repeated class names cost two bytes each after their first
appearance.

Dictionary *content* selection uses a recency heuristic rather than zstd's COVER
algorithm, which the pure-Go encoder does not implement. For a mirror stream
that is close to optimal anyway, because consecutive frames from one origin
share their boilerplate almost verbatim.

### 4. The mirror agent is primary; CDP's DOM domain is not used as a fallback

The design has the injected MutationObserver as primary with CDP DOM events as
a fallback and integrity source. In practice the injected agent is used for
both: it computes the document hash the integrity check compares against, and
`DOMSnapshot.captureSnapshot` is not used at all.

The reason is that the agent already walks the tree with exactly the semantics
the wire format wants (interned strings, flattened shadow roots, same-origin
iframes inlined, live form values), while `DOMSnapshot` returns a different
shape that would have to be translated. Two code paths producing subtly
different trees is precisely how silent divergence gets introduced.

The agent runs in an **isolated world**, so page script can neither see nor
tamper with it — a property CDP's DOM domain does not offer either.

### 5. The Google Chat adapter scrapes rather than using the Chat API

The design allows this explicitly ("or, pragmatically, puppeteers a dedicated
landside tab"). It is what is implemented, because the API path needs OAuth
scopes a Workspace admin can simply withhold, while the scraping path reuses
the profile that is already logged in.

All the app-specific knowledge is in a `Config` of CSS selectors, overridable
from a JSON file without a rebuild, because Chat's DOM is minified and changes
without notice. Every selector failure degrades to "found nothing" and the
generic mirror remains the working fallback.

### 6. The client is a PWA, not an Electron app

The design specifies an Electron shell. The client shipped that way first and
was then pivoted to a Chrome-targeted progressive web app. Most of the code was
unaffected — the transport was already written against the browser's
`WebTransport`, and the patcher and echo engine only ever needed a `Document` —
but three things changed shape:

**The no-page-JavaScript guarantee.** Electron gave it for free: the mirror
document declared `script-src 'none'`, and the shim still ran because preloads
execute in an isolated world that page CSP does not apply to. A browser has no
isolated world, so the mirror now renders into an iframe carrying
`sandbox="allow-same-origin"` and, deliberately, *not* `allow-scripts`. The
browser refuses to execute any script inside it, while `allow-same-origin` lets
the app reach in through `contentDocument` and patch. `allow-forms`,
`allow-popups` and `allow-top-navigation` are all withheld, so the frame cannot
reach the network or leave the page on its own. This is arguably a stronger
story than the isolated world: the guarantee is enforced by the sandbox, not by
a policy the content might influence. It is pinned by a test that fails if
anyone ever adds `allow-scripts`.

**Egress denial.** Electron cancelled every non-`skyhook://` request at the
session level. The service worker now refuses every cross-origin fetch, and the
app's CSP allows `connect-src` to exactly one server. The mirror transport is
not a fetch, so it is unaffected by either.

**The local store.** The design says SQLite; the Electron client used files.
Both are now IndexedDB plus Cache Storage. Image bytes live in Cache Storage so
the service worker serves them straight to the mirror frame with no hop through
the page.

The costs are real and worth naming: the archive is encrypted with a
non-extractable WebCrypto key in IndexedDB rather than an OS-keychain-wrapped
one, which resists reading the database out from under the browser but not an
attacker who already has the device and profile; and a backgrounded tab gets
throttled and may lose its connection, which Electron's `backgroundThrottling:
false` avoided. The second costs little in practice because it is the outage
case the reconnect path already handles, and the app nudges a reconnect on
`visibilitychange`.

What it buys: no second Chromium (which retires risk #4 in the design, "Electron
memory on a laptop in airplane mode"), instant updates through the service
worker instead of a per-platform installer matrix, and — when the server has a
real certificate — no `serverCertificateHashes` pinning or 13-day rotation
dance at all, since the same certificate covers the app origin and the
WebTransport connection.

### 7. Image encoding degrades gracefully instead of requiring libvips

AVIF and WebP encoding shell out to `avifenc` and `cwebp` when they are present
(the container image installs both). When they are not, the transcoder falls
back to stdlib JPEG for photographic content and PNG for flat-palette UI
sprites, chosen by the same palette heuristic the design describes. This keeps
`go test ./...` working on any machine and keeps the server pure-Go.

Resizing to rendered layout size — the part that actually saves the bytes —
happens either way.

### 8. Integer fields are capped at 32 bits on the client -> server path

`cbor-x`, the client's encoder, emits any integer above 2^32-1 as a float64,
and the server's decoder refuses to put a float into an `int64` field — which
rejects the entire frame. A wall-clock `Date.now()` in an input event therefore
silently dropped every click and keystroke, and the Go test client never
reproduced it because Go sends proper integers.

Two changes fix it and keep it fixed: timestamps are monotonic milliseconds
(which is what the protocol always specified), and every integer the client
writes goes through a `safeInt` clamp. The invariant is now covered from both
sides — a client test asserts no client frame contains a float, and a Go test
decodes fixtures the client actually produced.

### 9. Stream priority is enforced by the server's scheduler, not by QUIC

`webtransport-go` exposes no per-stream QUIC priority. The session's outbound
writer implements strict priority across four queues instead (ctrl/input, dom,
media, bulk), which gives the same ordering guarantee at the point where it
matters: nothing gets written to the socket ahead of a DOM diff.

## Known gaps

These are unbuilt or thin, and are honest to-dos rather than deviations:

- **P2 items from the design are not built**: the periodic-JPEG tile stream for
  canvas/video regions (single-frame capture *is* implemented via
  `Tab.CaptureRegion`, but no client UI triggers it yet), and a second adapter.
- **File upload** (R10) is not implemented; clipboard integration is limited to
  what the mirror's native selection gives you.
- **Find-in-page** works through Blink natively in the mirror, but there is no
  chrome-UI affordance for it yet.
- **The chat adapter's selectors are unvalidated** against the live app.
- **0-RTT resumption** is enabled but not asserted by a test; proving it needs a
  client that survives process restart, which the Go test client does not model.
- **Bookmarks** are stored and written but have no management UI beyond adding.
- **Installability is untested against a real install prompt**: the manifest,
  icons and service worker are all in place and the worker registers in a real
  browser under test, but nobody has clicked "Install" on a device yet.

## Measured results

From the end-to-end suite. The design asks for every milestone to be measured
against an emulated link rather than a LAN, so both columns are reported: a
plain loopback run, and a CI run with `tc netem` shaping the Skyhook port to
1.2 s RTT, 250 kbps and 2% loss.

| | Loopback | Emulated 1.2 s / 250 kbps / 2% |
|---|---|---|
| Whole suite (8 tests, each with its own Chromium) | 10 s | 158 s |
| Mirror delivers document and styles | 0.6 s | 23.3 s |
| Click → resulting mutation applied | 0.6 s | 20.6 s |
| Reconnect → resumed page state | 2.7 s | 27.5 s |
| Image with blurhash placeholder → bytes | 0.6 s | 15.5 s |
| **One appended chat-style message on the wire** | **73 bytes** | **73 bytes** |

Per-test figures include launching a browser and loading the fixture page, so
they are an upper bound on the interaction cost rather than a measure of it.
The number that matters for G6 is the last row, and it does not move with the
link: a new message costs 73 bytes because the intern table and the `splice`
op mean nothing structural is re-sent.

Other behaviours the suite pins down:

- A keyed list reorder arrives as a `move` op with the node count unchanged,
  which is the React-reorder mitigation from §2.15 working as intended.
- Used-CSS extraction ships matching rules and drops non-matching ones,
  verified against a fixture containing both.
- Page script never reaches the client: the mirrored document contains no
  `<script>` element and no inline handler, asserted directly.
- Typing reaches real page JavaScript landside (the fixture's own input
  handler rewrites a paragraph), and the live field value comes back as an
  attribute so a resync restores what was typed.
