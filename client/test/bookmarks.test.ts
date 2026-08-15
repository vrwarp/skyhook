/**
 * Bookmark logic.
 *
 * The saved list is the only state on this client that exists nowhere else —
 * the server can replace a lost page, a lost session and a lost image cache,
 * and cannot replace this — so the tests here are mostly about the ways it
 * could quietly lose or duplicate something.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';

import {
  BOOKMARK_LIMIT, Bookmarks, cleanTitle, displayHost, exportText, normalizeUrl, parseBookmarks,
  parseImport, search, type Bookmark, type BookmarkIO,
} from '../src/app/bookmarks.js';

/** A store that remembers, so a round trip through persistence is testable. */
function fakeIO(initial: unknown = []): BookmarkIO & { written: unknown } {
  return {
    written: initial,
    read(): Promise<unknown> {
      return Promise.resolve(this.written);
    },
    write(marks: Bookmark[]): Promise<void> {
      this.written = marks;
      return Promise.resolve();
    },
  };
}

async function loaded(initial: unknown = []): Promise<Bookmarks> {
  const marks = new Bookmarks(fakeIO(initial));
  await marks.whenReady();
  return marks;
}

describe('url normalisation', () => {
  it('collapses the forms of an address that mean the same page', () => {
    expect(normalizeUrl('HTTPS://News.YCombinator.com/'))
      .toBe(normalizeUrl('https://news.ycombinator.com'));
    expect(normalizeUrl('https://example.com:443/x')).toBe(normalizeUrl('https://example.com/x'));
    expect(normalizeUrl('  https://example.com/x  ')).toBe(normalizeUrl('https://example.com/x'));
  });

  it('keeps apart what is actually a different page', () => {
    expect(normalizeUrl('https://example.com/a?page=2'))
      .not.toBe(normalizeUrl('https://example.com/a?page=3'));
    expect(normalizeUrl('https://example.com/a'))
      .not.toBe(normalizeUrl('https://example.com/b'));
    expect(normalizeUrl('https://example.com/a#top'))
      .not.toBe(normalizeUrl('https://example.com/a'));
  });

  it('survives something that is not a URL at all', () => {
    // The address bar takes searches too, since the server resolves them.
    expect(normalizeUrl('weather in reykjavik')).toBe('weather in reykjavik');
    expect(normalizeUrl('')).toBe('');
  });

  it('reads a host for the second line of a row', () => {
    expect(displayHost('https://www.example.com/a')).toBe('example.com');
    expect(displayHost('nonsense')).toBe('');
  });
});

describe('titles', () => {
  it('collapses the whitespace a real <title> carries', () => {
    expect(cleanTitle('\n  Hacker\tNews\n ', 'https://news.ycombinator.com')).toBe('Hacker News');
  });

  it('falls back to the address when a link has no usable text', () => {
    // The mirror menu bookmarks a link with the anchor's own text, which is
    // frequently nothing at all.
    expect(cleanTitle('', 'https://example.com/deep/page')).toBe('example.com/deep/page');
    expect(cleanTitle('   ', 'https://example.com/')).toBe('example.com');
  });

  it('clamps a title that is really a paragraph', () => {
    expect(cleanTitle('x'.repeat(500), 'https://example.com').length).toBe(160);
  });
});

describe('reading what is on disk', () => {
  it('migrates the first on-disk shape, which had no ids or dates', () => {
    const marks = parseBookmarks([
      { title: 'Hacker News', url: 'https://news.ycombinator.com' },
      { title: '', url: 'https://example.com/x' },
    ]);
    expect(marks).toHaveLength(2);
    expect(marks[0].id).toBeTruthy();
    expect(marks[0].addedAt).toBe(0);
    expect(marks[1].title).toBe('example.com/x');
  });

  it('drops what cannot be a bookmark rather than showing it', () => {
    expect(parseBookmarks([null, 3, 'x', {}, { title: 'no url' }])).toEqual([]);
    expect(parseBookmarks('not an array')).toEqual([]);
    expect(parseBookmarks(undefined)).toEqual([]);
  });

  it('collapses duplicates that an older client could have written', () => {
    const marks = parseBookmarks([
      { title: 'One', url: 'https://example.com/' },
      { title: 'Two', url: 'https://EXAMPLE.com' },
    ]);
    expect(marks).toHaveLength(1);
    expect(marks[0].title).toBe('One');
  });
});

describe('search', () => {
  const marks = parseBookmarks([
    { title: 'Hacker News', url: 'https://news.ycombinator.com', addedAt: 1 },
    { title: 'Flight status', url: 'https://example.com/flights', addedAt: 2 },
    { title: 'Docs', url: 'https://docs.example.com/hacker/guide', addedAt: 3 },
  ]);

  it('orders by most recently used with no query', () => {
    expect(search(marks, '').map((m) => m.title)).toEqual(['Docs', 'Flight status', 'Hacker News']);
    const used = marks.map((m) => (m.title === 'Hacker News' ? { ...m, usedAt: 99 } : m));
    expect(search(used, '')[0].title).toBe('Hacker News');
  });

  it('puts a match on the front of a title or host above one in the middle', () => {
    // "Hacker News" starts with it; the docs entry only has it in the path.
    expect(search(marks, 'hacker').map((m) => m.title)).toEqual(['Hacker News', 'Docs']);
  });

  it('matches the address as well as the title, and limits', () => {
    expect(search(marks, 'ycombinator').map((m) => m.title)).toEqual(['Hacker News']);
    expect(search(marks, '', 2)).toHaveLength(2);
    expect(search(marks, 'nothing here')).toEqual([]);
  });
});

