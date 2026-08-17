# The Skyhook wire protocol

One connection, six logical channels, CBOR frames, zstd where it pays. The
authoritative definitions are `internal/protocol/frames.go` (Go) and
`client/src/shared/protocol.ts` (TypeScript); this document explains the shape
and the reasoning. Cross-language fixtures in `testdata/conformance.json` pin
the two implementations together, and CI fails if they drift.

## Transports

| Transport | When | Notes |
|---|---|---|
| WebTransport over QUIC | default | 0-RTT resumption, independent streams, datagrams |
| WebSocket over TLS | when UDP is blocked, and in tests | one socket, same framing above the header |

Both carry the same messages. Everything below the message header is identical.

## Message framing

```
byte 0      channel (high bit set = this stream carries exactly one object)
byte 1      codec: 0 raw, 1 zstd, 2 zstd + dictionary
bytes 2..5  dictionary id, little endian, codec 2 only
rest        CBOR payload
```

Length framing is the transport's job. WebSocket messages carry exact
boundaries. On WebTransport, each channel owns a unidirectional stream whose
first byte names the channel, followed by `u32` length-prefixed records; media
objects each get a fresh stream so a stalled image cannot head-of-line-block a
DOM diff.

Compression is per message, never streaming. That is a deliberate constraint:
the server keeps a ring buffer of unacknowledged frames and replays individual
frames after an outage, which a shared compression context would make
impossible.

Messages under 96 bytes and everything on the media channel skip compression —
a zstd header costs more than it saves on a 40-byte input event, and image bytes
are already compressed.

## Channels

| Channel | Id | Priority | Carries |
|---|---|---|---|
| `ctrl` | 0 | 0 | session management, acks, resync, tab ops, stats |
| `input` | 1 | 0 | semantic input events |
| `dom` | 2 | 1 | snapshots, mutation batches, style updates |
| `media` | 3 | 2 | images and favicons, one stream per object |
| `bulk` | 4 | 3 | dictionaries, adapter backlog |
| `telemetry` | 5 | 0 | scroll and viewport telemetry (datagrams, latest wins) |

Priority is enforced by the server's outbound scheduler: a burst of image bytes
can never delay a DOM diff or an acknowledgement.

Within the `dom`, `media` and `bulk` classes the scheduler is fair between
*tabs*, rotating between one queue per tab rather than draining a single line.
The tab the reader is looking at is served first and yields a turn every fourth
frame, so a page loading in a background tab arrives slowly instead of arriving
instead of the page being read. Order within a tab is preserved exactly — the
intern table makes it load-bearing.

`ctrl` does not rotate, and the difference is a guarantee rather than an
oversight: **its frames arrive in the order they were queued, across tabs as
well as within one.** Nothing on it is big enough to starve anything, and the
order carries meaning the plane side has no other source for — the server has no
opinion about which tab the reader wants to be in, so a client that asked for
two tabs decides from the order they are announced. Rotating that put readers in
the wrong tab.

Closing a tab discards whatever it still has queued, in every class. A close is
the reader saying they are no longer willing to spend the link on that page, and
on a link measured in seconds a queue is minutes; nothing is owed a repair
afterwards because the document it belonged to is gone on both sides.

## Frames

Every frame is a CBOR map with integer keys:

```
1: type      2: tab      3: seq
4: base      5: body     6: causedByInput
```

`body` is spliced in as raw CBOR (`cbor.RawMessage` in Go), so the ring buffer
can store and replay frames without re-encoding them.

### The DOM channel

**Snapshot** replaces a tab's document and resets the intern table:

- `strings` — every tag name, attribute name, attribute value and text run,
  deduplicated. Repeated minified class names cost two bytes after their first
  appearance, which is most of what a dictionary would have bought.
- `nodes` — `(id, parent, kind, ref, attrs[], flags)` in document order, so a
  patcher can build the tree in one pass.
- `css` — only the rules that matched something, minified.
- `images` — `(node, hash, w, h, blurhash)`; the hash is also the client's
  cross-flight cache key, so a warm client can render an image it was never
  sent.

