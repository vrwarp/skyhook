# Measuring rendering parity

This document is about how Skyhook knows the mirror shows what the page shows —
mechanically, repeatably, and with every known divergence written down. For why
the mirror diverges at all, read [DESIGN.md](DESIGN.md); for the catalogue of
what is built, [IMPLEMENTATION.md](IMPLEMENTATION.md).

## Why the document hash was never going to be enough

The one automated fidelity signal Skyhook shipped with is the three-way FNV
document hash: agent, server replica and patcher each fold (id, kind, name)
over their copy of the tree and compare. It is cheap, it runs everywhere, and
it is blind to attributes, styles, layout, images and text position — which is
to say, to most of what a reader sees. The implementation notes record six
shipped, reader-visible rendering bugs that all had `hashesAgree: true`. A
truncated stylesheet, a blank page styled into invisibility, a half-applied
theme: the trees agreed, the pixels did not.

The other signal was the maintainer's eyes, and bug bundles filed by hand.
That works — every one of those six bugs was found that way — but it is manual,
after the fact, and measures whatever pages the maintainer happened to visit.

The parity suite replaces neither. It is the third thing: a fixed corpus of
pages, each measured the same way on both halves, compared dimension by
dimension, and held against a checked-in baseline so the answer can only
change on purpose.

## How it measures

Both halves carry a **parity probe** — diagnostic-only, read-only, always on,
like the fingerprint it sits beside. The landside agent answers
`__skyhook.parityProbe(limit)`; the client answers
`window.__skyhookParity(tab)`. Each reports the same shape: per element, the
tag, the border box relative to its document root, a fixed 30-property
computed-style vector, its own text (first 24 characters), the attributes the
wire would carry, image state, and the resolved font; per document, the
viewport, scroll size and font status; plane-side additionally the pending
queues, missing images, substituted stand-ins, ghosts and CSP violations. The
three copies of the style-property list (agent.js, patcher.ts,
internal/parity) cross-reference each other the way the docHash constants do,
and the comparison refuses to run if their lengths drift.

`internal/parity` compares the two probes across **seven gated dimensions** —
structure, attributes, style, geometry, text, resources, interaction — plus an
**advisory pixel score** that is never gated and never in a baseline, because
the docs record expected cross-half pixel differences (font substitution,
photograph scaling) and a gate that cries wolf gets deleted. Every gated
result is deterministic: booleans, counts, and quantised buckets, no raw
floats.

What keeps seven strict dimensions from drowning in Skyhook's *deliberate*
divergences is the expected-divergence model, applied in this order:

1. **Global normalisation** (`internal/parity/normalize.go`): the by-design
   list. Wire-skipped attributes are skipped here too; image `src` compares by
   content, not URL; substituted fonts widen geometry tolerance ×4 on the
   nodes they substitute; page height compares as a ±2% band.
2. **Per-page `exclude`**: the dimension is not measured on this page, with a
   written reason.
3. **Per-page `expectedFail`**: the dimension is measured, asserted to fail,
   and tied to a gap id in the registry. An expected failure that *passes*
   fails the run with a lock-in message, because that means a gap closed and
   the records must say so.

## The corpus and the ratchet

The corpus lives in `test/parity/corpus/<group>/<page>/` — static pages, each
exercising one feature area, each with a manifest naming what it covers, what
gaps it measures, and what interactions to drive. The authoring rules are in
[test/parity/README.md](../test/parity/README.md). `make test-parity` runs
every page through the real pipeline: real Chromium landside, real client in a
real browser plane-side, tabs opened through the client's own UI, input
delivered as real mouse and keyboard events, a settle barrier between every
step.

Each page's gated results are pinned in
`test/parity/baselines/<group>--<page>.json`. **Any drift from baseline fails
the suite** — regressions and improvements alike. An improvement fails with
instructions rather than passing silently, because a silently-better result is
one nobody recorded and the next regression hides inside it. To accept a
deliberate change:

```sh
make parity-baseline   # reruns writing baselines + regenerates this doc's table
git diff               # the diff is the review artifact
```

A gated failure with no matching `expectedFail` fails the run even if the
baseline agrees, so a divergence cannot be catalogued by silence. The runner
writes a scoreboard (`scoreboard.md`, `scoreboard.json`) to
`$SKYHOOK_PARITY_OUT` (default `test/../.devdata/parity`); CI uploads it as an
artifact and posts the matrix into the job summary.

## The gap registry

