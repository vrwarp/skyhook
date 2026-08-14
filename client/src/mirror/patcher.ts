/**
 * The DOM patcher: applies snapshots and mutation batches to a real document.
 *
 * This is the counterpart of internal/mirror/model.go, and the two implement
 * the same algorithm on purpose — the server keeps a replica and compares
 * document hashes, so a divergence between them is a bug worth finding rather
 * than a mystery.
 *
 * Nothing here parses HTML. Nodes are constructed one at a time, so a <script>
 * element that somehow reached us would be created detached from any parser and
 * never run — and the sanitiser below never constructs one in the first place,
 * substituting an inert element instead. That is defence in depth: the document
 * this patches lives in a sandboxed frame with script execution disabled by the
 * browser (see mirror/host.ts).
 */
import {
  ImageMeta, MirrorNode, Mutation, NodeFlags, NodeKind, OpCode, Snapshot,
} from '../shared/protocol.js';

/**
 * Tags that are never materialised, whatever the server says.
 *
 * They are substituted, not dropped. A dropped node takes its whole subtree
 * with it — which is how same-origin iframes, whose documents the agent inlines
 * as children, used to lose all their content — and leaves the client's node
 * ids out of step with the server's, so the integrity check reports a
 * divergence that is not one and resyncs forever. The substitute is an inert
 * element carrying the original name in `data-skyhook-tag`, so the subtree
 * renders, the ids line up, and nothing that could execute is ever constructed.
 */
const FORBIDDEN_TAGS = new Set([
  'script', 'noscript', 'iframe', 'object', 'embed', 'applet', 'link', 'meta', 'base',
]);

/** What a forbidden tag is materialised as instead. */
const SUBSTITUTE_TAG = 'div';

const SVG_NS = 'http://www.w3.org/2000/svg';
const MATHML_NS = 'http://www.w3.org/1998/Math/MathML';
const XLINK_NS = 'http://www.w3.org/1999/xlink';

/**
 * The namespace an element belongs in.
 *
 * `createElement` builds everything in the HTML namespace, where an `<svg>` is
 * an unknown inline element with no rendering of its own and `viewBox` gets
 * folded to `viewbox`. Sites draw their logos, icons and chart furniture in
 * SVG, so getting this wrong is not an edge case: it is most of the marks on
 * most pages, silently absent.
 *
 * Namespace is inherited from the parent because SVG children (`path`, `g`,
 * `use`) carry no clue of their own — except across `foreignObject`, which
 * exists precisely to put HTML back inside a drawing.
 */
function namespaceFor(tag: string, parent: Node | undefined): string | null {
  if (tag === 'svg') return SVG_NS;
  if (tag === 'math') return MATHML_NS;
  const el = parent as Element | undefined;
  const ns = el?.namespaceURI;
  if (ns === SVG_NS) return el?.localName === 'foreignObject' ? null : SVG_NS;
  if (ns === MATHML_NS) return el?.localName === 'annotation-xml' ? null : MATHML_NS;
  return null;
}

/** Attributes dropped on arrival: nothing in a mirror needs to run code. */
const FORBIDDEN_ATTR_PREFIX = 'on';

export interface PatcherHooks {
  /** Called when an image node needs a placeholder or a real bitmap. The hook
   *  owns setting the element's src, because only the host knows how content
   *  hashes map onto URLs. */
  onImage?(node: HTMLImageElement, meta: ImageMeta | undefined, hash: string): void;
  /** Called when a region shot arrives for a canvas or video element: the
   *  photograph of pixels this side has no way to draw. Same division of
   *  labour as onImage — only the host knows where the bytes live. */
  onShot?(node: HTMLElement, meta: ImageMeta): void;
  /** Rewrites a skyhook image reference inside a CSS rule. */
  rewriteCSS?(rule: string): string;
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
  /** Element names as the server sent them, which is what the document hash is
   *  computed over — see createNode. */
  private names = new Map<number, string>();
  /**
   * Node flags exactly as they arrived, for elements that had any.
   *
   * Kept only to be reported: a capture puts these beside the flags the same
   * elements have landside, and a difference is a node whose copy here is
   * stale in a way no hash and no HTML diff can show — a custom element that
   * grew a shadow root after it was mirrored, most of all.
   */
  private flags = new Map<number, number>();
  private styleEl: HTMLStyleElement | null = null;
  private cssRules: string[] = [];
  private hooks: PatcherHooks;
  private root: HTMLElement | null = null;
  /** Sequence of the last applied batch. */
  seq = 0;
  /** Images referenced by the current document, keyed by content hash. */
  images = new Map<string, ImageMeta>();
  /**
   * The latest region shot for a canvas or video node, keyed by node id.
   *
   * Held by node rather than by hash because a shot is not a property of the
   * document the way an <img> src is — nothing in the markup refers to it, and
   * the next one replaces it. Held at all because the metadata travels on the
   * media channel and the node it belongs to travels on the DOM channel: a
   * shot that overtakes its own canvas would otherwise be dropped, and no
   * second one would come until the reader did something.
   */
  private shots = new Map<number, ImageMeta>();

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
    this.names = new Map();
    this.flags = new Map();
    this.images = new Map();
    this.shots = new Map();
    this.seq = 0;
    for (const im of snap.images) this.images.set(im.hash, im);

