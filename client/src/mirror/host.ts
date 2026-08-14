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
import { EchoEngine, modifierMask, valueOf } from './echo.js';
import { Patcher } from './patcher.js';

/** Base styles for a mirrored document, injected into each frame. */
const MIRROR_CSS = `
html, body { margin: 0; padding: 0; background: #fff; color: #111; }
.skyhook-ghost { opacity: .55; font-style: italic; }
img { background-repeat: no-repeat; background-size: cover; }
[data-skyhook-static] {
  background: repeating-linear-gradient(45deg, #eee, #eee 8px, #e5e5e5 8px, #e5e5e5 16px);
}
`;

/** Emitted by the host, forwarded to the server by the app shell. */
export interface HostEvents {
  input(tab: number, ev: Record<string, unknown>): void;
  scroll(tab: number, ev: Record<string, unknown>): void;
  applied(tab: number, seq: number, hash: number): void;
  wantImages(tab: number, hashes: string[]): void;
}

/** Resolves a content hash to the URL the service worker serves it from. */
export function imageURL(hash: string): string {
  return `/img/${hash}`;
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
        (node as HTMLElement | null)?.focus?.();
      },
      onScroll: (node, x, y) => {
        if (!node) {
          this.frame.contentWindow?.scrollTo(x, y);
          return;
        }
        const el = node as HTMLElement;
        el.scrollLeft = x;
        el.scrollTop = y;
      },
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
      const anchor = target.closest?.('a[href]') as HTMLAnchorElement | null;
      const node = this.patcher?.idOf(anchor ?? target) ?? 0;
      if (!node) return;
      // The mirror never navigates itself: a click is a semantic event the
      // server replays into the real page.
      ev.preventDefault();
      this.send({
        kind: InputKind.Click,
        node,
        modifiers: modifierMask(ev),
        button: ev.button,
        url: anchor?.getAttribute('href') ?? undefined,
      });
      // The frame has no allow-forms, so a submit control never produces a
      // native submit event; recognise it here instead.
      this.maybeSubmit(target);
    }, true);

    doc.addEventListener('dblclick', (ev) => {
      const node = this.patcher?.idOf(ev.target as Node) ?? 0;
      if (node) this.send({ kind: InputKind.DblClick, node, modifiers: modifierMask(ev) });
    }, true);

    doc.addEventListener('contextmenu', (ev) => {
      const node = this.patcher?.idOf(ev.target as Node) ?? 0;
      if (node) this.send({ kind: InputKind.Context, node, modifiers: modifierMask(ev) });
    }, true);

    doc.addEventListener('focusin', (ev) => this.echo?.focus(ev.target), true);
    doc.addEventListener('focusout', () => {
      this.echo?.blur((op) => this.applyOne(op));
    }, true);
    doc.addEventListener('input', (ev) => this.echo?.input(ev as InputEvent), true);
    doc.addEventListener('keydown', (ev) => {
      const key = ev as KeyboardEvent;
      if (key.key === 'Enter' && this.submitOnEnter(key.target as HTMLElement | null)) {
        key.preventDefault();
        return;
      }
      if (this.echo?.key(key)) key.preventDefault();
    }, true);

    const win = this.frame.contentWindow;
    win?.addEventListener('scroll', () => {
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
    this.echo?.release();
    this.patcher.applySnapshot(snap);
    this.frame.contentWindow?.scrollTo(snap.scrollX, snap.scrollY);
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
      el.removeAttribute('src');
      el.setAttribute('src', `${url}?v=1`);
      el.style.backgroundImage = '';
    }
  }

  private applyImage(el: HTMLImageElement, meta: ImageMeta | undefined, hash: string): void {
    if (!hash) return;
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
    const body = this.doc.body.textContent ?? '';
    for (const el of Array.from(this.doc.querySelectorAll('[data-skyhook-ghost]'))) {
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
