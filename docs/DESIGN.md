# Skyhook: Split-Architecture Browser for High-Latency, Low-Bandwidth Links

**PRD + Technical Design Document — v1.0**
Status: Draft for implementation · Owner: you · Target: personal use, single-user VPS deployment

---

# Part 1 — Product Requirements Document

## 1.1 Problem Statement

On in-flight wifi (RTT ≥ 1,000 ms, bandwidth ≈ 100–500 kbps, loss 1–5%, frequent multi-second outages), simple sites (Hacker News) work and modern SPAs (Google Chat, Gmail, Slack) do not. The failure is not primarily bandwidth: SPAs require 20–40 *serial* round trips across many origins (DNS, TCP, TLS, auth redirects, chunked JS, API calls, websocket upgrade) before first usable paint. At 1s RTT this exceeds the apps' internal timeouts, triggering retry storms and permanent spinners.

## 1.2 Product Thesis

Run a real Chromium **landside** (on a datacenter VPS with ~10 ms RTT to the open internet). It executes all JS and performs all origin fanout. The **plane-side client** executes zero page JavaScript and makes zero requests to the open internet. It receives a compressed, incrementally-updated mirror of the rendered document over a single multiplexed QUIC connection, and sends back semantic input events. Every page interaction costs at most one round trip over the bad link; most cost zero.

**Fidelity is explicitly sacrificed for usability.** Any rendering shortcut that preserves task completion (read messages, send messages, read docs, click links) is acceptable.

## 1.3 Goals

| # | Goal | Metric (over emulated 1.2s RTT / 250 kbps / 2% loss link) |
|---|------|--------|
| G1 | SPAs become usable | Google Chat: cold open → readable message list ≤ 8 s; warm open ≤ 3 s |
| G2 | Typing feels local | Keystroke → glyph on screen ≤ 50 ms (100% client-side) |
| G3 | Navigation is one round trip | Link click → meaningful content ≤ RTT + 1.5 s for typical pages |
| G4 | Scrolling is free | Scroll input → frame ≤ 16 ms, no network dependency |
| G5 | Link drops are non-events | 60 s outage → reconnect and resync ≤ 2 s after link restore, session and page state intact |
| G6 | Bandwidth headroom | Steady-state chat usage ≤ 5 kbps average; typical page load ≤ 150 KB on the wire |

## 1.4 Non-Goals (v1)

- Perfect visual fidelity, web fonts, pixel-exact layout.
- Canvas/WebGL/`<video>` support beyond static-image fallback.
- Multi-user service, sandboxing untrusted users, or productization.
- DRM content, WebRTC calls, file download/upload > 5 MB.
- Extension support, devtools, multiple simultaneous profiles.

## 1.5 Users & Use Cases

Single user (you), technical, owns both endpoints. Primary tasks, in priority order:

1. **Chat** (Google Chat, Slack): read new messages, send text replies. This drives G2/G6 and justifies the per-app adapter (§2.10).
2. **Reading** (docs, dashboards, news, webmail): mostly-static DOM after load. Drives G3/G4.
3. **Light interaction**: search boxes, form fills, button-driven web apps. Drives G2/G3.

## 1.6 Product Requirements

**P0 (must ship)**
- R1. Mirror-browse arbitrary HTTPS sites with clickable links, working forms, text selection, find-in-page.
- R2. Local echo for all text inputs; committed text reconciled with server truth.
- R3. Full-document transfer (not viewport-clipped) so scroll is local.
- R4. Session survival across link drops ≥ 10 min; landside page stays alive with its websockets.
- R5. Tabs (≤ 8), back/forward, URL bar, bookmarks.
- R6. Images: recompressed to layout size, progressive (blurhash placeholder → refined), prioritized below DOM updates.
- R7. Per-app adapter for Google Chat delivering G1's warm-open target.

**P1 (fast follow)**
- R8. Speculative interaction prefetch (§2.8). **Withdrawn** — built, then removed; see IMPLEMENTATION.md deviation 17.
- R9. Persistent cross-flight client cache (compression dictionaries, styles, images, adapter data).
- R10. Clipboard integration, basic file upload (≤ 5 MB, resumable).

