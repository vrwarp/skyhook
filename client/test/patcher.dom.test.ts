/**
 * Patcher tests. These run against jsdom because the patcher's whole job is
 * building real DOM nodes; asserting on a fake tree would test nothing.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { Patcher, imageHashOf } from '../src/mirror/patcher.js';
import { NodeFlags, NodeKind, OpCode, type Mutation, type Snapshot } from '../src/shared/protocol.js';

function snapshot(): Snapshot {
  return {
    strings: ['div', 'ul', 'li', 'first', 'second', 'id', 'log', 'input', 'text', 'type'],
    nodes: [
      { id: 1, parent: 0, kind: NodeKind.Element, ref: 0, attrs: [], flags: 0 },
      { id: 2, parent: 1, kind: NodeKind.Element, ref: 1, attrs: [5, 6], flags: 0 },
      { id: 3, parent: 2, kind: NodeKind.Element, ref: 2, attrs: [], flags: 0 },
      { id: 4, parent: 3, kind: NodeKind.Text, ref: 3, attrs: [], flags: 0 },
      { id: 5, parent: 2, kind: NodeKind.Element, ref: 2, attrs: [], flags: 0 },
      { id: 6, parent: 5, kind: NodeKind.Text, ref: 4, attrs: [], flags: 0 },
    ],
    css: ['ul{margin:0}'],
    url: 'https://example.test/',
    title: 'Fixture',
    viewport: { w: 800, h: 600, dpr: 1, mobile: false },
    images: [],
    scrollX: 0,
    scrollY: 0,
  };
}

/**
 * The document fingerprint the way the landside agent computes it, over the
 * wire rows rather than over any DOM. This is a transcription of
 * `__skyhook.docHash` in internal/mirror/agent.js, and of `Model.Hash` in
 * internal/mirror/model.go; the integrity check is only worth anything if all
 * three agree.
 */
function agentHash(snap: Snapshot): number {
  let h = 0x811c9dc5;
  const rows = [...snap.nodes].sort((a, b) => a.id - b.id);
  for (const n of rows) {
    // The agent fingerprints `tagName.toLowerCase()`, while the name on the
    // wire keeps the case SVG needs.
    const raw = snap.strings[n.ref] ?? '';
    const v = n.kind === NodeKind.Text ? raw : raw.toLowerCase();
    h ^= n.id & 0xff;
    h = Math.imul(h, 16777619) >>> 0;
    for (let i = 0; i < v.length && i < 32; i++) {
      h ^= v.charCodeAt(i) & 0xff;
      h = Math.imul(h, 16777619) >>> 0;
    }
  }
  return h >>> 0;
}

function mutation(ops: Partial<Mutation['ops'][number]>[], strings: string[] = []): Mutation {
  return {
    strings,
    docHash: 0,
    flush: false,
    ops: ops.map((o) => ({
      op: OpCode.Text, node: 0, parent: 0, before: 0, ref: 0, ref2: 0,
      nodes: [], off: 0, del: 0, add: [], drop: [], x: 0, y: 0, str: '',
      ...o,
    })) as Mutation['ops'],
  };
}

