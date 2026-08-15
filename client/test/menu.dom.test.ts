/**
 * Context menu tests.
 *
 * The menu is the only chrome that appears over the mirror, and it is the
 * replacement for a native menu the user already knows how to dismiss — so the
 * dismissal paths matter as much as the entries.
 */
import { afterEach, describe, expect, it, vi } from 'vitest';

import { closeMenu, menuIsOpen, showMenu } from '../src/app/menu.js';
import { PHONE } from '../src/app/layout.js';

function labels(menu: HTMLElement): string[] {
  return Array.from(menu.querySelectorAll('.item .label')).map((n) => n.textContent ?? '');
}

/** jsdom has no matchMedia, so the shell's one question about the screen is
 *  answered here rather than left to throw. */
function matchPhone(phone: boolean): void {
  window.matchMedia = ((query: string) => ({
    matches: query === PHONE && phone,
    media: query,
    addEventListener: () => undefined,
    removeEventListener: () => undefined,
  })) as unknown as typeof window.matchMedia;
}

describe('context menu', () => {
  afterEach(() => {
    closeMenu();
    document.body.innerHTML = '';
    matchPhone(false);
  });

  it('draws groups with a separator between them', () => {
    const menu = showMenu(10, 20, [
      [{ label: 'Open link in new tab', run: () => undefined }],
      [],
      [{ label: 'Reload', run: () => undefined }, { label: 'Back', run: () => undefined }],
    ]);
    expect(menu).not.toBeNull();
    expect(labels(menu!)).toEqual(['Open link in new tab', 'Reload', 'Back']);
    // An empty group must not leave a rule behind.
    expect(menu!.querySelectorAll('.sep')).toHaveLength(1);
    expect(menu!.style.left).toBe('10px');
    expect(menu!.style.top).toBe('20px');
  });

  it('runs an entry once and closes', () => {
    const run = vi.fn();
    const menu = showMenu(0, 0, [[{ label: 'Copy link address', run }]]);
    (menu!.querySelector('.item') as HTMLElement).click();
    expect(run).toHaveBeenCalledTimes(1);
    expect(menuIsOpen()).toBe(false);
    expect(document.querySelector('.menu')).toBeNull();
  });

  it('does not run a disabled entry', () => {
    const run = vi.fn();
    const menu = showMenu(0, 0, [[{ label: 'Back', disabled: true, run }]]);
    (menu!.querySelector('.item') as HTMLElement).click();
    expect(run).not.toHaveBeenCalled();
  });

  it('shows nothing when every group is empty', () => {
    expect(showMenu(0, 0, [[], []])).toBeNull();
    expect(menuIsOpen()).toBe(false);
  });

  it('closes on Escape and on a click elsewhere', () => {
    showMenu(0, 0, [[{ label: 'Reload', run: () => undefined }]]);
    document.dispatchEvent(new window.KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    expect(menuIsOpen()).toBe(false);

    showMenu(0, 0, [[{ label: 'Reload', run: () => undefined }]]);
    document.body.dispatchEvent(new window.Event('pointerdown', { bubbles: true }));
    expect(menuIsOpen()).toBe(false);
  });

  it('replaces a previous menu rather than stacking one on top', () => {
    showMenu(0, 0, [[{ label: 'Reload', run: () => undefined }]]);
    showMenu(5, 5, [[{ label: 'Close tab', run: () => undefined }]]);
    expect(document.querySelectorAll('.menu')).toHaveLength(1);
    expect(labels(document.querySelector('.menu') as HTMLElement)).toEqual(['Close tab']);
  });

  it('opens without stealing focus, and takes it only on arrow keys', () => {
    const field = document.createElement('input');
    document.body.appendChild(field);
    field.focus();

    const menu = showMenu(0, 0, [[
      { label: 'Cut', disabled: true, run: () => undefined },
      { label: 'Paste', run: () => undefined },
    ]]);
    // Focus must stay put: a blur inside the mirror is a semantic event the
    // server acts on, and right-clicking a field is not a request to leave it.
    expect(document.activeElement).toBe(field);

    document.dispatchEvent(new window.KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true }));
    expect(document.activeElement).toBe(menu!.querySelectorAll('.item')[1]);
  });

  it('separates what an entry costs from the gesture that does the same thing', () => {
    const menu = showMenu(0, 0, [[{
      label: 'Report a rendering problem…',
      hint: 'sends a screenshot',
      chord: 'Ctrl+Shift+D',
      run: () => undefined,
    }]]);
    // Both are drawn; the stylesheet is what hides a chord from a device that
    // has no keys to press. A cost is shown everywhere, because a phone reader
    // is on the same link as everyone else.
    const notes = Array.from(menu!.querySelectorAll('.hint')).map((n) => n.textContent);
    expect(notes).toEqual(['sends a screenshot', 'Ctrl+Shift+D']);
    expect(menu!.querySelectorAll('.hint.chord')).toHaveLength(1);
  });

  it('comes up from the bottom of a phone rather than at the point pressed', () => {
    // The point a long press reports is under the finger that made it, which is
    // the one part of the screen the reader cannot see.
    matchPhone(true);
    const menu = showMenu(180, 500, [[{ label: 'Reload', run: () => undefined }]]);
    expect(menu!.classList.contains('sheet')).toBe(true);
    // Placed by the stylesheet, not by coordinates that would fight it.
    expect(menu!.style.left).toBe('');
    expect(menu!.style.top).toBe('');
  });
});
