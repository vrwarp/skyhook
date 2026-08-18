/**
 * The mirror host: one sandboxed iframe per tab, plus the input capture that
 * turns interaction inside it into semantic events.
 *
 * This is where the PWA earns the security property Electron gave us for free.
 * The frame carries `sandbox="allow-same-origin"` and, deliberately, *not*
 * `allow-scripts`: the browser refuses to execute any script inside it, so page
 * JavaScript cannot exist plane-side even if something slipped past the
 * patcher's sanitising. Keeping `allow-same-origin` is what lets this document
 * reach in through `contentDocument` and patch the DOM directly.
 *
 * `allow-forms`, `allow-popups` and `allow-top-navigation` are all withheld,
 * so the frame has no way to reach the network or leave the page on its own.
 */
import { ImageMeta, InputKind, Mutation, MutationOp, OpCode, Snapshot } from '../shared/protocol.js';
import {
  EchoEngine, asEditable, caretOf, modifierMask, setCaret, setValue, valueOf,
} from './echo.js';
import { IMAGE_CACHE, imageCacheKey } from '../shared/caches.js';
import { Patcher } from './patcher.js';

/** Base styles for a mirrored document, injected into each frame. */
const MIRROR_CSS = `
/* The mirror's own container, and nothing else.
   This rule used to say "html, body", which in this document names four
   elements rather than two: the frame's own root and body, and the page's,
   which arrive as ordinary elements inside them (see IMPLEMENTATION.md #30 —
   the mirror deliberately puts nothing between its body and the page's root,
   and this was putting a stylesheet there instead of a box).
   It cost every page that never touched its margins the eight pixels the UA
   gives a body: mirrored, such a page started hard against the corner while
   landside it sat inset, which is a difference in every measurement taken of
   it. And it painted the page's own root white, so a page whose ground is dark
   got a white frame in the margin the moment that margin came back.
   ":root" is the frame's root and can be nothing else, and ":root > body" is
   the frame's body for the same reason. What the page's own html and body
   should look like is a question for the page and the UA, both of which know
   the answer. */
:root { margin: 0; padding: 0; background: #fff; color: #111; }
:root > body { margin: 0; padding: 0; }
.skyhook-ghost { opacity: .55; font-style: italic; }
img { background-repeat: no-repeat; background-size: cover; }
/* An iframe's inlined document, rendered into the box that stands in for it.
   Scrollable rather than clipped, which a real frame with scrolling="no" is
   not. The box is the size the frame had landside, but the document inside it
   is laid out here — by this browser, in this browser's fonts, with no frame
   viewport for a percentage height to resolve against. Landside it fitted, so
   the clipping only ever bites when this side's layout has drifted, and then
   hiding the overflow deletes it silently. What falls off the bottom of a
   widget is its buttons: the reader is left looking at a captcha with no way
   to submit it and no indication anything is missing. A scrollbar is the
   honest version of the same failure, and it keeps the control reachable. */
[data-skyhook-tag="iframe"] { display: block; overflow: auto; scrollbar-width: thin; }
/* A frame whose document is on another origin. Nothing landside can read it —
   no agent runs in it and its contentDocument is closed — so the stand-in is
   empty however right its box is, and an empty box is invisible: Gmail's app
   launcher is one, and clicking the grid of dots appeared to do nothing at all.
   A reader who cannot have the content is owed the difference between "this did
   not come" and "your click was lost", which is the same bargain the HUD makes.
   Only frames big enough to have been worth looking at are named, and the box
   is drawn faintly: on a page of ad slots this is furniture, not an alarm. */
[data-skyhook-tag="iframe"][data-sky-frame] {
  display: flex; align-items: center; justify-content: center;
  box-sizing: border-box; overflow: hidden; padding: 4px;
  border: 1px dashed rgba(0,0,0,.18); border-radius: 3px; background: #fbfbfb;
}
[data-skyhook-tag="iframe"][data-sky-frame]::after {
  content: attr(data-sky-frame) " — not mirrored";
  font: 12px/1.4 system-ui, sans-serif; color: #767676; text-align: center;
}
/* The frame's own html/body are inside the root now, where a document rule
   cannot reach them: this is delivered through Patcher.baseRootCSS instead, and
   kept here only so the two are read together. */
/* A canvas: shipped as a photograph because its content is reachable no other
   way, and panned rather than clicked for the same reason.
   The touch-action declaration is what decides whether a finger on this box
   belongs to the browser or to the page. Left at its default it belongs to the
   browser: one pointermove arrives, the browser decides the gesture is a
   scroll, and the rest of the pan is delivered as a pointercancel. Measured,
   with touch emulated: pointerdown, pointermove, pointercancel and nothing
   after it — against pointerdown, four pointermoves and a pointerup with this
   declaration, which is the difference between a map that pans from a phone
   and one that does not. It is what Leaflet and every embedded map set on
   themselves, for the same reason.
   The cost is that a page cannot be scrolled by dragging from inside a canvas,
   so a canvas taller than the screen has to be scrolled past from somewhere
   else. A decorative full-bleed canvas is usually pointer-events: none and so
   never a hit target at all; an interactive one is the case this feature
   exists for. */
[data-skyhook-static] {
  background: repeating-linear-gradient(45deg, #eee, #eee 8px, #e5e5e5 8px, #e5e5e5 16px);
  touch-action: none;
}
/* A page on its way. The cursor is the one affordance that appears where the
   reader is already looking — on the link they just clicked — and it is the
   operating system's own word for "taken, working on it". Links only: over
   text, the selection cursor is still the true one. */
html.skyhook-busy, html.skyhook-busy a[href] { cursor: progress; }
`;

/**
 * Puts the mirror's document into standards mode, which it is not born in.
 *
 * A frame at `about:blank` has no doctype, and a document with no doctype is
 * in quirks mode. Every page Skyhook mirrors is a modern page that declared
 * one, so the mirror renders the whole web under rules its own pages were
 * never written for — and quirks mode is not a rounding error. Its worst
 * clause for a mirror is percentage heights: in standards mode `height: 100%`
 * against an auto-height parent computes to auto, and in quirks mode it walks
 * up the ancestors until it finds a definite height and uses that.
 *
 * On Google's reCAPTCHA that one rule is the difference between a working
 * challenge and an unusable one. The grid is a table at `height: 100%` inside
 * containers that are all auto; landside it is content-sized and square, and
 * in the mirror the percentage reaches the frame's own 580px box, the table
 * stretches to fill it, and the four rows go from 97px to 145px. The 192px
 * that appears between the tiles pushes the footer out of the frame, and the
 * footer is where VERIFY and SKIP live. The reader gets a captcha they can
 * solve and cannot submit.
 *
 * The doctype has to be written rather than appended: `compatMode` is fixed
 * when the document is parsed, so inserting a DocumentType node afterwards
 * changes nothing. Re-opening the document reparses it, and keeps the same
 * Document object the caller is holding.
 *
 * `srcdoc` would carry a doctype without this, but it loses a race with the
 * frame's own initial about:blank and lands the patcher on a document that is
 * about to be replaced. Re-opening in place is the boring option that works.
 * Its one visible effect is that the document's URL becomes the shell's rather
 * than `about:blank`; nothing resolves differently, because an about:blank
 * frame already inherited that same base URL from its creator.
 */
const STANDARDS_SHELL = '<!DOCTYPE html><html><head></head><body></body></html>';

function forceStandardsMode(doc: Document): void {
  if (doc.compatMode === 'CSS1Compat') return;
  try {
    doc.open();
    doc.write(STANDARDS_SHELL);
    doc.close();
  } catch { /* a mirror in quirks mode still beats no mirror at all */ }
}

/** What the shell needs to know to draw a context menu for a right click. */
export interface MenuTarget {
  /** Mirror id of the node under the pointer, 0 if it has none. */
  node: number;
  /** Mirror id of the editable field under the pointer, 0 if it is not one. */
  field: number;
  /** Pointer position in the shell's coordinate space, not the frame's. */
  x: number;
  y: number;
  /** Absolute http(s) URL of the enclosing link, if the pointer is on one. */
  link?: string;
  /** The link's own text, for a bookmark title. */
  linkText?: string;
  /** Content hash of the image under the pointer. */
  image?: string;
  imageAlt?: string;
  /** Text selected in the field, or in the document if there is no field. */
  selection: string;
}

/**
 * One mirror frame, frozen at an instant, for a diagnostic capture. Everything
 * here is a copy: nothing in it refers back to a live document, so the slow
 * half of a capture can work from it long after the frame has moved on.
 */
export interface MirrorFreeze {
  tab: number;
  /** The mirrored document as HTML — what the reader is actually looking at. */
  html: string;
  /**
   * The same document, as a detached clone.
   *
   * `html` cannot be parsed back into the tree it came from. An inlined
   * frame's document is a nested `<html>`/`<body>`, and the HTML parser has
   * nowhere to put those: it drops them and promotes their children, which
   * takes the frame stand-ins with them. Anything rendered from the re-parsed
   * markup is a picture of a box tree the reader never had — on a page built
   * out of frames, which is the kind most worth capturing. A clone is the same
   * copy the rest of this interface promises, without the round trip.
   */
  doc?: Element;
  /** Content hashes of every image the document references. */
  images: string[];
  /**
   * Blob URL -> content hash, for the image references the stylesheet carries.
   *
   * Background images reach the mirror as blob URLs, which resolve nowhere but
   * in this browsing context; a screenshot has to trade them back for bytes.
   */
  cssImages?: [string, string][];
  width: number;
  height: number;
  docHeight: number;
  scrollX: number;
  scrollY: number;
  /** What the host and patcher believe, for planeside/tabs/<id>/state.json. */
  state: Record<string, unknown>;
  fingerprint: { total: number; truncated: boolean; nodes: [number, number, string, number][] };
  error?: string;
}

/** Emitted by the host, forwarded to the server by the app shell. */
export interface HostEvents {
  input(tab: number, ev: Record<string, unknown>): void;
  scroll(tab: number, ev: Record<string, unknown>): void;
  applied(tab: number, seq: number, hash: number, epoch: number): void;
  wantImages(tab: number, hashes: string[]): void;
  /** A link the user asked to open in a new tab (middle or ctrl/⌘ click). */
  openLink(tab: number, url: string): void;
  /**
   * A gesture that should produce a new page in this tab: a plain click on a
   * link, or a form submitted. Sent as the event goes landside, which is a
   * round trip before the server can say a navigation started — the whole
   * reason the shell has anything to show in the meantime.
   *
   * It is an expectation and not a fact. A link the page treats as a button
   * produces exactly this gesture and no navigation, so the shell's wait is
   * bounded rather than open-ended.
   */
  navigating(tab: number, url?: string): void;
  /** A right click, for the shell to answer with Skyhook's own menu. */
  menu(tab: number, target: MenuTarget): void;
  /**
   * The user is doing something else now — a click, a scroll, Escape — inside
   * the frame. Events there never reach the shell's own document, so anything
   * the shell has floating over the mirror has to be told. Returns true if
   * something was dismissed, which makes the gesture that dismissed it stop
   * there rather than also reaching the page.
   */
  dismiss(tab: number): boolean;
}

