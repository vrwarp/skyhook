/**
 * The tab model, which exists so that pressing "+" costs zero round trips.
 * Everything here is about the window between asking for a tab and being told
 * its id — a window that is over a second wide on the link this project is for,
 * and in which the user is already typing.
 */
import { describe, expect, it, beforeEach } from 'vitest';

import { TabModel } from '../src/app/tabs.js';
import type { TabState } from '../src/shared/protocol.js';

function state(over: Partial<TabState> = {}): TabState {
  return {
    url: '', title: '', loading: false, canBack: false, canForward: false,
    faviconId: '', closed: false, error: '', ref: '', ...over,
  };
}

describe('TabModel', () => {
  let sent: { name: string; args: Record<string, unknown> }[];
  let adopted: [number, number][];
  let dropped: number[];
  let model: TabModel;

  beforeEach(() => {
    sent = [];
    adopted = [];
    dropped = [];
    model = new TabModel({
      send: (name, args) => sent.push({ name, args }),
      adopted: (from, to) => adopted.push([from, to]),
      dropped: (id) => dropped.push(id),
    });
  });

  const refOf = (i = 0): string => String(sent[i].args.ref);

  it('draws the tab before the server has heard of it', () => {
    const id = model.open();
    expect(model.size).toBe(1);
    expect(model.active).toBe(id);
    expect(model.isProvisional(id)).toBe(true);
    // And asks for it in the same breath, tagged so the answer can be matched.
    expect(sent[0].name).toBe('openTab');
    expect(sent[0].args.ref).toBeTruthy();
  });

  it('opens a background tab without stealing the foreground', () => {
    const first = model.open('https://a.test/');
    model.applyState(1, state({ ref: refOf(0), url: 'https://a.test/' }));
    const second = model.open('https://b.test/', true);
    expect(model.active).toBe(1);
    expect(model.active).not.toBe(second);
    expect(sent[1].args.background).toBe(true);
    expect(first).not.toBe(second);
  });

  it('adopts the drawn tab rather than adding a second one', () => {
    const provisional = model.open('https://example.test/');
    model.applyState(4, state({ ref: refOf(0), url: 'https://example.test/', loading: true }));

    expect(model.size).toBe(1);
    expect(model.get(4)?.url).toBe('https://example.test/');
    expect(model.get(provisional)).toBeUndefined();
    expect(model.active).toBe(4);
    expect(model.isProvisional(4)).toBe(false);
    // The mirror frame drawn for the provisional tab is the same frame; the
    // shell is told so it can rekey it rather than throw it away.
    expect(adopted).toEqual([[provisional, 4]]);
  });

  it('holds what the user does to a tab that has no id yet, then sends it in order', () => {
    const id = model.open();
    model.forTab('navigate', id, { url: 'https://example.test/' });
    model.forTab('ack', id, { seq: 2, hash: 9 });
    expect(sent.filter((s) => s.name !== 'openTab')).toEqual([]);

    model.applyState(3, state({ ref: refOf(0) }));
    expect(sent.slice(1)).toEqual([
      { name: 'navigate', args: { url: 'https://example.test/', tab: 3 } },
      { name: 'ack', args: { seq: 2, hash: 9, tab: 3 } },
    ]);
  });

  it('sends straight through once a tab has a real id', () => {
    model.open();
    model.applyState(1, state({ ref: refOf(0) }));
    model.forTab('navigate', 1, { url: 'https://example.test/' });
    expect(sent[1]).toEqual({ name: 'navigate', args: { url: 'https://example.test/', tab: 1 } });
  });

  it('closes a tab that was abandoned before it was ever named', () => {
    const id = model.open();
    model.close(id);
    expect(model.size).toBe(0);
    expect(dropped).toEqual([id]);
    // Landside the page is being built regardless, so the close has to arrive
    // once there is an id to close.
    expect(sent.length).toBe(1);

    model.applyState(2, state({ ref: refOf(0) }));
    expect(sent[1]).toEqual({ name: 'closeTab', args: { tab: 2 } });
    // And the tab must not reappear on the way out.
    expect(model.size).toBe(0);
    expect(adopted).toEqual([]);
  });

  it('closes an ordinary tab immediately', () => {
    model.open();
    model.applyState(1, state({ ref: refOf(0) }));
    model.close(1);
    expect(sent[1]).toEqual({ name: 'closeTab', args: { tab: 1 } });
    expect(model.size).toBe(0);
  });

  it('merges a partial state instead of blanking the tab', () => {
    model.open();
    model.applyState(1, state({ ref: refOf(0), url: 'https://example.test/', title: 'Example' }));
    model.applyState(1, state({ loading: true }));
    expect(model.get(1)).toMatchObject({
      url: 'https://example.test/', title: 'Example', loading: true,
    });
  });

  it('drops a tab the server says is closed', () => {
    model.open();
    model.applyState(1, state({ ref: refOf(0) }));
    expect(model.applyState(1, state({ closed: true }))).toBeUndefined();
    expect(model.size).toBe(0);
    expect(dropped).toEqual([1]);
  });

  it('gives up unadopted tabs when the connection is replaced', () => {
    const stale = model.open();
    model.forTab('navigate', stale, { url: 'https://example.test/' });
    model.reset([
      { tab: 1, url: 'https://a.test/', title: 'A', seq: 4, active: true, loading: false },
    ]);

    expect(model.ids()).toEqual([1]);
    expect(model.active).toBe(1);
    expect(dropped).toEqual([stale]);
    // The held navigation must not be replayed onto whichever tab the server
    // happens to have named 1.
    expect(sent.length).toBe(1);
  });

  it('keeps the front tab across a resume that does not name one', () => {
    model.reset([
      { tab: 1, url: 'https://a.test/', title: 'A', seq: 0, active: false, loading: false },
      { tab: 2, url: 'https://b.test/', title: 'B', seq: 0, active: false, loading: false },
    ]);
    expect(model.active).toBe(1);
    model.select(2);
    model.reset([
      { tab: 1, url: 'https://a.test/', title: 'A', seq: 0, active: false, loading: false },
      { tab: 2, url: 'https://b.test/', title: 'B', seq: 0, active: false, loading: false },
    ]);
    expect(model.active).toBe(2);
  });

  it('keeps two tabs opened back to back apart', () => {
    // Pressing "+" twice inside one round trip is exactly what an unresponsive
    // button used to provoke, and both tabs have to survive it.
    const first = model.open();
    const second = model.open();
    expect(first).not.toBe(second);
    model.applyState(7, state({ ref: refOf(1) }));
    model.applyState(6, state({ ref: refOf(0) }));
    expect(model.ids().sort()).toEqual([6, 7]);
    expect(model.active).toBe(7);
  });
});