    const body = this.doc.body;
    if (!body) return;
    this.ensureStyleElement();
    this.cssRules = [];
    this.setCSS(snap.css);

    // Built detached and swapped in whole. Appending thousands of nodes into
    // the live document lays out the page again after every one of them, and
    // on a resync the reader watches their page empty itself and refill.
    const container = this.doc.createElement('div');
    container.setAttribute('data-skyhook-root', '1');

    for (const n of snap.nodes) {
      const el = this.createNode(n);
      if (!el) continue;
      const parent = n.parent === 0 ? container : this.nodes.get(n.parent);
      if (!parent) continue;
      parent.appendChild(el);
    }
    this.root = container;
    body.replaceChildren(container);
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
    let node: Node;
    switch (n.kind) {
      case NodeKind.Text:
        node = this.doc.createTextNode(this.str(n.ref));
        break;
      case NodeKind.Doctype:
        // A doctype inside a container would be invalid, so it becomes a
        // comment: renders as nothing, but still holds its id, which is what
        // keeps this document's fingerprint equal to the agent's.
        node = this.doc.createComment('');
        this.names.set(n.id, '');
        break;
      case NodeKind.Element: {
        // SVG element names are case-sensitive (`clipPath`, `linearGradient`),
        // so the name is built from as the server sent it; the lowercased form
        // is only for deciding what it is, and for the fingerprint.
        const name = this.str(n.ref) || 'div';
        const tag = name.toLowerCase();
        const forbidden = FORBIDDEN_TAGS.has(tag);
        const ns = forbidden ? null : namespaceFor(tag, this.nodes.get(n.parent));
        let el: Element;
        try {
          el = ns
            ? this.doc.createElementNS(ns, name)
            : this.doc.createElement(forbidden ? SUBSTITUTE_TAG : tag);
        } catch {
          el = this.doc.createElement(SUBSTITUTE_TAG);
        }
        if (forbidden) el.setAttribute('data-skyhook-tag', tag);
        for (let i = 0; i + 1 < n.attrs.length; i += 2) {
          this.setAttr(el, this.str(n.attrs[i]), this.str(n.attrs[i + 1]));
        }
        if (n.flags & NodeFlags.Canvas) {
          el.setAttribute('data-skyhook-static', '1');
          // A shot that arrived before its canvas did has been waiting for
          // exactly this element.
          const shot = this.shots.get(n.id);
          if (shot) this.hooks.onShot?.(el as HTMLElement, shot);
        }
        if (n.flags & NodeFlags.Editable) {
          el.setAttribute('data-skyhook-editable', '1');
        }
        // Hashing has to use the name the server sent, not the name of what was
        // built: the server compares this document's fingerprint against the
        // agent's, and a substitution is not a divergence.
        this.names.set(n.id, tag);
        if (n.flags) this.flags.set(n.id, n.flags);
        node = el;
        break;
      }
      default:
        return null;
    }
    this.nodes.set(n.id, node);
    this.ids.set(node, n.id);
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
    if (lower === 'src' && isImage(el)) {
      const hash = imageHashOf(value);
      if (hash) {
        // Never write the wire form into the DOM: the browser would try to
        // fetch an unknown scheme. The hook resolves it to a real URL.
        this.hooks.onImage?.(el as HTMLImageElement, this.images.get(hash), hash);
        return;
      }
    }
    try {
      // `xlink:href` is a namespaced attribute, and an SVG <use> resolves
      // nothing from a plain attribute that merely has a colon in its name —
      // which is every icon in a sprite sheet.
      if (lower.startsWith('xlink:')) {
        el.setAttributeNS(XLINK_NS, name, value);
      } else {
        el.setAttribute(name, value);
      }
    } catch {
      return;
    }
    // The rendered box of something that had to be substituted — an iframe —
    // stated in pixels, because the CSS that sized the original selects on a
    // tag name this element no longer has.
    if (lower === 'data-sky-box') {
      const [w, h] = value.split('x');
      const style = (el as HTMLElement).style;
      if (w) style.width = `${Number(w)}px`;
      if (h) style.height = `${Number(h)}px`;
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
    }
  }

  private forget(node: Node): void {
    const id = this.ids.get(node);
    if (id !== undefined) {
      this.nodes.delete(id);
      this.ids.delete(node);
      this.names.delete(id);
      this.flags.delete(id);
      this.shots.delete(id);
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
    this.renderCSS();
  }

  /**
   * Re-renders the stylesheet from the rules as they arrived.
   *
   * Rules are kept in their wire form rather than rewritten on arrival, because
   * a rule may name an image whose bytes are still crossing the link: what the
   * reference resolves to changes under it, and only the raw rule can be
   * resolved again.
   */
  refreshCSS(): void {
    if (this.styleEl) this.renderCSS();
  }

  private renderCSS(): void {
    if (!this.styleEl) return;
    const rewrite = this.hooks.rewriteCSS;
    this.styleEl.textContent = rewrite
      ? this.cssRules.map((r) => rewrite(r)).join('\n')
      : this.cssRules.join('\n');
  }

  /** Registers image metadata arriving after the snapshot. */
  setImageMeta(meta: ImageMeta): void {
    // A region shot names its node, not an element in the markup: it is the
    // photograph of a canvas, and there is no src anywhere pointing at it.
    // Either half identifies one — the placement box the server sends with
    // every shot, or the flag on the node it names — so neither has to be the
    // single point at which a canvas silently turns back into a blank box.
    if (meta.node && (meta.box.length === 4
      || (this.flags.get(meta.node) ?? 0) & NodeFlags.Canvas)) {
      this.shots.set(meta.node, meta);
      const el = this.nodes.get(meta.node);
      if (el && el.nodeType === Node.ELEMENT_NODE) {
        this.hooks.onShot?.(el as HTMLElement, meta);
      }
      return;
    }
    this.images.set(meta.hash, meta);
    // Match on the hash the host stamped on the element, not on `src`: by the
    // time metadata arrives `src` may be a placeholder or a blob URL, neither
    // of which says anything about what the image is.
    for (const el of Array.from(this.doc.querySelectorAll('img'))) {
      const img = el as HTMLImageElement;
      if (img.dataset.skyhookImg === meta.hash) {
        this.hooks.onImage?.(img, meta, meta.hash);
      }
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
        : this.names.get(id) ?? (node as Element).tagName?.toLowerCase() ?? '';
      h ^= id & 0xff;
      h = Math.imul(h, 16777619) >>> 0;
      for (let i = 0; i < v.length && i < 32; i++) {
        h ^= v.charCodeAt(i) & 0xff;
        h = Math.imul(h, 16777619) >>> 0;
      }
    }
    return h >>> 0;
  }

  /**
   * The (id, kind, value, flags) rows docHash is computed over, node by node.
   *
   * A hash mismatch says the two documents differ and nothing else. This says
   * which nodes: put it beside the agent's list from the same instant and the
   * diff is the bug. Values are truncated to the 32 characters the hash itself
   * looks at, so a difference here is always a difference the hash saw.
   *
   * The flags are the exception, and are here for the case the hash cannot
   * see: they are not hashed, so an element can agree on id, kind and name and
   * still be a different element — one that grew a shadow root landside after
   * this copy of it was made. These are the flags as they arrived; the agent
   * reports what its elements have now, so a difference means this copy is
   * stale rather than wrong.
   */
  fingerprint(limit = 20000): {
    total: number; truncated: boolean; nodes: [number, number, string, number][];
  } {
    const ids = Array.from(this.nodes.keys()).sort((a, b) => a - b);
    const out: [number, number, string, number][] = [];
    for (const id of ids) {
      if (out.length >= limit) break;
      const node = this.nodes.get(id);
      if (!node) continue;
      const v = node.nodeType === Node.TEXT_NODE
        ? node.nodeValue ?? ''
        : this.names.get(id) ?? (node as Element).tagName?.toLowerCase() ?? '';
      // The image flag is left out on both sides: it marks the act of queueing
      // an image for transcoding, which the plane side never does, so the two
      // could never agree on it.
      out.push([id, node.nodeType, v.slice(0, 32), (this.flags.get(id) ?? 0) & ~NodeFlags.Image]);
    }
    return { total: ids.length, truncated: ids.length > out.length, nodes: out };
  }

  /** What the patcher knows about itself, for a diagnostic capture. */
  diag(): Record<string, unknown> {
    return {
      seq: this.seq,
      nodes: this.nodes.size,
      strings: this.strings.length,
      names: this.names.size,
      cssRules: this.cssRules.length,
      images: this.images.size,
      docHash: this.docHash(),
      hasRoot: this.root !== null,
      styleAttached: this.styleEl?.isConnected === true,
    };
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

/**
 * Tag-name test rather than `instanceof`. The mirrored document lives in a
 * different JavaScript realm (a sandboxed iframe), where `HTMLImageElement` is
 * a different constructor entirely, so `instanceof` is always false there.
 */
function isImage(el: Element): boolean {
  return el.tagName?.toUpperCase() === 'IMG';
}

/** Extracts the content hash from a skyhook image URL. */
export function imageHashOf(src: string): string {
  const m = /^skyhook:\/\/img\/([0-9a-f]+)/i.exec(src);
  return m ? m[1] : '';
}
