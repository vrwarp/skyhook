/**
 * Address-bar completion.
 *
 * Two things are worth pinning here. A highlighted suggestion wins over the
 * text underneath it: the shell's own Enter handler navigates to whatever is in
 * the field, and if both fired, a reader who arrowed down to a saved page and
 * pressed Enter would spend a round trip on their half-typed prefix instead.
 * And the X removes the row it is on without taking the reader out of the list
 * or opening the page they were trying to be rid of — the two ways this gesture
 * fails expensively.
 */
import { afterEach, describe, expect, it, vi } from 'vitest';

import { parseBookmarks, type Bookmark } from '../src/app/bookmarks.js';
import { completions, parseHistory, type Completion, type HistoryEntry } from '../src/app/history.js';
import { Suggest } from '../src/app/suggest.js';

const MARKS: Bookmark[] = parseBookmarks([
  { title: 'Hacker News', url: 'https://news.ycombinator.com', addedAt: 1 },
  { title: 'Flight status', url: 'https://example.com/flights', addedAt: 2 },
  { title: 'Hacker Newsletter', url: 'https://hackernewsletter.com', addedAt: 3 },
]);

const VISITS: HistoryEntry[] = parseHistory([
  { url: 'https://hackerspace.example/', title: 'Hackerspace', typed: 4, visits: 4, lastAt: 50 },
  { url: 'https://example.org/weather', title: 'Weather', typed: 0, visits: 9, lastAt: 60 },
]);

function mount(entries = VISITS): {
  input: HTMLInputElement;
  suggest: Suggest;
  pick: ReturnType<typeof vi.fn>;
  forget: ReturnType<typeof vi.fn>;
  live: HistoryEntry[];
} {
  const input = document.createElement('input');
  document.body.appendChild(input);
  const pick = vi.fn();
  const live = entries.slice();
  // The shell owns the removal — the widget only asks for it — so the fake
  // here has to actually do it, or the redraw would put the row straight back.
  const forget = vi.fn((row: Completion & { kind: 'history' }) => {
    const at = live.findIndex((e) => e.url === row.url);
    if (at >= 0) live.splice(at, 1);
  });
  const suggest = new Suggest(input, {
    source: (query, limit) => completions(MARKS, live, query, limit),
    pick,
    forget,
  });
  return { input, suggest, pick, forget, live };
}

function type(input: HTMLInputElement, value: string): void {
  input.value = value;
  input.dispatchEvent(new window.Event('input'));
}

function press(input: HTMLInputElement, key: string, init: KeyboardEventInit = {}): KeyboardEvent {
  const ev = new window.KeyboardEvent('keydown', { key, bubbles: true, cancelable: true, ...init });
  input.dispatchEvent(ev);
  return ev;
}

function rows(): string[] {
  return Array.from(document.querySelectorAll('.suggest-item .suggest-title'))
    .map((n) => n.textContent ?? '');
}

function pointerDown(node: Element): Event {
  const ev = new window.Event('pointerdown', { bubbles: true, cancelable: true });
  node.dispatchEvent(ev);
  return ev;
}

afterEach(() => {
  document.body.innerHTML = '';
});

