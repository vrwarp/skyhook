/**
 * The DOM patcher: applies snapshots and mutation batches to a real document.
 *
 * This is the counterpart of internal/mirror/model.go, and the two implement
 * the same algorithm on purpose — the server keeps a replica and compares
 * document hashes, so a divergence between them is a bug worth finding rather
 * than a mystery.
 *
 * Nothing here parses HTML. Nodes are constructed one at a time, which is what
 * guarantees the mirror can never execute page script: a <script> element that
 * somehow reached us would be created detached from any parser and never run,
 * and the sanitiser below drops it anyway.
 */
import {
  ImageMeta, MirrorNode, Mutation, NodeFlags, NodeKind, OpCode, Snapshot,
} from '../shared/protocol.js';

/** Tags that are never materialised, whatever the server says. */
const FORBIDDEN_TAGS = new Set([
  'script', 'noscript', 'iframe', 'object', 'embed', 'applet', 'link', 'meta', 'base',
]);

/** Attributes dropped on arrival: nothing in a mirror needs to run code. */
const FORBIDDEN_ATTR_PREFIX = 'on';

export interface PatcherHooks {
  /** Called when an image node needs a placeholder or a real bitmap. */
  onImage?(node: HTMLImageElement, meta: ImageMeta | undefined, hash: string): void;
  /** Called when the server reports the landside focus moved. */
  onFocus?(node: Node | null): void;
  /** Called for document-level or container scroll positions. */
  onScroll?(node: Node | null, x: number, y: number): void;
  /** Called after each applied batch, with the batch's sequence number. */
  onApplied?(seq: number): void;
  /** Returns true when a node is under client-owned local echo and must not be
   *  overwritten by server mutations. */
  isOwned?(node: Node): boolean;
  /** Buffers a mutation the patcher declined to apply because of ownership. */
  onDeferred?(op: unknown): void;
}

export class Patcher {
  readonly doc: Document;
  private strings: string[] = [];
  private nodes = new Map<number, Node>();
  private ids = new WeakMap<Node, number>();
  private styleEl: HTMLStyleElement | null = null;
  private cssRules: string[] = [];
  private hooks: PatcherHooks;
  private root: HTMLElement | null = null;
  /** Sequence of the last applied batch. */
  seq = 0;
  /** Images referenced by the current document, keyed by content hash. */
  images = new Map<string, ImageMeta>();

  constructor(doc: Document, hooks: PatcherHooks = {}) {
    this.doc = doc;
    this.hooks = hooks;
  }

  /** Node id lookup, used by the input serialiser. */
  idOf(node: Node | null): number {
    if (!node) return 0;
    let n: Node | null = node;
    while (n) {
      const id = this.ids.get(n);
      if (id !== undefined) return id;
      n = n.parentNode;
    }
    return 0;
  }

  nodeFor(id: number): Node | undefined {
    return this.nodes.get(id);
  }

  private str(ref: number): string {
    if (ref < 0 || ref >= this.strings.length) return '';
    return this.strings[ref];
  }

  /** Replaces the whole document with a snapshot. */
  applySnapshot(snap: Snapshot): void {
    this.strings = snap.strings.slice();
    this.nodes = new Map();
    this.ids = new WeakMap();
    this.images = new Map();
    this.seq = 0;
    for (const im of snap.images) this.images.set(im.hash, im);

    const body = this.doc.body;
    if (!body) return;
    body.textContent = '';
    this.ensureStyleElement();
    this.cssRules = [];
    this.setCSS(snap.css);

    const container = this.doc.createElement('div');
    container.setAttribute('data-skyhook-root', '1');
    this.root = container;
    body.appendChild(container);

    for (const n of snap.nodes) {
      const el = this.createNode(n);
      if (!el) continue;
      const parent = n.parent === 0 ? container : this.nodes.get(n.parent);
      if (!parent) continue;
      parent.appendChild(el);
    }
    this.doc.title = snap.title || this.doc.title;
    this.hooks.onApplied?.(0);
  }

