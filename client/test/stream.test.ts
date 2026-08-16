/**
 * What the worker does with the sequence numbers on a stream of mutations.
 *
 * These exist because of a page whose text arrived shredded: a chat answer
 * streaming in three characters at a time landed with its characters in the
 * wrong nodes, mixed with a second answer streaming beside it, and stayed that
 * way for the life of the tab.
 *
 * None of it was a rendering bug. The worker decided whether an arriving batch
 * was new, and whether the one before it was missing, from the sequence the
 * *shell* had acknowledged — a number that lags the link by a postMessage, an
 * iframe apply and a postMessage back. So every batch still in that pipeline
 * read as a gap; the server answered each supposed gap by replaying batches the
 * client already had; and the replays read as new, because the ack that would
 * have said otherwise was itself still in the pipeline.
 *
 * Applying one twice is what wrecked the text. A batch's strings extend an
 * append-only intern table by position, so a duplicate leaves the table one
 * entry long and every string reference after it resolves to its neighbour.
 *
 * The properties here are therefore about arithmetic, not about pixels: a batch
 * is handed to the shell exactly once, a batch whose predecessor is merely
 * unacknowledged is not a gap, and a real gap is asked about once rather than
 * once per frame that arrives behind it.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { decodeFrame, encodeFrame, frameMessage, unframeMessage } from '../src/shared/codec.js';
import { Channel, F, FrameType, PROTOCOL_VERSION } from '../src/shared/protocol.js';

/** The parts of WebSocket the transport touches, driven by hand. */
class FakeSocket {
  static all: FakeSocket[] = [];
  binaryType = '';
  readyState = 0;
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  onclose: ((ev: { code: number; reason: string }) => void) | null = null;
  onmessage: ((ev: { data: ArrayBuffer }) => void) | null = null;
  sent: Uint8Array[] = [];

  constructor(public url: string) {
    FakeSocket.all.push(this);
  }

  send(msg: Uint8Array): void {
    this.sent.push(msg);
  }

  close(): void {
    this.readyState = 3;
    this.onclose?.({ code: 1000, reason: '' });
  }

  accept(): void {
    this.readyState = 1;
    this.onopen?.();
  }

  deliver(msg: Uint8Array): void {
    const buf = new ArrayBuffer(msg.length);
    new Uint8Array(buf).set(msg);
    this.onmessage?.({ data: buf });
  }
}

interface Posted { kind: string; args: Record<string, unknown> }

let posted: Posted[] = [];
let onWorkerMessage: ((ev: { data: unknown }) => void) | null = null;

