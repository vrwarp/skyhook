/**
 * Patcher tests. These run against jsdom because the patcher's whole job is
 * building real DOM nodes; asserting on a fake tree would test nothing.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { Patcher, imageHashOf } from '../src/preload/patcher.js';
import { NodeKind, OpCode, type Mutation, type Snapshot } from '../src/shared/protocol.js';

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
    speculative: false,
  };
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
});

describe('imageHashOf', () => {
  it('extracts the content hash from a mirror image URL', () => {
    expect(imageHashOf('skyhook://img/deadbeef')).toBe('deadbeef');
    expect(imageHashOf('https://example.com/a.png')).toBe('');
  });
});