**P2**
- R11. Periodic-JPEG fallback tile for canvas/video regions.
- R12. Second adapter (Slack or Gmail).

## 1.7 Explicitly Accepted Trade-offs

These are decisions, not open questions:

- **Security model = "the VPS is me."** All cookies, passwords, and sessions live landside. Typed passwords transit (encrypted) to the VPS. Acceptable for a personal, self-hosted deployment; unacceptable to ever offer to third parties. Mitigations in §2.12.
- **Page JS never runs client-side.** Sites whose core interaction is client-computed (games, drawing tools, spreadsheets with heavy local logic) will be degraded or unusable. Out of scope.
- **Hover states, JS-driven animations, and sub-100ms visual feedback loops are lost.** CSS-defined transitions/animations still run client-side for free.
- **Fonts:** system font substitution with metric overrides. Layout may shift a few px vs. true rendering. Nobody cares at 35,000 ft. *Amended in build:* an icon font is shipped instead, because it has no substitute — recognised either by the page drawing private-use codepoints in it, where every device is entitled to have nothing and the substitution is a row of empty boxes where the toolbar was, or by a matching rule asking for ligatures in it, which is how Material hides a glyph behind the word `mark_chat_unread` and where the substitution renders the word. See [IMPLEMENTATION.md §22 and §48](IMPLEMENTATION.md).

---

# Part 2 — Technical Design

## 2.1 Architecture Overview

```
PLANE SIDE                                   LANDSIDE (VPS)
┌──────────────────────────┐                 ┌──────────────────────────────┐
│  Skyhook Client          │                 │  Skyhook Server              │
│  (Electron/CEF shell)    │                 │  (Go or Rust host process)   │
│                          │   ONE QUIC/     │                              │
│  ┌────────────────────┐  │  WebTransport   │  ┌────────────────────────┐  │
│  │ Renderer webview   │  │   connection    │  │ Headless Chromium      │  │
│  │  - mirrored DOM    │◄─┼─────────────────┼─►│  (one tab per client   │  │
│  │  - client shim JS  │  │  streams:       │  │   tab, real profile,   │  │
│  │  - NO page JS      │  │   ctrl (rel)    │  │   real cookies)        │  │
│  │  - NO network      │  │   dom  (rel)    │  │  driven via CDP        │  │
│  └────────────────────┘  │   input(rel)    │  └────────────────────────┘  │
│  ┌────────────────────┐  │   media(unrel)  │  ┌────────────────────────┐  │
│  │ Chrome UI          │  │                 │  │ Adapters (Chat, …)     │  │
│  │ tabs/urlbar/etc    │  │                 │  │  direct API clients    │  │
│  └────────────────────┘  │                 │  └────────────────────────┘  │
│  ┌────────────────────┐  │                 │  ┌────────────────────────┐  │
│  │ Local store        │  │                 │  │ Transcoder (images),   │  │
│  │ dicts/cache/state  │  │                 │  │ zstd dict trainer,     │  │
│  └────────────────────┘  │                 │  │ session manager        │  │
└──────────────────────────┘                 └──────────────────────────────┘
```

Two processes, one protocol. The client is deliberately dumb: it renders, echoes input locally, and forwards semantic events. The server is authoritative for all page state.

## 2.2 Landside Server — Component Breakdown

**Host process (Go).** Owns the QUIC listener, session lifecycle, stream multiplexing, compression, and the CDP connection pool. Go over Rust for iteration speed; nothing in the host process is CPU-bound except image transcoding, which shells out to `libvips`/`libaom` workers.

That claim is about the Go process, and it held; what it was read as saying — that landside CPU is only ever transcoding — did not. The expensive landside work turned out to be in the *renderer*, in the injected agent: the used-CSS filter tested every rule against the document on every pass, which is quadratic in rules × elements and repeats after every batch of DOM changes. See [IMPLEMENTATION.md §35](IMPLEMENTATION.md). Landside CPU therefore has two sources, and they are diagnosed differently — the transcoder shows up in the server process, the mirror agent in a Chromium renderer.

