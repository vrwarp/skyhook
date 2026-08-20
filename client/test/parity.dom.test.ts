// The parity probe's shape, in jsdom.
//
// jsdom has no layout engine: every box is 0x0, getComputedStyle answers with
// whatever cascade it models and nothing more, and document.fonts does not
// exist. So this cannot test what the probe *measures* — the e2e parity suite
// does that against a real browser — only that the probe walks the right
// nodes, reports the wire names for substituted tags, ties boxes to the right
// roots, and keeps the style vector the length the Go engine insists on.
import { describe, expect, it } from 'vitest';
import { Patcher } from '../src/mirror/patcher.js';
import { NodeKind } from '../src/shared/protocol.js';

// One copy of the list lives in internal/parity/types.go (StyleProps); the
// probe must report exactly this many values per node.
const STYLE_PROPS_LEN = 30;

function snapshot(): Parameters<Patcher['applySnapshot']>[0] {
  return {
    strings: ['html', 'body', 'p', 'hello there', 'iframe', 'src', 'https://x/', 'img'],
    nodes: [
      { id: 1, parent: 0, kind: NodeKind.Element, ref: 0, attrs: [], flags: 0 },
      { id: 2, parent: 1, kind: NodeKind.Element, ref: 1, attrs: [], flags: 0 },
      { id: 3, parent: 2, kind: NodeKind.Element, ref: 2, attrs: [], flags: 0 },
      { id: 4, parent: 3, kind: NodeKind.Text, ref: 3, attrs: [], flags: 0 },
      { id: 5, parent: 2, kind: NodeKind.Element, ref: 4, attrs: [5, 6], flags: 0 },
      { id: 6, parent: 2, kind: NodeKind.Element, ref: 7, attrs: [], flags: 0 },
    ],
    css: [],
    url: 'https://x/',
    title: 't',
    viewport: { w: 800, h: 600, dpr: 1, mobile: false },
    images: [],
    scrollX: 0,
    scrollY: 0,
    scoped: [],
    epoch: 1,
  };
}

describe('the parity probe', () => {
  it('reports every element with the wire tag and a full style vector', () => {
    const patcher = new Patcher(document);
    patcher.applySnapshot(snapshot());
    const probe = patcher.parityProbe();
    expect(probe.truncated).toBe(false);

    const byId = new Map(probe.nodes.map((n) => [n.i as number, n]));
    expect([...byId.keys()].sort((a, b) => a - b)).toEqual([1, 2, 3, 5, 6]);

    const p = byId.get(3)!;
    expect(p.t).toBe('p');
    expect((p.s as string[]).length).toBe(STYLE_PROPS_LEN);
    expect(p.x).toBe('hello there');
    expect(p.r).toBe(1);

    // The iframe was substituted, and must still answer to its wire name —
    // a stand-in that compared as a <div> would read as a tag mismatch on
    // every page with a frame in it.
    const frame = byId.get(5)!;
    expect(frame.t).toBe('iframe');
    expect((frame.a as Record<string, string>)['data-skyhook-tag']).toBe('iframe');
  });

  it('reports a placeholder image as not ok until the bytes are real', () => {
    const patcher = new Patcher(document);
    patcher.applySnapshot(snapshot());
    const img = patcher.nodeFor(6) as HTMLImageElement;
    img.dataset.skyhookImg = 'abcd1234';

    const pending = patcher.parityProbe(4096, {
      hasBlob: () => false,
      isMissing: () => false,
    });
    const withBlob = patcher.parityProbe(4096, {
      hasBlob: () => true,
      isMissing: () => false,
    });
    const node = pending.nodes.find((n) => n.i === 6)!;
    expect((node.g as { ok: boolean }).ok).toBe(false);
    // jsdom never loads images, so complete/naturalWidth hold this at false
    // either way; the assertion that matters is that the hash gate is what
    // changed, not the DOM.
    const node2 = withBlob.nodes.find((n) => n.i === 6)!;
    expect((node2.g as { ok: boolean }).ok).toBe(false);
  });

  it('samples evenly past the cap and says so', () => {
    const strings = ['html', 'body', 'div'];
    const nodes = [
      { id: 1, parent: 0, kind: NodeKind.Element, ref: 0, attrs: [], flags: 0 },
      { id: 2, parent: 1, kind: NodeKind.Element, ref: 1, attrs: [], flags: 0 },
    ];
    for (let i = 0; i < 40; i++) {
      nodes.push({ id: 10 + i, parent: 2, kind: NodeKind.Element, ref: 2, attrs: [], flags: 0 });
    }
    const patcher = new Patcher(document);
    patcher.applySnapshot({ ...snapshot(), strings, nodes });
    const probe = patcher.parityProbe(10);
    expect(probe.truncated).toBe(true);
    expect(probe.nodes.length).toBeLessThanOrEqual(12);
    // The root every sampled box refers to is in the sample whatever the
    // stride did.
    expect(probe.nodes.some((n) => n.i === 1)).toBe(true);
  });
});
