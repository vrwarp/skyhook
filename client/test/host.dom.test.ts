/**
 * Mirror host tests.
 *
 * The first test is the important one: the sandbox attribute is the single
 * mechanism standing between mirrored content and script execution plane-side.
 * If someone ever adds `allow-scripts` to make something convenient work, this
 * fails loudly.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { MirrorHost, nearestList, type MenuTarget, type PullState } from '../src/mirror/host.js';
import { imageCacheKey } from '../src/shared/caches.js';
import {
  NodeFlags, NodeKind, OpCode, type ImageMeta, type Mutation, type Snapshot,
} from '../src/shared/protocol.js';

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

/** Adds a link inside the fixture's <li>, with the href the agent would send. */
function withLink(snap: Snapshot, href = 'https://example.test/next'): Snapshot {
  const base = snap.strings.length;
  snap.strings.push('a', 'href', href, 'Next page');
  snap.nodes.push(
    {
      id: 30, parent: 3, kind: NodeKind.Element, ref: base,
      attrs: [base + 1, base + 2], flags: 0,
    },
    { id: 31, parent: 30, kind: NodeKind.Text, ref: base + 3, attrs: [], flags: 0 },
  );
  return snap;
}

/** Adds an element the fixture's links can point a #fragment at. */
function withTarget(snap: Snapshot, id = 'c2'): Snapshot {
  const base = snap.strings.length;
  snap.strings.push('div', 'id', id);
  snap.nodes.push({
    id: 40, parent: 3, kind: NodeKind.Element, ref: base,
    attrs: [base + 1, base + 2], flags: 0,
  });
  return snap;
}

function events() {
  return {
    input: vi.fn(),
    scroll: vi.fn(),
    applied: vi.fn(),
    wantImages: vi.fn(),
    openLink: vi.fn(),
    navigating: vi.fn(),
    menu: vi.fn(),
    pull: vi.fn(),
    dismiss: vi.fn(() => false),
  };
}

/**
 * A popover with a search field, a second field and a result to click: the
 * shape every "new chat" dialog and every command palette has, and the one
 * where a blur sent on its own closes the thing the reader was aiming at.
 */
function withSearchPopover(snap: Snapshot): Snapshot {
  const base = snap.strings.length;
  snap.strings.push('input', 'button', 'Ada Lovelace');
  snap.nodes.push(
    { id: 50, parent: 2, kind: NodeKind.Element, ref: base, attrs: [], flags: NodeFlags.Editable },
    { id: 51, parent: 2, kind: NodeKind.Element, ref: base, attrs: [], flags: NodeFlags.Editable },
    { id: 52, parent: 2, kind: NodeKind.Element, ref: base + 1, attrs: [], flags: 0 },
    { id: 53, parent: 52, kind: NodeKind.Text, ref: base + 2, attrs: [], flags: 0 },
  );
  return snap;
}

async function mount(): Promise<{ host: MirrorHost; ev: ReturnType<typeof events> }> {
  const ev = events();
  const host = new MirrorHost(1, ev);
  document.body.appendChild(host.frame);
  await host.whenReady();
  return { host, ev };
}

/** The fixture with a canvas in it: the one element the mirror pans instead of
 *  clicking, and the only one a gesture is ever claimed on. */
async function mountWithCanvas(): Promise<{
  host: MirrorHost; ev: ReturnType<typeof events>;
}> {
  const { host, ev } = await mount();
  const snap = snapshot();
  snap.strings.push('canvas');
  snap.nodes.push({
    id: 10, parent: 2, kind: NodeKind.Element,
    ref: snap.strings.indexOf('canvas'), attrs: [], flags: NodeFlags.Canvas,
  });
  host.applySnapshot(snap);
  return { host, ev };
}

