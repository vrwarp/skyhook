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

/**
 * True if any number in the frame is float-encoded.
 *
 * This walks the CBOR structure rather than scanning for the float header
 * bytes, because a payload byte collides with one sooner than you would think:
 * the integer 250 encodes as `0x18 0xFA`, and 0xFA is the half-float marker. A
 * byte scan called that a float and failed a frame that was perfectly good.
 *
 * Decoding and inspecting the values instead would not work either — JavaScript
 * has one number type, so a float64 holding 1.7e12 is indistinguishable from an
 * integer after decoding, and that is exactly the bug being guarded against.
 */
function containsFloat(bytes: Uint8Array): boolean {
  let i = 0;
  const skip = (): boolean => {
    if (i >= bytes.length) return false;
    const initial = bytes[i++];
    const major = initial >> 5;
    const info = initial & 0x1f;
    let length = info;
    const view = new DataView(bytes.buffer, bytes.byteOffset);
    if (info === 24) {
      length = bytes[i++];
    } else if (info === 25) {
      length = view.getUint16(i);
      i += 2;
    } else if (info === 26) {
      length = view.getUint32(i);
      i += 4;
    } else if (info === 27) {
      length = Number(view.getBigUint64(i));
      i += 8;
    }
    switch (major) {
      case 0: // unsigned
      case 1: // negative
        return false;
      case 2: // bytes
      case 3: // text
        i += length;
        return false;
      case 4: // array
        for (let n = 0; n < length; n++) if (skip()) return true;
        return false;
      case 5: // map
        for (let n = 0; n < length; n++) if (skip() || skip()) return true;
        return false;
      case 6: // tag
        return skip();
      default: // major 7: simple values and floats
        return info === 25 || info === 26 || info === 27;
    }
  };
  while (i < bytes.length) {
    if (skip()) return true;
  }
  return false;
}

describe('containsFloat', () => {
  it('finds a float wherever it is nested', () => {
    // A CBOR map {1: [0.5]}: float64 header 0xfb inside an array inside a map.
    const nested = new Uint8Array([
      0xa1, 0x01, 0x81, 0xfb, 0x3f, 0xe0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
    ]);
    expect(containsFloat(nested)).toBe(true);
    // Half and single precision too.
    expect(containsFloat(new Uint8Array([0xf9, 0x3c, 0x00]))).toBe(true);
    expect(containsFloat(new Uint8Array([0xfa, 0x3f, 0x80, 0x00, 0x00]))).toBe(true);
  });

  it('is not fooled by an integer whose bytes look like a float header', () => {
    // 250 encodes as 0x18 0xFA, and 0xFA is the single-precision float marker.
    // A byte scan called this a float; a structural walk does not.
    expect(containsFloat(new Uint8Array([0x18, 0xfa]))).toBe(false);
    // [250, 251, 249] — every collision at once.
    expect(containsFloat(new Uint8Array([
      0x83, 0x18, 0xfa, 0x18, 0xfb, 0x18, 0xf9,
    ]))).toBe(false);
    // A text string whose characters collide is still just a string.
    expect(containsFloat(new Uint8Array([0x42, 0xfa, 0xfb]))).toBe(false);
  });
});

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
      // What the pointer really did, which the server replays instead of
      // synthesising: hold, position in the box, and the approach.
      hold: 83, point: [250, 500], path: [100, 200, 0, 140, 260, 16, 180, 300, 21],
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
      client: 'conformance', build: 'conformance-build',
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