**Mutation** is a batch of ops, emitted every 100 ms landside or flushed
immediately when the batch was caused by user input:

| Op | Purpose |
|---|---|
| `insert(parent, before, subtree)` | new content |
| `remove(node)` | with its descendants |
| `setAttr(node, nameRef, valueRef)` | `valueRef < 0` removes |
| `setText(node, textRef)` | whole replacement |
| `splice(node, off, del, insRef)` | minimal text edit — chat appends and typing |
| `move(node, parent, before)` | **critical**: React reorders are moves, not remove+insert |
| `styleRules(add[])` | incremental used-CSS |
| `focus`, `scroll`, `docInfo` | non-structural state |

`strings` in a mutation *extend* the intern table; the client appends them in
order. Frames carry `seq` and `base`; the client acknowledges `seq` on `ctrl`
and asks for a resync when `base` exceeds what it holds.

The intern table is positional and append-only, which makes **exactly once** the
requirement rather than a preference. A batch applied twice leaves the client's
table one entry longer than the server's, and every string reference after it
resolves to its neighbour: streamed text lands shredded, three characters at a
time, in the wrong nodes. Nothing catches it — the table is not part of the
document hash, and a re-inserted node reuses its id — so the tab stays wrong
until it is reloaded.

### Tabs

`TabOpen` carries `Navigate{url, ref, background}` and is answered immediately
with a `TabState{ref, url, loading}` naming the tab — before the landside page
has been built, and before anything has been mirrored. That ordering is the
point: the client draws a tab the moment the user asks for one and needs the id
a round trip before any content can exist, and a tab parked on `about:blank`
produces no lifecycle event to announce it with.

- `ref` is the client's own name for the tab it has already drawn. It appears on
  exactly one `TabState` per tab, the one that names it.
- `background` marks a tab the user is not looking at, so image priority stays
  with the page they are still reading.

Frames may arrive for a tab whose page is still being built; the server defers
them and applies them in order once it is.

### Sequencing, acks and resync

- The client acknowledges each applied batch with its own document hash.
- The server trims its replay ring on each ack.
- The ack is *not* the client's own record of what it has received. A batch
  travels from the network worker to the shell, into a sandboxed iframe, and
  back as an ack; several more arrive over the link inside that round trip. The
  worker decides "duplicate" and "gap" from what it has handed over, and only
  reports what has been applied. Deciding either from the acknowledged sequence
  makes every in-flight batch look like a gap and every replay sent in answer to
  that supposed gap look new — which is how one round trip's worth of frames
  turns into a permanently shredded page.
- Duplicates are dropped before gaps are considered: a batch at or below what
  the client already has is a repeat whatever its `base` says.
- One resync request per gap per second, per tab. The answer takes a round trip
  and the frames behind the gap keep arriving throughout; asking for each of
  them buries `ctrl` under requests for a repair already on its way.
- On a gap, hash mismatch, or cold resume, the client sends `Resync{tab, haveTo,
  reason}`. The server replays buffered frames when it can and they are smaller
  than 256 KB, and re-snapshots otherwise. A cold client (`haveTo == 0`) always
  gets a snapshot, because diffs are unappliable without a base.
- Every 30 seconds the server compares the client's reported hash with the
  landside document's. A mismatch triggers a resync and a log line, because
  silent divergence is what makes a mirror untrustworthy.

The document hash is the same FNV-1a walk in three places — the injected agent,
the Go replica, and the TypeScript patcher — so a mismatch means real divergence
rather than a hashing difference.

### Input

Input is semantic, never coordinates when a node id will do:

```
click(node, {button, modifiers})   server resolves the node and clicks it
key(key | text)                    Input.insertText for text, key events for control keys
setValue(node, text, start, end)   non-append edits, IME results, masks
submit(form, fields{})             one frame on submit, not per keystroke
scroll(tab, x, y, h, docH)         telemetry: image priority and infinite-scroll
```

