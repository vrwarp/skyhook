/**
 * The tabs, as a list.
 *
 * A phone has no room for a strip of them. Six tabs on a 393-pixel screen is
 * six titles clipped to two words each, and — worse — a "+" pushed off the end
 * of a strip that scrolls, which is the button that opens a tab made
 * unreachable in a browser whose tabs are the whole session. Every page in
 * that session cost seconds to fetch and is still loaded landside, so losing
 * the way back to one is not a cosmetic loss.
 *
 * So the strip becomes what every phone browser has made it: a count in the
 * toolbar that opens a list. A row per tab, wide enough for the title and the
 * host — a title alone does not tell two tabs on the same site apart — with
 * the close button its own target, far enough from the row that a thumb aiming
 * for one does not hit the other.
 *
 * The list is drawn from exactly what the strip is drawn from, tabs the server
 * has confirmed and tabs it has only been asked for, so the two cannot
 * disagree about what is open.
 */

/** One row's worth of tab, as the shell knows it. */
export interface TabRow {
  id: number;
  title: string;
  url: string;
  /** Whether a page is on its way into this tab, from either half of the story. */
  loading: boolean;
}

/** A tab asked for and not yet opened by the server: it has no id to act on. */
export interface GhostRow {
  url?: string;
}

export interface TabListActions {
  /** Bring one to the front. The panel closes: the reader asked for a page. */
  select(id: number): void;
  close(id: number): void;
  /**
   * Call off a page that is still coming, keeping the tab and whatever of it
   * has already arrived.
   *
   * This list is the only place a *background* tab can be stopped. The toolbar
   * button acts on the tab in front of the reader, and the tab spending the
   * link is exactly the one they have given up on and switched away from.
   */
  stop(id: number): void;
  /** A long press on a row, answered with the same menu the strip offers. */
  menu(id: number, x: number, y: number): void;
  open(): void;
}

/**
 * Built once and kept, so that a tab arriving while the list is open does not
 * rebuild the row under the finger on its way down.
 */
export class TabList {
  readonly root: HTMLElement;
  private rows: HTMLElement;
  private newTab: HTMLButtonElement;

  constructor(private actions: TabListActions) {
    this.root = document.createElement('div');
    this.root.className = 'tablist';

    this.rows = document.createElement('div');
    this.rows.className = 'tablist-rows';
    this.rows.setAttribute('role', 'tablist');
    this.root.appendChild(this.rows);

    this.newTab = document.createElement('button');
    this.newTab.type = 'button';
    this.newTab.className = 'tablist-new';
    this.newTab.textContent = '+  New tab';
    this.newTab.addEventListener('click', () => this.actions.open());
    this.root.appendChild(this.newTab);
  }

  render(tabs: TabRow[], ghosts: GhostRow[], active: number, online: boolean): void {
    this.rows.textContent = '';
    for (const tab of tabs) this.rows.appendChild(this.row(tab, tab.id === active));
    for (const ghost of ghosts) this.rows.appendChild(this.ghost(ghost));
    if (!tabs.length && !ghosts.length) {
      const empty = document.createElement('p');
      empty.className = 'marks-empty';
      empty.textContent = 'No tabs open.';
      this.rows.appendChild(empty);
    }
    // Opening one is a request to the server, so offline it would do nothing at
    // all — the same bargain the strip's own "+" makes.
    this.newTab.disabled = !online;
    this.newTab.title = online ? 'New tab' : 'Waiting for the link';
  }

  private row(tab: TabRow, on: boolean): HTMLElement {
    const row = document.createElement('div');
    row.className = `tabrow${on ? ' on' : ''}`;
    row.setAttribute('role', 'tab');
    row.setAttribute('aria-selected', String(on));
    row.tabIndex = 0;
    row.dataset.tab = String(tab.id);

    // The spinner is the one thing in the row that is about the waiting, so it
    // is what the reader reaches for to end it. A row that is not loading has
    // nothing to stop and shows no target at all.
    if (tab.loading) {
      const stop = document.createElement('button');
      stop.type = 'button';
      stop.className = 'tabrow-stop';
      stop.title = 'Stop loading';
      stop.setAttribute('aria-label', `Stop loading ${tab.title || hostOf(tab.url) || 'this tab'}`);
      stop.appendChild(spinner());
      stop.addEventListener('click', (ev) => {
        ev.stopPropagation();
        this.actions.stop(tab.id);
      });
      row.appendChild(stop);
    }

    const text = document.createElement('span');
    text.className = 'tabrow-text';
    const title = document.createElement('span');
    title.className = 'tabrow-title';
    title.textContent = tab.title || hostOf(tab.url) || 'New tab';
    text.appendChild(title);
    const host = document.createElement('span');
    host.className = 'tabrow-host';
    host.textContent = hostOf(tab.url) || 'nothing here yet';
    text.appendChild(host);
    row.appendChild(text);

    const close = document.createElement('button');
    close.type = 'button';
    close.className = 'tabrow-close';
    close.textContent = '×';
    close.setAttribute('aria-label', `Close ${title.textContent}`);
    close.addEventListener('click', (ev) => {
      ev.stopPropagation();
      this.actions.close(tab.id);
    });
    row.appendChild(close);

    row.addEventListener('click', (ev) => {
      if ((ev.target as HTMLElement | null)?.closest('button')) return;
      this.actions.select(tab.id);
    });
    row.addEventListener('keydown', (ev) => {
      if (ev.key !== 'Enter' && ev.key !== ' ') return;
      ev.preventDefault();
      this.actions.select(tab.id);
    });
    // A long press raises this in Chrome, which is the only gesture a phone has
    // for "and what else can I do with this one".
    row.addEventListener('contextmenu', (ev) => {
      ev.preventDefault();
      ev.stopPropagation();
      this.actions.menu(tab.id, ev.clientX, ev.clientY);
    });
    return row;
  }

  private ghost(ghost: GhostRow): HTMLElement {
    const row = document.createElement('div');
    row.className = 'tabrow ghost';
    row.title = 'Waiting for the server to open this tab';
    row.appendChild(spinner());
    const text = document.createElement('span');
    text.className = 'tabrow-text';
    const title = document.createElement('span');
    title.className = 'tabrow-title';
    title.textContent = (ghost.url && hostOf(ghost.url)) || 'New tab';
    text.appendChild(title);
    row.appendChild(text);
    return row;
  }
}

function spinner(): HTMLElement {
  const spin = document.createElement('span');
  spin.className = 'spin';
  spin.setAttribute('role', 'img');
  spin.setAttribute('aria-label', 'Loading');
  return spin;
}

function hostOf(url: string): string {
  try {
    return new URL(url).host;
  } catch {
    return '';
  }
}