/**
 * The hash behind an image the frame is showing.
 *
 * It is read from an attribute rather than from `src`, because what `src`
 * holds is a blob URL that says nothing about its content — see `blobFor`.
 */
export function hashFromImage(img: Element | null | undefined): string | undefined {
  const hash = (img as HTMLElement | null)?.dataset?.skyhookImg;
  return hash || undefined;
}

/**
 * A 1x1 transparent GIF, held by every image whose bytes have not landed.
 *
 * An <img> with no `src` — or one whose `src` does not load — draws its alt
 * text and a broken-image marker, which is worse than the blurhash the element
 * is already wearing. This loads instantly, from nothing, and shows nothing.
 */
const PENDING_PIXEL =
  'data:image/gif;base64,R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7';

/**
 * One `@font-face` block, for the check that decides whether to write it.
 *
 * A declaration block holds no braces of its own — the sheet arrives
 * CSSOM-serialised, so a `url()` here is a bare address or a base64 payload,
 * and neither carries one. A face this cannot find is a face left as written,
 * which is the same answer as before this existed.
 */
/** The keys a keyframe carries that describe the frame rather than the page. */
const KEYFRAME_META = new Set(['offset', 'computedOffset', 'easing', 'composite']);

const FONT_FACE_BLOCK = /@font-face\s*\{[^}]*\}/gi;

/**
 * Gives an image its box before its bytes exist.
 *
 * Until the bitmap arrives the frame holds a 1x1 transparent placeholder, and
 * an <img> with no dimensions is one line tall. Every image that lands then
 * pushes the text below it down the page — the reader loses their place once
 * per image. The agent sets width and height from the rendered layout box when
 * it has one; this covers the images it did not, using the size the transcoder
 * reports, and states the aspect ratio so a CSS-sized image reserves the right
 * height instead of the right pixels.
 */
function reserveSpace(el: HTMLImageElement, meta: ImageMeta | undefined): void {
  if (!meta?.w || !meta.h) return;
  if (!el.hasAttribute('width') && !el.hasAttribute('height')) {
    el.setAttribute('width', String(meta.w));
    el.setAttribute('height', String(meta.h));
  }
  if (!el.style.aspectRatio) el.style.aspectRatio = `${meta.w} / ${meta.h}`;
}

/** How close to the end still counts as "following along". */
const BOTTOM_SLACK = 64;

/** True when a scroller is parked at its end, where new content should follow. */
function atBottom(top: number, view: number, total: number): boolean {
  if (!total || !view) return false;
  return top + view >= total - BOTTOM_SLACK;
}

/** The gesture that follows a link in place, rather than somewhere else: the
 *  left button, held on its own. Every modifier means another tab, another
 *  window, or a download, and those are answered elsewhere. */
function plainClick(ev: MouseEvent): boolean {
  return ev.button === 0 && !ev.ctrlKey && !ev.metaKey && !ev.shiftKey && !ev.altKey;
}

/** A URL without its fragment: what two links have in common when they point
 *  into the same document. */
function stripFragment(url: URL): string {
  return url.origin + url.pathname + url.search;
}

/**
 * The element a fragment names, by the rules a browser follows: an id first,
 * then a named anchor. A fragment that names nothing here is not a fragment
 * this side can act on.
 *
 * The name is tried percent-decoded as well as written, because a link to a
 * heading in any language that is not English arrives encoded and the id it
 * names does not.
 */
function fragmentTarget(doc: Document, fragment: string): Element | null {
  if (!fragment) return null;
  const names = [fragment];
  try {
    const decoded = decodeURIComponent(fragment);
    if (decoded !== fragment) names.push(decoded);
  } catch {
    // Not valid percent-encoding, so what was written is all there is to try.
  }
  for (const name of names) {
    const byId = doc.getElementById(name);
    if (byId) return byId;
    // Only <a name> is a fragment target; a form control that happens to share
    // the name is not, and getElementsByName answers with both.
    const named = Array.from(doc.getElementsByName?.(name) ?? [])
      .find((el) => el.tagName === 'A');
    if (named) return named;
  }
  return null;
}

/** Lists a message could plausibly belong to, best first. */
const LIST_SELECTOR = '[role="log"], [role="list"], [role="feed"], ul, ol';

/**
 * The list a message typed into this composer would join.
 *
 * Searched outwards from the composer rather than down from the document,
 * because "the first list on the page" is not the transcript on any real chat
 * app — it is the navigation. Google Chat has ten elements at `role="list"` and
 * every one of them is in the left rail: the direct messages, the spaces, the
 * apps. Taking the first put the reader's own message in the sidebar, under
 * their list of conversations, which is a worse answer than not showing it at
 * all: an echo that appears somewhere the message will never be is not
 * reassurance, it is a second thing to be confused by.
 *
 * Outwards gets it right for the reason the layout is the way it is. A composer
 * sits inside the conversation it belongs to, and so does that conversation's
 * transcript; the sidebar is somewhere else entirely. So the first ancestor
 * whose subtree holds a list at all is the conversation pane, and the list
 * inside it is the transcript. If the walk reaches the body without finding
 * one, the page has no list this message could join, and the caller falls back
 * to no ghost.
 */
export function nearestList(from: HTMLElement): Element | null {
  for (let el: HTMLElement | null = from.parentElement; el; el = el.parentElement) {
    for (const found of Array.from(el.querySelectorAll?.(LIST_SELECTOR) ?? [])) {
      // A rich-text composer can hold a list of its own — the bullets the
      // reader is part-way through typing. Putting the echo inside the box the
      // message just left is the one place it certainly does not belong.
      if (!from.contains(found)) return found;
    }
    if (el.tagName === 'BODY') break;
  }
  return null;
}

export class MirrorHost {
  /** Not readonly: a tab is drawn before the server has named it, and takes
   *  its real id later — see `adopt`. */
  tab: number;
  readonly frame: HTMLIFrameElement;
  private events: HostEvents;
  private patcher: Patcher | null = null;
  private echo: EchoEngine | null = null;
  private doc: Document | null = null;
  private inputSeq = 0;
  private lastSeq = 0;
  private pendingImages = new Map<string, HTMLImageElement[]>();
  /** Canvas and video elements waiting for the bytes of a region shot. */
  private pendingShots = new Map<string, { el: HTMLElement; box: number[] }[]>();
  /** Where the photograph an element is wearing was taken from, so it can be
   *  hung again if the page rewrites the style it was painted into. */
  private shotBox = new WeakMap<HTMLElement, number[]>();
  /** Object URLs handed to the frame, by content hash. Revoked with the tab. */
  private blobs = new Map<string, string>();
  /** Hashes already looked for in the local cache, so a redraw does not ask
   *  again for bytes that have not crossed the link yet. */
  private probed = new Set<string>();
  /** Hashes a stylesheet references, which have no element to hang from. */
  private pendingCSS = new Set<string>();
  /** Hashes already asked of the server. */
  private requested = new Set<string>();

  /**
   * Hashes the server has said are not coming.
   *
   * Kept separately from `requested` because the two answer different
   * questions: `requested` stops a second ask for something still on its way,
   * this stops an element joining a queue that has already been told there is
   * nothing in it.
   */
  private missing = new Set<string>();
  private cssRefresh: ReturnType<typeof setTimeout> | null = null;
  private imageRequest: ReturnType<typeof setTimeout> | null = null;
  private ready: Promise<void>;
  private scrollTimer: ReturnType<typeof setTimeout> | null = null;
  /** URL of the document currently rendered, so a resync is distinguishable
   *  from a navigation. Deliberately not the same field as `pageUrl` below:
   *  this one may only ever move when a snapshot lands, or a navigation the
   *  server reported without one would read as a resync. */
  private url = '';
  /** Scrollers the reader has moved themselves. The server never moves these
   *  again unless they are pinned to the bottom. */
  private readerMoved = new WeakSet<Node>();
  private readerMovedDoc = false;
  /** The last position this host set programmatically, per scroller. A scroll
   *  event landing on exactly this position is ours, not the reader's; the
   *  event is asynchronous, so a flag would be cleared before it arrives. */
  private adoptedDoc: { x: number; y: number } = { x: -1, y: -1 };
  private adopted = new WeakMap<Node, { x: number; y: number }>();
  /** The mirrored page's own URL, which relative links resolve against. It
   *  follows the tab wherever it goes, snapshot or not. */
  private pageUrl = '';
  /** Recent pointer positions over the mirror, in viewport permille with the
   *  gap since the previous sample. The server replays these ahead of a click
   *  so the landside page sees the approach the reader actually made, rather
   *  than a cursor materialising on the target. */
  private pointerPath: Array<{ x: number; y: number; t: number }> = [];
  /** When the button went down, so a click can report how long it was held. */
  private pointerDownAt = 0;
  /** Whether a button is down over the mirror right now. A press that started
   *  in the shell — the URL bar, a menu — never sets this, which is the
   *  difference heldBlur turns on. */
  private pressing = false;
  /**
   * The field a press blurred, held back from the server until the gesture it
   * belongs to either reaches the page or turns out not to be going.
   *
   * Landside, a press moves focus by itself: the click about to be sent arrives
   * as a real mousePressed on the control, and the field the reader was in
   * blurs exactly when and because it would have if they were sitting in front
   * of the page. Sending a blur of our own does that same thing early, alone,
   * and a round trip before the click exists — and to any page that closes a
   * popover when its search field loses focus, which is most of them, that is
   * the popover closing over the result the reader was aiming at. The Google
   * Chat capture is this and nothing else: blur, then a click on a node the
   * blur had already destroyed, and the server answering "node 2219 not found
   * landside" while the reader watches the dialog vanish.
   *
   * So it waits. The gesture that follows clears it by doing the same job
   * better; a gesture that never reaches the page flushes it, because then
   * nothing else is going to tell the page the reader left.
   */
  private heldBlur = 0;

