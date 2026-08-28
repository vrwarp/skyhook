/**
 * A mirrored sub-document lives inside a shadow root, and its stylesheet lives
 * in that root's own sheet. These tests are about the boundary: that it is
 * built at all, that content lands inside it, and that a rule scoped to it does
 * not reach the page around it.
 *
 * jsdom, like the browser, refuses to append a ShadowRoot and retargets events
 * at a boundary, so the parts that matter are real here.
 */
import { beforeEach, describe, expect, it } from 'vitest';

import { Patcher } from '../src/mirror/patcher.js';
import { NodeKind, OpCode, type Mutation, type Snapshot } from '../src/shared/protocol.js';

/**
 * The shape the agent sends for a page holding an inlined frame:
 *
 *   html > body > div[data-skyhook-tag=iframe] > #shadow-root > html > body > p
 *
 * The stand-in for the frame is an ordinary element; the root hangs off it and
 * everything the frame contained is inside.
 */
function framedSnapshot(): Snapshot {
  return {
    strings: ['html', 'body', 'div', 'p', 'inside the frame', 'data-skyhook-tag', 'iframe'],
    nodes: [
      { id: 1, parent: 0, kind: NodeKind.Element, ref: 0, attrs: [], flags: 0 },
      { id: 2, parent: 1, kind: NodeKind.Element, ref: 1, attrs: [], flags: 0 },
      { id: 3, parent: 2, kind: NodeKind.Element, ref: 2, attrs: [5, 6], flags: 0 },
      // The shadow root. No name, no text: it is a boundary.
      { id: 4, parent: 3, kind: NodeKind.Fragment, ref: -1, attrs: [], flags: 0 },
      { id: 5, parent: 4, kind: NodeKind.Element, ref: 0, attrs: [], flags: 0 },
      { id: 6, parent: 5, kind: NodeKind.Element, ref: 1, attrs: [], flags: 0 },
      { id: 7, parent: 6, kind: NodeKind.Element, ref: 3, attrs: [], flags: 0 },
      { id: 8, parent: 7, kind: NodeKind.Text, ref: 4, attrs: [], flags: 0 },
    ],
    css: ['body{margin:0}'],
    scoped: [{ root: 4, rules: ['p{color:rgb(41,42,43)}'] }],
    url: 'https://example.test/framed',
    title: 'Framed',
    viewport: { w: 800, h: 600, dpr: 1, mobile: false },
    images: [],
    scrollX: 0,
    scrollY: 0,
  };
}

describe('a mirrored sub-document inside a shadow root', () => {
  let patcher: Patcher;

  beforeEach(() => {
    document.body.innerHTML = '';
    document.head.innerHTML = '';
    patcher = new Patcher(document);
  });

  it('builds the boundary rather than appending it', () => {
    patcher.applySnapshot(framedSnapshot());
    const box = document.querySelector('[data-skyhook-tag="iframe"]');
    expect(box).not.toBeNull();
    expect(box?.shadowRoot).not.toBeNull();
  });

  it('puts the frame’s content inside the root, not beside it', () => {
    patcher.applySnapshot(framedSnapshot());
    const root = document.querySelector('[data-skyhook-tag="iframe"]')?.shadowRoot;
    expect(root?.textContent).toContain('inside the frame');
    // And nowhere else: content in the light DOM would be content the frame's
    // own stylesheet no longer governs.
    expect(document.body.textContent ?? '').not.toContain('inside the frame');
  });

  it('registers every node, so the hash still describes the document', () => {
    patcher.applySnapshot(framedSnapshot());
    // Eight rows in, eight nodes tracked. A row the client drops is a row the
    // agent counted, and the integrity check would resnapshot for ever.
    expect(patcher.size).toBe(8);
  });

  it('keeps a scoped rule out of the document’s stylesheet', () => {
    patcher.applySnapshot(framedSnapshot());
    const sheet = document.querySelector('style[data-skyhook-css]');
    expect(sheet?.textContent ?? '').not.toContain('rgb(41,42,43)');
    expect(sheet?.textContent ?? '').toContain('body{margin:0}');
  });

  it('routes a scoped style op to its root and not to the page', () => {
    patcher.applySnapshot(framedSnapshot());
    const mutation: Mutation = {
      strings: [],
      ops: [{
        op: OpCode.Style, node: 4, parent: 0, before: 0, ref: 0, ref2: 0,
        nodes: [], off: 0, del: 0, add: ['p{font-weight:700}'], x: 0, y: 0, str: '',
      }],
      docHash: 0,
      flush: false,
    };
    patcher.applyMutation(mutation, 1);
    const sheet = document.querySelector('style[data-skyhook-css]');
    expect(sheet?.textContent ?? '').not.toContain('font-weight:700');
  });

  it('survives a second snapshot, where the root already exists', () => {
    patcher.applySnapshot(framedSnapshot());
    patcher.applySnapshot(framedSnapshot());
    const box = document.querySelector('[data-skyhook-tag="iframe"]');
    expect(box?.shadowRoot).not.toBeNull();
    expect(box?.shadowRoot?.textContent).toContain('inside the frame');
    expect(patcher.size).toBe(8);
  });
});
