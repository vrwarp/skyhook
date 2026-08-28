/**
 * Local echo tests: the G2 feature. Losing a keystroke is the one failure this
 * code must never have, so the tests are written around that.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { EchoEngine, mapCaret } from '../src/mirror/echo.js';

interface Sent {
  text: string[];
  keys: string[];
  values: string[];
  focus: boolean[];
}

function setup() {
  document.body.innerHTML = '<input id="box" type="text"><div id="log"></div>';
  const input = document.getElementById('box') as HTMLInputElement;
  const sent: Sent = { text: [], keys: [], values: [], focus: [] };
  const echo = new EchoEngine(document, {
    idOf: (node: Node | null) => (node === input ? 42 : 0),
    sendText: (_node: number, text: string) => sent.text.push(text),
    sendKey: (_node: number, key: string) => sent.keys.push(key),
    sendValue: (_node: number, value: string) => sent.values.push(value),
    sendFocus: (_node: number, focused: boolean) => sent.focus.push(focused),
    onChatSend: () => true,
  });
  // Events are dispatched for real rather than synthesised with a forged
  // target: the shim wires these same listeners up.
  document.addEventListener('input', (ev) => echo.input(ev as InputEvent), true);
  const type = (value: string): void => {
    input.value = value;
    input.dispatchEvent(new InputEvent('input', { bubbles: true }));
  };
  return { echo, input, sent, type };
}

describe('EchoEngine', () => {
  beforeEach(() => {
    document.body.innerHTML = '';
  });

  it('takes ownership on focus and reports it landside', () => {
    const { echo, input, sent } = setup();
    echo.focus(input);
    expect(echo.ownedId).toBe(42);
    expect(echo.isOwned(input)).toBe(true);
    expect(sent.focus).toEqual([true]);
  });

  it('sends only the inserted characters while typing', () => {
    const { echo, input, sent, type } = setup();
    echo.focus(input);
    type('h');
    type('he');
    expect(sent.text).toContain('e');
  });

  it('falls back to the whole value when the edit is not an append', () => {
    const { echo, input, sent, type } = setup();
    echo.focus(input);
    type('hello');
    type('helo');
    expect(sent.values).toContain('helo');
  });

  it('holds server mutations that touch the owned field', () => {
    const { echo, input } = setup();
    echo.focus(input);
    const op = { op: 4, node: 42 } as never;
    expect(echo.defer(op, () => input)).toBe(true);
    // Nodes outside the owned subtree are not deferred.
    const other = document.getElementById('log') as HTMLElement;
    expect(echo.defer(op, () => other)).toBe(false);
  });

  it('replays buffered server mutations on blur', () => {
    const { echo, input } = setup();
    echo.focus(input);
    echo.defer({ op: 4, node: 42 } as never, () => input);
    const apply = vi.fn();
    echo.blur(apply);
    expect(apply).toHaveBeenCalledTimes(1);
    expect(echo.ownedId).toBe(0);
  });

  /*
   * A focus op is about a moment, not about content.
   *
   * Holding one is worse than applying it. The host ignores a focus echo
   * that arrives while the reader owns a field, but a *deferred* one is
   * replayed at blur — the instant the reader leaves — and lands past both
   * guards, because a replayed op carries no cause. The caret jumps back
   * into the field they just left, and on a phone the keyboard comes back
   * up with it. Found by the keyboard-shortcut corpus page, whose click out
   * of a field kept landing back in it (P-142).
   */
  it('never holds a focus op to replay at blur', () => {
    const { echo, input } = setup();
    echo.focus(input);
    // OpCode.Focus is 9.
    const focusOp = { op: 9, node: 42 } as never;
    expect(echo.defer(focusOp, () => input)).toBe(false);
    const apply = vi.fn();
    echo.blur(apply);
    expect(apply).not.toHaveBeenCalled();
  });

  it('drops a server value identical to the local one', () => {
    const { echo, input } = setup();
    echo.focus(input);
    input.value = 'hello';
    echo.reconcile(42, 'hello');
    expect(input.value).toBe('hello');
  });

  it('lets server truth win when an input mask rewrites the text', () => {
    const { echo, input } = setup();
    echo.focus(input);
    input.value = '1234567890';
    input.setSelectionRange(10, 10);
    echo.reconcile(42, '(123) 456-7890');
    expect(input.value).toBe('(123) 456-7890');
    // The caret stays at the end of what the user typed, not back at zero.
    expect(input.selectionStart).toBeGreaterThan(9);
  });

  it('clears the composer and sends Enter for a chat send', () => {
    const { echo, input, sent, type } = setup();
    echo.focus(input);
    type('hello world');
    const handled = echo.key(new KeyboardEvent('keydown', { key: 'Enter' }));
    expect(handled).toBe(true);
    expect(input.value).toBe('');
    expect(sent.keys).toContain('Enter');
  });

  it('forwards control keys but lets Backspace edit natively', () => {
    const { echo, input, sent } = setup();
    echo.focus(input);
    expect(echo.key(new KeyboardEvent('keydown', { key: 'Backspace' }))).toBe(false);
    expect(echo.key(new KeyboardEvent('keydown', { key: 'ArrowUp' }))).toBe(true);
    expect(echo.key(new KeyboardEvent('keydown', { key: 'a' }))).toBe(false);
    expect(sent.keys).toContain('Backspace');
    expect(sent.keys).toContain('ArrowUp');
    expect(sent.keys).not.toContain('a');
  });
});

describe('mapCaret', () => {
  it('keeps the caret where the user left it when a prefix is inserted', () => {
    expect(mapCaret('1234', '(1234', 4)).toBe(5);
  });

  it('leaves the caret alone when the edit is after it', () => {
    expect(mapCaret('abc', 'abcdef', 1)).toBe(1);
  });

  it('never returns an offset past the end of the server string', () => {
    expect(mapCaret('abcdefgh', 'ab', 8)).toBeLessThanOrEqual(2);
  });
});
