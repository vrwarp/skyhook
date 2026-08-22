/**
 * The phone's reload: what a finger dragging down from the top of a page means.
 *
 * The compact chrome drops back, forward and reload into the ⋯ menu, which is
 * the right trade for two of them and the wrong one for the third. Back is
 * already the system's own gesture, and forward is rare. Reload is what a
 * reader reaches for when a page arrived wrong — and on this link a page that
 * arrived wrong is minutes of waiting about to be spent again, which is not a
 * thing to bury two taps deep in a menu. Every phone browser has bound it to
 * the same place for fifteen years: the top of the page itself.
 *
 * The shell had that gesture and was throwing it away twice over. Chrome's own
 * pull-to-refresh is suppressed deliberately — `overscroll-behavior: none` on
 * the shell's body, because the thing it would reload is the *app*, and that
 * loses every tab and every page already paid seconds for — and the mirror
 * frame swallowed the drag in silence. So on a phone, on the one control that
 * costs a whole page to get wrong, there was nothing on screen and nothing to
 * pull.
 *
 * The measuring happens in the mirror host, which is the only code with the
 * frame's document to listen to (see its pull-to-reload section). This is what
 * the measurement means: nothing here touches the DOM, because the numbers and
 * the words are the part worth testing on their own — what arms the gesture,
 * what the indicator says when a release would do nothing at all, and how far
 * the indicator moves for how far the finger did.
 */

/**
 * How far the finger has to take the page before a release reloads it.
 *
 * Measured on the finger rather than on the indicator, which travels half as
 * far: the resistance is what says the reader is pulling against something
 * instead of scrolling. Far enough that no tap with a shaky thumb behind it
 * ever arrives here, short enough to be one comfortable travel of that thumb.
 */
export const TRIGGER_PX = 64;

/** How much of the finger's travel the indicator follows. */
const RESIST = 0.5;

/** Where the indicator stops, however much further the finger goes. */
const MAX_TRAVEL_PX = 44;

/**
 * Why a release would not reload, when it would not.
 *
 * Both are refusals the reader cannot see coming. Offline, the worker drops a
 * navigate frame and no page is on its way; busy, one already is, and starting
 * it again on this link throws away everything of it that has landed. A
 * gesture that quietly does nothing in either case is indistinguishable from a
 * gesture the client failed to hear, so the indicator says which it is while
 * the finger is still down and never offers the release.
 */
export type Refusal = 'offline' | 'busy';

const LABELS: Record<Refusal, string> = {
  offline: 'Offline — a reload needs the link',
  busy: 'Already loading',
};

/** How the indicator is drawn for a pull this far along. */
export interface Indicator {
  /** How far below the chrome it sits, in CSS pixels. */
  travel: number;
  /** How far round the arrow has turned: a half turn by the time a release
   *  would do something, and no further. */
  turn: number;
  /** Whether letting go now reloads the page. */
  armed: boolean;
  /** Whether the label is a refusal rather than an invitation. */
  refused: boolean;
  label: string;
}

/** What to draw for a pull that has travelled this far. */
export function indicator(distance: number, refusal: Refusal | null): Indicator {
  const pulled = Math.max(distance, 0);
  const armed = fires(pulled, refusal);
  return {
    travel: Math.round(Math.min(pulled * RESIST, MAX_TRAVEL_PX)),
    turn: Math.round(Math.min(pulled / TRIGGER_PX, 1) * 180),
    armed,
    refused: refusal !== null,
    label: refusal ? LABELS[refusal] : armed ? 'Release to reload' : 'Pull to reload',
  };
}

/** Whether a release after a pull this far reloads the page. */
export function fires(distance: number, refusal: Refusal | null): boolean {
  return refusal === null && distance >= TRIGGER_PX;
}