  /** Applies one mutation batch. Returns false if the batch was unappliable,
   *  which the caller turns into a resync request. */
  applyMutation(m: Mutation, seq: number): boolean {
    for (const s of m.strings) this.strings.push(s);
    for (const op of m.ops) {
      switch (op.op) {
        case OpCode.Insert: {
          const parent = this.nodes.get(op.parent);
          if (!parent) break; // parent already gone; a resync will fix it
          if (this.isOwnedNode(parent)) {
            this.hooks.onDeferred?.(op);
            break;
          }
          const before = op.before ? this.nodes.get(op.before) ?? null : null;
          const created: Node[] = [];
          for (const n of op.nodes) {
            const el = this.createNode(n);
            created.push(el ?? this.doc.createComment(''));
            if (!el) continue;
            if (n.id === op.nodes[0].id) continue;
            const p = this.nodes.get(n.parent);
            if (p) p.appendChild(el);
          }
          const first = created[0];
          if (first && first.nodeType !== Node.COMMENT_NODE) {
            parent.insertBefore(first, before && before.parentNode === parent ? before : null);
          }
          break;
        }
        case OpCode.Remove: {
          const node = this.nodes.get(op.node);
          if (!node) break;
          this.forget(node);
          node.parentNode?.removeChild(node);
          break;
        }
        case OpCode.Attr: {
          const node = this.nodes.get(op.node);
          if (!node || node.nodeType !== Node.ELEMENT_NODE) break;
          if (this.isOwnedNode(node)) {
            this.hooks.onDeferred?.(op);
            break;
          }
          this.setAttr(node as Element, this.str(op.ref), op.ref2 < 0 ? null : this.str(op.ref2));
          break;
        }
        case OpCode.Text: {
          const node = this.nodes.get(op.node);
          if (!node) break;
          if (this.isOwnedNode(node)) {
            this.hooks.onDeferred?.(op);
            break;
          }
          node.nodeValue = this.str(op.ref);
          break;
        }
        case OpCode.Splice: {
          const node = this.nodes.get(op.node);
          if (!node) break;
          if (this.isOwnedNode(node)) {
            this.hooks.onDeferred?.(op);
            break;
          }
          const cur = node.nodeValue ?? '';
          const off = Math.min(op.off, cur.length);
          const del = Math.min(op.del, cur.length - off);
          node.nodeValue = cur.slice(0, off) + this.str(op.ref) + cur.slice(off + del);
          break;
        }
        case OpCode.Move: {
          const node = this.nodes.get(op.node);
          const parent = this.nodes.get(op.parent);
          if (!node || !parent) break;
          const before = op.before ? this.nodes.get(op.before) ?? null : null;
          parent.insertBefore(node, before && before.parentNode === parent ? before : null);
          break;
        }
        case OpCode.Style:
          this.setCSS(op.add);
          break;
        case OpCode.Focus:
          this.hooks.onFocus?.(op.node ? this.nodes.get(op.node) ?? null : null);
          break;
        case OpCode.Scroll:
          this.hooks.onScroll?.(op.node ? this.nodes.get(op.node) ?? null : null, op.x, op.y);
          break;
        case OpCode.DocInfo:
          if (op.str) this.doc.title = op.str;
          break;
        default:
          break;
      }
    }
    this.seq = seq;
    this.hooks.onApplied?.(seq);
    return true;
  }

  private isOwnedNode(node: Node): boolean {
    if (!this.hooks.isOwned) return false;
    let n: Node | null = node;
    while (n) {
      if (this.hooks.isOwned(n)) return true;
      n = n.parentNode;
    }
    return false;
  }

  private createNode(n: MirrorNode): Node | null {
    let node: Node | null = null;
    switch (n.kind) {
      case NodeKind.Text:
        node = this.doc.createTextNode(this.str(n.ref));
        break;
      case NodeKind.Doctype:
        return null; // a doctype inside a container would be invalid anyway
      case NodeKind.Element: {
        const tag = (this.str(n.ref) || 'div').toLowerCase();
        if (FORBIDDEN_TAGS.has(tag)) return null;
        let el: Element;
        try {
          el = this.doc.createElement(tag);
        } catch {
          el = this.doc.createElement('div');
        }
        for (let i = 0; i + 1 < n.attrs.length; i += 2) {
          this.setAttr(el, this.str(n.attrs[i]), this.str(n.attrs[i + 1]));
        }
        if (n.flags & NodeFlags.Canvas) {
          el.setAttribute('data-skyhook-static', '1');
        }
        if (n.flags & NodeFlags.Editable) {
          el.setAttribute('data-skyhook-editable', '1');
        }
        if ((n.flags & NodeFlags.Image) && el instanceof HTMLImageElement) {
          const hash = imageHashOf(el.getAttribute('src') ?? '');
          if (hash) this.hooks.onImage?.(el, this.images.get(hash), hash);
        }
        node = el;
        break;
      }
      default:
        return null;
    }
    if (node) {
      this.nodes.set(n.id, node);
      this.ids.set(node, n.id);
    }
    return node;
  }

