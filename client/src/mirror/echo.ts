/**
 * Local echo and reconciliation.
 *
 * The focused editable element enters client-owned mode: keystrokes render
 * instantly through Blink's own editing behaviour, and server mutations that
 * touch the owned subtree are held aside rather than applied. When the server's
 * version of the text comes back, it is compared with the local text; identical
 * (the common case) means the mutation is just confirmation and is dropped,
 * different (autocomplete, input masks, mention expansion) means the server
 * wins but the caret is preserved.
 *
 * Losing a keystroke is unacceptable; a brief flicker is not.
 */
import { MutationOp, OpCode } from '../shared/protocol.js';

export interface EchoHooks {
  /** Sends a text insertion to the server. */
  sendText(node: number, text: string): void;
  /** Sends a control key to the server. */
  sendKey(node: number, key: string, modifiers: number, repeat: number): void;
  /** Sends the whole field value, used when local and server state diverge. */
  sendValue(node: number, value: string, start: number, end: number): void;
  /** Sends focus/blur so the landside page sees the same focus we do. */
  sendFocus(node: number, focused: boolean): void;
  /** Resolves a DOM node to its mirror id. */
  idOf(node: Node | null): number;
  /** Optimistic chat send: returns true if the ghost message was placed. */
  onChatSend?(node: Node, text: string): boolean;
}

interface Owned {
  node: HTMLElement;
  id: number;
  /** Text as the client believes it to be. */
  local: string;
  /** Buffered server ops for this subtree, replayed on release. */
  deferred: MutationOp[];
}

export class EchoEngine {
  private owned: Owned | null = null;
  private hooks: EchoHooks;
  private doc: Document;
  /** Ghost nodes inserted optimistically, keyed by the text they represent. */
  private ghosts = new Map<string, HTMLElement>();

  constructor(doc: Document, hooks: EchoHooks) {
    this.doc = doc;
    this.hooks = hooks;
  }

  /** True when a node lies inside the client-owned subtree. */
  isOwned(node: Node): boolean {
    return this.owned !== null && node === this.owned.node;
  }

  /** The mirror id of the owned element, or 0. */
  get ownedId(): number {
    return this.owned?.id ?? 0;
  }

  /** Called on focusin. */
  focus(target: EventTarget | null): void {
    const el = asEditable(target);
    if (!el) return;
    const id = this.hooks.idOf(el);
    if (!id) return;
    if (this.owned?.node === el) return;
    this.release();
    this.owned = { node: el, id, local: valueOf(el), deferred: [] };
    this.hooks.sendFocus(id, true);
  }

  /**
   * Called on focusout: ownership ends and buffered server truth is applied.
   *
   * `hold` says the caller will tell the page about the focus change itself.
   * Ownership ends here regardless — that is this side's bookkeeping about a
   * field the reader has left, and it is true the moment they leave it — but
   * the page hears about it from whatever the caller is holding the blur for.
   * See MirrorHost.heldBlur, which exists because a page that closes a popover
   * on blur must not be told before the click it closed over has been sent.
   */
  blur(apply: (op: MutationOp) => void, hold = false): void {
    const owned = this.owned;
    if (!owned) return;
    if (!hold) this.hooks.sendFocus(owned.id, false);
    this.owned = null;
    for (const op of owned.deferred) apply(op);
  }

  /** Called on input events in the owned element. `target` is the composed
   *  target when the caller has one — `ev.target` renames an event from
   *  inside a shadow root to the host, which is never the owned field. */
  input(ev: InputEvent, target?: EventTarget | null): void {
    const owned = this.owned;
    if (!owned || (target ?? ev.target) !== owned.node) return;
    const now = valueOf(owned.node);
    const prev = owned.local;
    owned.local = now;

    // Send the smallest thing that reproduces the edit landside: an insertion
    // for typing and pasting, a whole-value set for anything else (deletes,
    // mid-string edits, IME composition results).
    if (now.length > prev.length && now.startsWith(prev)) {
      this.hooks.sendText(owned.id, now.slice(prev.length));
      return;
    }
    const caret = caretOf(owned.node);
    this.hooks.sendValue(owned.id, now, caret.start, caret.end);
  }