describe('MirrorHost', () => {
  // A tab is drawn, frame and all, a round trip before the server names it.
  // Everything that goes looking for one tab's document goes by the frame's
  // label, so the label has to follow the tab across being named.
  it('relabels its frame with the id the server gives the tab', async () => {
    const ev = events();
    const host = new MirrorHost(-1, ev);
    document.body.appendChild(host.frame);
    await host.whenReady();
    expect(host.frame.dataset.tab).toBe('-1');

    host.adopt(7);
    expect(host.tab).toBe(7);
    expect(host.frame.dataset.tab).toBe('7');
    host.destroy();
  });

  // A load event that lands before the frame has a document is not the load
  // that matters, and spending the subscription on it strands the tab.
  //
  // `ready` is what every snapshot and every batch for a tab awaits, so a
  // promise that never settles is a tab that is never drawn and — worse, because
  // it is what the server goes on — never acknowledges anything. The server
  // reads that as a client that has stopped short of a page it has already
  // sent, and only its thirty-second stalled-resync gets the page there at all.
  // Nothing anywhere reports the frame as the reason.
  it('keeps waiting for a document when a load arrives before there is one', async () => {
    const host = new MirrorHost(1, events());
    expect(host.frame.contentDocument).toBeNull();
    host.frame.dispatchEvent(new Event('load'));

    document.body.appendChild(host.frame);
    const stranded = Symbol('stranded');
    const settled = await Promise.race([
      host.whenReady().then(() => 'ready'),
      new Promise((r) => { setTimeout(() => r(stranded), 1000); }),
    ]);
    expect(settled).toBe('ready');
    host.destroy();
  });

  beforeEach(() => {
    document.body.innerHTML = '';
  });

  // Unconditionally, not at the end of the tests that stub: an assertion that
  // fails never reaches its own cleanup, and a leaked global URL takes every
  // test after it down with it — which reads as six broken features rather
  // than one broken test.
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it('sandboxes the mirror frame without allowing scripts', async () => {
    const { host } = await mount();
    const sandbox = host.frame.getAttribute('sandbox') ?? '';
    const tokens = sandbox.split(/\s+/).filter(Boolean);
    // allow-same-origin is required so the patcher can reach contentDocument.
    expect(tokens).toContain('allow-same-origin');
    // allow-modals is for exactly one call: the shell's print() on the frame
    // (P-110). Every way content could open a modal — alert, confirm,
    // beforeunload, a javascript: URL — needs script, and allow-scripts is
    // the flag this test exists to keep off, so the grant widens only what
    // the shell itself may ask for.
    expect(tokens).toContain('allow-modals');
    // Everything that could execute code or reach the network stays off.
    for (const forbidden of [
      'allow-scripts', 'allow-top-navigation', 'allow-popups', 'allow-forms',
      'allow-downloads', 'allow-presentation',
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
    expect(ev.applied).toHaveBeenCalledWith(1, 0, expect.any(Number), expect.any(Number));
    const [, , hash] = ev.applied.mock.calls[0] as [number, number, number, number];
    expect(hash).toBeGreaterThan(0);
  });

  /*
   * And says which document the hash is about.
   *
   * A snapshot restarts the numbering at zero, so "frame 0" is the same
   * sentence about every document this tab has ever held, and the server's
   * integrity check compares the client's hash for a frame against its own.
   * Answering about the previous document — which is what a client one round
   * trip behind does — reads landside as a diverged mirror, and costs the whole
   * document to repair something that was never broken.
   */
  it('says which document each acknowledgement is about', async () => {
    const { host, ev } = await mount();
    host.applySnapshot({ ...snapshot(), epoch: 4 });
    expect(ev.applied).toHaveBeenLastCalledWith(1, 0, expect.any(Number), 4);

    // And keeps saying it for the batches that follow, which carry no epoch of
    // their own: a mutation belongs to the document it was applied to.
    host.applyMutation(scrollOp(3, 0, 0), 1);
    const last = ev.applied.mock.calls.at(-1) as [number, number, number, number];
    expect(last[3]).toBe(4);

    // The next document is a different document, and is named as one.
    host.applySnapshot({ ...snapshot(), epoch: 5 });
    expect(ev.applied).toHaveBeenLastCalledWith(1, 0, expect.any(Number), 5);
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

  /**
   * Drags a mouse across an element, exactly as a browser reports one: the
   * pointer stream, and the mouse events that shadow it.
   *
   * jsdom lays nothing out, so every clientX here is a number the frame's
   * innerWidth turns into permille and nothing else depends on.
   */
  function dragAcross(host: MirrorHost, el: Element, from: number, to: number): void {
    const doc = host.frame.contentDocument!;
    const win = doc.defaultView!;
    const at = (type: string, x: number) => el.dispatchEvent(
      new win.PointerEvent(type, {
        bubbles: true, clientX: x, clientY: 40, button: 0, isPrimary: true,
        pointerType: 'mouse',
      }),
    );
    at('pointerdown', from);
    at('mousedown', from);
    at('pointermove', Math.round((from + to) / 2));
    at('pointermove', to);
    at('pointerup', to);
    at('mouseup', to);
    el.dispatchEvent(new win.MouseEvent('click', { bubbles: true, clientX: to, clientY: 40 }));
  }

  /**
   * The same gesture from a finger, as a phone actually reports it.
   *
   * Nothing here is a mouse event, because a browser sends none for a swipe:
   * measured under touch emulation, a finger dragged across a `touch-action:
   * none` box produces `pointerdown`, four `pointermove`s and a `pointerup`,
   * and not one `mousedown`, `mouseup` or `click`. A pan that needs any of
   * those cannot be made from the device this client is for.
   */
  function swipeAcross(host: MirrorHost, el: Element, from: number, to: number): void {
    const win = host.frame.contentDocument!.defaultView!;
    const at = (type: string, x: number) => el.dispatchEvent(
      new win.PointerEvent(type, {
        bubbles: true, clientX: x, clientY: 40, button: 0, isPrimary: true,
        pointerType: 'touch',
      }),
    );
    at('pointerdown', from);
    for (let i = 1; i <= 4; i++) at('pointermove', Math.round(from + ((to - from) * i) / 4));
    at('pointerup', to);
    // A touch pointer ceases to exist when the finger lifts, so the browser
    // sends these straight after the release. The drag must already be over.
    at('pointerout', to);
    at('pointerleave', to);
  }

  it('turns a drag across a canvas into a pan', async () => {
    const { host, ev } = await mount();
    const snap = snapshot();
    snap.strings.push('canvas');
    snap.nodes.push({
      id: 10, parent: 2, kind: NodeKind.Element,
      ref: snap.strings.indexOf('canvas'), attrs: [], flags: NodeFlags.Canvas,
    });
    host.applySnapshot(snap);
    const canvas = host.frame.contentDocument!.querySelector('canvas')!;

    dragAcross(host, canvas, 100, 400);

    const kinds = ev.input.mock.calls.map((c) => (c[1] as Record<string, unknown>).kind);
    expect(kinds).toContain('drag');
    // And not also a click: landside the two would be a pan followed by a
    // press wherever it ended.
    expect(kinds).not.toContain('click');

    const drag = (ev.input.mock.calls.find(
      (c) => (c[1] as Record<string, unknown>).kind === 'drag',
    )![1]) as Record<string, unknown>;
    expect(drag.node).toBe(10);
    const path = drag.path as number[];
    // Triplets, and the first sample is where the button went down — a pan
    // measured from halfway is a pan of the wrong distance. Two samples is the
    // floor rather than the expectation: intermediate moves go through the
    // same 12 ms throttle as a click's approach, and here every event fires in
    // the same millisecond. The press and the release always survive it, and
    // between them they are the displacement.
    expect(path.length % 3).toBe(0);
    expect(path.length / 3).toBeGreaterThanOrEqual(2);
    expect(path[0]).toBeLessThan(path[path.length - 3]);
  });

  it('does not let a drag swallow the click after next', async () => {
    const { host, ev } = await mount();
    const snap = snapshot();
    snap.strings.push('canvas');
    snap.nodes.push({
      id: 10, parent: 2, kind: NodeKind.Element,
      ref: snap.strings.indexOf('canvas'), attrs: [], flags: NodeFlags.Canvas,
    });
    host.applySnapshot(snap);
    const doc = host.frame.contentDocument!;
    const win = doc.defaultView!;
    const canvas = doc.querySelector('canvas')!;

    // A drag whose click never arrives: the pointer left the frame instead.
    canvas.dispatchEvent(new win.MouseEvent('mousedown', { bubbles: true, clientX: 100, button: 0 }));
    canvas.dispatchEvent(new win.MouseEvent('mousemove', { bubbles: true, clientX: 400 }));
    canvas.dispatchEvent(new win.MouseEvent('mouseleave', { bubbles: true, clientX: 400 }));

    ev.input.mockClear();
    const li = doc.querySelector('li')!;
    li.dispatchEvent(new win.MouseEvent('mousedown', { bubbles: true, clientX: 10, button: 0 }));
    li.dispatchEvent(new win.MouseEvent('click', { bubbles: true, clientX: 10 }));

    const kinds = ev.input.mock.calls.map((c) => (c[1] as Record<string, unknown>).kind);
    expect(kinds).toContain('click');
  });

  it('leaves a drag over ordinary content to the browser', async () => {
    // Press, move and release over text is the reader selecting it, which the
    // mirror does natively. Sending a pan as well would drag something
    // landside that the reader was only highlighting.
    const { host, ev } = await mount();
    host.applySnapshot(snapshot());
    const li = host.frame.contentDocument!.querySelector('li')!;

    dragAcross(host, li, 100, 400);

    const kinds = ev.input.mock.calls.map((c) => (c[1] as Record<string, unknown>).kind);
    expect(kinds).not.toContain('drag');
    expect(kinds).toContain('click');
  });

  it('does not send a pan for a press that never moved', async () => {
    const { host, ev } = await mount();
    const snap = snapshot();
    snap.strings.push('canvas');
    snap.nodes.push({
      id: 10, parent: 2, kind: NodeKind.Element,
      ref: snap.strings.indexOf('canvas'), attrs: [], flags: NodeFlags.Canvas,
    });
    host.applySnapshot(snap);
    const canvas = host.frame.contentDocument!.querySelector('canvas')!;

    dragAcross(host, canvas, 200, 200);

    const kinds = ev.input.mock.calls.map((c) => (c[1] as Record<string, unknown>).kind);
    // A map told it was dragged nowhere has been asked to do nothing, and the
    // click it really was would never arrive.
    expect(kinds).not.toContain('drag');
    expect(kinds).toContain('click');
  });

  /*
   * The reader taps a result inside a popover whose search field they were
   * typing in. Landside the press does the blur itself, in the page's order;
   * a blur of our own arrives a round trip earlier, and Google Chat answers it
   * by closing the dialog — so the click that follows names a node the page
   * has already destroyed, which is the "node 2219 not found landside" in the
   * capture this comes from.
   */
  it('does not blur a field ahead of the click that left it', async () => {
    const { host, ev } = await mount();
    host.applySnapshot(withSearchPopover(snapshot()));
    const doc = host.frame.contentDocument!;
    const win = doc.defaultView!;
    const field = doc.querySelectorAll('input')[0]!;
    const result = doc.querySelector('button')!;

    field.focus();
    ev.input.mockClear();

    // jsdom moves focus on focus()/blur() rather than as the press's default
    // action, so the order a browser produces is spelled out here.
    result.dispatchEvent(new win.MouseEvent('mousedown', { bubbles: true, button: 0 }));
    field.blur();
    result.dispatchEvent(new win.MouseEvent('mouseup', { bubbles: true, button: 0 }));
    result.dispatchEvent(new win.MouseEvent('click', { bubbles: true }));

    const kinds = ev.input.mock.calls.map((c) => (c[1] as Record<string, unknown>).kind);
    expect(kinds).toEqual(['click']);
  });

  it('blurs a field the reader left without pressing anything', async () => {
    // Focus going to the shell — the URL bar, a menu, a tab strip — produces
    // no gesture this side is going to send, so nothing else would ever tell
    // the page the reader has gone.
    const { host, ev } = await mount();
    host.applySnapshot(withSearchPopover(snapshot()));
    const field = host.frame.contentDocument!.querySelectorAll('input')[0]!;

    field.focus();
    ev.input.mockClear();
    field.blur();

    const sent = ev.input.mock.calls.map((c) => c[1] as Record<string, unknown>);
    expect(sent.map((p) => p.kind)).toEqual(['blur']);
    expect(sent[0].node).toBe(50);
  });

  it('sends a held blur when its gesture never reaches the page', async () => {
    // A press that ends on something the patcher cannot place sends nothing
    // landside, so the blur it was holding has nothing to ride on.
    const { host, ev } = await mount();
    host.applySnapshot(withSearchPopover(snapshot()));
    const doc = host.frame.contentDocument!;
    const win = doc.defaultView!;
    const field = doc.querySelectorAll('input')[0]!;
    const stray = doc.body.appendChild(doc.createElement('div'));

    field.focus();
    ev.input.mockClear();
    stray.dispatchEvent(new win.MouseEvent('mousedown', { bubbles: true, button: 0 }));
    field.blur();
    stray.dispatchEvent(new win.MouseEvent('mouseup', { bubbles: true, button: 0 }));
    stray.dispatchEvent(new win.MouseEvent('click', { bubbles: true }));

    const sent = ev.input.mock.calls.map((c) => c[1] as Record<string, unknown>);
    expect(sent.map((p) => p.kind)).toEqual(['blur']);
    expect(sent[0].node).toBe(50);
  });

  it('does not blur the field the reader pressed into', async () => {
    // Focus landing on the second field says everything the held blur was
    // waiting to say. Flushed after it, it would name the field the reader is
    // now typing in.
    const { host, ev } = await mount();
    host.applySnapshot(withSearchPopover(snapshot()));
    const doc = host.frame.contentDocument!;
    const win = doc.defaultView!;
    const [first, second] = Array.from(doc.querySelectorAll('input'));

    first.focus();
    ev.input.mockClear();
    second.dispatchEvent(new win.MouseEvent('mousedown', { bubbles: true, button: 0 }));
    first.blur();
    second.focus();
    second.dispatchEvent(new win.MouseEvent('mouseup', { bubbles: true, button: 0 }));
    second.dispatchEvent(new win.MouseEvent('click', { bubbles: true }));
    // And a gesture later, which is when a blur nothing resolved would surface.
    second.dispatchEvent(new win.MouseEvent('mousedown', { bubbles: true, button: 0 }));

    const kinds = ev.input.mock.calls.map((c) => (c[1] as Record<string, unknown>).kind);
    expect(kinds).not.toContain('blur');
    expect(kinds).toContain('focus');
    expect(kinds).toContain('click');
  });

  /*
   * The gesture from a finger, which is the device this client is for.
   *
   * It used to send nothing at all. beginDrag hung off `mousedown` and the
   * samples off `mousemove`, and a browser sends neither while a finger is
   * moving — a swipe is a pointer stream and nothing else, so a map could not
   * be panned from a phone.
   */
  it('turns a swipe across a canvas into a pan, with no mouse event anywhere', async () => {
    const { host, ev } = await mountWithCanvas();

    swipeAcross(host, host.frame.contentDocument!.querySelector('canvas')!, 100, 400);

    const drags = ev.input.mock.calls
      .map((c) => c[1] as Record<string, unknown>)
      .filter((p) => p.kind === 'drag');
    // Exactly one: the pointerleave that trails a finger's release must not
    // send the gesture a second time.
    expect(drags).toHaveLength(1);
    const drag = drags[0];
    expect(drag!.node).toBe(10);
    const path = drag!.path as number[];
    expect(path.length % 3).toBe(0);
    expect(path.length / 3).toBeGreaterThanOrEqual(2);
    expect(path[0]).toBeLessThan(path[path.length - 3]);
  });

  it('does not pan for a swipe the browser took for itself', async () => {
    // pointercancel is the browser saying it has claimed the gesture as a
    // scroll or a fling. What the reader did with it happened here; sending
    // the part that arrived would pan the page by however far the finger got.
    const { host, ev } = await mountWithCanvas();
    const doc = host.frame.contentDocument!;
    const win = doc.defaultView!;
    const canvas = doc.querySelector('canvas')!;
    const at = (type: string, x: number) => canvas.dispatchEvent(
      new win.PointerEvent(type, {
        bubbles: true, clientX: x, clientY: 40, button: 0, isPrimary: true,
        pointerType: 'touch',
      }),
    );

    at('pointerdown', 100);
    at('pointermove', 200);
    at('pointercancel', 200);
    at('pointerup', 400);

    const kinds = ev.input.mock.calls.map((c) => (c[1] as Record<string, unknown>).kind);
    expect(kinds).not.toContain('drag');
  });

  /*
   * The gesture census: which press-move-release the mirror claims as a drag.
   *
   * No page script runs in the mirror, so nothing here can ask the page what
   * it would do with a pointer — but the page already said. A grab cursor, a
   * role="slider", a touch-action: none are the affordances a drag widget
   * declares, and the mirror carries the attributes and stylesheets they are
   * declared in, so they are legible at the moment of the press. Everywhere
   * undeclared stays the reader's own text selection, which the tests above
   * pin.
   */

  /** Adds a plain div declaring itself a drag widget by one attribute — a
   *  grab cursor the way a map pane does, a role the way a slider does.
   *  jsdom's getComputedStyle reads inline style, so the cursor variant works
   *  without a stylesheet. */
  function withDragSurface(snap: Snapshot, name: string, value: string): Snapshot {
    const base = snap.strings.length;
    snap.strings.push('div', name, value, 'id', 'pad');
    snap.nodes.push({
      id: 70, parent: 1, kind: NodeKind.Element, ref: base,
      attrs: [base + 1, base + 2, base + 3, base + 4], flags: 0,
    });
    return snap;
  }

  /** Adds the two ends of a native HTML5 drag: a card that says
   *  draggable="true", and a plain zone for it to land on. */
  function withDraggableCard(snap: Snapshot): Snapshot {
    const base = snap.strings.length;
    snap.strings.push('div', 'draggable', 'true', 'id', 'card', 'zone');
    snap.nodes.push(
      {
        id: 80, parent: 1, kind: NodeKind.Element, ref: base,
        attrs: [base + 1, base + 2, base + 3, base + 4], flags: 0,
      },
      {
        id: 81, parent: 1, kind: NodeKind.Element, ref: base,
        attrs: [base + 3, base + 5], flags: 0,
      },
    );
    return snap;
  }

  /** The same gesture straight down the screen, for a surface that only
   *  claimed one axis: the browser keeps the other, and so must the mirror. */
  function dragDown(host: MirrorHost, el: Element, from: number, to: number): void {
    const win = host.frame.contentDocument!.defaultView!;
    const at = (type: string, y: number) => el.dispatchEvent(
      new win.PointerEvent(type, {
        bubbles: true, clientX: 150, clientY: y, button: 0, isPrimary: true,
        pointerType: 'mouse',
      }),
    );
    at('pointerdown', from);
    at('mousedown', from);
    at('pointermove', Math.round((from + to) / 2));
    at('pointermove', to);
    at('pointerup', to);
    at('mouseup', to);
    el.dispatchEvent(new win.MouseEvent('click', { bubbles: true, clientX: 150, clientY: to }));
  }

  it('claims the swipe a carousel kept, and leaves the scroll it gave away', async () => {
    // `touch-action: pan-y` is what every carousel says: the browser may
    // pan vertically, the page keeps the horizontal gesture. Reading only
    // `none` claimed neither (P-140).
    const { host, ev } = await mount();
    host.applySnapshot(withDragSurface(snapshot(), 'style', 'touch-action: pan-y'));
    const pad = host.frame.contentDocument!.getElementById('pad')!;

    dragAcross(host, pad, 100, 400);
    const across = ev.input.mock.calls.map((c) => c[1] as Record<string, unknown>);
    expect(across.filter((p) => p.kind === 'drag')).toHaveLength(1);

    ev.input.mockClear();
    dragDown(host, pad, 40, 300);
    const down = ev.input.mock.calls.map((c) => c[1] as Record<string, unknown>);
    // The browser's axis: the reader is scrolling, and a pan sent landside
    // would move a page they were only reading past.
    expect(down.filter((p) => p.kind === 'drag')).toHaveLength(0);
    expect(down.map((p) => p.kind)).toContain('click');
  });

  it('leaves a manipulation surface to the browser entirely', async () => {
    // `manipulation` refuses double-tap zoom and leaves panning alone: a
    // page asking for a faster tap, not for the gesture.
    const { host, ev } = await mount();
    host.applySnapshot(withDragSurface(snapshot(), 'style', 'touch-action: manipulation'));

    dragAcross(host, host.frame.contentDocument!.getElementById('pad')!, 100, 400);

    const sent = ev.input.mock.calls.map((c) => c[1] as Record<string, unknown>);
    expect(sent.filter((p) => p.kind === 'drag')).toHaveLength(0);
  });

  it('claims a drag on an element wearing a grab cursor', async () => {
    const { host, ev } = await mount();
    host.applySnapshot(withDragSurface(snapshot(), 'style', 'cursor: grab'));

    dragAcross(host, host.frame.contentDocument!.getElementById('pad')!, 100, 400);

    const sent = ev.input.mock.calls.map((c) => c[1] as Record<string, unknown>);
    // Exactly one drag and no click: landside the pair would be a pan
    // followed by a press wherever it ended.
    expect(sent.filter((p) => p.kind === 'drag')).toHaveLength(1);
    expect(sent.map((p) => p.kind)).not.toContain('click');
    const drag = sent.find((p) => p.kind === 'drag')!;
    expect(drag.node).toBe(70);
    // A mouse is the wire's zero, and zero is omitted.
    expect(drag.pt).toBeUndefined();
  });

  it('claims a drag on a slider, which declares itself in its role', async () => {
    const { host, ev } = await mount();
    host.applySnapshot(withDragSurface(snapshot(), 'role', 'slider'));

    dragAcross(host, host.frame.contentDocument!.getElementById('pad')!, 100, 400);

    const sent = ev.input.mock.calls.map((c) => c[1] as Record<string, unknown>);
    expect(sent.filter((p) => p.kind === 'drag')).toHaveLength(1);
    expect(sent.map((p) => p.kind)).not.toContain('click');
    expect(sent.find((p) => p.kind === 'drag')!.node).toBe(70);
  });

  it('keeps a drag through a boundary crossing inside the frame', async () => {
    // The capture listener hears pointerleave for every element boundary the
    // pointer crosses mid-drag — over a list, that is every row. Only really
    // leaving the frame ends the gesture, and the difference is the
    // relatedTarget: a crossing names the element being entered, a departure
    // from the frame names nothing. See the pointerleave wiring in wireInput.
    const { host, ev } = await mount();
    host.applySnapshot(withDragSurface(snapshot(), 'style', 'cursor: grab'));
    const doc = host.frame.contentDocument!;
    const win = doc.defaultView!;
    const pad = doc.getElementById('pad')!;
    const at = (type: string, x: number, init: PointerEventInit = {}) => pad.dispatchEvent(
      new win.PointerEvent(type, {
        bubbles: true, clientX: x, clientY: 40, button: 0, isPrimary: true,
        pointerType: 'mouse', ...init,
      }),
    );

    at('pointerdown', 100);
    at('pointermove', 150);
    // The pointer crosses into a sibling; pointerleave does not bubble, but
    // the document's capture listener hears it all the same.
    at('pointerleave', 250, { bubbles: false, relatedTarget: doc.querySelector('li') });
    at('pointermove', 300);
    at('pointerup', 400);

    const drags = ev.input.mock.calls
      .map((c) => c[1] as Record<string, unknown>)
      .filter((p) => p.kind === 'drag');
    expect(drags).toHaveLength(1);
    const path = drags[0].path as number[];
    // The path ends where the button came up. A drag cut off at the crossing
    // would have been sent from the leave instead, and end at 250's permille.
    expect(path[path.length - 3]).toBe(Math.round((400 / win.innerWidth) * 1000));
  });

  it('says a drag came from a finger, so landside replays it as one', async () => {
    const { host, ev } = await mount();
    host.applySnapshot(withDragSurface(snapshot(), 'style', 'cursor: grab'));

    swipeAcross(host, host.frame.contentDocument!.getElementById('pad')!, 100, 400);

    const drags = ev.input.mock.calls
      .map((c) => c[1] as Record<string, unknown>)
      .filter((p) => p.kind === 'drag');
    expect(drags).toHaveLength(1);
    expect(drags[0].node).toBe(70);
    // The wire's name for a touch pointer; a slider dragged with a mouse
    // gesture on a touch page is how landside misses the widget.
    expect(drags[0].pt).toBe(1);
  });

  it('carries the browser\'s own drag-and-drop as a single drag frame', async () => {
    const { host, ev } = await mount();
    host.applySnapshot(withDraggableCard(snapshot()));
    const doc = host.frame.contentDocument!;
    const win = doc.defaultView!;
    const card = doc.getElementById('card')!;
    const zone = doc.getElementById('zone')!;
    // jsdom has no DragEvent; the listeners read only the MouseEvent fields,
    // and the dataTransfer a real one carries is landside state the wire
    // never needs.
    const dnd = (target: Element, type: string, x: number) => {
      const event = new win.MouseEvent(type, {
        bubbles: true, cancelable: true, clientX: x, clientY: 40,
      });
      target.dispatchEvent(event);
      return event;
    };

    dnd(card, 'dragstart', 100);
    // Without the preventDefault on dragover the browser never delivers the
    // drop at all: cancelling it is what keeps the frame a legal drop target.
    expect(dnd(zone, 'dragover', 250).defaultPrevented).toBe(true);
    dnd(zone, 'drop', 400);

    const sent = ev.input.mock.calls.map((c) => c[1] as Record<string, unknown>);
    expect(sent.filter((p) => p.kind === 'drag')).toHaveLength(1);
    expect(sent.map((p) => p.kind)).not.toContain('click');
    expect(sent.find((p) => p.kind === 'drag')!.node).toBe(80);

    // A drag let go of nowhere — an escape, a release over nothing — ends in
    // dragend with no drop, and nothing crossed the page for the wire to say.
    dnd(card, 'dragstart', 100);
    dnd(card, 'dragend', 100);
    expect(ev.input.mock.calls.map((c) => c[1] as Record<string, unknown>)
      .filter((p) => p.kind === 'drag')).toHaveLength(1);
  });

  it('names the element a drag finished over, and where in its box', async () => {
    // The path already says how the gesture moved; node2 says what it landed
    // on, which survives the two halves laying the page out a few pixels
    // apart.
    const { host, ev } = await mount();
    host.applySnapshot(withDraggableCard(withDragSurface(snapshot(), 'style', 'cursor: grab')));
    const doc = host.frame.contentDocument!;
    const zone = doc.getElementById('zone')!;
    // jsdom neither lays out nor hit-tests, so both are told directly: the
    // release lands on the zone, whose box sits at 300..500 across the top.
    doc.elementFromPoint = () => zone;
    Object.defineProperty(zone, 'getBoundingClientRect', {
      value: () => ({ left: 300, top: 0, width: 200, height: 100 }),
      configurable: true,
    });

    dragAcross(host, doc.getElementById('pad')!, 100, 400);

    const drag = ev.input.mock.calls
      .map((c) => c[1] as Record<string, unknown>)
      .find((p) => p.kind === 'drag')!;
    expect(drag.node).toBe(70);
    expect(drag.node2).toBe(81);
    // Halfway across the zone, at the height the gesture travelled on — in
    // permille of the box, because the landside box is a different size.
    expect(drag.point2).toEqual([500, 400]);
  });

  /*
   * The wheel and the dwell: the census's two slower gestures. A wheel
   * turned over a claimed surface is the widget's zoom and crosses the wire;
   * anywhere else it stays the reader's own scroll. A mouse coming to rest
   * is the gesture every hover menu is built on, and one rest is one frame.
   */

  /** One tick of the wheel, dispatched the way a browser reports it. */
  function wheelOn(el: Element, deltaY: number, x = 150, y = 40): WheelEvent {
    const win = el.ownerDocument.defaultView!;
    const ev = new win.WheelEvent('wheel', {
      bubbles: true, cancelable: true, deltaY, clientX: x, clientY: y,
    });
    el.dispatchEvent(ev);
    return ev;
  }

  /** The pointer passing over an element, from a mouse unless told otherwise. */
  function pointerOver(el: Element, x: number, y: number, pointerType = 'mouse'): void {
    const win = el.ownerDocument.defaultView!;
    el.dispatchEvent(new win.PointerEvent('pointermove', {
      bubbles: true, clientX: x, clientY: y, isPrimary: true, pointerType,
    }));
  }

  it('coalesces a wheel over a claimed surface into one frame', async () => {
    vi.useFakeTimers();
    try {
      const { host, ev } = await mount();
      host.applySnapshot(withDragSurface(snapshot(), 'style', 'cursor: grab'));
      const pad = host.frame.contentDocument!.getElementById('pad')!;

      // Two ticks of a flick, both inside one flush window.
      const first = wheelOn(pad, -120);
      const second = wheelOn(pad, -120);
      // The widget consumes the wheel, so the mirror must not also scroll.
      expect(first.defaultPrevented).toBe(true);
      expect(second.defaultPrevented).toBe(true);
      // And nothing crosses until the beat is over.
      expect(ev.input).not.toHaveBeenCalled();

      await vi.advanceTimersByTimeAsync(150);

      const sent = ev.input.mock.calls.map((c) => c[1] as Record<string, unknown>);
      expect(sent).toHaveLength(1);
      expect(sent[0].kind).toBe('wheel');
      expect(sent[0].node).toBe(70);
      expect(sent[0].y).toBe(-240);
    } finally {
      vi.useRealTimers();
    }
  });

  it('leaves a wheel over ordinary content to the browser', async () => {
    // The mirror scrolls natively and reports where it got to; a wheel over
    // text is that scroll, and taking it would freeze the page under the
    // reader's fingers.
    vi.useFakeTimers();
    try {
      const { host, ev } = await mount();
      host.applySnapshot(snapshot());
      const li = host.frame.contentDocument!.querySelector('li')!;

      const tick = wheelOn(li, -120);
      await vi.advanceTimersByTimeAsync(150);

      expect(tick.defaultPrevented).toBe(false);
      expect(ev.input).not.toHaveBeenCalled();
    } finally {
      vi.useRealTimers();
    }
  });

  it('leaves the vertical wheel a one-axis widget gave the browser', async () => {
    // The other half of P-140: claiming a `pan-y` carousel for drags must
    // not also claim the wheel over it, or the page stops scrolling under
    // the reader entirely.
    vi.useFakeTimers();
    try {
      const { host, ev } = await mount();
      host.applySnapshot(withDragSurface(snapshot(), 'style', 'touch-action: pan-y'));
      const pad = host.frame.contentDocument!.getElementById('pad')!;
      const win = pad.ownerDocument.defaultView!;

      const down = wheelOn(pad, -120);
      await vi.advanceTimersByTimeAsync(150);
      expect(down.defaultPrevented).toBe(false);
      expect(ev.input).not.toHaveBeenCalled();

      // The axis the page kept is the page's, wheel included: a horizontal
      // wheel over a carousel is the reader asking it to move.
      const across = new win.WheelEvent('wheel', {
        bubbles: true, cancelable: true, deltaX: -120, clientX: 150, clientY: 40,
      });
      pad.dispatchEvent(across);
      await vi.advanceTimersByTimeAsync(150);
      expect(across.defaultPrevented).toBe(true);
      const sent = ev.input.mock.calls.map((c) => c[1] as Record<string, unknown>);
      expect(sent.filter((p) => p.kind === 'wheel')).toHaveLength(1);
    } finally {
      vi.useRealTimers();
    }
  });

  it('sends one hover for a mouse at rest, and nothing for staying there', async () => {
    vi.useFakeTimers();
    try {
      const { host, ev } = await mount();
      host.applySnapshot(withDragSurface(snapshot(), 'style', 'cursor: grab'));
      const pad = host.frame.contentDocument!.getElementById('pad')!;

      pointerOver(pad, 150, 40);
      await vi.advanceTimersByTimeAsync(400);

      let hovers = ev.input.mock.calls
        .map((c) => c[1] as Record<string, unknown>)
        .filter((p) => p.kind === 'hover');
      expect(hovers).toHaveLength(1);
      expect(hovers[0].node).toBe(70);

      // Drifting inside the slop is stillness, not a new rest...
      await vi.advanceTimersByTimeAsync(200);
      pointerOver(pad, 152, 42);
      pointerOver(pad, 149, 39);
      await vi.advanceTimersByTimeAsync(400);
      // ...and a fresh rest on the element already hovered has nothing to add.
      pointerOver(pad, 300, 40);
      await vi.advanceTimersByTimeAsync(400);

      hovers = ev.input.mock.calls
        .map((c) => c[1] as Record<string, unknown>)
        .filter((p) => p.kind === 'hover');
      expect(hovers).toHaveLength(1);
    } finally {
      vi.useRealTimers();
    }
  });

  it('never dwells a finger, which is nowhere between touches', async () => {
    vi.useFakeTimers();
    try {
      const { host, ev } = await mount();
      host.applySnapshot(withDragSurface(snapshot(), 'style', 'cursor: grab'));
      const pad = host.frame.contentDocument!.getElementById('pad')!;

      pointerOver(pad, 150, 40, 'touch');
      await vi.advanceTimersByTimeAsync(400);

      expect(ev.input).not.toHaveBeenCalled();
    } finally {
      vi.useRealTimers();
    }
  });

  it('does not hover the element a click already parked the pointer on', async () => {
    // The click's replay parks the landside pointer where it landed, so the
    // page's own hover machinery has already run there. A dwell saying so
    // again would spend a round trip repeating it.
    vi.useFakeTimers();
    try {
      const { host, ev } = await mount();
      host.applySnapshot(snapshot());
      const doc = host.frame.contentDocument!;
      const li = doc.querySelector('li')!;

      li.dispatchEvent(new doc.defaultView!.MouseEvent('click', { bubbles: true }));
      ev.input.mockClear();

      pointerOver(li, 150, 40);
      await vi.advanceTimersByTimeAsync(400);

      const kinds = ev.input.mock.calls.map((c) => (c[1] as Record<string, unknown>).kind);
      expect(kinds).not.toContain('hover');
    } finally {
      vi.useRealTimers();
    }
  });

  it('reports the press a finger actually made, not the gap between two compat events', async () => {
    /*
     * A phone fires mousemove, mousedown, mouseup and click after the finger
     * has come up, all stamped with the same millisecond. Measuring the press
     * between two of those reports 1 ms however long the reader held — every
     * click in the Google Chat capture does — and the server prefers a
     * reported hold to its own plausible one, so it replays a press no hand
     * could make. The press is pointerdown to click.
     */
    const { host, ev } = await mount();
    host.applySnapshot(snapshot());
    const doc = host.frame.contentDocument!;
    const win = doc.defaultView!;
    const li = doc.querySelector('li')!;
    const now = vi.spyOn(performance, 'now');

    now.mockReturnValue(1000);
    li.dispatchEvent(new win.PointerEvent('pointerdown', {
      bubbles: true, isPrimary: true, button: 0, pointerType: 'touch',
    }));
    // The whole compat burst, ninety-five milliseconds later and all at once.
    now.mockReturnValue(1095);
    li.dispatchEvent(new win.PointerEvent('pointerup', {
      bubbles: true, isPrimary: true, button: 0, pointerType: 'touch',
    }));
    li.dispatchEvent(new win.MouseEvent('mousedown', { bubbles: true }));
    li.dispatchEvent(new win.MouseEvent('mouseup', { bubbles: true }));
    li.dispatchEvent(new win.MouseEvent('click', { bubbles: true }));
    now.mockRestore();

    const click = ev.input.mock.calls
      .map((c) => c[1] as Record<string, unknown>)
      .find((p) => p.kind === 'click');
    expect(click).toBeDefined();
    expect(click!.hold).toBe(95);
  });

  it('still holds a blur behind a tap, which a phone brackets in mouse events', async () => {
    // The focus change is a default action of the compat mousedown, so the
    // window in which a focusout belongs to a press is that event and its
    // mouseup — on a finger exactly as under a mouse. See heldBlur.
    const { host, ev } = await mount();
    host.applySnapshot(withSearchPopover(snapshot()));
    const doc = host.frame.contentDocument!;
    const win = doc.defaultView!;
    const field = doc.querySelectorAll('input')[0]!;
    const result = doc.querySelector('button')!;

    field.focus();
    ev.input.mockClear();
    result.dispatchEvent(new win.PointerEvent('pointerdown', {
      bubbles: true, isPrimary: true, button: 0, pointerType: 'touch',
    }));
    result.dispatchEvent(new win.PointerEvent('pointerup', {
      bubbles: true, isPrimary: true, button: 0, pointerType: 'touch',
    }));
    result.dispatchEvent(new win.MouseEvent('mousedown', { bubbles: true }));
    field.blur();
    result.dispatchEvent(new win.MouseEvent('mouseup', { bubbles: true }));
    result.dispatchEvent(new win.MouseEvent('click', { bubbles: true }));

    const kinds = ev.input.mock.calls.map((c) => (c[1] as Record<string, unknown>).kind);
    expect(kinds).toEqual(['click']);
  });

  /*
   * A webfont the server cannot deliver must not leave a face behind.
   *
   * Everywhere else an unresolved reference becomes the transparent pixel, and
   * for a background that is right. For an `@font-face` it is the worst answer
   * available: a face whose src loads is a face, a 1x1 GIF loads, and font
   * matching then prefers it to the faces that work. The Google Chat capture
   * had two faces for `Google Symbols` — a subset that arrived and the 4.9 MB
   * variable font the transcoder refuses at its 1 MB cap — and the pixel that
   * replaced the second blanked every icon drawn from the family.
   */
  it('withholds a font face whose file is not coming, rather than pixelling it', async () => {
    const { host } = await mount();
    const snap = snapshot();
    snap.css = [
      '@font-face{font-family:"Icons";font-weight:400;src:url(skyhook://img/aaaa) format("woff2");}',
      '@font-face{font-family:"Icons";font-weight:100 700;src:url(skyhook://img/bbbb) format("woff2");}',
      '.hero{background-image:url(skyhook://img/cccc)}',
    ];
    host.applySnapshot(snap);
    host.setImageMeta({
      node: 0, hash: 'bbbb', w: 0, h: 0, blur: '', mime: '',
      bytes: 0, priority: 0, alt: '', box: [], missing: true,
    });

    const css = host.frame.contentDocument!.querySelector('style[data-skyhook-css]')!.textContent!;
    // The face that cannot be drawn is not written at all.
    expect(css).not.toContain('100 700');
    // A background is still pixelled: there the placeholder is the right
    // answer, and the box keeps its own colour.
    expect(css).toContain('.hero');
    expect(css).toContain('data:image/gif;base64,');
    // And no face anywhere is left pointing at one.
    for (const face of css.match(/@font-face[^}]*\}/g) ?? []) {
      expect(face).not.toContain('data:image/gif;base64,');
    }
  });

  it('writes a font face on the pass after its bytes land', async () => {
    // A font that is merely late is withheld, not pixelled, so the sheet is
    // correct the moment the bytes arrive rather than poisoned until a resync.
    stubCache({ [imageCacheKey('aaaa')]: new Uint8Array([1, 2, 3]) });
    const { host } = await mount();
    const snap = snapshot();
    snap.css = ['@font-face{font-family:"Icons";src:url(skyhook://img/aaaa) format("woff2");}'];
    host.applySnapshot(snap);
    const styleOf = () =>
      host.frame.contentDocument!.querySelector('style[data-skyhook-css]')!.textContent!;
    expect(styleOf()).not.toContain('@font-face');

    host.imageArrived('aaaa');
    await vi.waitFor(() => expect(styleOf()).toContain('@font-face'));
    expect(styleOf()).toContain('blob:');
  });

  /*
   * The images a stylesheet names have to be asked for, and after a navigation
   * they were not.
   *
   * Applying a snapshot renders its stylesheet, and rendering it is what puts
   * the images it names on the asking list. `releaseBlobs` ran afterwards and
   * cleared that list — along with the record that anything had been asked —
   * so the requests the new page had just made were thrown away. Any later CSS
   * delta re-rendered the sheet and quietly repaired it, which is why a page
   * that streams its CSS never showed this; a page whose CSS arrives whole in
   * the snapshot and never changes again has nothing to render it again, and
   * every background, icon and webfont it names simply never came.
   */
  it('asks for the images a new page\'s stylesheet names', async () => {
    const { host, ev } = await mount();
    const snap = snapshot();
    snap.url = 'https://example.test/somewhere-else';
    snap.css = ['.hero{background-image:url(skyhook://img/facade)}'];
    host.applySnapshot(snap);

    await vi.waitFor(() =>
      expect(ev.wantImages).toHaveBeenCalledWith(1, expect.arrayContaining(['facade'])));
    expect(host.freeze().state.pendingCSSImages).toContain('facade');
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
      bytes: 100, priority: 0, alt: '', box: [], missing: false,
    });
    host.applySnapshot(snap);

    expect(ev.wantImages).toHaveBeenCalledWith(1, ['deadbeef']);
    // The wire form never reaches the DOM: it would be an unfetchable scheme.
    const img = host.frame.contentDocument!.querySelector('img')!;
    expect(img.getAttribute('src')?.startsWith('data:image/gif')).toBe(true);
    // Nor does /img/<hash>: the frame is sandboxed, so it is not a service
    // worker client, and that URL would go to the network — which is the one
    // thing the frame must never do.
    expect(img.getAttribute('src')).not.toContain('/img/');
    // The hash rides on the element instead, so metadata and bytes arriving
    // later can still find it.
    expect(img.dataset.skyhookImg).toBe('deadbeef');
  });

  /*
   * An asset is asked for once per hash, which leaves a dropped answer with no
   * second chance. Media is expendable on a full queue by design, and the
   * server no longer re-sends bytes it has already sent, so the only thing
   * standing between a dropped push and a picture that never arrives is this:
   * a resync rebuilds the document, and whatever is still empty is asked for
   * again.
   */
  it('asks again, after a resync, for an image whose bytes never came', async () => {
    const { host, ev } = await mount();
    const snap = imageSnapshot();
    host.applySnapshot(snap);
    expect(ev.wantImages).toHaveBeenCalledWith(1, ['deadbeef']);

    ev.wantImages.mockClear();
    // The same document again, which is what a resync is.
    host.applySnapshot(imageSnapshot());
    expect(ev.wantImages).toHaveBeenCalledWith(1, ['deadbeef']);
  });

  it('does not ask again for an image it already has', async () => {
    stubCache({ [imageCacheKey('deadbeef')]: 'pretend pixels' });
    const { host, ev } = await mount();
    host.applySnapshot(imageSnapshot());
    // The bytes land, so nothing is waiting for them any more. Minting the
    // blob is asynchronous — the cache read is — so this waits for the element
    // to be showing it rather than for a fixed number of microtasks.
    host.imageArrived('deadbeef');
    const img = host.frame.contentDocument!.querySelector('img')!;
    await vi.waitFor(() => expect(img.getAttribute('src')).toBe('blob:mirror/c0ffee'));

    ev.wantImages.mockClear();
    host.applySnapshot(imageSnapshot());
    expect(ev.wantImages).not.toHaveBeenCalled();
  });

  /** A snapshot with one above-the-fold image in it. */
  function imageSnapshot(): Snapshot {
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
      bytes: 100, priority: 0, alt: '', box: [], missing: false,
    });
    return snap;
  }

  /** Mounts a snapshot holding one image, and returns the element. */
  async function withImage(): Promise<{ host: MirrorHost; img: HTMLImageElement }> {
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
    return { host, img: host.frame.contentDocument!.querySelector('img')! };
  }

  /**
   * Stubs Cache Storage with the given entries, and names any blob minted from
   * one. Only the two statics are replaced — swapping the whole URL global
   * would leave link resolution, which is `new URL(href, base)`, unable to
   * construct anything.
   */
  function stubCache(entries: Record<string, BodyInit>): { match: ReturnType<typeof vi.fn> } {
    const match = vi.fn(async (key: string) => {
      const body = entries[key];
      return body === undefined ? undefined : new Response(body);
    });
    vi.stubGlobal('caches', { open: async () => ({ match }) });
    vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:mirror/c0ffee');
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {});
    return { match };
  }

  it('stops waiting for an image the server says is not coming', async () => {
    // The client asks for a hash exactly once, so before there was anything to
    // say this, a landside failure left the element holding a transparent
    // pixel until the tab was closed.
    const { host, img } = await withImage();
    expect(img.getAttribute('src')?.startsWith('data:image/gif')).toBe(true);

    host.setImageMeta({
      node: 10, hash: 'c0ffee', w: 0, h: 0, blur: '', mime: '',
      bytes: 0, priority: 0, alt: 'a chart of the results', box: [], missing: true,
    });

    // No src at all is what makes an <img> draw its alt text, which is the
    // thing the page's author wrote for exactly this case.
    expect(img.hasAttribute('src')).toBe(false);
    expect(img.getAttribute('alt')).toBe('a chart of the results');
    expect(img.dataset.skyhookMissing).toBe('1');
    const state = host.freeze().state as Record<string, unknown>;
    expect(state.pendingImages).not.toContain('c0ffee');
    // And a capture can now tell "given up on" from "still waiting", which is
    // the difference between a landside failure and a slow link.
    expect(state.missingImages).toContain('c0ffee');
  });

  it('keeps a blurhash rather than replacing it with a broken-image marker', async () => {
    // An element already wearing an approximation of the picture is better off
    // keeping it than being told in a small grey icon that the picture failed.
    const { host, img } = await withImage();
    host.setImageMeta({
      node: 10, hash: 'c0ffee', w: 8, h: 8, blur: 'LEHV6nWB2yk8', mime: 'image/png',
      bytes: 40, priority: 0, alt: '', box: [], missing: false,
    });
    await vi.waitFor(() => expect(img.dataset.skyhookBlur).toBe('1'));

    host.setImageMeta({
      node: 10, hash: 'c0ffee', w: 0, h: 0, blur: '', mime: '',
      bytes: 0, priority: 0, alt: '', box: [], missing: true,
    });
    expect(img.dataset.skyhookMissing).toBe('1');
    expect(img.getAttribute('src')?.startsWith('data:image/gif')).toBe(true);
  });

  it('does not ask again for a hash it has been told is not coming', async () => {
    const { host, ev } = await mount();
    const snap = snapshot();
    snap.strings.push('img', 'skyhook://img/c0ffee', 'src');
    snap.nodes.push({
      id: 10, parent: 2, kind: NodeKind.Element,
      ref: snap.strings.indexOf('img'),
      attrs: [snap.strings.indexOf('src'), snap.strings.indexOf('skyhook://img/c0ffee')],
      flags: 2,
    });
    host.applySnapshot(snap);
    ev.wantImages.mockClear();

    host.setImageMeta({
      node: 10, hash: 'c0ffee', w: 0, h: 0, blur: '', mime: '',
      bytes: 0, priority: 0, alt: '', box: [], missing: true,
    });
    // Re-announcing the same element must not put it back on a queue that has
    // already been told there is nothing in it.
    host.applySnapshot(snap);
    expect(ev.wantImages).not.toHaveBeenCalledWith(1, expect.arrayContaining(['c0ffee']));
    expect((host.freeze().state as Record<string, unknown>).pendingImages).not.toContain('c0ffee');
  });

  it('shows an image out of a blob the shell minted for it', async () => {
    // The bytes reach the frame as a blob URL because the shell can read them
    // and the sandboxed frame cannot.
    const { host, img } = await withImage();
    const { match } = stubCache({ [imageCacheKey('c0ffee')]: new Uint8Array([1, 2, 3]) });

    host.imageArrived('c0ffee');
    await vi.waitFor(() => expect(img.getAttribute('src')).toBe('blob:mirror/c0ffee'));
    expect(match).toHaveBeenCalledWith(imageCacheKey('c0ffee'));
  });

  // The bytes are read out of Cache Storage rather than fetched from the URL
  // the service worker serves them on. The shell is only a client of that
  // worker once it has been claimed, and until then the same fetch reaches the
  // network, where the server answers an unknown path with the app shell —
  // leaving the element on a blob of index.html for the rest of the session.
  it('never goes to the network for an image', async () => {
    const { host, img } = await withImage();
    const fetchMock = vi.fn(async () => new Response('<!doctype html><title>app</title>', {
      headers: { 'content-type': 'text/html' },
    }));
    vi.stubGlobal('fetch', fetchMock);
    stubCache({}); // nothing cached yet

    host.imageArrived('c0ffee');
    await new Promise((r) => setTimeout(r, 0));
    expect(fetchMock).not.toHaveBeenCalled();
    // And with nothing to show, the placeholder stands.
    expect(img.getAttribute('src')?.startsWith('data:image/gif')).toBe(true);
  });

  /**
   * Mounts a snapshot holding a canvas, which is the one element whose content
   * no part of a snapshot describes: nothing here says what was painted, and
   * nothing plane-side will ever paint it.
   */
  async function withCanvas(): Promise<{ host: MirrorHost; canvas: HTMLElement }> {
    const { host } = await mount();
    const snap = snapshot();
    snap.strings.push('canvas');
    snap.nodes.push({
      id: 10, parent: 2, kind: NodeKind.Element,
      ref: snap.strings.indexOf('canvas'), attrs: [], flags: NodeFlags.Canvas,
    });
    host.applySnapshot(snap);
    return { host, canvas: host.frame.contentDocument!.querySelector('canvas')! };
  }

  function shotMeta(hash: string, box: number[]): ImageMeta {
    return {
      node: 10, hash, w: 200, h: 120, blur: '', mime: 'image/png',
      bytes: 400, priority: 0, alt: '', box, missing: false,
    };
  }

  it('paints a region shot onto the canvas it was taken from', async () => {
    const { host, canvas } = await withCanvas();
    stubCache({ [imageCacheKey('c0ffee')]: new Uint8Array([1, 2, 3]) });

    host.setImageMeta(shotMeta('c0ffee', [0, 20, 200, 100]));
    await vi.waitFor(() => expect(canvas.style.backgroundImage).toContain('blob:mirror/c0ffee'));
    // The rectangle goes back where it came from. A canvas photographed as the
    // part of it that was on screen must not be stretched over the whole box.
    expect(canvas.style.backgroundPosition).toBe('0px 20px');
    expect(canvas.style.backgroundSize).toBe('200px 100px');
    expect(canvas.style.backgroundOrigin).toBe('border-box');
    expect(canvas.style.backgroundRepeat).toBe('no-repeat');
  });

  it('does not repaint a canvas with a frame it has already moved past', async () => {
    const { host, canvas } = await withCanvas();
    stubCache({
      [imageCacheKey('older')]: new Uint8Array([1]),
      [imageCacheKey('newer')]: new Uint8Array([2]),
    });
    // Named after the bytes they were minted from, so the assertion can tell
    // which frame the canvas ended up wearing.
    let minted = 0;
    vi.spyOn(URL, 'createObjectURL').mockImplementation(
      () => ['blob:mirror/older', 'blob:mirror/newer'][minted++] ?? 'blob:mirror/extra',
    );

    // Two shots in flight, the second superseding the first; the first's bytes
    // arrive last, which over a lossy link is an ordinary Tuesday.
    host.setImageMeta(shotMeta('older', [0, 0, 200, 120]));
    await vi.waitFor(() => expect(canvas.style.backgroundImage).toContain('blob:mirror/older'));
    host.setImageMeta(shotMeta('newer', [0, 0, 200, 120]));
    await vi.waitFor(() => expect(canvas.style.backgroundImage).toContain('blob:mirror/newer'));

    host.imageArrived('older');
    await new Promise((r) => setTimeout(r, 0));
    expect(canvas.style.backgroundImage).toContain('blob:mirror/newer');
  });

  it('asks for the bytes of a shot that was never pushed', async () => {
    const { host, ev } = await mount();
    const snap = snapshot();
    snap.strings.push('canvas');
    snap.nodes.push({
      id: 10, parent: 2, kind: NodeKind.Element,
      ref: snap.strings.indexOf('canvas'), attrs: [], flags: NodeFlags.Canvas,
    });
    host.applySnapshot(snap);
    stubCache({});

    host.setImageMeta(shotMeta('c0ffee', [0, 0, 200, 120]));
    await vi.waitFor(() => expect(ev.wantImages).toHaveBeenCalledWith(1, ['c0ffee']));
  });

  it('keeps looking after a miss, rather than freezing on it', async () => {
    // Bytes that have not crossed the link yet are simply absent from the
    // cache; caching that absence would mean the image never appeared.
    const { host, img } = await withImage();
    stubCache({});
    host.imageArrived('c0ffee');
    await new Promise((r) => setTimeout(r, 0));

    stubCache({ [imageCacheKey('c0ffee')]: new Uint8Array([1, 2, 3]) });
    host.imageArrived('c0ffee');
    await vi.waitFor(() => expect(img.getAttribute('src')).toBe('blob:mirror/c0ffee'));
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
    // A search submitted is a page on its way, even though where it lands is
    // the server's to work out.
    expect(ev.navigating).toHaveBeenCalledWith(1);
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
      bytes: 900, priority: 0, alt: '', box: [], missing: false,
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

  it('opens a link in a new tab on middle click', async () => {
    const { host, ev } = await mount();
    host.applySnapshot(withLink(snapshot()));
    const doc = host.frame.contentDocument!;
    const view = doc.defaultView!;
    const aux = new view.MouseEvent('auxclick', { bubbles: true, cancelable: true, button: 1 });
    doc.querySelector('a')!.dispatchEvent(aux);

    expect(ev.openLink).toHaveBeenCalledWith(1, 'https://example.test/next');
    // Nothing goes landside: a new tab is a tab this side asks for, not a click
    // replayed into a page that would open a tab the client has no handle on.
    expect(ev.input).not.toHaveBeenCalled();
    expect(aux.defaultPrevented).toBe(true);
  });

  it('ignores a middle click that is not on a link', async () => {
    const { host, ev } = await mount();
    host.applySnapshot(snapshot());
    const doc = host.frame.contentDocument!;
    doc.querySelector('li')!.dispatchEvent(
      new doc.defaultView!.MouseEvent('auxclick', { bubbles: true, button: 1 }),
    );
    expect(ev.openLink).not.toHaveBeenCalled();
  });

  it('treats ctrl-click on a link as open-in-new-tab', async () => {
    const { host, ev } = await mount();
    host.applySnapshot(withLink(snapshot()));
    const doc = host.frame.contentDocument!;
    doc.querySelector('a')!.dispatchEvent(
      new doc.defaultView!.MouseEvent('click', { bubbles: true, ctrlKey: true }),
    );
    expect(ev.openLink).toHaveBeenCalledWith(1, 'https://example.test/next');
    expect(ev.input).not.toHaveBeenCalled();
  });

  it('resolves a relative link against the page URL', async () => {
    const { host, ev } = await mount();
    host.applySnapshot(withLink(snapshot(), '/deeper/page?q=1'));
    const doc = host.frame.contentDocument!;
    doc.querySelector('a')!.dispatchEvent(
      new doc.defaultView!.MouseEvent('auxclick', { bubbles: true, button: 1 }),
    );
    expect(ev.openLink).toHaveBeenCalledWith(1, 'https://example.test/deeper/page?q=1');
  });

  /**
   * Hacker News' parent | prev | next, and every footnote and table of
   * contents: a link into the document the reader is already looking at.
   *
   * These used to travel landside as clicks, where the real page scrolled
   * itself and reported back a pixel offset from a layout with different fonts
   * — which the client refuses outright once the reader has scrolled, so the
   * link did nothing at all. The whole document is already here; this side can
   * answer without spending a round trip on it.
   */
  it('scrolls to a link into the same document rather than sending it landside', async () => {
    const { host, ev } = await mount();
    host.applySnapshot(withTarget(withLink(snapshot(), 'https://example.test/#c2')));
    const doc = host.frame.contentDocument!;
    const win = doc.defaultView!;
    const target = doc.getElementById('c2')!;
    const into = vi.fn();
    target.scrollIntoView = into;

    const click = new win.MouseEvent('click', { bubbles: true, cancelable: true });
    doc.querySelector('a')!.dispatchEvent(click);

    // The element the fragment names, put where a browser puts it.
    expect(into).toHaveBeenCalledWith({ block: 'start' });
    expect(ev.input).not.toHaveBeenCalled();
    // And the frame still never follows a link itself.
    expect(click.defaultPrevented).toBe(true);
  });

  /**
   * The gap this closes is the whole reason the shell has a progress bar: the
   * click is a semantic event replayed seconds away, and until the server says
   * `frameStartedLoading` nothing on this side knows a page is coming. It has
   * to be told as the click goes out, not when the answer arrives.
   */
  it('tells the shell a page is coming as the link click goes out', async () => {
    const { host, ev } = await mount();
    host.applySnapshot(withLink(snapshot()));
    const doc = host.frame.contentDocument!;

    doc.querySelector('a')!.dispatchEvent(
      new doc.defaultView!.MouseEvent('click', { bubbles: true }));

    expect(ev.input).toHaveBeenCalledTimes(1);
    expect(ev.navigating).toHaveBeenCalledWith(1, 'https://example.test/next');
  });

  it('says nothing about a click that is not on a link', async () => {
    const { host, ev } = await mount();
    host.applySnapshot(snapshot());
    const doc = host.frame.contentDocument!;

    doc.querySelector('li')!.dispatchEvent(
      new doc.defaultView!.MouseEvent('click', { bubbles: true }));

    // The click still goes landside — it is what makes buttons work — but a
    // page is not what the reader is waiting for.
    expect(ev.input).toHaveBeenCalledTimes(1);
    expect(ev.navigating).not.toHaveBeenCalled();
  });

  it('says nothing about a link answered on this side', async () => {
    const { host, ev } = await mount();
    host.applySnapshot(withTarget(withLink(snapshot(), 'https://example.test/#c2')));
    const doc = host.frame.contentDocument!;
    doc.getElementById('c2')!.scrollIntoView = vi.fn();

    doc.querySelector('a')!.dispatchEvent(
      new doc.defaultView!.MouseEvent('click', { bubbles: true }));

    // A jump inside this document is instant and costs nothing. A bar promising
    // a page would be promising something that already happened.
    expect(ev.navigating).not.toHaveBeenCalled();
  });

  it('leaves the server the fragments this document cannot answer', async () => {
    // `#/inbox` is a hash-routed app's idea of a page, not a place in this
    // document. Nothing here answers to it, so the click goes to the page that
    // knows what it means.
    const { host, ev } = await mount();
    host.applySnapshot(withLink(snapshot(), 'https://example.test/#/inbox'));
    const doc = host.frame.contentDocument!;
    const win = doc.defaultView!;

    doc.querySelector('a')!.dispatchEvent(new win.MouseEvent('click', { bubbles: true }));

    const payload = ev.input.mock.calls[0][1] as Record<string, unknown>;
    expect(payload.kind).toBe('click');
    expect(payload.node).toBe(30);
  });

  it('sends a fragment on another page landside, where it is a navigation', async () => {
    const { host, ev } = await mount();
    host.applySnapshot(withTarget(withLink(snapshot(), 'https://example.test/other#c2')));
    const doc = host.frame.contentDocument!;
    const win = doc.defaultView!;
    const into = vi.fn();
    doc.getElementById('c2')!.scrollIntoView = into;

    doc.querySelector('a')!.dispatchEvent(new win.MouseEvent('click', { bubbles: true }));

    expect(into).not.toHaveBeenCalled();
    expect(ev.input).toHaveBeenCalledTimes(1);
  });

  it('opens a same-document link in a new tab on ctrl-click, fragment and all', async () => {
    const { host, ev } = await mount();
    host.applySnapshot(withTarget(withLink(snapshot(), 'https://example.test/#c2')));
    const doc = host.frame.contentDocument!;
    const win = doc.defaultView!;
    const into = vi.fn();
    doc.getElementById('c2')!.scrollIntoView = into;

    doc.querySelector('a')!.dispatchEvent(
      new win.MouseEvent('click', { bubbles: true, ctrlKey: true }));

    expect(ev.openLink).toHaveBeenCalledWith(1, 'https://example.test/#c2');
    expect(into).not.toHaveBeenCalled();
  });

  it('answers a right click with a menu target instead of the native menu', async () => {
    const { host, ev } = await mount();
    host.applySnapshot(withLink(snapshot()));
    const doc = host.frame.contentDocument!;
    const menu = new doc.defaultView!.MouseEvent('contextmenu', {
      bubbles: true, cancelable: true, clientX: 40, clientY: 60,
    });
    doc.querySelector('a')!.dispatchEvent(menu);

    expect(menu.defaultPrevented).toBe(true);
    // Forwarding the right click landside is now an entry on the menu, not the
    // automatic behaviour: it costs a round trip.
    expect(ev.input).not.toHaveBeenCalled();
    const [tab, target] = ev.menu.mock.calls[0] as [number, MenuTarget];
    expect(tab).toBe(1);
    expect(target.link).toBe('https://example.test/next');
    expect(target.linkText).toBe('Next page');
    expect(target.node).toBe(30);
    expect(target.field).toBe(0);
    expect(target.x).toBe(40);
    expect(target.y).toBe(60);
  });

  it('tells the shell to dismiss when the user acts inside the frame', async () => {
    // Events inside the frame never reach the shell's document, so a menu
    // floating over the mirror would otherwise survive a click on the page.
    // On the pointer event rather than the mouse one, because a finger that
    // swipes the page produces no mouse event at all and a menu left over a
    // page the reader has just scrolled is the same stale menu.
    const { host, ev } = await mount();
    host.applySnapshot(snapshot());
    const doc = host.frame.contentDocument!;
    const view = doc.defaultView!;
    doc.querySelector('li')!.dispatchEvent(
      new view.PointerEvent('pointerdown', { bubbles: true, isPrimary: true, button: 0 }),
    );
    expect(ev.dismiss).toHaveBeenCalledWith(1);

    // Escape stops at the menu when there is one, and reaches the page when
    // there is not.
    ev.dismiss.mockReturnValueOnce(true);
    const swallowed = new view.KeyboardEvent('keydown', {
      key: 'Escape', bubbles: true, cancelable: true,
    });
    doc.querySelector('li')!.dispatchEvent(swallowed);
    expect(swallowed.defaultPrevented).toBe(true);
    expect(ev.input).not.toHaveBeenCalled();
  });

  it('forwards a right click landside only when the menu asks for it', async () => {
    const { host, ev } = await mount();
    host.applySnapshot(snapshot());
    host.sendContextMenu(3);
    const payload = ev.input.mock.calls[0][1] as Record<string, unknown>;
    expect(payload.kind).toBe('contextmenu');
    expect(payload.node).toBe(3);
  });

  it('replaces a field selection locally and sends the whole value', async () => {
    const { host, ev } = await mount();
    const snap = snapshot();
    const base = snap.strings.length;
    snap.strings.push('form', 'input', 'data-sky-value', 'hello');
    snap.nodes.push(
      { id: 20, parent: 2, kind: NodeKind.Element, ref: base, attrs: [], flags: 0 },
      {
        id: 21, parent: 20, kind: NodeKind.Element, ref: base + 1,
        attrs: [base + 2, base + 3], flags: 1,
      },
    );
    host.applySnapshot(snap);

    const input = host.frame.contentDocument!.querySelector('input')!;
    input.setSelectionRange(0, 'hello'.length);
    host.replaceSelection(21, 'goodbye');

    // Locally instant, because waiting a round trip to see your own paste is
    // exactly what the echo engine exists to avoid.
    expect(input.value).toBe('goodbye');
    const set = ev.input.mock.calls
      .map((c) => c[1] as Record<string, unknown>)
      .find((p) => p.kind === 'setvalue');
    expect(set).toBeDefined();
    expect(set!.node).toBe(21);
    expect(set!.text).toBe('goodbye');
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
    expect(ev.applied).toHaveBeenLastCalledWith(1, 3, expect.any(Number), expect.any(Number));
  });

  /**
   * The last line of defence against the failure that shredded a streaming
   * page: a batch the server replayed after the client had already applied it.
   *
   * The damage is not the repeated ops — most are idempotent enough to survive
   * — but the strings. A batch's strings extend an append-only intern table by
   * position, so applying one twice leaves the table one entry long and every
   * reference after it lands on its neighbour. Text streamed in afterwards
   * arrives shredded three characters at a time, into the wrong nodes, and
   * nothing notices: the table is not part of the document hash.
   */
  it('refuses a batch it has already applied', async () => {
    const { host, ev } = await mount();
    host.applySnapshot(snapshot());
    const batch = (): Mutation => ({
      strings: [' and more'],
      docHash: 0,
      flush: false,
      ops: [{
        op: OpCode.Splice, node: 4, parent: 0, before: 0, ref: 11, ref2: 0,
        nodes: [], off: 5, del: 0, add: [], drop: [], x: 0, y: 0, str: '',
      }],
    });
    host.applyMutation(batch(), 3);
    const acks = ev.applied.mock.calls.length;

    host.applyMutation(batch(), 3);

    expect(host.frame.contentDocument!.body.textContent).toBe('first and more');
    // Not applied at all, rather than applied and then acknowledged again.
    expect(ev.applied.mock.calls.length).toBe(acks);

    // And the table underneath is still the length the server thinks it is, so
    // the next batch's references resolve to the strings it meant.
    host.applyMutation({
      strings: [' still'], docHash: 0, flush: false,
      ops: [{
        op: OpCode.Splice, node: 4, parent: 0, before: 0, ref: 12, ref2: 0,
        nodes: [], off: 14, del: 0, add: [], drop: [], x: 0, y: 0, str: '',
      }],
    }, 4);
    expect(host.frame.contentDocument!.body.textContent).toBe('first and more still');
  });

  describe('freezing for a capture', () => {
    it('takes the document, the patcher state and the fingerprint', async () => {
      const { host } = await mount();
      host.applySnapshot(snapshot());
      host.applyMutation({
        strings: [' more'], docHash: 0, flush: false,
        ops: [{
          op: OpCode.Splice, node: 4, parent: 0, before: 0, ref: 11, ref2: 0,
          nodes: [], off: 5, del: 0, add: [], drop: [], x: 0, y: 0, str: '',
        }],
      }, 5);

      const frozen = host.freeze();
      expect(frozen.tab).toBe(1);
      expect(frozen.html).toContain('first more');
      expect(frozen.error).toBeUndefined();
      expect(frozen.state.lastAppliedSeq).toBe(5);
      expect(frozen.state.url).toBe('https://example.test/');
      // The fingerprint is what turns "the hashes differ" into "these nodes
      // differ", so it has to list what the hash actually looks at.
      expect(frozen.fingerprint.total).toBe(4);
      expect(frozen.fingerprint.nodes.map((n) => n[0])).toEqual([1, 2, 3, 4]);
      expect(frozen.fingerprint.nodes[3][2]).toBe('first more');
    });

    // The capture worth having is the one taken at a divergence, and the very
    // next thing the server does about a divergence is resync the tab. If the
    // freeze were asynchronous the evidence would be gone by the time it ran.
    it('holds the diverged document even after a resync replaces it', async () => {
      const { host } = await mount();
      host.applySnapshot(snapshot());
      const frozen = host.freeze();

      const repaired = snapshot();
      repaired.strings[3] = 'repaired';
      host.applySnapshot(repaired);

      expect(host.frame.contentDocument!.body.textContent).toBe('repaired');
      expect(frozen.html).toContain('first');
      expect(frozen.html).not.toContain('repaired');
    });

    it('lists the images the document references, for the screenshot', async () => {
      const { host } = await mount();
      const snap = snapshot();
      const base = snap.strings.length;
      snap.strings.push('img', 'src', 'skyhook://img/abc123');
      snap.nodes.push({
        id: 40, parent: 3, kind: NodeKind.Element, ref: base,
        attrs: [base + 1, base + 2], flags: 0,
      });
      snap.images = [{
        node: 40, hash: 'abc123', w: 10, h: 10, blur: '', mime: 'image/webp',
        bytes: 1, priority: 0, alt: '', box: [], missing: false,
      }];
      host.applySnapshot(snap);

      // An SVG screenshot cannot fetch anything, so these hashes are what the
      // rasteriser inlines from Cache Storage.
      expect(host.freeze().images).toContain('abc123');
    });
  });
});

