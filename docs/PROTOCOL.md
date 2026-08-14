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

### Sequencing, acks and resync

- The client acknowledges each applied batch with its own document hash.
- The server trims its replay ring on each ack.
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

Every event carries `(seq, ts, expectDomSeq)`. Resulting mutation frames are
tagged `causedByInput: seq`, which the client uses for latency measurement and
echo reconciliation.

Resolving clicks by node id rather than coordinates is what makes the mirror
robust to layout drift between the mirrored document and the real page.

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
client → Hello{version, token, sessionId?, caps[], viewport, resume[], queued[]}
server → Welcome{sessionId, resumed, tabs[], caps[], keepaliveMs, adapters[]}
```

`resume` carries per-tab `(seq, hash)`; `queued` carries input frames the client
buffered while offline, which the server replays into the pages **before** any
resync, so the state the client syncs to already contains their typing.

Capabilities are advertised, not assumed: a client that does not say `zstd` gets
uncompressed frames, and one that does not say `zstd-dict` never sees a
dictionary-coded message.

## Versioning

`Version` is a single integer that must match exactly. There is one user and
both halves ship together; a negotiation matrix would be more machinery than the
problem deserves. A mismatch closes the connection with a clear error.
