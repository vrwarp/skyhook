/**
 * Address-bar completion, from the saved list and from where the reader has
 * actually been.
 *
 * On a link where a page costs seconds, a mistyped address is the most
 * expensive mistake the chrome can let a reader make: the round trip is spent
 * before anyone finds out it was wrong. Completing costs nothing, is local, and
 * works during an outage — the reader can line up where to go while the link is
 * down and press Enter when it comes back.
 *
 * This file used to say it was deliberately not history-backed, on the grounds
 * that there was no history store. There is one now (`history.ts`), and the
 * reason it was worth building is that the address people re-type is almost
 * never one they starred. What that note was really protecting survives intact:
 * matching is substring and never fuzzy, and nothing is ever written into the
 * field on the reader's behalf, because a completion that is nearly right costs
 * a round trip to find out.
 *
 * A history row carries an X, because a list built from behaviour rather than
 * from choice will contain things the reader does not want offered back — and
 * on a six-row dropdown one bad row is a sixth of the surface. A saved row
 * carries a ★ in the same slot instead: it says where the row came from, and it
 * answers the question the missing X would otherwise raise. Removing a bookmark
 * stays with the star and the panel, which are where its undo lives.
 *
 * The widget is a sibling of the menu (`menu.ts`) in spirit: drawn by the
 * shell, one at a time, it never takes focus away from the field it serves, and
 * it decides nothing — the shell owns what a pick and a removal actually do.
 */

import { displayHost } from './bookmarks.js';
import type { Completion } from './history.js';
import { isTouch } from './layout.js';

const LIMIT = 6;

export interface SuggestOptions {
  /** Candidates for what has been typed. Empty query means "the recent ones". */
  source(query: string, limit: number): Completion[];
  /** The reader chose one. The field has already been filled in. */
  pick(row: Completion): void;
  /**
   * The reader asked for a history row never to be offered again. The shell
   * does the removal, so that it happens with the same notice-and-undo every
   * other destructive gesture in this app has.
   */
  forget(row: Completion & { kind: 'history' }): void;
}

let boxes = 0;

export class Suggest {
  private box: HTMLElement;
  private shown: Completion[] = [];
  private at = -1;
  private readonly idBase: string;

  constructor(private input: HTMLInputElement, private opts: SuggestOptions) {
    boxes += 1;
    this.idBase = `suggest-${boxes}`;

    this.box = document.createElement('div');
    this.box.className = 'suggest';
    this.box.id = this.idBase;
    this.box.setAttribute('role', 'listbox');
    this.box.hidden = true;
    document.body.appendChild(this.box);

    this.input.setAttribute('autocomplete', 'off');
    this.input.setAttribute('role', 'combobox');
    this.input.setAttribute('aria-expanded', 'false');
    this.input.setAttribute('aria-controls', this.idBase);
    this.input.addEventListener('input', () => this.refresh());
    // Capture, and consuming what it uses, so the shell's own Enter handler on
    // the address bar does not also navigate to the half-typed text underneath
    // a highlighted suggestion.
    this.input.addEventListener('keydown', (ev) => this.onKeyDown(ev), true);
    this.input.addEventListener('blur', () => this.close());
    window.addEventListener('resize', () => this.close());
  }

  isOpen(): boolean {
    return !this.box.hidden;
  }

  /** What is highlighted, if anything. Exposed for tests and for Enter. */
  highlighted(): Completion | undefined {
    return this.at >= 0 ? this.shown[this.at] : undefined;
  }

  close(): void {
    this.box.hidden = true;
    this.box.textContent = '';
    this.shown = [];
    this.at = -1;
    this.input.setAttribute('aria-expanded', 'false');
    this.input.removeAttribute('aria-activedescendant');
  }

  /** Re-reads the source for the current text. Called on every keystroke. */
  refresh(open = false): void {
    const query = this.input.value.trim();
    // An empty field only offers anything when asked for it with the arrow key:
    // a list that drops open the moment the address bar is focused covers the
    // page the reader is still reading.
    if (!query && !open) {
      this.close();
      return;
    }
    const rows = this.opts.source(query, LIMIT);
    if (!rows.length) {
      this.close();
      return;
    }
    this.shown = rows;
    this.at = -1;
    this.draw();
  }

  /**
   * Re-reads the source without losing the reader's place, for the redraw after
   * a removal. Closing the list on every X would make triaging three bad rows
   * three trips back into the address bar; keeping the highlight where the
   * finger already is makes the second X the same gesture as the first.
   */
  private refreshInPlace(): void {
    const query = this.input.value.trim();
    const rows = this.opts.source(query, LIMIT);
    if (!rows.length) {
      this.close();
      return;
    }
    const at = this.at;
    this.shown = rows;
    this.at = at < 0 ? -1 : Math.min(at, rows.length - 1);
    this.draw();
  }

