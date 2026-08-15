# Prior art

Everything here has been tried before, by people with more time. This is what
each attempt knew that Skyhook did not, what was taken from it, and what was
deliberately left.

The rule for reading this document: a lesson only counts if it changed code or
closed a question. Admiration is not a finding.

## rrweb — session replay

**What it is.** A recorder that serialises a DOM and then a stream of mutations,
and a player that rebuilds them. Structurally the same problem as the mirror:
one side observes, the other side patches, and no page script runs on the
patching side.

**What it knew.**

*MutationObserver reports history, not outcome.* rrweb's observer notes are
explicit that records must be collected and only then interpreted, because a
batch describes a sequence of intermediate states and only the final one is
real. Two consequences it calls out by name: **dropped nodes** (added and
removed within the same batch, which must never be serialised) and correct
ordering of nested additions (a parent and its child both appearing as
additions, where serialising both duplicates work).

Skyhook decided per record. The costs, measured on the fixture:

| | before | after |
|---|---|---|
| Hoisting a 30-row block | 365 bytes, identities destroyed | 33 bytes, identities kept |
| 40 nodes added and dropped in one task | 59 bytes | 59 bytes |

The move case was the real one. `forget()` ran while handling the removal
record, so by the time the matching addition was read the node had no id left,
and the "emit a move" path — which the code had, and a test claimed to
cover — could never fire. Every keyed reorder re-sent its subtree. The dropped
node case was already handled, by accident: a `node.parentNode` check happened
to skip detached nodes. It is now deliberate, and covers the variant the old
check missed (a node moved into a subtree that was itself removed, which has a
parent but is not connected).

Identity matters beyond bytes. Input replay addresses nodes by id, so a rebuilt
subtree invalidates the client's handles, and anything the client held about
those nodes — focus, caret, scroll offset — goes with them.

*Attribute changes should be read at the end, not accumulated.* Only the final
value of an attribute matters in a batch. Skyhook pushed an op per record, so a
class attribute toggled every frame by an animation sent an op every frame. It
now records which attributes changed and reads their live values once.

*Passwords are not content.* rrweb ships masking as a privacy feature. Here it
is closer to a bug fix: the agent mirrored `input.value` back to the client for
every field including `type="password"`, so a typed password went into the
replay ring, into every resync, and into whatever the client persisted. The
client already has those characters — the user typed them — so sending them
back was pure cost. Masking now covers `type="password"`, the sensitive
`autocomplete` tokens (`current-password`, `new-password`, `one-time-code`,
`cc-number`, `cc-csc`) and an explicit `data-sky-mask` opt-in. The test is
narrow on purpose: a field wrongly judged sensitive would silently lose the
user's typing on a resync, so heuristics on names like "pass" are worse than
useless.

**What was already covered.** Shadow DOM and same-origin iframes are flattened
into the mirror rather than reproduced, which sidesteps rrweb's replay-side
complexity entirely. rrweb needs to rebuild shadow boundaries because a replay
must look like the original page to a debugging engineer; a browsing client only
needs it to *render* the same, and the browser does not care which tree the
nodes came from.

**What was taken but adapted.** rrweb serialises stylesheets by inlining them.
Skyhook goes further and drops rules whose selectors do not match the live DOM,
because rrweb pays a one-off cost per recording where this pays per page over a
1.2 s link.

rrweb also patches `Element.prototype.attachShadow`, because a shadow root
attached after the snapshot reaches no `MutationObserver` — the API reports
child lists, attributes and text and says nothing about shadow roots at all.
Skyhook had the bug rrweb had already fixed, and could not take the fix: the
agent runs in an isolated world, and the prototype it can reach is not the one
the page calls. The elements that were mirrored before their definition arrived
are polled instead, and re-read when they upgrade. Landside cycles are free;
only a real change reaches the wire. See
[IMPLEMENTATION.md §19](IMPLEMENTATION.md).

