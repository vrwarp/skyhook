/**
 * Encoding invariants for the client -> server direction.
 *
 * The server decodes into typed Go structs with integer fields. cbor-x switches
 * to float64 for any integer above 2^32-1, and the Go decoder refuses to put a
 * float into an int64 — silently rejecting the whole frame. That bug shipped
 * once (a wall-clock `Date.now()` timestamp) and dropped every keystroke and
 * click; these tests exist so it cannot happen twice.
 *
 * The file also writes ../testdata/client-frames.json, which the Go test suite
 * decodes to prove the two implementations agree in this direction too.
 */
import { describe, expect, it } from 'vitest';
import { mkdirSync, writeFileSync } from 'node:fs';
import { dirname } from 'node:path';
import { decode as cborDecode } from 'cbor-x';

import {
  ackBody, adapterCommandBody, encodeFrame, helloBody, imageWantBody, inputBody,
  navigateBody, resyncBody, safeInt, scrollBody, viewportBody,
} from '../src/shared/codec.js';
import { FrameType } from '../src/shared/protocol.js';

/** CBOR major type 7 covers floats and simple values. */
function containsFloat(bytes: Uint8Array): boolean {
  // Valid only for frames built from ASCII strings and small integers, which is
  // what every client -> server frame is: no payload byte can collide with a
  // float header.
  return bytes.includes(0xfb) || bytes.includes(0xfa) || bytes.includes(0xf9);
}

describe('safeInt', () => {
  it('keeps ordinary values untouched', () => {
    expect(safeInt(0)).toBe(0);
    expect(safeInt(4096)).toBe(4096);
    expect(safeInt(4294967295)).toBe(4294967295);
  });

  it('clamps anything the encoder would turn into a float', () => {
    expect(safeInt(Date.now())).toBe(0xffffffff);
    expect(safeInt(2 ** 40)).toBe(0xffffffff);
  });

  it('survives rubbish', () => {
    // A non-finite value is a programming error either way; zero is the least
    // harmful integer to put on the wire.
    expect(safeInt(NaN)).toBe(0);
    expect(safeInt(Infinity)).toBe(0);
    expect(safeInt(1.7)).toBe(1);
  });
});

describe('client frames encode integers as CBOR integers', () => {
  const frames: Record<string, Uint8Array> = {
    input: encodeFrame(FrameType.Input, 1, inputBody({
      kind: 'click', node: 42, seq: 7, modifiers: 8, button: 0,
      // A caller passing wall-clock time must not be able to break the frame.
      ts: Date.now(), expectSeq: 3,
    })),
    text: encodeFrame(FrameType.Input, 1, inputBody({
      kind: 'text', node: 42, seq: 8, text: 'hello', ts: 1234,
    })),
    ack: encodeFrame(FrameType.Ack, 2, ackBody(2, 99, 0xdeadbeef)),
    scroll: encodeFrame(FrameType.Scroll, 1, scrollBody({
      tab: 1, x: 0, y: 4096, h: 900, docH: 120000,
    })),
    resync: encodeFrame(FrameType.Resync, 1, resyncBody(1, 12, 'gap')),
    navigate: encodeFrame(FrameType.Navigate, 1, navigateBody('https://example.test/', '')),
    viewport: encodeFrame(FrameType.Viewport, 0, viewportBody({
      w: 1280, h: 800, dpr: 1, mobile: false,
    })),
    imageWant: encodeFrame(FrameType.ImageWant, 1, imageWantBody(['deadbeef'], ['cafebabe'])),
    adapter: encodeFrame(FrameType.AdapterCmd, 0, adapterCommandBody({
      adapter: 'googlechat', cmd: 'send', space: 's1', text: 'see you at the gate',
      localId: 'local-1',
    })),
    hello: encodeFrame(FrameType.Hello, 0, helloBody({
      token: 'conformance-token', caps: ['zstd'],
      viewport: { w: 1280, h: 800, dpr: 1, mobile: false },
      resume: [{ tab: 1, seq: 9, hash: 0xdeadbeef }],
      client: 'conformance',
    })),
  };

  for (const [name, bytes] of Object.entries(frames)) {
    it(`${name} contains no float-encoded numbers`, () => {
      expect(containsFloat(bytes)).toBe(false);
    });
  }

  it('a wall-clock timestamp is clamped rather than promoted to a float', () => {
    const decoded = cborDecode(frames.input) as Record<number, Record<number, unknown>>;
    // Field 12 of an input event is the timestamp.
    expect(decoded[5][12]).toBe(0xffffffff);
    expect(Number.isInteger(decoded[5][12])).toBe(true);
  });

  it('writes fixtures for the Go conformance test', () => {
    const path = new URL('../../testdata/client-frames.json', import.meta.url).pathname;
    mkdirSync(dirname(path), { recursive: true });
    const out: Record<string, string> = {};
    for (const [name, bytes] of Object.entries(frames)) {
      out[name] = Buffer.from(bytes).toString('base64');
    }
    writeFileSync(path, `${JSON.stringify(out, null, 2)}\n`);
    expect(Object.keys(out).length).toBeGreaterThan(5);
  });
});
