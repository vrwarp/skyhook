/**
 * The two places saved pages are shown: the side panel and the start page a
 * tab shows before it has been anywhere.
 *
 * Both are built here rather than in the shell because both draw the same row,
 * and a bookmark row carries more behaviour than it looks like it does — open
 * here, open in a background tab on a middle click, its own context menu, and
 * an inline rename that must not lose the reader's place in the list. Two
 * copies of that would drift.
 *
 * Neither view touches the network. That is the point of the feature: the list
 * renders during an outage exactly as it does on a good link, and the only
 * thing an outage takes away is the round trip at the end.
 */

import { displayHost, search, type Bookmark } from './bookmarks.js';
import { isTouch } from './layout.js';

/** Everything a row can ask the shell to do. */
export interface BookmarkActions {
  /** Open it. `newTab` is the middle-click gesture: background, page kept. */
  open(mark: Bookmark, where: 'here' | 'newTab'): void;
  remove(mark: Bookmark): void;
  rename(mark: Bookmark, title: string): void;
  /** A right click on a row; the shell draws its own menu at the point. */
  menu(mark: Bookmark, x: number, y: number): void;
}

/** What the panel's footer offers beyond the list itself. */
export interface BookmarkPanelActions extends BookmarkActions {
  exportAll(): void;
  importFrom(): void;
}

const OFFLINE_NOTE = 'Offline. The list is on this device, but opening a page needs the link.';

/**
 * Builds one row. Shared by both views so the gestures cannot diverge; the
 * caller decides what the row looks like by handing over a class name.
 */
function bookmarkRow(
  mark: Bookmark,
  className: string,
  actions: BookmarkActions,
  editing: { id: string | null; end(): void },
): HTMLElement {
  const row = document.createElement('div');
  row.className = className;
  row.dataset.id = mark.id;

  if (editing.id === mark.id) {
    row.appendChild(renameField(mark, actions, editing));
    return row;
  }

  row.setAttribute('role', 'button');
  row.tabIndex = 0;

  const title = document.createElement('span');
  title.className = 'mark-title';
  title.textContent = mark.title;
  row.appendChild(title);

  const host = document.createElement('span');
  host.className = 'mark-host';
  host.textContent = displayHost(mark.url) || mark.url;
  row.appendChild(host);

  row.addEventListener('click', (ev) => {
    if ((ev.target as HTMLElement | null)?.closest('button')) return;
    actions.open(mark, 'here');
  });
  // The gesture this shell already teaches on every link in a mirrored page:
  // a middle click opens in the background, leaving the page already paid for
  // on screen.
  row.addEventListener('auxclick', (ev) => {
    if ((ev as MouseEvent).button !== 1) return;
    ev.preventDefault();
    actions.open(mark, 'newTab');
  });
  row.addEventListener('keydown', (ev) => {
    if (ev.key !== 'Enter' && ev.key !== ' ') return;
    ev.preventDefault();
    actions.open(mark, ev.ctrlKey || ev.metaKey ? 'newTab' : 'here');
  });
  row.addEventListener('contextmenu', (ev) => {
    ev.preventDefault();
    ev.stopPropagation();
    actions.menu(mark, ev.clientX, ev.clientY);
  });
  return row;
}

/**
 * The inline rename. A bookmark made from a link carries the anchor's text,
 * which is how a list ends up with three entries called "more" — so renaming
 * has to be one gesture away, and it has to happen in place: a dialog would
 * lose the reader's scroll position in a list they are halfway through reading.
 */
function renameField(
  mark: Bookmark,
  actions: BookmarkActions,
  editing: { id: string | null; end(): void },
): HTMLInputElement {
  const input = document.createElement('input');
  input.className = 'mark-rename';
  input.value = mark.title;
  input.setAttribute('aria-label', `Rename ${mark.title}`);
  let done = false;
  const commit = (save: boolean): void => {
    if (done) return;
    done = true;
    if (save) actions.rename(mark, input.value);
    editing.end();
  };
  input.addEventListener('keydown', (ev) => {
    if (ev.key === 'Enter') {
      ev.preventDefault();
      commit(true);
    } else if (ev.key === 'Escape') {
      ev.preventDefault();
      commit(false);
    }
  });
  // Blur commits rather than discards: a reader who types a name and clicks
  // away has said what they want it called.
  input.addEventListener('blur', () => commit(true));
  return input;
}

/**
 * The bookmarks side panel: search, the list, and the two buttons that get the
 * list off this device.
 *
 * Built once and kept, rather than re-created per render, so that typing in the
 * search box does not rebuild the field under the caret.
 */
export class BookmarkPanel {
  readonly root: HTMLElement;
  private list: HTMLElement;
  private note: HTMLParagraphElement;
  private foot: HTMLElement;
  private searchField: HTMLInputElement;
  private marks: Bookmark[] = [];
  private online = true;
  private editing: string | null = null;

  constructor(private actions: BookmarkPanelActions) {
    this.root = document.createElement('div');
    this.root.className = 'marks';

    this.searchField = document.createElement('input');
    this.searchField.className = 'marks-search';
    this.searchField.type = 'search';
    this.searchField.placeholder = 'Search saved pages';
    this.searchField.setAttribute('aria-label', 'Search saved pages');
    this.searchField.addEventListener('input', () => this.draw());
    this.searchField.addEventListener('keydown', (ev) => {
      if (ev.key !== 'Escape' || !this.searchField.value) return;
      // Escape clears the filter before it closes the panel: a reader who has
      // typed themselves into an empty list wants the list back, not the panel
      // shut.
      ev.stopPropagation();
      this.searchField.value = '';
      this.draw();
    });
    this.root.appendChild(this.searchField);

    this.note = document.createElement('p');
    this.note.className = 'marks-note';
    this.note.hidden = true;
    this.root.appendChild(this.note);

    this.list = document.createElement('div');
    this.list.className = 'marks-list';
    this.root.appendChild(this.list);

    this.foot = document.createElement('div');
    this.foot.className = 'marks-foot';
    this.root.appendChild(this.foot);
  }

