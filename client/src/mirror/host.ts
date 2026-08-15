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
html, body { margin: 0; padding: 0; background: #fff; color: #111; }
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
[data-skyhook-tag="iframe"] html, [data-skyhook-tag="iframe"] body { display: block; }
[data-skyhook-static] {
  background: repeating-linear-gradient(45deg, #eee, #eee 8px, #e5e5e5 8px, #e5e5e5 16px);
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
  applied(tab: number, seq: number, hash: number): void;
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

export class MirrorHost {
  readonly tab: number;
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
  /** Object URLs handed to the frame, by content hash. Revoked with the tab. */
  private blobs = new Map<string, string>();
  /** Hashes already looked for in the local cache, so a redraw does not ask
   *  again for bytes that have not crossed the link yet. */
  private probed = new Set<string>();
  /** Hashes a stylesheet references, which have no element to hang from. */
  private pendingCSS = new Set<string>();
  /** Hashes already asked of the server. */
  private requested = new Set<string>();
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
        if (!doc) return;
        this.attach(doc);
        resolve();
      };
      this.frame.addEventListener('load', attach, { once: true });
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
        this.events.applied(this.tab, seq, this.patcher?.docHash() ?? 0);
      },
    });

    this.echo = new EchoEngine(doc, {
      idOf: (node: Node | null): number => this.patcher?.idOf(node) ?? 0,
      sendText: (node, text) => this.send({ kind: InputKind.Text, node, text }),
      sendKey: (node, key, modifiers, repeat) =>
        this.send({ kind: InputKind.Key, node, key, modifiers, repeat }),
      sendValue: (node, value, start, end) =>
        this.send({ kind: InputKind.SetValue, node, text: value, start, end }),
      sendFocus: (node, focused) =>
        this.send({ kind: focused ? InputKind.Focus : InputKind.Blur, node }),
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
  private regionAt(target: EventTarget | null): Element | null {
    const el = target as Element | null;
    return el?.closest?.('[data-skyhook-static]') ?? null;
  }

  private beginDrag(ev: MouseEvent): void {
    if (ev.button !== 0) return;
    const region = this.regionAt(ev.target);
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

  private wireInput(doc: Document): void {
    doc.addEventListener('mousemove', (ev) => this.recordPointer(ev as MouseEvent), true);

    // A canvas is reached through its pixels or not at all: there is no node
    // inside a map to click and no element inside a game board to focus, so a
    // press-move-release over one is the only way to say "pan from here to
    // there". Everywhere else that gesture is the reader selecting text, which
    // the mirror does natively and this must not take away — hence beginDrag
    // starting nothing unless the press landed on a region.
    doc.addEventListener('mouseup', (ev) => this.endDrag(ev as MouseEvent), true);
    // A pointer that left the frame mid-drag is not coming back to release the
    // button, and a drag left open would swallow the next click.
    doc.addEventListener('mouseleave', (ev) => this.endDrag(ev as MouseEvent), true);

    doc.addEventListener('click', (ev) => {
      const target = ev.target as HTMLElement | null;
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
      const mouse = ev as MouseEvent;
      // A link into the document already on screen — Hacker News' parent, prev
      // and next, a footnote, a table of contents — is a scroll and not a
      // navigation, and every line it could land on is already here. So this
      // side does it, at no round trip. Sending it landside instead spends one
      // to scroll a document laid out with different fonts, and all that comes
      // back is a pixel offset from that other layout, which is both wrong here
      // and refused outright once the reader has scrolled for themselves.
      if (anchor && plainClick(mouse) && this.jumpToFragment(anchor)) return;
      const node = this.patcher?.idOf(anchor ?? target) ?? 0;
      if (!node) return;
      this.send({
        kind: InputKind.Click,
        node,
        modifiers: modifierMask(ev),
        button: mouse.button,
        hold: this.holdMs(),
        point: this.pointInBox(mouse, (anchor ?? target) as Element),
        path: this.approachPath(),
      });
      // Following a link is the gesture this whole client is slowest at
      // answering, so the shell is told the moment it goes out rather than when
      // the page comes back. Everything narrower — a click on a button, a
      // checkbox — is left alone: those usually change the page in place, and a
      // page arriving is not what the reader is waiting for.
      const link = plainClick(mouse) ? this.linkAt(anchor) : undefined;
      if (link) this.events.navigating(this.tab, link.url);
      // The frame has no allow-forms, so a submit control never produces a
      // native submit event; recognise it here instead.
      this.maybeSubmit(target);
    }, true);

    // Middle click means "open in a new tab" in every browser, and a mirrored
    // page is not exempt. The frame withholds allow-popups, so left to itself
    // the gesture does nothing at all; claiming mousedown as well suppresses
    // Chrome's autoscroll, which would otherwise scroll a document the server
    // knows nothing about.
    doc.addEventListener('mousedown', (ev) => {
      const mouse = ev as MouseEvent;
      this.pointerDownAt = performance.now();
      // A drag whose click never came — the pointer left the frame, the page
      // swallowed it — must not leave the flag armed for the next gesture,
      // which would arrive as a press that does nothing.
      this.dragConsumedClick = false;
      this.recordPointer(mouse);
      this.beginDrag(mouse);
      this.events.dismiss(this.tab);
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
      const node = this.patcher?.idOf(ev.target as Node) ?? 0;
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

    doc.addEventListener('focusin', (ev) => this.echo?.focus(ev.target), true);
    doc.addEventListener('focusout', () => {
      this.echo?.blur((op) => this.applyOne(op));
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
    doc.addEventListener('scroll', (ev) => {
      const target = ev.target as Node | null;
      // The document's own scroll arrives here too; the window listener above
      // owns that one.
      if (!target || target.nodeType !== Node.ELEMENT_NODE) return;
      const el = target as HTMLElement;
      if (el === doc.documentElement) return;
      // Same reason as the window listener: a scrolled container leaves an open
      // menu pointing somewhere the node no longer is.
      this.events.dismiss(this.tab);
      const mine = this.adopted.get(el);
      if (!mine || mine.x !== el.scrollLeft || mine.y !== el.scrollTop) this.readerMoved.add(el);
    }, { capture: true, passive: true });
  }

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
    const target = ev.target as HTMLElement | null;
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
    this.patcher.applySnapshot(snap);

    if (resync) {
      this.scrollDocTo(keep.x, keep.y);
    } else {
      this.readerMovedDoc = false;
      this.readerMoved = new WeakSet();
      this.adopted = new WeakMap();
      this.scrollDocTo(snap.scrollX, snap.scrollY);
      // A navigation drops every image the old document was showing. The bytes
      // stay in Cache Storage, so re-minting a blob for one that comes back
      // costs nothing over the link; holding them all until the tab closes is
      // what an afternoon of reading would cost in memory.
      this.releaseBlobs();
    }
    this.requestPendingImages();
  }

  applyMutation(m: Mutation, seq: number): void {
    if (!this.patcher) return;
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
    this.patcher?.setImageMeta(meta);
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
    return rule.replace(/skyhook:\/\/img\/([0-9a-f]+)/gi, (_m, hash: string) => {
      const known = this.blobs.get(hash);
      if (known) return known;
      if (!this.pendingCSS.has(hash)) {
        this.pendingCSS.add(hash);
        this.requestImagesSoon();
        if (!this.probed.has(hash)) {
          this.probed.add(hash);
          this.imageArrived(hash);
        }
      }
      return PENDING_PIXEL;
    });
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
    const root = (composer as HTMLElement).closest?.('[data-skyhook-root], body');
    const list = root?.querySelector('[role="list"], ul, ol');
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
    try {
      base.doc = doc.documentElement.cloneNode(true) as Element;
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
