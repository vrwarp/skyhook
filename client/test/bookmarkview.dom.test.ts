/**
 * The two views on the saved list.
 *
 * What is tested here is mostly what the old feature had none of: that a saved
 * page can be found again, that the gestures on a row are the ones the rest of
 * this shell teaches, and that an outage changes what the views say rather than
 * whether they work.
 */
import { afterEach, describe, expect, it, vi } from 'vitest';

import { parseBookmarks, type Bookmark } from '../src/app/bookmarks.js';
import { BookmarkPanel, StartPage, type BookmarkPanelActions } from '../src/app/bookmarkview.js';

const MARKS: Bookmark[] = parseBookmarks([
  { title: 'Hacker News', url: 'https://news.ycombinator.com', addedAt: 1 },
  { title: 'Flight status', url: 'https://example.com/flights', addedAt: 2 },
  { title: 'more', url: 'https://example.com/story/1', addedAt: 3 },
]);

function actions() {
  const acts = {
    open: vi.fn<(mark: Bookmark, where: 'here' | 'newTab') => void>(),
    remove: vi.fn<(mark: Bookmark) => void>(),
    rename: vi.fn<(mark: Bookmark, title: string) => void>(),
    menu: vi.fn<(mark: Bookmark, x: number, y: number) => void>(),
    exportAll: vi.fn<() => void>(),
    importFrom: vi.fn<() => void>(),
  };
  // Fails to compile if the view ever asks for something not offered here.
  return acts satisfies BookmarkPanelActions;
}

function titles(root: HTMLElement, selector: string): string[] {
  return Array.from(root.querySelectorAll(`${selector} .mark-title`)).map((n) => n.textContent ?? '');
}

afterEach(() => {
  document.body.innerHTML = '';
});

