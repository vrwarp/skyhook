/**
 * The tab model: which tabs exist, which one is in front, and the bookkeeping
 * for a tab the user has asked for but the server has not yet named.
 *
 * Opening a tab is the one chrome action that cannot be answered plane-side —
 * only the landside browser can make a page, and only it can say what the tab
 * is called. Waiting for that answer before drawing anything made the "+"
 * button feel broken: on this link the round trip is over a second, and a
 * button that does nothing for a second is a button people press again.
 *
 * So a tab is drawn immediately with a provisional id (negative, never a real
 * one) and a `ref` the server echoes back on the frame that names it. When
 * that frame arrives the provisional tab *becomes* the real one — same strip
 * entry, same mirror frame — and anything the user did to it in the meantime
 * is sent under the real id. The user pays no round trip to see the tab, and
 * one round trip, unavoidably, before its content can start arriving.
 */
import type { TabRef, TabState } from '../shared/protocol.js';

export interface TabView {
  id: number;
  url: string;
  title: string;
  loading: boolean;
  canBack: boolean;
  canForward: boolean;
  /** True while the tab exists only plane-side, waiting to be named. */
  provisional: boolean;
}

/** A command aimed at a tab, held until the tab has a real id to carry it. */
interface Held {
  name: string;
  args: Record<string, unknown>;
}

export interface TabModelEvents {
  /** Sends a command to the network worker. */
  send(name: string, args: Record<string, unknown>): void;
  /** A provisional tab has been given its real id. */
  adopted?(from: number, to: number): void;
  /** A tab is gone and whatever is drawn for it should go too. */
  dropped?(id: number): void;
}

export class TabModel {
  private tabs = new Map<number, TabView>();
  /** Provisional id by the ref the server will echo. */
  private byRef = new Map<string, number>();
  /** Commands waiting for a provisional tab to be named. */
  private held = new Map<number, Held[]>();
  /** Refs whose tab was closed before the server ever named it. */
  private abandoned = new Set<string>();
  /**
   * Tabs the reader has closed.
   *
   * The shell learns what a tab is from frames, and the frames for a closed one
   * keep arriving for a round trip after the close — which on this link is
   * seconds, and while a document is streaming is far longer. Without this, a
   * state frame for a closed tab puts it back in the strip, where clicking it
   * reaches a tab the server has already been told to close.
   */
  private closed = new Set<number>();
  private nextRef = 1;
  private nextProvisional = -1;
  /** The tab in front. 0 means none. */
  active = 0;

  constructor(private readonly events: TabModelEvents) {}

  get size(): number {
    return this.tabs.size;
  }

  list(): TabView[] {
    return Array.from(this.tabs.values());
  }

  ids(): number[] {
    return Array.from(this.tabs.keys());
  }

  get(id: number): TabView | undefined {
    return this.tabs.get(id);
  }

  current(): TabView | undefined {
    return this.tabs.get(this.active);
  }

  /** True while a tab is drawn but not yet named by the server. */
  isProvisional(id: number): boolean {
    return this.tabs.get(id)?.provisional ?? false;
  }

  /**
   * Opens a tab. The tab exists plane-side when this returns; the id it returns
   * is provisional until the server answers. A background tab — a middle click,
   * or "open link in a new tab" — is drawn but not brought to the front, which
   * is what the gesture means everywhere else and what keeps the link spent on
   * the page the user is still reading.
   */
  open(url = '', background = false): number {
    const id = this.nextProvisional--;
    const ref = `r${this.nextRef++}`;
    this.byRef.set(ref, id);
    this.tabs.set(id, {
      id, url, title: '', loading: url !== '',
      canBack: false, canForward: false, provisional: true,
    });
    if (!background) this.active = id;
    this.events.send('openTab', { url, ref, background });
    return id;
  }

  /**
   * Applies a TabState from the server, adopting the provisional tab it names
   * if there is one. Returns the id the state landed on, or undefined if the
   * state closed the tab.
   */
  applyState(id: number, st: TabState): number | undefined {
    // A tab the reader has closed stays closed, whatever is still in flight
    // for it.
    if (this.closed.has(id)) return undefined;
    if (st.ref && this.abandoned.has(st.ref)) {
      // The user closed this tab while it was still provisional. It exists
      // landside now, so it has to be closed there — but nothing about it
      // should reappear in the strip.
      this.abandoned.delete(st.ref);
      this.flushHeld(this.byRef.get(st.ref) ?? 0, id);
      this.byRef.delete(st.ref);
      return undefined;
    }
    if (st.ref) this.adopt(st.ref, id);
    if (st.closed) {
      this.remove(id);
      return undefined;
    }
    const known = this.tabs.get(id);
    const tab: TabView = known ?? {
      id, url: '', title: '', loading: false,
      canBack: false, canForward: false, provisional: false,
    };
    // Merge rather than replace: most TabStates report one thing that changed
    // — a load starting, a title arriving — and carry nothing else.
    tab.url = st.url || tab.url;
    tab.title = st.title || tab.title;
    tab.loading = st.loading;
    tab.canBack = st.canBack;
    tab.canForward = st.canForward;
    tab.provisional = false;
    this.tabs.set(id, tab);
    if (!this.active) this.active = id;
    return id;
  }