  /** Called on keydown for control keys. Returns true if the key was handled
   *  locally and should not also be sent as text. */
  key(ev: KeyboardEvent): boolean {
    const owned = this.owned;
    const modifiers = modifierMask(ev);
    const id = owned ? owned.id : this.hooks.idOf(this.doc.activeElement);

    if (ev.key === 'Enter' && owned && !ev.shiftKey) {
      // A chat send: place a pending ghost so the message appears instantly,
      // even though confirmation is a round trip away.
      const text = owned.local;
      if (text && this.hooks.onChatSend?.(owned.node, text)) {
        this.ghosts.set(text, owned.node);
        setValue(owned.node, '');
        owned.local = '';
      }
      this.hooks.sendKey(id, 'Enter', modifiers, 1);
      return true;
    }
    if (CONTROL_KEYS.has(ev.key)) {
      this.hooks.sendKey(id, ev.key, modifiers, ev.repeat ? 1 : 1);
      // Editing keys are still handled natively so the field looks right; the
      // resulting input event reconciles the value.
      return ev.key !== 'Backspace' && ev.key !== 'Delete';
    }
    return false;
  }

  /** Offers a server op to the echo engine. Returns true if the engine took
   *  ownership of it (the patcher must not apply it). */
  defer(op: MutationOp, resolve: (id: number) => Node | undefined): boolean {
    const owned = this.owned;
    if (!owned) return false;
    const target = resolve(op.node);
    if (!target) return false;
    if (!containsOrIs(owned.node, target)) return false;

    if (op.op === OpCode.Attr) {
      // The server echoes the field value back as data-sky-value; that is the
      // reconciliation signal, not a mutation to apply blindly.
      return true;
    }
    owned.deferred.push(op);
    return true;
  }

  /** Reconciles the server's authoritative value for the owned field. */
  reconcile(nodeId: number, serverValue: string): void {
    const owned = this.owned;
    if (!owned || owned.id !== nodeId) return;
    const local = valueOf(owned.node);
    if (local === serverValue) return; // confirmation, nothing to do

    // The server changed the text under us: an input mask, an autocomplete, a
    // mention expansion. Server truth wins, but the caret is mapped through the
    // edit so typing continues where the user left off.
    const caret = caretOf(owned.node);
    const mapped = mapCaret(local, serverValue, caret.start);
    setValue(owned.node, serverValue);
    setCaret(owned.node, mapped);
    owned.local = serverValue;
  }

  /**
   * Records an edit the shell applied to the owned field's DOM itself — a
   * clipboard paste or cut from the context menu. Without this the next input
   * event would diff against a value that is two edits old and send the server
   * a bogus insertion.
   */
  noteValue(nodeId: number, value: string): void {
    if (this.owned?.id === nodeId) this.owned.local = value;
  }

  /** Removes an optimistic ghost once the real message arrives. */
  retireGhost(text: string): void {
    const ghost = this.ghosts.get(text);
    if (!ghost) return;
    this.ghosts.delete(text);
    ghost.remove?.();
  }

  /** Drops ownership without applying anything (used on resync). */
  release(): void {
    this.owned = null;
  }
}

const CONTROL_KEYS = new Set([
  'Enter', 'Tab', 'Backspace', 'Delete', 'Escape',
  'ArrowUp', 'ArrowDown', 'ArrowLeft', 'ArrowRight',
  'Home', 'End', 'PageUp', 'PageDown',
]);

export function modifierMask(ev: KeyboardEvent | MouseEvent): number {
  return (ev.altKey ? 1 : 0) | (ev.ctrlKey ? 2 : 0) | (ev.metaKey ? 4 : 0) | (ev.shiftKey ? 8 : 0);
}