/*
Where an optimistically-echoed message goes.

The ghost exists so a message appears the instant it is typed, seconds before
the server can confirm it. That is only worth anything if it appears where the
message will actually land. "The first list in the document" is not that place
on any real chat app: Google Chat's home has ten elements at `role="list"` and
every one is in the left rail — the direct messages, the spaces, the apps. The
transcript is somewhere else entirely, so the reader's own message went into
the sidebar under their list of conversations.

The fixture is that shape: navigation lists first in document order, then the
conversation pane holding a transcript and the composer that writes to it.
*/
describe('nearestList', () => {
  function chatApp(): HTMLElement {
    const app = document.createElement('div');
    app.innerHTML = `
      <nav>
        <div role="list" aria-label="List of Direct Messages"><div>ada</div></div>
        <div role="list" aria-label="List of spaces."><div>#general</div></div>
      </nav>
      <main>
        <div class="pane">
          <div role="list" class="transcript"><div>an earlier message</div></div>
          <div class="composer" contenteditable="true"></div>
        </div>
      </main>`;
    document.body.appendChild(app);
    return app;
  }

  afterEach(() => { document.body.textContent = ''; });

  it('finds the transcript beside the composer, not the navigation', () => {
    const app = chatApp();
    const composer = app.querySelector<HTMLElement>('.composer')!;
    expect(nearestList(composer)?.className).toBe('transcript');
  });

  it('answers with nothing when the page has no list to join', () => {
    const app = chatApp();
    for (const el of Array.from(app.querySelectorAll('[role="list"]'))) el.remove();
    const composer = app.querySelector<HTMLElement>('.composer')!;
    expect(nearestList(composer)).toBeNull();
  });

  it('does not put the echo inside the composer it just left', () => {
    // A rich-text composer holding the bullets the reader was typing. It is a
    // list, it is nearest, and it is the one place the message cannot go.
    document.body.innerHTML = `<div class="pane">
        <div role="list" class="transcript"><div>an earlier message</div></div>
        <div class="composer" contenteditable="true"><ul><li>a bullet</li></ul></div>
      </div>`;
    const composer = document.querySelector<HTMLElement>('.composer')!;
    expect(nearestList(composer)?.className).toBe('transcript');
  });

  it('still reaches the body on a page where the body is the container', () => {
    // Nothing between the composer and the body: a plain message board, where
    // the only list on the page is the one the message joins. Searching
    // outwards has to end somewhere, and ending at the body is what keeps the
    // simple case working.
    document.body.innerHTML = `<ul id="log"><li>an earlier message</li></ul>
      <input id="say">`;
    const composer = document.getElementById('say')!;
    expect(nearestList(composer)?.id).toBe('log');
  });
});