describe('the live list', () => {
  beforeEach(() => {
    vi.useRealTimers();
  });

  it('saves once however many times the star is pressed', async () => {
    const marks = await loaded();
    const first = marks.add('Hacker News', 'https://news.ycombinator.com');
    const second = marks.add('Hacker News', 'https://news.ycombinator.com/');
    expect(first.existed).toBe(false);
    expect(second.existed).toBe(true);
    expect(second.mark?.id).toBe(first.mark?.id);
    expect(marks.count()).toBe(1);
    expect(marks.has('https://NEWS.ycombinator.com')).toBe(true);
  });

  it('writes through, and reads back what it wrote', async () => {
    const io = fakeIO();
    const marks = new Bookmarks(io);
    await marks.whenReady();
    marks.add('Docs', 'https://example.com/docs');
    expect((io.written as Bookmark[])[0].title).toBe('Docs');

    const reopened = new Bookmarks(io);
    await reopened.whenReady();
    expect(reopened.all().map((m) => m.title)).toEqual(['Docs']);
  });

  it('puts a removed entry back exactly where it was', async () => {
    const marks = await loaded();
    marks.add('A', 'https://a.example.com');
    marks.add('B', 'https://b.example.com');
    marks.add('C', 'https://c.example.com');
    const gone = marks.remove(marks.find('https://b.example.com')!.id);
    expect(gone?.index).toBe(1);
    expect(marks.count()).toBe(2);
    marks.restore(gone!.mark, gone!.index);
    expect(marks.count()).toBe(3);
    // Restoring twice must not duplicate: the undo can be clicked once and the
    // toast can also time out around it.
    marks.restore(gone!.mark, gone!.index);
    expect(marks.count()).toBe(3);
  });

  it('renames, and refuses to be renamed into nothing', async () => {
    const marks = await loaded();
    const { mark } = marks.add('more', 'https://example.com/story/1');
    expect(marks.rename(mark!.id, '  Flight tracker ')).toBe(true);
    expect(marks.find('https://example.com/story/1')?.title).toBe('Flight tracker');
    marks.rename(mark!.id, '   ');
    expect(marks.find('https://example.com/story/1')?.title).toBe('example.com/story/1');
    expect(marks.rename('no such id', 'x')).toBe(false);
  });

  it('refuses to add past the cap rather than evicting somebody else', async () => {
    const seed = Array.from({ length: BOOKMARK_LIMIT }, (_, i) => ({
      title: `p${i}`, url: `https://example.com/${i}`, addedAt: i,
    }));
    const marks = await loaded(seed);
    expect(marks.count()).toBe(BOOKMARK_LIMIT);
    const result = marks.add('one more', 'https://example.com/extra');
    expect(result.full).toBe(true);
    expect(marks.count()).toBe(BOOKMARK_LIMIT);
    expect(marks.has('https://example.com/0')).toBe(true);
  });

  it('surfaces a failed write instead of pretending the page is kept', async () => {
    const errors: string[] = [];
    const marks = new Bookmarks({
      read: () => Promise.resolve([]),
      write: () => Promise.reject(new Error('quota exceeded')),
      onError: (message) => errors.push(message),
    });
    await marks.whenReady();
    marks.add('Docs', 'https://example.com/docs');
    await Promise.resolve();
    await Promise.resolve();
    expect(errors.join(' ')).toContain('quota exceeded');
  });

  it('notifies subscribers on every change, including the first load', async () => {
    const io = fakeIO([{ title: 'A', url: 'https://a.example.com' }]);
    const marks = new Bookmarks(io);
    const seen = vi.fn();
    marks.onChange(seen);
    await marks.whenReady();
    expect(seen).toHaveBeenCalledTimes(1);
    marks.add('B', 'https://b.example.com');
    expect(seen).toHaveBeenCalledTimes(2);
  });

  it('bumps what was opened to the front of the list', async () => {
    const marks = await loaded([
      { title: 'A', url: 'https://a.example.com', addedAt: 1 },
      { title: 'B', url: 'https://b.example.com', addedAt: 2 },
    ]);
    expect(marks.all()[0].title).toBe('B');
    marks.touch('https://a.example.com/');
    expect(marks.all()[0].title).toBe('A');
  });
});

describe('export and import', () => {
  it('round-trips', async () => {
    const marks = await loaded();
    marks.add('Hacker News', 'https://news.ycombinator.com');
    marks.add('Docs', 'https://example.com/docs');
    const text = exportText(marks.all());

    const fresh = await loaded();
    const { added, skipped } = fresh.merge(parseImport(text));
    expect([added, skipped]).toEqual([2, 0]);
    expect(fresh.all().map((m) => m.title).sort()).toEqual(['Docs', 'Hacker News']);
  });

  it('is additive: importing twice adds nothing the second time', async () => {
    const marks = await loaded([{ title: 'Kept name', url: 'https://example.com/x' }]);
    const incoming = parseImport(exportText(parseBookmarks([
      { title: 'Other name', url: 'https://example.com/x' },
      { title: 'New', url: 'https://example.com/y' },
    ])));
    expect(marks.merge(incoming)).toEqual({ added: 1, skipped: 1 });
    expect(marks.find('https://example.com/x')?.title).toBe('Kept name');
    expect(marks.merge(incoming)).toEqual({ added: 0, skipped: 2 });
  });

  it('accepts a bare array, and says what is wrong with anything else', () => {
    expect(parseImport('[{"title":"A","url":"https://a.example.com"}]')).toHaveLength(1);
    expect(() => parseImport('not json')).toThrow(/not JSON/);
    expect(() => parseImport('{"marks":[]}')).toThrow(/no bookmarks/);
  });
});