/**
 * Input types whose value settles through a native widget rather than typing:
 * a slider's thumb, a colour swatch, a date's calendar. The echo engine must
 * not own these — owning a slider meant one whole-value frame per tick of a
 * drag — so they relay once, on change, from the host (P-111). Checkboxes,
 * radios and the button types are excluded for the neighbouring reason: their
 * gesture is the click, which already replays landside, and echo ownership
 * was sending a meaningless value frame after every toggle.
 */
export const PICKER_INPUTS = new Set([
  'range', 'color', 'date', 'time', 'datetime-local', 'month', 'week',
]);

const NON_TEXT_INPUTS = new Set([
  ...PICKER_INPUTS,
  'checkbox', 'radio', 'file', 'button', 'submit', 'reset', 'image', 'hidden',
]);

export function asEditable(target: EventTarget | null): HTMLElement | null {
  if (!target || !(target as HTMLElement).tagName) return null;
  const el = target as HTMLElement;
  const tag = el.tagName.toUpperCase();
  if (tag === 'INPUT') {
    return NON_TEXT_INPUTS.has((el as HTMLInputElement).type) ? null : el;
  }
  if (tag === 'TEXTAREA') return el;
  if (el.isContentEditable) return el;
  if (el.getAttribute?.('data-skyhook-editable') === '1') return el;
  return null;
}

export function valueOf(el: HTMLElement): string {
  if ('value' in el && typeof (el as HTMLInputElement).value === 'string') {
    return (el as HTMLInputElement).value;
  }
  return el.textContent ?? '';
}

export function setValue(el: HTMLElement, value: string): void {
  if ('value' in el && typeof (el as HTMLInputElement).value === 'string') {
    (el as HTMLInputElement).value = value;
    return;
  }
  el.textContent = value;
}

export function caretOf(el: HTMLElement): { start: number; end: number } {
  const input = el as HTMLInputElement;
  if (typeof input.selectionStart === 'number') {
    return { start: input.selectionStart, end: input.selectionEnd ?? input.selectionStart };
  }
  const sel = el.ownerDocument?.getSelection?.();
  if (sel && sel.rangeCount > 0) {
    const range = sel.getRangeAt(0);
    return { start: range.startOffset, end: range.endOffset };
  }
  return { start: 0, end: 0 };
}

export function setCaret(el: HTMLElement, pos: number): void {
  const input = el as HTMLInputElement;
  if (typeof input.setSelectionRange === 'function' && typeof input.selectionStart === 'number') {
    try {
      input.setSelectionRange(pos, pos);
    } catch {
      // Number and email inputs refuse selection ranges; not worth caring.
    }
    return;
  }
  const doc = el.ownerDocument;
  const sel = doc?.getSelection?.();
  const node = el.firstChild;
  if (!sel || !doc || !node) return;
  const range = doc.createRange();
  const max = node.nodeValue?.length ?? 0;
  range.setStart(node, Math.min(pos, max));
  range.collapse(true);
  sel.removeAllRanges();
  sel.addRange(range);
}

/**
 * Maps a caret offset from the local string onto the server's string, by
 * keeping the caret the same distance from the end of the common suffix. This
 * is what keeps typing usable through an input mask that inserts characters
 * ahead of the caret.
 */
export function mapCaret(local: string, server: string, caret: number): number {
  let prefix = 0;
  const maxPrefix = Math.min(local.length, server.length);
  while (prefix < maxPrefix && local[prefix] === server[prefix]) prefix++;
  if (caret <= prefix) return caret;

  let suffix = 0;
  while (
    suffix < maxPrefix - prefix &&
    local[local.length - 1 - suffix] === server[server.length - 1 - suffix]
  ) {
    suffix++;
  }
  const fromEnd = local.length - caret;
  if (fromEnd <= suffix) return server.length - fromEnd;
  return Math.min(server.length, caret + (server.length - local.length));
}

function containsOrIs(root: Node, node: Node): boolean {
  return root === node || root.contains(node);
}
