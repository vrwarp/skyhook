/**
 * Where the reader has actually been: the second free navigation surface.
 *
 * The saved list (`bookmarks.ts`) is what somebody chose to keep. This is what
 * they did, which is a different and larger thing — nobody stars the page they
 * open every morning, they just type four letters of it. On a link where a page
 * costs seconds and a mistyped address costs two of them, the four letters
 * turning into the right address without a round trip is the whole point.
 *
 * Three decisions distinguish this from a browser's history, and all three come
 * from the link rather than from taste:
 *
 * 1. **Only confirmed pages are recorded.** An entry is written when the server
 *    says where a tab actually went, never from what was typed into the address
 *    bar. So a typo that resolves to nothing never enters the list and can never
 *    be completed back to later, and one page is one row rather than one row for
 *    the address typed and another for the address it redirected to.
 * 2. **What was typed outranks what was merely reached.** Following a link is
 *    cheap evidence — the page was in front of the reader already. Typing an
 *    address is somebody naming a destination from memory, which is exactly the
 *    thing an address bar should finish for them.
 * 3. **It evicts rather than refusing.** The saved list refuses to grow past its
 *    cap because dropping a bookmark is losing data the reader entered. This is
 *    a cache of a behaviour, so the least useful end of it can go.
 *
 * It is plane-side only: nothing sends it anywhere, it survives an outage
 * exactly as it reads on a good link, and `Store.wipe()` takes it with
 * everything else.
 */

import { cleanTitle, matchScore, normalizeUrl, type Bookmark } from './bookmarks.js';

/** One address the reader has been to. Identity is the normalised URL. */
export interface HistoryEntry {
  url: string;
  title: string;
  /** Times reached by an address typed into the address bar. */
  typed: number;
  /** Times reached at all, by any gesture. */
  visits: number;
  /** When it was last reached: what recency ties are broken on. */
  lastAt: number;
}

/**
 * Twice the saved list's cap. History earns its keep by covering the long tail
 * of "that thing I looked at last week", which a few hundred rows does not, and
 * unlike the saved list it is written on every navigation rather than on a
 * deliberate gesture — so the ceiling is what stops an unattended session from
 * growing a value that has to be read, written and cloned on every page.
 */
export const HISTORY_LIMIT = 1000;

/**
 * How long writes are allowed to sit unwritten. One page load produces a visit
 * and then a title or two as the document settles; writing the whole list three
 * times for one navigation is waste with nothing bought by it. The exposure is
 * a second of history on a hard kill, against a store that is a convenience by
 * construction.
 */
const WRITE_DELAY_MS = 1000;

/** What the store has to provide. `onError` is how a failed write gets seen. */
export interface HistoryIO {
  read(): Promise<unknown>;
  write(entries: HistoryEntry[]): Promise<void>;
  onError?(message: string): void;
}

/** Reads whatever is on disk into entries, discarding what cannot be one. */
export function parseHistory(value: unknown): HistoryEntry[] {
  if (!Array.isArray(value)) return [];
  const out: HistoryEntry[] = [];
  const seen = new Set<string>();
  for (const row of value) {
    if (!row || typeof row !== 'object') continue;
    const rec = row as Partial<HistoryEntry>;
    const url = typeof rec.url === 'string' ? rec.url.trim() : '';
    if (!url) continue;
    const key = normalizeUrl(url);
    if (seen.has(key)) continue;
    seen.add(key);
    out.push({
      url,
      title: cleanTitle(typeof rec.title === 'string' ? rec.title : '', url),
      typed: count(rec.typed),
      visits: Math.max(1, count(rec.visits)),
      lastAt: count(rec.lastAt),
    });
    if (out.length >= HISTORY_LIMIT) break;
  }
  return out;
}

function count(value: unknown): number {
  return typeof value === 'number' && isFinite(value) && value > 0 ? Math.floor(value) : 0;
}

/**
 * A row of the address bar's dropdown, whatever it came from.
 *
 * The two sources are kept apart here rather than flattened into one shape,
 * because the difference is visible to the reader: a saved page is theirs and
 * the dropdown must not destroy it, while a history row is a record of
 * something they did and removing it is the point of the X.
 */
export type Completion =
  | { kind: 'saved'; title: string; url: string; mark: Bookmark }
  | { kind: 'history'; title: string; url: string; entry: HistoryEntry };

