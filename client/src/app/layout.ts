/**
 * Which shell the app is drawing: the desktop one, or the phone one.
 *
 * Two questions, deliberately kept apart, because they have different answers
 * on the same device. How wide the screen is decides what the chrome looks
 * like — one row instead of two, sheets instead of panels, a list of tabs
 * instead of a strip of them. What kind of pointer is on it decides what the
 * chrome may *say*: an entry hinting "Ctrl+D" or "Middle click" on a screen
 * with no keyboard and no middle button is an instruction to do something
 * impossible, and the reader who tries it spends the one thing this client is
 * built to save.
 *
 * A narrow desktop window gets the phone layout and keeps the chord hints,
 * which is the right answer to both questions asked separately and the wrong
 * answer to either one asked for both.
 */

/** Screens this narrow get the phone chrome. */
export const PHONE = '(max-width: 600px)';
/** A finger, or anything else with no hover and no precision. */
export const TOUCH = '(pointer: coarse)';

/**
 * Answers a media query, defaulting to "no" where there is nothing to ask —
 * jsdom under the unit tests has no matchMedia, and the desktop shell is the
 * right shell to fall back to.
 */
function matches(query: string): boolean {
  return typeof window !== 'undefined'
    && typeof window.matchMedia === 'function'
    && window.matchMedia(query).matches;
}

/** Whether the chrome is drawn as a phone's: one row, sheets, a tab list. */
export function isPhone(): boolean {
  return matches(PHONE);
}

/** Whether the reader is pointing with a finger. */
export function isTouch(): boolean {
  return matches(TOUCH);
}

/** Whether this device is set to dark. See main.ts's deviceScheme. */
export function prefersDark(): boolean {
  return matches('(prefers-color-scheme: dark)');
}