  constructor(tab: number, events: HostEvents) {
    this.tab = tab;
    this.events = events;
    this.frame = document.createElement('iframe');
    this.frame.className = 'mirror';
    // Which landside tab this frame is showing. Nothing in the shell reads it —
    // it keeps its own map — but a frame is otherwise anonymous, and both a
    // capture and a test looking at a strip of them have no other way to say
    // which page is which.
    this.frame.dataset.tab = String(tab);
    this.frame.setAttribute('sandbox', 'allow-same-origin');
    this.frame.setAttribute('referrerpolicy', 'no-referrer');
    this.frame.src = 'about:blank';
    this.ready = new Promise<void>((resolve) => {
      const attach = (): void => {
        const doc = this.frame.contentDocument;
        // No document to patch yet. The listener stays on: this used to be a
        // one-shot, and a load event that arrived before the frame had a
        // document spent it — leaving `ready` pending for the life of the tab.
        // Nothing reports that. Every snapshot and every batch for the tab is
        // awaiting this promise, so the page is never drawn and never
        // acknowledged, and the server sees a client that has silently stopped
        // short of a page it has already sent.
        if (!doc) return;
        this.frame.removeEventListener('load', attach);
        this.attach(doc);
        resolve();
      };
      this.frame.addEventListener('load', attach);
      // about:blank can be ready before the load event lands.
      queueMicrotask(() => {
        if (this.frame.contentDocument?.readyState === 'complete') attach();
      });
    });
  }

  /** Resolves once the frame's document is patchable. */
  whenReady(): Promise<void> {
    return this.ready;
  }

  /**
   * Takes the id the server gave this tab. The frame is opened the instant the
   * user asks for a tab, which is a round trip before there is an id to open it
   * under; everything it emits from here on is about the real tab.
   */
  adopt(tab: number): void {
    this.tab = tab;
    // The frame carries its tab id for anything looking for one specific tab's
    // document. It was labelled with the provisional id when it was drawn, and
    // a label that outlives the name it was made from points at nothing.
    this.frame.dataset.tab = String(tab);
  }

  private attach(doc: Document): void {
    if (this.doc === doc && this.patcher) return;
    forceStandardsMode(doc);
    this.doc = doc;

    const style = doc.createElement('style');
    style.textContent = MIRROR_CSS;
    doc.head.appendChild(style);

    this.patcher = new Patcher(doc, {
      isOwned: (node: Node): boolean => this.echo?.isOwned(node) ?? false,
      onImage: (el, meta, hash) => this.applyImage(el, meta, hash),
      onShot: (el, meta) => this.applyShot(el, meta),
      onRestyled: (el) => this.restyled(el),
      rewriteCSS: (rule) => this.resolveCSSImages(rule),
      onFocus: (node) => {
        if (this.echo?.ownedId) return;
        // preventScroll matters more here than it looks: focusing an element
        // scrolls it into view by default, so a landside focus change — a click
        // landing on a control, page script focusing a search box — would throw
        // the reader to wherever that element happens to be.
        (node as HTMLElement | null)?.focus?.({ preventScroll: true });
      },
      onScroll: (node, x, y) => this.followScroll(node, x, y),
      onApplied: (seq) => {
        this.lastSeq = seq;
        // A snapshot is where shadow roots appear, and a root the client has
        // not bound is a sub-document whose scrolling nobody hears.
        this.bindRootScroll();
        this.events.applied(
          this.tab, seq, this.patcher?.docHash() ?? 0, this.patcher?.epoch ?? 0);
      },
    });

    this.echo = new EchoEngine(doc, {
      idOf: (node: Node | null): number => this.patcher?.idOf(node) ?? 0,
      sendText: (node, text) => this.send({ kind: InputKind.Text, node, text }),
      sendKey: (node, key, modifiers, repeat) =>
        this.send({ kind: InputKind.Key, node, key, modifiers, repeat }),
      sendValue: (node, value, start, end) =>
        this.send({ kind: InputKind.SetValue, node, text: value, start, end }),
      sendFocus: (node, focused) => {
        // Focus landing somewhere else says everything a held blur was waiting
        // to say, and says it about the right element: flushed after this, it
        // would blur the field the reader has just moved into.
        if (focused) this.heldBlur = 0;
        this.send({ kind: focused ? InputKind.Focus : InputKind.Blur, node });
      },
      onChatSend: (node, text) => this.placeGhost(node, text),
    });

    this.wireInput(doc);
  }

  // ------------------------------------------------------------------ input

  private send(ev: Record<string, unknown>): void {
    this.inputSeq += 1;
    this.events.input(this.tab, {
      tab: this.tab,
      seq: this.inputSeq,
      // Monotonic milliseconds, as the protocol specifies. Wall-clock time
      // would exceed what the encoder can represent as a CBOR integer, and the
      // server would reject the frame.
      ts: Math.round(performance.now()),
      expectSeq: this.lastSeq,
      ...ev,
    });
  }

  /** How many pointer samples travel with a click. Enough to describe an
   *  approach, few enough that the frame stays a frame. */
  private static readonly PATH_SAMPLES = 6;

  /** Samples closer together than this are the same gesture; dropping them
   *  costs nothing and keeps the frame small. */
  private static readonly PATH_MIN_GAP_MS = 12;

  /** A sample older than this is not part of the approach to this click. */
  private static readonly PATH_MAX_AGE_MS = 500;

  /**
   * A drag in progress over a canvas, or null.
   *
   * Only over a canvas. Everywhere else a press-move-release is the reader
   * selecting text, which the mirror does natively and must not have taken
   * away from it — and there is a node to click besides, which is the whole
   * reason the rest of the mirror never needs coordinates.
   */
  private dragging: {
    node: number;
    point: number[] | undefined;
    samples: Array<{ x: number; y: number; t: number }>;
  } | null = null;

  /** Set when a drag consumed the gesture, so its trailing click does not
   *  arrive landside as a second, contradictory instruction. */
  private dragConsumedClick = false;

  /** Movement below this is a hand not holding still, not a drag. */
  private static readonly DRAG_SLOP_PX = 5;

  /** How many samples describe a drag. More than a click's approach, because
   *  the shape of a pan is the message rather than the preamble to one. */
  private static readonly DRAG_SAMPLES = 16;

  /** The element under the pointer whose content is pixels, if any. */
  /**
   * The node an event actually happened on.
   *
   * `event.target` is retargeted at a shadow boundary: a click inside a
   * mirrored sub-document reports the box the document is inside, not the thing
   * under the finger, and the id sent landside would be the wrong one. The
   * composed path starts at the real node and is the same as `target` where
   * there is no boundary to cross.
   */
  private eventTarget(ev: Event): Node | null {
    const path = ev.composedPath();
    return (path.length ? (path[0] as Node) : (ev.target as Node | null)) ?? null;
  }

  private regionAt(target: EventTarget | null): Element | null {
    const el = target as Element | null;
    return el?.closest?.('[data-skyhook-static]') ?? null;
  }

  private beginDrag(ev: MouseEvent): void {
    if (ev.button !== 0) return;
    const region = this.regionAt(this.eventTarget(ev));
    if (!region) return;
    const node = this.patcher?.idOf(region) ?? 0;
    if (!node) return;
    const start = this.samplePointer(ev);
    if (!start) return;
    this.dragging = { node, point: this.pointInBox(ev, region), samples: [start] };
  }

  /**
   * Adds a sample to the drag in progress, keeping the press.
   *
   * Over the cap the oldest *middle* sample goes, never the first: where the
   * button went down is one end of the displacement being described, and a
   * pan measured from halfway is a pan of the wrong distance. The rest of the
   * path only has to look like a hand moving.
   */
  private recordDrag(sample: { x: number; y: number; t: number }): void {
    const drag = this.dragging;
    if (!drag) return;
    drag.samples.push(sample);
    if (drag.samples.length > MirrorHost.DRAG_SAMPLES) drag.samples.splice(1, 1);
  }

  /**
   * Ends a drag, sending it only if the pointer actually travelled.
   *
   * A press and release in the same spot is a click, and a map told it was
   * dragged nowhere has been asked to do nothing at all — while the click it
   * really was would never arrive.
   */
  private endDrag(ev: MouseEvent): void {
    const drag = this.dragging;
    this.dragging = null;
    if (!drag) return;
    const last = this.samplePointer(ev);
    if (last) drag.samples.push(last);
    if (drag.samples.length < 2) return;

    const win = this.frame.contentWindow;
    const w = win?.innerWidth ?? 0;
    const h = win?.innerHeight ?? 0;
    const first = drag.samples[0];
    const end = drag.samples[drag.samples.length - 1];
    const moved = Math.hypot(
      ((end.x - first.x) / 1000) * w,
      ((end.y - first.y) / 1000) * h,
    );
    if (moved < MirrorHost.DRAG_SLOP_PX) return;

    const path: number[] = [];
    for (let i = 0; i < drag.samples.length; i++) {
      const gap = i === 0 ? 0 : Math.round(drag.samples[i].t - drag.samples[i - 1].t);
      path.push(drag.samples[i].x, drag.samples[i].y, gap);
    }
    this.dragConsumedClick = true;
    this.send({
      kind: InputKind.Drag,
      node: drag.node,
      modifiers: modifierMask(ev),
      point: drag.point,
      path,
    });
    // A pan is a press and a release like any other, so it carries the blur
    // landside the same way a click does. See heldBlur.
    this.heldBlur = 0;
  }

  /** Sends a blur the gesture that caused it turned out not to be carrying. */
  private flushHeldBlur(): void {
    const node = this.heldBlur;
    if (!node) return;
    this.heldBlur = 0;
    this.send({ kind: InputKind.Blur, node });
  }

  /** Where the pointer is, in viewport permille, or null if the frame has no
   *  size to measure against yet. */
  private samplePointer(ev: MouseEvent): { x: number; y: number; t: number } | null {
    const win = this.frame.contentWindow;
    const w = win?.innerWidth ?? 0;
    const h = win?.innerHeight ?? 0;
    if (!w || !h) return null;
    return {
      x: Math.round((ev.clientX / w) * 1000),
      y: Math.round((ev.clientY / h) * 1000),
      t: performance.now(),
    };
  }

  private recordPointer(ev: MouseEvent): void {
    const last = this.pointerPath[this.pointerPath.length - 1];
    if (last && performance.now() - last.t < MirrorHost.PATH_MIN_GAP_MS) return;
    const sample = this.samplePointer(ev);
    if (!sample) return;
    this.pointerPath.push(sample);
    if (this.pointerPath.length > MirrorHost.PATH_SAMPLES) this.pointerPath.shift();
    this.recordDrag(sample);
  }

  /** The approach as the wire carries it: (x, y, dt) triplets, oldest first. */
  private approachPath(): number[] | undefined {
    const now = performance.now();
    const fresh = this.pointerPath.filter((p) => now - p.t <= MirrorHost.PATH_MAX_AGE_MS);
    if (fresh.length < 2) return undefined;
    const out: number[] = [];
    for (let i = 0; i < fresh.length; i++) {
      const gap = i === 0 ? 0 : Math.round(fresh[i].t - fresh[i - 1].t);
      out.push(fresh[i].x, fresh[i].y, gap);
    }
    return out;
  }