  private draw(): void {
    this.box.textContent = '';
    this.shown.forEach((row, i) => {
      const on = i === this.at;
      const item = document.createElement('div');
      item.className = `suggest-item${on ? ' on' : ''}`;
      item.id = `${this.idBase}-${i}`;
      item.setAttribute('role', 'option');
      item.setAttribute('aria-selected', String(on));

      const title = document.createElement('span');
      title.className = 'suggest-title';
      title.textContent = row.title;
      item.appendChild(title);

      const host = document.createElement('span');
      host.className = 'suggest-host';
      host.textContent = displayHost(row.url) || row.url;
      item.appendChild(host);

      item.appendChild(row.kind === 'history' ? this.forgetButton(row) : savedMark());

      // Pointer down rather than click, and prevented: the field must not blur
      // out from under the row before the click lands on it.
      item.addEventListener('pointerdown', (ev) => {
        ev.preventDefault();
        this.choose(row);
      });
      this.box.appendChild(item);
    });
    this.box.hidden = false;
    this.input.setAttribute('aria-expanded', 'true');
    if (this.at >= 0) this.input.setAttribute('aria-activedescendant', `${this.idBase}-${this.at}`);
    else this.input.removeAttribute('aria-activedescendant');
    this.place();
  }

  /**
   * The X. Always drawn on a touch screen, because there is no hover there to
   * reveal it with and an affordance a finger cannot discover is not one; on a
   * pointer it appears on the row under the cursor or under the highlight, so
   * six of them do not sit in the reader's way while they are reading.
   */
  private forgetButton(row: Completion & { kind: 'history' }): HTMLButtonElement {
    const button = document.createElement('button');
    button.type = 'button';
    button.className = `suggest-x${isTouch() ? ' always' : ''}`;
    button.textContent = '✕';
    // Out of the tab order: Tab through the toolbar must still step from the
    // address bar to the next control rather than into a list that is only
    // there while somebody is typing. Shift+Delete is the keyboard's way in.
    button.tabIndex = -1;
    button.setAttribute('aria-label', `Remove ${row.title} from history`);
    button.title = 'Remove from history (Shift+Delete)';
    button.addEventListener('pointerdown', (ev) => {
      // Before the row's own handler, which would navigate to the very page
      // being removed, and before the field can blur.
      ev.preventDefault();
      ev.stopPropagation();
      this.forget(row);
    });
    return button;
  }

  private forget(row: Completion & { kind: 'history' }): void {
    this.opts.forget(row);
    this.refreshInPlace();
  }

  private place(): void {
    const rect = this.input.getBoundingClientRect();
    this.box.style.left = `${Math.round(rect.left)}px`;
    this.box.style.top = `${Math.round(rect.bottom + 2)}px`;
    this.box.style.width = `${Math.round(rect.width)}px`;
  }

  private choose(row: Completion): void {
    this.input.value = row.url;
    this.close();
    this.opts.pick(row);
  }

  private move(step: number): void {
    if (!this.isOpen()) {
      this.refresh(true);
      if (!this.isOpen()) return;
    }
    const n = this.shown.length;
    this.at = this.at < 0
      ? (step > 0 ? 0 : n - 1)
      : (this.at + step + n) % n;
    this.draw();
    // The address bar keeps the caret; the highlight is the only selection.
    // Filling the field as the highlight moves would fight a reader who is
    // still typing.
  }

  private onKeyDown(ev: KeyboardEvent): void {
    if (ev.key === 'ArrowDown' || ev.key === 'ArrowUp') {
      ev.preventDefault();
      this.move(ev.key === 'ArrowDown' ? 1 : -1);
      return;
    }
    if (!this.isOpen()) return;
    // The chord every desktop browser already teaches for this, so a reader who
    // knows it does not have to find the X with a mouse to use the feature.
    if (ev.key === 'Delete' && ev.shiftKey) {
      const row = this.highlighted();
      if (row?.kind !== 'history') return;
      ev.preventDefault();
      ev.stopPropagation();
      this.forget(row);
      return;
    }
    if (ev.key === 'Escape') {
      ev.preventDefault();
      ev.stopPropagation();
      this.close();
      return;
    }
    if (ev.key === 'Enter') {
      const row = this.highlighted();
      if (!row) {
        this.close();
        return;
      }
      ev.preventDefault();
      ev.stopImmediatePropagation();
      this.choose(row);
    }
  }
}

/**
 * The ★ a saved row carries where a history row carries its X. It is the answer
 * to "why can I delete that one and not this one": this row is a page the
 * reader kept, and the way to stop keeping it is the star in the toolbar or the
 * panel — both of which offer the undo that a dropdown cannot.
 */
function savedMark(): HTMLElement {
  const mark = document.createElement('span');
  mark.className = 'suggest-mark';
  mark.textContent = '★';
  mark.title = 'Saved page';
  mark.setAttribute('aria-label', 'Saved page');
  mark.setAttribute('role', 'img');
  return mark;
}
