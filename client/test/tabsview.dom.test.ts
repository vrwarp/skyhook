/**
 * The tab list: what a phone gets instead of a strip.
 *
 * The strip's failure on a narrow screen was not cosmetic — past three tabs the
 * "+" scrolled off the end and there was no way to open one at all — so what is
 * tested here is that every gesture the strip has survives the move: switching,
 * closing, the menu, and opening a tab when the list is the only place left
 * offering it.
 */
import { describe, expect, it, vi } from 'vitest';

import { TabList, type TabListActions } from '../src/app/tabsview.js';

function actions() {
  const acts = {
    select: vi.fn<(id: number) => void>(),
    close: vi.fn<(id: number) => void>(),
    menu: vi.fn<(id: number, x: number, y: number) => void>(),
    open: vi.fn<() => void>(),
  };
  // Fails to compile if the view ever asks for something not offered here.
  return acts satisfies TabListActions;
}

const TABS = [
  { id: 1, title: 'Hacker News', url: 'https://news.ycombinator.com/', loading: false },
  { id: 2, title: '', url: 'https://example.com/story', loading: true },
];

function rows(list: TabList): HTMLElement[] {
  return Array.from(list.root.querySelectorAll('.tabrow'));
}

describe('tab list', () => {
  it('draws a row per tab, with the host under the title', () => {
    const list = new TabList(actions());
    list.render(TABS, [], 1, true);
    const drawn = rows(list);
    expect(drawn).toHaveLength(2);
    expect(drawn[0].querySelector('.tabrow-title')?.textContent).toBe('Hacker News');
    expect(drawn[0].querySelector('.tabrow-host')?.textContent).toBe('news.ycombinator.com');
    // A tab with no title yet is named by where it is, not left blank.
    expect(drawn[1].querySelector('.tabrow-title')?.textContent).toBe('example.com');
  });

  it('marks the tab on screen, for a list of pages from the same site', () => {
    const list = new TabList(actions());
    list.render(TABS, [], 2, true);
    const drawn = rows(list);
    expect(drawn[0].classList.contains('on')).toBe(false);
    expect(drawn[1].classList.contains('on')).toBe(true);
    expect(drawn[1].getAttribute('aria-selected')).toBe('true');
  });

  it('spins for a tab still waiting for its page', () => {
    const list = new TabList(actions());
    list.render(TABS, [], 1, true);
    expect(rows(list)[0].querySelector('.spin')).toBeNull();
    expect(rows(list)[1].querySelector('.spin')).not.toBeNull();
  });

  it('switches on a tap and closes on the close button, never both', () => {
    const acts = actions();
    const list = new TabList(acts);
    list.render(TABS, [], 1, true);
    (rows(list)[1].querySelector('.tabrow-close') as HTMLElement).click();
    expect(acts.close).toHaveBeenCalledWith(2);
    expect(acts.select).not.toHaveBeenCalled();

    rows(list)[0].click();
    expect(acts.select).toHaveBeenCalledWith(1);
  });

  it('answers a long press with the tab menu', () => {
    const acts = actions();
    const list = new TabList(acts);
    list.render(TABS, [], 1, true);
    rows(list)[0].dispatchEvent(new MouseEvent('contextmenu', { bubbles: true, cancelable: true }));
    expect(acts.menu).toHaveBeenCalledWith(1, expect.any(Number), expect.any(Number));
  });

  it('shows a tab asked for that the server has not opened yet', () => {
    const list = new TabList(actions());
    list.render(TABS, [{ url: 'https://example.org/late' }], 1, true);
    const ghost = rows(list)[2];
    expect(ghost.classList.contains('ghost')).toBe(true);
    expect(ghost.querySelector('.tabrow-title')?.textContent).toBe('example.org');
    expect(ghost.querySelector('.spin')).not.toBeNull();
    // Nothing to close and nothing to switch to: it does not exist yet.
    expect(ghost.querySelector('.tabrow-close')).toBeNull();
  });

  it('offers a new tab, and refuses one during an outage', () => {
    const acts = actions();
    const list = new TabList(acts);
    list.render(TABS, [], 1, true);
    const add = list.root.querySelector('.tablist-new') as HTMLButtonElement;
    expect(add.disabled).toBe(false);
    add.click();
    expect(acts.open).toHaveBeenCalledTimes(1);

    // Opening a tab is a request to the server, so offline it would do nothing
    // at all. Better to look unavailable than to look broken.
    list.render(TABS, [], 1, false);
    expect((list.root.querySelector('.tablist-new') as HTMLButtonElement).disabled).toBe(true);
  });

  it('says so rather than showing nothing when there are no tabs', () => {
    const list = new TabList(actions());
    list.render([], [], 0, true);
    expect(list.root.textContent).toContain('No tabs open');
  });
});
