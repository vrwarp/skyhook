/**
 * Bookmarks: the only navigation surface on this client that costs nothing.
 *
 * Everything else a reader can do to get somewhere spends the link. Typing an
 * address spends a page load and, if the address is wrong, a second one; a
 * link on the page had to be paid for before it could be clicked. The list in
 * this file is plane-side data: opening it, searching it and reading it during
 * a ten-minute outage are all free, and the only round trip is the one the
 * reader actually asked for.
 *
 * That is why the rules here are stricter than a browser's would be. Entries
 * are deduplicated by normalised URL, because a list you cannot trust to hold
 * one copy of a page is a list you have to read carefully, and reading
 * carefully is what nobody does at 35,000 feet. Titles are cleaned and clamped,
 * because a bookmark made from a link takes the anchor's own text — which is
 * frequently `more`, `→`, or four hundred characters of navigation furniture.
 * And the list is ordered by when it was last *used*, not when it was made,
 * because the page you opened yesterday is the page you want today.
 *
 * The persistence layer is injected rather than imported so this module can be
 * exercised without IndexedDB, and so the shell can decide what a failed write
 * should say.
 */

/** One saved page. `usedAt` is absent until it has been opened from the list. */
export interface Bookmark {
  id: string;
  title: string;
  url: string;
  addedAt: number;
  usedAt?: number;
}

/**
 * The cap exists because the whole list is one IndexedDB value: it is read
 * whole, written whole, and carried into a diagnostic capture. Five hundred
 * entries is far more than a personal browser accumulates in a year and still
 * a value small enough to write on every change without thinking about it.
 */
export const BOOKMARK_LIMIT = 500;

const TITLE_MAX = 160;

/** What the store has to provide. `onError` is how a failed write gets seen. */
export interface BookmarkIO {
  read(): Promise<unknown>;
  write(marks: Bookmark[]): Promise<void>;
  onError?(message: string): void;
}

/** The result of an add: what is in the list now, and why it may be unchanged. */
export interface AddResult {
  mark?: Bookmark;
  /** The URL was already saved; `mark` is the entry that was already there. */
  existed: boolean;
  /** The list is at BOOKMARK_LIMIT and nothing was added. */
  full: boolean;
}

let counter = 0;

function newId(): string {
  const uuid = globalThis.crypto?.randomUUID?.();
  if (uuid) return uuid;
  counter += 1;
  return `bm-${Date.now().toString(36)}-${counter}`;
}

/**
 * The form of a URL two bookmarks are compared by. Deliberately conservative:
 * scheme and host are case-insensitive per RFC 3986 and a default port means
 * the same thing as no port, but everything after the authority is left alone.
 * A query string is frequently the whole address on the sites this client is
 * used for, and `?page=2` is not the same page as `?page=3`.
 *
 * A string that is not a URL at all — what the address bar accepts, since the
 * server resolves searches — normalises to its trimmed self, so two identical
 * searches still collapse to one entry.
 */
export function normalizeUrl(raw: string): string {
  const trimmed = raw.trim();
  if (!trimmed) return '';
  try {
    const u = new URL(trimmed);
    const path = u.pathname === '/' && !u.search && !u.hash ? '' : u.pathname;
    return `${u.protocol}//${u.host}${path}${u.search}${u.hash}`;
  } catch {
    return trimmed.toLowerCase();
  }
}

/** The host, for the second line of a row. Empty for anything unparseable. */
export function displayHost(url: string): string {
  try {
    return new URL(url).host.replace(/^www\./, '');
  } catch {
    return '';
  }
}

/**
 * A title worth showing. Whitespace in a page title is arbitrary — a `<title>`
 * spanning three indented lines is common — and an anchor's text is worse. When
 * nothing usable survives, the address is a better label than an empty row.
 */
export function cleanTitle(title: string, url: string): string {
  const collapsed = title.replace(/\s+/g, ' ').trim();
  if (collapsed) return collapsed.slice(0, TITLE_MAX);
  const host = displayHost(url);
  if (!host) return url.slice(0, TITLE_MAX);
  try {
    const u = new URL(url);
    const path = u.pathname === '/' ? '' : u.pathname;
    return `${host}${path}`.slice(0, TITLE_MAX);
  } catch {
    return host;
  }
}