/*
Where a message goes when the page keeps the Enter for itself.

The ghost is a message drawn before anything can confirm it, and until now
nothing could ever take it back: it retires when the real message turns up, and
a message that never went left a bubble that reads as sent for the rest of the
session. The reader then types the next one into a composer they believe is
empty, and the page appends it to the first.

That is not a hypothetical. Google Chat keeps Enter for its own emoji
autocomplete — `:/` becomes an emoji, nothing is sent — and "the icons are
still missing :/" and "message" arrived in the transcript as one line (P-132).
A mention picker, a slash-command menu and a validation error all keep it too,
and none of them say so. What they all leave behind is text in the composer,
which is the whole signal: a chat composer that still holds text did not send.
*/
describe('an optimistic send the page did not make', () => {
  /** The fixture: a transcript to send into, and a composer to type in. */
  function withComposer(snap: Snapshot): Snapshot {
    const base = snap.strings.length;
    snap.strings.push('div', 'contenteditable', 'true', 'data-sky-value', '');
    snap.nodes.push({
      id: 40, parent: 1, kind: NodeKind.Element, ref: base,
      attrs: [base + 1, base + 2, base + 3, base + 4], flags: NodeFlags.Editable,
    });
    return snap;
  }

  /** One attribute op, as the agent sends a field's live text. */
  function valueOp(node: number, value: string): Mutation {
    return {
      strings: ['data-sky-value', value], docHash: 0, flush: false,
      ops: [{
        op: OpCode.Attr, node, parent: 0, before: 0, ref: -1, ref2: -1,
        nodes: [], off: 0, del: 0, add: [], drop: [], x: 0, y: 0, str: '',
      }],
    };
  }

  /** Types into the composer the way the reader does: locally, then told. */
  function type(host: MirrorHost, text: string): HTMLElement {
    const doc = host.frame.contentDocument!;
    const view = doc.defaultView as unknown as typeof globalThis;
    const composer = doc.querySelector<HTMLElement>('[contenteditable]')!;
    composer.dispatchEvent(new view.FocusEvent('focusin', { bubbles: true }));
    composer.textContent = text;
    composer.dispatchEvent(new view.InputEvent('input', { bubbles: true }));
    return composer;
  }

  function pressEnter(composer: HTMLElement): void {
    const view = composer.ownerDocument.defaultView as unknown as typeof globalThis;
    composer.dispatchEvent(new view.KeyboardEvent('keydown', {
      key: 'Enter', bubbles: true, cancelable: true,
    }));
  }

  /** Resolves the refs of a batch's own strings, which join the table on top. */
  function refs(m: Mutation, base: number): Mutation {
    m.ops[0].ref = base;
    m.ops[0].ref2 = base + 1;
    return m;
  }

  it('takes the ghost back when the composer still holds the text', async () => {
    const { host } = await mount();
    const snap = withComposer(snapshot());
    const base = snap.strings.length;
    host.applySnapshot(snap);
    const doc = host.frame.contentDocument!;

    const composer = type(host, 'the icons are still missing :/');
    pressEnter(composer);
    expect(doc.querySelectorAll('[data-skyhook-ghost]')).toHaveLength(1);
    expect(composer.textContent).toBe('');

    // The page's answer: the Enter went to its emoji autocomplete, the smiley
    // is an emoji now, and the message is still sitting in the composer.
    host.applyMutation(refs(valueOp(40, 'the icons are still missing 🫤'), base), 3);

    expect(doc.querySelectorAll('[data-skyhook-ghost]')).toHaveLength(0);
    expect(composer.textContent).toBe('the icons are still missing 🫤');
  });

  it('leaves the ghost alone when the composer comes back empty', async () => {
    const { host } = await mount();
    const snap = withComposer(snapshot());
    const base = snap.strings.length;
    host.applySnapshot(snap);
    const doc = host.frame.contentDocument!;

    const composer = type(host, 'this one went');
    pressEnter(composer);
    host.applyMutation(refs(valueOp(40, ''), base), 3);

    // An empty composer is the page saying it took the message. The ghost
    // stands until the real one arrives to retire it.
    expect(doc.querySelectorAll('[data-skyhook-ghost]')).toHaveLength(1);
  });

  /*
   * The echo the reader has already typed past.
   *
   * Every keystroke is replayed landside, so the field's text comes back as
   * the reader types — and over this link it comes back several keystrokes
   * late. Taking a late echo as truth would put the field back to what it held
   * a second ago and lose everything since, which is the one thing local echo
   * exists to prevent. A mutation says which input provoked it, and that is
   * what tells the two apart.
   */
  it('ignores a value echoed from an edit the reader has typed past', async () => {
    const { host } = await mount();
    const snap = withComposer(snapshot());
    const base = snap.strings.length;
    host.applySnapshot(snap);

    const composer = type(host, 'hello there');
    // Caused by the very first thing the reader did, long since typed past.
    host.applyMutation(refs(valueOp(40, 'hel'), base), 3, 1);
    expect(composer.textContent).toBe('hello there');
  });

  /*
   * A burst of typing is one frame, not one per key.
   *
   * Appended deltas concatenate losslessly, so pooling them for a beat costs
   * nothing but the beat — and saves a frame, a landside replay, an echo
   * batch and an ack per keystroke it absorbs. Anything that is not appended
   * text flushes the pool ahead of itself, so the wire order of what the
   * reader did survives exactly.
   */
  it('pools a burst of keystrokes into one text frame', async () => {
    vi.useFakeTimers();
    try {
      const { host, ev } = await mount();
      const snap = withComposer(snapshot());
      host.applySnapshot(snap);

      let composer = type(host, 'h');
      composer = type(host, 'he');
      composer = type(host, 'hel');
      const textFrames = () => ev.input.mock.calls
        .map((c) => c[1] as Record<string, unknown>)
        .filter((p) => p.kind === 'text');
      expect(textFrames()).toHaveLength(0);

      await vi.advanceTimersByTimeAsync(200);
      expect(textFrames()).toHaveLength(1);
      expect(textFrames()[0].text).toBe('hel');

      // A control key flushes the pool ahead of itself: the Enter must not
      // arrive landside before the letters it follows.
      type(host, 'hell');
      pressEnter(composer);
      const kinds = ev.input.mock.calls
        .map((c) => (c[1] as Record<string, unknown>).kind)
        .filter((k) => k === 'text' || k === 'key');
      expect(kinds).toEqual(['text', 'text', 'key']);
      expect(textFrames()[1].text).toBe('l');
    } finally {
      vi.useRealTimers();
    }
  });
});