describe('the saved-pages panel', () => {
  it('lists most recent first, with where each one goes', () => {
    const acts = actions();
    const panel = new BookmarkPanel(acts);
    document.body.appendChild(panel.root);
    panel.render(MARKS, true);

    expect(titles(panel.root, '.mark')).toEqual(['more', 'Flight status', 'Hacker News']);
    expect(panel.root.querySelector('.mark .mark-host')?.textContent).toBe('example.com');
    expect(panel.root.querySelector('.marks-count')?.textContent).toBe('3 saved');
  });

  it('filters as the reader types, and counts what is showing', () => {
    const panel = new BookmarkPanel(actions());
    document.body.appendChild(panel.root);
    panel.render(MARKS, true);

    const field = panel.root.querySelector('.marks-search') as HTMLInputElement;
    field.value = 'flight';
    field.dispatchEvent(new window.Event('input'));
    expect(titles(panel.root, '.mark')).toEqual(['Flight status']);
    expect(panel.root.querySelector('.marks-count')?.textContent).toBe('1 of 3');

    field.value = 'nothing like this';
    field.dispatchEvent(new window.Event('input'));
    expect(panel.root.querySelector('.marks-empty')?.textContent).toContain('nothing like this');
  });

  it('says what is empty, and how to fill it', () => {
    const panel = new BookmarkPanel(actions());
    panel.render([], true);
    expect(panel.root.querySelector('.marks-empty')?.textContent).toContain('★');
  });

  it('opens here on a click and in the background on a middle click', () => {
    const acts = actions();
    const panel = new BookmarkPanel(acts);
    document.body.appendChild(panel.root);
    panel.render(MARKS, true);
    const row = panel.root.querySelector('.mark') as HTMLElement;

    row.dispatchEvent(new window.MouseEvent('click', { bubbles: true }));
    expect(acts.open).toHaveBeenCalledWith(MARKS[2], 'here');

    row.dispatchEvent(new window.MouseEvent('auxclick', { button: 1, bubbles: true }));
    expect(acts.open).toHaveBeenLastCalledWith(MARKS[2], 'newTab');
  });

  it('is reachable from the keyboard', () => {
    const acts = actions();
    const panel = new BookmarkPanel(acts);
    document.body.appendChild(panel.root);
    panel.render(MARKS, true);
    const row = panel.root.querySelector('.mark') as HTMLElement;
    expect(row.tabIndex).toBe(0);

    row.dispatchEvent(new window.KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    expect(acts.open).toHaveBeenCalledWith(MARKS[2], 'here');
  });

  it('hands a right click to the shell rather than the browser', () => {
    const acts = actions();
    const panel = new BookmarkPanel(acts);
    document.body.appendChild(panel.root);
    panel.render(MARKS, true);
    const row = panel.root.querySelector('.mark') as HTMLElement;
    const ev = new window.MouseEvent('contextmenu', { bubbles: true, cancelable: true });
    row.dispatchEvent(ev);
    expect(ev.defaultPrevented).toBe(true);
    expect(acts.menu).toHaveBeenCalledWith(MARKS[2], 0, 0);
  });

  it('renames in place, keeping the reader where they were', () => {
    const acts = actions();
    const panel = new BookmarkPanel(acts);
    document.body.appendChild(panel.root);
    panel.render(MARKS, true);

    panel.beginRename(MARKS[2].id);
    const field = panel.root.querySelector('.mark-rename') as HTMLInputElement;
    expect(field.value).toBe('more');
    field.value = 'Flight tracker thread';
    field.dispatchEvent(new window.KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    expect(acts.rename).toHaveBeenCalledWith(MARKS[2], 'Flight tracker thread');
    // Back to a normal row, with the list still under it.
    expect(panel.root.querySelector('.mark-rename')).toBeNull();
    expect(panel.root.querySelectorAll('.mark')).toHaveLength(3);
  });

  it('abandons a rename on Escape', () => {
    const acts = actions();
    const panel = new BookmarkPanel(acts);
    document.body.appendChild(panel.root);
    panel.render(MARKS, true);
    panel.beginRename(MARKS[0].id);
    const field = panel.root.querySelector('.mark-rename') as HTMLInputElement;
    field.value = 'discarded';
    field.dispatchEvent(new window.KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    expect(acts.rename).not.toHaveBeenCalled();
    expect(panel.root.querySelector('.mark-rename')).toBeNull();
  });

  it('stays usable offline, and says what an outage takes away', () => {
    const panel = new BookmarkPanel(actions());
    panel.render(MARKS, false);
    const note = panel.root.querySelector('.marks-note') as HTMLElement;
    expect(note.hidden).toBe(false);
    expect(note.textContent).toContain('opening a page needs the link');
    // The list itself is local, so it is all still there to read and search.
    expect(panel.root.querySelectorAll('.mark')).toHaveLength(3);

    panel.render(MARKS, true);
    expect((panel.root.querySelector('.marks-note') as HTMLElement).hidden).toBe(true);
  });

  it('offers the way out of the box', () => {
    const acts = actions();
    const panel = new BookmarkPanel(acts);
    panel.render(MARKS, true);
    const buttons = Array.from(panel.root.querySelectorAll('.marks-act')) as HTMLElement[];
    expect(buttons.map((b) => b.textContent)).toEqual(['Export', 'Import']);
    buttons[0].click();
    buttons[1].click();
    expect(acts.exportAll).toHaveBeenCalled();
    expect(acts.importFrom).toHaveBeenCalled();
  });
});

describe('the start page', () => {
  it('shows saved pages where a tab would otherwise be blank', () => {
    const acts = actions();
    const start = new StartPage(acts);
    document.body.appendChild(start.root);
    start.render(MARKS, { show: true, online: true });

    expect(start.root.hidden).toBe(false);
    expect(titles(start.root, '.start-mark')).toEqual(['more', 'Flight status', 'Hacker News']);
    (start.root.querySelector('.start-mark') as HTMLElement)
      .dispatchEvent(new window.MouseEvent('click', { bubbles: true }));
    expect(acts.open).toHaveBeenCalledWith(MARKS[2], 'here');
  });

  it('never covers a page that has been paid for', () => {
    const start = new StartPage(actions());
    start.render(MARKS, { show: false, online: true });
    expect(start.root.hidden).toBe(true);
  });

  it('explains itself when there is nothing saved yet', () => {
    const start = new StartPage(actions());
    start.render([], { show: true, online: true });
    expect(start.root.querySelector('.start-note')?.textContent).toContain('★');
    expect(start.root.querySelectorAll('.start-mark')).toHaveLength(0);
  });

  it('tells the truth about an outage', () => {
    const start = new StartPage(actions());
    start.render(MARKS, { show: true, online: false });
    expect(start.root.querySelector('.start-note')?.textContent).toContain('Offline');
    expect(start.root.querySelectorAll('.start-mark')).toHaveLength(3);
  });
});