describe('Patcher', () => {
  let patcher: Patcher;

  beforeEach(() => {
    document.head.textContent = '';
    document.body.textContent = '';
    patcher = new Patcher(document);
  });

  it('builds the document from a snapshot', () => {
    patcher.applySnapshot(snapshot());
    const list = document.querySelector('ul');
    expect(list).not.toBeNull();
    expect(list?.getAttribute('id')).toBe('log');
    expect(document.body.textContent).toBe('firstsecond');
    expect(document.querySelector('style[data-skyhook-css]')?.textContent).toContain('ul{margin:0}');
  });

  /*
   * The snapshot's root goes into the body directly, with nothing between.
   *
   * The patcher builds the tree detached and swaps it in whole, and the
   * obvious way to hold a detached tree is a wrapper <div>. But a wrapper is a
   * box, and its height is auto, and the mirrored root is the page's own
   * <html>: `html, body { height: 100% }` — the way every full-height site on
   * the web says "fill the window" — then resolves against auto, computes to
   * auto, and the entire app collapses to the height of whatever happens to be
   * in normal flow. Google Chat rendered as a header, the word "Shortcuts",
   * and 800px of white.
   *
   * Nothing downstream can catch it: the wrapper is not a mirrored node, so
   * both sides hash the same document and agree. It is the box tree that
   * differs, and only the reader sees that. A DocumentFragment holds the tree
   * just as well and leaves no box behind, so the test is on the shape.
   */
  it('puts the snapshot root in the body with no box in between', () => {
    patcher.applySnapshot(snapshot());
    const root = document.querySelector('ul')?.parentElement;
    expect(root).not.toBeNull();
    expect(root?.parentElement).toBe(document.body);
    expect(patcher.rootElement).toBe(root);
    expect(document.body.children.length).toBe(1);
  });

  it('applies a move without rebuilding the subtree', () => {
    patcher.applySnapshot(snapshot());
    const before = document.querySelectorAll('li').length;
    patcher.applyMutation(mutation([{ op: OpCode.Move, node: 5, parent: 2, before: 3 }]), 1);
    expect(document.body.textContent).toBe('secondfirst');
    expect(document.querySelectorAll('li').length).toBe(before);
  });

  it('applies a text splice, which is how chat appends stay small', () => {
    patcher.applySnapshot(snapshot());
    patcher.applyMutation(mutation(
      [{ op: OpCode.Splice, node: 4, off: 5, del: 0, ref: 10 }],
      [' message'],
    ), 1);
    expect(document.body.textContent).toBe('first messagesecond');
  });

  it('inserts and removes subtrees', () => {
    patcher.applySnapshot(snapshot());
    patcher.applyMutation(mutation([{
      op: OpCode.Insert, parent: 2, before: 0,
      nodes: [
        { id: 10, parent: 2, kind: NodeKind.Element, ref: 2, attrs: [], flags: 0 },
        { id: 11, parent: 10, kind: NodeKind.Text, ref: 10, attrs: [], flags: 0 },
      ],
    }], ['third']), 1);
    expect(document.body.textContent).toBe('firstsecondthird');

    patcher.applyMutation(mutation([{ op: OpCode.Remove, node: 3 }]), 2);
    expect(document.body.textContent).toBe('secondthird');
    expect(patcher.nodeFor(4)).toBeUndefined();
  });

  it('never materialises script elements or inline handlers', () => {
    const snap = snapshot();
    snap.strings.push('script', 'onclick', 'alert(1)', 'javascript:alert(1)', 'href', 'a');
    const scriptRef = snap.strings.indexOf('script');
    snap.nodes.push(
      { id: 20, parent: 1, kind: NodeKind.Element, ref: scriptRef, attrs: [], flags: 0 },
      {
        id: 21, parent: 1, kind: NodeKind.Element,
        ref: snap.strings.indexOf('a'),
        attrs: [
          snap.strings.indexOf('onclick'), snap.strings.indexOf('alert(1)'),
          snap.strings.indexOf('href'), snap.strings.indexOf('javascript:alert(1)'),
        ],
        flags: 0,
      },
    );
    patcher.applySnapshot(snap);
    expect(document.querySelector('script')).toBeNull();
    const anchor = document.querySelector('a');
    expect(anchor?.getAttribute('onclick')).toBeNull();
    expect(anchor?.getAttribute('href')).toBeNull();
  });

  it('renders an inlined iframe document without materialising an iframe', () => {
    // The agent inlines a same-origin iframe's document as children of the
    // iframe element. Dropping the element used to take the whole document with
    // it, so the page simply had a hole where the frame was.
    const snap = snapshot();
    const base = snap.strings.length;
    snap.strings.push('iframe', 'inside the frame', 'data-sky-box', '300x150');
    snap.nodes.push(
      {
        id: 30, parent: 1, kind: NodeKind.Element, ref: base,
        attrs: [base + 2, base + 3], flags: 0,
      },
      { id: 31, parent: 30, kind: NodeKind.Element, ref: 0, attrs: [], flags: 0 },
      { id: 32, parent: 31, kind: NodeKind.Text, ref: base + 1, attrs: [], flags: 0 },
    );
    patcher.applySnapshot(snap);

    expect(document.querySelector('iframe')).toBeNull();
    const stand = document.querySelector('[data-skyhook-tag="iframe"]') as HTMLElement | null;
    expect(stand).not.toBeNull();
    expect(stand!.textContent).toBe('inside the frame');
    // Sized explicitly, because the CSS that sized the real iframe selects on a
    // tag name this element does not have.
    expect(stand!.style.width).toBe('300px');
    expect(stand!.style.height).toBe('150px');
  });

  /*
   * A frame that changes size after it was serialised. Every popover on a
   * Google property is one: the frame sits inside a wrapper the page animates
   * open, so the box the agent first measured is the closed one — zero high —
   * and a stand-in left at that size is a panel the reader never sees.
   */
  it('resizes a stand-in when the frame it stands for changes size', () => {
    const snap = snapshot();
    const base = snap.strings.length;
    snap.strings.push('iframe', 'data-sky-box', '370x0');
    snap.nodes.push({
      id: 30, parent: 1, kind: NodeKind.Element, ref: base,
      attrs: [base + 1, base + 2], flags: 0,
    });
    patcher.applySnapshot(snap);
    const stand = document.querySelector('[data-skyhook-tag="iframe"]') as HTMLElement;
    expect(stand.style.height).toBe('0px');

    // A mutation's strings extend the table the snapshot left, so the refs are
    // where they land in it.
    const boxRef = base + 1;
    let next = snap.strings.length;
    patcher.applyMutation(
      mutation([{ op: OpCode.Attr, node: 30, ref: boxRef, ref2: next }], ['370x570']), 1);
    expect(stand.style.width).toBe('370px');
    expect(stand.style.height).toBe('570px');

    // And back again: a popover that closes has to close here too, or it sits
    // over the page as a hole nothing can be clicked through.
    next += 1;
    patcher.applyMutation(
      mutation([{ op: OpCode.Attr, node: 30, ref: boxRef, ref2: next }], ['370x0']), 2);
    expect(stand.style.height).toBe('0px');
  });

  /*
   * The page's own `style` writes replace the whole declaration. The box is
   * ours and is not in them, so re-applying it is the only thing between a
   * frame and a collapse the agent has no reason to say anything more about.
   */
  it('keeps a stand-in its size through the page rewriting its style', () => {
    const snap = snapshot();
    const base = snap.strings.length;
    snap.strings.push('iframe', 'data-sky-box', '320x180');
    snap.nodes.push({
      id: 30, parent: 1, kind: NodeKind.Element, ref: base,
      attrs: [base + 1, base + 2], flags: 0,
    });
    patcher.applySnapshot(snap);
    const stand = document.querySelector('[data-skyhook-tag="iframe"]') as HTMLElement;

    const styleRef = snap.strings.length;
    patcher.applyMutation(
      mutation([{ op: OpCode.Attr, node: 30, ref: styleRef, ref2: styleRef + 1 }],
        ['style', 'border: 0; opacity: 1']), 1);
    expect(stand.style.opacity).toBe('1');
    expect(stand.style.width).toBe('320px');
    expect(stand.style.height).toBe('180px');

    // Taking the box away gives the pixels back rather than leaving the page
    // wearing a size nothing on the server believes in any more.
    patcher.applyMutation(
      mutation([{ op: OpCode.Attr, node: 30, ref: base + 1, ref2: -1 }]), 2);
    expect(stand.style.width).toBe('');
    expect(stand.style.height).toBe('');
  });

  /*
   * A tick and a chosen option are properties, and the agent reports them by
   * marking the element. Removing the mark is how it says the page unticked the
   * box — and removing an attribute does not unset a property, so without this
   * the mirror keeps showing a tick that landside stopped drawing.
   */
  it('unticks a control when its mark is taken away', () => {
    const snap = snapshot();
    const base = snap.strings.length;
    snap.strings.push('data-sky-checked', '1', 'option', 'data-sky-selected');
    snap.nodes.push(
      {
        id: 30, parent: 1, kind: NodeKind.Element, ref: snap.strings.indexOf('input'),
        attrs: [base, base + 1], flags: 0,
      },
      {
        id: 31, parent: 1, kind: NodeKind.Element, ref: base + 2,
        attrs: [base + 3, base + 1], flags: 0,
      },
    );
    patcher.applySnapshot(snap);
    const box = document.querySelector('input') as HTMLInputElement;
    const opt = document.querySelector('option') as HTMLOptionElement;
    expect(box.checked).toBe(true);
    expect(opt.selected).toBe(true);

    patcher.applyMutation(mutation([
      { op: OpCode.Attr, node: 30, ref: base, ref2: -1 },
      { op: OpCode.Attr, node: 31, ref: base + 3, ref2: -1 },
    ]), 1);
    expect(box.checked).toBe(false);
    expect(opt.selected).toBe(false);
  });

  /*
   * Everything the host paints into an element's inline style — a canvas's
   * photograph, an image's placeholder — is discarded by the page's next
   * `style` write, which carries none of it and was made for some unrelated
   * reason. The host is told so it can put its own back.
   */
  it('tells the host when a page style write discarded what it painted', () => {
    const restyled = vi.fn();
    patcher = new Patcher(document, { onRestyled: restyled });
    const snap = snapshot();
    const base = snap.strings.length;
    snap.strings.push('canvas', 'style', 'border: 1px solid #333');
    snap.nodes.push({
      id: 30, parent: 1, kind: NodeKind.Element, ref: base,
      attrs: [base + 1, base + 2], flags: NodeFlags.Canvas,
    });
    patcher.applySnapshot(snap);
    expect(restyled).not.toHaveBeenCalled();

    patcher.applyMutation(
      mutation([{ op: OpCode.Attr, node: 30, ref: base + 1, ref2: base + 3 }],
        ['border: 1px solid #999']), 1);
    const canvas = document.querySelector('canvas') as HTMLElement;
    expect(restyled).toHaveBeenCalledWith(canvas);
  });

  /*
   * A style attribute that names an image is written twice: once when it
   * arrives, and again every time bytes land, because only the wire form can be
   * resolved a second time. Both writes replace the whole declaration, so both
   * discard what the mirror had painted into it — and the second happens on a
   * document nobody touched, which is the one nothing else would put right.
   */
  it('keeps its own painting through a style attribute that names an image', () => {
    const restyled = vi.fn();
    patcher = new Patcher(document, { onRestyled: restyled });
    const snap = snapshot();
    const base = snap.strings.length;
    snap.strings.push('iframe', 'data-sky-box', '320x180', 'style',
      'background-image: url(skyhook://img/abc123)');
    snap.nodes.push({
      id: 30, parent: 1, kind: NodeKind.Element, ref: base,
      attrs: [base + 1, base + 2], flags: 0,
    });
    patcher.applySnapshot(snap);
    const stand = document.querySelector('[data-skyhook-tag="iframe"]') as HTMLElement;
    expect(stand.style.height).toBe('180px');

    patcher.applyMutation(
      mutation([{ op: OpCode.Attr, node: 30, ref: base + 3, ref2: base + 4 }]), 1);
    expect(stand.style.backgroundImage).toContain('skyhook://img/abc123');
    expect(stand.style.width).toBe('320px');
    expect(stand.style.height).toBe('180px');
    expect(restyled).toHaveBeenCalledWith(stand);

    // And again when the bytes arrive and the declaration is re-rendered.
    restyled.mockClear();
    stand.style.height = '';
    patcher.refreshCSS();
    expect(stand.style.height).toBe('180px');
    expect(restyled).toHaveBeenCalledWith(stand);
  });

  /*
   * An insert whose parent has not arrived yet. A tab is described by several
   * agents once it holds a cross-origin frame, each on a queue of its own, so a
   * frame's document can overtake the element it hangs from. Dropped, that is a
   * frame missing for the life of the page with nothing anywhere to say so.
   */
  it('keeps an insert whose parent has not arrived, and applies it when it does', () => {
    patcher.applySnapshot(snapshot());
    const base = 100;
    // A subtree hung from a node the client does not have.
    patcher.applyMutation(mutation([{
      op: OpCode.Insert, parent: 500, before: 0,
      nodes: [
        { id: 501, parent: 500, kind: NodeKind.Element, ref: 0, attrs: [], flags: 0 },
        { id: 502, parent: 501, kind: NodeKind.Text, ref: 3, attrs: [], flags: 0 },
      ],
    }]), 1);
    expect(patcher.nodeFor(501)).toBeUndefined();

    // The parent arrives; what was waiting goes in behind it.
    patcher.applyMutation(mutation([{
      op: OpCode.Insert, parent: 1, before: 0,
      nodes: [{ id: 500, parent: 1, kind: NodeKind.Element, ref: 0, attrs: [], flags: 0 }],
    }]), 2);
    expect(patcher.nodeFor(500)).toBeDefined();
    expect(patcher.nodeFor(501)).toBeDefined();
    expect((patcher.nodeFor(500) as Element).textContent).toBe('first');
    expect(base).toBe(100);
  });

  it('hashes what the server sent, so a substitution is not a divergence', () => {
    // The server compares this hash against the agent's every thirty seconds
    // and re-snapshots the whole document when they differ. A patcher that
    // hashed the elements it happened to build would report a divergence on
    // every page with an iframe, forever.
    const snap = snapshot();
    const base = snap.strings.length;
    snap.strings.push('iframe', 'script', 'inside');
    snap.nodes.push(
      { id: 30, parent: 1, kind: NodeKind.Element, ref: base, attrs: [], flags: 0 },
      { id: 31, parent: 30, kind: NodeKind.Text, ref: base + 2, attrs: [], flags: 0 },
      { id: 32, parent: 1, kind: NodeKind.Element, ref: base + 1, attrs: [], flags: 0 },
    );
    patcher.applySnapshot(snap);
    expect(patcher.docHash()).toBe(agentHash(snap));
  });

  it('reflects live form values so a resync restores what was typed', () => {
    const snap = snapshot();
    snap.strings.push('data-sky-value', 'hello there');
    snap.nodes.push({
      id: 30, parent: 1, kind: NodeKind.Element,
      ref: snap.strings.indexOf('input'),
      attrs: [snap.strings.indexOf('data-sky-value'), snap.strings.indexOf('hello there')],
      flags: 1,
    });
    patcher.applySnapshot(snap);
    const input = document.querySelector('input') as HTMLInputElement;
    expect(input.value).toBe('hello there');
    expect(input.getAttribute('data-skyhook-editable')).toBe('1');
  });

  it('holds mutations that touch a client-owned subtree', () => {
    const deferred = vi.fn();
    const owned = new Set<number>();
    patcher = new Patcher(document, {
      isOwned: (node) => owned.has(patcher.idOf(node)),
      onDeferred: deferred,
    });
    patcher.applySnapshot(snapshot());
    owned.add(3);
    patcher.applyMutation(mutation([{ op: OpCode.Text, node: 4, ref: 4 }]), 1);
    expect(deferred).toHaveBeenCalledTimes(1);
    expect(document.body.textContent).toBe('firstsecond'); // untouched
  });

  it('tolerates ops for nodes it no longer has', () => {
    patcher.applySnapshot(snapshot());
    expect(() => patcher.applyMutation(mutation([
      { op: OpCode.Text, node: 999, ref: 3 },
      { op: OpCode.Remove, node: 998 },
      { op: OpCode.Attr, node: 997, ref: 5, ref2: 6 },
    ]), 1)).not.toThrow();
  });

  it('produces a document hash that changes with content', () => {
    patcher.applySnapshot(snapshot());
    const before = patcher.docHash();
    patcher.applyMutation(mutation([{ op: OpCode.Text, node: 4, ref: 4 }]), 1);
    expect(patcher.docHash()).not.toBe(before);
  });

  it('resolves node ids from descendants, which is what click targeting needs', () => {
    patcher.applySnapshot(snapshot());
    const text = document.querySelector('li')?.firstChild as Node;
    expect(patcher.idOf(text)).toBe(4);
  });

  // Logos, icons and chart furniture are all SVG. Built with createElement they
  // land in the HTML namespace, where they draw nothing at all and `viewBox`
  // decays to `viewbox`.
  it('builds SVG in the SVG namespace, with its names and attributes intact', () => {
    const snap = snapshot();
    const base = snap.strings.length;
    snap.strings.push('svg', 'viewBox', '0 0 16 16', 'clipPath', 'ic', 'id', 'path');
    snap.nodes.push(
      { id: 10, parent: 2, kind: NodeKind.Element, ref: base, attrs: [base + 1, base + 2], flags: 0 },
      { id: 11, parent: 10, kind: NodeKind.Element, ref: base + 3, attrs: [base + 5, base + 4], flags: 0 },
      { id: 12, parent: 11, kind: NodeKind.Element, ref: base + 6, attrs: [], flags: 0 },
    );
    patcher.applySnapshot(snap);

    const svg = document.querySelector('svg')!;
    expect(svg.namespaceURI).toBe('http://www.w3.org/2000/svg');
    // Case-folded to `viewbox` the attribute is inert, and the drawing has no
    // coordinate system.
    expect(svg.getAttribute('viewBox')).toBe('0 0 16 16');
    // SVG element names are case-sensitive: `clippath` clips nothing.
    const clip = svg.firstElementChild!;
    expect(clip.localName).toBe('clipPath');
    // And the namespace is inherited, or `path` is an unknown HTML element.
    expect(clip.firstElementChild!.namespaceURI).toBe('http://www.w3.org/2000/svg');

    // The fingerprint still has to match the agent's, which lowercases.
    expect(patcher.docHash()).toBe(agentHash(snap));
  });

  it('reports the flags it was sent, which the hash does not cover', () => {
    const snap = snapshot();
    const base = snap.strings.length;
    snap.strings.push('sky-card');
    // A custom element the server saw with a shadow root, and one it did not.
    snap.nodes.push(
      { id: 10, parent: 2, kind: NodeKind.Element, ref: base, attrs: [], flags: NodeFlags.Shadow },
      { id: 11, parent: 2, kind: NodeKind.Element, ref: base, attrs: [], flags: 0 },
    );
    patcher.applySnapshot(snap);

    const rows = new Map(patcher.fingerprint().nodes.map((n) => [n[0], n]));
    expect(rows.get(10)![3]).toBe(NodeFlags.Shadow);
    expect(rows.get(11)![3]).toBe(0);
    // Two elements identical in every column the hash reads. Without the
    // flags, a mirror holding the wrong one of them looks perfectly healthy.
    expect(rows.get(10)!.slice(0, 3)).toEqual(rows.get(11)!.slice(0, 3).map((v, i) => (i === 0 ? 10 : v)));
  });

  it('puts HTML back into the HTML namespace inside foreignObject', () => {
    const snap = snapshot();
    const base = snap.strings.length;
    snap.strings.push('svg', 'foreignObject', 'p');
    snap.nodes.push(
      { id: 10, parent: 2, kind: NodeKind.Element, ref: base, attrs: [], flags: 0 },
      { id: 11, parent: 10, kind: NodeKind.Element, ref: base + 1, attrs: [], flags: 0 },
      { id: 12, parent: 11, kind: NodeKind.Element, ref: base + 2, attrs: [], flags: 0 },
    );
    patcher.applySnapshot(snap);

    const p = document.querySelector('p')!;
    expect(p.namespaceURI).toBe('http://www.w3.org/1999/xhtml');
  });
});

describe('imageHashOf', () => {
  it('extracts the content hash from a mirror image URL', () => {
    expect(imageHashOf('skyhook://img/deadbeef')).toBe('deadbeef');
    expect(imageHashOf('https://example.com/a.png')).toBe('');
  });
});
