/**
 * Address-bar completion.
 *
 * The thing worth pinning here is that a highlighted suggestion wins over the
 * text underneath it: the shell's own Enter handler navigates to whatever is in
 * the field, and if both fired, a reader who arrowed down to a saved page and
 * pressed Enter would spend a round trip on their half-typed prefix instead.
 */
import { afterEach, describe, expect, it, vi } from 'vitest';

import { parseBookmarks, search, type Bookmark } from '../src/app/bookmarks.js';
import { Suggest } from '../src/app/suggest.js';

const MARKS: Bookmark[] = parseBookmarks([
  { title: 'Hacker News', url: 'https://news.ycombinator.com', addedAt: 1 },
  { title: 'Flight status', url: 'https://example.com/flights', addedAt: 2 },
  { title: 'Hacker Newsletter', url: 'https://hackernewsletter.com', addedAt: 3 },
]);

function mount(): { input: HTMLInputElement; suggest: Suggest; pick: ReturnType<typeof vi.fn> } {
  const input = document.createElement('input');
  document.body.appendChild(input);
  const pick = vi.fn();
  const suggest = new Suggest(input, {
    source: (query, limit) => search(MARKS, query, limit),
    pick,
  });
  return { input, suggest, pick };
}

function type(input: HTMLInputElement, value: string): void {
  input.value = value;
  input.dispatchEvent(new window.Event('input'));
}

function press(input: HTMLInputElement, key: string): KeyboardEvent {
  const ev = new window.KeyboardEvent('keydown', { key, bubbles: true, cancelable: true });
  input.dispatchEvent(ev);
  return ev;
}

function rows(): string[] {
  return Array.from(document.querySelectorAll('.suggest-item .suggest-title'))
    .map((n) => n.textContent ?? '');
}

afterEach(() => {
  document.body.innerHTML = '';
});

describe('address-bar suggestions', () => {
  it('offers saved pages that match what has been typed', () => {
    const { input, suggest } = mount();
    type(input, 'hacker');
    expect(suggest.isOpen()).toBe(true);
    expect(rows()).toEqual(['Hacker Newsletter', 'Hacker News']);
  });

  it('stays shut on an empty field until it is asked for', () => {
    const { input, suggest } = mount();
    type(input, '');
    expect(suggest.isOpen()).toBe(false);
    // Arrow down is the ask: the recent list, without covering the page for
    // somebody who only clicked into the address bar.
    press(input, 'ArrowDown');
    expect(suggest.isOpen()).toBe(true);
    expect(rows()).toHaveLength(3);
  });

  it('closes rather than offering nothing', () => {
    const { input, suggest } = mount();
    type(input, 'nothing saved matches this');
    expect(suggest.isOpen()).toBe(false);
  });

  it('walks the list and wraps', () => {
    const { input, suggest } = mount();
    type(input, 'hacker');
    press(input, 'ArrowDown');
    expect(suggest.highlighted()?.title).toBe('Hacker Newsletter');
    press(input, 'ArrowDown');
    expect(suggest.highlighted()?.title).toBe('Hacker News');
    press(input, 'ArrowDown');
    expect(suggest.highlighted()?.title).toBe('Hacker Newsletter');
    press(input, 'ArrowUp');
    expect(suggest.highlighted()?.title).toBe('Hacker News');
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
    expect(pick).toHaveBeenCalledWith(MARKS[2]);
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
    const row = document.querySelector('.suggest-item') as HTMLElement;
    const ev = new window.Event('pointerdown', { bubbles: true, cancelable: true });
    row.dispatchEvent(ev);
    expect(ev.defaultPrevented).toBe(true);
    expect(pick).toHaveBeenCalledWith(MARKS[2]);
  });

  it('leaves the typed text alone while the highlight moves', () => {
    const { input } = mount();
    type(input, 'hacker');
    press(input, 'ArrowDown');
    expect(input.value).toBe('hacker');
  });
});
