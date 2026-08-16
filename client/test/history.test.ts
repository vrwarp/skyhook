/**
 * Where the reader has been, and what the address bar does with it.
 *
 * The rules worth pinning are the ones that come from the link rather than from
 * taste: an address the reader typed outranks one they merely landed on, one
 * page is one row however many times it reports itself, and the list evicts its
 * least useful end rather than growing without limit.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { parseBookmarks, type Bookmark } from '../src/app/bookmarks.js';
import {
  completions, History, HISTORY_LIMIT, parseHistory, type HistoryEntry,
} from '../src/app/history.js';

function io(seed: unknown = []): {
  read: () => Promise<unknown>;
  write: (e: HistoryEntry[]) => Promise<void>;
  written: HistoryEntry[][];
} {
  const written: HistoryEntry[][] = [];
  return {
    read: () => Promise.resolve(seed),
    write: (e) => { written.push(e); return Promise.resolve(); },
    written,
  };
}

/** A store whose writes land immediately, so the assertions are about content. */
async function loaded(seed: unknown = []): Promise<{ h: History; store: ReturnType<typeof io> }> {
  const store = io(seed);
  const h = new History(store, 0);
  await h.whenReady();
  return { h, store };
}

describe('reading what is on disk', () => {
  it('keeps one row per address and repairs what it can', () => {
    const entries = parseHistory([
      { url: 'https://a.example/', title: '  Spaced   out  ', typed: 2, visits: 5, lastAt: 9 },
      { url: 'https://a.example/', title: 'a duplicate an older client wrote' },
      { url: '   ', title: 'no address at all' },
      { url: 'https://b.example/', title: '' },
      'not a record',
    ]);
    expect(entries).toHaveLength(2);
    expect(entries[0].title).toBe('Spaced out');
    // A row with no usable title is labelled with its address rather than
    // rendering as an empty line the reader cannot identify.
    expect(entries[1].title).toBe('b.example');
    // Anything that was on disk was reached at least once, whatever it claims.
    expect(entries[1].visits).toBe(1);
  });

  it('refuses anything that is not a list', () => {
    expect(parseHistory(undefined)).toEqual([]);
    expect(parseHistory({ url: 'https://a.example/' })).toEqual([]);
  });
});

describe('recording arrivals', () => {
  it('counts a revisit rather than adding a second row', async () => {
    const { h } = await loaded();
    h.record('https://a.example/one', 'One', true);
    h.record('https://a.example/one', 'One, renamed');
    expect(h.count()).toBe(1);
    const entry = h.find('https://a.example/one') as HistoryEntry;
    expect(entry.visits).toBe(2);
    expect(entry.typed).toBe(1);
    expect(entry.title).toBe('One, renamed');
  });

  it('does not blank a title when a revisit arrives before the page has one', async () => {
    const { h } = await loaded();
    h.record('https://a.example/one', 'One');
    h.record('https://a.example/one', '');
    expect(h.find('https://a.example/one')?.title).toBe('One');
  });

  it('retitles an address that is already there, and only that', async () => {
    const { h } = await loaded();
    h.record('https://a.example/one', '');
    h.retitle('https://a.example/one', 'The title, arriving late');
    h.retitle('https://b.example/never-visited', 'Nothing to retitle');
    expect(h.find('https://a.example/one')?.title).toBe('The title, arriving late');
    expect(h.find('https://a.example/one')?.visits).toBe(1);
    expect(h.count()).toBe(1);
  });

  it('hands a removal back so an undo can put it in again', async () => {
    const { h } = await loaded();
    h.record('https://a.example/one', 'One');
    const gone = h.forget('https://a.example/one') as HistoryEntry;
    expect(h.count()).toBe(0);
    expect(h.forget('https://a.example/one')).toBeUndefined();
    h.restore([gone]);
    expect(h.find('https://a.example/one')?.title).toBe('One');
  });

  it('gives a clear back whole, for the same reason', async () => {
    const { h } = await loaded();
    h.record('https://a.example/one', 'One');
    h.record('https://b.example/two', 'Two');
    const gone = h.clear();
    expect(h.count()).toBe(0);
    expect(gone).toHaveLength(2);
    h.restore(gone);
    expect(h.count()).toBe(2);
  });

  it('drops what was reached in passing before what was named', async () => {
    const { h } = await loaded();
    // Filled past the cap with untyped visits, and one typed address made the
    // oldest of all of them — the entry most obviously worth keeping and the
    // first an ordinary "drop the oldest" rule would throw away.
    h.record('https://typed.example/', 'Typed once', true);
    for (let i = 0; i < HISTORY_LIMIT + 20; i += 1) {
      h.record(`https://seen.example/${i}`, `Seen ${i}`);
    }
    expect(h.count()).toBe(HISTORY_LIMIT);
    expect(h.find('https://typed.example/')).toBeDefined();
    expect(h.find('https://seen.example/0')).toBeUndefined();
  });

  it('writes the changes of a moment once, and on demand', async () => {
    vi.useFakeTimers();
    try {
      const store = io([]);
      const h = new History(store, 1000);
      await h.whenReady();
      // A page load is a visit and then a title or two; three whole-list writes
      // for one navigation is waste with nothing bought by it.
      h.record('https://a.example/one', '');
      h.retitle('https://a.example/one', 'One');
      h.retitle('https://a.example/one', 'One, settled');
      expect(store.written).toHaveLength(0);
      vi.advanceTimersByTime(1000);
      expect(store.written).toHaveLength(1);
      expect(store.written[0][0].title).toBe('One, settled');

      // And a page going away has no later to be written at.
      h.record('https://b.example/two', 'Two');
      h.flush();
      expect(store.written).toHaveLength(2);
      // Nothing outstanding: a second flush must not write the list again.
      h.flush();
      expect(store.written).toHaveLength(2);
    } finally {
      vi.useRealTimers();
    }
  });
});

