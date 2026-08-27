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
| **M4 — Chat adapter** | Warm open ≤ 3 s, offline history, outbox | **Framework and adapter are built** (append-log, outbox, backlog replay, client archive and UI). The Google Chat selectors are validated against a real conversation as of §54, from a capture rather than a live session; they will need re-cutting whenever Chat's DOM moves. |
| **M5 — Polish** | Speculative prefetch, per-origin dictionaries, tabs/bookmarks, metrics HUD | **Tabs, bookmarks and the HUD are built. Prefetch was built and then removed** — see deviation 17. Bookmarks are a start page, a panel and address-bar completion rather than a list that is only written to — see deviation 23, and the address bar completes from where the reader has been as well as from what they saved — see deviation 34. Dictionary training is implemented and tested server-side but is not enabled on the wire — see below. |

The client is a Chrome-targeted PWA served by the server itself; the Electron
shell the design called for was built first and then pivoted away from
(deviation 6). It draws two chromes — a wide one for a mouse and a one-row one
for a phone, in either orientation — from the same behaviour (deviation 27).

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

CDP's **CSS domain** *is* used, for one thing the agent cannot do. Used-rule
extraction walks `document.styleSheets`, and a stylesheet served from another
origin throws on `cssRules`: the CSSOM will not show a page its own CDN's CSS.
On a site that keeps every stylesheet on a media domain — Amazon is one — that
is the entire design system, and the page arrives with all of its structure and
none of its appearance. DevTools is not bound by the same-origin policy, so the
host reads the text with `CSS.getStyleSheetText` and hands it back to the agent,
which replays it into a constructed stylesheet and filters it exactly like the
rest. Relative `url()`s are resolved against the sheet's own address on the way
past, because a constructed sheet would otherwise resolve them against the
document. The recovered rules take the blocked sheet's place in the walk rather
than being appended, so the cascade order is the one the page had.

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
Both are now IndexedDB plus Cache Storage. Image bytes live in Cache Storage,
where the service worker serves them from — but *not* straight to the mirror
frame, which was the original plan and does not work: a sandboxed frame is not
a service worker client, so `/img/<hash>` requested from inside it goes to the
network and comes back as whatever the server says about an unknown path. The
shell is a client, so it reads the bytes and hands the frame a blob URL, which
needs no fetch at all. The same blob resolves the `url()` references in
recovered stylesheets.

