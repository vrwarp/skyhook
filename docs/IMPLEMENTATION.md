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
| **M5 — Polish** | Speculative prefetch, per-origin dictionaries, tabs/bookmarks, metrics HUD | **Tabs, bookmarks and the HUD are built. Prefetch was built and then removed** — see deviation 17. Dictionary training is implemented and tested server-side but is not enabled on the wire — see below. |

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

Two things about frames are still not what a browser does. A cross-origin frame
cannot be read at all and renders as an empty box of the right size. And a frame
that navigates is picked up by a `load` hook that re-snapshots the document,
which is blunt: nothing about a document being replaced reaches the
MutationObserver watching the old one, so there is no diff to send.

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
is happening and stops entirely once every watched element has upgraded.

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

The general rule this is an instance of: a selector whose truth differs between
the two sides has to be answered landside and carried, never re-asked
plane-side. `:defined` is the sharpest case because it always differs.

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

### 22. A click has to be answered before the page can be

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
per interaction and none when idle. An animation running on its own is not
followed — that is the design's P2 tile stream, and it is not built.

The landside half of the same two reports was a different bug with the same
symptom. A VPS has no GPU, and Chromium no longer falls back to SwiftShader for
WebGL on its own: it blocklists the context, `getContext('webgl')` returns
null, and the site shows its own "something went wrong" — landside, before the
mirror has been asked for anything. The server's screenshot of that error was
faithful. `--enable-unsafe-swiftshader` is what lets the fallback happen; the
sandbox cost it names is the one this project already pays everywhere else,
which is that the landside browser exists to run untrusted code so the plane
side does not have to.

## Known gaps

These are unbuilt or thin, and are honest to-dos rather than deviations:

- **Canvas regions are photographed on interaction, not streamed.** See
  [§23](#23-a-canvas-is-pixels-and-pixels-have-to-be-photographed): a canvas
  refreshes when the page loads and after each input, so an animation running
  on its own is not followed. The design's P2 tile stream (§R11, 0.5 fps) is
  not built.
- **A second adapter** (design P2) is not built.
- **An icon font has no system substitute, and renders as tofu.** The agent
  drops `@font-face` and the design takes system substitution as the trade
  (§1.4, §2.1), which is right for text: a page set in a webfont reads fine in
  the reader's own. It is wrong for an icon font, where the glyphs live in the
  private use area and the ligature names — `search`, `directions` — mean
  nothing to any font a device actually has. The Google Maps capture that
  prompted §23 has thirty `.google-symbols` spans, and every one of them is an
  empty box plane-side. Shipping a subset of the codepoints a page actually
  uses is the fix, and it is not built.
- **File upload** (R10) is not implemented. Clipboard integration is the mirror's
  native selection plus cut/copy/paste on the context menu; copy still executes
  plane-side, so the cross-tab paste fidelity §2.6 wants from a landside copy is
  not there.
- **Find-in-page** works through Blink natively in the mirror, but there is no
  chrome-UI affordance for it yet.
- **The chat adapter's selectors are unvalidated** against the live app.
- **0-RTT resumption** is enabled but not asserted by a test; proving it needs a
  client that survives process restart, which the Go test client does not model.
- **Bookmarks** are stored and written but have no management UI beyond adding.
- **Installability is untested against a real install prompt**: the manifest,
  icons and service worker are all in place and the worker registers in a real
  browser under test, but nobody has clicked "Install" on a device yet.
- **The document is delivered whole, not viewport-first.** A snapshot serialises
  the entire DOM, and only images are prioritised by viewport position. Both
  Menlo's Smart DOM and, twenty years earlier, OBML's pagination send what is
  visible first and stream the rest — on a long document over this link that is
  the largest remaining win. See [PRIOR-ART.md](PRIOR-ART.md).
- **A closed shadow root is invisible.** `attachShadow({mode: 'closed'})` cannot
  be read from an isolated world any more than it can from the page, so such a
  component mirrors as its light DOM and nothing else. Late-attached *open*
  roots are handled (§19); closed ones cannot be.

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
| Hoisting a 30-row block (a keyed reorder) | 33 bytes | — |
| 40 nodes added and dropped in one task | 59 bytes | — |

The last two rows are the mutation-batch work described in
[PRIOR-ART.md](PRIOR-ART.md). The reorder cost 365 bytes and destroyed node
identity before it; the churn figure is unchanged, and is there to keep it that
way.

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