Everything above drives the browser, and so is queued per tab landside and run
off the connection's read loop: a `Page.navigate` that has not committed must
not stop the client being heard in its other tabs. `navigate{action: "stop"}`
and `TabClose` are the two exceptions, and are the reason for the queue as much
as anything: both are about work already in flight, so both cancel it rather
than waiting behind it.

Every event carries `(seq, ts, expectDomSeq)`. Resulting mutation frames are
tagged `causedByInput: seq`, which the client uses for latency measurement and
echo reconciliation.

Resolving clicks by node id rather than coordinates is what makes the mirror
robust to layout drift between the mirrored document and the real page.

`drag(node, {point, path})` is the exception, and the exception proves the
rule. A canvas is reached through its pixels or not at all: there is no node
inside a map to click and none inside a game board to focus, so nothing the
rest of this list can say means "pan from here to there". What a map
understands is a button going down, moving and coming up, and the distances
between those points are the whole message. The path arrives in the same units
as a click's approach — viewport permille — which is what makes it comparable
across two layouts, and the press lands at `point` inside the node's box.
Plane-side the gesture is only claimed when the press was on such a region:
anywhere else it is the reader selecting text.

A click additionally carries what the reader's pointer actually did, because
the alternative is the server inventing it:

```
hold  17  ms the button was down
point 18  where in the node's box the pointer was, permille of its width/height
path  19  the approach: (x, y, dt) triplets, viewport permille and milliseconds
```

Permille rather than pixels: the reader's box was laid out with different fonts
and is rarely the size the landside box is. All three are optional — an adapter
or a keyboard activation has nothing to report, and the server falls back to a
plausible imitation. The whole set costs a few tens of bytes on a frame already
being sent, and it is the difference between a click a page can measure as
human and one it can measure as not.

### Media

`ImageMeta` (blurhash, dimensions, content hash) always ships first and is
tiny. Bytes follow immediately for above-the-fold images; below-the-fold images
wait for the client to say it does not already have that hash. That is the
mechanism that makes a warm cross-flight cache pay off.

A client asks for a hash exactly once, so the server has to say when an asset is
*not* coming: `ImageMeta.Missing` carries no size, no type and no blurhash,
because a fetch or a decode failed landside before anything could measure one.
Without it a failure is indistinguishable from slowness, and the element waits
on a placeholder for the rest of the session. It is an ordinary optional field —
a client that does not know it ignores it and behaves as every client did
before.

Two things that are not images travel this way, because what the channel
actually carries is "bytes identified by their content hash", and both need
exactly that:

- **Region shots.** A canvas, WebGL surface or video has no description a
  mirror can send — its content is whatever page JavaScript painted, and page
  JavaScript never runs plane-side — so the landside browser photographs the
  box instead. The metadata names the node it was taken from and carries `box`,
  the `(x, y, w, h)` in CSS pixels placing the photograph inside that node's
  border box: a canvas half off the viewport is shipped as the half that
  exists rather than stretched over the whole element. The hash is of the
  pixels, so a canvas the reader did not change costs one small metadata frame
  and no bytes at all.
- **Webfonts.** Kept only for a family the page draws private-use codepoints
  in, which is an icon font and has no substitute on any device. They reach the
  channel the ordinary way — an `@font-face` `src` is a `url()` in a stylesheet
  like any background image — and pass through the transcoder untouched, since
  there is no smaller version of a font to make.

### Adapters

Adapters are append-only logs: `AdapterRecord{kind, id, space, author, text, ts,
seq}` batched into `AdapterEvent`, with `backlog: true` marking a "while you
were gone" replay after a reconnect. The client keeps the archive locally, which
is what makes a chat open in milliseconds instead of seconds.

The client → server direction is a small command set: `send`, `sync`,
`markread`, `open`.

### Captures

A capture is one diagnostic bundle: both halves of a tab, frozen at the same
instant and zipped up landside. It is the only frame family that deliberately
spends the link — a screenshot in each direction is worth more bytes than any
mirror update ever is — so it happens when somebody asks, or when the server has
caught the two halves holding different documents.