describe('address-bar suggestions', () => {
  it('offers saved pages and visited ones together', () => {
    const { input, suggest } = mount();
    type(input, 'hacker');
    expect(suggest.isOpen()).toBe(true);
    // `hackernewsletter.com` and `hackerspace.example` are both addresses the
    // reader is partway through re-typing, which beats a title that merely
    // begins with the word; the saved one wins that tie, and Hacker News —
    // matched on its title, since its host is `news.ycombinator.com` — is last.
    expect(rows()).toEqual(['Hacker Newsletter', 'Hackerspace', 'Hacker News']);
  });

  it('stays shut on an empty field until it is asked for', () => {
    const { input, suggest } = mount();
    type(input, '');
    expect(suggest.isOpen()).toBe(false);
    // Arrow down is the ask: the recent list, without covering the page for
    // somebody who only clicked into the address bar.
    press(input, 'ArrowDown');
    expect(suggest.isOpen()).toBe(true);
    expect(rows()).toHaveLength(5);
  });

  it('closes rather than offering nothing', () => {
    const { input, suggest } = mount();
    type(input, 'nothing here matches this');
    expect(suggest.isOpen()).toBe(false);
  });

  it('walks the list and wraps', () => {
    const { input, suggest } = mount();
    type(input, 'hacker');
    press(input, 'ArrowDown');
    expect(suggest.highlighted()?.title).toBe('Hacker Newsletter');
    press(input, 'ArrowUp');
    expect(suggest.highlighted()?.title).toBe('Hacker News');
    press(input, 'ArrowDown');
    expect(suggest.highlighted()?.title).toBe('Hacker Newsletter');
  });

  it('takes Enter only when something is highlighted', () => {
    const { input, suggest, pick } = mount();
    type(input, 'hacker');

    // Nothing highlighted: the shell's own handler must still get the key, so
    // typing an address and pressing Enter navigates to what was typed.
    const passed = press(input, 'Enter');
    expect(passed.defaultPrevented).toBe(false);
    expect(pick).not.toHaveBeenCalled();
    expect(suggest.isOpen()).toBe(false);

    type(input, 'hacker');
    press(input, 'ArrowDown');
    const taken = press(input, 'Enter');
    expect(taken.defaultPrevented).toBe(true);
    expect(pick).toHaveBeenCalledWith(expect.objectContaining({ kind: 'saved', url: 'https://hackernewsletter.com' }));
    expect(input.value).toBe('https://hackernewsletter.com');
  });

  it('closes on Escape without letting the key travel on', () => {
    const { input, suggest } = mount();
    type(input, 'hacker');
    const ev = press(input, 'Escape');
    expect(suggest.isOpen()).toBe(false);
    expect(ev.defaultPrevented).toBe(true);
  });

  it('picks on a pointer down, before the field can blur', () => {
    const { input, pick } = mount();
    type(input, 'hacker');
    const ev = pointerDown(document.querySelector('.suggest-item') as HTMLElement);
    expect(ev.defaultPrevented).toBe(true);
    expect(pick).toHaveBeenCalledTimes(1);
  });

  it('leaves the typed text alone while the highlight moves', () => {
    const { input } = mount();
    type(input, 'hacker');
    press(input, 'ArrowDown');
    expect(input.value).toBe('hacker');
  });

  it('offers the X only on rows it is allowed to remove', () => {
    const { input } = mount();
    type(input, 'hacker');
    const items = Array.from(document.querySelectorAll('.suggest-item'));
    // Saved, visited, saved: only the middle one is history's to forget. The
    // other two say so with a ★ rather than leaving the gap unexplained.
    expect(items.map((n) => !!n.querySelector('.suggest-x'))).toEqual([false, true, false]);
    expect(items.map((n) => !!n.querySelector('.suggest-mark'))).toEqual([true, false, true]);
    expect(items[1].querySelector('.suggest-x')?.getAttribute('aria-label'))
      .toBe('Remove Hackerspace from history');
  });

  it('removes a row on the X without navigating to it or closing the list', () => {
    const { input, suggest, pick, forget } = mount();
    type(input, 'hacker');
    const ev = pointerDown(document.querySelectorAll('.suggest-item')[1].querySelector('.suggest-x') as Element);

    expect(ev.defaultPrevented).toBe(true);
    expect(forget).toHaveBeenCalledWith(expect.objectContaining({ url: 'https://hackerspace.example/' }));
    // The one thing this gesture must never do is open the page being removed.
    expect(pick).not.toHaveBeenCalled();
    expect(suggest.isOpen()).toBe(true);
    expect(rows()).toEqual(['Hacker Newsletter', 'Hacker News']);
  });

  it('closes when the X takes the last row with it', () => {
    const { input, suggest } = mount();
    type(input, 'weather');
    expect(rows()).toEqual(['Weather']);
    pointerDown(document.querySelector('.suggest-x') as Element);
    expect(suggest.isOpen()).toBe(false);
  });

  it('removes the highlighted row on Shift+Delete', () => {
    const { input, forget } = mount();
    type(input, 'hacker');
    press(input, 'ArrowDown');
    press(input, 'ArrowDown');
    expect(rows()[1]).toBe('Hackerspace');

    const ev = press(input, 'Delete', { shiftKey: true });
    expect(ev.defaultPrevented).toBe(true);
    expect(forget).toHaveBeenCalledTimes(1);
    expect(rows()).toEqual(['Hacker Newsletter', 'Hacker News']);
  });

  it('leaves a saved row alone on Shift+Delete', () => {
    const { input, forget } = mount();
    type(input, 'hacker');
    press(input, 'ArrowDown');
    const ev = press(input, 'Delete', { shiftKey: true });
    // Destroying a bookmark from a dropdown, where the undo cannot follow it,
    // is not what this chord means.
    expect(ev.defaultPrevented).toBe(false);
    expect(forget).not.toHaveBeenCalled();
    expect(rows()).toHaveLength(3);
  });

  it('tells assistive technology which row is active', () => {
    const { input } = mount();
    type(input, 'hacker');
    expect(input.getAttribute('aria-expanded')).toBe('true');
    expect(input.getAttribute('aria-activedescendant')).toBeNull();
    press(input, 'ArrowDown');
    const active = input.getAttribute('aria-activedescendant');
    expect(active).toBeTruthy();
    expect(document.getElementById(active as string)?.getAttribute('aria-selected')).toBe('true');
  });
});