/**
 * Reads whatever is in storage into bookmarks, discarding what cannot be one.
 *
 * The shape on disk has changed once already — the first version stored bare
 * `{title, url}` pairs with no identity and no timestamps — and a reader
 * upgrading the client mid-flight cannot be asked to re-save anything. So an
 * entry missing an id or a date is given one rather than dropped, and anything
 * without a usable URL is dropped rather than shown.
 */
export function parseBookmarks(value: unknown): Bookmark[] {
  if (!Array.isArray(value)) return [];
  const out: Bookmark[] = [];
  const seen = new Set<string>();
  for (const row of value) {
    if (!row || typeof row !== 'object') continue;
    const rec = row as Partial<Bookmark>;
    const url = typeof rec.url === 'string' ? rec.url.trim() : '';
    if (!url) continue;
    const key = normalizeUrl(url);
    if (seen.has(key)) continue;
    seen.add(key);
    const addedAt = typeof rec.addedAt === 'number' && isFinite(rec.addedAt) ? rec.addedAt : 0;
    const mark: Bookmark = {
      id: typeof rec.id === 'string' && rec.id ? rec.id : newId(),
      title: cleanTitle(typeof rec.title === 'string' ? rec.title : '', url),
      url,
      addedAt,
    };
    if (typeof rec.usedAt === 'number' && isFinite(rec.usedAt)) mark.usedAt = rec.usedAt;
    out.push(mark);
    if (out.length >= BOOKMARK_LIMIT) break;
  }
  return out;
}

/** When an entry was last touched: what the list is ordered by. */
export function recency(mark: Bookmark): number {
  return Math.max(mark.usedAt ?? 0, mark.addedAt);
}

/**
 * Ranks the list against a query, for the panel's filter and the address bar's
 * suggestions alike.
 *
 * Substring rather than fuzzy: a fuzzy match that offers the wrong page is a
 * round trip spent for nothing, and on this link that is several seconds the
 * reader watches. A match at the start of the title or the host outranks one in
 * the middle, and beyond that recency decides.
 */
export function search(marks: Bookmark[], query: string, limit = Infinity): Bookmark[] {
  const q = query.trim().toLowerCase();
  const ordered = marks.slice().sort((a, b) => recency(b) - recency(a));
  if (!q) return limit === Infinity ? ordered : ordered.slice(0, limit);
  const scored: { mark: Bookmark; score: number }[] = [];
  for (const mark of ordered) {
    const title = mark.title.toLowerCase();
    const url = mark.url.toLowerCase();
    const host = displayHost(mark.url).toLowerCase();
    let score = -1;
    if (title.startsWith(q) || host.startsWith(q)) score = 3;
    else if (title.includes(q)) score = 2;
    else if (url.includes(q)) score = 1;
    if (score < 0) continue;
    scored.push({ mark, score });
  }
  // Stable within a score band, so ties keep the recency order established
  // above rather than jumping around as the reader types.
  scored.sort((a, b) => b.score - a.score);
  const picked = scored.map((s) => s.mark);
  return limit === Infinity ? picked : picked.slice(0, limit);
}

/** The export file: plain JSON, because the point of it is being readable. */
export function exportText(marks: Bookmark[]): string {
  return `${JSON.stringify({ kind: 'skyhook-bookmarks', version: 1, marks }, null, 2)}\n`;
}

/**
 * Reads an export back. Accepts the wrapper this file writes and a bare array,
 * which is both the old on-disk shape and what a hand-written file looks like.
 * Throws with something a reader can act on, since this is reached from a file
 * they picked themselves.
 */
export function parseImport(text: string): Bookmark[] {
  let value: unknown;
  try {
    value = JSON.parse(text);
  } catch (err) {
    throw new Error(`that file is not JSON: ${String(err)}`, { cause: err });
  }
  const marks = Array.isArray(value)
    ? parseBookmarks(value)
    : parseBookmarks((value as { marks?: unknown })?.marks);
  if (!marks.length) throw new Error('no bookmarks in that file');
  return marks;
}

/**
 * The live list. Loads once, keeps the whole thing in memory — it is a few
 * kilobytes and every read of it happens in front of a reader who is waiting —
 * and writes through on every change.
 */
