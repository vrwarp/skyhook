/**
 * What a pull down from the top of a page means.
 *
 * The gesture is measured in the mirror host and drawn by the shell; this is
 * the part in between, and it is the part worth testing on its own. Two things
 * decide whether the reader spends minutes of a bad link by accident or on
 * purpose: how far the finger has to go before letting go does anything, and
 * whether the indicator ever promises a reload it is not going to perform.
 */
import { describe, expect, it } from 'vitest';

import { TRIGGER_PX, fires, indicator } from '../src/app/pull.js';

describe('pull to reload', () => {
  it('arms only once the finger has meant it', () => {
    expect(fires(0, null)).toBe(false);
    expect(fires(TRIGGER_PX - 1, null)).toBe(false);
    expect(fires(TRIGGER_PX, null)).toBe(true);
    expect(indicator(TRIGGER_PX - 1, null).armed).toBe(false);
    expect(indicator(TRIGGER_PX, null).armed).toBe(true);
  });

  it('says what letting go will do, in both states', () => {
    expect(indicator(10, null).label).toBe('Pull to reload');
    expect(indicator(TRIGGER_PX, null).label).toBe('Release to reload');
  });

  /*
   * The two refusals are the whole reason the indicator carries words at all.
   *
   * Offline, the worker drops the navigate frame and no page is coming; busy,
   * one already is, and asking again on this link throws away every byte of it
   * that has landed. Both are invisible from inside the gesture, so a pull
   * that quietly did nothing would be indistinguishable from a pull the client
   * never heard — and the reader's next move is to make it again, harder.
   */
  it('never offers a release it will not honour', () => {
    for (const refusal of ['offline', 'busy'] as const) {
      const far = indicator(TRIGGER_PX * 2, refusal);
      expect(far.armed).toBe(false);
      expect(far.refused).toBe(true);
      expect(fires(TRIGGER_PX * 2, refusal)).toBe(false);
      // And it says which of the two it is, rather than going quiet.
      expect(far.label).not.toBe('Pull to reload');
      expect(far.label).not.toBe('Release to reload');
      expect(far.label.length).toBeGreaterThan(0);
    }
    expect(indicator(TRIGGER_PX, 'offline').label).toContain('Offline');
    expect(indicator(TRIGGER_PX, 'busy').label).toContain('loading');
  });

  it('resists the pull rather than tracking it, and stops', () => {
    // Half the finger's travel: the difference between the two is what says
    // the reader is pulling against something instead of scrolling.
    expect(indicator(40, null).travel).toBe(20);
    expect(indicator(TRIGGER_PX, null).travel).toBe(32);
    // However far the finger goes, the indicator does not follow it down the
    // screen: what is below it is the page.
    const far = indicator(4000, null).travel;
    expect(far).toBeLessThan(64);
    expect(indicator(400, null).travel).toBe(far);
  });

  it('turns the arrow through half a circle and no further', () => {
    expect(indicator(0, null).turn).toBe(0);
    expect(indicator(TRIGGER_PX / 2, null).turn).toBe(90);
    expect(indicator(TRIGGER_PX, null).turn).toBe(180);
    expect(indicator(TRIGGER_PX * 10, null).turn).toBe(180);
  });

  it('treats a distance below zero as no pull at all', () => {
    // The host reports a gesture the reader took back as zero, but a frame
    // that reports a negative one must not be drawn above the chrome.
    const back = indicator(-50, null);
    expect(back.travel).toBe(0);
    expect(back.turn).toBe(0);
    expect(back.armed).toBe(false);
  });
});
