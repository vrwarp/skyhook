/**
 * Mirror host tests.
 *
 * The first test is the important one: the sandbox attribute is the single
 * mechanism standing between mirrored content and script execution plane-side.
 * If someone ever adds `allow-scripts` to make something convenient work, this
 * fails loudly.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { MirrorHost, imageURL } from '../src/mirror/host.js';
import { NodeKind, OpCode, type Mutation, type Snapshot } from '../src/shared/protocol.js';

function snapshot(): Snapshot {
  return {
    strings: ['div', 'ul', 'li', 'first', 'second', 'id', 'log', 'form', 'input', 'name', 'q'],
    nodes: [
      { id: 1, parent: 0, kind: NodeKind.Element, ref: 0, attrs: [], flags: 0 },
      { id: 2, parent: 1, kind: NodeKind.Element, ref: 1, attrs: [5, 6], flags: 0 },
      { id: 3, parent: 2, kind: NodeKind.Element, ref: 2, attrs: [], flags: 0 },
      { id: 4, parent: 3, kind: NodeKind.Text, ref: 3, attrs: [], flags: 0 },
    ],
    css: [],
    url: 'https://example.test/',
    title: 'Fixture',
    viewport: { w: 800, h: 600, dpr: 1, mobile: false },
    images: [],
    scrollX: 0,
    scrollY: 0,
    speculative: false,
  };
}

function events() {
  return {
    input: vi.fn(),
    scroll: vi.fn(),
    applied: vi.fn(),
    wantImages: vi.fn(),
  };
}

async function mount(): Promise<{ host: MirrorHost; ev: ReturnType<typeof events> }> {
  const ev = events();
  const host = new MirrorHost(1, ev);
  document.body.appendChild(host.frame);
  await host.whenReady();
  return { host, ev };
}

describe('MirrorHost', () => {
  beforeEach(() => {
    document.body.innerHTML = '';
  });

  it('sandboxes the mirror frame without allowing scripts', async () => {
    const { host } = await mount();
    const sandbox = host.frame.getAttribute('sandbox') ?? '';
    const tokens = sandbox.split(/\s+/).filter(Boolean);
    // allow-same-origin is required so the patcher can reach contentDocument.
    expect(tokens).toContain('allow-same-origin');
    // Everything else that could execute code or reach the network stays off.
    for (const forbidden of [
      'allow-scripts', 'allow-top-navigation', 'allow-popups', 'allow-forms',
      'allow-modals', 'allow-downloads', 'allow-presentation',
    ]) {
      expect(tokens).not.toContain(forbidden);
    }
  });

  it('applies a snapshot into the frame, not the host document', async () => {
    const { host } = await mount();
    host.applySnapshot(snapshot());
    const doc = host.frame.contentDocument!;
    expect(doc.body.textContent).toBe('first');
    // The app shell's own document must stay clean.
    expect(document.body.textContent).toBe('');
    expect(host.nodes).toBeGreaterThan(0);
  });

  it('acknowledges each applied batch with a document hash', async () => {
    const { host, ev } = await mount();
    host.applySnapshot(snapshot());
    expect(ev.applied).toHaveBeenCalledWith(1, 0, expect.any(Number));
    const [, , hash] = ev.applied.mock.calls[0] as [number, number, number];
    expect(hash).toBeGreaterThan(0);
  });

  it('turns a click inside the frame into a semantic input event', async () => {
    const { host, ev } = await mount();
    host.applySnapshot(snapshot());
    const doc = host.frame.contentDocument!;
    const li = doc.querySelector('li')!;
    li.dispatchEvent(new doc.defaultView!.MouseEvent('click', { bubbles: true }));
    expect(ev.input).toHaveBeenCalledTimes(1);
    const [tab, payload] = ev.input.mock.calls[0] as [number, Record<string, unknown>];
    expect(tab).toBe(1);
    expect(payload.kind).toBe('click');
    expect(payload.node).toBe(3);
    expect(payload.seq).toBe(1);
  });

  it('asks for images the frame references', async () => {
    const { host, ev } = await mount();
    const snap = snapshot();
    snap.strings.push('img', 'skyhook://img/deadbeef', 'src');
    snap.nodes.push({
      id: 10, parent: 2, kind: NodeKind.Element,
      ref: snap.strings.indexOf('img'),
      attrs: [snap.strings.indexOf('src'), snap.strings.indexOf('skyhook://img/deadbeef')],
      flags: 2,
    });
    snap.images.push({
      node: 10, hash: 'deadbeef', w: 10, h: 10, blur: '', mime: 'image/png',
      bytes: 100, priority: 0, alt: '',
    });
    host.applySnapshot(snap);

    expect(ev.wantImages).toHaveBeenCalledWith(1, ['deadbeef']);
    // The wire form never reaches the DOM: it would be an unfetchable scheme.
    const img = host.frame.contentDocument!.querySelector('img')!;
    expect(img.getAttribute('src')).toBe(imageURL('deadbeef'));
  });

  it('sends a form submission when a submit control is clicked', async () => {
    // The frame has no allow-forms, so no native submit event ever fires; the
    // host has to recognise the control itself.
    const { host, ev } = await mount();
    const snap = snapshot();
    const base = snap.strings.length;
    snap.strings.push('form', 'input', 'name', 'q', 'button', 'type', 'submit', 'data-sky-value', 'hello');
    snap.nodes.push(
      { id: 20, parent: 2, kind: NodeKind.Element, ref: base, attrs: [], flags: 0 },
      {
        id: 21, parent: 20, kind: NodeKind.Element, ref: base + 1,
        attrs: [base + 2, base + 3, base + 7, base + 8], flags: 1,
      },
      {
        id: 22, parent: 20, kind: NodeKind.Element, ref: base + 4,
        attrs: [base + 5, base + 6], flags: 0,
      },
    );
    host.applySnapshot(snap);

    const doc = host.frame.contentDocument!;
    const button = doc.querySelector('button')!;
    button.dispatchEvent(new doc.defaultView!.MouseEvent('click', { bubbles: true }));

    const submit = ev.input.mock.calls
      .map((c) => c[1] as Record<string, unknown>)
      .find((p) => p.kind === 'submit');
    expect(submit).toBeDefined();
    expect(submit!.node).toBe(20);
    expect((submit!.fields as Record<string, string>).q).toBe('hello');
  });

  it('applies mutations and keeps the sequence for acknowledgement', async () => {
    const { host, ev } = await mount();
    host.applySnapshot(snapshot());
    const mutation: Mutation = {
      strings: [' and more'],
      docHash: 0,
      flush: false,
      ops: [{
        op: OpCode.Splice, node: 4, parent: 0, before: 0, ref: 11, ref2: 0,
        nodes: [], off: 5, del: 0, add: [], drop: [], x: 0, y: 0, str: '',
      }],
    };
    host.applyMutation(mutation, 3);
    expect(host.frame.contentDocument!.body.textContent).toBe('first and more');
    expect(ev.applied).toHaveBeenLastCalledWith(1, 3, expect.any(Number));
  });
});
