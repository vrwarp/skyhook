/**
 * What the shell is waiting for, and how long it is worth waiting.
 *
 * An ordinary browser knows a navigation started because it started it: the
 * spinner, the bar and the greyed address are all drawn from a fact it holds
 * locally. Here the navigation starts on the far side of the link. A click on a
 * Hacker News story is a semantic event sent to a browser several seconds away,
 * and the first word back that anything is happening — the tab-state frame the
 * landside `frameStartedLoading` produces — arrives a full round trip later. In
 * between, the most ordinary act in browsing produces no evidence at all that
 * the click was even received, on the one link where that reassurance is worth
 * most.
 *
 * So the only honest thing this side can say immediately is "we asked", and
 * this is the record of the asking. It is deliberately not a claim that a page
 * is loading: a click may land on script that does nothing, and a `#` used as a
 * button is indistinguishable from a link until the answer comes back. Every
 * ask therefore carries a deadline, and an ask nothing ever answers expires
 * quietly rather than spinning for the rest of the session.
 */

/** One thing the shell has asked the server for and not yet seen answered. */
export interface Ask {
  /** Where the gesture pointed, when it named somewhere. */
  url?: string;
  /** What was asked for, as the status line says it: "Loading", "Reloading". */
  verb: string;
  /** What the tab was showing when the ask went out. A snapshot for anything
   *  else is this ask, arrived. */
  from: string;
  /** Clock reading when the ask went out, and when to stop believing in it. */
  at: number;
  until: number;
}

/** What a caller says about a navigation it has just asked for. */
export interface AskInit {
  verb: string;
  url?: string;
  from?: string;
}

/**
 * How long an unanswered ask stays on screen.
 *
 * In round trips, because that is what it is waiting for: the click out, the
 * navigation landside, the first tab-state frame back. Six of them is generous
 * on purpose — an ask that expires while the answer is still in flight puts the
 * chrome back to idle and then, seconds later, into loading again, which reads
 * as a fault rather than as a wait. The floor covers a fast link, where six
 * round trips is no time at all but a page still takes a moment to arrive; the
 * ceiling covers an outage that the status frames have not caught up with yet.
 */
const PATIENCE_RTTS = 6;
const MIN_PATIENCE_MS = 8_000;
const MAX_PATIENCE_MS = 40_000;

/** How long to keep believing in an ask, at the link's current round trip. */
export function patience(rttMs: number): number {
  const scaled = Math.round(rttMs * PATIENCE_RTTS);
  return Math.min(MAX_PATIENCE_MS, Math.max(MIN_PATIENCE_MS, scaled));
}

/**
 * The asks outstanding: one per tab that has been sent somewhere, plus the
 * tabs that have been asked for and do not exist yet.
 *
 * Server-reported loading is deliberately not held here. It is the tab's own
 * state and the shell already keeps it; this tracks only the window in which
 * this side knows something the server has not told it yet.
 */
export class Progress {
  private asks = new Map<number, Ask>();
  private opens: Ask[] = [];
  /**
   * Where each tab was last asked to go, kept from the moment of asking until
   * a document actually arrives.
   *
   * It outlives the ask on purpose. An ask is retired the moment the server
   * confirms the tab is loading, and from then until the navigation commits —
   * seconds, on this link — the tab's own URL is still the page being left. A
   * status line drawn from it in that window reads "Loading <where you already
   * are>", which is the one page the reader is certainly not going to.
   */
  private going = new Map<number, string>();

  /** Records a navigation asked for on an existing tab. */
  ask(tab: number, init: AskInit, now: number, rttMs: number): void {
    if (!tab) return;
    this.asks.set(tab, {
      verb: init.verb,
      url: init.url,
      from: init.from ?? '',
      at: now,
      until: now + patience(rttMs),
    });
    if (init.url) this.going.set(tab, init.url);
    else this.going.delete(tab);
  }

  /**
   * Where a tab is headed, for as long as it has not got there.
   *
   * Empty for a gesture that named nowhere — back, forward, a form submitted —
   * because this side genuinely does not know where those land, and guessing
   * is what produced the wrong answer in the first place.
   */
  destination(tab: number): string {
    return this.going.get(tab) ?? '';
  }

  /** Records a tab asked for that the server has not yet opened. */
  askOpen(init: AskInit, now: number, rttMs: number): void {
    this.opens.push({
      verb: init.verb,
      url: init.url,
      from: '',
      at: now,
      until: now + patience(rttMs),
    });
  }

  /** What this tab is waiting for, if anything. */
  waiting(tab: number): Ask | undefined {
    return this.asks.get(tab);
  }

  /** The tabs asked for and not yet arrived, oldest first. */
  get opening(): readonly Ask[] {
    return this.opens;
  }

  /**
   * Takes the server's word for whether a tab is loading.
   *
   * Only `true` retires an ask. A tab-state frame saying `false` is emitted for
   * every reason a tab has to speak — a title changing, a history sync — and one
   * of those in flight when the click went out would otherwise cancel a wait
   * that has not begun. Once the server says it is loading, the tab's own state
   * says everything this did, and better.
   */
  serverLoading(tab: number, loading: boolean): boolean {
    if (!loading) return false;
    return this.asks.delete(tab);
  }

  /**
   * Notes a snapshot arriving for a tab.
   *
   * A document for a different URL than the one the ask left is that ask,
   * arrived — the affordance can come down without waiting for the tab-state
   * frame behind it. A snapshot for the same URL is a resync, which is the
   * server closing a gap it could not close with diffs and says nothing about
   * where the reader asked to go.
   */
  arrived(tab: number, url: string): boolean {
    // A document has landed, whatever it is, so wherever the tab was headed it
    // is no longer on its way there. This happens even for a resync, which is
    // the server replacing a document with the same one: the tab is showing a
    // real page either way, and a destination kept past that is a destination
    // that will label the tab's next unlabelled wait.
    if (url) this.going.delete(tab);
    const ask = this.asks.get(tab);
    if (!ask || !url || url === ask.from) return false;
    return this.asks.delete(tab);
  }

  /** Retires the oldest pending open: a tab this side asked for has appeared. */
  appeared(): boolean {
    return this.opens.shift() !== undefined;
  }

  /** Forgets a closed tab. */
  forget(tab: number): boolean {
    this.going.delete(tab);
    return this.asks.delete(tab);
  }

  /** Drops everything, for a link that has gone down or a session restarting. */
  clear(): boolean {
    const had = this.asks.size > 0 || this.opens.length > 0;
    this.asks.clear();
    this.opens = [];
    this.going.clear();
    return had;
  }

  /** Expires asks nothing has answered. Reports whether any were dropped. */
  sweep(now: number): boolean {
    let dropped = false;
    for (const [tab, ask] of this.asks) {
      if (ask.until <= now) {
        this.asks.delete(tab);
        dropped = true;
      }
    }
    const keep = this.opens.filter((o) => o.until > now);
    if (keep.length !== this.opens.length) {
      this.opens = keep;
      dropped = true;
    }
    return dropped;
  }

  /** When the next ask expires, so a caller can arm one timer for all of them. */
  deadline(): number {
    let next = Infinity;
    for (const ask of this.asks.values()) next = Math.min(next, ask.until);
    for (const open of this.opens) next = Math.min(next, open.until);
    return next;
  }
}