/*
The position reported for a scrolled container is the reader's, not the one the
server put back under them.

A container's scroll is throttled: the reader scrolls, and a quarter of a second
later the server is told where they got to. In that window the server's own idea
of where the container sits can arrive — a position from before the scroll,
already in flight — and followScroll applies it, because it declines to move a
scroller the reader has taken over *except* when they are at the bottom of it,
which is the one place following along is the point. A reader who has just
scrolled to the end of a list is exactly there.

So the box went back to where it had been, and the report then described that
instead of the scroll: the page was told to stay where it was, the list never
built the rows below, and the reader scrolled into blank space. Over a fast link
the stale position has usually already landed; over 1.2 s of round trip it is
still in flight, which is why the emulated-link job was the only one that ever
saw it (P-134).
*/
describe('a container scroll over a slow link', () => {
  function withScroller(snap: Snapshot): Snapshot {
    const base = snap.strings.length;
    snap.strings.push('div', 'id', 'feed');
    snap.nodes.push({
      id: 60, parent: 1, kind: NodeKind.Element, ref: base,
      attrs: [base + 1, base + 2], flags: NodeFlags.ScrollDiv,
    });
    return snap;
  }

  /** A box with somewhere to scroll, which jsdom will not work out for itself. */
  function sized(el: HTMLElement, top: number): void {
    Object.defineProperty(el, 'clientHeight', { value: 200, configurable: true });
    Object.defineProperty(el, 'scrollHeight', { value: 400, configurable: true });
    el.scrollTop = top;
  }

  it('reports where the reader left it, not where the server put it back', async () => {
    vi.useFakeTimers();
    try {
      const { host, ev } = await mount();
      host.applySnapshot(withScroller(snapshot()));
      const doc = host.frame.contentDocument!;
      const feed = doc.getElementById('feed')!;

      // The reader scrolls to the end of what they have.
      sized(feed, 200);
      feed.dispatchEvent(new (doc.defaultView as unknown as typeof globalThis)
        .Event('scroll', { bubbles: false }));

      // The server's position from before that scroll arrives while the report
      // is still waiting, and the reader is at the bottom, so it is applied.
      host.applyMutation(scrollOp(60, 0, 0), 2);
      expect(feed.scrollTop).toBe(0);

      await vi.advanceTimersByTimeAsync(400);

      const sent = ev.scroll.mock.calls.map((c) => c[1] as Record<string, unknown>)
        .filter((s) => s.node === 60);
      expect(sent).toHaveLength(1);
      expect(sent[0].y).toBe(200);
    } finally {
      vi.useRealTimers();
    }
  });

  /*
   * The other half of the same window: the node, not the position.
   *
   * A snapshot inside the throttle window rebuilds the patcher's map, and the
   * element the reader scrolled stops having an id — so the report found no
   * node to name and gave up without a word. Watched live it reads id=8 at the
   * scroll and id=0 a quarter of a second later, which is why the shaped job
   * failed with a server log that recorded nothing at all: the frame was never
   * sent (P-134).
   */
  it('reports the node the reader scrolled, even once the document has moved on', async () => {
    vi.useFakeTimers();
    try {
      const { host, ev } = await mount();
      host.applySnapshot(withScroller(snapshot()));
      const doc = host.frame.contentDocument!;
      const feed = doc.getElementById('feed')!;

      sized(feed, 200);
      feed.dispatchEvent(new (doc.defaultView as unknown as typeof globalThis)
        .Event('scroll', { bubbles: false }));

      // A resync lands before the report does, and the tree it built has no
      // room for the element the reader was holding.
      host.applySnapshot(snapshot());
      await vi.advanceTimersByTimeAsync(400);

      const sent = ev.scroll.mock.calls.map((c) => c[1] as Record<string, unknown>)
        .filter((s) => s.node === 60);
      expect(sent).toHaveLength(1);
      expect(sent[0].y).toBe(200);
    } finally {
      vi.useRealTimers();
    }
  });

  it('reports the last position of a scroll still in progress', async () => {
    vi.useFakeTimers();
    try {
      const { host, ev } = await mount();
      host.applySnapshot(withScroller(snapshot()));
      const doc = host.frame.contentDocument!;
      const feed = doc.getElementById('feed')!;
      const scroll = () => feed.dispatchEvent(
        new (doc.defaultView as unknown as typeof globalThis).Event('scroll', { bubbles: false }));

      // One throttle window, three positions: what the reader ends on is what
      // the page is told, and it is told once.
      sized(feed, 40);
      scroll();
      sized(feed, 120);
      scroll();
      sized(feed, 200);
      scroll();
      await vi.advanceTimersByTimeAsync(400);

      const sent = ev.scroll.mock.calls.map((c) => c[1] as Record<string, unknown>)
        .filter((s) => s.node === 60);
      expect(sent).toHaveLength(1);
      expect(sent[0].y).toBe(200);
    } finally {
      vi.useRealTimers();
    }
  });
});