  /** Where in the target's box the pointer was, in permille. The landside box
   *  is laid out with different fonts, so a fraction travels and pixels do not. */
  private pointInBox(ev: MouseEvent, target: Element | null): number[] | undefined {
    if (!target?.getBoundingClientRect) return undefined;
    const r = target.getBoundingClientRect();
    if (!r.width || !r.height) return undefined;
    const fx = Math.round(((ev.clientX - r.left) / r.width) * 1000);
    const fy = Math.round(((ev.clientY - r.top) / r.height) * 1000);
    if (fx < 0 || fx > 1000 || fy < 0 || fy > 1000) return undefined;
    return [fx, fy];
  }

  /** How long the button was held, when this click came from a real press. */
  private holdMs(): number | undefined {
    if (!this.pointerDownAt) return undefined;
    const held = Math.round(performance.now() - this.pointerDownAt);
    // A keyboard-activated click has no press; a press from minutes ago is not
    // this click's. Either way the server is better off inventing a duration.
    if (held < 0 || held > 2000) return undefined;
    return held;
  }

  /*
   * A click inside the mirror, as the semantic event the server replays.
   *
   * Its own method because of how many ways it ends without one: a link
   * opened in a new tab, a fragment scrolled to here, a gesture already spent
   * on a pan, a node the patcher cannot place. Each of those leaves the
   * landside page untouched, and the caller has a blur waiting on the answer.
   */
  private clickInFrame(ev: MouseEvent): void {
    const target = this.eventTarget(ev) as HTMLElement | null;
    if (!target) return;
    const anchor = target.closest?.('a[href], area[href]') as HTMLAnchorElement | null;
    // The mirror never navigates itself: a click is a semantic event the
    // server replays into the real page. This has to happen before anything
    // that can bail out. A link the patcher cannot place — one under local
    // echo, one left over from a batch that did not apply — would otherwise
    // follow itself, and the consequences are not cosmetic: the frame fetches
    // the URL from the plane side, which is the one thing this client must
    // never do, and lands on a cross-origin document that the patcher can no
    // longer touch, which kills the tab for the rest of the session.
    if (anchor) ev.preventDefault();
    // Ctrl/⌘-click is the keyboard half of "open in a new tab", and it means
    // the same thing here. Sending it landside instead would open a tab on
    // the VPS that this side has no handle on. It goes before the bail below
    // because opening a tab needs the URL and not the node: a link the
    // patcher cannot place is still a link the reader can follow.
    const newTab = this.linkAt(anchor);
    if (newTab && (ev.ctrlKey || ev.metaKey)) {
      this.events.openLink(this.tab, newTab.url);
      return;
    }
    // A gesture that panned a map is not also a click on it: landside the
    // two would be a pan followed by a press wherever it ended. First,
    // because a gesture already spent is not a click of any kind.
    if (this.dragConsumedClick) {
      this.dragConsumedClick = false;
      return;
    }
    // A link into the document already on screen — Hacker News' parent, prev
    // and next, a footnote, a table of contents — is a scroll and not a
    // navigation, and every line it could land on is already here. So this
    // side does it, at no round trip. Sending it landside instead spends one
    // to scroll a document laid out with different fonts, and all that comes
    // back is a pixel offset from that other layout, which is both wrong here
    // and refused outright once the reader has scrolled for themselves.
    if (anchor && plainClick(ev) && this.jumpToFragment(anchor)) return;
    const node = this.patcher?.idOf(anchor ?? target) ?? 0;
    if (!node) return;
    this.send({
      kind: InputKind.Click,
      node,
      modifiers: modifierMask(ev),
      button: ev.button,
      hold: this.holdMs(),
      point: this.pointInBox(ev, (anchor ?? target) as Element),
      path: this.approachPath(),
    });
    // The press this click becomes landside blurs the field the reader left,
    // in the page's own order and at the page's own moment. Nothing more to
    // hold. See heldBlur.
    this.heldBlur = 0;
    // Following a link is the gesture this whole client is slowest at
    // answering, so the shell is told the moment it goes out rather than when
    // the page comes back. Everything narrower — a click on a button, a
    // checkbox — is left alone: those usually change the page in place, and a
    // page arriving is not what the reader is waiting for.
    const link = plainClick(ev) ? this.linkAt(anchor) : undefined;
    if (link) this.events.navigating(this.tab, link.url);
    // The frame has no allow-forms, so a submit control never produces a
    // native submit event; recognise it here instead.
    this.maybeSubmit(target);
  }

  /*
   * ------------------------------------------------------ the reader's pointer
   *
   * Everything that measures the gesture rather than naming its target listens
   * on pointer events, and has to. A phone is the device this client was
   * written for, and a phone does not produce mouse events while a finger is
   * moving — it produces them, all at once and all stamped with the same
   * millisecond, after the finger has come up, and only for a gesture the
   * browser decided was a tap. Measured under touch emulation, a 94 ms tap
   * arrives as:
   *
   *     pointerdown@1513  pointerup@1608  mousemove@1608  mousedown@1608
   *     mouseup@1608      click@1608
   *
   * and a swipe arrives as:
   *
   *     pointerdown@2224  pointermove ×4  pointerup@2477
   *
   * with no mouse event of any kind. Three things follow, and all three were
   * wrong. The press this side reported was `mousedown` to `click`, which on a
   * phone is the gap between two events fired in the same millisecond — every
   * tap in the Google Chat capture reports a 1–5 ms hold, and the server
   * prefers a reported hold to its own plausible one, so it replays a press no
   * hand could make. The approach needs two `mousemove` samples and a phone
   * sends one. And the pan — `beginDrag` on `mousedown`, sampled on
   * `mousemove` — was never started at all, which is why a map could not be
   * moved from the device this exists to serve.
   *
   * Pointer events are the same stream for a mouse, a finger and a pen, they
   * carry the press the reader actually made, and they arrive while it is
   * happening. What stays on the mouse events is `pressing`, and only that:
   * see heldBlur, where what is being bracketed is the focus change, and the
   * focus change is a default action of the compat `mousedown` on both kinds
   * of pointer.
   */
  private wireInput(doc: Document): void {
    doc.addEventListener('pointermove', (ev) => this.recordPointer(ev as PointerEvent), true);

    // A canvas is reached through its pixels or not at all: there is no node
    // inside a map to click and no element inside a game board to focus, so a
    // press-move-release over one is the only way to say "pan from here to
    // there". Everywhere else that gesture is the reader selecting text, which
    // the mirror does natively and this must not take away — hence beginDrag
    // starting nothing unless the press landed on a region.
    doc.addEventListener('pointerup', (ev) => this.endDrag(ev as PointerEvent), true);
    // The browser has taken the gesture for itself — a scroll, a fling, the
    // system's own back swipe. It is not a pan and it never becomes one: what
    // the reader did with it happened here, not landside, and sending the part
    // that arrived would pan the page by however far the finger got before the
    // browser claimed it.
    doc.addEventListener('pointercancel', () => { this.dragging = null; }, true);
    // A pointer that left the frame mid-drag is not coming back to release the
    // button, and a drag left open would swallow the next click.
    doc.addEventListener('pointerleave', (ev) => this.endDrag(ev as PointerEvent), true);

    // `pressing` brackets the focus change, which is a default action of the
    // compat mousedown and lands between these two on a phone exactly as it
    // does under a mouse. Nothing else is measured here.
    doc.addEventListener('mouseup', () => { this.pressing = false; }, true);
    doc.addEventListener('mouseleave', () => { this.pressing = false; }, true);

    doc.addEventListener('click', (ev) => {
      this.clickInFrame(ev);
      // A click that never reached the page did not carry the reader out of
      // the field they were in either, and nothing else is coming that will.
      this.flushHeldBlur();
    }, true);

    doc.addEventListener('pointerdown', (ev) => {
      const pointer = ev as PointerEvent;
      // A second finger is not a second gesture: the pan belongs to the pointer
      // that started it, and a pinch is not something this can carry anyway.
      if (!pointer.isPrimary) return;
      this.pointerDownAt = performance.now();
      // A gesture that reached neither the page nor a click — the pointer left
      // the frame, the page swallowed it — leaves its blur held. Nothing later
      // is going to resolve it, and the landside page is still sitting in a
      // field the reader left a gesture ago, so it goes now, before this press
      // moves the focus it is about.
      this.flushHeldBlur();
      // A drag whose click never came — the pointer left the frame, the page
      // swallowed it, or a finger produced no compat click at all — must not
      // leave the flag armed for the next gesture, which would arrive as a
      // press that does nothing.
      this.dragConsumedClick = false;
      this.recordPointer(pointer);
      this.beginDrag(pointer);
      this.events.dismiss(this.tab);
    }, true);

    // Middle click means "open in a new tab" in every browser, and a mirrored
    // page is not exempt. The frame withholds allow-popups, so left to itself
    // the gesture does nothing at all; claiming mousedown as well suppresses
    // Chrome's autoscroll, which would otherwise scroll a document the server
    // knows nothing about. It stays on `mousedown` because autoscroll is that
    // event's default action, and preventing the pointer event does not
    // reliably prevent it.
    doc.addEventListener('mousedown', (ev) => {
      const mouse = ev as MouseEvent;
      this.pressing = true;
      // Inside a text field the gesture is X11's primary-selection paste, which
      // the browser performs natively and the echo engine picks up as an input
      // event. Leave it alone.
      if (mouse.button === 1 && !asEditable(mouse.target)) ev.preventDefault();
    }, true);

    doc.addEventListener('auxclick', (ev) => {
      const mouse = ev as MouseEvent;
      if (mouse.button !== 1 || asEditable(mouse.target)) return;
      ev.preventDefault();
      const link = this.linkAt(mouse.target);
      if (link) this.events.openLink(this.tab, link.url);
    }, true);

    doc.addEventListener('dblclick', (ev) => {
      const node = this.patcher?.idOf(this.eventTarget(ev)) ?? 0;
      if (!node) return;
      const mouse = ev as MouseEvent;
      this.send({
        kind: InputKind.DblClick,
        node,
        modifiers: modifierMask(ev),
        hold: this.holdMs(),
        point: this.pointInBox(mouse, mouse.target as Element),
        path: this.approachPath(),
      });
      this.heldBlur = 0;
    }, true);

    doc.addEventListener('contextmenu', (ev) => {
      // The native menu is worse than useless over a mirror: its entries act on
      // the sandboxed frame, so "Open link in new tab" opens about:blank,
      // "Reload" reloads a document with no origin, and "Save image" saves a
      // hash. The shell answers with Skyhook's own menu instead; forwarding the
      // right click to the landside page is one of the entries on it.
      ev.preventDefault();
      this.events.menu(this.tab, this.menuTarget(ev as MouseEvent));
    }, true);

    doc.addEventListener('focusin', (ev) => this.echo?.focus(this.eventTarget(ev)), true);
    doc.addEventListener('focusout', () => {
      const held = this.pressing ? this.echo?.ownedId ?? 0 : 0;
      // Ownership always ends here — that half is local, and the buffered
      // server truth for the field has waited long enough. Only telling the
      // page about it waits, and only for a press. See heldBlur.
      this.echo?.blur((op) => this.applyOne(op), this.pressing);
      if (held) this.heldBlur = held;
    }, true);
    doc.addEventListener('input', (ev) => this.echo?.input(ev as InputEvent), true);
    doc.addEventListener('keydown', (ev) => {
      const key = ev as KeyboardEvent;
      // Escape shuts the shell's menu before it means anything to the page.
      if (key.key === 'Escape' && this.events.dismiss(this.tab)) {
        key.preventDefault();
        return;
      }
      if (key.key === 'Enter' && this.submitOnEnter(key.target as HTMLElement | null)) {
        key.preventDefault();
        return;
      }
      if (this.echo?.key(key)) key.preventDefault();
    }, true);

    const win = this.frame.contentWindow;
    win?.addEventListener('scroll', () => {
      // A menu anchored to a node that has just moved is pointing at the wrong
      // thing; the scroll telemetry below is throttled, but this cannot be.
      this.events.dismiss(this.tab);
      if (win.scrollX !== this.adoptedDoc.x || win.scrollY !== this.adoptedDoc.y) {
        this.readerMovedDoc = true;
      }
      if (this.scrollTimer) return;
      this.scrollTimer = setTimeout(() => {
        this.scrollTimer = null;
        this.events.scroll(this.tab, {
          tab: this.tab,
          x: win.scrollX,
          y: win.scrollY,
          h: win.innerHeight,
          docH: doc.documentElement.scrollHeight,
        });
      }, 250);
    }, { passive: true });

    // Scroll events do not bubble, but they do reach a capturing listener on
    // the document, which is how a scrolled container is noticed at all.
    doc.addEventListener('scroll', this.onElementScroll, { capture: true, passive: true });
  }