`test/parity/gaps.json` is the catalogue of known divergence: every gap has an
id that never changes, a status, and — if open — either a corpus page that
measures it or a written reason none can. The corpus and the registry hold
each other to account at load time: a manifest naming an unknown gap fails,
and an open gap with no page and no excuse fails.

Statuses: **open** (a defect: the mirror should do better and does not),
**by-design** (a deliberate trade, measured so a change is noticed),
**fixed** (was open, now holds; its page pins the fix), **disproven**
(claimed, measured, found not to be so).

`P-0xx` ids are the gaps the implementation notes already admitted when the
registry was created; `P-1xx` are the ones the code audit behind this system
found. Several of the audit's claims were settled by measurement on the
suite's first runs: two were verified (P-114, P-115), one was disproven
(P-117), and five previously unknown bugs were found by the corpus itself
(P-119 through P-123).

The table below is generated from `gaps.json` and the corpus by
`make parity-baseline` (or `SKYHOOK_UPDATE_PARITY_DOCS=1 go test
./internal/parity -run Registry`); a unit test fails when it is stale.

<!-- parity:registry:begin -->
59 gaps: 13 open, 11 by-design, 34 fixed, 1 disproven.

| gap | status | what diverges | measured by |
|---|---|---|---|
| P-001 | open | A canvas that animates unprompted is not followed | — reader-initiated animation is pinned by test/canvas_test.go; the unprompted case is the canvasStreamEvery knob, a configuration choice rather than a parity property |
| P-002 | fixed | Icon fonts ship whole, never subsetted | — a byte-cost gap, not a rendering one: whether the glyphs draw at all is fonts/icon-ligature's business |
| P-003 | open | A font registered through the FontFace API cannot ship; its glyphs render as their ligature names | fonts/faces |
| P-004 | open | wheel and hover are protocol surface no client sends | — hover is now sent — the mirror menu's Hover-here entry is the client's InHover (P-111) — so this entry narrows to wheel: still protocol surface no client sends, deliberately, because scroll telemetry already carries where the reader is and streaming wheel deltas would spend the link on what zoom-canvas widgets alone consume; catalogued for the executor revision that takes on drag |
| P-005 | by-design | Password and sensitive-autocomplete values never cross the wire | forms/password |
| P-006 | open | The landside browser is a phone with a mouse: touch is never emulated, so maxTouchPoints is 0 and pointer-aware pages lay out differently per half | — both harness browsers are mice too, so the divergence cannot arise in-corpus; imported real pages carry the widened page-height band it causes |
| P-007 | fixed | File upload is not implemented | — |
| P-008 | fixed | Clipboard copy executes plane-side only; cross-tab paste fidelity is absent | — |
| P-009 | open | Find-in-page has no chrome-UI affordance | — a chrome-UI gap, not a mirror-rendering one |
| P-010 | open | A frame is only preemptible between messages; a large snapshot owns the link while it sends | — a latency property of the scheduler, measured by the netem suite's timings rather than by parity |
| P-011 | open | The document is delivered whole, never viewport-first | — a delivery-order property; parity measures the settled document, which is identical either way |
| P-012 | open | A closed shadow root is invisible: such a component mirrors as its light DOM only | shadow/closed |
| P-013 | by-design | Reader-preference media features are answered plane-side, so the two halves may genuinely lay out differently | — |
| P-014 | by-design | prefers-color-scheme is settled landside; changing it re-snapshots every tab | — |
| P-015 | by-design | srcset and sizes are dropped; one server-chosen rendition ships | images/responsive |
| P-016 | by-design | An animated GIF ships its first frame only | media/gif |
| P-017 | by-design | Canvas, WebGL and video are photographs: region shots, taken after input, never live | media/canvas |
| P-018 | by-design | Ad and creative networks are blocked landside, so their content never exists to mirror | — |
| P-019 | by-design | Alerts, confirms and prompts are auto-accepted landside and never shown to the reader | — |
| P-020 | fixed | Landside scroll is matched by range fraction, so IntersectionObserver-driven lazy loading fires approximately | textmisc/scroll-lazyload |
| P-021 | fixed | A mirrored sub-document is laid out plane-side against a box, not a viewport: reader fonts, no percentage heights, fixed and sticky resolve against the wrong thing | frames/viewport-units |
| P-022 | by-design | A cross-origin frame that cannot be mirrored stays a labelled box; past the depth or count limits, so does one that could be | — |
| P-023 | by-design | A cross-origin frame smaller than 64x32 gets no label at all — indistinguishable from a bug | — settled as the design: a frame below the label floor is an ad pixel or a tracking beacon, and naming its host would put furniture on every page that carries them. The affordance floor is FRAME_LABEL_MIN_W/H in agent.js; the frame element, its box and its src still cross, so a bundle can always tell the box from a bug |
| P-101 | fixed | A select change never reaches the landside page except through a form submit | forms/select |
| P-102 | fixed | setvalue and blur are dispatched to the top-level agent only; a non-append edit inside a mirrored cross-origin frame is silently lost | forms/frame-editing |
| P-103 | fixed | A blob: image source serialises as a landside blob URL the client can never fetch: a broken image with no fallback and no Missing notice | images/blob-src |
| P-104 | fixed | Favicons never travel: TabState.FaviconID is decoded by the client and set by nothing | — |
| P-105 | by-design | HTML comments are never mirrored, though KindComment exists on both halves of the protocol | — |
| P-106 | fixed | object, embed and applet are dropped whole with no stand-in: an unexplained hole where an iframe gets a labelled box | textmisc/object-embed |
| P-107 | open | Dead wire surface: TypeIntegrity, OpImage and ScrollEvent.Visible are never produced or consumed | — wire bookkeeping with no rendering consequence; recorded so the next protocol change deletes it rather than trips over it |
| P-108 | fixed | Downloads are unhandled: a download link writes a file onto the VPS and the reader sees nothing happen | — |
| P-109 | fixed | window.open and a plain click on target=_blank open a landside tab no client tab will ever show | nav/target-blank |
| P-110 | fixed | There is no print path: window.print does nothing for the reader | — |
| P-111 | open | mousemove-driven widgets — drag-and-drop, sliders, hover menus — have no input path | — needs a pointer-move step in the interaction executor; catalogued for the executor's next revision alongside P-004 |
| P-112 | open | An audio element is photographed as a still control strip; no audio ever crosses | media/audio |
| P-113 | fixed | DPR is never applied to image transcodes or region shots: every picture is soft on a 2x or 3x screen | — |
| P-114 | fixed | A prefers-color-scheme media query nested inside a plain style rule crosses with its question intact instead of being resolved landside | css/nesting-scheme |
| P-115 | fixed | The shell's font-src 'self' may refuse every blob: webfont the pipeline ships | fonts/faces |
| P-116 | fixed | An external SVG sprite reference resolves to a URL the sandboxed frame can never fetch: the icon renders as nothing | images/svg-sprite |
| P-117 | disproven | A style write onto a canvas permanently blanks its photograph until the reader touches something | media/canvas-restyle |
| P-118 | fixed | The GIF tap-to-play the transcoder's comment promises does not exist in the client | — |
| P-119 | fixed | The mirror's own :root color and font leak into pages that rely on UA defaults | css/ua-defaults, real/hn-front, real/wikipedia-article |
| P-120 | fixed | A top margin that collapses through the page's body is lost at the mirror boundary | css/margin-collapse |
| P-121 | fixed | The server's echo of the reader's own focus arrives a round trip late and yanks focus back into the field they have already left | — timing-dependent by nature: the executor settles before each step so every other page measures its own gap; the guard is exercised by every interaction page's focus traffic and the fix is pinned by the staleness comparison itself |
| P-122 | fixed | Top-layer state does not cross: a popover the page showed, or a modal dialog, is closed in the mirror | textmisc/disclosure |
| P-123 | fixed | In the script-disabled mirror a canvas is not a replaced element: it stretches to its container and its attribute aspect ratio scales it | media/canvas, media/canvas-restyle |
| P-124 | fixed | The document hash disagrees across languages on non-ASCII text: the agent and patcher fold UTF-16 code units while the Go replicas fold byte-indexed runes, so a healthy mirror of any page with a multi-byte character in a text node's first 32 characters reports divergence | — |
| P-125 | fixed | The mirror renders every page in standards mode: a quirks-mode page loses its quirks — table cells stop refusing the page font, and the geometry cascade follows | real/hn-front |
| P-126 | fixed | A shorthand property set with var() loses its longhands in used-CSS extraction: the CSSOM serialises them as empty strings, the wire carries “border-top-color: ;”, and the mirror's parser drops the declarations | css/var-shorthand, real/wikipedia-article |
| P-127 | fixed | The isolated-world injection race can leave a navigated tab unmirrored: when ensureWorld loses to a navigation and the settle-time retries lose too, the mirror keeps a three-node about:blank document while the page renders fully landside | — a CDP timing race, not reproducible by a corpus page on demand. Evidence from the thirty-article conformance sweep: bundle 00 (expectedNodes 3, agent.json started:false against a fully rendered page.html) and bundle 16 (the re-injected agent answers an empty fingerprint and serverHash 2166136261 — the bare FNV basis — while the 2068-node document it sent earlier renders plane-side); seven 'isolated world setup failed' warnings in one hour of local captures, most of which recovered on a later retry |
| P-128 | fixed | The three fingerprint writers disagree at the edges: DOM nodeType against protocol kind for container roots, lowercased names against clipPath, 32 UTF-16 units against 32 runes | — |
| P-129 | open | A collapsed-border table sizes its caption differently when its borders arrive after first layout: the mirrored figure runs 2px wide and a paragraph wrapping around the float re-wraps | real/wikipedia-article |
| P-130 | open | A quirks-mode body stretches to the viewport minus its own margins landside; the mirrored body is an inner box, and CSS cannot state a margin-box stretch for it | real/hn-front |
| P-131 | fixed | A tab's cached history flags described the page it had left: the back gesture at the start of a history was answered by the shell and vanished | — |
| P-132 | fixed | A message the page never sent was drawn as sent, and the next one was typed into a composer that still held it | — |
| P-133 | fixed | A font the page inlines as a data: URI is refused by the shell's CSP: the ligature fires and the glyph draws nothing | — |
| P-134 | fixed | Scrolling a container plane-side never reached the page: a list that builds its rows on demand never built any more | — |
| P-135 | fixed | A checkbox's third state never crossed: indeterminate is a property with no attribute, so a half-ticked box mirrored as empty | — |
| P-136 | fixed | A failing e2e test's log dump was 93% Chromium's complaints about the machine, so the records describing the mirror had been pushed out of the ring | — |
<!-- parity:registry:end -->

