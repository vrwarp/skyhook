/**
 * Address-bar completion from the saved list.
 *
 * On a link where a page costs seconds, a mistyped address is the most
 * expensive mistake the chrome can let a reader make: the round trip is spent
 * before anyone finds out it was wrong. Completing from bookmarks costs
 * nothing, is local, and works during an outage — the reader can line up where
 * to go while the link is down and press Enter when it comes back.
 *
 * Deliberately not history-backed and deliberately not fuzzy. The client has no
 * history store, and a fuzzy match that offers a page the reader did not mean
 * is the round trip this exists to save.
 *
 * The widget is a sibling of the menu (`menu.ts`) in spirit: drawn by the
 * shell, one at a time, and it never takes focus away from the field it serves.
 */

import { displayHost, type Bookmark } from './bookmarks.js';

const LIMIT = 6;

export interface SuggestOptions {
  /** Candidates for what has been typed. Empty query means "the recent ones". */
  source(query: string, limit: number): Bookmark[];
  /** The reader chose one. The field has already been filled in. */
  pick(mark: Bookmark): void;
}

export class Suggest {
  private box: HTMLElement;
  private shown: Bookmark[] = [];
  private at = -1;

  constructor(private input: HTMLInputElement, private opts: SuggestOptions) {
    this.box = document.createElement('div');
    this.box.className = 'suggest';
    this.box.setAttribute('role', 'listbox');
    this.box.hidden = true;
    document.body.appendChild(this.box);

    this.input.setAttribute('autocomplete', 'off');
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
  highlighted(): Bookmark | undefined {
    return this.at >= 0 ? this.shown[this.at] : undefined;
  }

  close(): void {
    this.box.hidden = true;
    this.box.textContent = '';
    this.shown = [];
    this.at = -1;
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
    const marks = this.opts.source(query, LIMIT);
    if (!marks.length) {
      this.close();
      return;
    }
    this.shown = marks;
    this.at = -1;
    this.draw();
  }

  private draw(): void {
    this.box.textContent = '';
    this.shown.forEach((mark, i) => {
      const row = document.createElement('div');
      row.className = `suggest-item${i === this.at ? ' on' : ''}`;
      row.setAttribute('role', 'option');
      row.setAttribute('aria-selected', String(i === this.at));

      const title = document.createElement('span');
      title.className = 'suggest-title';
      title.textContent = mark.title;
      row.appendChild(title);

      const host = document.createElement('span');
      host.className = 'suggest-host';
      host.textContent = displayHost(mark.url) || mark.url;
      row.appendChild(host);

      // Pointer down rather than click, and prevented: the field must not blur
      // out from under the row before the click lands on it.
      row.addEventListener('pointerdown', (ev) => {
        ev.preventDefault();
        this.choose(mark);
      });
      this.box.appendChild(row);
    });
    this.box.hidden = false;
    this.place();
  }

  private place(): void {
    const rect = this.input.getBoundingClientRect();
    this.box.style.left = `${Math.round(rect.left)}px`;
    this.box.style.top = `${Math.round(rect.bottom + 2)}px`;
    this.box.style.width = `${Math.round(rect.width)}px`;
  }

  private choose(mark: Bookmark): void {
    this.input.value = mark.url;
    this.close();
    this.opts.pick(mark);
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
    if (ev.key === 'Escape') {
      ev.preventDefault();
      ev.stopPropagation();
      this.close();
      return;
    }
    if (ev.key === 'Enter') {
      const mark = this.highlighted();
      if (!mark) {
        this.close();
        return;
      }
      ev.preventDefault();
      ev.stopImmediatePropagation();
      this.choose(mark);
    }
  }
}