  /*
   * The same listener, on a shadow root.
   *
   * A scroll event is not composed, so its path stops at the root it happened
   * in and a listener on the document never sees it. Everything a reader does
   * inside a mirrored sub-document — a widget with its own scrollbar, a frame
   * taller than its box — would go unrecorded, and coming back to the page
   * would put them somewhere they had never been.
   *
   * Re-run whenever a snapshot lands, because that is when roots appear.
   * addEventListener with the same function and the same options is idempotent,
   * so a root that was already bound is not bound twice.
   */
  /*
   * Copies every shadow root's content into the matching node of a clone.
   *
   * The two trees are walked in step: a clone has the same shape as its source,
   * so the nth descendant of one is the nth of the other. Where the source has
   * a root, the clone gets that root's rules as a <style> and its children as
   * ordinary children — a flattened picture of a boundary that the artifact
   * formats cannot express.
   */
  /*
   * Pins whatever a running animation is showing at this instant.
   *
   * The picture is rendered by serialising the clone into an SVG and drawing
   * that, and an SVG drawn as an image is a still: its clock never starts, so
   * every CSS animation in it paints its first frame. A Google Chat capture
   * came back with the message the reader had just sent in the colour its
   * half-second send-fade *begins* at — a flat grey — while their screen had
   * long since settled on the blue it ends at. Nothing in the bundle mentioned
   * an animation, so the picture read as the mirror having lost the bubble's
   * background, which is a report about the wrong half of the system.
   *
   * So the values the reader is actually looking at are copied onto the clone,
   * read from the live element at the moment of the freeze, and the animation
   * that would paint over them is stopped there. Only the properties the
   * animation itself names, so nothing else in the cascade is overruled, and
   * only for the elements that have one running — which on a page that is not
   * animating is none, and costs the walk nothing.
   */
  private pinAnimationsInto(source: Element, clone: Element): number {
    const view = source.ownerDocument.defaultView;
    let running: Animation[];
    try {
      running = source.ownerDocument.getAnimations();
    } catch { return 0; /* a document that will not enumerate them is one we do without */ }
    if (!running.length || !view) return 0;

    const wanted = new Map<Element, Set<string>>();
    for (const animation of running) {
      const effect = animation.effect as KeyframeEffect | null;
      const target = effect?.target;
      if (!target) continue;
      let props = wanted.get(target);
      if (!props) {
        props = new Set<string>();
        wanted.set(target, props);
      }
      try {
        for (const frame of effect.getKeyframes()) {
          for (const key of Object.keys(frame)) {
            if (KEYFRAME_META.has(key)) continue;
            props.add(key.replace(/[A-Z]/g, (c) => `-${c.toLowerCase()}`));
          }
        }
      } catch { /* an effect that will not describe itself is one we do without */ }
    }
    if (!wanted.size) return 0;

    let pinned = 0;
    const src = [source, ...source.querySelectorAll('*')];
    const dst = [clone, ...clone.querySelectorAll('*')];
    for (let i = 0; i < src.length && i < dst.length; i++) {
      const props = wanted.get(src[i]);
      if (!props?.size) continue;
      const now = view.getComputedStyle(src[i]);
      const style = (dst[i] as HTMLElement).style;
      if (!style) continue;
      for (const prop of props) {
        const value = now.getPropertyValue(prop);
        if (value) style.setProperty(prop, value);
      }
      // And the animation itself is stopped, because pinning the value alone
      // does not survive: an animation outranks even an inline style in the
      // cascade, so the copy would start over from its first frame and paint
      // straight over what was just written. A still is what this is.
      style.setProperty('animation-name', 'none');
      pinned += 1;
    }
    return pinned;
  }

  private flattenRootsInto(source: Element, clone: Element): void {
    const src = [source, ...source.querySelectorAll('*')];
    const dst = [clone, ...clone.querySelectorAll('*')];
    for (let i = 0; i < src.length && i < dst.length; i++) {
      const root = src[i].shadowRoot;
      if (!root) continue;
      const target = dst[i];
      const rules: string[] = [];
      for (const sheet of root.adoptedStyleSheets) {
        try {
          for (const rule of sheet.cssRules) rules.push(rule.cssText);
        } catch { /* a sheet that will not enumerate is one we do without */ }
      }
      if (rules.length) {
        const style = target.ownerDocument.createElement('style');
        style.textContent = rules.join('\n');
        target.appendChild(style);
      }
      for (const child of Array.from(root.childNodes)) {
        target.appendChild(target.ownerDocument.importNode(child, true));
      }
    }
  }

  private bindRootScroll(): void {
    for (const root of this.patcher?.shadowRoots() ?? []) {
      root.addEventListener('scroll', this.onElementScroll, { capture: true, passive: true });
    }
  }

  private onElementScroll = (ev: Event): void => {
    const doc = this.frame?.contentDocument;
    const target = this.eventTarget(ev) as Node | null;
    // The document's own scroll arrives here too; the window listener owns that.
    if (!target || target.nodeType !== Node.ELEMENT_NODE) return;
    const el = target as HTMLElement;
    if (doc && el === doc.documentElement) return;
    // Same reason as the window listener: a scrolled container leaves an open
    // menu pointing somewhere the node no longer is.
    this.events.dismiss(this.tab);
    const mine = this.adopted.get(el);
    if (!mine || mine.x !== el.scrollLeft || mine.y !== el.scrollTop) this.readerMoved.add(el);
  };

  // ------------------------------------------------------------------ scroll

  /**
   * Applies a scroll position the server reported.
   *
   * The reader owns the viewport. Landside scrolling happens for reasons that
   * have nothing to do with them — the server nudges the real page to keep lazy
   * loading working, and page script scrolls itself — and none of that is a
   * reason to move someone who is reading. So a server scroll is applied only
   * when the reader has not taken this scroller over, or when they are sitting
   * at the bottom of it, which is the one place following along is the point
   * (a chat log, a live feed).
   */
  private followScroll(node: Node | null, x: number, y: number): void {
    if (!node) {
      const win = this.frame.contentWindow;
      if (!win) return;
      if (this.readerMovedDoc && !atBottom(
        win.scrollY, win.innerHeight, this.doc?.documentElement.scrollHeight ?? 0)) {
        return;
      }
      this.scrollDocTo(x, y);
      return;
    }
    const el = node as HTMLElement;
    if (typeof el.scrollTop !== 'number') return;
    if (this.readerMoved.has(el) && !atBottom(el.scrollTop, el.clientHeight, el.scrollHeight)) {
      return;
    }
    el.scrollLeft = x;
    el.scrollTop = y;
    this.adopted.set(el, { x: el.scrollLeft, y: el.scrollTop });
  }

  /** Scrolls the mirrored document, recording the position as ours. */
  private scrollDocTo(x: number, y: number): void {
    const win = this.frame.contentWindow;
    if (!win) return;
    win.scrollTo(x, y);
    // Read back rather than trusting the request: the position is clamped to
    // the document, and the clamped value is what the scroll event will carry.
    this.adoptedDoc = { x: win.scrollX, y: win.scrollY };
  }

  // ------------------------------------------------------------------- links

  /**
   * The link a node sits inside, resolved and filtered down to what the shell
   * can actually act on. The agent absolutises URL attributes landside, but a
   * `<base>`-less fragment can still arrive relative, so the page's own URL is
   * the fallback base.
   */
  private linkAt(target: EventTarget | Node | null): { url: string; text: string } | undefined {
    const el = target as HTMLElement | null;
    const anchor = el?.closest?.('a[href], area[href]') as HTMLAnchorElement | null;
    if (!anchor) return undefined;
    const url = this.resolve(anchor.getAttribute('href'));
    if (!url) return undefined;
    return { url, text: (anchor.textContent ?? '').replace(/\s+/g, ' ').trim().slice(0, 120) };
  }

  /**
   * Scrolls to the target of a link into this same document, and says whether
   * it did.
   *
   * Everything else answers false and travels landside as an ordinary click: a
   * link to another page, a bare `#` that page script uses as a button, a
   * fragment naming something this document does not hold. The last of those is
   * what keeps a hash-routed app working — `#/inbox` names no element here, so
   * the click goes landside and the router runs where the router lives.
   */
  private jumpToFragment(anchor: Element): boolean {
    const doc = this.doc;
    const win = this.frame.contentWindow;
    if (!doc || !win || !this.pageUrl) return false;
    let url: URL;
    let here: URL;
    try {
      url = new URL(anchor.getAttribute('href') ?? '', this.pageUrl);
      here = new URL(this.pageUrl);
    } catch {
      return false;
    }
    // Same document means everything but the fragment agreeing: the agent sends
    // hrefs absolutised landside, so what arrives for `#comment` is the page's
    // own URL with a hash on the end.
    if (stripFragment(url) !== stripFragment(here)) return false;
    const target = fragmentTarget(doc, url.hash.slice(1));
    if (!target) return false;
    // scrollIntoView rather than arithmetic on the document: the target may sit
    // inside a scroller of its own, and every ancestor of it has to move for the
    // reader to end up looking at the thing they asked for.
    target.scrollIntoView({ block: 'start' });
    // `:target` crossed the link as a mark rather than as a pseudo-class,
    // because this frame has no fragment in its address to answer one with
    // (rewriteLandsideState, css.go). This jump is the event that would have
    // moved it landside, so it moves here: without this the page's own
    // highlight goes on naming whichever note the reader arrived by, and every
    // link they follow inside the page appears to do nothing but scroll.
    const marked = doc.querySelector('[data-sky-target]');
    if (marked !== target) {
      marked?.removeAttribute('data-sky-target');
      target.setAttribute('data-sky-target', '');
    }
    // Where the reader asked to be, which is not where landside is sitting.
    // Without this the next scroll the server reports would pull them off it.
    this.adoptedDoc = { x: win.scrollX, y: win.scrollY };
    this.readerMovedDoc = true;
    return true;
  }