/*
Pull to reload, measured the way a phone actually reports the gesture.

The client's reload button is not on the phone's toolbar — it is in the ⋯ menu,
and a page that arrived wrong costs minutes of this link to fetch again — so
the gesture every phone browser binds it to is the one that has to work. The
measuring is here because this is the only code with the frame's document to
listen to; what the numbers mean is app/pull.ts.

Touch events and not pointer events, and the tests are written on touch events
for the same reason the code is: a pull down at the top of a page is a gesture
the browser has already decided is a scroll, and the note beside the pointer
wiring says what it does with those — one `pointermove`, then a `pointercancel`
and nothing after it. There is no pull to be measured on that stream.

What each of these is really asking is which gestures are *not* this one. A
reader scrolling a menu, panning a map, swiping a carousel or taking a pull
back has not asked for a page, and on this link a page asked for by accident is
the expensive mistake.
*/
describe('a pull down from the top of a page', () => {
  type Point = { clientX: number; clientY: number };

  /** Fingers on the glass. jsdom builds its TouchList out of whatever it is
   *  handed, and the gesture reads two fields of each. */
  function fingers(points: Point[]): Touch[] {
    return points as unknown as Touch[];
  }

  function touch(host: MirrorHost, target: Element, kind: string, points: Point[]): void {
    const win = host.frame.contentDocument!.defaultView!;
    target.dispatchEvent(new win.TouchEvent(kind, { bubbles: true, touches: fingers(points) }));
  }

  /** A finger down at the top of the page and dragged straight down, in the
   *  steps one actually travels in. */
  function pullDown(host: MirrorHost, target: Element, by: number): void {
    touch(host, target, 'touchstart', [{ clientX: 60, clientY: 20 }]);
    for (let i = 1; i <= 4; i++) {
      touch(host, target, 'touchmove', [{ clientX: 60, clientY: 20 + (by * i) / 4 }]);
    }
    touch(host, target, 'touchend', []);
  }

  /** Where the fixture's list sits, which is somewhere a finger can land. */
  async function mounted(): Promise<{
    host: MirrorHost; ev: ReturnType<typeof events>; page: Element;
  }> {
    const { host, ev } = await mount();
    host.applySnapshot(snapshot());
    return { host, ev, page: host.frame.contentDocument!.getElementById('log')! };
  }

  function states(ev: ReturnType<typeof events>): PullState[] {
    return ev.pull.mock.calls.map((c) => c[1] as PullState);
  }

  it('reports the drag as it happens and the distance it ended at', async () => {
    const { host, ev, page } = await mounted();
    pullDown(host, page, 100);

    const seen = states(ev);
    // The shell draws from these: a pull nobody hears about until the finger
    // leaves is a gesture with no affordance, which is the same as no gesture.
    expect(seen.length).toBeGreaterThan(1);
    expect(seen.slice(0, -1).every((s) => !s.released)).toBe(true);
    expect(seen.map((s) => s.distance)).toEqual([25, 50, 75, 100, 100]);
    expect(seen[seen.length - 1].released).toBe(true);
  });

  it('says nothing at all for a finger that barely moved', async () => {
    const { host, ev, page } = await mounted();
    // A tap with a shaky thumb behind it. Below the slop there is no gesture,
    // and the shell is never given an indicator to put away.
    pullDown(host, page, 4);
    expect(ev.pull).not.toHaveBeenCalled();
  });

  it('leaves a sideways swipe to the page', async () => {
    const { host, ev, page } = await mounted();
    touch(host, page, 'touchstart', [{ clientX: 200, clientY: 20 }]);
    for (let i = 1; i <= 4; i++) {
      touch(host, page, 'touchmove', [{ clientX: 200 - i * 30, clientY: 20 + i * 2 }]);
    }
    touch(host, page, 'touchend', []);

    expect(ev.pull).not.toHaveBeenCalled();
  });

  it('gives the gesture up when the finger goes back up, and does not take it again', async () => {
    const { host, ev, page } = await mounted();
    touch(host, page, 'touchstart', [{ clientX: 60, clientY: 100 }]);
    touch(host, page, 'touchmove', [{ clientX: 60, clientY: 180 }]);
    // Back past where it started: the reader has changed their mind and is
    // scrolling on down the page.
    touch(host, page, 'touchmove', [{ clientX: 60, clientY: 60 }]);
    // And down again, which is a scroll in progress and not a second pull.
    touch(host, page, 'touchmove', [{ clientX: 60, clientY: 200 }]);
    touch(host, page, 'touchend', []);

    const seen = states(ev);
    expect(seen[seen.length - 1]).toEqual({ distance: 0, released: true });
    expect(seen.filter((s) => s.released)).toHaveLength(1);
  });

  it('reports nothing when the page is not at its top', async () => {
    const { host, ev, page } = await mounted();
    const win = host.frame.contentDocument!.defaultView!;
    Object.defineProperty(win, 'scrollY', { value: 320, configurable: true });

    pullDown(host, page, 100);
    expect(ev.pull).not.toHaveBeenCalled();
  });

  it('leaves the drag to a list that has been scrolled', async () => {
    const { host, ev, page } = await mounted();
    // Scroll chaining: a drag inside a scrolled menu belongs to the menu until
    // the menu is back at its own top. Stealing it for a reload takes the page
    // out from under a reader who was only looking at a list.
    const list = page.parentElement!;
    list.scrollTop = 90;

    pullDown(host, page, 100);
    expect(ev.pull).not.toHaveBeenCalled();
  });

  it('leaves a canvas to the pan it is already claimed by', async () => {
    const { host, ev } = await mountWithCanvas();
    const canvas = host.frame.contentDocument!.querySelector('canvas')!;

    pullDown(host, canvas, 100);
    expect(ev.pull).not.toHaveBeenCalled();
  });

  it('ends at nothing when a second finger arrives', async () => {
    const { host, ev, page } = await mounted();
    touch(host, page, 'touchstart', [{ clientX: 60, clientY: 20 }]);
    touch(host, page, 'touchmove', [{ clientX: 60, clientY: 120 }]);
    // A pinch is not a pull, and the hundred pixels the first finger travelled
    // are not a reload the reader asked for.
    touch(host, page, 'touchmove',
      [{ clientX: 60, clientY: 140 }, { clientX: 200, clientY: 300 }]);
    touch(host, page, 'touchend', []);

    expect(states(ev)[states(ev).length - 1]).toEqual({ distance: 0, released: true });
  });

  it('leaves a press that closed a sheet to the sheet', async () => {
    // On a phone the panel is a sheet over the page, and touching the page is
    // how the reader puts it away. A drag that does that is spent: the reader
    // was reaching past the sheet, not asking for the page again — and asking
    // for it again costs them minutes of this link.
    const { host, ev, page } = await mounted();
    ev.dismiss.mockReturnValue(true);
    const win = host.frame.contentDocument!.defaultView!;
    page.dispatchEvent(new win.PointerEvent('pointerdown', {
      bubbles: true, clientX: 60, clientY: 20, button: 0, isPrimary: true, pointerType: 'touch',
    }));

    pullDown(host, page, 100);
    expect(ev.pull).not.toHaveBeenCalled();
  });

  it('ends at nothing when the browser takes the gesture', async () => {
    const { host, ev, page } = await mounted();
    touch(host, page, 'touchstart', [{ clientX: 60, clientY: 20 }]);
    touch(host, page, 'touchmove', [{ clientX: 60, clientY: 120 }]);
    // The system's own back swipe, the app going away, a call arriving.
    touch(host, page, 'touchcancel', []);

    expect(states(ev)[states(ev).length - 1]).toEqual({ distance: 0, released: true });
  });
});