**Headless Chromium**, launched with `--headless=new --remote-debugging-port`, one persistent user profile on disk (real login sessions persist across flights). One CDP target per client tab. Key CDP usage:

- `Page.navigate`, `Page.lifecycleEvent` — navigation control and load-state tracking.
- `DOMSnapshot.captureSnapshot` — initial full snapshot. Returns flattened node/layout/style arrays with an interned string table; this is the wire format's ancestor (§2.5).
- `DOM.enable` + mutation events, plus an **injected MutationObserver** via `Page.addScriptToEvaluateOnNewDocument` that batches mutations at 100 ms and reports them through `Runtime.bindings`. The injected observer is primary (CDP's own DOM events miss attribute/character-data granularity we want); CDP is the fallback and the source of truth for periodic integrity checks.
- `CSS.getComputedStyleForNode` — never shipped wholesale; see style pipeline below.
- `Input.dispatchMouseEvent` / `dispatchKeyEvent` / `insertText` — replaying client input.
- `Network.enable` — interception for the image transcoder and for blocking font/analytics/ad requests landside (saves server work and mirror noise; use a uBlock-style list).
- `Page.setDeviceMetricsOverride` — server viewport mirrors the client's reported window size, so layout and media queries match the device.

**Style pipeline.** Shipping computed styles per node is enormous; shipping stylesheets means shipping the CSSOM of a 3 MB app. Instead the server runs **used-rule extraction**: walk the CSSOM (via `CSS.enable`), test each rule's selector against the live DOM (`document.querySelector` landside), and emit only matched rules, minified, with unused custom properties stripped. Re-run incrementally when mutations add new classes. Empirically this cuts app CSS 85–95%. Client applies these as ordinary stylesheets, so cascade, `:hover`, transitions, and animations work natively and cost zero bytes at interaction time.

**Session manager.** A session = profile + set of tabs + compression state + client cache manifest. Sessions persist for 12 h after last contact (configurable). Landside tabs keep running while the client is gone — websockets stay connected, chats keep accumulating — which is the feature that makes reconnects magical (G5).

**Image transcoder.** Intercepts image responses. Pipeline: decode → resize to *rendered layout size* × devicePixelRatio (from layout snapshot) → encode AVIF quality 40 (photos) or lossless-ish WebP (UI sprites/screenshots, detected by palette heuristic) → emit `blurhash` (~30 bytes) immediately, full bytes later on the unreliable-ordered media stream. Images above the fold get priority; offscreen images deferred until scroll telemetry says they're near. GIF/video posters: first frame only, tap-to-request-more.

## 2.3 Plane-Side Client — Component Breakdown

**Shell:** Electron. (CEF is leaner; Electron wins on build velocity for a personal tool. Revisit if idle RAM matters.) Three parts:

1. **Chrome UI** (tabs, URL bar, connection health meter showing RTT/bandwidth/queue depth — you'll want this constantly).
2. **Renderer webview** per tab, locked down: `sandbox=true`, all network egress blocked via `session.webRequest` denylist (only `skyhook://` custom protocol resolves, served from the local mirror store), JS disabled for page content — except the **client shim**, a preloaded script that owns:
   - DOM patcher: applies snapshot + mutation stream to the live document.
   - Local echo engine (§2.7).
   - Input capturer: serializes semantic events (§2.6).
   - Scroll/viewport telemetry (throttled to 4 Hz, coalesced).
   - Blurhash renderer and progressive image swapper.
3. **Local store** (SQLite + flat files): zstd dictionaries keyed by origin+hash, image cache keyed by content hash, adapter message archives, session resume tokens, bookmarks.

Because the renderer is Blink, we inherit correct HTML/CSS layout, text shaping, selection, find-in-page, and accessibility **for free**. The entire client is a patcher plus an input serializer — this is the decision that keeps the project a few thousand lines instead of a rendering engine.

## 2.4 Transport

**QUIC via WebTransport (server: quic-go).** Rationale is entirely about the link profile:

- **0-RTT resumption**: after the first flight-segment handshake, reconnects send data in the first packet. With outages every few minutes, this is worth more than any codec choice.
- **Stream independence**: a lost image packet never head-of-line-blocks a DOM diff. With TCP at 2% loss and 1.2 s RTT, one drop stalls everything for seconds.
- **Datagrams** for input events and telemetry (loss-tolerant, latest-wins).

Streams and their delivery semantics:

| Stream | Reliability | Priority | Content |
|---|---|---|---|
| `ctrl` | reliable, ordered | 0 (highest) | session mgmt, tab ops, acks, resync |
| `input` | reliable, ordered per-tab | 0 | semantic input events (also mirrored on datagrams; first arrival wins by seqno) |
| `dom` | reliable, ordered per-tab | 1 | snapshots, mutation batches, style updates |
| `media` | unreliable-ish (per-object streams, cancellable) | 2 | images, favicons |
| `bulk` | reliable | 3 | dictionary updates, file transfers |

**Forward error correction** on the `dom` stream: XOR parity packet per 4 data packets when loss estimate > 1%. Spending 25% bandwidth overhead to avoid a 1.2 s retransmit round trip is the correct trade at this RTT; toggle dynamically from QUIC loss stats.

**Congestion control:** BBR, floor the pacing rate rather than trusting loss-based backoff — airline links exhibit random loss, not congestion loss.

**Keepalive** 15 s with 3-miss detection; on loss, client enters offline mode (renders cached state, queues input) and probes with 0-RTT resumes every 5 s.

## 2.5 Wire Protocol (DOM channel)

CBOR frames, zstd-compressed with per-origin dictionaries. Never JSON on the wire.

**Snapshot frame** (new document): interned string table (all tag names, attr names, attr values, text runs deduplicated); node array of `(id, parentIdx, tag|textRef, attrRefs[])` in document order; used-CSS bundle; viewport metadata; list of pending image placeholders `(nodeId, w, h, blurhash, contentHash)`. Content hashes let the client serve images from cross-flight cache before they're ever sent.

**Mutation frame** (batched at 100 ms landside, or flushed immediately if triggered by user input — see §2.7):
```
ops: [
  insert(parentId, beforeId, subtree),      // subtree in snapshot format
  remove(nodeId),
  setAttr(nodeId, nameRef, valueRef),
  setText(nodeId, textRef | splice(ofs, del, insRef)),  // splice for chat-log appends & typing
  move(nodeId, newParentId, beforeId),      // critical: React reorders are moves, not remove+insert
  styleRules(add[], removeIdx[])
]
seq: monotonic per tab
baseSeq: last seq this diff builds on
```
Client acks `seq` on `ctrl`. Server keeps a ring buffer of unacked frames for replay; on resync request (hash mismatch or gap), it sends delta-since-`baseSeq` or, past the buffer, a fresh snapshot. A Merkle-ish subtree hash exchange every 30 s catches silent divergence.

**Dictionaries.** Nightly (or on-demand) landside job trains zstd dictionaries per origin from that origin's recent frames. Dict updates ship on `bulk`, identified by hash; frames name the dict they used. First-ever visit uses a generic HTML dict baked into the client. Expected effect: minified-class-heavy DOMs (Google apps) compress 3–5× better than dictionary-less zstd.

**Budget check (Google Chat DOM mirror):** snapshot ~2,500 nodes ≈ 350 KB raw → ~45 KB dict-zstd ≈ 1.5 s at 250 kbps. New-message mutation ≈ 300 bytes on the wire. Steady state well under G6's 5 kbps.

## 2.6 Input Path

Client captures events at the semantic level, never raw coordinates when avoidable:

- `click(nodeId, {button, modifiers})` — server resolves nodeId → landside node → synthesizes a trusted click at its center via `Input.dispatchMouseEvent`. Robust to any layout drift between mirror and truth.
- `key(text | keySeq)` for the focused editable — server uses `Input.insertText` (fast path) with key events only for control keys (Enter, Tab, Esc, arrows).
- `submit(formId, fields{})` — form fills ship as one frame on explicit submit, not per-keystroke.
- `scroll(tabId, x, y)` — telemetry via datagram; drives image prefetch and infinite-scroll triggering landside (server synthesizes wheel events when client nears mirrored-content end).
- `select(nodeId range)` only when an action needs it (copy executes landside for cross-tab paste fidelity; locally the mirror's native selection already works).

Every input event carries `(seq, timestamp, expectedDomSeq)`. The server tags resulting mutation frames with `causedByInput: seq`, which the client uses for latency measurement and echo reconciliation.

## 2.7 Local Echo & Reconciliation (the G2 feature)

The focused editable element's subtree enters **client-owned mode**:

1. On focus: client marks the node owned; server-sent mutations touching it are held in a side buffer, not applied.
2. Keystrokes render locally, instantly, via the native contenteditable/input behavior (Blink does the work). Simultaneously each keystroke streams to the server (`input` stream + datagram mirror).
3. Server applies keystrokes to the true page; the page's JS does whatever it does (draft state, mentions popup, etc.). Resulting mutations come back tagged `causedByInput`.
4. Reconciliation: for mutations tagged with input the client already rendered, apply silently if server text == local text (the common case — this is just confirmation). If they differ (autocomplete, input masks, mention expansion), replace local content with server truth **but preserve the local caret** via diff-based caret mapping. Brief flicker is acceptable; lost keystrokes are not.
5. On blur/submit: ownership released, buffered server mutations replayed.

Special case, **Enter in chat inputs**: client optimistically appends the sent message to the mirrored message list styled as "pending" (client-local ghost node), clears the input, and removes the ghost when the server's authoritative message-list mutation arrives. This makes sending feel instant even though confirmation takes ~1.3 s.

Popup-dependent widgets (mention pickers, autocomplete dropdowns) inherently lag one RTT. Accepted; they still *work*.

## 2.8 Speculative Prefetch (P1) — withdrawn

> This was built as described and then removed. Fetching links the user never
> asked for is the traffic pattern origins bot-block on, and it was spending a
> logged-in session's reputation to save a round trip. See IMPLEMENTATION.md
> deviation 17.

Server-side heuristic ranks likely next interactions: visible links in reading order, elements with cursor-proximity from scroll telemetry, app-specific hints from adapters (e.g., "conversation list items"). For the top N=5, the server clones the tab's state cheaply where possible — for same-origin `<a href>` navigations it *actually navigates* a hidden pooled tab and pre-computes the snapshot diff vs. current page; for JS-driven clicks it does nothing (cloning arbitrary SPA state is unreliable — cut from scope).

Speculated diffs ship at priority 3 with a `speculative(interactionFingerprint)` header, cached client-side, dropped on LRU. On a matching click, the client applies the cached diff instantly and notifies the server, which fast-forwards the real tab. On mismatch, normal path. Net effect: link-follow within a site frequently costs **zero** perceived round trips. Budget guard: speculation bytes capped at 30% of link capacity and always preemptible by real traffic.

## 2.9 Reconnect & Resync (G5)

On link restore: 0-RTT resume → client sends `(sessionId, per-tab lastAppliedSeq, queuedInputEvents)` in the first flight. Server replays queued input into pages, then per tab sends either buffered mutation replay (short gap) or fresh snapshot (long gap / buffer overflow) — its choice, based on which is smaller. Chat adapters additionally push "while you were gone" message backlog. Target: usable within 2 s of link restore, no user action, no lost typed input.

## 2.10 Per-App Adapter: Google Chat (R7)

The mirror makes Chat *work*; the adapter makes it *pleasant*. Landside adapter process authenticates with the Chat API using your credentials (or, pragmatically, puppeteers a dedicated landside tab and scrapes via injected script where API scopes are annoying). It maintains: space list, per-space message log, membership, unread counts — synced to a client-side SQLite archive via a trivial append-log protocol (~100–300 bytes/message).

Client renders a native chat UI (simple HTML, local) over the archive: instant cold open from cache (G1 warm ≤ 3 s is really ≤ 300 ms), offline reading of full history, optimistic send with queued-while-offline outbox. The generic mirror remains the fallback for anything the adapter doesn't cover (threads with weird widgets, settings pages) — one tap opens the same conversation in mirror mode.

This dual-path design is deliberate: adapters where you live, mirror everywhere else. The adapter interface (append-log + outbox + auth broker) is generic enough that Slack (P2) reuses ~80% of the client side.

## 2.11 What Runs Where — Summary Table

| Concern | Landside | Plane side |
|---|---|---|
| Page JavaScript | ✅ all of it | ❌ never |
| Network to internet | ✅ all origins | ❌ blocked; QUIC to VPS only |
| Layout & paint | (headless, for metrics) | ✅ Blink renders mirror |
| Cookies, auth, passwords | ✅ profile on VPS disk | ❌ none stored |
| Scroll | synthesized for infinite-scroll | ✅ local, free |
| Text input echo | authoritative | ✅ instant local |
| CSS animation/transitions | source of truth | ✅ runs natively |
| Image decode/transcode | ✅ resize + AVIF | decode & display |
| Compression dict training | ✅ | stores & applies |
| Find-in-page, selection | — | ✅ native local |
| Chat adapter logic | ✅ API client | native UI over local archive |
| Session persistence | ✅ 12 h TTL | resume tokens |

## 2.12 Security Posture

- QUIC with mutual TLS: client cert pinned server-side, server cert pinned client-side. No other auth surface; VPS firewall drops everything but the QUIC port and SSH.
- VPS disk (profile, cookies) on LUKS-encrypted volume; Chromium profile additionally under a dedicated Unix user.
- Client local store encrypted at rest (OS keychain-wrapped key) — it contains message archives.
- Kill switch: client command wipes landside session + profile.
- Accepted residual risk: VPS compromise = full account compromise. Documented, personal-use-only.

## 2.13 Failure & Degradation Matrix

| Condition | Behavior |
|---|---|
| Canvas/WebGL/video region | Gray placeholder + "static frame" button → server ships one JPEG of the region (P2: 0.5 fps tile stream) |
| Mirror divergence detected | Subtree resnapshot, log for protocol fixing |
| Site fights headless (bot detection) | Server uses headful Chromium under Xvfb + standard stealth patches; escalate per-site to "just don't" |
| OAuth/login flows | Work via mirror (they're just forms); password typed → transits to VPS (accepted, §2.12) |
| Link down | Offline banner; reading, scrolling, adapter archives, input queueing all keep working |
| Server crash | systemd restart; sessions rebuilt from profile disk; client re-snapshots all tabs |

## 2.14 Milestones

1. **M1 — Wire a mirror (1–2 weekends).** CDP snapshot → CBOR/zstd → Electron patcher. Click + type forwarding, no echo. *Exit: browse HN and log into a simple site over an emulated 1.2 s / 250 kbps link (use `tc netem`).*
2. **M2 — Feel (2 weekends).** Local echo + reconciliation, ghost-send, scroll telemetry, image pipeline with blurhash, used-CSS extraction. *Exit: G2/G4 met; Google Chat usable end-to-end via mirror.*
3. **M3 — Survive (1 weekend).** QUIC hardening: 0-RTT resume, FEC, offline mode, resync protocol. *Exit: G5 met under scripted 60 s outages.*
4. **M4 — Chat adapter (2 weekends).** *Exit: G1 warm-open ≤ 3 s; offline history; outbox.*
5. **M5 — Polish.** ~~Speculative prefetch~~ (withdrawn, §2.8), dictionaries trained per-origin, tabs/bookmarks UX, metrics HUD. *Exit: G3/G6 measured and met.*

Every milestone is tested against the netem-emulated link, not real flights; real flights are the victory lap.

## 2.15 Key Risks

1. **Reconciliation edge cases** (IME, rich-text editors, input masks) — the hardest code in the project. Contained by scope: plain inputs + contenteditable chat boxes first; exotic editors fall back to server-echo (laggy but correct).
2. **React reorder storms** producing huge diffs — mitigated by `move` ops + keyed-list detection (match subtree hashes before emitting remove+insert).
3. **Headless detection** by Google — mitigated by headful-under-Xvfb and persistent real profile; residual risk of captcha friction on first login (do logins on the ground).
4. **Electron memory on a laptop in airplane mode** — measure at M2; CEF port is the escape hatch.
