/**
 * Mirror host tests.
 *
 * The first test is the important one: the sandbox attribute is the single
 * mechanism standing between mirrored content and script execution plane-side.
 * If someone ever adds `allow-scripts` to make something convenient work, this
 * fails loudly.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { MirrorHost, type MenuTarget } from '../src/mirror/host.js';
import { imageCacheKey } from '../src/shared/caches.js';
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
    menu: vi.fn(),
    dismiss: vi.fn(() => false),
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
    expect(img.getAttribute('src')?.startsWith('data:image/gif')).toBe(true);
    // Nor does /img/<hash>: the frame is sandboxed, so it is not a service
    // worker client, and that URL would go to the network — which is the one
    // thing the frame must never do.
    expect(img.getAttribute('src')).not.toContain('/img/');
    // The hash rides on the element instead, so metadata and bytes arriving
    // later can still find it.
    expect(img.dataset.skyhookImg).toBe('deadbeef');
  });

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
    const { host, ev } = await mount();
    host.applySnapshot(snapshot());
    const doc = host.frame.contentDocument!;
    const view = doc.defaultView!;
    doc.querySelector('li')!.dispatchEvent(new view.MouseEvent('mousedown', { bubbles: true }));
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
    expect(ev.applied).toHaveBeenLastCalledWith(1, 3, expect.any(Number));
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
        bytes: 1, priority: 0, alt: '',
      }];
      host.applySnapshot(snap);

      // An SVG screenshot cannot fetch anything, so these hashes are what the
      // rasteriser inlines from Cache Storage.
      expect(host.freeze().images).toContain('abc123');
    });
  });
});