**Left deliberately.** rrweb's *continuous* canvas capture — periodic
`toDataURL` snapshots or a recorded WebGL command stream. Following a canvas
frame by frame still needs a video codec or a lie, and both are worse than
telling the reader this page needs the ground.

What rrweb was right about is that a canvas has to be photographed at all.
Skyhook takes one landside screenshot of the region on load and one after each
input, which is where its answer parts from rrweb's: rrweb records for a replay
that nobody is waiting on, so a fixed frame rate costs it only disk. Here the
frames cross the bad link while the reader watches, so the shutter is tied to
interaction rather than to a clock — nothing is spent while nothing is
happening, and what the reader just did is on screen a round trip later. Not
`toDataURL` either: a WebGL context without `preserveDrawingBuffer` reads back
blank, which is most of them. See
[IMPLEMENTATION.md §23](IMPLEMENTATION.md).

## The gap rrweb pointed at that we did not have

**Constructed stylesheets.** Not in rrweb's documentation as a lesson, but its
changelog carries the scar: adopted stylesheets missed in a full snapshot.
Checking Skyhook for the same bug found it. `document.styleSheets` does not
include `adoptedStyleSheets`, and every Lit-based web component ships its CSS
that way — so a component-heavy page arrived with structure intact and no
styling at all, which reads as far more broken than a missing rule. Both the
document and every shadow root are now checked.

## Blimp — Chromium as a thin client (2015–2017)

**What it is.** Google's own attempt: a Chromium engine on a server, a thin
Chromium client on the phone, with the **compositor layer tree and display
lists** mirrored rather than the DOM.

**What it knew.** Mirroring below the DOM buys exactness — the client renders
what Blink laid out, not an approximation — and costs everything else. The
layer tree is post-layout, so it is bound to the viewport and device
characteristics of the engine side, and it carries no semantics: no text
selection, no accessibility tree, no password manager, no reader mode. Blimp
also demonstrated the maintenance shape of the idea: it lived inside Chromium's
tree and needed core abstractions (`WebContents` assumes a Blink renderer) to
bend around it. It was removed.

**What was taken.** The negative result, which is worth more than a technique:
mirror at the DOM, not the compositor, and stay outside the browser's source
tree. Skyhook drives Chromium through CDP precisely so that a Chromium upgrade
is a package update rather than a rebase. The fidelity Blimp bought is what
Skyhook trades away on purpose — it is the same trade the design document
already made, and it is reassuring to find it made independently, then
abandoned for reasons that do not apply to a single-user tool.

## Menlo Security "Smart DOM" — remote browser isolation

**What it is.** The commercial descendant of the Blimp idea: layer tree and
display lists sent to a thin JavaScript client that reconstructs a
*semantically equivalent* DOM.

**What it knew.** Their argument for reconstructing a DOM rather than shipping
pixels is exactly Skyhook's: a bag of pixels has no `<input type="password">`
for a password manager to recognise, nothing for a screen reader to read, and
nothing to select or search. Two delivery ideas are directly applicable and are
**not yet implemented here**:

- *Viewport-first delivery.* They prioritise visible layers over off-screen
  ones. Skyhook prioritises images by viewport position but sends the whole
  document in one snapshot. On a long page that is the single biggest remaining
  win, and it is recorded as a known gap rather than hand-waved.
- *Deprioritising background animation.* Ads and spinners generate mutations
  forever. Skyhook now spends nothing on nodes that never settle in the DOM,
  which is a partial answer; a real one throttles by region.

## Opera Mini / OBML — the ancestor

**What it is.** Presto rendering server-side since 2005, sending a binary
markup (OBML) to a phone that could not have run the page. The commercially
proven version of this entire architecture, at a scale nothing else here
approaches.