  /** Absolutises a mirrored href, refusing anything that is not a web link. */
  private resolve(href: string | null): string | undefined {
    if (!href) return undefined;
    try {
      const url = new URL(href, this.pageUrl || undefined);
      if (url.protocol !== 'http:' && url.protocol !== 'https:') return undefined;
      return url.href;
    } catch {
      return undefined;
    }
  }

  /** Tells the host which URL the tab is showing, so relative links resolve. */
  setPageUrl(url: string): void {
    if (url) this.pageUrl = url;
  }

  // -------------------------------------------------------------- context menu

  private menuTarget(ev: MouseEvent): MenuTarget {
    const target = this.eventTarget(ev) as HTMLElement | null;
    const link = this.linkAt(target);
    const img = target?.closest?.('img') as HTMLImageElement | null;
    const field = asEditable(target);
    // The frame is positioned inside the shell, and the menu is drawn by the
    // shell: translate out of the frame's coordinate space once, here, where
    // both are in reach.
    const rect = this.frame.getBoundingClientRect();
    return {
      node: this.patcher?.idOf(target) ?? 0,
      field: field ? this.patcher?.idOf(field) ?? 0 : 0,
      x: rect.left + ev.clientX,
      y: rect.top + ev.clientY,
      link: link?.url,
      linkText: link?.text,
      image: hashFromImage(img),
      imageAlt: img?.getAttribute('alt') ?? undefined,
      selection: this.selectionIn(field),
    };
  }

  /** The selected text, taken from the field if the pointer is in one: a text
   *  field's own selection is invisible to the document's. */
  private selectionIn(field: HTMLElement | null): string {
    const input = field as HTMLInputElement | null;
    if (input && typeof input.selectionStart === 'number') {
      return String(input.value ?? '')
        .slice(input.selectionStart, input.selectionEnd ?? input.selectionStart);
    }
    return this.doc?.getSelection?.()?.toString() ?? '';
  }

  /** Forwards a right click landside, for pages that answer it with a menu of
   *  their own. It arrives in the mirror as ordinary DOM. */
  sendContextMenu(node: number): void {
    if (!node) return;
    this.send({ kind: InputKind.Context, node, modifiers: 0 });
  }

  /**
   * Replaces the selection in an editable field, for the menu's cut and paste.
   * The edit is applied locally first — the whole point of the echo engine is
   * that an edit is not worth a round trip of waiting — and sent as a whole
   * value, which is the server's existing path for anything that is not typing.
   */
  replaceSelection(field: number, text: string): void {
    const el = this.patcher?.nodeFor(field) as HTMLElement | undefined;
    if (!el) return;
    el.focus?.();
    const before = valueOf(el);
    const { start, end } = caretOf(el);
    const next = before.slice(0, start) + text + before.slice(end);
    const caret = start + text.length;
    setValue(el, next);
    setCaret(el, caret);
    this.echo?.noteValue(field, next);
    this.send({ kind: InputKind.SetValue, node: field, text: next, start: caret, end: caret });
  }

  /** Selects everything in an editable field. Selection is local: the server
   *  has nothing to replay. */
  selectAll(field: number): void {
    const el = this.patcher?.nodeFor(field) as HTMLElement | undefined;
    if (!el) return;
    const input = el as HTMLInputElement;
    if (typeof input.select === 'function') {
      el.focus?.();
      input.select();
      return;
    }
    const sel = this.doc?.getSelection?.();
    sel?.removeAllRanges();
    sel?.selectAllChildren?.(el);
  }

  /** Sends a form submission for a clicked submit control. */
  private maybeSubmit(target: HTMLElement): boolean {
    const control = target.closest?.('button, input[type=submit], input[type=image]') as
      HTMLInputElement | HTMLButtonElement | null;
    if (!control) return false;
    const type = (control.getAttribute('type') ?? (control.tagName === 'BUTTON' ? 'submit' : ''))
      .toLowerCase();
    if (type !== 'submit' && type !== 'image') return false;
    return this.submitForm(control.closest('form'));
  }

  private submitOnEnter(target: HTMLElement | null): boolean {
    if (!target) return false;
    const tag = target.tagName?.toUpperCase();
    // Enter in a textarea or a chat composer is a newline or a send, not a
    // form submission; the echo engine owns those.
    if (tag !== 'INPUT') return false;
    const form = target.closest('form');
    if (!form) return false;
    return this.submitForm(form);
  }

  private submitForm(form: HTMLFormElement | null): boolean {
    if (!form) return false;
    const node = this.patcher?.idOf(form) ?? 0;
    if (!node) return false;
    const fields: Record<string, string> = {};
    for (const el of Array.from(form.elements)) {
      const input = el as HTMLInputElement;
      if (input.name && typeof input.value === 'string') fields[input.name] = input.value;
    }
    this.inputSeq += 1;
    this.events.input(this.tab, {
      tab: this.tab, seq: this.inputSeq, ts: Math.round(performance.now()),
      kind: InputKind.Submit, node, fields,
    });
    // A submitted form is a page on its way, in every case that is not an app
    // answering it with script. Where it lands is the server's business — the
    // action may be relative, a redirect, or computed — so the shell is told
    // that something is coming without being told where.
    this.events.navigating(this.tab);
    return true;
  }

  // ------------------------------------------------------------------ frames

  applySnapshot(snap: Snapshot): void {
    if (!this.patcher) return;
    this.setPageUrl(snap.url);
    this.echo?.release();
    // A held blur names a node in the document being replaced. Sent after this
    // it would name whatever the server has since given that id to, and the
    // gesture it belonged to is over either way.
    this.heldBlur = 0;
    // A snapshot for the document already on screen is a resync — the server
    // closing a gap it could not close with diffs — and the reader should not
    // be able to tell it happened. Only a genuine navigation adopts the
    // landside scroll position, which is what carries a #fragment target.
    const win = this.frame.contentWindow;
    const resync = !!snap.url && snap.url === this.url;
    const keep = { x: win?.scrollX ?? 0, y: win?.scrollY ?? 0 };
    this.url = snap.url ?? '';

    // Every element is about to be rebuilt, so a shot still waiting on its
    // bytes is waiting for an element that will not be in the document when
    // they land. The server re-photographs against the new document.
    this.pendingShots.clear();
    // A navigation drops every image the old document was showing. The bytes
    // stay in Cache Storage, so re-minting a blob for one that comes back
    // costs nothing over the link; holding them all until the tab closes is
    // what an afternoon of reading would cost in memory.
    //
    // Before the new document is applied, not after. Applying it renders its
    // stylesheet, and rendering a stylesheet is what puts the images it names
    // on the asking list — so releasing afterwards threw away the requests the
    // new page had just made, along with the record that it had made them. On
    // a page whose CSS arrives whole in the snapshot and never changes again,
    // nothing renders that sheet a second time and nothing ever asks: every
    // background, every icon and every webfont it names would simply never
    // come, with no pending list to show for it.
    if (!resync) this.releaseBlobs();
    this.patcher.applySnapshot(snap);

    if (resync) {
      this.scrollDocTo(keep.x, keep.y);
      // Anything still waiting for bytes has been waiting since before the
      // resync, and nothing else will offer them: an asset is asked for once
      // per hash, and the server does not re-send what it has already sent, so
      // a push the link dropped would leave that picture missing for the life
      // of the document. This is the one moment that knows a request went
      // unanswered — the document has just been rebuilt and these are still
      // empty — and it costs one round trip per picture that is genuinely
      // absent, and nothing at all for a page whose images all arrived.
      this.askAgainForWhatNeverCame();
    } else {
      this.readerMovedDoc = false;
      this.readerMoved = new WeakSet();
      this.adopted = new WeakMap();
      this.scrollDocTo(snap.scrollX, snap.scrollY);
    }
    this.requestPendingImages();
  }

  applyMutation(m: Mutation, seq: number): void {
    if (!this.patcher) return;
    // A batch this document has already had. The worker drops replays before
    // they get here, but it decides from what it has handed over and this is
    // what has actually been applied — the two differ across a reconnect, where
    // the server replays from the last sequence it was told about and the
    // batches after it were applied but never acknowledged.
    //
    // Applying one twice appends its strings to the intern table a second time,
    // which shifts every reference after it by one and turns the rest of the
    // session's text into someone else's. Silently: the table is not hashed,
    // and re-inserting a node the document already has reuses its id. Cheaper
    // to refuse the batch than to detect the damage later.
    if (seq > 0 && seq <= this.lastSeq) return;
    const ops = m.ops.filter((op) => {
      if (op.op === OpCode.Attr) this.reconcileAttr(op);
      return !this.echo?.defer(op, (id) => this.patcher?.nodeFor(id));
    });
    this.patcher.applyMutation({ ...m, ops }, seq);
    this.retireGhosts();
    this.requestPendingImages();
  }

  private applyOne(op: MutationOp): void {
    this.patcher?.applyMutation({ strings: [], ops: [op], docHash: 0, flush: false }, this.lastSeq);
  }

  setImageMeta(meta: ImageMeta): void {
    if (meta.missing) this.noteMissing(meta);
    // A hash can come back: nothing records a failure landside, so the next
    // snapshot submits it again and it may well succeed the second time.
    else if (meta.hash) this.missing.delete(meta.hash);
    this.patcher?.setImageMeta(meta);
  }

  /**
   * Stops waiting for an asset the server says is not coming.
   *
   * The client asks for a hash exactly once, so before there was anything to
   * say this, a landside failure left every element that referenced it holding
   * a transparent pixel until the tab was closed. Dropping the hash from the
   * waiting lists is most of the point; showing the alt text is the rest of
   * it, and is what the page's author wrote for this case.
   */
  private noteMissing(meta: ImageMeta): void {
    const hash = meta.hash;
    if (!hash) return;
    this.missing.add(hash);
    const waiting = this.pendingImages.get(hash);
    if (waiting) {
      this.pendingImages.delete(hash);
      for (const el of waiting) {
        if (meta.alt && !el.getAttribute('alt')) el.setAttribute('alt', meta.alt);
        this.showMissing(el);
      }
    }
    // A background image that is not coming stays the transparent pixel it
    // already is: there is no alt text for one, and a broken-image marker
    // tiled across a panel is worse than nothing at all.
    this.pendingCSS.delete(hash);
    this.pendingShots.delete(hash);
  }

