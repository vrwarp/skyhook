/**
 * The Skyhook context menu.
 *
 * A mirrored page is a sandboxed frame full of nodes the browser believes are
 * local and inert, so every entry on the native menu is subtly wrong: "Open
 * link in new tab" opens `about:blank`, "Reload" reloads a document with no
 * origin, "Save image" saves a file named after a content hash, and "Back" goes
 * back in the shell's history rather than the page's. Each of those has a
 * Skyhook equivalent that costs one frame over the link, and this is the widget
 * that offers them.
 *
 * It is deliberately one menu at a time, drawn into the shell's document: the
 * frame cannot host it (no script runs there) and would clip it anyway.
 */
import { isPhone } from './layout.js';

/** One row. `run` is called after the menu closes, so it can move focus. */
export interface MenuItem {
  label: string;
  /**
   * Right-aligned note about the entry itself — usually what it costs. Shown
   * on every device, because a phone reader is on the same link as everyone
   * else and "sends a screenshot" is the whole reason to hesitate.
   */
  hint?: string;
  /**
   * Right-aligned gesture that does the same thing: a keyboard chord, a middle
   * click. Hidden from a coarse pointer, which has neither — see layout.ts.
   */
  chord?: string;
  disabled?: boolean;
  run(): void;
}

/** Groups are drawn with a separator between them; empty ones are dropped. */
export type MenuGroups = MenuItem[][];

let open: HTMLElement | null = null;

/** True while a menu is on screen. */
export function menuIsOpen(): boolean {
  return open !== null;
}

export function closeMenu(): void {
  if (!open) return;
  open.remove();
  open = null;
  document.removeEventListener('pointerdown', onPointerDown, true);
  document.removeEventListener('keydown', onKeyDown, true);
  window.removeEventListener('resize', closeMenu);
  window.removeEventListener('blur', closeMenu);
}

/**
 * Opens a menu at a point in the shell's coordinate space. Returns the element
 * so tests (and only tests) can look at what was offered.
 */
export function showMenu(x: number, y: number, groups: MenuGroups): HTMLElement | null {
  closeMenu();
  const drawn = groups.filter((group) => group.length > 0);
  if (!drawn.length) return null;

  const menu = document.createElement('div');
  menu.className = 'menu';
  menu.setAttribute('role', 'menu');
  drawn.forEach((group, i) => {
    if (i > 0) {
      const rule = document.createElement('div');
      rule.className = 'sep';
      menu.appendChild(rule);
    }
    for (const item of group) menu.appendChild(row(item));
  });

  document.body.appendChild(menu);
  place(menu, x, y);
  open = menu;
  // Capture, so a click that lands on the mirror frame's own listeners still
  // dismisses the menu first.
  document.addEventListener('pointerdown', onPointerDown, true);
  document.addEventListener('keydown', onKeyDown, true);
  window.addEventListener('resize', closeMenu);
  window.addEventListener('blur', closeMenu);
  return menu;
}

function row(item: MenuItem): HTMLElement {
  const button = document.createElement('button');
  button.type = 'button';
  button.className = 'item';
  button.setAttribute('role', 'menuitem');
  button.disabled = item.disabled === true;

  const label = document.createElement('span');
  label.className = 'label';
  label.textContent = item.label;
  button.appendChild(label);

  if (item.hint) {
    const hint = document.createElement('span');
    hint.className = 'hint';
    hint.textContent = item.hint;
    button.appendChild(hint);
  }

  if (item.chord) {
    const chord = document.createElement('span');
    chord.className = 'hint chord';
    chord.textContent = item.chord;
    button.appendChild(chord);
  }

  button.addEventListener('click', () => {
    closeMenu();
    try {
      item.run();
    } catch (err) {
      console.warn('[skyhook] menu action failed', err);
    }
  });
  return button;
}

/**
 * Puts the menu where the gesture can be answered.
 *
 * On a wide screen that is next to the pointer, kept inside the window the way
 * a native menu is. On a phone it is the bottom of the screen, full width: the
 * point a long press reports is under the finger that made it, which is the one
 * place on the screen the reader cannot see, and half a menu drawn from there
 * runs off the edge. The sheet also puts the entries where the thumb already
 * is, which the top of a 6-inch screen is not.
 */
function place(menu: HTMLElement, x: number, y: number): void {
  if (isPhone()) {
    menu.classList.add('sheet');
    return;
  }
  const w = menu.offsetWidth || 220;
  const h = menu.offsetHeight || 0;
  const maxX = Math.max(0, window.innerWidth - w - 4);
  const maxY = Math.max(0, window.innerHeight - h - 4);
  menu.style.left = `${Math.max(0, Math.min(x, maxX))}px`;
  menu.style.top = `${Math.max(0, Math.min(y, maxY))}px`;
}

function onPointerDown(ev: Event): void {
  if (open && !open.contains(ev.target as Node)) closeMenu();
}

function onKeyDown(ev: KeyboardEvent): void {
  if (!open) return;
  if (ev.key === 'Escape') {
    ev.preventDefault();
    closeMenu();
    return;
  }
  if (ev.key !== 'ArrowDown' && ev.key !== 'ArrowUp') return;
  // Nothing is focused when the menu opens: right-clicking a text field must
  // not blur it, because blur is a semantic event the server acts on.
  const items = Array.from(open.querySelectorAll('button:not([disabled])')) as HTMLElement[];
  if (!items.length) return;
  ev.preventDefault();
  const step = ev.key === 'ArrowDown' ? 1 : -1;
  const at = items.indexOf(document.activeElement as HTMLElement);
  const next = at < 0
    ? (step > 0 ? 0 : items.length - 1)
    : (at + step + items.length) % items.length;
  items[next].focus();
}