/** Sorts stronger evidence first: saved, then typed, then merely reached. */
function weight(c: Completion): number {
  if (c.kind === 'saved') return 3;
  return c.entry.typed > 0 ? 2 : 1;
}

function when(c: Completion): number {
  return c.kind === 'saved'
    ? Math.max(c.mark.usedAt ?? 0, c.mark.addedAt)
    : c.entry.lastAt;
}

/**
 * What the address bar offers for a query: both lists, ranked together.
 *
 * Match quality decides first and provenance second, which is what
 * "prioritise what I typed" means once it meets a real query. A page whose
 * address the reader is halfway through re-typing is a better answer than a
 * bookmark that merely contains those letters somewhere in its title — the
 * bookmark wins only when the two match equally well, and it wins every tie.
 *
 * A URL that is both saved and visited is one row, shown as the saved one. Two
 * rows for one page in a list of six is the dropdown wasting the reader's
 * screen to tell them something they did not ask about.
 */
export function completions(
  marks: Bookmark[],
  entries: HistoryEntry[],
  query: string,
  limit = Infinity,
): Completion[] {
  const q = query.trim();
  const seen = new Set<string>();
  const scored: { row: Completion; score: number }[] = [];

  const consider = (row: Completion): void => {
    const key = normalizeUrl(row.url);
    if (!key || seen.has(key)) return;
    const score = matchScore(q, row.title, row.url);
    if (score < 0) return;
    seen.add(key);
    scored.push({ row, score });
  };

  // Saved first, so it is the saved entry that claims a URL both lists hold.
  for (const mark of marks) {
    consider({ kind: 'saved', title: mark.title, url: mark.url, mark });
  }
  for (const entry of entries) {
    consider({ kind: 'history', title: entry.title, url: entry.url, entry });
  }

  // An empty query is a different question — "where was I?", asked with the
  // arrow key — and it gets the honest answer to that one: most recent first,
  // whatever it came from. Ranking it by provenance instead would fill the six
  // rows with the saved list, which is the one thing the reader can already see
  // in full on the page behind the dropdown.
  if (!q) scored.sort((a, b) => when(b.row) - when(a.row));
  else {
    scored.sort((a, b) => b.score - a.score
      || weight(b.row) - weight(a.row)
      || when(b.row) - when(a.row));
  }
  const rows = scored.map((s) => s.row);
  return limit === Infinity ? rows : rows.slice(0, limit);
}

/**
 * The live list. Loads once, kept whole in memory — every read of it happens in
 * front of a reader waiting on a keystroke — and written back with the changes
 * of the last second folded together.
 */
export class History {
  private entries = new Map<string, HistoryEntry>();
  private listeners = new Set<() => void>();
  private ready: Promise<void>;
  private timer: ReturnType<typeof setTimeout> | undefined;
  private pending = false;

  constructor(private io: HistoryIO, private delayMs = WRITE_DELAY_MS) {
    this.ready = this.load();
  }

  private async load(): Promise<void> {
    try {
      // Oldest first, which is the order the map is kept in — see `touch`.
      const entries = parseHistory(await this.io.read()).sort((a, b) => a.lastAt - b.lastAt);
      for (const entry of entries) this.entries.set(normalizeUrl(entry.url), entry);
    } catch (err) {
      this.io.onError?.(`history could not be read: ${String(err)}`);
      this.entries.clear();
    }
    this.announce();
  }

  /** Resolves when what was on disk has been read. */
  whenReady(): Promise<void> {
    return this.ready;
  }

  /**
   * Most recently reached first. A copy: callers rank and filter it freely.
   *
   * Read off the map's own order rather than sorted by timestamp, because a
   * browser can put several pages in the same millisecond — a redirect chain is
   * exactly that — and an order that is arbitrary between them is an order that
   * decides differently each time it is asked, in a list the reader is watching
   * while they type.
   */
  all(): HistoryEntry[] {
    return Array.from(this.entries.values()).reverse();
  }

  count(): number {
    return this.entries.size;
  }

  find(url: string): HistoryEntry | undefined {
    const key = normalizeUrl(url);
    return key ? this.entries.get(key) : undefined;
  }