  /**
   * Lets an image fall back to its alt text.
   *
   * An <img> with no `src` draws its alt text and a broken-image marker, which
   * is exactly what PENDING_PIXEL exists to avoid *while bytes are still on
   * their way*. Once they are not, the same behaviour is the honest one — with
   * one exception: an element already wearing a blurhash has an approximation
   * of the real picture on it, which beats a marker saying the picture failed.
   */
  private showMissing(el: HTMLImageElement): void {
    el.dataset.skyhookMissing = '1';
    if (el.dataset.skyhookBlur === '1') return;
    el.removeAttribute('src');
  }

  /** Called when the store has bytes for a hash: show them. */
  imageArrived(hash: string): void {
    if (!this.pendingImages.has(hash) && !this.pendingCSS.has(hash)
      && !this.pendingShots.has(hash)) return;
    void this.blobFor(hash).then((url) => {
      if (!url) return;
      const waiting = this.pendingImages.get(hash);
      if (waiting) {
        this.pendingImages.delete(hash);
        for (const el of waiting) this.showImage(el, url);
      }
      const shots = this.pendingShots.get(hash);
      if (shots) {
        this.pendingShots.delete(hash);
        for (const s of shots) this.showShot(s.el, url, s.box, hash);
      }
      if (this.pendingCSS.delete(hash)) this.refreshCSSSoon();
    });
  }

  /**
   * Resolves the image references the server left in a CSS rule.
   *
   * The server rewrites `url(...)` to a content hash, because it has no idea
   * where the client will keep the bytes; `skyhook://img/...` left as written
   * is a scheme no browser knows, so every backgrounded logo, icon and hero
   * image renders as nothing at all. Until the bytes land the reference points
   * at a transparent pixel, which at least lets the box keep its own colour.
   */
  private resolveCSSImages(rule: string): string {
    if (!rule.includes('skyhook://img/')) return rule;
    const pruned = rule.replace(FONT_FACE_BLOCK, (block) => (this.faceHasItsFile(block) ? block : ''));
    if (!pruned.includes('skyhook://img/')) return pruned;
    return pruned.replace(/skyhook:\/\/img\/([0-9a-f]+)/gi, (_m, hash: string) => {
      const known = this.blobs.get(hash);
      if (known) return known;
      this.wantCSSImage(hash);
      return PENDING_PIXEL;
    });
  }

  /*
   * Whether an `@font-face` has the file it names, and so may be written at all.
   *
   * Everywhere else a reference this side cannot resolve becomes the
   * transparent pixel: an image still on its way leaves the box its own colour,
   * and one that is never coming leaves it that way for good. An `@font-face`
   * is the one place where that answer is worse than no answer. A face whose
   * `src` loads is a face — and a 1x1 GIF loads — so the family gains a face
   * that draws nothing, and font matching prefers it to the faces that work.
   *
   * The Google Chat capture is this exactly. Chat declares two faces for
   * `Google Symbols`: a subset at weight 400, which arrived, and the 4.9 MB
   * variable font at `font-weight: 100 700`, which the transcoder refuses at
   * its 1 MB cap — "font is 4888276 bytes: source image too large". The pixel
   * took the refused one's place, shadowed the subset for every weight it
   * covers, and every icon drawn from the family rendered as nothing at all.
   * The send button in the composer was empty on a page whose other icons were
   * fine, which reads as one broken button rather than as a broken font.
   *
   * So the face is withheld instead. A face that does not exist is one that
   * font matching skips, which leaves the page whichever of its faces did
   * arrive; and the sheet is re-rendered whenever bytes land, so a font that
   * was merely late is written on the pass after it.
   */
  private faceHasItsFile(block: string): boolean {
    let ready = true;
    for (const m of block.matchAll(/skyhook:\/\/img\/([0-9a-f]+)/gi)) {
      if (this.blobs.has(m[1])) continue;
      this.wantCSSImage(m[1]);
      ready = false;
    }
    return ready;
  }

  /** Puts a stylesheet's image on the asking list, unless it is not coming. */
  private wantCSSImage(hash: string): void {
    // Not coming: nothing should queue or ask for it again.
    if (this.missing.has(hash) || this.pendingCSS.has(hash)) return;
    this.pendingCSS.add(hash);
    this.requestImagesSoon();
    if (!this.probed.has(hash)) {
      this.probed.add(hash);
      this.imageArrived(hash);
    }
  }

  /** Re-renders the stylesheet once, however many images just landed. */
  private refreshCSSSoon(): void {
    if (this.cssRefresh) return;
    this.cssRefresh = setTimeout(() => {
      this.cssRefresh = null;
      this.patcher?.refreshCSS();
    }, 120);
  }

  /**
   * Turns a hash into a URL the mirror frame can actually load.
   *
   * A sandboxed frame is not a service worker client, so `/img/<hash>` from
   * inside it reaches the network — and on this link there is no network. The
   * shell reads the bytes and hands the frame a blob URL, which needs no fetch
   * at all.
   *
   * It reads Cache Storage directly rather than fetching that URL itself. The
   * shell *is* a service worker client, but only once the worker has claimed
   * it, and until then the same fetch goes to the network, where the server
   * answers an unknown path with the app shell. Minting a blob out of that
   * would leave the element pointing at an `index.html` that decodes to no
   * image, for the rest of the session — the failure the network cannot
   * produce is the one worth designing out.
   */
  private async blobFor(hash: string): Promise<string | null> {
    const known = this.blobs.get(hash);
    if (known) return known;
    try {
      const cache = await caches.open(IMAGE_CACHE);
      const hit = await cache.match(imageCacheKey(hash));
      if (!hit) return null;
      const blob = await hit.blob();
      if (!blob.size) return null;
      const url = URL.createObjectURL(blob);
      this.blobs.set(hash, url);
      return url;
    } catch {
      return null;
    }
  }

  private showImage(el: HTMLImageElement, url: string): void {
    el.setAttribute('src', url);
    el.style.backgroundImage = '';
    delete el.dataset.skyhookBlur;
  }

  /**
   * Paints a region shot onto the element it was taken from.
   *
   * A background image rather than a replaced element, because the canvas is
   * still the page's own canvas: the site's CSS sizes it, positions it and
   * stacks it, and all of that keeps working around a background where
   * swapping in an <img> would throw it away. The box places the photograph
   * where it came from — a canvas half off the bottom of the landside viewport
   * was photographed as the half that exists, and stretching that half over
   * the whole element would be a picture of somewhere the reader is not.
   */
  private showShot(el: HTMLElement, url: string, box: number[], hash: string): void {
    // Bytes for a frame the reader has already moved past: the element is
    // wearing a newer photograph and this one would be a step backwards.
    if (el.dataset.skyhookShot !== hash) return;
    this.shotBox.set(el, box);
    el.style.backgroundImage = `url("${url}")`;
    el.style.backgroundRepeat = 'no-repeat';
    // The border box, because that is the box the agent measured against.
    el.style.backgroundOrigin = 'border-box';
    if (box.length === 4) {
      const [x, y, w, h] = box;
      el.style.backgroundPosition = `${x}px ${y}px`;
      el.style.backgroundSize = `${w}px ${h}px`;
    } else {
      // No placement given: the photograph is of the whole element.
      el.style.backgroundPosition = '0 0';
      el.style.backgroundSize = '100% 100%';
    }
  }

  /**
   * Puts back what a page's `style` write threw away.
   *
   * The mirror paints two things into an element's inline style that the page
   * knows nothing about: the photograph of a canvas, and an image's blur
   * placeholder and reserved box. A `style` attribute op replaces the whole
   * declaration — the landside element it was copied from carries none of
   * this — so both vanish on a write the page made for some unrelated reason,
   * a border colour or a transform.
   *
   * For an image that is a flicker: the bytes are known and the src is an
   * attribute, so only the placeholder is lost. For a canvas it is permanent.
   * Shots are taken in answer to input (see shot.go), so nothing will paint
   * that element again until the reader touches something — a map or a game
   * goes blank and stays blank.
   */
  private restyled(el: HTMLElement): void {
    const shot = el.dataset.skyhookShot;
    if (shot) {
      const url = this.blobs.get(shot);
      if (url) {
        this.showShot(el, url, this.shotBox.get(el) ?? [], shot);
        return;
      }
    }
    const img = el.dataset.skyhookImg;
    if (img && el.tagName === 'IMG') {
      // The blur went with the style, so the mark for it has to go too: kept,
      // it would claim a placeholder this element no longer draws — and for an
      // asset already announced as missing, that claim is what stands between
      // the reader and the alt text.
      if (!this.blobs.has(img)) delete el.dataset.skyhookBlur;
      this.applyImage(el as HTMLImageElement, this.patcher?.images.get(img), img);
    }
  }

  private applyShot(el: HTMLElement, meta: ImageMeta): void {
    if (!meta.hash) return;
    // The frame this element was waiting for is now a frame behind. Left in
    // the queue it would hold a reference to the element until its bytes
    // arrived, once per repaint, for as long as the link stayed down.
    this.forgetShot(el, el.dataset.skyhookShot);
    el.dataset.skyhookShot = meta.hash;
    const known = this.blobs.get(meta.hash);
    if (known) {
      this.showShot(el, known, meta.box, meta.hash);
      return;
    }
    const list = this.pendingShots.get(meta.hash) ?? [];
    if (!list.some((p) => p.el === el)) list.push({ el, box: meta.box });
    this.pendingShots.set(meta.hash, list);
    // Shots are pushed unasked, being the content rather than an illustration
    // beside it. Asking anyway covers the push that was dropped while the link
    // was down, which is the one case where nothing would ever announce them.
    if (!this.probed.has(meta.hash)) {
      this.probed.add(meta.hash);
      this.imageArrived(meta.hash);
    }
    this.requestImagesSoon();
  }

  /** Drops an element from the queue waiting on a superseded shot. */
  private forgetShot(el: HTMLElement, hash: string | undefined): void {
    if (!hash) return;
    const list = this.pendingShots.get(hash);
    if (!list) return;
    const rest = list.filter((p) => p.el !== el);
    if (rest.length) this.pendingShots.set(hash, rest);
    else this.pendingShots.delete(hash);
  }