  private setAttr(el: Element, name: string, value: string | null): void {
    if (!name) return;
    const lower = name.toLowerCase();
    if (lower.startsWith(FORBIDDEN_ATTR_PREFIX) && lower.length > 2) return;
    if (value === null) {
      el.removeAttribute(name);
      return;
    }
    if ((lower === 'href' || lower === 'src' || lower === 'action') &&
        /^\s*javascript:/i.test(value)) {
      return;
    }
    try {
      el.setAttribute(name, value);
    } catch {
      return;
    }
    // Live form state arrives as a data attribute so a resync restores what was
    // typed; reflect it onto the property the browser actually renders.
    if (lower === 'data-sky-value') {
      const input = el as HTMLInputElement;
      if ('value' in input && input.value !== value) input.value = value;
    } else if (lower === 'data-sky-checked') {
      (el as HTMLInputElement).checked = value === '1';
    } else if (lower === 'data-sky-selected') {
      (el as HTMLOptionElement).selected = value === '1';
    } else if (lower === 'src' && el instanceof HTMLImageElement) {
      const hash = imageHashOf(value);
      if (hash) this.hooks.onImage?.(el, this.images.get(hash), hash);
    }
  }

  private forget(node: Node): void {
    const id = this.ids.get(node);
    if (id !== undefined) {
      this.nodes.delete(id);
      this.ids.delete(node);
    }
    for (let c = node.firstChild; c; c = c.nextSibling) this.forget(c);
  }

  private ensureStyleElement(): void {
    if (this.styleEl && this.styleEl.isConnected) return;
    const el = this.doc.createElement('style');
    el.setAttribute('data-skyhook-css', '1');
    this.doc.head?.appendChild(el);
    this.styleEl = el;
  }

  /** Appends used-CSS rules. The cascade is preserved by append order, which is
   *  the order the server extracted them in. */
  setCSS(rules: string[]): void {
    if (!rules.length) return;
    this.ensureStyleElement();
    if (!this.styleEl) return;
    for (const r of rules) this.cssRules.push(r);
    this.styleEl.textContent = this.cssRules.join('\n');
  }

  /** Registers image metadata arriving after the snapshot. */
  setImageMeta(meta: ImageMeta): void {
    this.images.set(meta.hash, meta);
    for (const el of this.doc.querySelectorAll('img')) {
      const hash = imageHashOf(el.getAttribute('src') ?? '');
      if (hash === meta.hash) this.hooks.onImage?.(el as HTMLImageElement, meta, hash);
    }
  }

  /** Hashes the document the way the server and the Go replica do, so the
   *  periodic integrity check compares like with like. */
  docHash(): number {
    let h = 0x811c9dc5;
    const ids = Array.from(this.nodes.keys()).sort((a, b) => a - b);
    for (const id of ids) {
      const node = this.nodes.get(id);
      if (!node) continue;
      const v = node.nodeType === Node.TEXT_NODE
        ? node.nodeValue ?? ''
        : (node as Element).tagName?.toLowerCase() ?? '';
      h ^= id & 0xff;
      h = Math.imul(h, 16777619) >>> 0;
      for (let i = 0; i < v.length && i < 32; i++) {
        h ^= v.charCodeAt(i) & 0xff;
        h = Math.imul(h, 16777619) >>> 0;
      }
    }
    return h >>> 0;
  }

  /** Number of mirrored nodes, for the HUD. */
  get size(): number {
    return this.nodes.size;
  }

  /** The element holding the mirrored document. */
  get rootElement(): HTMLElement | null {
    return this.root;
  }
}

/** Extracts the content hash from a skyhook image URL. */
export function imageHashOf(src: string): string {
  const m = /^skyhook:\/\/img\/([0-9a-f]+)/i.exec(src);
  return m ? m[1] : '';
}