describe('what the address bar offers', () => {
  const MARKS: Bookmark[] = parseBookmarks([
    { title: 'Hacker News', url: 'https://news.ycombinator.com', addedAt: 100 },
  ]);
  const VISITS: HistoryEntry[] = parseHistory([
    { url: 'https://news.example/typed', title: 'Named from memory', typed: 3, visits: 3, lastAt: 200 },
    { url: 'https://news.example/followed', title: 'Followed a link', typed: 0, visits: 30, lastAt: 300 },
    { url: 'https://news.ycombinator.com/', title: 'Hacker News', typed: 9, visits: 40, lastAt: 400 },
  ]);

  let shown: ReturnType<typeof completions>;
  beforeEach(() => {
    shown = completions(MARKS, VISITS, 'news.example', 6);
  });

  it('puts what was typed above what was only reached', () => {
    // Thirty visits against three, and the three still win: following a link is
    // cheap evidence — the page was already in front of the reader — while
    // typing an address is somebody naming a destination from memory, which is
    // the thing an address bar exists to finish.
    expect(shown.map((c) => c.title)).toEqual(['Named from memory', 'Followed a link']);
  });

  it('shows a page that is both saved and visited once, as the saved one', () => {
    const rows = completions(MARKS, VISITS, 'ycombinator', 6);
    expect(rows).toHaveLength(1);
    expect(rows[0].kind).toBe('saved');
  });

  it('ranks an address being re-typed above a title that merely contains it', () => {
    const marks = parseBookmarks([
      { title: 'All about weather', url: 'https://example.com/almanac', addedAt: 1 },
    ]);
    const entries = parseHistory([
      { url: 'https://weather.example/today', title: 'Today', visits: 1, lastAt: 2 },
    ]);
    expect(completions(marks, entries, 'weather', 6).map((c) => c.url))
      .toEqual(['https://weather.example/today', 'https://example.com/almanac']);
  });

  it('answers an empty query with everything, most recent first', () => {
    const rows = completions(MARKS, VISITS, '', 6);
    expect(rows).toHaveLength(3);
    // Recency alone: the bookmark is the least recently reached of the three
    // and it sorts like it, because the question this list answers is where the
    // reader was, not what they keep.
    expect(rows.map((c) => c.title)).toEqual(['Followed a link', 'Named from memory', 'Hacker News']);
    expect(rows[2].kind).toBe('saved');
  });

  it('offers nothing rather than something nearly right', () => {
    // Not fuzzy, deliberately: a completion that is close costs a round trip to
    // find out it was wrong, and on this link that is seconds.
    expect(completions(MARKS, VISITS, 'nwes', 6)).toEqual([]);
  });

  it('keeps to the limit it is given', () => {
    expect(completions(MARKS, VISITS, '', 2)).toHaveLength(2);
  });
});