  /** Puts the caret in the search box, for the keyboard path into the panel. */
  focusSearch(): void {
    this.searchField.focus();
    this.searchField.select();
  }

  render(marks: Bookmark[], online: boolean): void {
    this.marks = marks;
    this.online = online;
    // A rename in flight on an entry that has just been removed elsewhere has
    // nothing left to rename.
    if (this.editing && !marks.some((m) => m.id === this.editing)) this.editing = null;
    this.draw();
  }

  /** Opens the inline rename on an entry, from the row's own context menu. */
  beginRename(id: string): void {
    this.editing = id;
    this.draw();
    (this.list.querySelector('.mark-rename') as HTMLInputElement | null)?.focus();
  }

  private draw(): void {
    const query = this.searchField.value;
    const shown = search(this.marks, query);
    this.note.hidden = this.online;
    if (!this.online) this.note.textContent = OFFLINE_NOTE;

    this.list.textContent = '';
    const editing = { id: this.editing, end: (): void => { this.editing = null; this.draw(); } };
    for (const mark of shown) {
      this.list.appendChild(bookmarkRow(mark, 'mark', this.rowActions(), editing));
    }
    if (!shown.length) this.list.appendChild(this.empty(query));

    this.drawFoot(shown.length);
  }

  /**
   * Removal from the panel goes through the shell, which offers the undo. The
   * row's own delete button is deliberately not a confirmation dialog: a
   * confirmation on every removal costs more attention over a list than one
   * undo on the rare mistake.
   */
  private rowActions(): BookmarkActions {
    return {
      open: (mark, where) => this.actions.open(mark, where),
      remove: (mark) => this.actions.remove(mark),
      rename: (mark, title) => this.actions.rename(mark, title),
      menu: (mark, x, y) => this.actions.menu(mark, x, y),
    };
  }

  private empty(query: string): HTMLElement {
    const box = document.createElement('p');
    box.className = 'marks-empty';
    box.textContent = query.trim()
      ? `Nothing saved matches “${query.trim()}”.`
      : 'Nothing saved yet. Press ★ in the toolbar to keep the page you are on.';
    return box;
  }

  private drawFoot(shownCount: number): void {
    this.foot.textContent = '';
    const count = document.createElement('span');
    count.className = 'marks-count';
    const total = this.marks.length;
    count.textContent = shownCount === total
      ? `${total} saved`
      : `${shownCount} of ${total}`;
    this.foot.appendChild(count);

    for (const [label, run] of [
      ['Export', (): void => this.actions.exportAll()],
      ['Import', (): void => this.actions.importFrom()],
    ] as [string, () => void][]) {
      const button = document.createElement('button');
      button.type = 'button';
      button.className = 'marks-act';
      button.textContent = label;
      button.addEventListener('click', run);
      this.foot.appendChild(button);
    }
  }
}

/**
 * The start page: what a tab shows before it has been anywhere.
 *
 * A new tab used to be a blank white frame — landside `about:blank` faithfully
 * mirrored — which left the address bar as the only way forward. Typing a whole
 * address is the most expensive way to navigate on this link and the one most
 * likely to be spent on a typo, so the tab with nothing in it is exactly where
 * the free list belongs.
 *
 * It is drawn by the shell over the frame area, not inside the mirror frame:
 * no script runs there, and a document the server did not send has no business
 * being in a mirror.
 */
export class StartPage {
  readonly root: HTMLElement;
  private grid: HTMLElement;
  private note: HTMLParagraphElement;

  constructor(private actions: BookmarkActions) {
    this.root = document.createElement('div');
    this.root.id = 'start';
    this.root.hidden = true;

    const inner = document.createElement('div');
    inner.className = 'start-inner';
    this.root.appendChild(inner);

    const heading = document.createElement('h1');
    heading.textContent = 'Saved pages';
    inner.appendChild(heading);

    this.note = document.createElement('p');
    this.note.className = 'start-note';
    inner.appendChild(this.note);

    this.grid = document.createElement('div');
    this.grid.className = 'start-grid';
    inner.appendChild(this.grid);
  }

  /** `show` is false for a tab that already has a page: never cover one. */
  render(marks: Bookmark[], opts: { show: boolean; online: boolean }): void {
    this.root.hidden = !opts.show;
    if (!opts.show) return;

    this.grid.textContent = '';
    const shown = search(marks, '', 24);
    const editing = { id: null, end: (): void => undefined };
    for (const mark of shown) {
      this.grid.appendChild(bookmarkRow(mark, 'start-mark', this.actions, editing));
    }

    if (!marks.length) {
      this.note.textContent = 'Nothing saved yet. Press ★ in the toolbar to keep a page: '
        + 'the list lives on this device, opens instantly, and is readable during an outage.';
      return;
    }
    if (!opts.online) {
      this.note.textContent = OFFLINE_NOTE;
      return;
    }
    // The second sentence names a gesture, so it has to name one the reader
    // has. Told to press a middle button it does not have, a phone stops
    // believing the rest of the sentence too — and the rest of the sentence is
    // the only place this app says what a page costs.
    this.note.textContent = isTouch()
      ? 'Opening one of these costs a single round trip. Touch and hold for more.'
      : 'Opening one of these costs a single round trip. Middle-click for a background tab.';
  }
}