## Fixing a gap

A gap fix is a change that flips a corpus page's expected failure into a pass.
The suite will fail with the lock-in message; the fix commit then carries:
the code change, the manifest's `expectedFail` entry removed, the gap's status
flipped to `fixed` in `gaps.json`, and the re-baselined measurement pinning
the new behaviour. From then on the page is the regression test for the fix.

## Bundle triage and import

The parity suite measures the corpus; bug bundles measure the world. The
`skyhookctl bundle` tools connect the two:

```sh
skyhookctl bundle triage capture.zip            # which layer diverged, offline
skyhookctl bundle import -out test/parity/corpus/real/<name> capture.zip
```

`triage` mechanises the three-legged reading that
[OPERATIONS.md](OPERATIONS.md#diagnosing-the-mirror) describes by hand: the
agent leg (live page against the journalled frames), the patcher leg (journal
against the client's document), the CSS leg (rejected rules held against the
classes the mirror contains), the fingerprint cross-diff, and a replay of the
journal checked against the recorded replica hash. Exit code 0 is clean, 1 is
diverged, 2 is unreadable.

For breadth beyond the corpus, `scripts/bundle_sweep.py` captures a whole
list of URLs as bundles and triages the lot — a conformance run over markup
nobody wrote to be measurable. Its first run, over every article on one
day's Hacker News front page, is written up in
[test/parity/sweeps/](../test/parity/sweeps/); it hardened five triage
normalisation rules and filed two gaps. Sweep bundles contain the pages
themselves and are never committed — the write-up and what the sweep taught
the tools are.

`import` turns a bundle's landside document into a corpus page skeleton:
sanitised (scripts, handlers, hidden-input values, oversized `data-*`
payloads, external references), made hermetic (subresources rewritten to
`assets/`, fetched under size caps with `-fetch-assets` or replaced with
deterministic placeholders), and optionally scrubbed (`-scrub-text` replaces
every word with same-shape filler, for captures of private pages). The human
finishes the job: trim the page to the feature under test, write the
manifest's `covers` and `gaps`, confirm the attribution — `real/` pages
refuse to load without one — then `make parity-baseline`.

## What this deliberately does not do

- **No scheduled parity runs against live sites.** A live page changes under
  the measurement, so every result is an unreproducible one-off. The importer
  makes turning today's live page into a fixed corpus page cheap; that is the
  supported path.
- **No live `skyhookctl parity -url` probe.** The Go client has no layout
  engine, so it can answer for structure but not for style or geometry — a
  fourth probe surface for a third of the signal. The corpus and the bundle
  tools cover the need.
- **No gating on pixels**, as above: the score is reported, tracked in the
  scoreboard, and kept out of the baselines.