The costs are real and worth naming: the archive is encrypted with a
non-extractable WebCrypto key in IndexedDB rather than an OS-keychain-wrapped
one, which resists reading the database out from under the browser but not an
attacker who already has the device and profile; and a backgrounded tab gets
throttled and may lose its connection, which Electron's `backgroundThrottling:
false` avoided. The second costs little in practice because it is the outage
case the reconnect path already handles, and the app nudges a reconnect on
`visibilitychange`.

What it buys: no second Chromium (which retires risk #4 in the design, "Electron
memory on a laptop in airplane mode"), updates through the service worker
instead of a per-platform installer matrix — though "instant" was the wrong word
for them, and [§33](#33-a-client-that-runs-from-its-own-cache-cannot-see-that-it-is-old)
is what it cost — and — when the server has a
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

*Decoding* would not degrade the same way, and for a while it did not degrade at
all. Go reads PNG, JPEG, GIF and WebP; it does not read AVIF, and a site that
serves AVIF usually serves nothing else — so `image.DecodeConfig` failed, the
pipeline dropped the key without announcing it, and the client had asked once
for something that was never coming. A capture of an ASUS product page is what
this was found in: thirteen images, every one of them `.avif`, every one of them
an empty box, and a gallery whose picture would not change however many times
the reader clicked it, because there had never been a picture in it.

The fix takes the same shape the encoders do, but leans on the browser rather
than on a command-line tool. When the bytes sniff as a picture in a container
this process has no decoder for, they go back to the tab that asked for them:
`createImageBitmap` on a Blob made in the agent's isolated world, drawn into an
`OffscreenCanvas` scaled to the layout box, read back as PNG. Chromium already
holds a decoder for everything the web has agreed on — it decoded these bytes
once to paint the page — and the round trip is landside, on the half of the
connection with bandwidth to spare. The result is transcoded and cached under
the same content hash as everything else, so the trip is paid once.

Only a *recognised* container makes the trip: AVIF, HEIF, JPEG XL, BMP, ICO,
TIFF. Half the decode failures in a real capture are not images at all — an SVG
paint server referenced as `url(#gradient)` resolves to the page itself, and
what comes back is HTML — and asking Chromium to decode a web page would turn
one cheap failure into a slow one, once per reference. Without a live tab the
failure is unchanged, and now says which format it was, which is the sentence
that distinguishes "this whole site is unreadable" from "this one asset is
damaged".

### 7a. A failed asset is announced, because the client only ever asks once

The missing decoder turned out to be one way into a wider hole. Nothing in the
image pipeline ever announced a *failure*: every exit — a 403, a redirect to a
login, a source over the size cap, a full queue, a codec nobody has — logged a
line and returned. The key was never mentioned, the waiting list kept its tabs
on it forever, and the client had already spent its one question.

That "once" is deliberate and stays: a second ask costs a round trip on a link
where round trips are the whole problem. What was missing was the other half of
the bargain. `ImageMeta.Missing` says the bytes are not coming, the waiting list
is emptied, and the element drops the transparent placeholder so it falls back
to the alt text the page's author wrote for exactly this case — unless it is
already wearing a blurhash, which is a better picture than a broken-image
marker. A background image stays the transparent pixel it already was; there is
no alt text for one, and a broken-image marker tiled across a panel is worse
than nothing.

The failure is deliberately *not* recorded in the metadata table. A key with no
entry is one the next snapshot submits again, and re-trying on the next resync
is the only second chance anything here has.

Two more entrances to the same hole are closed with it. A fetch that succeeds
and returns no bytes — which is how `loadNetworkResource` reports a body it
could not read — used to reach the codecs, where "no bytes" and "a format I do
not know" are the same answer, so an empty response was logged as a missing
codec. And metadata that outlives the bytes it describes is the same silence
again: the table is bounded by a count and the cache by a size, so they part
company on any busy page, and a key announced out of the table whose bytes have
been evicted is answered by nothing at all. Both now say what happened.

### 7b. The landside cache had never once been read after a restart

It survived on disk and was consulted by nobody. Both readers ask the in-memory
metadata table first, that table starts empty, and so a directory holding half a
gigabyte of already-transcoded assets was re-fetched from the origin and
re-encoded — every page, every restart. The cache was a ledger for its own
eviction and nothing else. The lazily-re-sniffed mime, with its comment about
entries recovered at startup, was written for a path that could not be reached.

What was missing was not the bytes but everything needed to *announce* them.
Publishing an asset means naming its size, its type and its blurhash, and those
lived only in the map that had just been thrown away. So each entry now carries
its own description: a JSON header behind a magic prefix, ahead of the bytes in
the same file. One file per entry keeps eviction exactly as it was, and the
prefix means an entry written by an older build is recognisably not one of these
and is dropped rather than misread.

That fallback being unreachable had also hidden a bug in it. `Sniff` knew the
raster magic numbers and nothing else, so an SVG and a webfont — the two things
here that pass through untranscoded — both came back as
`application/octet-stream`. That answer rides to the `content-type` the client
stores the bytes under and from there to the type of the Blob it mints a URL
from, where, for an SVG handed to an `<img>`, it is load-bearing: a browser will
not sniff past the wrong type on a blob URL. Making the cache readable without
fixing that would have blanked every logo on the page.

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

### 10. The reader owns the viewport; landside scroll is never adopted

The design has `scroll` telemetry flowing plane-side → landside to drive lazy
loading and infinite scroll, and the agent reporting landside scroll positions
back as ops. Wiring both directions up as described produces a feedback loop:
the client reports where the reader is, the server scrolls the real page there,
the agent reports the real page's new position, and the client applies it. The
landside position is never exactly the reader's — different document height,
different image sizes — so every round trip displaced the reader by the error,
and a page that was merely being read scrolled itself to the bottom at roughly
a viewport every second.

Telemetry still flows landside, because lazy loading genuinely needs it. What
changed is that the return path is no longer authoritative:

- the agent does not report scroll positions it produced itself (`ownScroll`),
  which is also a small saving on the wire;
- the client applies a server scroll only to a scroller the reader has not
  moved, or one they are parked at the bottom of — following a chat log to its
  tail is the one case where being moved is the point;
- landside focus is applied with `preventScroll`, since focusing an element
  scrolls it into view and a landside focus change is not a request to move the
  reader;
- a snapshot for the URL already on screen is treated as a resync and keeps the
  reader's scroll position, rather than adopting the landside one. Only a real
  navigation adopts it, which is what carries a `#fragment` target.

The mapping the server sends is a fraction of the *scrollable range* rather than
of document height, so the top stays the top and the bottom stays the bottom —
an infinite list still only fetches when the reader is genuinely at the end.

The corollary took a bug report to notice: a link *into the document already on
screen* cannot be sent landside either. Hacker News' `parent | prev | next` are
`#fragment` links, and they used to travel as clicks — so the real page scrolled
itself and reported back a pixel offset from a layout with different fonts, to a
reader who by then owned their scroll position and was never moved again. The
gesture did nothing at all. The client now follows those links itself, scrolling
to the element the fragment names, which is both correct and free: the whole
document is already here, so it costs no round trip. A fragment that names
nothing in this document — `#/inbox` in a hash-routed app — is not one this side
can answer, and still goes landside to the router that knows what it means.

`test/stability_test.go` and `test/comments_test.go` pin all of this in the real
client.

### 11. A same-origin iframe is rendered into a substitute element

The design has the agent inline same-origin frames, and it does. The client
cannot materialise the `<iframe>` that held them: an iframe is a browsing
context, and a browsing context fetches things, which is the one thing the
plane side never does. So the patcher builds an inert element carrying the
original name in `data-skyhook-tag`, and the frame's inlined document renders
inside it.

Substituting rather than dropping matters for three separate reasons, all of
which were live bugs:

- a dropped element takes its subtree with it, so the frame's whole document
  was missing from the mirror;
- a dropped element leaves the client's node ids out of step with the agent's,
  which the integrity check reads as divergence;
- the CSS that sized the real frame selects on `iframe` and cannot match the
  substitute, so the agent sends the frame's rendered box as `data-sky-box` and
  the client applies it directly.

Inlining the frame's document also puts its nodes into the tab's id space, and
that is where a fourth bug lived. The client sends back an id, the agent answers
with `getBoundingClientRect`, and the host replays the click by dispatching a
mouse event at that point in the *top-level* viewport — but the rect came from
the frame's document, measured from the frame's own top left. Every click inside
a frame therefore landed short by exactly where the frame sat, on whatever was
above and to the left of it. Nothing reports that: the mirror is right, the
event is delivered, the page is fine, and the control simply never responds.
`__skyhook.rect` now walks `frameElement` up to the top document and adds each
frame's border-box origin plus its border and padding. Reproduced by
`TestPWAClicksAControlInsideAnInlinedFrame`.

One thing about frames is still not what a browser does. A frame that navigates
is picked up by a `load` hook that re-snapshots the document, which is blunt:
nothing about a document being replaced reaches the MutationObserver watching
the old one, so there is no diff to send.

### 11b. A frame's box is measured once, and frames change size

`data-sky-box` was written when the frame was serialised and never again, so a
frame that changed size afterwards kept the box it was born with for the life of
the document. That is not an edge case: it is how every popover on a Google
property opens. Gmail's app launcher is a frame inside a wrapper the page
animates from `height: 0`, and the frame is put in the document as the animation
starts — so the measurement was 370x0. The wrapper's own height reached the
client (it is an inline style, and a style is only an attribute), the stand-in
inside it stayed zero high, and the panel was not on screen at all. Nothing
downstream says a word about it: the documents agree, the hashes agree — the
hash covers ids, kinds and names, not attributes — and the only symptom is that
clicking the grid of dots appears to do nothing, which the reader cannot tell
apart from an input the link swallowed.

Polling every frame's `getBoundingClientRect` is not affordable, and a
MutationObserver is no use here: the style that changed is on an ancestor, and
layout is not a mutation. A `ResizeObserver` reports exactly this and nothing
else, so every frame gets one — created from its *own* window, because an
inlined frame's elements belong to that document. What it marks dirty is
re-measured on the way into the next flush rather than in the callback, so a
size that moves ten times across a 300ms transition costs one op, and a resize
that arrives with a mutation rides the same frame as the mutation.

Plane-side, the box is re-applied whenever the page rewrites the element's
`style`: the width and height are the mirror's, not the page's, and a `style`
write replaces the whole declaration. `TestPWAResizesAFrameStandInWhenItsFrameGrows`
is the panel opening; the patcher's own tests pin the style rewrite and the
box being taken away again.

### 11c. A frame the mirror could not read says so

A cross-origin frame cannot be read at all — no agent runs in it, and its
`contentDocument` is closed — so its stand-in is empty however right its box is.
Empty, it is also invisible, and an invisible hole where a panel should be is
indistinguishable from a bug: the app launcher above was reported as one twice,
once for the box and once for what was never in it.

So a frame whose document could not be read is named. The agent puts the frame's
origin in `data-sky-frame`, and the client draws the stand-in as a faint dashed
panel that says *ogs.google.com — not mirrored*. It is the same bargain the HUD
makes everywhere else: the reader cannot have the content, and is owed the
difference between "this did not come" and "your click was lost". Only frames
big enough to have been worth looking at are named — 64x32 is the floor, which
keeps the beacons and tracking pixels every page carries out of it — and the
mark is kept current by the same watch as the box, because a frame is readable
`about:blank` until the moment it is not. `TestPWASaysWhichFrameItCouldNotRead`
covers it.

That label is now the fallback rather than the answer: §11f mirrors the frame
itself, and the mark is cleared on any frame the host has an agent in. What is
left wearing it is a frame nothing could reach — one nested past the depth limit,
or a target that would not take the agent.

### 11f. A cross-origin frame, mirrored by an agent of its own

The label above was honest and no use to a reader who wanted the app launcher.
So a frame nothing can read now reads itself: the agent runs in it, in an
isolated world, and the host splices what comes back into the document above it.
`internal/mirror/frames.go` is the whole of it. Five things had to be true.

**Every frame that needs one gets an agent — and the interesting half is not the
one the design predicted.** A frame on another *site* is a target of its own, in
a process of its own, and the host has to attach to that target with
`Target.setAutoAttach` before any script of ours runs in it. A frame that is
cross-origin but *same-site* is not a target at all: mail.google.com holding
ogs.google.com is one process, and no attachment event will ever fire. That is
the shape of nearly every real one — the app launcher included — so a mirror
that followed only targets would have missed the bug this was written for. It
did, in fact, on the first run of the tests: nothing attached, because
`127.0.0.1:A` and `127.0.0.1:B` are the same site too.

Both are covered by having the frame speak first. The tab's own script already
runs in every frame of its process, so the test is local: a document whose
`frameElement` is null has a parent that cannot read it, and nothing above it is
mirroring it. Such a frame announces itself and waits; the host answers with
`adopt`, which hands it a slot and starts it. A same-origin frame sees its own
element, says nothing, and is inlined by the agent above it exactly as before.

**Ids are namespaced, inside 32 bits.** Each frame allocates in a block of its
own, so two agents cannot collide. The width is not arbitrary: the client
encodes an id above 2^32-1 as a float and the host's decoder refuses to put a
float in an integer field, dropping the frame whole — the bug `safeInt` exists
for. The first cut used 2^32 ids per frame and brought it straight back, as
`node 4294967295 not found landside` on every click inside a frame. The page now
keeps everything below 2^31 and the frames divide what is above: 8M ids each,
254 to a tab.

**One client table, several interning agents.** Strings travel as indices into a
table the client appends to, which is exactly right while one agent is writing.
A second agent numbers from zero in a table of its own, so its `ref: 0` means
its first string and arrives at a client whose zero is the page's. Nothing
errors — the indices are all in range — and the frame renders in the page's
words. The first tags of any HTML document are the same everywhere, so it even
looked like it was working: `<html><head>`, and then the rest of the document
silently missing. The host now maps each agent's table onto the client's and
rewrites every ref it forwards (`internal/mirror/strings.go`). The client is
unchanged.

**The stream has one writer's worth of order.** Sequence numbers are consecutive
and the client drops a batch with a gap in front of it, so taking the next number
and putting the frame on the wire has to be one step. With the page's agent and
every frame's each on a queue of its own, it was not. Emission is now serialised
on the tab, and splicing is serialised per frame and made idempotent — two
concurrent splices of one document built it twice under the same ids, leaving
the client's map pointing at nodes that were not the ones in its tree, which
renders as two half-documents inside one box.

**The hash chains.** §12's three-way contract assumed one landside writer. The
integrity check now asks each agent in slot order for the hash of what it holds,
seeded with the answer from the one before; ids sort by slot, so that is exactly
the order the client hashes in. The sequence number the client is checked against
is the tab's, taken after fencing every event queue (`cdp.Client.Fence`) —
reading it before the queues had drained named a frame the client already had
while the hash described the document one frame later, which is a divergence
report that is only a race with itself.

**An insert waits for its parent instead of being dropped.** The client used to
throw away an insert addressed at a node it did not have, silently, which is
exactly what a frame's document is when it overtakes the element it hangs from —
and the host had no way to know, so it believed the frame was mirrored while the
reader looked at an empty box. Ordering is the client's to know, because the
client is the only one that knows what it has; it holds such an insert and
applies it when the parent arrives. (One more of these was self-inflicted: the
frame's parent was looked up over CDP while the tab's lock was held, which
stalled every frame waiting to go out behind a browser round trip. Batches that
took ninety seconds take twenty-three.)

**It converges on a clock, not on a chain of events.** Splicing a frame depends
on a run of things going right in an order nobody controls: the frame announces,
the page serialises the element it hangs from, the snapshot arrives, the insert
goes out. Each step got a retry, and each retry hung off an event — the parent
emitting again, a backoff timer, the frame speaking. Every one of those was
reached by a path that sometimes did not happen, and the symptom was always
identical: a frame adopted, mirroring, sending mutations landside, and simply
absent on the client until the integrity check noticed thirty seconds later and
resynced the whole tab. Which is to say the *recovery* worked and the arrival did
not. So the last piece is a reconciler: every two seconds the tab compares what
it is mirroring against what the client has, and asks any frame that is out of
place to say itself again. One CDP call per frame that is wrong, none for a frame
that is right, and every race above collapses into the same convergent loop.

**Eight levels down, once the levels stopped taking each other away.** This was
capped at one for a while and the cap was honest: the levels below the first
arrived and then went. The cause is worth keeping. Splicing a frame replaces its
subtree, so the client drops what was inside it — including any frame spliced in
there — while those frames' agents saw nothing happen at all and went on
describing a document nobody had. The reconciler could not help: it looks for
frames that believe they are absent, and these believed they were fine. So a
splice now invalidates everything mirrored inside it and asks each of those
frames to say itself afresh, which is the whole of what deep nesting needed.

**A bad link paces the repair; it never calls it off.** The reconciler re-sends a
frame's whole document, and a page is re-snapshotted on every resync, so a page
with several frames in it can put all of them onto a queue the reader is already
waiting on. Gating that on the emitter's backlog signal — the trade `shotSoon`
makes for a screenshot — looks like the same restraint and is not: a screenshot
is worth skipping because something else will ask for it, and a missing frame is
a hole in the document that nothing else closes. Worse, the hole is itself a
reason the integrity check keeps resyncing, so the backlog suppresses the repair
and the absent repair sustains the backlog. Both cross-origin frame tests spent
three minutes in that deadlock, and the fast end-to-end job, which had been
green, lost them too. What the link buys is pacing: every frame at once when
there is room, one per two-second tick when there is not (`framesDue`), which
converges on any link and floods none. A re-splice also asks only the frames
inside the one it replaced — asking the whole tab made a page eight frames deep
send forty snapshot requests in fifty milliseconds, each splice re-asking every
frame not yet spliced.

**And the test that fails is not always the code that is wrong.** The two tests
that failed over the emulated link were waiting for a sentence the fixture's
widget painted over 1.2 seconds after loading. A client that has not rendered
the frame by then is waiting for text that exists nowhere, and 250 kbps
guarantees it — as does a loaded machine, which is how it finally reproduced
locally. The frame now leaves its first line alone and changes its mind on a
line of its own, repeatedly, saying how many times; the test reads the count it
can already see and waits for a higher one. That is the general shape for
"followed a change" over a link with no promised speed: assert on something
monotonic that the client itself has seen, never on a state the page passes
through.

rrweb has this bug open against its own cross-origin recording — "when the
parent is reset during its FullSnapshot, the iframe context is wiped, and
subsequent child events can't be played in the DOM correctly" — and it is harder
there: a recorder living inside a page cannot ask a child frame to re-snapshot.
Driving the browser from outside is what makes the fix available here.

Eight is measured — nine documents deep, every level arriving and staying — and
bounded by what a click costs: one round trip per level to walk a rectangle up
into the top-level viewport.

Also not covered: a frame the page hides and shows repeatedly pays a re-snapshot
each time, for the same reason; and a closed shadow root inside such a frame is
as invisible as it is anywhere else. `test/foreignframe_test.go` covers the
document, its scoping, its later mutations, a click landing on a control inside
it, and the hash agreeing; `TestPWAMirrorsFramesInsideFramesAndSaysWhereItStops` covers
the floor.

### 11d. The frame's box was one of five, and they now share a sweep

Fixing the box invited the obvious question — what else is read once and never
again? — and the answer was five more, all of the same shape. The mirror carries
two kinds of thing. Structure is reported by a `MutationObserver` and arrives on
its own. Everything else is a **property or a measurement**, read at the moment
the element was serialised, with no event to say it has moved. Nothing catches
that afterwards, either: the integrity check hashes ids, kinds and names, so
every one of these leaves both sides agreeing about a document they disagree
about, and no resync is ever triggered. Each was reproduced against a real
browser before it was fixed.

**What a control holds.** `value`, `checked` and `selected` are properties.
`value` was refreshed by an `input` event landside — which covers the reader's
own typing and nothing else — and the other two were refreshed by nothing at
all. So everything a *page* does to its own form was silent: a React controlled
input re-rendering, a draft restored, a search box cleared after submit, a
"select all" ticking every row, a dependent dropdown moving to its new choice.
A checkbox the page ticked stayed unticked in the mirror for ever. The controls
the mirror carries state for are now watched, and the sweep reports the
difference; reading these three forces no layout, so the pass costs a property
read per control. Removing the mark is how the agent says *unticked*, so the
patcher unsets the property rather than merely dropping the attribute — an
attribute going away does not untick a box.

**A shadow root attached by hand.** §19 watches custom elements that were
mirrored *undefined* and re-reads them when their definition lands. Any element
can be given a root at any time, though, and two shapes fall outside that watch:
a plain `<div>` a widget takes over, and a component already defined when it was
serialised that attaches its root a tick later. Both mirror as light DOM
rendered flat, which is the Reddit failure of §19 through a door §19 did not
cover — and the manual claimed otherwise. The sweep now asks an exact question
rather than a heuristic one: an element wearing a shadow root the agent has no
id for is a root that was never mirrored, and it is re-read.

**The page's own name.** The title and URL travel as fields on a mutation frame,
and a frame is only sent when there are ops to put in it. A page whose *only*
change is its title therefore never sent one — unread counts, "(2) Slack", a
build finishing in a tab nobody is looking at — and the title's own text node is
not mirrored either (head content is replaced by used-CSS), so its mutation
record is dropped and nothing is left to notice. It now travels as a `DocInfo`
op. It has to be an op and not an empty frame: the host discards a mutation with
nothing in it, and the agent's sequence numbers are the ones the integrity check
waits for the client to reach, so a frame the host dropped would leave the two
counting differently.

**One sweep, not three.** These share a shape — a question only landside can
answer, no event to answer it, and an answer worth nothing until something asks
— so they share §19's poll rather than adding two more timers. It backs off to
eight seconds while it keeps finding nothing and drops back to half a second the
moment anything moves. `test/livestate_test.go` covers the lot.

### 11e. What the client paints is not the page's to overwrite

The frame box was half of a second pattern, and the other half was live. The
client paints things into an element's inline style that the page knows nothing
about: a frame's stand-in size, the photograph of a canvas, an image's blur
placeholder and reserved box. The landside element none of it came from carries
none of it — so the page's next `style` write, made for some unrelated reason,
replaces the whole declaration and takes all of it away.

For an image that is a flicker. For a canvas it is permanent: shots are taken in
answer to input (§ region shots), so nothing repaints that element until the
reader touches something, and a mirrored map or game goes blank and stays blank
with nothing on screen to explain it. The patcher now tells the host when a page
`style` write landed on an element, and the host puts its own painting back —
only for writes to a document the client already has, because at creation there
is nothing painted yet and firing it per element in a snapshot would be a few
thousand calls that can only answer no.
`TestPWAKeepsACanvasPhotographThroughARestyle` is the canvas.

### 11a. Stylesheets a page picks up after its load event

The CSSOM will not show a page a stylesheet served from another origin, so the
host reads those over CDP — which is not bound by the same-origin policy — and
hands the text back as a constructed sheet the agent can filter like any other.
That recovery ran on `Page.loadEventFired`, once, which catches the sheets the
document was parsed with and none of the ones after: a widget that inserts its
iframe on load brings a whole stylesheet with it, and so does the next chunk of
a client-side route. Those stayed unreadable for the life of the document, and
their markup arrived with no styling at all.

The agent now sends `{t:'sheets'}` the first time it sees a href it cannot open,
and the host answers by running the same recovery pass. Google's "unusual
traffic" interstitial is the page that showed it up: its reCAPTCHA checkbox is a
`<span>` that is only a checkbox once its stylesheet says so, so plane-side it
was an invisible zero-size element with nothing to click. Pinned by
`TestPWARecoversAStylesheetThatArrivesAfterLoad`.

### 12. The document hash is a three-way contract, and was wrong

`__skyhook.docHash`, `Model.Hash` and `Patcher.docHash` are three
implementations of one function, and the server treats any disagreement as
proof the mirror has diverged — then re-snapshots the whole document. The agent's
copy multiplied with `*` rather than `Math.imul`, so above 2^29 the FNV product
needed more than a double's 53 bits and the low bits were rounded away, and it
walked a `Map` in insertion order where the others sort by id. It therefore
disagreed with both on essentially every page, and every session re-sent every
document every thirty seconds for as long as it was open.

`test/integrity_test.go` now pins agent against replica, and the real client's
reported hash against the agent, on a plain page and on one with a frame.

### 13. The mirror's context menu is the shell's, not the browser's

The design leans on inheriting Blink's behaviour for free (§2.3): layout, text
shaping, selection, find-in-page. The context menu is where that stops. Every
native entry is computed from the sandboxed frame rather than from the page the
frame is showing, so "Open link in new tab" opens `about:blank` (the sandbox
withholds `allow-popups` anyway), "Reload" reloads a document with no origin,
"Save image" saves a file named after a content hash, and "Back" walks the
shell's history instead of the tab's. Nothing on it is right.

The menu is now drawn by the shell (`client/src/app/menu.ts`) from what the
mirror host reports about the node under the pointer: link entries, image
entries, clipboard entries for an editable field, and the tab's own
back/forward/reload/bookmark/duplicate. Two consequences are worth naming.
Forwarding the right click landside — which pages with their own menus answer,
the reply arriving in the mirror as ordinary DOM — used to be automatic and is
now an entry on the menu, because it costs a round trip and most right clicks do
not want one. And the clipboard entries for a mirrored field apply their edit to
the local DOM first and send the whole field value, so a paste is visible
immediately and reconciles through the echo engine's existing path (§2.7)
instead of waiting out the link.

Middle click is the same story in miniature: the sandbox swallows it, so the
host claims the gesture and asks the shell for a background tab. Ctrl/⌘-click on
a link is treated identically. Replaying either landside would open a tab on the
VPS that the client has no handle on — the session only tracks tabs it opened.

Right-click keeps the native menu in exactly two places: the shell's own text
fields and the chat panel. Those are real local documents, so the browser's
clipboard entries do the right thing there, and the URL bar is where people
paste.

### 14. The browser's own back and forward drive the tab, not the shell

The toolbar's arrows were the only working way back. Every other one a reader
has — the browser's buttons, the mouse's side buttons, Alt+←, ⌘+[, the
two-finger swipe, Android's system back — acts on the shell's history, and the
shell has one entry: the app. The most ordinary instinct in browsing therefore
threw away the session and every page it had paid for, while the page the reader
wanted sat one round trip away, never asked for.

The shell now keeps a history entry on either side of itself and lives in the
middle one (`claimHistoryGestures` in `client/src/app/main.ts`). A `popstate`
onto either side is the gesture: it is spent on the active tab's history and the
middle entry is restored underneath the reader.

Catching the step rather than the gesture is the whole point. Chromium resolves
all of them to a session-history traversal before any of them is an event a page
can see — a mouse side button is not cancelable from the renderer at all, and a
cancelable chord like Alt+← only stays cancelable per platform and per browser.
Recognising them individually means re-implementing that keymap and being wrong
in the expensive direction: a chord the browser acts on *and* the shell answers
goes back two pages, which on this link is a page fetched in order to be thrown
away. The first implementation of this did exactly that, and
`test/navigation_test.go` presses a real mouse back button through CDP because
that is the only way to tell the two apart.

A back gesture the tab cannot answer is deliberately let through: it lands on
the entry behind, where the app looks unchanged and a second press leaves for
real. The trap is armed again as soon as a tab has somewhere to go back to. A
browser that cannot be left by the gesture that leaves browsers is worse than
one that has to be told twice.
### 15. A navigation's URL and its history flags travel together

Every tab-state frame is stamped with the tab's cached `canBack` and
`canForward`, and a navigation is exactly the moment those change. The frame
announcing a new URL was sent before asking the browser what the history now
looked like, so it carried the *previous* page's flags: "you are on the index"
and "there is nothing forward of here", the second of which stopped being true
the instant the first became true. A corrected frame followed once the page had
settled.

Landside that gap is 11 ms and nobody notices. On the link this project exists
for, the correction queues behind the new page's snapshot at 250 kbps, and the
two frames are seconds apart. Every back or forward gesture the reader makes in
that window is dropped on the floor — `goHistory` refuses to spend a gesture the
tab says it cannot answer, so the press does nothing at all and the reader
presses again.

The history is now read before the URL is announced, in the same frame. It costs
one landside CDP call ahead of the URL bar updating, against the second that
frame then spends in the air.

`test/navigation_test.go` catches this only over the emulated link, where it
appears as a forward gesture that never arrives.

### 16. Attaching to a running browser needs `window.open`, not `Target.createTarget`

The design assumes the server launches Chromium and owns it. `chromeAttach`
adds the other case — a browser already running, whose profile and logins are
shared — and there the landside browser is somebody's, so Skyhook has to keep
its tabs in a window of its own.

CDP offers no way to say that. `Target.createTarget` takes no window id; left
alone it puts the tab in whichever window the user touched last (measured: it
goes to their window even with `background: true`, and even right after we
opened and activated one of our own), and `newWindow` opens a *fresh* window
per tab rather than reusing ours. The one placement rule that does hold is the
web's: `window.open` puts a tab in its opener's window and keeps doing so no
matter which window has focus.

So attach mode opens one window with `newWindow`, keeps a blank anchor tab in
it, and creates every later tab by evaluating `window.open` there —
`userGesture` set so the popup blocker allows it, and `noopener` set so a
mirrored page gets no handle on the anchor. The new tab is recognised by its
`openerId`, which Chromium records as the anchor even under `noopener`; tab
creation is serialised, so only one is ever in flight. The tab is opened blank
and navigated afterwards, so no page URL is ever spliced into JavaScript
source.

A tab meant to stay blank is *not* navigated to `about:blank`. An earlier
version marked each tab with a nonce in its URL fragment and then navigated it
away to clear the mark; Chromium wedges a tab closed immediately after a
navigation it has not committed, so the prefetch pool discarding a spare left
an unclosable tab in the reader's browser — reproducible on Chrome 151, not on
Chromium 141. Identifying the tab by opener rather than by a mark in its URL
removes the navigation, and with it the wedge.

The rest is restraint: targets that existed before we attached are never
attached to, closed or listed, and shutdown closes our own tabs instead of
calling `Browser.close`, which would quit the browser out from under whoever is
using it. `test/attach_test.go` drives a second real browser as "the user" and
asserts each of these, including that the user activating their own window
mid-run does not divert the next tab.
### 17. Speculative prefetch was built, and then removed

The design's §2.8 asked for it and it worked: a hidden pooled tab walked the
top five same-origin links on the page, and a link-follow that hit a
speculation cost zero perceived round trips. It is gone anyway.

What it looked like from the origin's side is the problem. A logged-in session
requesting five permalinks every four seconds, each from `about:blank` with no
referer, no user activation and no interaction between them, is not a reading
pattern — it is a crawl, and it is the pattern rate limiters and bot scoring
exist to catch. Skyhook is not a scraper, but the traffic it generated could
not be told apart from one, and the account paying for that was the user's own.

The trade was bad on its merits too. Speculation spent landside bandwidth and
origin goodwill on pages the user mostly did not open, to save one round trip
on the ones they did. On a 1.2 s link that round trip is worth something, but
not an account.

Removed with it: `TypeSpeculative` (frame 23) and `Snapshot.speculative`
(field 10), the client's speculation cache, the agent's `links()` collector,
and `InputEvent.URL` — the anchor href the client attached to every click,
which only speculation ever read. The frame and field numbers are retired
rather than reused, so a stale client cannot be silently misread by a new
server.

### 18. The landside browser is used as a browser, not as an instrument

Skyhook is not a scraper, but almost everything about how it drove Chromium
looked like one to an origin, and the account paying for that was the user's.
Four things changed, all of them the same change.

**The browser fetches its own images.** The transcoder used to fetch image
bytes itself over a Go `http.Client`, with a hardcoded `Chrome/126` user agent
and the page's cookies, which is a second visitor arriving with the first one's
session: different TLS fingerprint, different header order, no client hints, a
UA disagreeing with the browser that requested the page. Images now come back
through `Network.loadNetworkResource` on the tab that referenced them. The
direct path survives only for assets whose tab has closed, and sends no
credentials, so a cookie never leaves Chromium.

**One story about who it is.** Setting `userAgent` moved the `User-Agent`
header and `navigator.userAgent` and nothing else; `Sec-CH-UA` and
`navigator.userAgentData` kept describing the real build. The option therefore
made things worse than leaving it unset — a browser contradicting itself is a
louder signal than an unusual user agent. The override is now
`Emulation.setUserAgentOverride` with client-hint metadata derived from the
string itself, so the two cannot drift.

**Headful by default.** Headless Chromium puts `HeadlessChrome` in its own user
agent and sets `navigator.webdriver`. The image and the systemd unit both
supply a virtual display; on Linux with none available the server says what is
missing and starts headless rather than refusing to boot.

**The reader's own pointer.** A click used to be replayed as one instantaneous
event at the exact centre of the element. The client now measures what its
pointer actually did — hold, position within the box, approach — and the server
replays that, synthesising only when there is nothing to replay. The data is a
few tens of bytes on a frame already being sent.

What is deliberately *not* here: nothing in this list lies about what the
browser is. It is a real Chromium, driven by a real person, on a real profile;
the work was in stopping the plumbing from contradicting that.

### 16. Diagnosing the split renderer needs both halves at the same instant

The integrity check tells you the two halves disagree. It never tells you why,
and the design has no answer for that: a hash mismatch is a boolean about a page
that has since moved on, on a device you cannot reach, in a session that expires
in twelve hours. Every mirror bug so far has been found by reproducing it
locally, which works for the ones that reproduce.

So a **capture** freezes both halves at one instant and zips them landside:
`<dataDir>/captures/<timestamp>-<id>.zip`. What is in it and how to read it is
in [OPERATIONS.md](OPERATIONS.md#diagnosing-the-mirror); what is worth recording
here is the four things that were not obvious.

**The replay ring cannot serve as the frame record.** It drops a frame the
moment the client acknowledges it — and a mirror that went wrong went wrong
while applying frames it acknowledged. So each tab keeps a second, separate
journal, bounded by bytes and reset at each snapshot, which the capture replays
through `mirror.Model` to produce `expected.html`: the client's document as the
frames actually sent specify it. That artifact is what makes the bug locatable,
because it splits "the agent serialised it wrong" from "the patcher applied it
wrong" — two failures that look identical from either end. A journal that had to
drop frames says so, and the capture then declines to claim a reconstruction it
cannot stand behind.

**A capture at a divergence races the repair of that divergence.** The server's
next act after finding a mismatch is to resync the tab, which starts a new
snapshot, empties the journal, and replaces the client's diverged document with
a correct one. Gathering any of that afterwards produces a beautiful bundle
describing a mirror that is working. So the perishable state is frozen
synchronously on both sides before the resync is allowed to happen: landside in
`Session.freezeTabs`, and plane-side in `MirrorHost.freeze`, which serialises
the mirrored DOM before its handler awaits anything. The request rides `ctrl`
and the resync rides `dom`, so the client hears about the capture first; the
ordering is load-bearing, and both halves say so where it matters.

**There is no browser API that hands a page a picture of itself.**
`getDisplayMedia` wants a permission prompt and a user gesture; a canvas cannot
draw a live DOM. The client serialises the frozen document into an SVG
`<foreignObject>`, loads that as an image and draws it onto a canvas — with two
catches that each cost the whole screenshot if missed. The markup has to be
well-formed XML, and real pages carry attribute names (`@click`, `:class`,
`x-on:keyup.enter`) that are not, so one Vue directive anywhere on the page
would otherwise blank the picture; those are stripped. And an SVG image may not
load anything external, so every mirrored image is inlined as a data URI first —
which is possible only because the bytes are already plane-side in Cache
Storage, the same fact that lets a warm client paint a page it never downloaded.
The rasteriser is tested against a real browser holding a real mirror, and the
assertion is that the resulting WebP decodes and is not one flat colour: every
way this path fails produces a plausible-looking blank rectangle.

**A bundle is a thing people send to each other.** So the reader's keystrokes
are recorded as a length and a short digest rather than verbatim — the timeline
is the reproduction steps and is worth keeping, but it is also sometimes a
password — and form submissions record field names without values. `captureText`
turns that off for an operator who needs the contents. The pairing token is
never in a bundle at all.

The cost is deliberate and bounded: a capture is the one thing in Skyhook that
spends the link on purpose. Nothing is captured unless somebody asks — the
divergence trigger exists, and is off until an operator turns it on, because a
bundle written with nobody present is page content on disk that nobody chose to
write. When it is on it is rate-limited, because a page that diverges once
usually diverges every thirty seconds. Both directions are capped, and artifacts
are gathered cheapest-and-most-valuable first, so a capture cut short by an
outage loses its screenshot rather than its DOM. `captureKeep: 0` removes the
feature, and the frame journals with it.

### 19. A custom element's upgrade is landside truth, not a plane-side event

Plane-side runs no page JavaScript. That is the product, and it means one thing
the design did not think through: **no custom element in the mirror will ever
upgrade.** Both halves of the mirror got this wrong, in opposite directions, and
a mirrored Reddit was the result — four dropdown menus rendered open and stacked
over the feed, under a collapsed search bar.

**The DOM half.** An element is serialised once, with whatever it had at that
moment. On a code-split site the definition arrives with a later bundle, and the
upgrade attaches a shadow root and distributes the light DOM into its slots —
none of which reaches a `MutationObserver`, which reports child lists,
attributes and character data and has nothing whatever to say about
`attachShadow`. The mirror kept the pre-upgrade skeleton for ever: the
component's own markup missing, and the light-DOM children destined for a slot
inside some collapsed popup rendered flat and in the open. rrweb closes this by
patching `Element.prototype.attachShadow`; that is not available here, because
the agent lives in an isolated world and the prototype it can reach is not the
one the page calls. So the elements that were mirrored undefined are watched and
re-read when they upgrade — a poll landside, where cycles are free and nothing
reaches the wire unless something actually changed. It backs off while nothing
is happening. It no longer stops when every watched element has upgraded:
[§11d](#11d-the-frames-box-was-one-of-five-and-they-now-share-a-sweep) gave the
same pass two more questions to ask, and both outlive the last upgrade.

**The CSS half.** `:defined` is a live question landside and a settled one
plane-side. The used-CSS filter asks it landside, at a moment of its own
choosing, and the answer was then shipped to a document where it means the
opposite: the placeholder styling a site hangs off `:not(:defined)` was dropped
as unused (every element had upgraded by the time the filter looked) while the
styling gated on `:defined` was shipped to a client where it could never match.
Reddit uses both — `.nd\:invisible:not(:defined)` is what keeps a menu's items
out of sight until the menu exists. So the agent records the landside answer on
the element as `data-sky-undefined`, and `rewriteDefined` re-points the rules at
that mark: `:not(:defined)` becomes `[data-sky-undefined]` and `:defined`
becomes `:not([data-sky-undefined])`. Specificity is unchanged — a pseudo-class
and an attribute selector count the same — and the mark is cleared by the same
pass that re-reads the upgraded element, so the two halves cannot drift apart.

**The scoping half.** The same page came back with a third version of the same
mistake, and this one was structural rather than temporal. The mirror flattens
every shadow tree into its host, so plane-side there is no boundary left for a
shadow-scoped selector to be scoped *by*: `:host` matches nothing outside a
shadow tree, and `::part()` names a part of a tree that is no longer there. A
component's stylesheet therefore crossed the link intact and did nothing —
`:host` rules shipped and inert, `::part()` rules dropped by the filter, since
`querySelector` never matches a pseudo-element and every one of them looked like
a rule the page had no use for. It is not a corner: Reddit's search field is a
`faceplate-search-input` whose box padding, its font and the `white-space: pre`
that keeps its placeholder on one line are all `:host` rules, and with all of
them dead the field spelled "Find anything" straight down the screen, a letter
per line, on top of the header.

So they are re-pointed as the sheet is read, in the one place that still knows
which element hosts it — `:host` becomes the host's tag name, `:host(S)` carries
S's conditions onto it, `:host-context(S)` becomes the host under an ancestor
matching S or matching it itself, and `X::part(p)` becomes `X [part~="p"]`,
which is exactly where flattening left it. `::slotted()` is left dropped, and a
part renamed on the way out by `exportparts` is matched under its inner name,
because that is the name the flattened tree keeps. Specificity does move here —
`:host` counts as a
pseudo-class and lands as a type selector — but by the same amount for every
rule in a component's sheet, so the order a component intends among its own
rules survives; only ties against another sheet can turn over, and those were
approximate already in a document whose shadow styles are all hoisted into one.

The filter had to learn the same distinction, because a selector that reaches
across the boundary cannot be tested by asking one side of it. A shadow sheet's
rules are tested against the shadow root with the host compound taken off the
front — the host is the root's own, so its half of the question is answered
before it is asked — and a `::part()` rule is tested by asking whether the
element whose parts it styles is on the page at all, which is the question that
actually decides it: a site ships parts for drawers and modals it is not
currently showing, and those still go.

**The composition half.** With the selectors re-pointed, the same page came back
wearing "Open navigation", "Go to Reddit Home", "Sign up for Reddit", "Log in to
Reddit" and "Open settings menu" across the top of itself, permanently. Those
are tooltips. A component of that kind keeps its text in the light DOM and slots
it wherever it wants it drawn, which for anything that opens and closes is
inside a box that is hidden until it opens:

```html
<div part="body" class="tooltip-body" hidden>
  <slot name="content"></slot>          <!-- where the text is drawn -->
</div>
…
<span slot="content">Open navigation</span>   <!-- where the text lives -->
```

The mirror was serialising the second one where it *sits*. "Flattening the
shadow tree in place" had been read as *move the light DOM inside the host*,
and composing is not that: a slotted node is drawn at its slot, and everything
the slot is inside applies to it. Emitted beside the slot instead, the text came
out from under the box that hides it — and no CSS rewrite could have helped,
because the box was no longer an ancestor. One capture had 974 empty slots and
five loose captions.

So the serialiser composes. A `<slot>` emits `assignedNodes({flatten: true})` —
flattened, because a slot with nothing assigned draws its own fallback and a
slot assigned another slot resolves through it, and both are what the reader
sees. An open host emits none of its light DOM in place: what a slot claimed is
emitted there, and what no slot claimed is emitted nowhere, because the browser
draws it nowhere. `knownParentId` answers the same way, or the first mutation
after a snapshot would put a slotted node back beside the component and undo the
composition one record at a time.

**Both of those are gone now, and §31 is why.** The selector rewriting and the
composition walk were reconstructions of a boundary the mirror had thrown away,
and the boundary is mirrored instead: a shadow root crosses the link as a node
of its own, the component's sheet is adopted by it, and `:host`, `::part()`,
`::slotted()` and slot assignment all mean what they meant without anything
being rewritten. What is recorded above is what it cost to do without one, which
is the argument for keeping it.

The general rule all four are instances of: what the mirror sends has to be the
document as it is *rendered*, and every one of these was some part of the
pipeline answering for the document as it is *written*. `:defined` is the
sharpest case because it always differs; `:host` is the widest, because on a
site built out of web components it is most of what a component says about
itself; and the slot is the one that no amount of CSS could have repaired,
because it was the tree that was wrong and not the rules.

### 20. A divergence check has to compare two documents, not two instants

§12 fixed the hash. What was left was *when* it is read. The check asked the
agent for the hash of the page now and compared it against the hash the client
had reported for the last frame it acknowledged — two different documents on
anything that changes faster than the link's round trip, which is a news front
page, a feed, a chat, most of the web. Every thirty seconds the mirror was
declared diverged and resynced: a replay if the ring covered it, the whole
document if it did not. The resync then competed with the traffic that had made
the client late, so the check made the condition it was misreading worse — the
same unbounded loop as §12, from the opposite end, and invisible for the same
reason: it looks exactly like a mirror that keeps breaking.

A check now anchors itself. `__skyhook.checkpoint()` drains the observer's
pending records, flushes what the agent is holding, and returns the hash
together with the sequence number of the frame that produces it; `Ack` catches
the client's hash for that same sequence number as it goes past. A client that
never reaches the frame has proved nothing, and the check says nothing rather
than something false. A tab between documents is skipped outright: the agent
hashes an empty document for that moment, and the empty-document hash —
`0x811c9dc5`, the FNV offset basis, hashing nothing at all — was reaching the
comparison and costing a cold snapshot every time a page navigated.

The same lie was in the bundles. `hashesAgree` compared the last acked hash
against a live one and reported "false" for a mirror that was merely a frame
behind; it is now only present when the client had acknowledged the newest
frame, and says `hashesComparable: false` otherwise.

**A sequence number does not identify a frame on its own.** The anchoring above
is only as good as the anchor, and one number is reused: a snapshot restarts a
tab's numbering at zero, so frame 0 means one document before a re-snapshot and
a different one after. When the client's last word was "I have frame 0" and a
snapshot then made frame 0 mean something else, the check found an ack waiting
with the right number and the wrong document, compared it against the new one,
and reported the difference as a divergence — costing a resync of a page that
was fine. Acks still in flight when the snapshot went out do the same thing:
they were sent a round trip ago, about the document being replaced.

Both are closed without a protocol change, because a snapshot is always frame 0
and so the first ack that can belong to the new document is the one that says 0.
Sending a snapshot retires the frame the check anchors to — `acked` and
`lastHash` are cleared — and every ack arriving before the snapshot's own is
dropped rather than credited. `TestASnapshotRetiresTheFrameTheCheckAnchorsTo`
fails against the old code with "the check was answered with a hash from before
the snapshot".

This had been latent since the check was written, and surfaced only once §19's
loading fix stopped masking it. `Loading()` used to be the state of whichever
frame spoke last, so on any page with subframes it was stuck true — and the
integrity check skips a loading tab. Reporting it accurately let the check run
in windows it had been silently sitting out. A guard that works by accident
stops working, and what it was hiding arrives all at once.

### 21. What a capture leaves out is evidence too

Chasing §19 through a real bundle showed the gaps, which were all of one kind:
the bundle recorded what the system did and not what it decided against.

- **The rejected CSS.** A bundle held the rules that passed the used-rule filter
  and nothing about the rest, so a rule dropped in error and a rule the site
  never wrote were the same artifact — nothing. Finding §19 meant inferring the
  filter's behaviour from which neighbouring utility classes happened to
  survive. `css-rejected.txt` is now the other half of that record, capped, with
  the totals in `state.json`.
- **Stylesheets nothing could read.** The other explanation for a missing rule,
  and equally invisible: `agent.json` and `state.json` now carry the blocked and
  recovered counts.
- **What a screenshot is a picture of.** The landside picture is the whole
  scrollable page, or past `MaxShotHeight` only the viewport; the plane-side one
  is the top of the document at its own limit and its own scale. Two pictures of
  one tab over two different regions, with nothing saying so, look exactly like
  a rendering bug. Both halves now write a `screenshot.json` beside the image.
- **Per-node flags.** `fingerprint.json` listed `(id, kind, value)` — precisely
  what the hash covers, which was the point, and precisely why it could not show
  §19: an element that grew a shadow root after it was mirrored agrees on all
  three. The flags are now a fourth column on both sides. Landside they are read
  live, plane-side they are what was sent, so a difference means the client's
  copy is stale rather than wrong.

One bug fell out of writing it: `blockedSheets()` called `cssDelta()` for its
discovery walk and discarded the result — but a walk *records* what it collects
as emitted, so every rule that walk was the first to see was dropped from the
page for good. That is the late-arriving stylesheets, on every load. Everything
that walks the sheets now goes through `emitCSSDelta`.
### 22. A new tab is drawn before it exists, and built off the reader

Opening a tab was the one chrome action that still cost a visible round trip in
both directions, and it was worse than it looked.

Plane-side, pressing "+" only sent a frame. Nothing was drawn until the server's
`TabState` came back — one round trip, seconds on this link — so the button
looked broken and got pressed again. The tab is now drawn immediately with a
provisional (negative) id and a `ref` the server echoes on the frame that names
it; the URL bar is cleared and focused in the same gesture, because that is
where the user was going anyway. When the answer arrives the drawn tab *becomes*
the real one — same strip entry, same mirror frame — and anything the user did
to it meanwhile is replayed under the real id, in order
(`client/src/app/tabs.ts`). The tab costs zero round trips to appear and one,
unavoidably, before its content can start arriving.

Landside, `OpenTab` ran on the connection's single reader: a target creation, a
dozen sequential CDP calls, and a `Page.navigate` that only resolves when the
origin commits. Everything else the user did queued behind it — a keystroke in
another tab, a scroll, an acknowledgement — so opening a tab onto a slow origin
froze the whole browser for as long as that origin took to answer.
`TestOpeningATabDoesNotStallTheRestOfTheBrowser` measured six seconds of it. The
tab is now announced synchronously and built on its own goroutine; frames that
arrive for a tab whose page does not exist yet are deferred and drained in order
when it does, and the page is only published once nothing is left waiting, so
ordering survives.

Two things fell out of the same change. `TabOpen` now carries `background`, so a
middle-click tab no longer takes image priority away from the page the reader
is still on. And the URL bar tracks its tab unless it is *edited*
rather than unless it is *focused* — a new tab focuses it, and the old test
would have left it stuck showing an address the tab had long since left.

The welcome is not the moment the link comes up. The transport reports itself
online and *then* sends `Hello`, so the `Welcome` that answers it is a full
round trip behind — a second and a half of a client that says it is connected,
with an enabled "+" button. A first draft treated every welcome as grounds for
discarding tabs the server had not named, on the reasoning that they belonged to
a connection that was gone. That is right for a reconnect and wrong for the
connection the request was actually sent on: anyone reaching for "+" in that
first round trip lost the tab, and whatever they had typed into it, to the
welcome that followed. Provisional tabs now carry the connection they were asked
for on, and a welcome only discards the ones from earlier connections. Nothing
plane-side made this visible — the netem job did, by failing five PWA tests that
pass at loopback latency.

### 23. A click has to be answered before the page can be

Every affordance a browser shows while a page loads — the bar, the tab spinner,
the address bar going grey — it shows because it started the navigation itself.
Here it did not. A click on a Hacker News story is a semantic event replayed
into a Chromium several seconds away, and the earliest this side can hear that
anything is happening is the tab-state frame landside `frameStartedLoading`
produces, a full round trip later. Until then the client had exactly one thing
to show — a `·` prepended to the tab's title by the tab-state frame that had not
arrived yet — which is to say nothing at all. The most ordinary act in browsing
produced no evidence it had been heard, on the one link where that reassurance
is worth most, and the reader's only recourse was to click again.

The shell now records what it has *asked* for
(`client/src/app/progress.ts`) at the moment the gesture goes out, and draws
three things from it: an indeterminate bar along the top of the mirror, a
spinner in the tab (which is the only one of the three that can speak for a
background tab), and a status line naming the destination in the corner where
browsers put it. The mirror frame gets `cursor: progress` over links, which is
the affordance that appears where the reader is already looking. A tab asked for
and not yet opened gets a placeholder in the strip, for the same reason: a
middle click that was heard and one that was missed used to look identical for a
round trip, and the reader who clicks again gets two tabs.

Three things about it are deliberate:

- **It is an ask, not a claim.** A link a page uses as a button produces exactly
  the same gesture as a link that navigates, and nothing ever comes back to say
  which it was. So every ask carries a deadline — six round trips, floored at 8
  s — and one that nothing answers expires quietly. Six is generous on purpose:
  an ask that expires while its answer is still in flight puts the chrome back
  to idle and then into loading again, which reads as a fault rather than a wait.
- **The server's word wins as soon as it arrives.** A tab-state frame saying the
  tab is loading retires the local ask; from there the tab's own state drives
  everything. Only `loading: true` retires one, because tab-state frames are
  emitted for every reason a tab has to speak and most of them carry
  `loading: false` — one of those in flight when the click went out would
  otherwise cancel a wait that had not begun.
- **Nothing invents a percentage.** How far along a page is cannot be known from
  this end, and a bar filling at a rate nobody measured is a lie told at the
  exact moment the reader is deciding whether to trust the client. The bar
  sweeps; it does not fill.

Nothing is drawn while the link is down: the network worker drops navigate
frames during an outage, so a bar promising a page would be promising something
that was never sent. The HUD's `offline` is the honest affordance for that
state, and any outstanding ask is dropped when the link goes.

`test/loading_test.go` drives it through the real client against a landside page
that takes three seconds to answer, which is the only way to look at a window
that is otherwise a few milliseconds wide on loopback.
### 23. A canvas is pixels, and pixels have to be photographed

Everything else the mirror ships is a description — nodes, text, attributes,
the CSS that was actually used — and descriptions survive the trip because both
ends agree what they mean. A canvas has no description. Its content is whatever
JavaScript painted into it, and page JavaScript is precisely what never runs
plane-side, so the mirrored DOM of a finished game and of a blank one are
identical. Two bug reports arrived on the same afternoon saying a game and a
map "didn't work"; both were canvases, and both mirrored perfectly as an empty
box.

The design called this out (§R11) and the code had half of it: `CaptureRegion`
was written, documented as the canvas fallback, and called from nowhere. It
also had a bug that only running it would have found — the CDP screenshot clip
is in **page** coordinates and it passed the agent's **viewport**-relative
rect, so on any scrolled page it would have photographed whatever happened to
sit that far down the document.

What is built now (`internal/mirror/shot.go`):

- The agent's `shots()` reports the canvas, WebGL and video boxes currently on
  screen, clipped to the viewport, in page coordinates, largest first, capped
  at four. Deliberately not `rect()`: that scrolls an offscreen target into
  view, which is right for a click and would, here, move the page in order to
  photograph a corner of it.
- The host screenshots each box and hands the bytes to the ordinary image
  pipeline (`Request.Src`), which transcodes them like any other picture.
- The key is the content hash of what was painted, so an unchanged canvas
  costs nothing: same pixels, same key, and the client already holds the bytes.
- `ImageMeta.Box` says where the rectangle belongs inside the element, so a
  canvas half off the bottom of the viewport is painted as the half that
  exists rather than stretched over the whole box.
- The client paints it as a `background-image` on the canvas itself, not as a
  substituted `<img>`: the site's CSS sizes, positions and stacks that element,
  and all of it keeps working around a background.

**When a shot is taken** is the part with a real trade in it. A canvas can
repaint sixty times a second without touching the DOM, so there is no signal to
follow and a poll would spend the whole link on frames nobody asked for. The
signal used instead is input: the reader pressed a key or dragged the map,
which is the moment something they caused might have changed. Cost stays
proportional to interaction, and the client keeps its promise of one round trip
per interaction and none when idle.

One shot per input is not enough on its own, because the thing the reader
started takes time to finish: tiles slide, a map eases to a stop, a spinner
runs until the answer lands. A photograph 350 ms in catches that mid-flight and
leaves it frozen there until they touch something else, which reads as a mirror
that stopped updating. So a pass that saw pixels change looks again, and keeps
looking until two passes running find nothing new. The animation is followed to
wherever it settles and then costs nothing, and nobody had to say in advance
how long it was going to take. A run is capped at 24 follow-ups, so a canvas
that never settles stops being followed rather than owning the link, and a run
gives up early when the session's send queues are already deep — an unasked-for
frame of an animation must not delay the thing the reader actually did.

Following a canvas that animates with nobody watching is the one behaviour here
that spends bandwidth on a page the reader is not touching, so it is off until
an operator asks (`canvasStreamEvery`, the design's P2 tile stream; "2s" is
0.5 fps).

**Reaching a canvas at all** is the other half. Every input the mirror replays
names a node — click this, type into that — and a canvas has nothing inside it
to name: no element in a map to click, none in a game board to focus. What a
map understands is a button going down, moving, and coming up, and the distance
between those points is the entire instruction. `InDrag` was in the protocol
and implemented nowhere, the third slot in this file's collection of them; it
now carries a press point in permille of the node's box and a path in permille
of the viewport, the same units a click's approach already used and the only
ones that survive two different layouts. Plane-side the gesture is claimed only
when the press lands on a region — anywhere else a press-move-release is the
reader selecting text, which the mirror does natively and must not lose — and
the click that trails it is swallowed, because landside a pan followed by a
press wherever it ended is two instructions where the reader gave one.

The landside half of the same two reports was a different bug with the same
symptom. A VPS has no GPU, and Chromium no longer falls back to SwiftShader for
WebGL on its own: it blocklists the context, `getContext('webgl')` returns
null, and the site shows its own "something went wrong" — landside, before the
mirror has been asked for anything. The server's screenshot of that error was
faithful. `--enable-unsafe-swiftshader` is what lets the fallback happen; the
sandbox cost it names is the one this project already pays everywhere else,
which is that the landside browser exists to run untrusted code so the plane
side does not have to.

### 24. An asset nothing pushes is an asset asked for exactly once

Shipping icon fonts (§23) put a new kind of asset on the path that stylesheets
use, and that path turned out to have a window in it.

Nothing pushes what a stylesheet names. The server can see a viewport position
for an `<img>` and not for a background image or a webfont, so those wait to be
asked for — and the client asks once per content hash, because asking twice
costs a round trip on a link where round trips are the entire problem. One
request, one answer, or the asset is missing for the rest of the session.

The worker published a finished key into its metadata table, took the list of
clients waiting for it, and only then wrote the bytes to the cache. A request
arriving in between read the cache (miss), joined the waiting list (already
taken) and was answered by neither.

Worth being precise about the evidence: this was found by reading the code
after one full-suite run where the icon font never arrived, on a machine
running ten browsers at once. That failure has not recurred, and it was never
reproduced deliberately — the window is microseconds wide, and closing it is
cheap enough not to need a better reason. It may or may not have been what went
wrong that time.

The cache is now written before the key is published, and `Want` decides
whether a key is finished from the metadata table rather than by looking in the
cache — so under the one lock, "this key is done" and "its bytes are there to
send" are the same statement. The narrowness is the lesson worth keeping: an
asset that is only ever asked for once has no second chance to paper over a
race, and this path has three of those (background images, webfonts, and now
region shots when a push is dropped).

**And an asset pushed once must not be pushed again.** The other half of the
same path had the opposite fault. An above-the-fold picture is shipped unasked
the moment its key is known, and the pipeline ships it again — the whole of it —
every time a snapshot submits that key. A snapshot is submitted on every resync,
so a client that had fallen behind was answered with the repair *and* with every
picture above the fold, in full, down the link that had made it late. The client
needed none of it: images live in a cache keyed by content hash, a resync
neither empties that cache nor clears what the client has already asked for, and
the pictures were still on screen throughout.

The pipeline cannot know that — it is shared by every session and knows keys,
not clients — so the session keeps the ledger instead: the keys this client has
been given. A key on it is not sent again, and `Want` takes it off, because a
client asking for a hash is the one signal that means its own cache no longer
has it. That leaves the failure this must not trade for — a picture withheld
from a client that needs it — resting on a path the client already had and uses.
The dead option it replaced said something similar and did nothing:
`IdleSnapshotAfter` was documented as re-snapshotting a silent page for a client
that asked, and was read by nothing, `planResync` having decided that from ring
coverage since before it was written.

**What the check does while a page acquires frames, measured rather than
assumed.** Chaining the hash across agents (§11f) made one measurement into a
walk: the page, then every mirrored frame, then the sequence number. Three
things about that were worth establishing rather than reasoning about, because
the guess was wrong twice. The walk is cheap — six milliseconds on a page
holding six cross-origin frames, being round trips to agents that are already
running rather than work. A check is paced by the client's acknowledgement, not
by the ticker: it anchors to a sequence number and waits for the client to
report its hash for that one, so on a quiet page a check costs an ack interval,
and at the real cadence of thirty seconds that is invisible. And a page whose
frames are still arriving is checked, concludes, and is not reported as
diverged — `TestTheCheckKeepsWorkingWhileFramesArrive` holds all of that down.

**A walk that spans an arrival describes no document at all.** The walk asks each
agent in turn and reads the sequence number when it has them all, which leaves
two ways to hash something other than what the client holds. A frame that has
been adopted and not yet spliced is a document the client has never been sent,
and hashing it reports a divergence against nodes nobody sent. A frame that
splices *during* the walk has put its document on the wire behind it, so the
sequence number just read counts nodes that none of the hashes covered. Either
way the check says the mirror is wrong when what happened is that a frame
arrived, and the answer to a divergence is the whole document, again, over the
link.

So the walk now visits exactly the frames the client has, and a counter says
whether that set held still: a generation that moved between the first agent and
the sequence number means the measurement describes a document that never
existed, and the check reports no measurement rather than a divergence.

This one was found the honest way round, and only just. It was written down here
first as a known, rare, self-healing wrong answer, because every fixture built to
provoke it deliberately was defeated by how fast the walk is — six milliseconds
on a page holding six frames. It reproduced on the next full-suite run at
`-parallel 8`, where eight browsers make the walk slow enough to span a splice,
and the test that had been passing all along printed exactly what it was written
to print. The lesson is about the fixture rather than the fix: a race that will
not reproduce on an idle machine may only need the machine to be busy, which is
the one condition CI has and a developer's laptop does not.

What also changed is that a measurement which cannot be taken now says so. It
used to return in silence, and a check that quietly does nothing reads exactly
like a check that passed.

### 23. Bookmarks are a navigation surface, not a list that gets written to

The design asks for bookmarks in one clause of R5, beside tabs and the URL bar,
and what was built matched the clause: a star that appended `{title, url}` to an
array in IndexedDB. Nothing ever read the array back. There was no way to see a
saved page, remove one, or tell whether the page on screen was already saved —
so the second click, the one a reader makes because the first produced no
visible change, saved it again.

Read against this link rather than against a browser, the feature is more
important than that clause makes it sound. Every other way of getting somewhere
spends the link: an address costs a page load, and a mistyped one costs two; a
link on the page had to be paid for before it could be clicked. The saved list
is the only navigation surface that is entirely plane-side — reading it,
searching it and rearranging it are free, they work during an outage, and the
single round trip is the one the reader chose. That is worth building four
surfaces on rather than one:

- **The star** is a toggle that says which way it is thrown (`aria-pressed`, a
  filled glyph, a changed title), is idempotent by normalised URL, and reports
  what it did in a toast whose action is the undo. So is removal: no
  confirmation dialog anywhere, because the notice is the way back.
- **The panel** grew a second view beside chat — search, rename in place, remove,
  a per-row menu, and export/import. Rename matters because a bookmark made from
  a mirror link carries the anchor's own text, which is regularly `more` or
  nothing at all. Export matters because this is the one thing on the client
  that exists nowhere else: `Store.wipe()`, a cleared profile or a reinstalled
  PWA all take it.
- **The start page** replaced the blank white frame a new tab used to be. A tab
  that has not been anywhere now shows the saved list, drawn by the shell over
  the frame area rather than inside a mirror — no script runs there, and a
  document the server never sent has no business in a mirror. It reads §22's
  `isBusy` for whether to stand aside, so a tab with a page already on the way
  cannot be offered the list again by a surface that disagrees with the bar
  above it about what is happening.
- **The address bar** completes from the list as the reader types: substring,
  never fuzzy, because a suggestion that is nearly right costs a round trip to
  find out.

Two smaller things fell out of it. A tab opened with `+` is now focused, which
it was not — the server has no opinion about focus and the client never formed
one, so the button silently opened a background tab; that was survivable when a
new tab was blank and is not when it is where the reader goes to start. And the
address bar shows an empty field rather than `about:blank` for a tab that has
not been anywhere, since that string is only ever something to type over.

The list itself is validated on read rather than trusted: the on-disk shape has
already changed once, entries missing ids or timestamps are repaired instead of
dropped, and duplicates an older client wrote are collapsed. It refuses to grow
past 500 rather than evicting somebody's oldest entry to make room — a silent
data loss they would discover on the flight they needed it.

`test/bookmarks_test.go` drives the whole journey in a real browser: an empty
tab offers the list, the star keeps a page, the list survives a reload of the
app, one click brings the page back into the tab in front, and the undo restores
what a removal took.

### 23. The plane-side picture was a picture of something else

Two faults, both found while reading a real bundle of a page whose widget lives
in a frame, and both with the same shape: the picture was wrong in a way that
looked like the mirror being wrong. That is worse than no picture. A bundle is
read by someone who was not there, and a rendering artifact they cannot
distinguish from a mirror bug sends them after a bug that does not exist — which
is precisely what happened, for some hours, to the author of this section.

**Images the stylesheet names were dropped.** An SVG image may not load anything
external, so the rasteriser trades every mirrored image for a data URI first.
It only ever walked `<img>`. Every reference the *stylesheet* carries — the
backgrounded icons, logos and sprites that a widget is mostly made of — stayed a
blob URL, which resolves to nothing inside an SVG, and painted as absence. The
tally of missing images stayed at zero throughout, because it counted elements.
The freeze now carries the host's blob-to-hash map, the `url()` references are
traded in the same pass as the elements, content is served from the byte budget
before decoration, and an unresolved one is counted.

**Inlined frames were flattened.** A same-origin frame's document is inlined
into the mirror as a nested `<html>`/`<body>`, which the patcher builds through
`createElement`. The rasteriser worked from `frame.html` — serialised markup,
parsed back with the HTML parser, which has nowhere to put a second `<html>`. It
drops both, merges their attributes onto the page's own, and promotes the
children; nested stand-ins go with them. In one real bundle that was five
`<html>`/`<body>` pairs gone and three of seven frame stand-ins with them, so
the picture came out of a box tree the reader never had. The freeze now carries
a clone of the document beside the markup, and the picture is rendered from the
clone; a freeze without one still renders, and says so in `NOTES.txt`.

`mirror.html` is unchanged and still the serialised markup, because it is the
artifact a person reads and diffs against `expected.html`. Note that *opening*
it in a browser re-parses it, with the same losses — read it, diff it, but do
not treat it as a rendering.

### 24. A frame that outgrows its box must not swallow its own controls

The stand-in for an inlined frame is given the box the real frame had landside,
because the CSS that sized the original selects on a tag name this element no
longer has. What goes *inside* that box is laid out here, though — by the
reader's browser, in whatever fonts the reader has (Skyhook blocks the page's
own), against a nested `<html>`/`<body>` that is an ordinary block box and not
a viewport, so a percentage height resolves against something else entirely.

Landside the content fitted, so the clipping the stand-in inherited from
`overflow: hidden` only ever engages when *this* side's layout has drifted —
and then it deletes the difference without a word. What sits at the bottom of a
widget is its buttons. The observed case was Google's reCAPTCHA: the challenge
grid laid out taller plane-side than the 400×580 the page gave the frame, and
the footer holding VERIFY/SKIP fell outside the box. The reader gets a captcha
they can solve and cannot submit, with nothing on screen suggesting anything is
missing — not a scrollbar, not a cut edge, nothing.

The stand-in now scrolls. The overflow is still a bug wherever it comes from,
but a scrollbar is a failure the reader can see and get past, and it costs
nothing on the overwhelming majority of frames whose content does fit.

Worth being clear about what this does not do: it does not make the layout
right, and it is not a substitute for finding out why a particular frame comes
out too tall. It converts an invisible, unrecoverable failure into a visible,
recoverable one.

### 25. The mirror was rendering the entire web in quirks mode

A frame at `about:blank` has no doctype, and a document parsed without one is
in quirks mode. The mirror frame was created that way and never given one, so
every page Skyhook has ever shown was laid out under rules none of those pages
were written for. Nothing reported it: quirks mode is not an error, it is a
different and quietly wrong answer, and most of the time the difference is
small enough to read past.

The clause that matters here is percentage heights. Under standards rules a
`height: 100%` whose parent has an auto height computes to auto; in quirks mode
it walks up the ancestors until it finds a definite height and uses that
instead. Google's reCAPTCHA challenge is a table at `height: 100%` inside
containers that are all auto-height, so landside it is content-sized and square
— and in the mirror the percentage reached past all of them to the frame's own
580px box. The table stretched to fill it and its four rows went from 97px to
145px each. The 192px of surplus that appears between the tiles pushes the
challenge's footer outside the frame, and the footer is where VERIFY and SKIP
live: a captcha the reader can solve and cannot submit.

Two things made this hard to see, and both are worth remembering:

- **Every diagnostic path renders in standards mode.** The capture screenshot
  goes through an SVG `foreignObject`, and anyone re-opening `mirror.html` gets
  a file with a doctype. Both produce the correct layout, so the bundle's own
  picture showed a working challenge while the reader was looking at a broken
  one. The frozen DOM and CSS were correct, the two halves' hashes agreed, and
  the artifact disagreed with the reader — which reads as the reader being
  wrong.
- **`compatMode` is fixed when the document is parsed.** Appending a
  DocumentType node afterwards does nothing at all. The document has to be
  re-opened and rewritten, which is what `forceStandardsMode` does.

`srcdoc` would carry the doctype without rewriting anything, and was tried
first. It loses a race with the frame's own initial about:blank and lands the
patcher on a document that is about to be replaced. Re-opening in place is the
boring option that works; its one visible effect is that the frame's URL
becomes the shell's, which changes no resolution because an about:blank frame
had already inherited that same base URL from its creator.

### 26. Loading the app is rejoining a session, not starting a browser

A Skyhook tab is a real Chromium tab on the VPS. It is not a thing the client
owns a copy of and it is not reconstructible plane-side: the client holds a
mirror of what that tab currently shows, in a frame, in a page, and the page is
the most disposable object in the system. A reload throws it away. So does
closing the installed app, a crash, a background tab the OS reclaimed, and a
service worker update.

None of that touches the tabs. They are landside, they kept loading while the
app was gone, and they are still logged in. The session that owns them lives for
12 hours without a client — which was designed for a flight, and is exactly as
right for the ten seconds between one page load and the next.

The client did not ask for it back. It stored the session id from the Welcome
that named it (`store.writeSessionId`) and never once read it back, so every
load introduced itself as a stranger, was given a fresh and empty session
(`Manager.resolve`), and left the reader's tabs running landside with nothing
able to reach them. What the reader saw was an empty strip and a blank frame.
What it cost was every page in that session, re-fetched over a link where a page
is measured in seconds — and a Chromium per orphaned session, burning landside
until the TTL collected it.

The shell now reads the stored id at startup and hands it to the network worker
with the pairing, and the worker offers it in its first Hello. It adopts one
only when it holds none of its own: a worker that has already been welcomed into
a session knows the newer id, and the stored one is what that replaced. The
server's side of the handshake needed nothing — `Hello.sessionId` and the tab
list in `Welcome` were always there, and `resolve` had always been willing to
hand a session back to whoever named it.

Getting the strip back is the visible half. Three things behind it are what make
the tabs usable rather than decorative:

- **The frames have to fill.** A cold client holds no document for any tab and
  no diff can build one, so the worker asks for a snapshot of every tab the
  Welcome names. That was already the behaviour on reconnect; it now runs on the
  path that matters most, where *every* tab is cold.
- **Welcome does not carry history.** A `TabRef` is a URL, a title and a
  sequence number. Whether a tab can go back is the browser's answer and no
  client that has just loaded can know it, so the toolbar sat disabled over a
  tab with ten pages behind it until the reader spent a navigation finding out
  otherwise. A resumed session now gets a `TabState` per tab
  (`Session.RefreshTabs`) immediately behind the Welcome, which is a few dozen
  bytes for something already known landside.
- **Tab order is muscle memory.** `TabRefs` ranged a map, so the strip came back
  shuffled — different on every reload. It is sorted by tab id, which is the
  order they were opened in and the order the strip was already in.

The reconcile in the other direction is the same fix seen from the other end: a
Welcome that is *not* a resume — the server was restarted under a reconnect —
means the tabs this side is holding no longer exist, and a strip of tabs that
cannot be reached is worse than a short one, because every click on one goes to
a tab id the server will refuse.

`test/resume_test.go` drives the whole of it through the real client: two tabs,
one of them with a page behind the page it is on, then the app loaded again with
no pairing fragment — the installed PWA being started rather than paired. Both
tabs come back, in order, with their documents in them, in the one session that
was there before; the active tab is the one on screen, and the tab with history
knows it has it. Without the fix the test fails with the symptom it was written
for: `+ ← → ↻ ★ Chat` and nothing else.

### 27. The chrome was drawn for a mouse on a wide screen

The client is a PWA and a PWA installs on a phone, which is the device most
likely to be the only one somebody has out at 35,000 feet. What it installed
was a desktop browser's chrome, scaled to nothing.

Measured on a 393-pixel screen, before any of this: the toolbar wanted 600
pixels and got 393, so the star was clipped, the saved-pages button was cut off
mid-word, and the Chat button and the entire HUD — the link state, the round
trip, the queue depth, the bytes spent, which is the instrument this whole
product is built around — were off the right-hand edge with no way to scroll to
them. Past three tabs the strip pushed its own "+" off the end, so a browser
whose tabs are its session had no way to open one. The side panel is 380 pixels
wide; opening it on a 393-pixel screen scrolled the whole document sideways and
took the chrome with it. Every control was 27 to 34 pixels tall against a
44-pixel guideline, and the tab close button was 15 by 18. The context menu —
the only route to "open in a new tab", "copy link address" and "report a
rendering problem" — was drawn at a mouse position, offered 30-pixel rows, and
hinted `Ctrl+D` and `Middle click` at a device with neither.

And under all of it, `viewport.mobile` was hard-coded `false`. The flag has been
in the wire format and honoured by `Tab.SetViewport` since the beginning; the
client never set it. So a phone reader got every site's *desktop* layout, laid
out landside at 393 pixels because that is what the client reported, and
mirrored at that scale. That is the one failure this client cannot pinch-zoom
its way out of, and it was a one-line fix behind everything else.

The shell now has two forms. They share every line of behaviour and differ in
what is on screen:

- **The chrome is one row instead of two**, which is 56 pixels rather than 88 —
  a tenth of the page back on a phone held sideways. The strip goes, and back,
  forward and reload go with it: the system back gesture is already caught and
  spent on the tab's own history ([§14](#14-the-browsers-own-back-and-forward-drive-the-tab-not-the-shell)),
  and all three are in the ⋯ menu, one tap away and not a tap made on every page.
  Reload came back out of that menu in
  [§66](#66-the-reload-gesture-the-phone-shell-was-refusing-to-the-browser) —
  not onto the toolbar, but onto the gesture a phone already keeps it on.
- **The tabs become a list** behind a count in the toolbar, the way every phone
  browser has done it for fifteen years. The count spins when a tab that is not
  on screen is still fetching, which with the strip gone is the only thing that
  says a background tab is doing anything. "New tab" is a row in that list and
  cannot scroll off it.
- **The HUD collapses to its colour** — a dot, tappable for the three numbers it
  drops — and spells itself back out when the answer is *offline* or *slow*,
  which are exactly the states where the reader is about to waste a gesture.
- **The panel and the menu become sheets**, up from the bottom where the thumb
  is, dismissed by touching the page they cover.
- **Nothing worth hitting is under 44 pixels**, and no entry hints a chord at a
  device with no keys: `MenuItem.chord` is a gesture and is hidden from a coarse
  pointer, while `MenuItem.hint` is what the entry *costs* and is shown
  everywhere, because a phone reader is on the same link as everyone else.
- **The address bar drops the scheme** while nobody is typing in it and puts the
  full URL back on focus, so what is edited and sent is always the address and
  never a description of it. Enter blurs it, which sends the on-screen keyboard
  away from the page that is arriving.

Two things it does not do, deliberately. It does not report `mobile` for a touch
laptop or a narrow desktop window — both halves of the question have to be true,
because a desktop screen wants the desktop web. And `interactive-widget` is
pinned to `resizes-visual` in the viewport meta rather than left to the browser
default: the other behaviour resizes the layout viewport when the keyboard
opens, which this shell answers by reporting a new viewport, which the server
answers by laying the real page out again and sending a fresh snapshot. Tapping
into a search box would cost a page.

The wide shell is untouched, and that is a property rather than a hope: every
rule is inside `@media (max-width: 600px)` or `(max-height: 500px)` for layout,
or `(pointer: coarse)` for target sizes, and the end-to-end suite — which drives
the real PWA at 1280x900 — measures the same chrome, byte for byte, as before.

The loop that produced it also found a bug that was never about phones. The
status line named where a page was coming from by falling back to the tab's own
URL once the server confirmed the navigation had started — and between that
confirmation and the commit, which is seconds on this link, the tab's own URL is
still the page being *left*. So the one affordance that answers "where am I
going" spent the whole wait naming the one page the reader was definitely not
going to. `Progress` now keeps the destination for as long as the tab has not
got there ([progress.ts](../client/src/app/progress.ts)), and the label is drawn
from somewhere this side actually asked to go or from nothing at all.

### 28. A deploy was replacing the shell one file at a time

The mobile chrome from [§27](#27-the-chrome-was-drawn-for-a-mouse-on-a-wide-screen)
shipped, and an Android phone kept drawing the desktop one. Not because a media
query failed — because the phone was running the new `index.html` against the
previous `app.css`. The new markup's two extra buttons were on screen, styled by
a stylesheet that has no rules for them, inside a chrome sized for a mouse.

Three things had to be true at once, and were:

- **The cache name was a constant.** `skyhook-shell-v1`, hard-coded, so every
  build shared one cache and `activate` deliberately kept it.
- **The worker never changed.** A browser decides whether to install a new
  service worker by byte-comparing the script, and `service-worker.ts` had no
  reason to differ between builds — so a deploy that changed every other file in
  the shell never triggered an install, and the precache never re-ran.
- **The shell refreshed per file, in the background.** `serveShell` returned the
  cached copy and re-fetched it "so the next start is current" — each file on
  its own, whenever it happened to be asked for. There was no moment at which
  the cache held one build.

So the steady state after a deploy was a shell made of two of them, for as long
as it took each file to be requested. The build had been emitting a
`dist/version.json` since the beginning, with a comment saying it was "a build
stamp the service worker can key its cache on, so a deploy does not serve half
of one version and half of another". Nothing read it.

The cache is now named after a hash of the files it holds. `esbuild.mjs` builds
the app and the network worker, copies the static files, hashes that set, and
builds the service worker last with the id substituted in — so the worker's
bytes differ exactly when the shell differs, which is what makes the browser
install it, which is what creates a new cache, which `install` fills completely
before `activate` deletes every older generation. The background refresh is
gone: with a whole-generation swap it has nothing left to do and was the thing
mixing the generations in the first place.

The id is content-addressed rather than a timestamp or a version number, for two
reasons. A rebuild of identical sources produces an identical id, so redeploying
the same bytes does not evict a phone's cache or make it re-fetch a shell it
already holds. And nothing has to be remembered: a hand-maintained version's
failure mode is sitting at `v1` through every deploy, which is what happened.

`TestPWAKeysItsShellCacheToTheBuildItServes` asserts the property rather than
the mechanism — the worker running on the client keys its cache on exactly the
build the server is serving, that cache is the only shell generation left, and
it holds every precached file. Against the old worker it fails with
`shell caches = [skyhook-shell-v1], want exactly [skyhook-shell-<id>]`.

### 29. A url() token has to be read, and a rule has to close itself

A mirrored Gmail arrived as bare markup — no layout, icons inflated to the width
of the window, a heading where the logo should be. The DOM was not at fault: the
capture's two halves agreed on the hash, and the markup matched landside node
for node. The stylesheet had crossed the link too, all 293 KB of it. Chromium
was parsing 649 rules out of it and discarding 2,773.

Gmail's own templating leaves an unsubstituted variable inside a url string:

```css
background-image:url("//ssl.gstatic.com/…/var(--hub-nav-container-button-icon-asset)_1x.png")
```

which is legal — a *quoted* url token may hold a bracket. The pattern that
rewrote page URLs into image keys could not:

```
url\(\s*['"]?([^'")]+)['"]?\s*\)
```

`[^'")]+` ends the address at the first bracket, so the rewrite consumed two
thirds of the token and left `_1x.png")` in the sheet as text. The orphaned
quote opened a string that ran through the rule's closing brace and the `@media`
block after it, and everything past that byte stopped being CSS. Eighteen bytes
of one background-image cost the page four fifths of its styling.

Two things came out of it, because the second is what makes the first survivable.

**Read the token.** `replaceCSSURLs` scans instead of matching: a quoted token
ends at its closing quote, an unquoted one at the first unescaped bracket, and
whichever it was, the address is handed back through `cssURLToken`, which quotes
it when the address needs quoting rather than when the page happened to. Scanning
settles the quieter half of the same bug too — a `url(` inside a string is text
the page means to display, and no pattern can tell that from a reference.

**Check what comes out.** A bundle is rules joined end to end, so a rule that
cannot close itself never fails alone. That had already happened once, from a
different direction: a custom property stripped out from under its selector,
which is why `stripUnusedVars` drops a rule whose declarations all go rather than
emit a selector with nothing to close it. Two doors into one failure is enough to
stop guarding the doors, so `wellFormedRule` now runs over every rule after the
last transform that can touch it, and `dropMalformed` drops the ones whose braces
or quotes do not end. One rule lost against every rule after it is not a close
call.

`TestABracketInAURLDoesNotTruncateTheSheet` puts a bracketed url() above an
ordinary rule in the fixture and checks the ordinary rule, which is the symptom a
reader would report. Against the old pattern it fails with the same wreckage the
capture held: `#bracket-url{background-image:url(skyhook://img/44fee7e3).png");}`.

### 30. The mirror's own wrapper was breaking every full-height layout

Google Chat mirrored as a header bar, the word "Shortcuts", and eight hundred
pixels of white. The DOM was perfect: 1238 nodes on both sides, hashes agreeing,
every string of text present in `expected.html` — "Welcome, benson", the
shortcut list, the three empty-state cards — and none of it on screen. A capture
that says the two documents are identical and the reader saying the page is
blank are both true at once only if the difference is in the box tree, and the
box tree is the one thing neither side hashes.

The patcher builds a snapshot detached and swaps it in whole, which is right: a
resync that appends a thousand nodes into the live document lays the page out a
thousand times and the reader watches their page empty itself and refill. What
was wrong was the container. It was a `<div>`, and it went into the document
with the tree still inside it.

A `<div>` is a box, and its height is auto. The mirrored root is the page's own
`<html>`, so the whole of a page's full-height layout resolves *through* that
box:

    html, body { height: 100% }      <- the page's own rule; 881px, correct
      div (skyhook's)                <- auto
        html                         <- 100% of auto = auto
          body                       <- auto
            .app { height: 100% }    <- auto, and so is everything under it

One box, and every `height: 100%` below it collapses to its content. That is not
an unusual pattern to break — it is how essentially every application on the web
says "fill the window". Gmail, Chat, Docs, Slack.

This had been true since the patcher was written and hurt nothing until
[§25](#25-the-mirror-was-rendering-the-entire-web-in-quirks-mode). Quirks mode's
percentage-height rule walks up the ancestors until it finds a definite height,
so it walked straight past the wrapper to the frame's own viewport and got very
nearly the right answer for entirely the wrong reason. Fixing the compatibility
mode was correct and made the reCAPTCHA usable; it also removed the accident
that had been hiding this, which is worth stating plainly because "the fix
exposed a second bug" is the shape a regression report takes when nobody knows
about the first one.

The container is now a `DocumentFragment`. It holds a detached tree exactly as
well and generates no box at all, so the page's root becomes a child of the
frame's body and the chain is the one the page was written for. There is no
replacement wrapper and no compensating stylesheet rule: the fix is that
Skyhook stops putting anything in the middle.

`TestPWAKeepsTheDocumentRootDirectlyInTheBody` asserts both halves — that
nothing sits between the frame's body and the page's root, and that a fixture
whose only definite height is the viewport reaches it. Against the wrapper it
fails with `#main came out 18px in a 725px frame, wanted about 685px`.

### 31. A frame the page starts is not the page still loading

The same capture had the tab flagged `loading` sixteen minutes after
`readyState` went to `complete`. `Page.frameStartedLoading` and
`Page.frameStoppedLoading` fire for every frame in a tab and carry the
`frameId` that says which; Skyhook subscribed to both and discarded the
parameters, so the tab's loading flag was the state of whichever frame spoke
last.

Google's sites make that the ordinary case. chat.google.com finishes, and then
injects a cookie-rotation frame and a contact hovercard — and over a link with a
second of latency, something is always still in flight. The reader gets a
spinner in the tab strip and a progress cursor over every link on a page that is
finished and interactive.

The other direction is the more expensive one. A subframe that finishes while
the page is still coming clears the flag early, so the shell says the page has
arrived when it has not. That is the reassurance
[§22](#22-a-click-has-to-be-answered-before-the-page-can-be) exists to provide,
and a false one is worse than none.

Both handlers now compare the `frameId` against the tab's main frame, which is
the distinction `onFrameNavigated` was already making with the same field.
`Page.loadEventFired` is main-frame only and needed no change.

### 32. An optimistic echo has to land where the message will

Reading the same capture for what else assumed a chat app looks a particular
way: the ghost message — the local echo that appears the instant Enter is
pressed, seconds before the server can confirm it — was placed in the first
element matching `[role="list"], ul, ol` anywhere in the document.

On Google Chat every one of the ten `role="list"` elements on the home screen
is in the left rail: the direct messages, the spaces, the apps. The reader's own
message would have appeared in the sidebar, under their list of conversations.
An echo in the wrong place is worse than no echo: it is not reassurance, it is a
second thing to be confused by, and the real message arriving elsewhere a moment
later does not explain it.

The search now runs outwards from the composer instead of down from the
document, which gets it right for the reason the layout is that way. A composer
sits inside the conversation it writes to, and so does that conversation's
transcript; the navigation is somewhere else. So the first ancestor whose
subtree contains a list at all is the conversation pane. The walk still ends at
the body, which is what keeps a plain message board — where the body *is* the
container — working as before.

### 31. Keeping the boundary instead of reconstructing it

Three of the fixes above — the shadow-scoped selector rewrite, the slot
composition, and the used-CSS filter's per-root testing — are the same work
done three times: rebuilding, by hand and approximately, a boundary the mirror
had already thrown away. This records why the boundary should be kept instead,
and what it costs, because the measurements were worth more than the argument.

**What flattening still gets wrong, and cannot stop getting wrong.** A shadow
sheet's rules are hoisted into one document-level stylesheet. Rewriting fixes
the rules that *name* the boundary (`:host`, `::part()`); it can do nothing
about the rules that merely relied on it. In one capture of Reddit's front page,
**77 of 881 shipped rule groups select on a bare tag name alone** — `label`,
`textarea`, `ul`, `svg`, `rpl-popper`, `faceplate-menu`. Landside, `label {
display: flex; position: relative }` was scoped to one text input's shadow root.
Plane-side it is in the common sheet and reaches every label on the page. The
same is true of a same-origin iframe's sheet, which is hoisted the same way and
whose `body { … }` matches the outer body. `::slotted()` and `exportparts`
renaming stay unimplemented for the same reason: there is no boundary left for
them to mean anything against.

**Measured, in the browser the client actually runs in** (Chromium 151,
`sandbox="allow-same-origin"` with no `allow-scripts`, driven from the parent
realm the way the patcher drives it):

| Question | Answer |
| --- | --- |
| Can the client build a root inside the sandboxed frame? | Yes, and page JavaScript still never runs |
| Does the boundary actually scope? | A document rule measured 13px outside a root and 0px inside |
| Cost of 1000 roots — Reddit's order | 9 ms to build vs 7 ms flat; 16 ms layout vs 13 ms |
| One sheet shared by 1000 roots? | Yes, via `adoptedStyleSheets` |
| Does a serialised root survive re-parsing? | Yes — `parseHTMLUnsafe`, `setHTMLUnsafe` and `getHTML({serializableShadowRoots:true})` all round-trip |
| Protocol headroom | Node kinds mirror `nodeType` (1, 3, 8, 10); **11 is DocumentFragment and free** |

Performance is not the objection it looks like, and the capture artifact gets
better rather than worse: §23's re-parse damage is partly repaired, because a
declarative root survives the parser even though the nested `<html>`/`<body>`
wrappers inside it still do not.

**What it costs, including the parts that are not obvious:**

- **Constructed stylesheets are realm-bound.** A `CSSStyleSheet` built in the
  parent realm is refused by the frame's root — `NotAllowedError: Sharing
  constructed style...`. It has to be built from the frame's own
  `contentWindow.CSSStyleSheet`.
- **Non-composed events do not cross a boundary.** Measured: a `scroll` from
  inside a root reaches a listener on the root and never reaches one on the
  document. The client listens for `scroll` on the document, so scroll telemetry
  — which is most of what keeps a reader's place — goes silent for any scroller
  inside a component unless a listener is registered per root.
- **Composed events retarget.** On a real browser-generated `focusin`, `target`
  reports the host and `composedPath()[0]` the true node. The client reads
  `ev.target` in about seven places; every one of them has to change.
- **CSS delivery has to gain a scope.** `Snapshot.CSS` is a flat `[]string` with
  no owner. Per-root sheets need the rules to say which root they belong to,
  which is a protocol change and a conformance-fixture change — CBOR's
  `keyasint,omitempty` keeps it backward compatible, but it is not free.
- **Scoping is not isolation.** Inherited properties — `color`, `font` — cross
  the boundary as they do anywhere else.

**Both halves are done.** Frames first, components second, and the second was
much the smaller change because the first had found everything. What components
added was the boundary for every shadow host rather than only for a frame; what
they *removed* was larger — the selector rewriting, the `:host`/`::part()`
re-pointing, the slot composition walk and the `knownParentId` special case all
went, because each existed only to stand in for the boundary. `::slotted()` and
`exportparts`, both listed under known gaps for want of anything to mean, now
work because there is something for them to mean.

**The order.** Iframes first. An inlined frame is one root per frame rather than
a thousand per page, it exercises every part of the mechanism — the node kind,
`attachShadow`, frame-realm sheet construction, `composedPath` input routing,
serialisation in the capture — and it independently closes the iframe half of
the leak. Components second, where the seventy-seven rules are.

**What the first increment cost, which is the argument for doing it first.**
Three of the four things that broke were not on the list above:

- **`instanceof` does not cross a realm.** The patcher builds nodes with the
  mirror frame's document, so a host element belongs to that frame's realm and
  `host instanceof Element` — `Element` being the shell's — is false for every
  host there is. Every frame arrived as an empty box, and jsdom, where the unit
  tests share one realm, said it was fine. `nodeType` is a number and does not
  care whose realm it came from.
- **A shadow root is attached, not inserted.** `appendChild` on one throws, and
  a throw inside the snapshot loop takes the rest of the document with it.
- **The client's own compensating CSS stops at the boundary like anyone's.**
  `[data-skyhook-tag="iframe"] html, body { display: block }` was a document
  rule about nodes that are now inside a root. It is adopted into each root
  instead.
- **Neither `cloneNode` nor `importNode` carries a root, and `XMLSerializer`
  does not write one.** A capture's clone and the picture rendered from it both
  lost the frame. The freeze flattens roots into the clone on the way out —
  rules as a `<style>`, content as ordinary children — because an artifact
  nobody interacts with loses nothing by being flat, and losing the frame
  entirely is not a trade.

Six end-to-end tests reached into frame content with document-level queries and
had to learn to descend through the root. That is not a cost of the change so
much as a measure of it: every one of them was written against a tree where the
boundary did not exist.


### 35. The capture drew the collapse the reader did not have

[§30](#30-the-mirrors-own-wrapper-was-breaking-every-full-height-layout) took a
wrapper `<div>` out from under the mirrored root, because an auto-height box in
that position collapses every `height: 100%` beneath it. The rasteriser has a
wrapper of its own, and it was still there.

`screenshot()` serialises the frozen mirror into an SVG `foreignObject`, and
that needs one element to serialise — so the document's children are moved into
a `<div>` carrying the page's background and default font. That div is the
frame's `<body>` as far as everything below it can tell, and it was given a
width and no height. The same collapse, one layer along, surviving the fix
because it is separate code that happens to make the same mistake.

What made it expensive is the direction it lies in. §25 recorded the trap where
every diagnostic path rendered in standards mode, so the bundle's own picture
showed a working reCAPTCHA to someone looking at a broken one. This is that trap
inverted: Chat had been fixed, the reader's screen was fine, and the capture
reported a header and a screenful of white. Two captures and a long
investigation went into a mirror that was already correct — the DOM agreed, the
1251-node fingerprints agreed row for row, the 2592 stylesheet rules agreed but
for three `skyhook://` to `blob:` image rewrites, and laying the client's own
serialised document out reproduced the page perfectly. Everything the bundle
contained was healthy; only the bundle's picture was not.

The wrapper is now given the height it stands for — the frame's viewport, which
is the box the document really resolves against when the reader is looking at
it. A `DocumentFragment`, which is what fixed the patcher, is no use here
because the markup has to be serialised.

The page's white moved from the wrapper to a `<rect>` under the
`foreignObject`. A wrapper sized to the viewport no longer covers a document
taller than one, and a shot of a long page fading to transparent below the fold
would have been a new way to lie about the same thing.

`TestPWACapturePicturesAFullHeightPage` asserts it, and the assertion has to be
about *where* the ink is. `hasInk` cannot see this failure at all: the header
renders either way, so the picture is never one flat colour. The fixture anchors
a dark bar to the bottom of a full-height layout — somewhere it can only be if
the chain survived — and the test looks for it in the bottom eighth of the shot.
Against the old wrapper it fails with the bar missing from a 90px strip that is
nothing but white.


### 36. A mutex that only guarded half the way in

`Model` has carried a mutex, and a comment saying exactly what it is for: a
replica is written by whichever goroutine feeds it frames and read by whoever is
asking what the page says, and walking `Nodes` while a mutation inserts into it
is a "concurrent map read and map write" — not a test failure but a runtime
fatal that takes the whole suite down, in whichever test happened to be running.

The lock was real and the methods took it. What leaked was everything around
them. `Nodes`, `CSS`, `Scoped`, `URL`, `Title` and `Seq` are exported fields, so
callers indexed and ranged over them directly — fifteen sites across the
end-to-end suite, plus the capture path in the client library and two in
`skyhookctl`. Worse, `Find` and `FindByText` took the lock, found a node, and
returned a `*ModelNode` **pointer into the live replica**; the caller then read
`Attrs` and `Children` after the lock was released. The race was moved one line
further out and made invisible.

`go test -race` reported six of them across three tests. None of this was ever
going to show up in CI, because the end-to-end package is the one package the
suite does not run under `-race` — it drives real browsers and the detector's
slowdown does not fit the budget. Confirmed pre-existing rather than introduced:
a control run on an untouched `e8813d7` produces five races and two failures
with none of the branch's commits present.

The accessors now hand back copies — `Node`, `ChildText`, `CSSRules`,
`ScopedRules`, `Meta` — and `Find`/`FindByText` clone before returning.
`EachNode` exists for the walks that genuinely want every node, and calls back
under the read lock with a pointer the caller is told not to retain. Copying is
affordable precisely because these are test and tooling accessors: they answer a
question about one node, at human speed, beside a link that costs a second.

The same three tests now report zero races.

This is worth keeping in mind next to §30 and §35: a guard that is correct in
the middle and open at both ends is the shape all three of those bugs share.

## Known gaps

These are unbuilt or thin, and are honest to-dos rather than deviations.
Every gap here — and every gap found since — carries an id in the registry at
`test/parity/gaps.json`, most with a corpus page that *measures* it, so the
prose below is the explanation and [PARITY.md](PARITY.md) is the ledger:

- **A canvas that animates unprompted is not followed by default.** See
  [§23](#23-a-canvas-is-pixels-and-pixels-have-to-be-photographed): an
  animation the reader started is followed until it settles, but a clock or an
  idle game loop needs `canvasStreamEvery` turned on, and that spends the link
  on a page nobody is touching.
- **A font over the cap is cut only when its icons can be named.** §57: a family
  refused at the 1 MB cap is subset to the ligature names the page's own markup
  draws, which covers the icon fonts this was written for. A family drawn in
  private-use codepoints instead (§23), a CFF font, or one whose icons are
  wanted at a weight other than the default still crosses whole or not at all —
  and under the cap nothing is cut, so a page using six glyphs of a 900 KB
  family still pays for all of them.
- **An icon font registered through the FontFace API cannot be shipped at all.**
  §48: a family added with `document.fonts.add(new FontFace(…))` appears in no
  stylesheet, and `FontFace` exposes no `src`, so there is no url() to rewrite
  and nothing to fetch. Google Chat builds one of its four icon families this
  way and its glyphs arrive as their ligature names.
- **`wheel` and `hover` are protocol surface nothing sends.** Both are replayed
  landside (`Tab.wheel`, `Tab.hover`) and no client has ever emitted one: the
  mirror scrolls plane-side and reports where it got to, and hover is the
  reader's own (§35's media-feature reasoning). They are kept because the
  protocol is versioned, not because they work.
- **The landside browser is a phone with a mouse.** `SetViewport` passes
  `mobile` to `Emulation.setDeviceMetricsOverride` and never enables touch
  emulation, so landside `navigator.maxTouchPoints` is 0 and a page branching
  on touch in *script* builds its mouse interaction model. That is currently
  self-consistent — §49's gestures are replayed as real mouse events, which is
  what such a page is listening for — and it is why enabling touch landside is
  not a one-line change: it would have to arrive with touch input to feed it.
- **A second adapter** (design P2) is not built.
- **File upload** (R10) is not implemented. Clipboard integration is the mirror's
  native selection plus cut/copy/paste on the context menu; copy still executes
  plane-side, so the cross-tab paste fidelity §2.6 wants from a landside copy is
  not there.
- **Find-in-page** works through Blink natively in the mirror, but there is no
  chrome-UI affordance for it yet.
- **The chat adapter's selectors are validated against one conversation, not
  against Chat.** §54 checked them against a real direct message from a reader's
  capture, which is what turned four wrong ones up; a space with several people
  in it, a thread, and Chat inside Gmail have still never been read. The unread
  count in particular hangs off a minified class because Chat gives its badge no
  role, label or data attribute.
- **0-RTT resumption** is enabled but not asserted by a test; proving it needs a
  client that survives process restart, which the Go test client does not model.
- **Bookmarks are per-device and stay there.** The list is plane-side only, with
  no server copy and no sync between two paired browsers; export and import are
  the whole of the story. Reordering is by use rather than by hand — there are
  no folders and no manual sort.
- **History has no surface of its own.** It completes addresses and it can be
  forgotten a row or a list at a time (§34), but there is nowhere to browse it:
  no panel, no search over it, no grouping by day, and no export. It is also
  per-device, with no sync and no server copy, for the same reasons the saved
  list is.
- **Installability is untested against a real install prompt**: the manifest,
  icons and service worker are all in place and the worker registers in a real
  browser under test, but nobody has clicked "Install" on a device yet. It also
  needs a certificate the browser trusts, which [§39](#39-the-certificate-was-a-choice-between-the-app-and-the-transport)
  now makes reachable without a proxy — and the ACME path itself is exercised
  only against a stand-in authority in `internal/transport/acme_test.go`, never
  against Let's Encrypt, because a test that issues real certificates spends a
  real rate limit.
- **A frame is only preemptible between messages.** The outbound scheduler is
  fair between tabs and strict between channels, and both only get to choose at
  message boundaries: a 200 kB snapshot handed to the socket occupies the link
  for the six seconds it takes at 250 kbps, and a close arriving in second two
  cannot take those bytes back. Capture artifacts are chunked at 32 kB for
  exactly this reason ([§16](#16-diagnosing-the-split-renderer-needs-both-halves-at-the-same-instant));
  dom frames are not, and chunking them means the patcher learning to apply half
  a document, which the intern table makes more than a transport change. Until
  then the honest bound on "kill a tab and get the link back" is one message.
- **The document is delivered whole, not viewport-first.** A snapshot serialises
  the entire DOM, and only images are prioritised by viewport position. Both
  Menlo's Smart DOM and, twenty years earlier, OBML's pagination send what is
  visible first and stream the rest — on a long document over this link that is
  the largest remaining win. See [PRIOR-ART.md](PRIOR-ART.md).
- **A closed shadow root is invisible.** `attachShadow({mode: 'closed'})` cannot
  be read from an isolated world any more than it can from the page, so such a
  component mirrors as its light DOM and nothing else. Late-attached *open*
  roots are handled — by the upgrade watch when a custom element brings one
  (§19), and by the sweep when any other element does (§11d); closed ones cannot
  be.

### 33. A client that runs from its own cache cannot see that it is old

[§28](#28-a-deploy-was-replacing-the-shell-one-file-at-a-time) made a deploy
swap the shell whole instead of a file at a time. It left the larger half of the
problem standing: the swap only happens when the browser goes and looks, and
nothing plane-side had any way of knowing it had not.

The client is a PWA whose service worker answers every request for the app out
of the cache it filled at its last upgrade. That is the point of it — the thing
has to start with no network at all — and the consequence is that the app cannot
find out it is stale by asking. Fetching `index.html` returns the cached one.
Fetching `version.json` returned the cached one, and worse: the file was being
cached per generation, so between a deploy landing and the worker catching up an
old shell could cache the *new* stamp and conclude it was current. The advice
the app itself gave on a version refusal — "reload the page to pick up the
version the server is serving" — was a reload served from that cache, which came
back as the same build and was refused again.

None of this is visible. There is no error, no banner, no slow path. A reader
opens the app at 35,000 feet on a build the server replaced three weeks ago and
meets bugs that were fixed in the interval, and the only symptom is the bug.

Both halves now state what they are, over the one channel a cache does not sit
in front of: the live connection.

- **The app's build id is compiled into its bytes.** `esbuild.mjs` already
  hashed the shell to name the worker's cache; it now builds the app and the
  network worker twice — once unstamped, to hash, and again with the id
  substituted in. Hashing the unstamped output is what keeps the id honest: it
  changes when the sources change and not because the previous build had a
  different id, which would be a value that changed every time and meant
  nothing.
- **The server states the build it serves.** It reads `version.json` from its
  web root — re-reading it when a deploy replaces it, since a stamp read once at
  boot would describe an app that is no longer there — and names it in every
  `Welcome`, alongside its own version.
- **Neither is enforced.** The protocol version already says whether the two
  halves can talk, and closes the connection when they cannot. A build
  difference is not that: the wire format is the same, everything works, and
  refusing over it would strand a reader mid-flight over a patch release.

Where they differ the client says so once and offers the update. Pressing it
re-fetches the worker, which installs the new generation whole, takes the page
over, and only then reloads — an order that matters more than it looks. Reload
first, which is the obvious implementation, and the page comes back from the
cache that is still in charge, as the build it already was. That failure is
indistinguishable from a button that does nothing, and it is invisible to any
test that does not run a real service worker.

Nothing updates on its own. The download is the whole app over a link that
charges seconds for it, and this client does not spend the link without being
asked — the same rule that governs captures and prefetch.

The build ids also settle a question every diagnostic bundle used to beg: which
plane-side build drew the document in it. A capture now records the client's
build, the build the server was serving, and the server's own version, because
a mirror bug is half plane-side and the patcher in the bundle may not be the
patcher in the tree.

`TestPWAIsToldWhenTheServerHasANewerBuild` deploys a new stamp under a running
client and asserts it is told; `TestPWAUpdatesItselfOntoTheServersBuild` deploys
a genuinely different build, presses the button, and asserts the app comes back
as the one the server serves — with the reload-does-nothing premise asserted in
between, since the whole mechanism is worthless if a reload had been enough.

### 34. The address bar was completing from the wrong list

[§23](#23-bookmarks-are-a-navigation-surface-not-a-list-that-gets-written-to)
made the saved list a navigation surface and hung address-bar completion off it.
`suggest.ts` said in its own header that this was *deliberately* not
history-backed, on two grounds: that a fuzzy match offering a page the reader did
not mean costs a round trip to find out, and that the client had no history store
to complete from anyway.

The first ground was right and still is. The second was a fact about the code,
not an argument, and it was doing the work of one. Nobody stars the site they
open every morning — they type four letters of it and press Enter — so the list
the address bar was completing from was, almost by construction, the one list
that does not contain the addresses people actually re-type. On this link that
is the expensive gap: typing a whole address is the costliest way to navigate,
and a typo in one costs a second page load to discover.

So there is a history store now (`history.ts`), and what the old note was
protecting survives intact: matching is substring and never fuzzy, and nothing is
ever written into the field on the reader's behalf. Chrome's inline
autocompletion — filling in the rest of the address and selecting it — was
considered and refused for the same reason the file already refuses to touch the
field as the highlight moves: a fast Enter over a wrong completion is several
seconds of a bad link spent on the wrong page.

What distinguishes it from a browser's history is three decisions, all of which
come from the link:

- **Only confirmed arrivals are recorded.** An entry is written when the server
  says where a tab actually went, never from what was typed at the address bar.
  The "the reader named this one from memory" flag is carried across the round
  trip from the gesture that started it, the same way `wantForeground` carries
  focus intent through an `openTab`. So a mistyped address that resolves to
  nothing never enters the list and can never be completed back to later, and one
  page is one row rather than one row for what was typed and another for what it
  redirected to.
- **What was typed outranks what was merely reached.** Following a link is cheap
  evidence — the page was already in front of the reader. Typing an address is
  somebody naming a destination from memory, which is the thing an address bar
  exists to finish. Match quality still decides first, so a strong history hit
  beats a weak bookmark hit; a saved page wins every tie. One `matchScore` serves
  the panel filter, the start page and the dropdown, so they cannot drift apart
  about what "matches" means. Its top band is new: the query as a prefix of the
  address *as a person types one*, scheme and `www.` stripped, which is what
  re-typing looks like and what fires on the third keystroke instead of the
  tenth.
- **It evicts rather than refusing.** The saved list refuses to grow past 500
  because dropping a bookmark loses something the reader entered (§23). This is a
  cache of a behaviour, so at 1000 the least useful end goes — never a typed
  address while a page merely passed through is still there. The map is kept in
  the order things were last reached rather than sorted on demand, because a
  redirect chain can put three pages in the same millisecond and an order that is
  arbitrary between them is an order that answers differently each time it is
  asked, in a list the reader is watching while they type.

A list built from behaviour rather than from choice will contain things the
reader does not want offered back, and on a six-row dropdown one such row is a
sixth of the surface — so each history row carries an **✕**, with Shift+Delete
doing the same from the keyboard. The widget does not perform the removal: it
asks the shell, which does it with the same notice-and-undo every other
destructive gesture here has, and no confirmation dialog. A saved row carries a
**★** in the same slot instead. That is not decoration — it answers the question
the missing ✕ would otherwise raise, and it keeps the dropdown from being a place
where a bookmark can be destroyed without the undo that lives with the star and
the panel. Removing a row redraws the list in place rather than closing it, since
triaging three bad rows should not be three trips back into the address bar. On a
touch screen the ✕ is always drawn, because there is no hover there to reveal it
with and an affordance a finger cannot discover is not one.

Two smaller consequences. Arrow-down on an empty field used to offer recent
bookmarks and now offers recent *anything*, ordered by recency alone: that list
answers "where was I?", and ranking it by provenance would fill all six rows with
the saved list, which is the one thing already on the page behind the dropdown.
And history is a record of everywhere the reader went rather than of what they
chose to keep, so it gets a bulk *Clear history* in the shell menu — with the
undo in the notice, like everything else — alongside the guarantees it shares
with the saved list: plane-side only, no server copy, and taken by `Store.wipe()`.

Writes coalesce into a one-second window with a flush on `pagehide`. A page load
produces a visit and then a title or two as the document settles, and three
whole-list writes for one navigation is waste with nothing bought by it; the
exposure is a second of history on a hard kill, against a store that is a
convenience by construction.

`test/history_test.go` drives it through the real UI in a real browser: two pages
reached by typing their addresses, the app reloaded whole, the address bar
completing from what survived, a completion opening a page that is *not* the one
on screen, the ✕ removing a row without opening it, and the undo putting it back.

### 35. The used-CSS filter was asking the document about every rule, forever

The filter decides each style rule by asking the page whether anything matches
its selector. That is the right question, and it is asked in the wrong place:
`querySelector` costs nothing when it matches — the search stops at the first
hit — and costs a full walk of the root when it does not, because proving that
nothing matches means looking at everything. A used-CSS pass is *mostly*
failures by construction, so a pass cost rules × elements.

That would be a one-off price if a pass happened once. It does not:
`handleMutations` schedules one after every batch of DOM records, so the whole
bundle is re-tested every `CSS_DEBOUNCE_MS` for as long as the page keeps
changing — and the rules that already shipped are re-tested along with the rest,
since deduplication happens on the way out, after the question has been asked.

Measured on a synthetic utility bundle (12,000 rules over 9,000 elements), which
is an ordinary size for a Tailwind-class site:

| | before | after |
|---|---|---|
| One used-CSS pass | 1,242 ms | 46 ms |
| `__skyhook.snapshot()` end to end | 1,636 ms | 172 ms |
| Renderer main thread, appending one `<div>` per second | **91% busy**, in 1.5 s blocks | 4% busy |

The 91% is the finding. A page that mutates at all — a feed, a clock, a chat, a
spinner — held the landside renderer down continuously, and everything the
mirror does happens on that thread: serialising mutations, answering the host's
evaluates, laying the page out. The reader on the other end of a 1.2 s link was
waiting behind 1.5 s blocks that had nothing to do with their link.

**What replaced it.** One walk of each root per pass builds the set of tag
names, class names and ids that actually occur under it; a rule whose rightmost
compound needs a name that is not in those sets is rejected on a set lookup.
This is the bucketing a browser's own style engine does, and it is sound in one
direction only, which is the direction that matters: the index may prove that a
rule *cannot* match, and everything else — attribute selectors, pseudo-classes,
namespaces, `*`, anything it cannot parse — falls through to `querySelector`
exactly as before. The rightmost compound is the one that decides, and a
compound is a conjunction, so one absent name settles it.

Escaped class names are the trap worth naming: `.md\:flex` is the class called
`md:flex`, which is what `classList` reports, and an index that compared the
escaped spelling would silently drop every Tailwind variant on the page.

`TestPresenceIndexAgreesWithTheDocument` asks both the index and the document
about every rule on a fixture built out of the shapes where they might differ,
and fails on any disagreement — a rule wrongly dropped is invisible from the
client and looks like a site that renders badly.
`TestOneUsedCSSPassStaysCheap` puts a bound on a pass, and
`TestUtilityBundleDoesNotStallTheRenderer` checks the verdicts end to end
through the real mirror while the fixture mutates.

### 36. Every tab's events went through one queue, and slow work ran on it

Found while measuring §35, and the same shape of mistake one level down.

CDP events cannot be handled on the socket reader: a handler that makes a CDP
call would wait for a reply that only that reader can deliver. So they were
queued and drained by a goroutine — **one** goroutine, for every target the
browser has.

That is only sound if handlers are quick, and none of the interesting ones are.
`Page.loadEventFired` recovered the page's cross-origin stylesheets, which is a
round trip per sheet and a fetch of up to 4 MB each. `Runtime.bindingCalled`
carrying a snapshot serialised, filtered, minified, CBOR-encoded and compressed
a whole document, and could then sit in `enqueue` for up to two seconds waiting
on a send queue the link had not drained. All of it on the one goroutine that
every *other* tab's mutations also had to pass through.

Two consequences, and the second is the worse one. A background tab finishing
its page delayed the tab the reader was actually looking at. And while the
queue was blocked it kept filling — at which point the reader drops events,
because the alternative is deadlocking the connection. A dropped
`Runtime.bindingCalled` is a lost mutation, which the integrity check later
catches as a divergence and repairs with a resync. So a slow link filled the
send queue, which blocked dispatch, which dropped mutations, which cost a
resync, which put a whole document back on the link that was already too slow —
each step making the next one likelier.

The queue is now per target. Everything registered on it is scoped to one
session (`Subscribe` binds the session id), so per-session ordering is the only
guarantee anything depends on, and one queue per target preserves it exactly
while letting tabs proceed independently. `Session.Forget` drops a target's
handlers and stops its goroutine when the tab closes, which the single-queue
version never needed and the old handler map never did either — those leaked
for the life of the browser.

Separately, `onLoad` now does its stylesheet recovery on its own goroutine,
which is what `onFrameNavigated` and the agent's `sheets` message already did.
Per-target queues stop one tab delaying another; they do not stop a tab
delaying *itself*, and the tab that has just finished loading is precisely the
one whose mutations someone is waiting for. Only the loading flag is still set
inline, because it is one small frame and the shell is waiting on it to stop
drawing a busy cursor.

`internal/cdp/dispatch_test.go` pins all of it: that a blocked target does not
delay another, that one target's events stay in order, that forgetting a
session drops its handlers and its queue, and that forgetting cannot be aimed
at the browser-level queue — whose key is the empty string, and whose prefix
would otherwise match every global handler there is.

### 37. A repair that grew geometrically each time it failed to repair anything

From a capture of one Reddit session, five minutes long, which sent **12.3 MB**
and received 9 kB. At the 250 kbps this project targets that is 6.6 minutes of
solid transmission inside a 5-minute session: the link was never not saturated.
Three faults, each amplifying the next.

**A replay was recorded as output.** `Resync` replayed through `EmitFrame`,
which is the path a tab's *own* frames take — into the replay ring, the journal
and the compression trainer. So every replayed frame was appended to the ring it
had just been read from, the ring came to hold each frame twice, and the next
request from the same point returned twice as many. The session log shows it
against an unmoving `haveTo`:

| haveTo | frames | bytes |
|---|---|---|
| 13 | 8 | 16,508 |
| 13 | 16 | 33,016 |
| 13 | 32 | 66,032 |
| 20 | 96 | 35,360 |
| 20 | 192 | 70,720 |
| 20 | 384 | 141,440 |

Exact powers of two, which is what distinguishes this from a page that was
merely busy — real activity gives irregular increments. Every doubling was
re-sent over a link the client was already behind on, and every byte of it
discarded plane-side as a duplicate.

**A replay of nothing was treated as a repair.** `Ring.Since` answers an empty
ring with "I can serve this" and no frames, which is true for a client that has
missed nothing. But a client only asks after seeing a frame it cannot apply, so
an empty ring means the frames it needs are gone, and replaying zero of them
leaves it exactly as broken. It asks again on the next mutation, and the next:
**78 "resync by replay frames=0" inside three milliseconds**. Only
`hash-mismatch` escaped this, on the reasoning that "coming back with nothing
missed is the good case, and it must stay free" — but that case never reaches
here. A client that reconnects with nothing missing sends no resync at all, and
one resuming a tab it does not hold sends `cold`, which asks for a snapshot
outright.

**Nothing throttled the asking.** A client that is behind asks on every frame
that arrives while it is behind, which on a page mutating faster than the link
drains is far quicker than any answer can reach it.

The fixes are one each. Replays go out through `replayFrame`, which sends and
records nothing, because a replay produces nothing. `planResync` — the decision,
separated from the doing, so it is testable without a browser — treats an empty
ring as a snapshot for every reason. And a tab ignores a request it has already
answered within `resyncCooldown`, which is longer than a round trip on the link
this targets, because the request that matters is the one sent *after* the
answer arrived.

The cooldown covers two shapes, and the second is why exact-repeat suppression
alone is not enough: a client that is behind keeps applying what it holds, so
its `haveTo` creeps forward and every request looks new — in this capture it
walked 20 → 21 while two snapshots were already in flight. So while a whole
document is on its way, any further request for that tab is that document's job.

What was suppressed is counted per tab and rides into a capture as
`resyncDropped`, because a storm being absorbed correctly is otherwise
invisible, and "the link went quiet" is the report it would arrive as.

`internal/session/resync_test.go` pins all of it, and each test was checked to
fail against the code it guards: the ring is the same size after three replays
of the same ground (it reproduces the doubling as `[3 6 12]` on the wire), an
empty ring plans a snapshot for every reason, 78 identical requests are answered
once, a genuinely different gap is still answered, a gap arriving while a
snapshot is in flight is not, and the mute lifts after the cooldown so a request
that really was lost is asked again.

### 38. A frame nobody misses

The plane side notices a missing batch when a *later* one arrives and does not
fit. That works for as long as the page keeps producing, and fails completely
the moment it stops: on a page that has gone quiet, a frame that never landed is
never missed. There is no later frame to not fit.

The server is the only half that can see it. It knows what it sent and what was
acknowledged, and the integrity check already compares the two every thirty
seconds — but an inconclusive check did nothing at all. It logged, and returned.
One netem run recorded exactly that: `the client never reached the frame it was
checked against`, seq 1 against acked 0, **five times over three minutes**, while
the reader looked at a page whose late stylesheet had not arrived and the tab sat
idle. The server said the right thing five times and never acted on it.

Doing nothing was deliberate, and half right. A client that is behind and
catching up must be left alone: resyncing it puts a document on the link in
competition with the very frames that made it late, which is how a check meant
to protect the mirror becomes the reason it never converges. What the old code
missed is that *behind* and *stopped* are different conditions, and the second
one only the server can end.

`noteStuck` separates them. A repair is asked for only when the same frame is
outstanding, against the same acknowledgement, on a page that has produced
nothing new in between — and only on the second consecutive check, because one
sample cannot tell a stalled client from one that was mid-flight when it was
taken. Anything moving on either side resets the tally.

The repair is an ordinary resync, so it costs what it should: the missing frames
if the ring still holds them, a document only if it does not.

`internal/session/stalled_test.go` pins the distinction from both sides — a
stalled client is repaired on the second check and not the first or third, a
client working through a backlog is left alone however far behind it is, a
client behind a page that is still emitting is left alone, and a caught-up
client is never considered at all.

### 39. The certificate was a choice between the app and the transport

Every deployment had to give up one of the two things the client is built on,
and nothing in the tree said so.

The self-signed certificate is not a compromise on the wire: the client pins its
exact SHA-256, which is stronger than trusting the public CA set, and it is what
lets a personal server run with no public name at all. What it costs is
elsewhere. Chrome will not register a service worker behind a certificate it
does not trust, and the service worker is the entire offline story — the app
that starts from its own cache at 35,000 feet, the shell that survives an
outage, the install. A pinned deployment can mirror pages perfectly and can
never be the thing the README describes.

The reverse proxy fixes precisely that and takes WebTransport away, because no
HTTP proxy forwards HTTP/3 to an upstream. That trade is documented and real —
stream independence and 0-RTT resume are what make a bad link bearable — but it
is a trade, and it was the only escape from the first one.

There was a third arrangement all along, `tlsCert`/`tlsKey`, and it was broken
in a way nobody would find. The pairing file was built from
`cert.FingerprintB64()` unconditionally, so a server with a real certificate
handed the client a pin for it. WebTransport refuses `serverCertificateHashes`
whose certificate is valid for more than 14 days, which is every certificate a
public authority issues. So the one deployment that should have had both got
neither: every QUIC dial failed on a rule about validity windows, the client
fell back to the socket exactly as it does behind a proxy, and the HUD said
`websocket` with no error anywhere to explain it.

`CertBundle.Pin` is the fix and the statement of the rule: only a certificate
this process minted is pinnable, because only that one is short-lived by
construction. Everything else goes out with no `certSha256` and the client uses
ordinary TLS trust — which is what a real certificate is *for*.

That left getting one easy enough to be the default answer, which is
`internal/transport/acme.go`. `hosts` are the names, the challenge follows from
where the listeners already are, and two settings turn it on. Three details are
not obvious:

- **The certificate is answered per handshake**, never held. `GetCertificate`
  rather than `Certificates` is what makes renewal invisible: the replacement is
  simply what the next handshake gets. Contrast the self-signed path, where
  rotation invalidates the pin every client holds and needs a restart *and* a
  re-pairing — a fortnightly ritual that this arrangement deletes.
- **Renewal cannot be left to the handshake.** The manager renews while
  answering, which covers a server in daily use and covers nothing else. A
  Skyhook that is opened when somebody flies is exactly the server that goes two
  months without a handshake and then presents an expired certificate to the one
  connection that mattered. `acmeRenewalLoop` asks every twelve hours whether
  anybody is connecting or not.
- **The warm-up has to look like a browser.** Priming with a synthetic
  `ClientHelloInfo` orders under a cache key derived from the hello, and a hello
  with no cipher suites orders an *RSA* certificate. Every real handshake would
  then miss that entry and order a second one — a warm-up that doubles issuance
  is the shortest path to a rate limit. The suites are set for that reason and
  nothing else.

There is a third challenge, and it is a different shape. `dns-01` proves the
name by publishing a TXT record rather than by being connected to, so it needs
no inbound port at all — which is the point for a machine behind a NAT, on a
link that filters 80 and 443, or with both already spoken for. autocert does not
implement it and cannot be made to: it picks challenges itself, and its whole
design is issuance *during* a handshake. That design does not survive contact
with DNS, where publishing a record and waiting for the world to agree about it
takes tens of seconds at best. So dns-01 is a second issuer over
`x/crypto/acme` directly, behind the same `GetCertificate`, and it issues ahead
of time and keeps the result — which is a plainer thing anyway, and makes
renewal a scheduled job rather than a lucky handshake. It is also the only
challenge that can prove a wildcard, so it is the only one allowed to ask for
one; a wildcard is then kept out of `hosts`, because it certifies a name and
names no server anyone can dial.

The two issuers share the account key file, in autocert's name and encoding, so
changing challenge type keeps the registration instead of quietly opening a
second account with the authority. They share the certificate encoding too, so
`<dataDir>/acme` looks the same either way and neither strands the other's
files.

Three things about dns-01 are worth naming:

  * **There is no provider list and will not be one.** Every DNS API is
    different, and a personal browser has no business carrying a matrix of
    cloud SDKs so that one of them can be used. Skyhook runs a command, passing
    the action, name and value as arguments *and* in the environment because
    half the scripts people already have read one and half read the other. The
    provider's own error text is quoted into Skyhook's, since a message about a
    bad token is the most useful thing anybody could be shown at that moment.
  * **The propagation wait is the feature, not the timeout.** Accepting a
    challenge the instant the provider's API returns is the classic way to have
    an authorization refused. Skyhook finds the zone's own nameservers and asks
    them directly, rather than asking the machine's resolver — which has cached
    the empty answer from just before the record was published, and that cached
    "no" is exactly what stands between a correct record and a challenge that
    would now pass.
  * **Killing a hook is not enough to get its output back.** A hook is usually
    a shell script, and a script that runs `curl` hands it the same stdout;
    cancelling kills the shell and leaves curl holding the pipe, which is what
    `CombinedOutput` is reading. Without `WaitDelay` a hook with a hanging child
    blocks for as long as the child likes, whatever timeout was configured. A
    test asserts the call returns well before its own child does.

The challenge port is where the two socket-answered deployments actually fail,
and it is deliberately not validated to death. Ports 80 and 443 are what the authority *dials*, not
what this process *binds*: a container publishes `80:8080` because an
unprivileged uid cannot have port 80, and refusing that would refuse the
deployment this feature is most useful in. So the server compares the two
numbers and says once, loudly, when they differ, and leaves the forwarding to
the operator. Everything that genuinely cannot work — `behindProxy`, an address
instead of a name, a wildcard, no agreement to the subscriber terms — is refused
at startup with a sentence naming the fix, because the alternative is an
authority error some minutes later written for people implementing ACME.

### 40. Everything that had to line up first, and nothing that said so

Three separate things have to be true before a first run works, and not one of
them is discoverable from the others.

The client is a separate build, and the server only looked for it in `webRoot`
or `<dataDir>/webapp` — so `go run ./cmd/skyhookd` in a checkout with a freshly
built client served *nothing*, and the fix (copy or symlink `client/dist` into
the data directory) is only obvious to somebody who has read `resolveWebRoot`.
A browser you already have open needs two flags it was not started with, and
without them the server fails on a devtools port in a message about a devtools
port. And a certificate needs a name, a challenge, and either a free port or a
DNS hook, each of which fails minutes later in a certificate authority's
vocabulary rather than a browser operator's.

Two changes, and they are different in kind.

The first is that the server now finds the build in the checkout it came out of,
by walking up from the working directory and from the binary for a `go.mod` with
a `client/dist` beside it. That is a default, not a feature: the build twenty
metres away in the same repository is what the operator meant. It cannot fire
anywhere it should not, because a container and a systemd unit both set
`webRoot` and neither `/usr/local/bin` nor `/` has a `client/dist` above it.

The second is `skyhookd -setup`, and the thing that makes it worth having is not
that it asks — it is that it **looks**. Every answer that can be checked is
checked while the person who typed it is still there: it connects to the
devtools endpoint and prints the browser's version back, resolves the name,
tries to bind the challenge port, and runs the DNS hook for real against a
throwaway record. A configuration file full of plausible answers is exactly what
the old path produced, and each wrong one surfaced later, somewhere else, as
something that looked like a different problem.

Writing that check found a bug in the code it was checking. The propagation wait
returned "no error" when it could find no nameserver for the zone — which is
right for issuance, where the authority is perfectly able to judge for itself,
and catastrophic for a self-test, where it means a hook that publishes nothing
at all is reported as working. `forTXT` now returns whether it actually saw the
record, and the two callers want opposite things from that: issuance warns and
carries on, the self-test refuses to bless what it could not check.

Two rules hold the whole thing together. Nothing is written until the entire
plan has been shown and agreed, so an abandoned run leaves no trace and an
existing config is moved aside rather than overwritten. And the file it writes
is run through `config.Load` — the same loader the server uses — before it is
installed, because a setup program that produces a configuration the server then
refuses would be worse than no setup program at all.

### 41. One tab could take the whole browser, and killing it did not help

From a capture taken on a phone: a 6.6 s round trip, 10.6 MB sent against 7.8 kB
received, and a note that reads *"not a rendering problem but now the app isn't
responsive when I tried to load reddit."* Nothing in it is a rendering problem.
Four faults, and the last of them is why the first three could not be worked
around.

**The reader's own tab starved behind a tab they were not looking at.** The
outbound scheduler is strict priority by channel, which is the right answer to
"an image must not delay a diff" and no answer at all to "a tab must not delay a
tab": inside a class it was one FIFO, so whichever tab filled it first was served
first and completely. Reddit's snapshot and its mutations went in ahead of
everything the read tab produced, and for the four minutes that took, that tab's
`acked` stayed at 0 — through three clicks, until the server declared it stalled
and resynced it. Each class now keeps a queue per tab and rotates between them.
Per-tab order is preserved exactly, because a mutation's strings extend an
append-only intern table by position; order between tabs never guaranteed
anything. The tab in front of the reader goes first and yields every fourth
frame, so a background page arrives slowly rather than not at all.

**Closing a tab freed nothing.** `CloseTab` closed the browser target and told
the client, and left every frame the tab had already encoded sitting in the send
queues. The capture has the reader closing the offending tab at 02:14:10 and
still waiting at 02:16:32. A close now discards that tab's queued frames in every
class and logs what it took back, and the emit path turns away anything the
mirror was still serialising for a tab that has gone. Nothing is owed a repair
afterwards, which is exactly what distinguishes this from dropping frames for a
tab that is still open: the document they belonged to is gone on both sides.

The exception is the news that the tab closed, and it needed one — the emit path
turns away *ctrl* traffic for a dead tab too, and it has to. A tab emits a last
state frame as its target goes down, which arrives behind the close and says the
tab is loading nothing, which is a tab that exists. It cost a test that failed
one run in three to find, and it is the same shape as the plane-side bug below:
everything about a closed tab has to stop, including the parts that look
harmless.

**A closed tab went on costing the client.** Plane-side, a frame for a tab the
shell no longer held reached `hostFor`, which builds a mirror frame for a tab it
does not know — so closing a tab put a fresh iframe back in the page and patched
the dead tab's document into it, invisible, competing for the main thread with
the tab that was kept. The worker had the worse half of it: a mutation for a tab
it had no snapshot for is the definition of a cold client, so it asked for a
resync and the server answered with a whole document for a page that had been
closed. Both halves now remember what the reader closed. The worker drops those
frames before decoding them, never acks or asks after them, and re-sends a close
that could not be sent while the link was down — the tab used to come back from
an outage the reader had already dismissed it in. The record is cleared when the
session id changes, because a restarted server numbers its tabs from one again.

**And the kill switch could not be heard.** This is the one that made the app
unresponsive rather than merely slow. Every client frame was dispatched on the
connection's read loop, in arrival order, and two of the calls that path makes
have no deadline worth having: `Page.navigate` returns when the navigation
commits, and a snapshot's `Runtime.evaluate` returns when the page's own main
thread is free. The event log — which records a frame when it is dispatched —
shows what that cost: reddit navigated at 02:12:28, and then **nothing at all
for a hundred seconds**, from any tab, until it committed at 02:14:08. The
reader spent that time pressing things. The close they eventually sent was in
the same queue, behind the navigation it was meant to call off.

Each tab now drains its own queue on its own goroutine and the reader's
connection goes straight back to reading. Two frames go around that queue rather
than into it, and they are exactly the two that are about work already in
flight: `TabClose` cancels the tab's context outright, and `navigate{stop}`
cancels whatever call is holding the queue before asking for `Page.stopLoading`.
Cancelling the call is not cancelling the navigation — only `Page.stopLoading`
does that — but it is what gives the stop a queue to go into.

**Stop is also, finally, a button.** The wire and the landside half had carried
`navigate{action: "stop"}` from the beginning and no chrome ever sent it, so the
only way to end a page that was still coming was to close the tab and lose
whatever had arrived. Reload becomes stop while a page is on its way, the way
every browser has drawn it since 1994; the phone shell, which hides reload for
room, draws it *only* while it is a stop; and the tab list — the only place a
background tab can be reached — makes the spinner on a loading row the target
that ends it. What has landed stays: the mirror is patched rather than replaced,
so a page stopped half-drawn leaves a half-drawn page and not a blank one.

Three things the netem run found that a loopback run could not, and the first is
the one worth remembering: **fairness is not free where order is the message.**
Rotating between tabs inside the ctrl class reordered the *announcements* of two
tabs opened a moment apart — the newer one was the active one, so it jumped the
queue — and the shell decides which tab to put the reader in from exactly that
order, because the server has no opinion about focus and never has. The reader
asked for a saved page and was left in the empty tab they had opened before it.
Nothing on ctrl is big enough to starve anything, so the class it buys the least
in is the one it broke: ctrl keeps a single line now, still queued per tab so a
close can take its frames out of it.

The other two are consequences of the drop above rather than of the capture.
**A tab being opened looks exactly like a tab that has closed** from the emit
path: neither has a tabState, because a tab starts mirroring inside
`mirror.NewTab` — the moment its
agent is installed — and `OpenTab` has nothing to register until that returns.
The frames in that window are the tab's whole first document, and dropping them
left a tab acknowledging nothing for four minutes with an empty frame in front
of the reader. `opening` holds the id across the gap, so the two cases are
distinguishable; and the emit path now asks `worthSending`, which keeps ctrl
traffic that is not a state frame, because an error about a tab that has just
gone is what somebody is reading the log for.

**And nothing came to its rescue**, which is the more interesting half. §38's
stalled-client repair exists for exactly this — a frame the plane side cannot
notice is missing — and it sat out all eight checks. A snapshot is frame 0, so
`acked == 0` is what a client that has applied the document and a client that
has never heard of it both look like, and `noteStuck` read the first. The
document hash tells them apart: every ack carries one and it is zero until the
first arrives. A missing snapshot is the case that most needs this check, since
there is no later frame for a gap to show up in — so it was missing precisely
where it mattered most.

`internal/session/sched_test.go` pins the scheduling and the purge — that a tab
which queued sixteen frames first does not get served completely first, that the
active tab takes most of the link and still yields, that a tab's own frames keep
their order, that ctrl keeps its order across tabs too, that closing takes back
that tab's frames and nobody else's, that the only thing left for a closed tab
is the close itself, and that ctrl traffic queued during an outage is still
there when the link returns.
`internal/session/inbound_test.go` pins the dispatch path: a tab stuck in a call
that will not return does not stop another tab's work, a stop ends what the tab
is doing without ending the tab, and a close ends it outright.
`client/test/killtab.test.ts` pins the plane side, and `test/stop_test.go` runs
both through a real browser — including the button the reader actually presses.

Each was checked against the code it guards: with one queue per class the
starvation test reports the read tab waiting behind sixteen frames, with the
purge removed the close leaves all thirteen frames queued, with dispatch inline
the blocked-tab test times out, without the interrupt the stop test waits out
its thirty seconds on a page that never commits, without `opening` a tab being
built loses both its snapshot and its state frame, with `acked >= seq` alone a
client that never heard the document is never repaired, and with ctrl rotating
the second of two tabs is announced first — which is the netem failure, in three
lines and without a browser.

## Measured results

From the end-to-end suite. The design asks for every milestone to be measured
against an emulated link rather than a LAN, so both columns are reported: a
plain loopback run, and a CI run with `tc netem` shaping the Skyhook port to
1.2 s RTT, 250 kbps and 2% loss.

| | Loopback | Emulated 1.2 s / 250 kbps / 2% |
|---|---|---|
| Mirror delivers document and styles | 0.6 s | 23.3 s |
| Click → resulting mutation applied | 0.6 s | 20.6 s |
| Reconnect → resumed page state | 2.7 s | 27.5 s |
| Image with blurhash placeholder → bytes | 0.6 s | 15.5 s |
| **One appended chat-style message on the wire** | **73 bytes** | **73 bytes** |
| Hoisting a 30-row block (a keyed reorder) | 33 bytes | — |
| 40 nodes added and dropped in one task | 59 bytes | — |

The last two rows are the mutation-batch work described in
[PRIOR-ART.md](PRIOR-ART.md). The reorder cost 365 bytes and destroyed node
identity before it; the churn figure is unchanged, and is there to keep it that
way.

There used to be a whole-suite row here — 8 tests, 10 s on loopback and 158 s
over the emulated link. It has been dropped rather than updated. The suite is
about ninety-five tests now, so the old figure was long stale, but the deeper
problem is that a single number stopped describing anything: the suite runs
several tests at once, each leasing one of `scripts/netem.sh lanes`' shaped
lanes, so its wall clock is a function of how many lanes CI was given and not a
property of the code under test. The per-test rows above are the honest
measurements, and they are the ones a change to the protocol would move.

The per-test figures are unaffected by running tests together, which is the
point of a lane per test: each concurrent test gets a netem qdisc of its own,
so it sees the whole 250 kbit rather than a share of it. Sharing one shaped
port would have divided the link and bought nothing.

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

### 43. Three more the busy machine found, and one the slow link did

§41 and §42 took most of the end-to-end suite's load sensitivity. Underneath
them were two faults belonging to the reader, and a third that only a link with
no room could show.

**A stop still queued behind the navigation it calls off.** §41 says
`navigate{stop}` "cancels whatever call is holding the queue", and that is
exactly true when the navigation being stopped is the one holding it. When the
landside browser is behind, it is not: the reader's navigation is still
*queued*, `interrupt` cancels whatever is running instead, and the stop goes in
behind the navigation it was meant to end. That navigation then starts, does not
commit — which is why stop was pressed — and the queue never reaches the frame
that would have called it off. `tabDepth`'s own comment says a wedged tab is
answered by "stop or close, neither of which queues"; the code and its account
of itself had come apart, and only on a machine slow enough for the difference
to show. A stop now bumps a generation and the loop drops *navigations* queued
before it — only navigations, because the rest of that queue is what the reader
typed, and `tabDepth` is sized so none of it is lost.

**The tab strip was ordered by hearsay.** `Map.set` keeps a key where it was
first put, so re-setting one already there leaves it in place. The tab model
updated in place, which made the strip's order the order this client first heard
of each tab rather than the order the session gives — and the session sorts its
own list for exactly this reason, saying in its comment that tab order is muscle
memory. The two agree whenever frames arrive tidily; on a reload where they do
not, the reader comes back to their tabs rearranged. Reset now rebuilds in the
order it was given, then appends what this side asked for on this connection and
the session cannot have named yet.

**Deciding to send a picture and recording that it was sent were two steps.**
The image ledger refuses a key the client already holds, and a picture is
submitted several times for one page: the snapshot that names it, a mutation
that names it again, the snapshot a resync sends. On loopback the ledger logs
one send and three refusals and looks perfect. With an encode and a queue push
between the decision and the record, though, any two submissions that overlapped
both read "not sent yet" before either wrote "sent" — and a link where one send
is still going when the next submission arrives overlaps them constantly. The
emulated 1.2 s / 250 kbps link spent **27870 bytes delivering a 13664-byte
picture the client was already holding**, against 471 on loopback.

The key is claimed under the same lock that decides, and the claim says whether
it was this attempt's own, so a frame the queue refuses gives back what this
attempt took and nothing else — a key already on the books stays there, which is
what keeps an ask that crossed the bytes on the wire from costing a re-send.

Two things about how it was found are worth more than the fix. The test that
caught it had to be rewritten twice: waiting for any image frame after a resync
called the client's own second-chance path a failure, and counting bytes without
separating keys called a picture re-fetched at a new laid-out size a failure
too. The invariant is narrower than either — *a key the client was already
holding must not cross again* — and only that version was both true on the bad
link and able to fail. And the race itself is asked of the ledger directly, two
submissions in a row answered yes then no, because a race asked of goroutines
reproduces when it feels like it: the first concurrent test passed against the
broken code.

### 44. A frame number does not name a document

The integrity check hashes the landside page, waits for the client's
acknowledgement of the frame it measured, and compares. Over netem it reported
`mirror divergence` twice on a page nothing was wrong with, and the numbers said
what it was before any test did: the second report's client hash was the *first
report's server hash*, and both said `seq=0`.

A snapshot restarts a tab's frame numbering at zero. Frame 0 is therefore not a
frame — it is the sentence "I have the current document", said identically about
every document the tab has ever held, and a page that builds itself sends
several of them a second. The check anchored on that number. A client one round
trip behind — the ordinary state at 1.2 s RTT — answers about the document
before the one being measured, the two hashes differ because they describe two
documents, and the mirror is resynced for being right. The repair costs a whole
document on the link that made the client late in the first place, which is §37
again, triggered by the guard that exists to prevent it.

Two halves, because the ambiguity has two sides. The tab counts the documents it
has sent (`docEpoch`), so the server can tell that the page it measured has been
replaced since — that half needs nothing on the wire and was the first fix.
It is not enough: it catches the document moving *after* the measurement and
says nothing about an answer that predates it. So the epoch goes on the
snapshot, and every acknowledgement about that document echoes it back
(`TabAck.Epoch`, `Snapshot.Epoch`). An answer that names another document is not
an answer, and the check waits for one that does.

An ack with no epoch — a client older than the field — is heard as before, and
keeps the old ambiguity along with it. The alternative is a check that goes
inconclusive against every such client, which is not neutral: two of those in a
row is what §38's stall detector reads as a stopped client, and it would resync
a mirror with nothing wrong with it once a minute.

What makes this one worth writing down is that the first fix looked complete.
The e2e test that found it passed afterwards, on the fast link and on the slow
one, and the fault came back a day later with the same two hashes in the log —
because the epoch was being compared against the wrong thing at the wrong end.
A measurement is only as good as the answer's claim to be about it.

### 45. Half a theme is not a theme

A capture of a GitHub file page came in reported as "the navigation on the left
seems broken (some css is missing?)", and the file tree in the plane-side
screenshot is exactly that: filenames a shade off white, on white, beside a
branch picker and a search box painted in dark greys, under a pane header that
is still light. Everything the capture measures said nothing was wrong. The two
halves agreed on the document hash, node for node across 9,261 nodes. The
used-CSS filter reported 19,311 rules rejected out of 20,867, and every one of
the 4,000 it listed genuinely matched nothing — checked by replaying the
document and asking it about each of them.

Nothing had been dropped. The palette had been decided twice.

GitHub serves `<html data-color-mode="auto" data-light-theme="light"
data-dark-theme="dark">` and hangs its entire set of colour properties off a
media query:

```css
@media (prefers-color-scheme: dark) {
  [data-color-mode][data-color-mode="auto"][data-dark-theme="dark"] {
    --bgColor-default: #0d1117; --fgColor-default: #f0f6fc; …
  }
}
```

The landside browser is light, so that block does not apply and the light one
does; the rules crossed the link as written, wrapper and all, and the reader's
browser is the one that answered on the other side. Resolving `--bgColor-default`
against the delivered bundle gives `#fff` under a light reader and `#0d1117`
under a dark one — from the same bytes.

What the reader got was not the dark theme. It was the parts of the page that
read a colour property in the dark theme, over the parts that do not. The
mirror's own chrome is `html, body { background: #fff; color: #111 }`, a flat
white behind every page it draws, and the page's own `body` is a node *inside*
that document rather than the thing that paints the canvas — so the theme's
background never reaches the surface it was written for. `--fgColor-muted` did
reach the filenames. Near-white text, white ground.

That would be a bug even if the theme had arrived whole, because a stylesheet is
the only part of a mirror that can change its mind on the other side. The images
were fetched, chosen and transcoded from the landside render; the canvases were
rasterised there; the capture's landside screenshot is in that palette; the
chrome around the document is a constant. A page repainted plane-side is a page
repainted alone.

So the agent now answers that question itself, before the rules are sent, and
sends what is left of the query ([agent.js](../internal/mirror/agent.js),
`resolveMediaList`). A block that cannot match here is not sent — it is noted as
rejected, so the next capture *says* the theme was decided rather than leaving
it to be worked out. A block that always matches is sent without its wrapper,
its rules unchanged and in the same place in the cascade, since a media query
carries no specificity. A block that is partly this side's keeps exactly the
part that is not:

| written | crosses as |
|---|---|
| `@media (prefers-color-scheme: dark)` | nothing |
| `@media (prefers-color-scheme: light)` | the rules, unwrapped |
| `@media screen and (prefers-color-scheme: light)` | `@media screen` |
| `@media (min-width: 40em) and (prefers-color-scheme: light)` | `@media (min-width: 40em)` |
| `@media not all and (prefers-color-scheme: dark)` | the rules, unwrapped |
| `@media (min-width: 40em)` | unchanged |

The condition is parsed rather than pattern-matched, because the feature can sit
anywhere in one: inside `not (…)`, inside a nested `((…) and (…))`, in the
middle of an `and` chain, in one arm of a comma-separated list. The parse folds
the constants out and writes back what is left, and any shape it cannot read is
shipped as written — the failure that costs a wrapper, never a rule.

**`prefers-color-scheme` is the only feature answered this way, and the
restraint is the design.** The neighbouring features look like the same problem
and are not:

- **The viewport is already shared.** The client reports its window and
  `Tab.SetViewport` puts the landside tab in exactly that box, device pixel
  ratio included, so `width`, `orientation` and `resolution` are one question
  with one answer. They also have to stay live: the reader can turn a phone
  sideways, and the mirror should reflow at the turn rather than at the next
  round trip.
- **The reader is the reader's.** `prefers-reduced-motion`, `prefers-contrast`,
  `forced-colors`, `hover` and `pointer` are facts about a person, and the
  person is plane-side. The landside browser is headless with nobody at it: it
  answers `no-preference` and `pointer: none` because nothing was ever asked of
  it, and freezing that would hand a reader who *did* ask a page that ignores
  them — animation to somebody who turned animation off. This is the same answer
  `:hover` and `:focus` have always got from the used-CSS filter, for the same
  reason.

The one that had to change is the one that is not really a preference at all: it
does not say how the reader would like to be shown the page, it says which of
two pages the server rendered.

`TestTheLandsideBrowserDecidesTheColorScheme` pairs every rule in its fixture
with its opposite, so the bundle names which browser answered — the light values
are this side's and the 100-series values are the reader's — and holds the line
on the other three: a viewport query, a `hover` query and a
`prefers-reduced-motion` query all have to arrive still wrapped, still asking.

#### The other three ways the palette crossed unanswered

The `@media` block turned out to be the least of it. Asking, of every other
route a colour scheme has into a mirror, "who answers this, and when?" found
three more — two of them worse, because they were not being answered at all.

**A sheet's own `media` was read by nobody.** `document.styleSheets` lists every
`<link>` and `<style>` whatever their media attribute says, and a browser parses
a sheet it is not currently applying, so a walk that goes straight to `cssRules`
collects the rules of a sheet the page is not using and hands them over with
nothing left to say they were conditional. This is how a site has split its
themes since long before `@media` blocks were the fashion, and still the common
way:

```html
<link rel="stylesheet" media="(prefers-color-scheme: dark)" href="dark.css">
<link rel="stylesheet" media="(prefers-color-scheme: light)" href="light.css">
```

Both sheets crossed, unwrapped, one after the other. Which theme the reader got
was decided by which `<link>` the page happened to write second — worse than the
`@media` bug, which at least let one browser decide. `media="print"` is the same
fault with a plainer symptom and no connection to themes at all: a page's print
rules, applied to the screen, for every page that ships them that way. The sheet
is now walked under its own media, resolved exactly as a block's condition is
(`collectSheet`), which is a four-line change to `collectSheets` and one shared
`collectInto` under it and under `collectGroup`.

**`color-scheme` was crossing as written.** A page that says

```css
:root { color-scheme: light dark }
```

has not chosen anything. It has said it can be either and left the choice to the
browser, and what the browser then paints in the chosen scheme is everything the
page does not paint itself: form controls, scrollbars, the canvas behind the
document, the default text colour — and every `light-dark()` value in the sheet.
No media query is involved, so nothing in the previous fix touched it, and a
light page arrived with dark checkboxes, dark dropdowns and a dark scrollbar.

A value naming one scheme is already an answer and is left exactly as written: a
page that asked for dark asked for dark. A value naming both is collapsed to the
one this browser picked (`pinColorSchemes`), which is the same answer the media
query gets, arrived at the same way. `only` is a separate instruction — it turns
off the browser's own darkening — and survives the collapse. The rewrite is
scanned rather than pattern-matched for the reason `replaceCSSURLs` is:
`content: "color-scheme: light dark"` is text a page means to display, and a
pattern cannot tell it from a declaration. It runs over inline `style`
attributes too, which travel with the DOM rather than with the sheet.

**The canvas was the mirror's, not the page's.** A page's background does not
paint its root box; it paints the *canvas*, the whole surface behind the
document however short the document is, and the value is taken from `<html>` or
— where that has none — from `<body>`. That propagation is a property of being a
document's root, and plane-side neither element is one: both are ordinary
elements inside the mirror's own document ([§30](#30-the-mirrors-own-wrapper-was-breaking-every-full-height-layout)),
and the surface behind them is painted by the mirror's chrome, which is a flat
`#fff`.

A page that paints its `<html>` has always come out right, and by accident:
`html` is a type selector, so the page's own rule matches the mirror's root as
well as the page's copy of one. A page that paints only its `<body>` — the
ordinary way to write it — does not, and a dark site arrives as a dark document
on a white field: white below the fold, white in the margins, white wherever the
document does not reach. Measured on a rebuilt mirror, `body { background: … }`
leaves the canvas at `rgb(255,255,255)` where landside it was `rgb(13,17,23)`.

So the landside canvas is read for what it is and sent as one rule about the
mirror's own root. `:root` is the one selector that cannot be confused about
which document it means — plane-side it is the frame's `html` and never the
page's — and the rule is `!important` because it is not part of the page's
cascade at all: it is this side reporting a fact the other side cannot work out,
and a page rule landing in a later delta must not overturn it. Only the
top-level agent says anything; a frame repainting the reader's whole page would
be a worse bug than the one it fixes.

**And a browser can repaint a mirror without reading a single rule.** Chrome
for Android's "Dark theme" inverts a page that has not said which scheme it is
in — algorithmically, at paint time, below anything a stylesheet can see. Over a
mirror it repaints the DOM half of a document whose other half cannot follow:
the images were fetched, chosen and transcoded from a light landside render, and
the canvases were rasterised there. Measured on a rebuilt mirror with Chromium's
auto-dark override on, a mirrored light page comes out `rgb(18,18,18)`; with the
one declaration that turns it off, `rgb(255,255,255)`.

So the same rule that carries the canvas carries the scheme the document was
painted in, written with `only` — the keyword that means "and do not
second-guess it". A page that never mentioned a scheme was painted light,
because that is what the landside browser does with `normal`: it is headless,
with nothing forcing its hand. `color-scheme` is inherited rather than imposed,
so an element inside the page that declares its own still wins for itself, which
is what a dark card on a light page needs.

The end state is a mirror that renders the same whatever the reader's browser
prefers and whatever it would rather do about it — verified by screenshot in all
four combinations of reader scheme and force-dark.

That last one had a bug of its own, and it is the ordinary shape of this
codebase's mistakes. The rule is only re-sent when the colour changes, and
"already sent" was remembered across a snapshot — which throws the sheet away
and rebuilds it. A CSS pass that happened to run before the first snapshot
therefore recorded the rule as sent and then the snapshot dropped it, so the
fixture passed or failed on the order of two timers. `snapshot()` resets it with
everything else it resets.

#### The chrome was describing four elements and meant two

Sending the canvas exposed the thing standing in front of it. The stylesheet the
client injects into every mirror frame opened with

```css
html, body { margin: 0; padding: 0; background: #fff; color: #111 }
```

and in that document `html, body` names four elements, not two: the frame's own
root and body, and the page's, which arrive as ordinary elements inside them.
That is the arrangement [§30](#30-the-mirrors-own-wrapper-was-breaking-every-full-height-layout)
went to some trouble to get — nothing between the frame's body and the page's
root, so a `height: 100%` chain reaches the viewport — and a type selector
reaches straight through it. §30 stopped putting a box in the middle; this was
putting a stylesheet there instead.

Two consequences, the same fault from either side. Every page that never touched
its margins lost the eight pixels the UA gives a `<body>`, so a plain page
started hard against the corner where landside it sat inset — measured:
`p@(0,0)` mirrored against `p@(8,8)` landside, for markup with no CSS at all —
and every measurement the server took of that page described a layout the reader
was not looking at. And the page's own root was painted white, which is invisible
while the margin is zero and becomes a white frame around a dark page the moment
it is not.

`:root` and `:root > body` can only be the frame's own two. What the page's html
and body should look like is a question for the page and for the UA, and both of
them know the answer: with the chrome scoped, the same markup mirrors at
`p@(8,8)` with the page's root transparent, which is where the canvas rule then
puts the landside colour.

`TestPWALeavesThePagesOwnMarginsAlone` drives the real client at the real page
and asserts all three: the mirrored body carries the UA's 8px, the paragraph
starts where the landside one did, and the page's own root is nobody else's to
paint.

One thing came out of the plumbing rather than the diagnosis. Unwrapping a block
means pushing its rules where the wrapper used to go, and the group walker marks
what it emits as sent — which for a wrapper is the *contents*, so that one rule
inside starting to match does not resend the whole block ([§35](#35-the-used-css-filter-was-asking-the-document-about-every-rule-forever)).
With no wrapper the contents *are* what is emitted, so marking them first made
the caller drop every one of them as already sent, and the three unwrapped rules
in the fixture arrived as nothing at all. An unwrapped rule is deduped by
whoever receives it, exactly like a rule that was never in a group.

### 46. The other question the plane side answers wrong

`:target` names the element the document's own URL points at. A reference work
says which of two hundred footnotes you asked for by styling exactly that, and
so does a source viewer highlighting the line a permalink names — which is what
the URL in [§45](#45-half-a-theme-is-not-a-theme)'s capture was:
`…/UIKitUtils.m#L299-L328`.

The mirror is a frame with no fragment in its address and never gets one. The
client jumps to a fragment by *scrolling* — that is `jumpToFragment`, and it is
the whole reason an in-page link on this link costs nothing — so `location.hash`
inside the frame stays empty for the life of the tab. `:target` therefore
matched nothing plane-side, and the pair failed together: no highlight on the
note the reader came for, and the `:not(:target)` styling worn by every note
including that one.

This is [§45](#45-half-a-theme-is-not-a-theme)'s shape with a different subject,
and it is `:defined`'s shape exactly, so it gets `:defined`'s answer. The agent
marks the element landside (`data-sky-target`), and `rewriteLandsideState` —
which was `rewriteDefined`, and is renamed because the name had stopped being
true — re-points the selector at the mark. Specificity is unchanged: an
attribute selector and a pseudo-class both count the same.

The mark then has to keep up, and it moves for two reasons that have nothing to
do with each other.

**Landside, the URL changes.** A fragment changing reaches no mutation observer,
so it is watched two ways: `hashchange` for promptness, and the sweep, which is
what covers a `pushState` that `hashchange` never fires for.

**Plane-side, the reader follows a link.** `jumpToFragment` handles an in-page
link without telling anybody, which is the point of it — so the landside URL
does not change, the landside mark does not move, and every link the reader
follows inside the page would appear to do nothing but scroll. The client moves
the mark with the jump that moved the reader. That leaves the two sides
disagreeing about which element is the target until the next snapshot, which is
the same bargain the adopted scroll position already makes and for the same
reason: what the reader did is the more recent fact.

There is one place the mark cannot be in the snapshot, and it is worth writing
down because it looked like a flaky test for half an hour. **`:target` is not
settled at `DOMContentLoaded`, which is when the snapshot is taken.** Chromium
scrolls to the indicated part of a document once it has *loaded* and sets
`:target` at that moment — measured on the fixture, every run:

```
at-script-start:    null      readyState=loading      hash=#note-2
at-DOMContentLoaded: null     readyState=interactive  hash=#note-2
at-load:            note-2    readyState=complete     hash=#note-2
```

So a deep link's snapshot reads null however deep the link was, and the mark
arrives as the attribute op the load handler queues one batch later. Delaying
the snapshot to make it arrive together would cost every page the load event to
save one attribute on some of them. The test waits for the mark rather than
assuming it rode with the document, and says why.

### 47. Three gaps, closed

[§45](#45-half-a-theme-is-not-a-theme) and [§46](#46-the-other-question-the-plane-side-answers-wrong)
each left something written down rather than built. All three were the same
shape of admission — *this is where the answer stops being complete* — and all
three turned out to be a day's work rather than a project.

**A media query inside a block that crosses as text.** Almost everything is
walked: `collectRules` goes into a `@media` and asks this browser about its
condition. Two things are not, and cannot be — a `@scope` body, whose rules are
written against a root the document cannot be asked about, and a grouping
at-rule this build has no name for, which ships whole because guessing at its
prelude is worse than keeping it. Both hand over `cssText`, so a
`prefers-color-scheme` query inside one reached the reader with its question
intact: §45's fault, in the two places §45's fix could not see.

The text is now scanned for them (`resolveMediaInText`). It is a small parser
and deliberately a nervous one: it steps over strings and comments for the
reason everything here does — `content: "@media print"` is text a page means to
display — and anything it cannot read is left exactly as written, which costs a
wrapper and never a rule. The condition itself goes to the same
`resolveMediaList` a walked block's does, so the two paths cannot drift.

**The canvas as a background rather than a colour.** §45 sent the landside
canvas *colour*, which is what a light/dark disagreement turns on and is most of
the real cases. It is not all of them: a page whose ground is a gradient or a
tiled texture had that flattened to the mirror's own white. The whole set now
travels — image, position, size, repeat, attachment, origin, clip — taken from
whichever element the propagation itself takes it from, and sent as a set
because a `background-image` landing on top of the mirror's plain
`background: #fff` would otherwise be sized and repeated by *that* rule's
defaults. The url() inside it is left absolute, so the server rewrites it into
an image key through `rewriteCSSImages` exactly as it does for any other
background on the page: the picture crosses the link the same way and at the
same cost. A page with only a colour still sends one declaration.

**The reader's say in the answer.** Settling `prefers-color-scheme` landside is
what makes a themed page arrive whole, and it is also what leaves the reader
with whatever the landside browser happens to be. The say could not be a
plane-side toggle — that a mirror cannot repaint itself is §45's whole finding —
so it is a preference that travels *to* the server: `Viewport.scheme`, riding
with the width and the pixel ratio because it is the same kind of fact about the
reader's window, and applied at the same place (`Tab.setColorScheme`, via
`Emulation.setEmulatedMedia`). The question is still answered once, by the
browser that paints the page. The reader now gets to tell that browser what to
answer.

Two details are worth the words. The client's default is not "leave it blank" —
it sends whichever scheme *this device* is set to, so the landside browser is
put in the reader's own scheme and paints the page there. That is the one
arrangement where the reader gets the theme they prefer *and* the two sides
agree about it, because the server rendered the theme it sent. And changing it
costs a document per open tab: a stylesheet is a delta the client only appends
to, so rules written under the old answer cannot be taken back a rule at a time
and the tab is re-snapshotted. That is why it is a preference rather than a
switch, why the menu entry carries `resends open pages` as its hint, and why the
client reads it from the store *before* the first Hello — a scheme that arrived
after a tab was built would cost that tab a page to apply.

`TestTheReaderCanAskForTheOtherColorScheme` drives the whole loop: a light
bundle arrives, the reader asks for dark, and what comes back has the dark rules
from all three routes §45 found — the `@media` block, the `<style media>` and
the `<link media>` — with the light ones gone and `color-scheme` pinned the
other way.

### 48. Two things a Google Chat capture had to say

A reader sent a diagnostic bundle of `chat.google.com` with the note *"this page
rendering is still pretty broken. creating a chat also doesn't work correctly
because the pop-up closes immediately when clicking anywhere."* The two halves
of that sentence turned out to be two unrelated bugs, and the bundle names both
precisely enough that neither needed reproducing.

**The nav had the word `star` in it where the star was.** The mirror was not
wrong about the document: `clientHash` and `expectedHash` agree, all 21 frames
applied, 1,896 nodes on both sides. It was wrong about one stylesheet rule.
`fontsWithoutSubstitute` keeps a webfont only for a family the page draws
private-use codepoints in — the [§23](#23-the-plane-side-picture-was-a-picture-of-something-else)
reasoning, and correct as far as it goes, since a substitute has nothing at a
codepoint that means nothing. Material does not write an icon font that way. The
glyph is at the *ligature*: the markup says
`<i class="google-material-icons">mark_chat_unread</i>` and the font substitutes
that whole run for one picture. Nothing in it is private use, so the scan found
nothing to keep, the family was dropped, and the reader's own font rendered
exactly what the markup said. That is worse than the empty boxes the private-use
case gives — an empty box reads as absence, and `spool` growing out of the side
of a chip reads as the page.

The signal is the declaration that makes the ligature work.
`font-feature-settings: "liga"` asks for a substitution that would not otherwise
happen, which is a thing to ask for only when the substitution *is* the content;
prose never asks, because ligatures are already on, so the one thing a text
stylesheet writes into that property is the negation `"liga" 0`. On the capture
the split is exact: eleven of twenty thousand rules mention the property, the
four asking for ligatures name `Google Symbols`, `Google Material Icons`,
`Material Icons Extended` and `Google Symbols Subset`, and every rule turning
them off names Google Sans.

So `ligatureFamilies` walks the sheets — not the DOM, because the evidence is a
declaration and reading it costs one property access against the style
resolution a computed read forces — and asks `selectorMatches` only about the
handful that got that far, which is what keeps a stylesheet declaring
`.material-icons` and never using it from buying a font nothing draws. It runs
before the collecting walk rather than inside it because the two halves of the
evidence are in different sheets: Google ships the icon classes with the app and
the `@font-face` from `fonts.googleapis.com`, and which the walk reaches first
is not something to depend on. Both walks now read a sheet through one
`readableRules`, so a cross-origin sheet the host recovered is visible to the
scan and the collector alike — a rule recovered for one and invisible to the
other would have the two disagreeing about a page neither can read twice.

Three of the capture's four icon families are recovered by this. The fourth,
`Google Symbols Subset`, is a subset Chat builds at runtime and registers
through `document.fonts.add(new FontFace(…))`; it appears in no stylesheet, and
`FontFace` exposes no `src`, so there is nothing to ship. Its glyphs still
arrive as their names. That is a gap, not a fix waiting to be written.

**The dialog closed over the result being tapped.** The session log has the
whole thing twice, five minutes apart: `blur` on the search field, then a click
four milliseconds later, then `mirror: node 2219 not found landside`. The client
sends a blur of its own on `focusout`, and landside that is
`document.activeElement.blur()` on its own round trip. Google Chat answers a
search field losing focus by closing the dialog, which destroys the result row —
so the click that arrived next named a node the page no longer had. From the
reader's side the popup closes whenever they touch it.

The blur was redundant before it was harmful. Landside a press moves focus by
itself: the click the client is about to send arrives as a real `mousePressed`
on the control, and the field blurs exactly when and because it would have if
the reader were sitting in front of the page. Sending one separately does the
same job early, alone, and out of order.

So a blur a press caused is now *held* (`MirrorHost.heldBlur`) rather than sent.
The gesture that follows clears it by doing the job better — a click, a
double-click, a pan, all of which land as real presses — and a gesture that
never reaches the page flushes it, because then nothing else is going to tell
the page the reader left. Focus arriving somewhere else clears it too, since a
held blur flushed after a focus would name the field the reader has just moved
*into*. The local half is unchanged: ownership of the field ends on `focusout`
as it always did, and the buffered server ops for it are applied there. Only the
telling waits. A blur with no press behind it — focus going to the URL bar, to a
menu, to a tab strip — still goes immediately, because there is no gesture
coming that would carry it.

`clickInFrame` came out of the listener to make this checkable: the click path
has five exits and four of them send nothing landside, so the caller flushes
whatever the click did not take. Four tests in `host.dom.test.ts` cover the
four outcomes, and against the old code the first of them reproduces the
capture's two frames exactly — `['blur', 'click']` where `['click']` belongs.

### 49. Every gesture was a mouse gesture, on a client written for a phone

[§48](#48-two-things-a-google-chat-capture-had-to-say) fixed a blur that a press
should never have sent, and the reader who reported it added that the swipe
gesture did not work either. It did not, and neither did two other things, and
all three were one mistake: everything that *measures* a gesture rather than
naming its target listened on mouse events, and a phone does not produce mouse
events while a finger is moving.

Measured, in Chromium with touch emulated. A 94 ms tap:

```
pointerdown@1513  pointerup@1608  mousemove@1608  mousedown@1608
mouseup@1608      click@1608
```

and a swipe across a box the page has claimed with `touch-action: none`:

```
pointerdown@2224  pointermove ×4  pointerup@2477
```

with no mouse event of any kind. A swipe across a box that has *not* claimed it
is shorter still — `pointerdown`, one `pointermove`, `pointercancel` — because
the browser decides after the first move that the gesture is a scroll and takes
it.

Three things follow from the first listing and one from the second.

**The press this side reported was the gap between two events fired in the same
millisecond.** `pointerDownAt` was set on `mousedown` and read on `click`, and
on a phone those are the same instant: every tap in the Google Chat capture
reports a hold of 1 to 5 ms, against the 40–100 ms the server's own comment
gives for a human. And the server prefers a reported hold to its own plausible
one — `holdFor` only invents a duration when none was sent — so the measurement
that exists to make a replayed click look human was making every click from a
phone look like a machine. Worse than sending nothing, which is what the code
was written to beat.

**The approach was always absent.** `approachPath` needs two `mousemove`
samples inside 500 ms and a phone sends one, after the fact.

**The pan could not be made at all.** `beginDrag` hung off `mousedown` and its
samples off `mousemove`, so on a touchscreen the gesture never started. A map
was unpannable from the one device this project exists to serve, and nothing
said so: the drag was tested from the protocol inwards — `TestADragPansACanvas`
sends the frame the client is supposed to produce — and never once from the
reader's finger outwards, which is the half that was broken.

So the pointer path moved onto pointer events, which are the same stream for a
mouse, a finger and a pen, carry the press the reader actually made, and arrive
while it is happening. `pointerdown` starts the drag and stamps the press,
`pointermove` samples it, `pointerup` ends it, and `pointercancel` — the browser
saying it has claimed the gesture for a scroll or a fling — drops it without
sending, because what the reader did with that gesture happened plane-side and
sending the part that arrived would pan the page by however far the finger got
first.

One thing deliberately stayed on the mouse events: `pressing`, the flag §48's
held blur reads. What that brackets is the focus change, and the focus change is
a default action of the compat `mousedown` — it lands between `mousedown` and
`mouseup` on a finger exactly as it does under a mouse, which is why a tap into
a search result still holds its blur.

**And the canvas had to claim the gesture.** Pointer events alone are not
enough: without `touch-action: none` the browser takes the swipe after one move
and sends `pointercancel`, which is the second listing above. The declaration
goes on `[data-skyhook-static]` — canvases, and nothing else — where it is what
Leaflet and every embedded map set on themselves, for the same reason. The cost
is that a page cannot be scrolled by dragging from inside a canvas, so a canvas
taller than the screen has to be scrolled past from somewhere else; a decorative
full-bleed canvas is usually `pointer-events: none` and never a hit target at
all, and an interactive one is the case the whole feature exists for.

`TestPWAAFingerPansACanvas` is the test that was missing. A real touchscreen
emulated in the plane-side browser, real touch events dispatched at the glass,
the client's own listeners deciding what the gesture was, the frame crossing the
link, and the landside page reporting how far it was panned. Against the old
client it reports `offset: 0,0`.

### 50. A second Chat capture, and the instrument that was lying about it

The reader tried [§48](#48-two-things-a-google-chat-capture-had-to-say) and
[§49](#49-every-gesture-was-a-mouse-gesture-on-a-client-written-for-a-phone),
and sent a second bundle: *"bunch of rendering problems on chat"*. The icons
were fixed. Most of what the new bundle *showed* was wrong anyway — because
three of the four things wrong with it were wrong with the picture rather than
with the page.

`hashesAgree: true` again, so the DOM was never in question. Rendering the
bundle's own `mirror.html` and `expected.css` in a browser — the two artifacts
the readme calls reliable — settles each difference in turn.

**The message bubble had no colour.** In the picture the message the reader had
just sent was a soft grey blob; on their screen it is a blue pill. The delivered
CSS computes `rgb(211,227,253)` for it, which is the blue. The grey is
`--gm3-color-surface-container`, and the rule that names it is
`animation: 0.5s ease bgFade`, whose `0%` is that grey and whose `100%` is the
blue. The picture is an SVG drawn as an image, and an SVG drawn as an image is a
still: its clock never starts, so every CSS animation in it paints its first
frame. The bundle showed the reader a frame that existed for half a second,
minutes earlier, and said nothing about it.

`pinAnimationsInto` now copies the values a running animation is showing onto
the copy the picture is drawn from, taken from the live element at the instant
of the freeze, and stops the animation there — pinning alone does not survive,
because an animation outranks even an inline style in the cascade and the copy
would paint straight over it. Only the properties the animation itself names,
and only for elements that have one running, so a page that is not animating
pays a `getAnimations()` call and nothing else. The count goes in the bundle as
`pinnedAnimations`: a reader looking at a still of an animating page is owed the
fact that it was animating.

**The side panel had no icons.** Four blank strips where Calendar, Keep, Tasks
and Contacts are. Each is a `<div>` with `style="background-image: url(blob:…)"`
— a background the host writes onto the element rather than into the sheet, the
way `styleAttrImages` sends it. `inlineImages` traded blob URLs for data URIs in
every `<style>` and nowhere else, so those four resolved to nothing inside the
SVG and painted as absence. It now reads and rewrites style attributes too.

**The chat column was wider than landside.** Not a fault: `pointer` and `hover`
are answered plane-side by the reader's own device (§35's reasoning), so the
mirror lays the page out for a finger while the landside browser laid it out for
the mouse it thinks it has. Reproduced exactly by re-rendering the same document
with touch emulation on. It is the known gap §49 records — the landside browser
is a phone with a mouse — showing up as a visible difference between the two
halves of a bundle, and it is worth knowing before reading a capture as a bug.

And then the one that was real.

**The send button was empty.** Not in the picture only: in a faithful re-render
too. The glyph is `<span class="google-symbols">send</span>`, a ligature icon
from a family §48 taught the agent to keep — and the family has two faces. The
subset at `font-weight: 400` arrived. The other is the whole Google Symbols
variable font at `font-weight: 100 700`, 4,888,276 bytes, which the transcoder
refuses at its 1 MB cap: *"font is 4888276 bytes: source image too large"*.
Correctly refused; 4.9 MB is two and a half minutes of this link.

What happened next is the bug. Everywhere else a reference the client cannot
resolve becomes the transparent pixel — an image on its way leaves the box its
own colour, one that is never coming leaves it that way for good — and
`resolveCSSImages` applied that to every `url()` in the sheet, including an
`@font-face` `src`. A face whose `src` loads is a face, and a 1×1 GIF loads. So
the family gained a face that draws nothing, font matching preferred it to the
subset that worked for every weight it covers, and every icon drawn from
`Google Symbols` rendered as nothing at all. One empty button on a page whose
other icons were fine, which reads as one broken button rather than as a broken
font.

A face whose file is not here is now withheld instead. Font matching skips a
face that does not exist, which leaves the page whichever of its faces did
arrive, and the sheet is re-rendered whenever bytes land, so a font that was
merely late is written on the pass after it.

**And the pass after it never came.** Writing that test found the second half.
`applySnapshot` released the old document's blobs *after* applying the new one —
and applying it renders its stylesheet, which is what puts the images that
stylesheet names on the asking list. `releaseBlobs` then cleared that list, and
the record that anything had been asked, so the requests the new page had just
made were thrown away. Any later CSS delta re-rendered the sheet and quietly
repaired it, which is why a page that streams its CSS — Chat does — never showed
this. A page whose CSS arrives whole in the snapshot and never changes again has
nothing to render it a second time: every background, every icon and every
webfont it names simply never came, with no pending list to show for it. The
release now happens before the document it is releasing is replaced.

`TestPWACapturePicturesWhatTheReaderIsLookingAt` drives a page that is
mid-animation and carries a background on a style attribute, and checks the
picture for both. Against the old client the fade comes back `rgb(239,15,14)` —
the red it starts at — and the tile comes back the page's own grey.

### 51. The link was still paying for the page the reader had left

A capture, with the reader's own note on it: *"it seems to be jumping around
between pages. it eventually loads. what's happening?"* — a phone on a 412 ms
round trip, `hashesAgree: true`, `journalComplete: true`, nothing wrong with the
document at either end. Everything wrong with it was in the timeline.

```
02:47:17  click             the article on the Hacker News page
02:47:28  navigate  back    ← the reader gives up on it
02:47:29  navigate  back    ← and again, because nothing happened
02:47:51  image transcode failed  philo.gay/…/accel_dis.jpg  source image too large
02:47:56  navigate  back    ← a third time
02:48:03  client disconnected
02:48:06  a picture the queue would not take   457596 bytes
   …      thirty-one more of them
02:48:46  a picture the queue would not take   122650 bytes
```

The reader pressed back at 02:47:28. The article they were leaving was still
being fetched, decoded and encoded at 02:48:46 — seventy-eight seconds later,
across four pipeline workers, and for the first thirty-five of those the bytes
were going down the link the page they *were* waiting for had to share. Then the
client dropped, and the last forty seconds of that work were thrown away one
image at a time by a queue with nowhere to put it: `claimed=false`, thirty-three
times, several hundred kilobytes each.

Nothing in the server had ever been told a navigation happened. `worthSending`
knows about a tab that has *closed*; a tab that has merely gone somewhere else
is live, owes its images, and gets them. `Pipeline.process` opened
`context.Background()` with a sixty-second timeout and no relationship to
anything: the queues in front of it hold 512 and 4096 requests, so a request can
sit for a minute before a worker reaches it, and by then the page that named it
may be two navigations old.

**Requests are stamped with the page that named them.** `Tab.navEpoch` counts
document commits — `onFrameNavigated` on the main frame, and nothing else.
Deliberately not `docEpoch`, which counts snapshots: a page building itself
re-snapshots several times a second and every one of those is the same page, so
an epoch that counted them would call a page stale while it was still arriving.
`Tab.wantImage` stamps every request centrally, because a call site that forgot
would produce work nothing could ever decide about.

**The pipeline asks whether the page is still there**, through a `Relevance` it
is given rather than anything it knows itself — it has tabs and keys, and the
session has documents. It asks at the two points where the answer changes what
happens next: before a fetch it has not started, which is the free one, and
after one returns, before paying for the transcode and the link. And while the
fetch runs, on a one-second ticker, because that is where the seconds actually
are: a slow origin holds a worker for as long as it likes, and the direct path
waits 45 s before it gives up. Unstamped work is never stale — "epoch 0" means
nobody said, not "the first page".

Tab ids are handed out per session and every session starts at 1, so the router
that answers this takes every session owning the id and calls the work stale
only if not one of them is still on the document it was stamped with. The
delivery methods beside it can live with the ambiguity — an image is named by
the hash of its content, so a picture delivered to a session that did not ask
for it is one it already has under that name — but a *cancellation* cannot: one
reader navigating would call off what another is waiting for.

**And what was already queued goes back.** `Session.PageChanged` drops the media
class for that tab the moment a navigation commits. The queue is bounded in
frames and not in bytes — a thousand slots is however many megabytes the last
page happened to name — so "it will drain" was never an answer for a reader who
navigates twice while the first page is still shipping. Only media: the dom and
ctrl queues hold the new document and the announcement that says which page it
is, which are the two things the reader is waiting for.

Which turned up the older half of it. **A frame the queue threw away was
recorded as delivered.** `mayShipImage` claims a key by the act of deciding to
send it — that is §37's fix, and it is what stops two overlapping submissions
both spending the link on one picture — and `noteImageAnswered` gives the claim
back when the attempt fails. It runs from `onSent`, which runs from the writer.
Neither `dropTab` nor `dropIf` ever called it. So every picture dropped by
`drainOffline` when a client went out of coverage, and every one dropped by a
tab closing, left the ledger certain the client was holding it; the next resync
skipped it, and the reader kept a blank box until they thought to ask. Both
drops now settle what they discard as unsent.

`TestNavigationCallsOffThePicturesOfThePageBefore` drives it end to end against
an origin that will not hand over its picture until the test says so, so the
fetch is provably in flight at the moment the tab navigates. The picture is
above the fold, which means nothing has to ask for it — it would be pushed the
instant it was ready, and that it is not is the whole assertion. Against the
unwired build the picture crosses the link, and the test says so in under a
second.

Three things the same capture shows that are not this, and are not changed:

- **The client was gone for 116 seconds** after the disconnect at 02:48:03, with
  `reconnectDelayMs: 500` and a backoff that caps at 5 s. That is a phone with
  its screen off, not a reconnect loop failing.
- **The two halves disagree about the page's height** — 130275 px landside,
  140985 px in the mirror. That is the known gap §49 records: the landside
  browser lays the page out for the mouse it thinks it has, and the mirror lays
  it out for a finger.
- **A 4.9 MB variable font and eight multi-megabyte JPEGs were refused** by the
  transcoder's 1 MB and dimension caps. Correctly, and §50 covers what has to
  happen to the references when they are.

### 52. Three more of the same shape, and the one that had the reader's words

[§51](#51-the-link-was-still-paying-for-the-page-the-reader-had-left) is one
instance of a class: work that outlives the thing that asked for it, and that
nothing ever calls off. Reading the rest of the code for that shape found three
more. The first is the one whose symptom is the sentence the reader actually
wrote.

**A navigation that lost the race got to say where the reader was.**
`onFrameNavigated` starts a goroutine per commit, and nothing sequences them
against each other:

```go
go func() {
    t.syncHistory(ctx)                                                // a round trip
    t.emitState(protocol.TabState{URL: p.Frame.URL, Loading: true})   // this commit's URL
    t.applyBlocklist(ctx, p.Frame.URL)
    ...
}()
```

That URL is read from the event, not from the browser, and
`Page.getNavigationHistory` is a round trip whose latency this side does not
control — CDP calls are multiplexed by id (`Client.Call`), so two of them are
genuinely concurrent. The capture has two back presses 1.8 s apart, on a
landside browser that was at that moment fetching thirty-three images through
that same tab's session. The older run finishing second announces the page
*before* the one the reader is on, with that page's cached `canBack`/`canForward`
— which the comment right above it explains is how a back gesture gets dropped
on the floor — and with `Loading` forced back on, which nothing fires a second
time to clear. The tab sits on the wrong address under a spinner until the stale
run finishes the rest of its own chain, which on this link is seconds. Then it
applies that page's request blocklist to the page the reader is on, and asks for
a re-snapshot: 403,073 bytes on the wire, for the page in this very capture.

A run that has been overtaken now stops. `announceCommit` is the one step that
must not be taken late — everything else in the tail reads live state and is
merely redundant — so it is the step that carries the check, and it reports
whether there is any point going on. `onLoad`'s tail gets the same guard before
`recoverBlockedSheets`, which is a round trip per sheet for a page that has been
navigated past.

**The send queues were bounded in frames, not in bytes.** Four classes, 1024
slots each. A ctrl frame is a hundred bytes and an image frame is hundreds of
kilobytes, so one number meant "a hundred kilobytes" in one class and "several
hundred megabytes" in another — and the second is not a queue, it is hours. Each
class now has a byte budget as well, chosen by what being full costs rather than
by what fits: media is expendable and self-heals through the ledger and the
client's next ask, so 4 MB — the capture names thirty-three pictures in forty
seconds adding up to 6,448,130 bytes, for one article; dom is repaired only by a
resync, so 16 MB, which is around forty of the largest document this has ever
been pointed at. A message
larger than its whole budget still goes when nothing else is queued, because a
frame no queue can accept is worse than a queue briefly over its mark.

`Backlogged` had the same unit problem and it matters more, because it is the
one signal that stops optional work — following an animation, photographing a
canvas again — from piling onto a link that is not keeping up. Eight frames is
eight of whatever the queue holds: eight mutations is a moment, eight pictures
is a minute and a half at 250 kbps. It now answers on bytes too.

**A region-shot run outlived the page that started it.** Up to `shotFollowMax`
passes with a delay between each, a screenshot and a transcode per region per
pass, carrying straight across a navigation. Every pass reads the live document
so it was waste rather than error, and the page that arrives starts a run of its
own from its snapshot — there was never anything in the old run the new page
needed. The pass is now stamped with the page that scheduled it.

Two things with this shape that were checked and left alone: `recoverBlockedSheets`
and `RefreshState` both re-read live state on every call, so a late run states
the truth rather than yesterday's. The first was worth suspecting — it looked
like page A's stylesheets could be injected into page B — and it is not what the
code does.

### 53. The one picture the layout box could not make small

Everything the image pipeline does about size, it does by encoding into the box
the page lays an image out in. That lever is the right one nearly every time: a
1600px hero painted into a 320px column loses 96% of its bytes to `fit` alone,
and nothing about the result looks worse, because those bytes were never
reaching the reader's eyes.

It leaves one case, and it is the case that hurts. A picture whose box really is
that large — a full-width photograph, a screenshot at natural size, an
illustration the page draws at 2400px — has no smaller box to encode into, and
neither does one that arrives before the layout has a box for it at all
(`w` and `h` are zero, and `fit` reads that as "keep natural size"). Those
encode honestly to megabytes. There was no cap on the result at all: the only
size limits in the transcoder are on the *source* (24 MB of bytes, 40 MP of
pixels) and both of them drop the picture rather than shrink it. So the pipeline
had exactly two behaviours for a large image, refuse it or ship all of it, and
the second one is a megabyte — thirty-two seconds at 250 kbps, which is the
whole page's budget spent on one hero.

`MaxOutBytes` is the third behaviour, and it defaults to 1 MB. Over it, the
picture goes down a ladder until it fits: quality first, at 55 and then 30, then
size, at 0.7, 0.5, 0.35, 0.25 and 0.15 of the box. Quality first because the box
is the size the reader is going to see the picture at and resampling below it is
the one loss here that looking closer cannot undo — a photograph at q30 is a
photograph, a photograph at a quarter of its box is a thumbnail. The floor is 30
rather than 10 because below about that WebP stops looking soft and starts
looking broken, and a smaller picture at a decent quality reads better than a
full-size one made of blocks.

WebP is the target for three reasons, and the third is the one that decided it.
It is lossy at every quality the ladder names, so the ladder is a ladder. It
keeps an alpha channel while being lossy, which JPEG cannot, and transparency is
most of what a large PNG on a page is carrying. And every browser the client
runs in has decoded it for a decade. AVIF would be smaller again at every rung
and is not used: its encoder is minutes of CPU at these dimensions, and this
path runs while a reader waits.

Three details cost more thought than the ladder did.

**The handoff, not the codec, is the expensive half.** `cwebp` reads a file, so
each rung meant writing the image out as PNG first — and PNG-encoding a 2400px
source five times costs more than every `cwebp` run put together. The PNG is now
written once and `cwebp -resize` does the resampling, so a full ladder is one
handoff. On an adversarial input (2400×2400 of incompressible noise, which no
real page carries) a transcode that costs 2.9 s uncapped costs 7.1 s down to
1 MB; a real photograph of those dimensions fits on the first rung.

**Two quality steps, not four.** The ladder started at 75 and stepped through 40,
and neither rung decided anything. Anything that reaches here has already been
encoded at q70 and come back over the cap, so 75 is spent to come back barely
smaller than the thing it is replacing; and by the time 55 has missed, the gap
left is not one a step to 40 closes. Measured against the four-step ladder on
1200px and 2400px sources at caps of 1 MB, 256 KB and 64 KB, every case landed
on the same dimensions — the difference is that where 40 used to fit, 30 now
does, so the result undershoots the cap by more (150 KB rather than 196 KB under
a 256 KB cap). Bytes left on the table, in exchange for the encodes not spent
finding them.

**A rung that resampled changes what the metadata says.** `Result.W`/`H` are
what the client reserves space with, so they are now the dimensions of the bytes
that were actually chosen, not of the encode that was rejected. The blurhash is
taken before the ladder runs and stays right through it: it describes the
picture, and a rung only changes what that picture costs.

Vectors are not on this path at all — `passThroughSVG` returns before any
encoder is reached, which is correct twice over: an SVG has no quality to give
up, and rasterising one to hit a byte target would spend more bytes than the
markup does while losing the property that made it worth shipping. Neither are
webfonts, which have their own 1 MB refusal (§50) for the same reason: nothing
in a font can be made smaller here.

Without `cwebp` the ladder falls back to what the standard library ships — JPEG
for a picture with no transparency to lose, PNG for one that has some. PNG
ignores the quality column, so there the ladder is the size column alone, which
converges more slowly and occasionally not at all. The smallest encode found is
then shipped even though it is still over the cap: a picture that costs too much
is a better answer than an empty box, and it is the answer the reader had before
any of this existed.

### 54. The chat adapter had never seen a chat

[§5](#5-the-google-chat-adapter-scrapes-rather-than-using-the-chat-api) put all
the app-specific knowledge in a `Config` of CSS selectors, and the milestone
table said what that was worth: *"a starting point, not a validated set: they
need a session against the real app to tune"*. The captures from §48 and §50 are
that session — a reader's own Chat, DOM and all — and running the real extractor
with the shipped config against it is a one-line experiment nobody had run.

What it produces, from a conversation holding one direct message and two
messages in it:

```
spaces:   []
space:    {id: "", name: ""}
messages: [{id:"Kiyw_2D928g", space:"", author:"", text:"You",         ts:0},
           {id:"GRXYmFCJIKA", space:"", author:"", text:"Benson Tsai", ts:0}]
```

The message bodies are the senders' names. That is worse than the failure §5
promised — *"every selector failure degrades to found nothing"* — because it is
not nothing, it is junk, and the archive it feeds is append-only.

Four selectors were wrong, and three of them were wrong because the obvious
reading of Chat's DOM is the wrong one.

**A message is not `[data-message-id]`.** Chat puts that attribute on the
sender's `<span role="heading">`; the body, the timestamps and the reactions are
all outside it. So `qs(el, messageText)` found nothing inside the span, and
`text(… || el)` fell back to the element's own text — the sender's name — while
the timestamp element was equally out of reach and every message came out at
epoch zero. What holds one message is a `[data-topic-id]`. There are two other
kinds of those in a conversation — the "History is on" notice and an "Add
reaction" strip — and the one that is a message is the one holding a body, which
is what `[data-topic-id]:has([jsname="bgckF"])` says.

**The open conversation is not a selected roster entry.** Chat marks no entry as
selected at all, so `ActiveSpace` never matched and every message was filed
under the empty string. What does say which conversation is open is the
`role="main"` panel, which carries the same `data-group-id` the roster entry
does.

**A roster entry's name is not its text.** Its text is "Active Unread Benson
Tsai 1 Notification", spread across minified role-less spans that no selector
can tell apart. The name is on `[data-name]`, whose own text is empty — so
`scanSpaces` found the one chat, resolved its name to nothing, and dropped it on
`if (!name) return`. `label()` now reads a configured list of attributes before
falling back to text, which fixes the same failure for message authors: the
second message's author element has the name only in an attribute too.

**And `[aria-label*="unread" i]` matched "Mark as unread".** Chat's own context
menu, counted as an unread badge on every conversation in the roster.

The unread count is the one that is still unsatisfying. Chat's badge is a bare
`<span aria-hidden="true">1</span>` with no role, no label and no data
attribute, and six other aria-hidden spans sit in the same roster entry — so it
is named by a minified class, against §5's own rule, because there is nothing
else to name it with. It will rot, and when it does it reports no unread
messages rather than wrong ones.

`TestGoogleChatReadsARealChat` runs the real script with the real config against
`internal/adapter/googlechat/testdata/conversation.html`, which is that
conversation lifted out of the capture with nothing renamed. It asserts the
whole extraction rather than one selector at a time, because what failed here
was not one selector missing but four of them agreeing on a shape Chat does not
have. The payload it injects comes from `Extractor`, which is now the only place
that builds one — the config keys are written out by hand rather than taken from
the struct tags, so a test that assembled its own could have passed while the
adapter shipped something without them.

Two things this still has not seen: a space with several people in it, and Chat
inside Gmail, which is where `Config.URL` points. The selectors changed here are
all about the inner Chat app rather than the shell around it, so they should
carry, but nothing here proves it.

### 55. The sentence the mirror rearranged

A reader typed "the test message has gone through!" into Google Chat and Chat
sent "e through!the test message has gon". Not a dropped keystroke and not a
garbled one: every character arrived, and the sentence was assembled in the
wrong order — the ten characters after a Backspace, then the twenty-four
before it. The archive that keeps a copy is append-only, and so is the other
person's screen.

The input trace in the bundle names the moment exactly:

```
01:43:14.011  key       Backspace
01:43:14.013  setvalue  «24 chars»
01:43:14.118  text      «1 char»   ×10
01:43:15.202  key       Enter
```

The echo engine turns any edit that shortens the field into a whole-value set
carrying the caret ([echo.ts](../client/src/mirror/echo.ts)), which is right:
a deletion cannot be expressed as an insertion, and the caret is the only thing
that says where the next one goes. The client had the sentence right and sent
the caret with it. Landside, `setValue`'s contenteditable branch assigned
`textContent` and stopped:

```js
if (el.isContentEditable) {
  el.textContent = value;          // start and end never read
  el.dispatchEvent(new InputEvent('input', …));
  return true;
}
```

Assigning `textContent` destroys the text node the selection points into, and
Blink answers a destroyed selection by collapsing it to the start of the
editing host. `insertText` then does exactly what it is told — `execCommand`
inserts at the caret — and the caret is at offset zero. Ten characters went in
in order, at the front.

The input and textarea branches four lines above never had this bug, and the
reason is worth keeping: `setSelectionRange` is the only way to put a caret in
a field, so they always called it. A contenteditable's caret is the document's
selection, which is lost by accident and has to be restored on purpose.
`placeCaret` does that, mapping the client's character offsets onto the text
nodes with a `Range` the way `caretOf` measured them; `caretOffset` is its
inverse, and `insertText`'s fallback path — the one for a frame that does not
hold the browser's focus, which used to append blindly — splices at it now.

`TestTypingAfterAValueSetGoesWhereTheCaretIs` asserts both halves against a
contenteditable that reports its own contents: a caret left at the end, which
is the reported bug, and a caret left in the middle, which is the same code
path being asked a question that "the end" would answer by accident. Reverting
the fix turns it red with "said[ passedthe test]", which is the reader's
failure in miniature.

### 56. The conversation that would not come back to the bottom

The same capture's second complaint: the chat frame did not scroll. It
scrolled fine. The conversation was a scroller with 706px of content in a
642px box and a `scrollTop` of zero — parked at its oldest message, with the
one the reader had just sent 64px below the fold and nothing able to reach it.

A container's scroll position crosses as an op, and the op is emitted from a
`scroll` event listener. That covers every container the page moves while the
client is watching, and nothing at all about one that was already where it
belonged: a chat list pins itself to its newest entry as it builds, before the
agent's listener exists, and then never scrolls again. `FLAG_SCROLL` was
computed for exactly these elements and shipped in their node flags, where the
client read it only to report it back in a capture.

Which made this a property of resyncs rather than of pages. The server had
resynced by snapshot twenty seconds before the bundle was taken; the client
rebuilt the document from nothing, every scroller came up at the top, and the
landside page — which had not moved — had nothing to say about it. A reader who
scrolls back down never sees it again, which is why it reads as "sometimes the
chat frame doesn't work".

`Snapshot.Scrolls` carries `(node, x, y)` for the containers already scrolled
when the walk ran. The agent collects them in `serializeAttrs`, where the flag
is computed anyway, into an array that exists only for the length of a snapshot
— the mutation path shares that function, and a container that arrives already
scrolled inside an insert is reported by the scroll listener that is by then
watching it. The client offers each to `PatcherHooks.onScroll`, the same hook a
live op takes, so `followScroll`'s rules about who owns a scroller are written
once and apply to both.

`TestAScrolledContainerArrivesWhereThePageLeftIt` drives the real client in a
real browser, because the assertion is about a rendered box: `scrollTop` means
nothing in a model that was never laid out, and it was a model agreeing with the
server that hid this in the first place. The conformance fixture carries a
`scrolls` entry too — the position has exactly one chance to cross, and a key
that stopped lining up between Go and TypeScript would take a conversation's
newest message off the screen and change nothing else.

### 57. Cutting the icon font down to the icons

§56's capture had one more thing in it, and the first reading of it was wrong.
The 4.9 MB Google Symbols face was refused at the transcoder's cap — `font is
4888276 bytes: source image too large` — and that looked like the reason Chat's
markup carries words like `checkmark_chat_unread`. It is not: those words are
how a ligature icon font is written, the glyph is drawn at the ligature, and
Chat declares a second, 7 KB subset face for the same family that arrived
perfectly well. §49's withholding dropped the broken face, the small one won,
and every icon on that page rendered. Nothing was wrong.

What is wrong is the page that has no second face. Served an icon family whose
only file is over the cap, the mirror renders the names:

```
mirrored text: "a page whose icon font is too big to send  mark_chat_unreadstar"
```

which is not what the cap's own comment promises — "over the cap the page keeps
its empty boxes, which is what it had before". A toolbar of words is worse than
an empty toolbar, and both are worse than the icons.

So the font is cut instead. The lever nothing else here has is that a font's
bytes are its outlines: there is no quality to give up, but there are glyphs to
leave out, and a page draws about thirty icons out of a family that carries
thousands. Three things make that cheap:

- **The variable tables are the file.** Decompressed, Google Symbols is 13.1 MB,
  and 11.4 MB of it is `gvar` — the deltas for every weight and fill of every
  icon. Dropping the axes leaves the default instance, which is what the page
  renders anyway, and takes 87% off before a single glyph is considered.
- **Glyph ids stay where they are.** A ligature is a GSUB rule about glyph ids;
  renumbering means rewriting GSUB, `cmap`, `hmtx` and anything else that names
  one, and the Go font library that reads woff2 says `TODO: GDEF, GPOS, GSUB`
  about its own subsetter. Emptying an unwanted glyph's outline instead leaves
  every table still correct and costs the two bytes of its `loca` entry. Worse
  compression, far less to get wrong.
- **The names are already in the markup.** `mark_chat_unread` is the text of
  the element that draws the icon, so what to keep is a fact the landside half
  can read off the document.

That last fact has to cross, and the rule that names the file is what carries
it: the agent appends a `-sky-icons` descriptor listing the family's icon
names, and `fontIconNames` takes it off again before the rule goes anywhere
near the client. It is deliberately *not* a `--custom-property`, which is what
it was first — `stripUnusedVars` removes custom properties nothing reads, one
pass before the code that reads this one, and the font went on being refused
with nothing in any log to say why.

Together: 4,888,276 bytes of woff2 to 283,012 bytes of sfnt for the fourteen
icons a Chat toolbar draws, and Chromium renders them. The result is a plain
TrueType file rather than a woff2 — there is no Brotli encoder here — which was
measured before it was relied on: the same page renders identically with the
page's own rule still claiming `format("woff2")`, because browsers sniff the
bytes.

**The subset is cut once.** `fontKey` hashes the address together with the
sorted icon names, so the pipeline's existing cache does the rest: the same
page on the next flight hits `Submit`'s metadata check and never fetches the
font, let alone decompresses and rebuilds it; a page that has since found six
more icons misses on a different key and gets a font that covers them; and two
pages sharing an icon font cannot be served each other's cut.
`TestAFontIsFetchedOncePerSetOfIcons` counts fetches rather than subsets on
purpose — a cache hit means the bytes never left the origin, so nothing
downstream of the fetch can have run.

What this does not do: CFF fonts, whose outlines live in a charstring index
this does not touch; contextual ligatures (GSUB type 6), which an icon font has
no use for; and weights other than the default, which is the axis dropped with
`gvar`. All three refuse rather than guess, and refusing is what every font
over the cap did before.

`internal/imgproc/testdata/symbols-subset.woff2` is the fixture both suites cut
from — Chat's own 7 KB subset face, lifted out of the capture, 67 ligatures in
GSUB and Apache 2.0 like the icons it draws. The e2e test pads it past the cap
and asserts on what the browser drew: a ligature icon is one glyph an em wide,
and the same name in any other font is as wide as its letters. A model that
never shaped text cannot tell those apart, which is exactly how this stayed
invisible.

### 58. The back gesture the shell answered for a page it had already left

CI's shaped-link job failed on a test that had passed for weeks:

```
--- FAIL: TestTheBrowsersOwnBackAndForwardDriveTheTab
    navigation_test.go:261: the back gesture at the start of the tab's history
        was not let through: the shell answered it instead, or re-armed the
        trap under it
```

The diff it failed on had touched neither history nor navigation, and it
reproduced on no local run at any width. That combination — a test that only
fails on the slow, loaded runner — is usually a race the fast machine hides,
and it was.

The mirror's back button is drawn from `TabState.CanBack` and `CanForward`,
which are read out of the browser with `Page.getNavigationHistory` and cached on
the tab, because most state frames are partial and a frame that left them out
would read as "there is no history". The cache was describing the page before
the one the tab was on — not for a moment, but until the next navigation asked
again.

That is a lie the reader's shell believes, and which way it is wrong decides
what it costs. The test walks the tab to the start of its history and makes one
more back gesture, which the shell must *let through* to the browser so the
mirror can re-arm its `skyhook:here` sentinel under it. A tab still claiming a
page behind it makes the shell answer the gesture itself: the gesture is spent,
nothing moves, the trap stays unarmed, and nothing tells the reader anything
happened.

**The first fix was wrong, and CI said so within the hour.** It recorded which
page the flags had been measured on and blanked them for any frame about
another — "I don't know" reported as "no history", on the reasoning that a
gesture let through is the case the sentinel already handles. It made the
shaped job green and broke the ordinary one: `goHistory` declines a gesture it
cannot answer, so a tab whose flags had not arrived yet dropped the reader's
back button on the *first* page of every session. One assertion earlier in the
same test caught it. Both directions are wrong, which is what says the choice
was never between them.

The mechanism, found by logging every `emitState` and every history read
against the URL each was about:

- **A navigation's tail keeps asking after the reader has left.** The commit
  asks, and so do the load event, the settle, and any client refresh — for the
  *previous* page, since they were started under it. Those overlap, the stale
  question is answered last, and landing last it won. Nothing corrected it until
  the next navigation.
- **The browser announces a commit before its own history records it.** A tab
  that had just committed the index answered `Page.getNavigationHistory` with a
  single entry, `about:blank`. The commit's own question, asked at the right
  moment about the right page, still came back about the page before.

So the answer is stamped and checked rather than trusted. `applyHistory` takes
the nav epoch the read was asked under and drops an answer a commit has
overtaken, and `syncHistory` re-asks — up to five times, 20 ms apart, all of it
landside where nothing is waiting — while the browser's current entry still
names a page the tab is not on. Both are cheap: the retry did not fire once
across a full e2e run on a quiet box, and the epoch check is a comparison.

One theory got an experiment and lost first: that the history list lagged the
commit *by itself*, which a probe across the whole suite at `-parallel 8`
refuted with zero mismatches — before a later run showed the lag plainly. A
probe that finds nothing means the race did not happen that time, and treating
it as proof of a mechanism's absence is how the first fix came to be aimed at
the wrong half.

The same CI run reported a 210 ms tap measured at 356 ms by
`TestClickCarriesTheReadersOwnPointerData`. `pressHold` (§49) takes the press
dispatch's own round trip off the sleep so the release trip does not lengthen
the reader's tap, which holds while the two trips are alike and stops holding
on a box running eight browsers. That test is now a `newSerialHarness` test,
which is what this repository already says to do with an assertion that
measures the machine rather than the link.

### 59. Two messages, one bubble, and the channel that had never carried anything

A reader's capture, with a note on it:

> two of my messages got combined in the end: "the icons are still missing :/"
> and "message". also the icons are still missing.

Both are in the bundle, and neither is where it looks.

**What happened to the messages.** The landside screenshot shows what Google
Chat actually received:

```
the icons are still missing 🫤 message
```

The `:/` is an emoji. Chat's composer has an emoji autocomplete, and the Enter
that would have sent the message was spent choosing U+1FAE4 instead — which the
server log confirms from the other side, fetching
`notoemoji/17.0/1fae4/512.webp` 0.57 s after the Enter was replayed. Nothing was
sent. Eleven seconds later the reader typed "message" and pressed Enter again,
and the page sent what the composer had held all along.

That is Chat's own behaviour, and a reader sitting in front of Chat would see
the popup and know. What made it silent here is ours, in three parts, each of
which had to be found separately because each looked correct:

- **The ghost has no failure path.** On Enter the client draws the message
  optimistically, clears its own copy of the composer, and sends the key
  (`echo.ts`). `retireGhosts` removes the bubble when the real message turns up.
  Nothing removes it when the message never went, so it reads as sent for the
  rest of the session, over a composer the reader has been told is empty.

- **A contenteditable never reported its text.** The agent watched `INPUT`,
  `TEXTAREA` and `OPTION`, and `liveValue` read `el.value` — which is
  `undefined` on an editing host. So the composer that every modern chat app is
  built from shipped no `data-sky-value`, while the page's real changes to that
  subtree were held aside on purpose, because the echo engine owns it. The one
  moment the page rewrites what the reader typed is the one moment nothing could
  tell them.

- **And the channel had never carried anything.** `reconcileAttr` read
  `op.str`, which is the field a *title* op carries; an attribute op puts its
  name and value in `ref` and `ref2`, as references into the intern table.
  `op.str` is empty on every attribute op ever sent, so the reconciliation path
  had never once run — for a contenteditable, or for an `<input>`, or for
  anything. It also never checked *which* attribute had changed, so had it
  worked it would have offered a class change as the field's text.

The fix is those three, plus the thing the third one exposes. An editing host is
watched like a field, bounded by `LIVE_TEXT_MAX` so a document editor does not
ship its document on every change. `reconcileAttr` resolves the value from the
intern table — after the batch is applied, since a batch's own strings join the
table on the way in — and only for `data-sky-value`. A composer that comes back
non-empty after a ghosted send is the page saying the send did not happen, so
the ghost is taken back and the text put where the reader can see it.

And because that channel now carries something, it can also lie: every keystroke
is replayed landside, so the field's text comes back as the reader types, and
over this link it comes back several keystrokes late. Taking a late echo as
truth would put the field back to what it held a second ago and lose everything
since — the one thing local echo exists to prevent. A mutation says which input
provoked it, and an echo older than the reader's newest edit is dropped on that
basis, the same way §-49's focus handling already reasons about `applyingCause`.

One thing tried and removed: the agent first reported an editing host's text
only when the change was not an append, on the theory that typing explains an
append and the client typed it. It cost a dropped signal on the first try —
every string starts with the empty one, so a composer going from empty straight
to rewritten reads as an append — and measuring it afterwards showed the whole
rule was worth 18 bytes out of 2,642, because the live sweep already coalesces.
A watched editing host now reports on change, like the textarea it is standing
in for.

**What happened to the icons**, and why the capture disagreed with the reader.
`planeside/tabs/1/state.json` says `cspViolations: 50`. The policy was
`font-src 'self' blob:` — blob: added when §-48's fonts were refused, nothing for
the faces a page inlines in its own stylesheet. An icon font is exactly the size
that invites inlining, and Chat ships the subset that draws its toolbar and the
Starred shortcut as a `data:font/ttf` URI. It was refused as itself: the face
registers, the ligature fires, the glyph draws nothing.

The reader asked the sharpest question in the whole exchange — why the capture's
own screenshot showed the Starred icon they could not see. Because the plane-side
screenshot is not a photograph of their frame: `capture.ts` re-renders the frozen
markup inside an `<svg><foreignObject>` loaded as a `data:` image and draws it to
a canvas, in the shell's document, where the mirror frame's policy does not
apply. The picture drew the icon the reader was being refused. `capture.ts`'s own
comment warns about exactly this inversion — "a capture that disagrees with the
reader is read as the reader being wrong" — and here it hid a real bug rather
than inventing one. `img-src` has had `data:` all along; `font-src` now does too.

### 60. The half of the scroll conversation that was never spoken

A reader's note on a capture: *"emojis aren't loading as i scroll"*.

Google Chat's emoji picker builds the cells it needs and no more, so the mirror
had the 112 that happened to exist. Scrolling past them found blank space. The
server log is the whole diagnosis: between the click that opened the picker and
the capture twenty-five seconds later, during which the reader was scrolling, it
records nothing at all. Not a scroll, not an image, nothing.

Every part of the answer was already built. `ScrollEvent` has a `Node` field,
`HandleScroll` has a branch for it, the agent has `scrollTo(id, x, y)`, and §56
put container positions into the snapshot so a resync could restore them. What
was missing was at the plane-side end, in `onElementScroll`: it noticed the
container being scrolled, dismissed any open menu, recorded that the reader had
taken this scroller over so a server-driven scroll would not move them — and
sent nothing. The document's own scroll had been reported since the beginning.
A container's never was, which left the landside branch dead surface in the
same way `wheel` is: written, correct, and never once reached.

So a scrolled container is now reported the way the document is, throttled per
element on the same 250 ms, carrying its own height and scroll height rather
than the document's.

Two things were tried and dropped, both for the same reason. The first was
teaching `scrollTo` to treat the end of the reader's range as the end of the
container's, on the theory that a list which builds rows on demand is taller
landside than in the mirror. The second was the flush nudge the document's own
handler does near its end, so an `IntersectionObserver` fires on a page nobody
is painting. Neither could be made to matter: with the fixture rewritten to
watch a sentinel the way a real list does, the plain absolute position grows the
list, twice over, with nothing else added. Both would have been reasoning
shipped as code, and the shape of this bug is a warning about that — a whole
landside mechanism, carefully built, that nothing ever called.

One parity run failed during verification and its log was not kept; it has not
recurred in five runs since, including a repeat of the back-to-back conditions
it happened under. It is recorded here because an unexplained failure that is
not written down is one nobody will recognise the second time.

### 61. Auditing for the shape of the last three bugs

Three bugs in a row had the same shape: a mechanism built on one side of the
link that the other side never reached. The composer's text had a channel
nothing ever wrote to (§59). A scrolled container had a landside handler
nothing ever called (§60). Rather than wait for a fourth reader to find the
next one, the wire was walked end to end asking, of every field and every
entry point, *who writes this and who reads it*.

What that turned up, in order of what it costs a reader:

- **A checkbox's third state never crossed.** `indeterminate` is a property
  with no attribute behind it, so the serializer — which walks attributes —
  saw a box that was merely unchecked. That is the header tick of every
  partly-selected list there is, and the reader was shown the wrong answer to
  the only question it asks. Fixed, on all four surfaces that carry live state:
  the serializer, the sweep, the parity probe's read-only twin, and the
  patcher, including the removal path, because dropping an attribute does not
  unset a property (P-135).

- **`ImageWant.Have` is filled by the client and read by nobody.** Its comment
  says "already cached, do not send". The client only sends the frame when it
  is *also* asking for something, so a reader whose cross-flight cache holds
  every picture on a page says nothing at all, and a new session re-ships all
  of them. The ledger that stops this within a session — `imgSent` — starts
  empty on the next one. Left as a gap rather than fixed: telling the server
  what a warm cache holds without spending the link enumerating hashes is a
  design question, not an oversight.

- **`Op.Drop` is declared and written by nobody.** The field is `OpStyle`'s
  "rule indices to drop", and the client does not read it either, so a page
  that removes a rule from a constructed stylesheet leaves the mirror wearing
  it. Both ends absent means this is unbuilt rather than half-built, which is
  the better of the two.

- **`InSelect` is a deliberate no-op** — selection is native in the mirror —
  and so is `wheel`. The registry entry that catalogues this names `wheel` and
  `hover`, and `hover` has since been implemented (P-111); it should say
  `select`.

- **`__skyhook.node` and `__skyhook.stats`** have no caller. `diag` supersedes
  the second. Dead helpers, not dead features.

The audit also found the reason the last one was so slow to find. A failing
e2e test prints its own server log, and that log had become 93% Chromium's
opinion of the machine: dozens of lines about the absent system bus and the
absent GPU, written in the browser's first second, in a ring of 500 records
shared with everything the mirror has to say. The first thing this repository
tells you to read when a test fails had stopped being worth reading. Those
lines are dropped at the drain now (P-136), matched on message rather than
severity, because the browser calls all of them ERROR and some real ones are
ERROR too.

That paid for itself within the hour: with the dbus gone, the dumps of the
remaining flakes on a four-core box are dominated by repeated TLS handshake
failures, which is a lead rather than a wall. Left as a lead — it is not
understood yet, and the honest place for it is written down rather than
guessed at.

### 62. The half of §60 that only the bad link could see

§60's container scroll passed every desk and both unshaped jobs, and failed on
the emulated link the first time CI ran it there: three minutes, a mirror still
showing ten rows, and a server log that recorded `seq=0` throughout — no scroll,
no mutation, no error. Nothing had happened at all.

The report is throttled by 250 ms, and it read the box when the timer fired
rather than when the reader scrolled. In that window the server's own position
for that container can arrive — one from before the scroll, already in flight —
and `followScroll` applies it, because it declines to move a scroller the reader
has taken over *except* when they are sitting at the bottom of it, which is the
one place following along is the point. A reader who has just scrolled to the
end of a list is exactly there.

So the box went back to where it had been, and the report described that: the
page was told to stay put, the rows below were never built, and the reader
scrolled into blank space. On a fast link the stale position has usually already
landed before the reader moves; over 1.2 s of round trip it is still in the air.

The position is taken as the reader leaves it now, and a later scroll in the
same window replaces it rather than starting a second timer. Pinned in
`host.dom.test.ts` rather than by a shaped run: the race is a stale op landing
inside a throttle window, which fake timers can state exactly and a slow link
can only make likely.

The container branch of `HandleScroll` also says what it did now. Its first
failure in CI produced a log with nothing in it, and a silent path that has just
been given work to do is one nobody can debug — which is the same lesson as
§61's, arriving from the other direction within the hour.

### 63. The same window, the other half of it

§62 fixed the throttle window by taking the reader's *position* as they left
it. The shaped job failed again on the next run, the same way and for the
sibling reason: the report still looked the *node* up when the timer fired.

A document the server replaces inside that quarter of a second — a resync, a
navigation — rebuilds the patcher's map, and the element the reader scrolled
stops having an id at all. `idOf` returned 0, and the report gave up without a
word. That is the whole of why the failing job's log recorded nothing: no
scroll, no mutation, no error, `seq=0` for three minutes. The frame was never
sent, and nothing said so.

It was caught by instrumenting the two moments and running the loaded set until
it broke:

```
ZZscrollevent top=200 id=8
ZZreport id=0 y=200
```

Eight at the scroll, zero a quarter of a second later. The id is taken with the
position now, at the moment the reader scrolls, and the current id is preferred
only if there still is one.

Three things this cost, worth naming because they are the lesson rather than
the bug. The first fix carried the reasoning to the position and not to the id
sitting two lines away, which is what happens when a fix is aimed at a
symptom's shape instead of its cause. The second is that a silent give-up —
`if (!id) return` — is indistinguishable from a feature that was never called,
and this path had two of them. And the third is that both were only ever
visible on the emulated link, where the gap between what the reader does and
what the server has is wide enough to put a whole document replacement inside
it: the honest test for this is fake timers stating the race exactly, which is
now what pins it.

### 64. The address bar arriving before the buttons beside it

§58 fixed the tab's cached history flags describing the page it had left, and
the shaped job went green. The *unshaped* job then failed the same test, the
same way — `TestTheBrowsersOwnBackAndForwardDriveTheTab`, the back gesture at
the start of a tab's history answered by the shell instead of let through — and
the fix was in the wrong half of the mechanism.

A commit moves two things that have to travel together. The address arrives
with the event: `Page.frameNavigated` carries the URL, and the tab has it the
moment the browser speaks. What the browser thinks of the tab's history has to
be *asked for*, which is a round trip. The commit's own frame waits for the
answer and sends both, deliberately and with a comment saying why.

But the commit's frame is not the only one. The new page's snapshot arrives,
its loading stops, its favicon lands, and each of those calls `emitState` —
which filled a missing URL in from `t.url` and stamped every frame with the
cached `canBack`. Any frame emitted inside that window shipped the new address
with the previous page's history.

Going back to the first page of a tab is where that lands, because
`about:blank` stops loading in less time than one CDP round trip. The client
was told "you are on about:blank" and "you can still go back" in the same
frame. It drew the empty address bar and the start page — which is exactly what
the test waits for — and, believing the tab still had somewhere to go, answered
the reader's next back gesture rather than letting it through to the browser.
The gesture is spent, nothing moves, and the trap that guards the session is
left unarmed: the same reader-visible fault as §58, arriving through a frame
§58 never looked at.

§58's retry loop widened the window rather than opening it — up to five 20 ms
waits before the flags land — which is why the unshaped job started failing
where the shaped one had just been fixed.

So a frame emitted in that window now says nothing about where the tab is. The
URL goes out empty, which the client already reads as "unchanged", and the
flags beside it are the ones the client is holding — they cannot have moved,
because the only thing that writes them is a read for the page the tab is
actually on. The commit's frame, a round trip later, moves both together, and
is stamped as the one entitled to: stamped there rather than at the read, so
that a read the browser could not answer still lets the address bar move. A
stuck address bar is a worse failure than a history flag one page out.

The commit and the URL also move under one lock now. They did not, and the
window between them was a clipboard grant wide — another round trip to the
browser, during which a frame would have seen the new address beside the old
epoch and thought the pair agreed.

Two things worth naming. The failure evidence was `trap="skyhook:here"`,
`canBack: false`, three entries — and that is *identical* for the two faults it
could have been (the shell answering the gesture, or the shell letting it
through and re-arming the trap underneath). Only the order of the `popstate`s
tells them apart, and it is gone by the time anything asks; the test records it
now. And the second: §58 was a correct fix to a real bug that did not fix the
reported failure, because the mechanism had two ends and the reproduction only
ever showed the symptom. A green shaped job was taken as the fix landing rather
than as one of two paths closing.

### 65. Two messages sharing a body

`protocol.ImageMeta` says two unrelated things depending on who sent it. One is
a *description* — size, type, blurhash, and the alt text of the element that
referenced the asset — and comes from the snapshot that named the key or the
transcode that made it. The other is a *verdict*: `{Hash, Missing: true}`, sent
from the four places in the pipeline that can conclude the bytes are not
coming. None of those ever learned anything else about the key. Only
`abandon` holds a `Request` at all, and only for the tab that asked.

`internal/client` kept a table of these and replaced it entry for entry, so a
verdict landing on a described key erased the description — including the alt
text, which is the whole of what is left to show. `answerIfStranded` fires
whenever a client's `ImageWant` reaches a key that is neither done nor in
flight, which over the emulated link is any key the pipeline gave up on before
the want arrived. That is
`TestAnImageThatCannotBeFetchedIsReportedRatherThanLeftPending` failing on the
shaped job, on the one thing it asserts about the alt.

The mirror's own client never had this, and the reason is worth keeping: it
fills an element's alt in from the meta only when the element has none, because
the alt it renders came with the element. `meta.Alt` is a restatement, not the
source. The table on this side had no such second copy, and no rule about
partial frames either.

So it folds now instead of replacing: a field the frame does not carry is left
alone. `Missing` is the exception in both directions — it is the whole of what a
verdict says, and a later description (a re-snapshot, or the bytes arriving
after all) is the key getting its second chance and has to be able to clear it.

This is the same rule §58 and §64 are about, three frame types along: a partial
frame must not blank the fields it does not carry, and every place that keeps a
table of them has to say so. `TabState` says it in a comment on the struct
fields; `ImageMeta` now says it on the fold.

### 66. The reload gesture the phone shell was refusing to the browser

The compact chrome (§27) drops back, forward and reload from the toolbar and
puts all three in the ⋯ menu. Two of those are the right trade — the system's
own back gesture is already caught and spent on the tab's history (§14), and
forward is rare — and the third is not. Reload is what a reader reaches for
when a page arrived wrong, and on this link a page that arrived wrong is
minutes of waiting about to be spent again. It is the one control whose whole
job is *that did not work, do it again*, and it was two taps deep in a menu.

Every phone browser has bound reload to the same gesture for fifteen years, and
this shell had that gesture and gave it to nobody. Not by oversight, either:
the shell takes it away from the browser on purpose, because what Chrome does
with a pull at the top of the page is reload *the app*, and reloading the app
throws away every tab and every page already paid seconds for.
`body { overscroll-behavior: none }` has said exactly that, in a comment, since
the phone shell was written. So the phone reader had no reload button on screen
and a reload gesture whose only implementation was its suppression.

It is answered now, in three pieces: the mirror host measures the drag, because
it is the only code with the frame's document to listen to; `client/src/app/pull.ts`
says what a drag that far means; and the shell draws an indicator over the
frame and, on the release, spends it on `reloadTab`.

**Touch events, and not the pointer events everything else here uses.** The
input capture is built on pointer events for a measured reason (§27's own
note): they are one stream for a mouse, a finger and a pen, and they arrive
while the gesture is happening. But the same note records what the browser does
with a gesture it has decided is a scroll — one `pointermove` arrives and the
rest is delivered as a `pointercancel`, which is why a canvas has to declare
`touch-action: none` to be pannable at all. A pull down at the top of a page is
by definition a gesture the browser has already called a scroll. There is no
pull to be measured on that stream, and `TestPWAAFingerPansACanvas` asserts the
`pointercancel` that says so. Touch events keep coming for the whole of a
gesture the browser has claimed, which is why every pull-to-refresh ever
written is built on them.

**All four listeners are passive, and nothing calls `preventDefault`.** The
page is already at its top when a pull begins, so the browser has nothing left
to scroll and the drag costs it nothing to deliver. A non-passive `touchmove`
on the mirror document is the usual way to write this, and it would put the
main thread in front of every scroll in the mirror to buy a gesture that does
not need it. What would have been the browser's own answer to the overscroll —
the elastic stretch, drawn under the shell's indicator — is refused in CSS
instead: `overscroll-behavior: none` on the mirror frame's own root, which is
the frame's `:root` and not the page's `html`, so no page element's computed
style changes and the parity baselines do not move.

**Most of the work is deciding which gestures are not this one.** Four
questions are asked when the finger lands, and any "no" means the gesture can
never become a pull however it goes on to move: is this one finger, is the page
already at its top, is the finger on a canvas (which pans instead of
scrolling), and is anything between the finger and the document itself scrolled
down. The last is scroll chaining, asked as `scrollTop` up the composed path: a
drag inside a scrolled menu belongs to that menu until the menu is back at its
own top, and taking it for a reload takes the page out from under a reader who
was only looking at a list. Two more end a gesture already under way — the
finger going up or sideways, and a second finger arriving.

A fifth was found by asking what else a downward drag over the mirror already
means. On a phone the panel is a sheet over the page, and touching the page is
how the reader puts it away; a drag that does that is spent, and must not also
ask for the page again. The host already calls the shell's `dismiss` hook on
every press and had been discarding its answer, and the answer was wrong
anyway: `dismiss` reported only whether a *menu* had been open, so a press that
closed a sheet was reported as having dismissed nothing — which also meant
Escape with a sheet open closed the sheet and went on to reach the page. It
reports both halves now, and a pull whose press was spent this way is never
claimed. The question is asked at the claim rather than at the press, because a
finger produces two event streams and this must not depend on which of them the
browser delivers first.

**A release that will do nothing says so before it happens.** Offline, the
worker drops a navigate frame and no page is coming; busy, one already is, and
asking again throws away every byte of it that has landed. Both are invisible
from inside the gesture, so the indicator carries words: it never offers
"Release to reload" when it would not honour it, and says which of the two
reasons it is while the finger is still down. A pull that quietly did nothing
is indistinguishable from a pull the client never heard, and the reader's next
move after one of those is to make it again, harder.

The indicator is the shell's own furniture and is drawn over the frame rather
than inside it. Nothing this side paints may end up in the mirrored document:
that document is what a capture uploads, what the parity suite measures against
the landside page, and what the reader is being told is the page (§11e).

`TestPWAAPullDownReloadsThePage` drives the whole of it from the reader's end —
a real touchscreen emulated in the plane-side browser, real touch events at the
glass, the shell arming, and an origin that counts its own servings so that a
second arrival of the same page is visible at all. It also asserts the pull
that stops short: a 30-pixel drag raises the indicator, does not arm it, and
does not spend the link.

### 67. A drag is a gesture the page already described

Three of the parity corpus's new gesture pages (§68) failed the same way: a
pointer-event sortable list, an HTML5 drag-and-drop card, and a div-built
slider's held thumb all did nothing over the mirror, because the client
claimed a drag on exactly one kind of element — a canvas region — and
everywhere else a press-move-release is the reader selecting text. That rule
was right to be conservative: selection is native in the mirror, and a client
that hijacks it to guess at drags has broken the commonest gesture there is
to serve a rarer one. What was wrong was the premise that the client cannot
know which elements want dragging.

It can, because the page already said so, and the mirror already carries the
saying. Every drag widget declares its affordance in the material the wire
ships: a `grab` or resize cursor in its stylesheet, `touch-action: none` to
claim the gesture from the browser's scroller, `role="slider"` for the
machine-readable ones, `draggable="true"` for the browser's own drag-and-drop.
So the census (`dragSurfaceAt`) walks the composed path at pointerdown reading
computed styles and attributes, and claims the gesture only where the page
declared one — plain text and links never qualify, and selection stays whole.
The claim costs no wire bytes and no landside work: the evidence was already
plane-side.

What crosses is the canvas pan's frame, taught to name its other end. A drag
that finished on something says what — `node2` and `point2`, the node-id
discipline applied to the release — because the two halves lay the page out a
few pixels apart, and a drop delivered by viewport permille alone lands on
the list row *beside* the one the reader chose. The replay pins the last move
to the destination's landside box, which is the difference between "near the
right card" and "on it".

The browser's own drag-and-drop needed both halves handled specially. Plane
side there is nothing to claim: pressing a draggable element starts the
native drag, which cancels the pointer stream — so the host watches the drag
the browser runs (dragstart for the source, dragover to keep the frame a
legal drop target, drop for the landing) and sends the same one frame. The
reader gets the ghost image and drop cursor for free, from their own browser.
Landside the same asymmetry returns: synthetic mouse moves never start a
native drag, so for a `draggable` source the replay arms
`Input.setInterceptDrags`, lets Chromium report the drag the moves would have
begun, and completes it with real `dragOver` and `drop` events at the
destination — the page's own dragstart handler runs landside and rebuilds the
dataTransfer the wire never carries. One Chromium subtlety cost an afternoon:
a held move without `button: "left"` never begins a native drag — page JS
cannot tell the difference (a move's `.button` is 0 either way), but the
browser's drag controller can, and the interception starves without it.

The measurement found two latent bugs on its way in, both the same shape:
`pointerleave` and `mouseleave` do not bubble, but the host's *capture*
listeners on the document hear them for every element boundary the pointer
crosses — so a drag across a list of cards ended at the first card's edge,
and a press that wandered over a child span dropped its held blur (§48)
early. A canvas never noticed because a canvas has no children. Both
listeners now act only when the pointer really left the frame, which is the
crossing with no element being entered: `relatedTarget === null`.

`widgets/drag-sortable`, `widgets/dnd-html5` and `widgets/slider-track` pin
the whole path through the real client; `TestDraggingACardReordersASortableList`
and `TestADraggableCardDropsOnAZone` pin the replay;
`TestPWADraggingACardReordersTheList` pins the census recognising a reader's
own mouse. P-111's registry entry narrows to the hover third (§70).

### 68. Measuring the gestures before fixing them

The parity corpus measured the fidelity of pages that only need clicking,
because clicking, typing and scrolling were the only steps its executor could
perform. The gestures readers actually lose over the mirror — dragging a
card, holding a slider's thumb, resting a pointer on a menu, a wheel over a
zoom widget, a finger panning a map — were catalogued (P-004, P-006, P-111)
and measured by nothing, which is the state §61 warns about: mechanisms
argued over in prose with no instrument holding either side to it.

So the instrument came first. The executor learned five steps — `drag`,
`touchDrag`, `hover`, `wheel`, `dblclick` — each performed the way a reader
performs it, through the real client. A drag is real mouse events along a
sampled path with real time between them; a touchDrag is real touch events
and nothing else, because that is the stream a phone produces (§49) — and
the frame is instrumented and the gesture retried, because an injected press
is not a finger and a loaded machine can drop one whole. Two mechanics were
not obvious. Injected mouse moves never start a browser's native drag, so
the drag step arms `Input.setInterceptDrags` and completes an intercepted
drag with real `dragOver`/`drop` events — without which no executor can even
*ask* whether HTML5 drag-and-drop works. And a group shares one client
window, so each page parks the pointer on neutral chrome before measuring:
a previous page's hover would otherwise leave `:hover` state lying across
whatever this page laid out under the old pointer position.

Six corpus pages use the steps, every one proving its own wiring with a
click before blaming the gesture. Their first run wrote the ledger the fixes
would be held to: sortable, drag-and-drop, slider-drag and hover all failing
under their gaps; the slider's click-to-seek half passing (the trailing
click of a failed drag lands at the release point — a fidelity fact nobody
had written down); and hover-menu catching the mirror wearing `:hover`
state the landside page was never told about, as a style-dimension
divergence — hover is state, and the reader seeing state the page does not
have is exactly the kind of divergence the suite exists to name.

### 69. A finger arrives as a finger

The old known-gaps entry for P-006 explained why enabling touch emulation
landside was not a one-line change: a browser claiming `maxTouchPoints > 0`
invites the page to build its touch interaction model, and a mirror that
then replays every gesture as mouse events has lied to that model twice, in
opposite directions. Emulation had to arrive together with touch input to
feed it — which the drag work (§67) finally made possible, because gestures
now cross with the pointer kind that made them (`InputEvent.PT`).

So the claim and the input travel as one fact. The client's viewport carries
`touch`, read off `navigator.maxTouchPoints` at send time; `SetViewport`
turns on `Emulation.setTouchEmulationEnabled` landside when it is set, the
same call that already sizes the window and paints the scheme, because it is
the same kind of fact about the reader's machine. And a gesture stamped
touch is replayed as touch: a tap becomes `Input.dispatchTouchEvent` down,
the reader's own hold, up — no approach, no hover, because a finger is
nowhere before it lands — and a drag becomes the touch stream, same path,
same pinned destination, same anti-flick rest as the mouse replay. Two
gestures deliberately stay mice: a right-click or double-click is a mouse
idea whichever pointer made it, and a drag from a `draggable` source keeps
the interception path, because preserving the browser's own drag-and-drop
matters more than the modality of the pointer that made it.

`touch/drag-pan` pins all three layers through the real client — the pan
arriving at all, arriving as `pointerType: "touch"`, and the page seeing a
machine that admits to a touchscreen — and `TestAFingersDragArrivesAsTouch`
pins the replay at the protocol level. The phone client needed no change at
all: it was already sending its gestures from pointer events (§49), so the
day the viewport flag shipped, its taps and pans started arriving as what
they always were.

### 70. The wheel the widgets eat, and the rest that is a gesture

P-004's registry entry had already worked out where its own line was: wheel
deltas must not stream for documents, because scroll telemetry carries where
the reader is for a fraction of the bytes — and the entry admitted in the
same breath that this starves "what zoom-canvas widgets alone consume". The
gesture census (§67) is what turns that sentence into a predicate. A wheel
turned over a surface that claims gestures — the same cursors,
`touch-action: none` and roles that claim a drag — is a widget eating the
wheel, so the mirror stops scrolling (the widget landside will not either)
and sends it: ticks coalesced for a beat, one frame carrying the deltas, the
node, and where in its box the cursor sat. The point matters because every
map zooms about the cursor, so the replay parks the landside pointer there
before turning the wheel. A wheel anywhere else is untouched: native scroll,
telemetry, no new bytes. `widgets/wheel-zoom` pins the widget half through
the real client; the fixture's zoom reports which side of the stage the
wheel arrived on, so a replay that parks the pointer in the wrong place is
visible, and `TestSlidersAndHoverMenusReachThePage` pins exactly that.

Hover took the other half of P-111 out. Pointer moves are still never
streamed — that discipline predates this work and survives it — but a
pointer that comes to rest is not a stream, it is a gesture: the one every
hover menu, tooltip and preview card is built on, and the one gesture the
mirror answered only through a context-menu entry no reader discovers. A
rest of 400 ms within a hand's tremor, on an element the landside pointer
is not already parked on, sends one InHover naming the element and the
resting point inside it; the landside pointer parks there and the page's
own mouseover machinery does what it does. There is no other rate limit,
because the gesture is its own: a new hover needs 400 ms of stillness on a
new element, which a hand cannot produce faster than about once a second.
A dwell on the node a click just landed on says nothing — the click's
replay already parked the pointer there — and a finger never dwells, because
a finger is nowhere between touches.

The one measured surprise was already in the ledger before the fix:
`widgets/hover-menu`'s baseline had recorded the mirror wearing `:hover`
state the landside page did not have, as a style-dimension divergence. With
the dwell parking the landside pointer, both halves wear it, and the
dimension that caught the divergence is the one that now pins its absence.

### 71. The scroll ledger spoke for every box that had ever moved

The agent keeps a ledger of scroll positions — `lastScroll` — whose reason
for existing is silence: `ownScroll` writes the host's own nudges into it so
the scroll listener sees no change and reports nothing (§10). But the
throttled flush walked the whole ledger and emitted an op for *every* entry,
moved or not. One container scrolling re-announced every container that had
ever scrolled, every 250 ms flush, forever — and re-announced the host's own
`scrollIntoView` nudges with them, the exact positions the ledger exists to
keep off the wire. The client's `followScroll` guard discarded most of what
arrived, which is why nobody saw it: bytes spent to be refused, the same
shape as the wheel-echo entry in the audit (§61), found the same way —
reading the wire with the question "who asked for this".

The fix is a dirty set beside the ledger: a genuine scroll marks its box,
the flush emits the marked boxes only, and `ownScroll` unmarks — a host
nudge that lands inside the throttle window supersedes the reader-caused
report queued for the same box, because what would go out after it is the
nudge's position, not the reader's. `TestAScrolledContainerIsAnnouncedOnce`
pins the shape with two landside-scrolled boxes: moving the second must not
re-announce the first.