  /**
   * Records that a tab arrived somewhere. Called only for a URL the server has
   * confirmed, never for what was typed — see the header.
   *
   * `typed` says the navigation started as an address the reader entered
   * themselves, which is carried from the gesture rather than inferred here:
   * by the time the answer comes back, the field it was typed into has moved on.
   */
  record(url: string, title: string, typed = false): void {
    const key = normalizeUrl(url);
    if (!key) return;
    const now = Date.now();
    const existing = this.entries.get(key);
    if (existing) {
      existing.visits += 1;
      if (typed) existing.typed += 1;
      existing.lastAt = now;
      // A revisit under a better title updates it; a revisit that arrives
      // before the document has a title must not blank the one already there.
      if (title.trim()) existing.title = cleanTitle(title, existing.url);
      this.touch(key, existing);
    } else {
      this.entries.set(key, {
        url: url.trim(),
        title: cleanTitle(title, url),
        typed: typed ? 1 : 0,
        visits: 1,
        lastAt: now,
      });
      this.evict();
    }
    this.changed();
  }

  /**
   * A page's title arriving after its URL did. Common: the server confirms
   * where a tab went long before the document it went to has a `<title>`.
   */
  retitle(url: string, title: string): void {
    if (!title.trim()) return;
    const entry = this.find(url);
    if (!entry) return;
    const next = cleanTitle(title, entry.url);
    if (next === entry.title) return;
    entry.title = next;
    this.changed();
  }

  /** Drops one, handing it back so an undo can put it in again. */
  forget(url: string): HistoryEntry | undefined {
    const key = normalizeUrl(url);
    if (!key) return undefined;
    const entry = this.entries.get(key);
    if (!entry) return undefined;
    this.entries.delete(key);
    this.changed();
    return entry;
  }

  /** Puts entries back, for the undo on a single removal or on a clear. */
  restore(entries: HistoryEntry[]): void {
    let added = false;
    for (const entry of entries) {
      const key = normalizeUrl(entry.url);
      if (!key || this.entries.has(key)) continue;
      this.entries.set(key, entry);
      added = true;
    }
    if (!added) return;
    // An undo puts back entries of every age, so the map's order has to be
    // rebuilt rather than appended to — otherwise a page restored from last
    // week would sit at the young end and outlive everything around it.
    const ordered = Array.from(this.entries.values()).sort((a, b) => a.lastAt - b.lastAt);
    this.entries.clear();
    for (const entry of ordered) this.entries.set(normalizeUrl(entry.url), entry);
    this.evict();
    this.changed();
  }

  /** Empties it, handing back what was there so the toast can offer the undo. */
  clear(): HistoryEntry[] {
    const gone = Array.from(this.entries.values());
    if (!gone.length) return gone;
    this.entries.clear();
    this.changed();
    return gone;
  }

  /**
   * Moves an entry to the young end of the map.
   *
   * The map is kept in the order things were last reached, oldest first, which
   * is what makes both eviction and `all()` a walk rather than a sort — and,
   * more importantly, what makes them answer the same way twice when a handful
   * of pages share a millisecond.
   */
  private touch(key: string, entry: HistoryEntry): void {
    this.entries.delete(key);
    this.entries.set(key, entry);
  }

  /**
   * Trims to the cap, oldest first. A typed address never goes while a page
   * merely passed through is still there: what is thrown away is the tail of
   * things seen once on the way somewhere, which is exactly what nobody is
   * about to re-type. Only a list that is *all* typed addresses loses one.
   */
  private evict(): void {
    if (this.entries.size <= HISTORY_LIMIT) return;
    for (const [key, entry] of this.entries) {
      if (this.entries.size <= HISTORY_LIMIT) return;
      if (entry.typed === 0) this.entries.delete(key);
    }
    for (const key of Array.from(this.entries.keys())) {
      if (this.entries.size <= HISTORY_LIMIT) return;
      this.entries.delete(key);
    }
  }

  /** Subscribes to changes. Returns the unsubscribe. */
  onChange(fn: () => void): () => void {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  }

  /**
   * Writes now rather than at the end of the window. For `pagehide`, where
   * there is no later.
   */
  flush(): void {
    if (this.timer) {
      clearTimeout(this.timer);
      this.timer = undefined;
    }
    if (!this.pending) return;
    this.pending = false;
    void this.io.write(this.all()).catch((err: unknown) => {
      this.io.onError?.(`history could not be saved: ${String(err)}`);
    });
  }

  private changed(): void {
    this.pending = true;
    if (!this.timer) this.timer = setTimeout(() => this.flush(), this.delayMs);
    this.announce();
  }

  private announce(): void {
    for (const fn of this.listeners) fn();
  }
}
