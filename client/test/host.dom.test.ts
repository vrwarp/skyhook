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

/** A batch carrying one scroll op, the way the server reports landside scroll. */
function scrollOp(node: number, x: number, y: number): Mutation {
  return {
    strings: [], docHash: 0, flush: false,
    ops: [{
      op: OpCode.Scroll, node, parent: 0, before: 0, ref: 0, ref2: 0,
      nodes: [], off: 0, del: 0, add: [], drop: [], x, y, str: '',
    }],
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

  it('refuses to follow a link even when it cannot place it', async () => {
    // A link the patcher has no id for used to fall through the click handler
    // untouched, and the frame followed it: the plane side fetches a URL, which
    // this client must never do, and the frame lands on a cross-origin document
    // the patcher can never touch again.
    const { host, ev } = await mount();
    host.applySnapshot(snapshot());
    const doc = host.frame.contentDocument!;
    const orphan = doc.createElement('a');
    orphan.setAttribute('href', 'https://example.test/somewhere');
    orphan.textContent = 'not in the patcher';
    doc.body.appendChild(orphan);

    const delivered = orphan.dispatchEvent(
      new doc.defaultView!.MouseEvent('click', { bubbles: true, cancelable: true }));
    expect(delivered).toBe(false); // false means the default was prevented
    // And nothing is sent for a node the server could not act on anyway.
    expect(ev.input).not.toHaveBeenCalled();
  });

  it('reserves an image its box before the bytes exist', async () => {
    // Until the bitmap arrives the frame holds a 1x1 placeholder. Without a
    // reserved box every image that lands pushes the page down under whoever
    // is reading it.
    const { host } = await mount();
    const snap = snapshot();
    snap.strings.push('img', 'skyhook://img/c0ffee', 'src');
    snap.nodes.push({
      id: 10, parent: 2, kind: NodeKind.Element,
      ref: snap.strings.indexOf('img'),
      attrs: [snap.strings.indexOf('src'), snap.strings.indexOf('skyhook://img/c0ffee')],
      flags: 2,
    });
    host.applySnapshot(snap);

    const img = host.frame.contentDocument!.querySelector('img')!;
    // The agent could not measure this one: no width, no height, no box.
    expect(img.hasAttribute('width')).toBe(false);

    host.setImageMeta({
      node: 10, hash: 'c0ffee', w: 320, h: 240, blur: '', mime: 'image/png',
      bytes: 900, priority: 0, alt: '',
    });
    expect(img.getAttribute('width')).toBe('320');
    expect(img.getAttribute('height')).toBe('240');
    expect(img.style.aspectRatio).toBe('320 / 240');
  });

  it('leaves the scroll position alone when the server resends the same document', async () => {
    const { host } = await mount();
    const win = host.frame.contentWindow!;
    const scrollTo = vi.spyOn(win, 'scrollTo').mockImplementation(() => {});

    const first = snapshot();
    first.scrollY = 900;
    host.applySnapshot(first);
    // A new document adopts the landside position: that is what carries a
    // #fragment target.
    expect(scrollTo).toHaveBeenLastCalledWith(0, 900);

    // The same URL again is a resync — a gap the server could not close with
    // diffs — and the reader should not be able to tell it happened.
    const again = snapshot();
    again.scrollY = 4000;
    host.applySnapshot(again);
    expect(scrollTo).toHaveBeenLastCalledWith(0, 0);
    scrollTo.mockRestore();
  });

  it('refuses to move a reader who has scrolled', async () => {
    const { host } = await mount();
    const win = host.frame.contentWindow!;
    host.applySnapshot(snapshot());

    const scrollTo = vi.spyOn(win, 'scrollTo').mockImplementation(() => {});
    // The reader scrolls. jsdom does not move the window itself, so say where
    // they landed and announce it the way a real scroll would.
    Object.defineProperty(win, 'scrollY', { value: 1500, configurable: true });
    win.dispatchEvent(new Event('scroll'));

    host.applyMutation(scrollOp(0, 0, 8966), 4);
    expect(scrollTo).not.toHaveBeenCalled();
    scrollTo.mockRestore();
  });

  it('still follows a container the reader has not touched, and stops once they have', async () => {
    const { host } = await mount();
    host.applySnapshot(snapshot());
    const doc = host.frame.contentDocument!;
    const list = doc.querySelector('ul')!;

    host.applyMutation(scrollOp(2, 0, 120), 4);
    expect(list.scrollTop).toBe(120);

    // The reader scrolls the container themselves; it is theirs from here.
    list.scrollTop = 40;
    list.dispatchEvent(new Event('scroll'));
    host.applyMutation(scrollOp(2, 0, 500), 5);
    expect(list.scrollTop).toBe(40);
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