```
client → Capture{reason, note}                       "something looks wrong"
server → Capture{id, reason, note, tabs[], maxBytes, screenshots}
client → CapturePart{id, name, data, more}           ... one per artifact/chunk
client → CapturePart{id, done: true}
server → CaptureDone{id, path, bytes} | {error}
```

Three details carry the weight:

- **`Capture` rides `ctrl`, and the resync that follows a divergence rides
  `dom`.** The server takes a capture *before* it repairs the tab, and ctrl
  outranks dom in the outbound scheduler, so the client hears "capture" before
  it hears "here is a new document". The client's handler freezes its mirrored
  DOM synchronously for the same reason: one `await` and the diverged document
  is gone.
- **Parts are chunked by hand**, at 32 kB. `bulk` is a message stream; a 400 kB
  screenshot sent whole sits in front of everything behind it for as long as the
  link takes to clear it.
- **A `.gz` suffix on `name` means the client gzipped the artifact**, and the
  server stores it decompressed under the name without the suffix. This is the
  one path where the client pays for bytes, and a mirrored document is mostly
  repeated class names.

`maxBytes` is the server telling the client how much of the link it may spend.
The server enforces its own ceiling regardless: a bundle is written on the
server's disk, and the server does not let a peer decide how much of it to use.

## Handshake

```
client → Hello{version, token, sessionId?, caps[], viewport, resume[], queued[],
               client, build}
server → Welcome{sessionId, resumed, tabs[], caps[], keepaliveMs, adapters[],
                 server, clientVersion, clientBuild}
```

`resume` carries per-tab `(seq, hash)`; `queued` carries input frames the client
buffered while offline, which the server replays into the pages **before** any
resync, so the state the client syncs to already contains their typing.

`sessionId` is offered on every connection, including the first one after a page
load. Tabs are landside and outlive the page that was showing them, so a client
that omits it is asking for a new session and abandoning whatever it had open.
The client stores the id from each Welcome and offers it back; a session it names
that the server no longer has is answered with a new one and `resumed: false`,
which tells the client the tabs it was holding are gone. A resumed session is
sent a `TabState` per tab immediately after the Welcome, because a `TabRef`
carries no history flags and a client that has just loaded has no other way to
learn them.

Capabilities are advertised, not assumed: a client that does not say `zstd` gets
uncompressed frames, and one that does not say `zstd-dict` never sees a
dictionary-coded message.

## Versioning

`Version` is a single integer that must match exactly. There is one user and
both halves ship together; a negotiation matrix would be more machinery than the
problem deserves. A mismatch closes the connection with `CloseVersionMismatch`,
which the client shows as a refusal rather than an outage — reconnecting cannot
fix it, and retrying is how a client ends up flapping between "offline" and
"connected" for ever.

### Which build each half is

The protocol version says whether the two halves can *talk*. It does not say
whether they are the same release, and on this client they routinely are not: the
plane-side app is a PWA served by its own service worker out of its own cache, so
a deploy landside changes nothing in a browser that already holds a copy — and
neither a reload nor any other request can see past that cache, because answering
out of it is the worker's entire job.

So the handshake carries build identity in both directions, and nothing is
refused over it:

- `client` and `build` in the Hello are the app's name with its version
  (`skyhook-pwa/0.1.0`) and the id of the exact bytes running. The id is a hash
  of the shell, compiled into it at build time and written to `version.json`
  beside it; it is also what the service worker names its cache after. The
  headless Go client sends `skyhookctl` and no build, and a blank build is never
  compared.
- `server`, `clientVersion` and `clientBuild` in the Welcome are the server's
  own version and the version and build of the app **the server is serving right
  now** — read from `version.json` under its web root, and re-read when a deploy
  replaces it.

A client compares `clientBuild` against its own and offers the reader the
upgrade; the server logs the disagreement, and a diagnostic bundle records both
ids, because a mirror bug is half plane-side and "which patcher drew this" is
not answerable from the document alone. See
[OPERATIONS.md](OPERATIONS.md#versions-and-updates).