export class Bookmarks {
  private marks: Bookmark[] = [];
  private listeners = new Set<() => void>();
  private ready: Promise<void>;

  constructor(private io: BookmarkIO) {
    this.ready = this.load();
  }

  private async load(): Promise<void> {
    try {
      this.marks = parseBookmarks(await this.io.read());
    } catch (err) {
      this.io.onError?.(`bookmarks could not be read: ${String(err)}`);
      this.marks = [];
    }
    this.changed(false);
  }

  /** Resolves when the stored list has been read. */
  whenReady(): Promise<void> {
    return this.ready;
  }

  /** Most recently used first. A copy: callers render and filter it freely. */
  all(): Bookmark[] {
    return search(this.marks, '');
  }

  count(): number {
    return this.marks.length;
  }

  find(url: string): Bookmark | undefined {
    const key = normalizeUrl(url);
    if (!key) return undefined;
    return this.marks.find((m) => normalizeUrl(m.url) === key);
  }

  has(url: string): boolean {
    return this.find(url) !== undefined;
  }

  /**
   * Saves a page. Idempotent by normalised URL: the star is a toggle and a
   * reader who cannot tell whether the first click registered will click again.
   *
   * A full list refuses rather than evicting. Dropping somebody's oldest
   * bookmark to make room for a new one is a data loss they never asked for and
   * would not find out about until the flight they needed it.
   */
  add(title: string, url: string): AddResult {
    const trimmed = url.trim();
    if (!trimmed) return { existed: false, full: false };
    const existing = this.find(trimmed);
    if (existing) return { mark: existing, existed: true, full: false };
    if (this.marks.length >= BOOKMARK_LIMIT) return { existed: false, full: true };
    const mark: Bookmark = {
      id: newId(),
      title: cleanTitle(title, trimmed),
      url: trimmed,
      addedAt: Date.now(),
    };
    this.marks.push(mark);
    this.changed();
    return { mark, existed: false, full: false };
  }

  /** Removes by id, reporting where it was so an undo can put it back. */
  remove(id: string): { mark: Bookmark; index: number } | undefined {
    const index = this.marks.findIndex((m) => m.id === id);
    if (index < 0) return undefined;
    const [mark] = this.marks.splice(index, 1);
    this.changed();
    return { mark, index };
  }

  /** Puts a removed entry back where it was. The other half of an undo. */
  restore(mark: Bookmark, index: number): void {
    if (this.marks.some((m) => m.id === mark.id)) return;
    this.marks.splice(Math.max(0, Math.min(index, this.marks.length)), 0, mark);
    this.changed();
  }

  /** Renames. An empty name falls back to the address rather than blanking. */
  rename(id: string, title: string): boolean {
    const mark = this.marks.find((m) => m.id === id);
    if (!mark) return false;
    const next = cleanTitle(title, mark.url);
    if (next === mark.title) return false;
    mark.title = next;
    this.changed();
    return true;
  }

  /** Records that an entry was opened, which is what the ordering is about. */
  touch(url: string): void {
    const mark = this.find(url);
    if (!mark) return;
    mark.usedAt = Date.now();
    this.changed();
  }

  /**
   * Folds an imported list in. Existing entries win: an import is additive, so
   * running it twice, or importing a stale export, cannot rewrite a title the
   * reader has since fixed.
   */
  merge(incoming: Bookmark[]): { added: number; skipped: number } {
    let added = 0;
    let skipped = 0;
    for (const mark of incoming) {
      if (this.has(mark.url) || this.marks.length >= BOOKMARK_LIMIT) {
        skipped += 1;
        continue;
      }
      this.marks.push({ ...mark, id: newId() });
      added += 1;
    }
    if (added) this.changed();
    return { added, skipped };
  }

  /** Subscribes to changes. Returns the unsubscribe. */
  onChange(fn: () => void): () => void {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  }

  private changed(persist = true): void {
    if (persist) {
      void this.io.write(this.marks.slice()).catch((err: unknown) => {
        this.io.onError?.(`bookmarks could not be saved: ${String(err)}`);
      });
    }
    for (const fn of this.listeners) fn();
  }
}