beforeEach(async () => {
  posted = [];
  onWorkerMessage = null;
  FakeSocket.all = [];
  vi.useFakeTimers();
  vi.stubGlobal('self', {
    addEventListener: (name: string, fn: (ev: { data: unknown }) => void) => {
      if (name === 'message') onWorkerMessage = fn;
    },
    postMessage: (m: Posted) => posted.push(m),
  });
  vi.stubGlobal('WebSocket', FakeSocket);
  vi.stubGlobal('caches', { open: () => Promise.resolve({ match: () => Promise.resolve(null) }) });
  vi.resetModules();
  await import('../src/worker/net.worker.js');
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

function command(name: string, args: Record<string, unknown> = {}): void {
  onWorkerMessage?.({ data: { name, args } });
}

async function settle(ms = 0): Promise<void> {
  await vi.advanceTimersByTimeAsync(ms);
}

const TAB = 4;

/** Brings the worker up on a live connection with a welcomed session. */
async function connect(): Promise<FakeSocket> {
  command('configure', {
    pairing: {
      url: 'https://skyhook.example.com/skyhook',
      fallbackUrl: 'wss://skyhook.example.com/skyhook',
      token: 'a-token',
      preferFallback: true,
    },
    viewport: { w: 1280, h: 800, dpr: 1, mobile: false },
  });
  await settle();
  const sock = FakeSocket.all[0];
  sock.accept();
  sock.deliver(frameMessage(Channel.Ctrl, encodeFrame(FrameType.Welcome, 0, new Map<number, unknown>([
    [F.welcome.version, PROTOCOL_VERSION],
    [F.welcome.sessionId, 'session-1'],
    [F.welcome.resumed, false],
    [F.welcome.keepaliveMs, 15000],
  ]))));
  await settle();
  sock.sent.length = 0;
  return sock;
}

/** A document with one text node, which is all the sequence checks care about. */
function snapshot(): Uint8Array {
  const body = new Map<number, unknown>();
  body.set(F.snapshot.strings, ['html', 'hello']);
  body.set(F.snapshot.nodes, [
    new Map<number, unknown>([[F.node.id, 1], [F.node.kind, 1], [F.node.ref, 0]]),
  ]);
  body.set(F.snapshot.url, 'https://example.com/');
  return frameMessage(Channel.Dom, encodeFrame(FrameType.Snapshot, TAB, body));
}

/** One batch that interns `text` and appends it to node 1. */
function mutation(seq: number, base: number, text: string): Uint8Array {
  const body = new Map<number, unknown>();
  body.set(F.mutation.strings, [text]);
  body.set(F.mutation.ops, [
    new Map<number, unknown>([
      [F.op.op, 1],
      [F.op.parent, 1],
      [F.op.nodes, [
        new Map<number, unknown>([[F.node.id, seq + 100], [F.node.parent, 1], [F.node.kind, 3]]),
      ]],
    ]),
  ]);
  return frameMessage(Channel.Dom, encodeFrame(FrameType.Mutation, TAB, body, { seq, base }));
}

/** The sequence numbers of the batches handed to the shell, in order. */
function handedOver(): number[] {
  return posted.filter((p) => p.kind === 'mutation').map((p) => Number(p.args.seq));
}

/** Every resync the worker asked for, as (haveTo, reason). */
function resyncs(sock: FakeSocket): { haveTo: number; reason: string }[] {
  return sock.sent
    .map((m) => decodeFrame(unframeMessage(m).payload))
    .filter((f) => f.type === FrameType.Resync)
    .map((f) => {
      const b = f.body as Record<number, unknown>;
      return {
        haveTo: Number(b[F.resync.haveTo] ?? 0),
        reason: String(b[F.resync.reason] ?? ''),
      };
    });
}

describe('a batch reaches the shell exactly once', () => {
  it('drops a replay of a batch the shell has not acknowledged yet', async () => {
    const sock = await connect();
    sock.deliver(snapshot());
    await settle();

    sock.deliver(mutation(1, 0, 'To '));
    sock.deliver(mutation(2, 1, 'fin'));
    // The server answers a resync by replaying from the ring. This is that
    // replay landing before the shell's ack for the same batch got back: the
    // exact 1.2 ms window that shredded the page.
    sock.deliver(mutation(2, 1, 'fin'));
    sock.deliver(mutation(3, 2, 'd t'));
    await settle();

    expect(handedOver()).toEqual([1, 2, 3]);
  });

  it('drops a replay of a batch the shell applied before a reconnect', async () => {
    const sock = await connect();
    sock.deliver(snapshot());
    await settle();

    sock.deliver(mutation(1, 0, 'To '));
    sock.deliver(mutation(2, 1, 'fin'));
    await settle();
    // Only the first was acknowledged; the second's ack was lost with the link.
    // The server resumes from what it was told and replays both.
    command('ack', { tab: TAB, seq: 1, hash: 7 });
    await settle();

    sock.deliver(mutation(2, 1, 'fin'));
    await settle();

    expect(handedOver()).toEqual([1, 2]);
  });
});

describe('a gap is what is missing, not what is unacknowledged', () => {
  it('does not call an unacknowledged predecessor a gap', async () => {
    const sock = await connect();
    sock.deliver(snapshot());
    await settle();

    // A streaming page, batch every ~100 ms, with no ack coming back at all:
    // the shell is busy applying. Nothing here is missing.
    for (let seq = 1; seq <= 10; seq++) {
      sock.deliver(mutation(seq, seq - 1, `c${seq}`));
      await settle(100);
    }

    expect(handedOver()).toEqual([1, 2, 3, 4, 5, 6, 7, 8, 9, 10]);
    expect(resyncs(sock)).toHaveLength(0);
  });

  it('asks once for a batch that really is missing, not once per frame behind it', async () => {
    const sock = await connect();
    sock.deliver(snapshot());
    await settle();

    sock.deliver(mutation(1, 0, 'To '));
    await settle();
    // Batch 2 never arrives. Everything after it is unappliable and keeps
    // coming: this is the shape that produced 1984 resync requests in one
    // session, and the server answered every one of them.
    for (let seq = 3; seq <= 12; seq++) {
      sock.deliver(mutation(seq, seq - 1, `c${seq}`));
      await settle(50);
    }

    expect(handedOver()).toEqual([1]);
    expect(resyncs(sock)).toHaveLength(1);
  });

  it('asks again if the first request went unanswered', async () => {
    const sock = await connect();
    sock.deliver(snapshot());
    await settle();

    sock.deliver(mutation(1, 0, 'To '));
    await settle();
    sock.deliver(mutation(3, 2, 'c3'));
    await settle(2000);
    sock.deliver(mutation(4, 3, 'c4'));
    await settle();

    expect(resyncs(sock)).toHaveLength(2);
  });
});
