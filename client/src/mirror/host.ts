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
import { Patcher } from './patcher.js';

/** Base styles for a mirrored document, injected into each frame. */
const MIRROR_CSS = `
html, body { margin: 0; padding: 0; background: #fff; color: #111; }
.skyhook-ghost { opacity: .55; font-style: italic; }
img { background-repeat: no-repeat; background-size: cover; }
/* An iframe's inlined document, rendered into the box that stands in for it. */
[data-skyhook-tag="iframe"] { display: block; overflow: hidden; }
[data-skyhook-tag="iframe"] html, [data-skyhook-tag="iframe"] body { display: block; }
[data-skyhook-static] {
  background: repeating-linear-gradient(45deg, #eee, #eee 8px, #e5e5e5 8px, #e5e5e5 16px);
}
`;

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

/** Emitted by the host, forwarded to the server by the app shell. */
export interface HostEvents {
  input(tab: number, ev: Record<string, unknown>): void;
  scroll(tab: number, ev: Record<string, unknown>): void;
  applied(tab: number, seq: number, hash: number): void;
  wantImages(tab: number, hashes: string[]): void;
  /** A link the user asked to open in a new tab (middle or ctrl/⌘ click). */
  openLink(tab: number, url: string): void;
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

/** Resolves a content hash to the URL the service worker serves it from. */
export function imageURL(hash: string): string {
  return `/img/${hash}`;
}

/** The inverse: the content hash behind an image the frame is showing. */
export function hashFromImageURL(src: string | null | undefined): string | undefined {
  const match = /^\/img\/([^?#]+)/.exec(src ?? '');
  return match ? match[1] : undefined;
}

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

  constructor(tab: number, events: HostEvents) {
    this.tab = tab;
    this.events = events;
    this.frame = document.createElement('iframe');
    this.frame.className = 'mirror';
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
    this.doc = doc;

    const style = doc.createElement('style');
    style.textContent = MIRROR_CSS;
    doc.head.appendChild(style);

    this.patcher = new Patcher(doc, {
      isOwned: (node: Node): boolean => this.echo?.isOwned(node) ?? false,
      onImage: (el, meta, hash) => this.applyImage(el, meta, hash),
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

  private wireInput(doc: Document): void {
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
      const node = this.patcher?.idOf(anchor ?? target) ?? 0;
      if (!node) return;
      this.send({
        kind: InputKind.Click,
        node,
        modifiers: modifierMask(ev),
        button: ev.button,
      });
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
      if (node) this.send({ kind: InputKind.DblClick, node, modifiers: modifierMask(ev) });
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
      image: hashFromImageURL(img?.getAttribute('src')),
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

    this.patcher.applySnapshot(snap);

    if (resync) {
      this.scrollDocTo(keep.x, keep.y);
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

  /** Called when the store has bytes for a hash: force the frame to re-fetch. */
  imageArrived(hash: string): void {
    const waiting = this.pendingImages.get(hash);
    if (!waiting) return;
    this.pendingImages.delete(hash);
    const url = imageURL(hash);
    for (const el of waiting) {
      // Straight to the busted URL: clearing src first would leave the element
      // with no image for a frame, and an image with no image is a hole the
      // page closes up and then reopens.
      el.setAttribute('src', `${url}?v=1`);
      el.style.backgroundImage = '';
    }
  }

  private applyImage(el: HTMLImageElement, meta: ImageMeta | undefined, hash: string): void {
    if (!hash) return;
    reserveSpace(el, meta);
    if (meta?.blur && !el.dataset.skyhookBlur) {
      el.dataset.skyhookBlur = '1';
      // A page of grey boxes is what a mirror feels like without this, and the
      // placeholder costs about thirty bytes.
      void import('../shared/blurhash.js').then(({ decodeBlurhashToCSS }) => {
        el.style.backgroundImage = decodeBlurhashToCSS(meta.blur, 8, 8);
        el.style.backgroundSize = 'cover';
      });
    }
    const url = imageURL(hash);
    if (el.getAttribute('src') !== url) el.setAttribute('src', url);
    const list = this.pendingImages.get(hash) ?? [];
    if (!list.includes(el)) list.push(el);
    this.pendingImages.set(hash, list);
  }

  private requestPendingImages(): void {
    if (!this.pendingImages.size) return;
    this.events.wantImages(this.tab, Array.from(this.pendingImages.keys()));
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

  /** Marks the mirror as showing stale content during an outage. */
  setOffline(offline: boolean): void {
    this.doc?.documentElement.classList.toggle('skyhook-offline', offline);
  }

  /** Node count, for the HUD. */
  get nodes(): number {
    return this.patcher?.size ?? 0;
  }

  destroy(): void {
    this.frame.remove();
    this.patcher = null;
    this.echo = null;
    this.doc = null;
  }
}