**What it knew.** Two things Skyhook already does for the same reasons: a binary
format rather than compressed HTML, and page JavaScript running on the server
with only semantic events coming back from the client. And one thing it did that
Skyhook refuses: OBML pages were *snapshots*, with a hard two-second server-side
script budget and `setInterval`/`setTimeout` disabled, so anything that updated
itself simply did not work. That is the line between "a browser for a bad link"
and "a page-fetching service", and it is why Skyhook keeps a live session with a
running page rather than rendering a page to a document.

The OBML idea still worth stealing is **pagination**: OBML split long pages into
chunks fetched on demand. That is viewport-first delivery in 2005 clothing, and
it is the same known gap noted above — which is a good sign about the gap.

## browservice — Chromium for 1990s browsers

**What it is.** A CEF-hosted Chromium rendering to an off-screen buffer, sent to
ancient browsers as PNG or JPEG, with input travelling back in the URLs of the
image requests.

**What it knew.** How far the "no client capability at all" end of the spectrum
can be pushed, and what it costs: no text selection, no clipboard without a
bespoke control bar, no downloads without a confirmation dance, and audio that
plays on the server. Its own notes concede the keyboard handling is "hackish"
and the link is unencrypted.

**What was taken.** Nothing directly — it targets a constraint Skyhook does not
have (the client is a modern Chrome). It is included because it is the control
experiment: it shows precisely which affordances vanish when you give up the
DOM, and every one of them is an affordance Skyhook keeps for free by mirroring
structure instead of pixels.

## Mighty — the cautionary tale

**What it is.** A venture-funded attempt to stream a whole browser from the
cloud with GPU-backed video encoding, shut down in 2022.

**What it knew.** The economics, not the engineering. Per-user cloud rendering
priced against a laptop that is already paid for is a losing business, and video
streaming of a browser needs a *better* network than the one the user has —
which inverts the entire premise on a bad link. Skyhook's per-user cost is one
small VPS the user already rents, and its bandwidth goal is measured in tens of
bytes per interaction. The lesson is architectural discipline: the moment this
becomes a pixel pipeline, it needs the link it was built to survive without.

## Summary of what changed here

| Source | Lesson | Status |
|---|---|---|
| rrweb | Interpret a mutation batch at its end, not per record | Implemented; moves keep node identity, 365 → 33 bytes on the fixture |
| rrweb | Dropped nodes must never be serialised | Implemented deliberately (was incidental) |
| rrweb | Coalesce attribute changes to their final value | Implemented |
| rrweb | Mask password and one-time-code fields | Implemented |
| rrweb changelog | Constructed stylesheets are invisible to `document.styleSheets` | Implemented for the document and every shadow root |
| rrweb | A shadow root attached after the snapshot reaches no observer | Same bug, different fix: patching `attachShadow` is impossible from an isolated world, so un-upgraded custom elements are polled and re-read |
| Blimp | Mirror the DOM, not the compositor; stay out of the browser tree | Already the design; independently confirmed |
| Smart DOM | Viewport-first delivery of the document | **Known gap** — see docs/IMPLEMENTATION.md |
| OBML | Live session beats page snapshot | Already the design |
| OBML | Paginate long documents | **Known gap**, same as above |
| browservice | What pixels cost you | Control experiment; no change |
| Mighty | Per-user pixel streaming is the wrong economics | No change; reinforces the bandwidth budget |

## Sources

- rrweb observer notes, serialisation notes and changelog — <https://github.com/rrweb-io/rrweb>
- Blimp in the Chromium tree — <https://chromium.googlesource.com/chromium/src/+/HEAD/blimp/> (removed; mirrored at <https://github.com/crosswalk-project/chromium-crosswalk/tree/master/blimp>)
- Menlo Security, "The Mobile Isolation Era Begins: Smart DOM" — <https://www.menlosecurity.com/blog/the-mobile-isolation-era-begins-smart-dom>
- Opera Mini and OBML — <https://en.wikipedia.org/wiki/Opera_Mini>
- browservice — <https://github.com/ttalvitie/browservice>
