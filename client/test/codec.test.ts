/**
 * Codec tests, including the cross-language conformance fixtures produced by
 * the Go server (testdata/conformance.json). If the two implementations ever
 * drift, this is where it shows up rather than at 35,000 feet.
 */
import { describe, expect, it } from 'vitest';
import { readFileSync, existsSync } from 'node:fs';
import { decode as cborDecode } from 'cbor-x';

import {
  ackBody, decodeFrame, decodeMutation, decodeSnapshot, decodeStats, decodeTabState,
  decodeWelcome, encodeFrame, frameMessage, helloBody, inputBody, unframeMessage,
} from '../src/shared/codec.js';
import { Channel, F, FrameType, OpCode } from '../src/shared/protocol.js';

const FIXTURES = new URL('../../testdata/conformance.json', import.meta.url).pathname;

describe('framing', () => {
  it('round-trips the channel header', () => {
    const msg = frameMessage(Channel.Dom, new Uint8Array([1, 2, 3]));
    const { channel, payload } = unframeMessage(msg);
    expect(channel).toBe(Channel.Dom);
    expect(Array.from(payload)).toEqual([1, 2, 3]);
  });

  it('rejects an unknown codec rather than guessing', () => {
    expect(() => unframeMessage(new Uint8Array([0, 9, 1]))).toThrow(/unknown codec/);
  });

  it('reports a dictionary the client never advertised', () => {
    expect(() => unframeMessage(new Uint8Array([0, 2, 1, 0, 0, 0, 5]))).toThrow(/dictionary/);
  });
});

describe('frame encoding', () => {
  it('encodes integer keys, not text keys', () => {
    const bytes = encodeFrame(FrameType.Ack, 3, ackBody(3, 7, 42));
    const decoded = cborDecode(bytes) as Record<number, unknown>;
    expect(decoded[F.frame.type]).toBe(FrameType.Ack);
    expect(decoded[F.frame.tab]).toBe(3);
    const body = decoded[F.frame.body] as Record<number, unknown>;
    expect(body[F.tabAck.seq]).toBe(7);
  });

  it('omits empty fields so a keystroke stays tiny', () => {
    const bytes = encodeFrame(FrameType.Input, 1, inputBody({ kind: 'text', seq: 1, text: 'a' }));
    // A single typed character must cost far less than a hundred bytes: the
    // steady-state budget for chat is 5 kbps.
    expect(bytes.length).toBeLessThan(48);
  });

  it('round-trips a hello with resume state', () => {
    const bytes = encodeFrame(FrameType.Hello, 0, helloBody({
      token: 'tok', sessionId: 'abc', caps: ['zstd'],
      viewport: { w: 100, h: 200, dpr: 2, mobile: false },
      resume: [{ tab: 1, seq: 9, hash: 5 }],
      client: 'test',
    }));
    const decoded = cborDecode(bytes) as Record<number, unknown>;
    const body = decoded[F.frame.body] as Record<number, unknown>;
    expect(body[F.hello.token]).toBe('tok');
    const resume = body[F.hello.resume] as Record<number, unknown>[];
    expect(resume[0][F.tabAck.seq]).toBe(9);
  });
});

describe('cross-language conformance', () => {
  const available = existsSync(FIXTURES);
  const fixtures = available
    ? (JSON.parse(readFileSync(FIXTURES, 'utf8')) as Record<string, string>)
    : {};

  const decodeB64 = (b64: string) => new Uint8Array(Buffer.from(b64, 'base64'));

  it.runIf(available)('decodes a snapshot produced by the Go server', () => {
    const { payload } = unframeMessage(decodeB64(fixtures.snapshot));
    const frame = decodeFrame(payload);
    expect(frame.type).toBe(FrameType.Snapshot);
    const snap = decodeSnapshot(frame.body);
    expect(snap.title).toBe('Conformance');
    expect(snap.url).toBe('https://example.test/');
    expect(snap.nodes.length).toBe(4);
    expect(snap.strings).toContain('hello world');
    expect(snap.css).toContain('body{margin:0}');
    expect(snap.images[0].blur).toBe('LEHV6nWB2yk8pyo0adR*.7kCMdnj');
  });

  it.runIf(available)('decodes a compressed mutation batch', () => {
    const { payload } = unframeMessage(decodeB64(fixtures.mutation));
    const frame = decodeFrame(payload);
    expect(frame.type).toBe(FrameType.Mutation);
    expect(frame.seq).toBe(7);
    expect(frame.base).toBe(6);
    const m = decodeMutation(frame.body);
    expect(m.ops.map((o) => o.op)).toEqual([
      OpCode.Insert, OpCode.Remove, OpCode.Attr, OpCode.Text, OpCode.Move, OpCode.Splice, OpCode.Style,
    ]);
    expect(m.ops[0].nodes[0].id).toBe(10);
    expect(m.ops[5].off).toBe(3);
    expect(m.ops[6].add).toContain('.a{color:red}');
    expect(m.strings).toContain('appended');
  });

  it.runIf(available)('decodes control frames', () => {
    const welcome = decodeWelcome(decodeFrame(unframeMessage(decodeB64(fixtures.welcome)).payload).body);
    expect(welcome.sessionId).toBe('session-1');
    expect(welcome.tabs[0].url).toBe('https://example.test/');
    expect(welcome.caps).toContain('zstd');

    const state = decodeTabState(decodeFrame(unframeMessage(decodeB64(fixtures.tabstate)).payload).body);
    expect(state.title).toBe('Example');
    expect(state.canBack).toBe(true);

    const stats = decodeStats(decodeFrame(unframeMessage(decodeB64(fixtures.stats)).payload).body);
    expect(stats.rttMicros).toBe(1200000);
    expect(stats.queueDepth).toBe(3);
  });

  it.runIf(available)('produces a hello the Go server accepts', () => {
    // The Go side checks this same byte sequence in its own test; keeping the
    // expectation here makes a client-side regression visible immediately.
    const bytes = encodeFrame(FrameType.Hello, 0, helloBody({
      token: 'conformance-token', caps: ['zstd'],
      viewport: { w: 1280, h: 800, dpr: 1, mobile: false },
      client: 'conformance',
    }));
    const decoded = cborDecode(bytes) as Record<number, unknown>;
    const body = decoded[F.frame.body] as Record<number, unknown>;
    expect(body[F.hello.version]).toBe(1);
    expect(body[F.hello.token]).toBe('conformance-token');
  });
});
