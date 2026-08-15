/**
 * The record of what the shell has asked for and not yet been answered.
 *
 * The interesting cases are all about *ending* a wait, because ending one early
 * is what makes the chrome flicker and ending one late is what makes it lie.
 */
import { describe, expect, it } from 'vitest';

import { Progress, patience } from '../src/app/progress.js';

const RTT = 1200;

describe('patience', () => {
  it('is measured in round trips, with a floor and a ceiling', () => {
    // A fast link still gets the floor: six round trips of 20 ms is no time at
    // all, and a page takes a moment to arrive however good the link is.
    expect(patience(20)).toBe(8000);
    // In-flight wifi and a satellite hop: six round trips, believed.
    expect(patience(2000)).toBe(12000);
    expect(patience(3000)).toBe(18000);
    // And an unreasonable one is capped rather than believed.
    expect(patience(60000)).toBe(40000);
  });
});

describe('Progress', () => {
  it('holds a wait from the moment the gesture goes out', () => {
    const p = new Progress();
    p.ask(1, { verb: 'Loading', url: 'https://news.ycombinator.com/item?id=1' }, 0, RTT);
    expect(p.waiting(1)?.url).toBe('https://news.ycombinator.com/item?id=1');
    expect(p.waiting(2)).toBeUndefined();
  });

  it('hands over to the server the moment it says the tab is loading', () => {
    const p = new Progress();
    p.ask(1, { verb: 'Loading', url: 'https://example.test/next' }, 0, RTT);
    expect(p.serverLoading(1, true)).toBe(true);
    expect(p.waiting(1)).toBeUndefined();
  });

  /**
   * A tab-state frame is emitted for every reason a tab has to speak, and most
   * of them carry `loading: false`. One of those in flight when the click went
   * out used to cancel a wait that had not begun — the bar appeared, vanished a
   * round trip later, and came back when the navigation actually started.
   */
  it('ignores a tab-state frame that merely says the tab is not loading', () => {
    const p = new Progress();
    p.ask(1, { verb: 'Loading', url: 'https://example.test/next' }, 0, RTT);
    expect(p.serverLoading(1, false)).toBe(false);
    expect(p.waiting(1)).toBeDefined();
  });

  it('ends a wait when the page it was waiting for arrives', () => {
    const p = new Progress();
    p.ask(1, {
      verb: 'Loading', url: 'https://example.test/next', from: 'https://example.test/',
    }, 0, RTT);
    expect(p.arrived(1, 'https://example.test/next')).toBe(true);
    expect(p.waiting(1)).toBeUndefined();
  });

  /**
   * A resync is the server replacing a document it could not patch into
   * agreement. It arrives as a snapshot for the page already on screen and says
   * nothing about where the reader asked to go.
   */
  it('does not mistake a resync for the page it is waiting for', () => {
    const p = new Progress();
    p.ask(1, {
      verb: 'Loading', url: 'https://example.test/next', from: 'https://example.test/',
    }, 0, RTT);
    expect(p.arrived(1, 'https://example.test/')).toBe(false);
    expect(p.waiting(1)).toBeDefined();
  });

  /**
   * A `#` an app uses as a button is a link as far as this side can tell: the
   * click goes landside, nothing navigates, and nothing ever comes back to say
   * so. The deadline is the only thing that ends that wait.
   */
  it('expires a wait nothing ever answers', () => {
    const p = new Progress();
    p.ask(1, { verb: 'Loading', url: 'https://example.test/#' }, 0, RTT);
    expect(p.sweep(patience(RTT) - 1)).toBe(false);
    expect(p.waiting(1)).toBeDefined();
    expect(p.sweep(patience(RTT))).toBe(true);
    expect(p.waiting(1)).toBeUndefined();
    // Nothing left to wake up for.
    expect(p.deadline()).toBe(Infinity);
  });

  it('reports the earliest deadline, so one timer covers every wait', () => {
    const p = new Progress();
    p.ask(1, { verb: 'Loading' }, 0, RTT);
    p.ask(2, { verb: 'Loading' }, 500, RTT);
    expect(p.deadline()).toBe(patience(RTT));
  });

  it('keeps a placeholder per tab asked for, oldest first', () => {
    const p = new Progress();
    p.askOpen({ verb: 'Opening', url: 'https://example.test/one' }, 0, RTT);
    p.askOpen({ verb: 'Opening', url: 'https://example.test/two' }, 10, RTT);
    expect(p.opening.map((o) => o.url))
      .toEqual(['https://example.test/one', 'https://example.test/two']);

    expect(p.appeared()).toBe(true);
    expect(p.opening.map((o) => o.url)).toEqual(['https://example.test/two']);
    expect(p.appeared()).toBe(true);
    // A tab arriving that nobody asked for is not an error, and there is
    // nothing left to retire.
    expect(p.appeared()).toBe(false);
  });

  it('expires a tab the server never opened', () => {
    const p = new Progress();
    p.askOpen({ verb: 'Opening' }, 0, RTT);
    expect(p.sweep(patience(RTT))).toBe(true);
    expect(p.opening).toHaveLength(0);
  });

  it('drops everything when the link goes down', () => {
    const p = new Progress();
    p.ask(1, { verb: 'Loading' }, 0, RTT);
    p.askOpen({ verb: 'Opening' }, 0, RTT);
    expect(p.clear()).toBe(true);
    expect(p.waiting(1)).toBeUndefined();
    expect(p.opening).toHaveLength(0);
    // Idempotent: a second status frame saying the same thing is not a change.
    expect(p.clear()).toBe(false);
  });

  it('forgets a closed tab', () => {
    const p = new Progress();
    p.ask(1, { verb: 'Loading' }, 0, RTT);
    expect(p.forget(1)).toBe(true);
    expect(p.waiting(1)).toBeUndefined();
  });

  it('remembers where a tab is going after the server takes the ask over', () => {
    const p = new Progress();
    p.ask(1, { verb: 'Loading', url: 'https://example.test/next', from: 'https://example.test/' },
      0, RTT);
    // The tab-state frame that says "loading" retires the ask, and for the rest
    // of the wait the tab's own URL is still the page being left. Without the
    // destination the status line names that page, which is the one place the
    // reader is not going.
    expect(p.serverLoading(1, true)).toBe(true);
    expect(p.waiting(1)).toBeUndefined();
    expect(p.destination(1)).toBe('https://example.test/next');
  });

  it('forgets the destination once a document lands', () => {
    const p = new Progress();
    p.ask(1, { verb: 'Loading', url: 'https://example.test/next', from: 'https://example.test/' },
      0, RTT);
    p.serverLoading(1, true);
    p.arrived(1, 'https://example.test/next');
    expect(p.destination(1)).toBe('');
  });

  it('forgets the destination on a resync too', () => {
    const p = new Progress();
    p.ask(1, { verb: 'Loading', url: 'https://example.test/next', from: 'https://example.test/' },
      0, RTT);
    // A snapshot for the page the tab is already on is the server closing a gap
    // it could not close with diffs. It is still a document on screen, so the
    // tab is no longer on its way anywhere.
    expect(p.arrived(1, 'https://example.test/')).toBe(false);
    expect(p.destination(1)).toBe('');
  });

  it('names nowhere for a gesture that named nowhere', () => {
    const p = new Progress();
    p.ask(1, { verb: 'Going back' }, 0, RTT);
    p.serverLoading(1, true);
    expect(p.destination(1)).toBe('');
  });

  it('drops destinations with everything else', () => {
    const p = new Progress();
    p.ask(1, { verb: 'Loading', url: 'https://example.test/next' }, 0, RTT);
    p.clear();
    expect(p.destination(1)).toBe('');
    p.ask(2, { verb: 'Loading', url: 'https://example.test/next' }, 0, RTT);
    p.forget(2);
    expect(p.destination(2)).toBe('');
  });
});