  private applyImage(el: HTMLImageElement, meta: ImageMeta | undefined, hash: string): void {
    if (!hash) return;
    el.dataset.skyhookImg = hash;
    reserveSpace(el, meta);
    const known = this.blobs.get(hash);
    if (known) {
      this.showImage(el, known);
      return;
    }
    // Announced as not coming. Joining the waiting list would hold a
    // transparent pixel over the alt text for the rest of the session, and
    // asking again would spend a round trip to be told the same thing.
    if (this.missing.has(hash)) {
      this.showMissing(el);
      return;
    }
    if (meta?.blur && !el.dataset.skyhookBlur) {
      el.dataset.skyhookBlur = '1';
      // A page of grey boxes is what a mirror feels like without this, and the
      // placeholder costs about thirty bytes.
      void import('../shared/blurhash.js').then(({ decodeBlurhashToCSS }) => {
        if (el.dataset.skyhookBlur !== '1') return; // bytes won the race
        el.style.backgroundImage = decodeBlurhashToCSS(meta.blur, 8, 8);
        el.style.backgroundSize = 'cover';
      });
    }
    if (el.getAttribute('src') !== PENDING_PIXEL) el.setAttribute('src', PENDING_PIXEL);
    const list = this.pendingImages.get(hash) ?? [];
    if (!list.includes(el)) list.push(el);
    this.pendingImages.set(hash, list);
    // The bytes may already be in the cache from an earlier flight, in which
    // case nothing will ever announce them. Once per hash: a page of 125
    // images redrawn on every mutation batch would otherwise ask 125 times a
    // second for bytes that are not there yet.
    if (!this.probed.has(hash)) {
      this.probed.add(hash);
      this.imageArrived(hash);
    }
  }

  /**
   * Asks for the bytes of every image still missing, once per hash.
   *
   * Only images above the fold are pushed unasked; everything else — and every
   * image a stylesheet names, which the server cannot see a viewport position
   * for — waits to be asked for. Asking twice costs a round trip on a link
   * where round trips are the whole problem, so each hash goes once.
   */
  private requestPendingImages(): void {
    const want: string[] = [];
    for (const hash of this.pendingImages.keys()) {
      if (!this.requested.has(hash)) want.push(hash);
    }
    for (const hash of this.pendingShots.keys()) {
      if (!this.requested.has(hash)) want.push(hash);
    }
    for (const hash of this.pendingCSS) {
      if (!this.requested.has(hash)) want.push(hash);
    }
    if (!want.length) return;
    for (const hash of want) this.requested.add(hash);
    this.events.wantImages(this.tab, want);
  }

  /**
   * Forgets having asked for the assets that never arrived, so the next round
   * of requests includes them.
   */
  private askAgainForWhatNeverCame(): void {
    // Not the region shots: those were dropped a moment ago with the elements
    // they belonged to, and the server photographs the new document itself.
    for (const hash of this.pendingImages.keys()) this.requested.delete(hash);
    for (const hash of this.pendingCSS) this.requested.delete(hash);
  }

  /** Batches the requests a burst of patches would otherwise make one at a time. */
  private requestImagesSoon(): void {
    if (this.imageRequest) return;
    this.imageRequest = setTimeout(() => {
      this.imageRequest = null;
      this.requestPendingImages();
    }, 120);
  }

  private reconcileAttr(op: MutationOp): void {
    const owned = this.echo?.ownedId ?? 0;
    if (!owned || op.node !== owned) return;
    const node = this.patcher?.nodeFor(op.node) as HTMLElement | undefined;
    if (!node) return;
    const server = op.str || undefined;
    if (server !== undefined && server !== valueOf(node)) this.echo?.reconcile(op.node, server);
  }

  // --------------------------------------------------------------- optimistic

  private placeGhost(composer: Node, text: string): boolean {
    const list = nearestList(composer as HTMLElement);
    if (!list || !this.doc) return false;
    const ghost = this.doc.createElement('div');
    ghost.className = 'skyhook-ghost';
    ghost.setAttribute('data-skyhook-ghost', text);
    ghost.textContent = text;
    list.appendChild(ghost);
    return true;
  }

  private retireGhosts(): void {
    if (!this.doc) return;
    const ghosts = Array.from(this.doc.querySelectorAll('[data-skyhook-ghost]'));
    // Serialising the whole document costs half a megabyte on a big page, and
    // this runs after every batch. There is almost never a ghost to retire.
    if (!ghosts.length) return;
    const body = this.doc.body.textContent ?? '';
    for (const el of ghosts) {
      const text = el.getAttribute('data-skyhook-ghost') ?? '';
      // Once the authoritative copy arrives the text appears twice; the ghost
      // has done its job.
      if (text && body.split(text).length > 2) el.remove();
    }
  }

  /**
   * Takes everything a diagnostic capture needs from this frame, synchronously.
   *
   * Synchronously is the whole contract. The capture worth having is the one
   * taken at a divergence, and the server's next act after finding one is to
   * resync the tab — which replaces this document with a correct one. The
   * resync arrives on the dom channel, the capture request on ctrl, and the
   * shell reads them on the same thread; so as long as nothing here awaits, the
   * diverged document is already in hand before the replacement can be applied.
   * Everything slow — rasterising, compressing, sending — happens afterwards,
   * from this frozen copy rather than from the live frame.
   */
  freeze(): MirrorFreeze {
    const win = this.frame.contentWindow;
    const doc = this.doc;
    const rect = this.frame.getBoundingClientRect();
    const base: MirrorFreeze = {
      tab: this.tab,
      html: '',
      images: [],
      cssImages: [],
      width: Math.max(1, Math.round(rect.width || win?.innerWidth || 0)),
      height: Math.max(1, Math.round(rect.height || win?.innerHeight || 0)),
      docHeight: doc?.documentElement.scrollHeight ?? 0,
      scrollX: win?.scrollX ?? 0,
      scrollY: win?.scrollY ?? 0,
      state: {},
      fingerprint: { total: 0, truncated: false, nodes: [] },
    };
    if (!doc || !this.patcher) {
      base.error = 'this tab has no patchable document (the frame never attached)';
      return base;
    }
    try {
      base.html = doc.documentElement.outerHTML;
    } catch (err) {
      base.error = `could not serialise the mirrored document: ${String(err)}`;
    }
    // Alongside the markup, not instead of it: the markup is the artifact a
    // person reads, and the clone is the one a renderer can trust.
    //
    // Neither cloneNode nor importNode carries a shadow root, and XMLSerializer
    // does not write one, so a mirrored sub-document would be absent from both
    // the clone and the picture rendered from it. The clone is flattened on the
    // way out instead: each root's rules go in as a <style> and its content
    // becomes ordinary children of the stand-in. That loses the scoping, which
    // for an artifact nobody interacts with costs nothing and is the difference
    // between a frame being in the picture and not.
    let pinned = 0;
    try {
      const clone = doc.documentElement.cloneNode(true) as Element;
      // Before flattening, which appends to the clone and so moves everything
      // after it out of step with the live tree the walk pairs it against.
      pinned = this.pinAnimationsInto(doc.documentElement, clone);
      this.flattenRootsInto(doc.documentElement, clone);
      base.doc = clone;
    } catch { /* the picture falls back to the markup, with its parse losses */ }
    try {
      base.fingerprint = this.patcher.fingerprint();
    } catch (err) {
      base.error = `${base.error ?? ''} fingerprint failed: ${String(err)}`.trim();
    }
    // From the attribute, not from `src`: `src` is a blob URL, which says
    // nothing about its content and cannot be resolved anywhere else.
    for (const el of Array.from(doc.querySelectorAll('img'))) {
      const hash = hashFromImage(el);
      if (hash && !base.images.includes(hash)) base.images.push(hash);
    }
    // The same trade for the images only CSS names. These have no element to
    // read a hash off, so the map the host built on the way in is the only
    // record of which bytes a `url(blob:…)` stands for.
    base.cssImages = Array.from(this.blobs, ([hash, url]) => [url, hash]);
    base.state = {
      tab: this.tab,
      url: this.url,
      pageUrl: this.pageUrl,
      lastAppliedSeq: this.lastSeq,
      inputSeq: this.inputSeq,
      patcher: this.patcher.diag(),
      pendingImages: Array.from(this.pendingImages.keys()),
      // The images only the stylesheet names, which have no element to hang a
      // hash on and so appear nowhere else in a bundle. A logo the server
      // fetched from the wrong address is a transparent pixel here and a
      // complete-looking rule landside; without this list the capture shows a
      // blank box and nothing at all to say the client was still waiting.
      pendingCSSImages: Array.from(this.pendingCSS),
      // Told about and given up on, which a capture could not previously tell
      // apart from still waiting — the difference between a slow link and a
      // landside failure, and the whole reason this list exists.
      missingImages: Array.from(this.missing),
      // A canvas is the one element whose emptiness a capture cannot show:
      // the mirrored markup is identical whether or not its photograph
      // arrived. These two say which it was.
      shots: Array.from(doc.querySelectorAll('[data-skyhook-shot]'))
        .map((el) => (el as HTMLElement).dataset.skyhookShot),
      pendingShots: Array.from(this.pendingShots.keys()),
      // The reader having taken the scroller over is why the server's scroll
      // positions stop being applied, which looks exactly like a mirror that
      // has stopped updating.
      readerMovedDocument: this.readerMovedDoc,
      adoptedScroll: { ...this.adoptedDoc },
      scroll: { x: base.scrollX, y: base.scrollY },
      frame: {
        width: base.width, height: base.height, docHeight: base.docHeight,
        offline: doc.documentElement.classList.contains('skyhook-offline'),
      },
      imageHashes: base.images.length,
      ghosts: doc.querySelectorAll('[data-skyhook-ghost]').length,
      substituted: doc.querySelectorAll('[data-skyhook-tag]').length,
      // Elements that were mid-animation, and whose current values were copied
      // onto the copy the picture is drawn from. An SVG drawn as an image is a
      // still and starts every animation over, so without this the picture
      // shows a frame the reader never saw — and says nothing about it. See
      // pinAnimationsInto.
      pinnedAnimations: pinned,
    };
    return base;
  }

  /** Marks the mirror as showing stale content during an outage. */
  setOffline(offline: boolean): void {
    this.doc?.documentElement.classList.toggle('skyhook-offline', offline);
  }

  /** Marks the mirror as waiting for a page it has asked for. */
  setBusy(busy: boolean): void {
    this.doc?.documentElement.classList.toggle('skyhook-busy', busy);
  }

  /** Node count, for the HUD. */
  get nodes(): number {
    return this.patcher?.size ?? 0;
  }

  private releaseBlobs(): void {
    for (const url of this.blobs.values()) URL.revokeObjectURL(url);
    this.blobs.clear();
    this.probed.clear();
    this.pendingCSS.clear();
    this.requested.clear();
    this.missing.clear();
  }

  destroy(): void {
    this.frame.remove();
    if (this.cssRefresh) clearTimeout(this.cssRefresh);
    if (this.imageRequest) clearTimeout(this.imageRequest);
    this.releaseBlobs();
    this.pendingImages.clear();
    this.pendingShots.clear();
    this.patcher = null;
    this.echo = null;
    this.doc = null;
  }
}