  /**
   * Rebuilds from the server's list after a connection is established. The
   * server's tabs are the truth: a provisional tab that was never adopted was
   * either never created landside or is in that list under its real id, and
   * either way keeping the plane-side ghost would show the user a tab that can
   * never load anything.
   */
  reset(refs: TabRef[], sameSession = true): void {
    // A different session never had the tabs this side closed, so nothing is
    // in flight for them and the ids it hands out next may reuse those numbers.
    if (!sameSession) this.closed.clear();
    for (const id of this.ids()) {
      if (this.tabs.get(id)?.provisional) this.remove(id);
    }
    // Whatever this side holds and the session does not is gone. Not
    // remembered as closed: the server never had it, so nothing is in flight
    // for it, and an id it has never used is one it may yet.
    const held = new Set(refs.map((r) => r.tab));
    for (const id of this.ids()) {
      if (id > 0 && !held.has(id)) this.remove(id);
    }
    // Every ref was issued on the connection that has just been replaced. A
    // tab that survived is in the list below under its real id; a command still
    // waiting on a name it will never get would go to the wrong tab.
    this.byRef.clear();
    this.held.clear();
    this.abandoned.clear();
    for (const r of refs) {
      const known = this.tabs.get(r.tab);
      this.tabs.set(r.tab, {
        id: r.tab,
        url: r.url || known?.url || '',
        title: r.title || known?.title || '',
        loading: r.loading,
        canBack: known?.canBack ?? false,
        canForward: known?.canForward ?? false,
        provisional: false,
      });
      if (r.active) this.active = r.tab;
    }
    if (!this.tabs.has(this.active)) {
      this.active = refs.length ? (refs[0]?.tab ?? 0) : 0;
    }
  }

  /** Whether a tab is one the reader has closed. */
  isClosed(id: number): boolean {
    return this.closed.has(id);
  }

  /** Brings a tab to the front. */
  select(id: number): void {
    if (this.tabs.has(id)) this.active = id;
  }

  /**
   * Closes a tab, both plane-side and landside. Closing one that has not been
   * named yet still has to reach the server — the page is being built there
   * whether or not the user still wants it — so the request is held like any
   * other and the ref stays bound until it can be sent.
   */
  close(id: number): void {
    this.forTab('closeTab', id, {});
    // Remembered, so the frames still crossing the link for this tab find a
    // model that has stopped believing in it. A provisional id is never named
    // by the server, so there is nothing to remember.
    if (id > 0) this.closed.add(id);
    if (id < 0) {
      for (const [ref, prov] of this.byRef) {
        if (prov === id) this.abandoned.add(ref);
      }
    }
    this.remove(id);
  }

  /**
   * Sends a command about a tab, holding it if the tab has no real id yet.
   * Typing a URL into a tab that was opened half a round trip ago is the
   * ordinary case, not an edge case: it must arrive, in order, once the tab
   * has a name.
   */
  forTab(name: string, id: number, args: Record<string, unknown> = {}): void {
    if (id < 0) {
      if (!this.tabs.has(id)) return;
      const queue = this.held.get(id) ?? [];
      queue.push({ name, args });
      this.held.set(id, queue);
      return;
    }
    this.events.send(name, { ...args, tab: id });
  }

  private adopt(ref: string, id: number): void {
    const provisional = this.byRef.get(ref);
    this.byRef.delete(ref);
    if (provisional === undefined || provisional === id) return;
    const drawn = this.tabs.get(provisional);
    if (!drawn) return;
    this.tabs.delete(provisional);
    drawn.id = id;
    drawn.provisional = false;
    this.tabs.set(id, drawn);
    if (this.active === provisional) this.active = id;
    this.events.adopted?.(provisional, id);
    this.flushHeld(provisional, id);
  }

  private flushHeld(provisional: number, id: number): void {
    const queue = this.held.get(provisional) ?? [];
    this.held.delete(provisional);
    for (const cmd of queue) this.events.send(cmd.name, { ...cmd.args, tab: id });
  }

  private remove(id: number): void {
    if (!this.tabs.delete(id)) return;
    // Anything still held for an abandoned tab is deliberately kept: it is
    // waiting for the id that lets it be sent, and the close is usually in it.
    if (!this.hasAbandonedRef(id)) {
      this.held.delete(id);
      for (const [ref, prov] of this.byRef) {
        if (prov === id) this.byRef.delete(ref);
      }
    }
    if (this.active === id) {
      const next = this.tabs.keys().next().value;
      this.active = typeof next === 'number' ? next : 0;
    }
    this.events.dropped?.(id);
  }

  private hasAbandonedRef(id: number): boolean {
    for (const ref of this.abandoned) {
      if (this.byRef.get(ref) === id) return true;
    }
    return false;
  }
}
