/**
 * The reader's input survives the link going, whichever way it goes.
 *
 * Typing during an outage is already kept: an input the worker sees while it
 * knows itself offline goes into the outbox and rides the next Hello. What was
 * not kept is the input that crosses the boundary — the send that begins while
 * the worker still believes it is online and fails on the way out. On this
 * link, which drops every few minutes, that is exactly the last tap before the
 * bars went, and it vanished with nothing anywhere recording it: no queue
 * entry, no error, no retry. The reader tapped, nothing happened, and the only
 * evidence was their memory of having tapped.
 *
 * This is the plane-side half of the same class of bug as the server's wedged
 * tab queue: an input accepted and then silently dropped on a serial path.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { decode as cborDecode } from 'cbor-x';

import { encodeFrame, frameMessage, unframeMessage } from '../src/shared/codec.js';
import { Channel, F, FrameType, PROTOCOL_VERSION } from '../src/shared/protocol.js';

class FakeSocket {
  static all: FakeSocket[] = [];
  /** Set to make every send throw, which is a link that has gone under a
   *  worker that has not been told yet. */
  static refuseSends = false;
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
    if (FakeSocket.refuseSends) throw new Error('the link went');
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
  FakeSocket.refuseSends = false;
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

function welcomeFrame(sessionId: string): Uint8Array {
  return frameMessage(Channel.Ctrl, encodeFrame(FrameType.Welcome, 0, new Map<number, unknown>([
    [F.welcome.version, PROTOCOL_VERSION],
    [F.welcome.sessionId, sessionId],
    [F.welcome.resumed, true],
    [F.welcome.keepaliveMs, 15000],
    [F.welcome.tabs, []],
  ])));
}

async function connect(): Promise<FakeSocket> {
  command('configure', {
    pairing: {
      url: 'https://skyhook.example.com/skyhook',
      fallbackUrl: 'wss://skyhook.example.com/skyhook',
      token: 'a-token',
      preferFallback: true,
    },
    viewport: { w: 448, h: 851, dpr: 3, mobile: true },
  });
  await settle();
  const sock = FakeSocket.all[FakeSocket.all.length - 1];
  sock.accept();
  sock.deliver(welcomeFrame('session-1'));
  await settle();
  sock.sent.length = 0;
  posted.length = 0;
  return sock;
}

/** The input frames inside the Hello this side sends on reconnecting. */
function queuedInHello(sock: FakeSocket): unknown[] {
  for (const raw of sock.sent) {
    const frame = cborDecode(unframeMessage(raw).payload) as Record<number, unknown>;
    if (frame[F.frame.type] !== FrameType.Hello) continue;
    const body = frame[F.frame.body] as Record<number, unknown>;
    return (body[F.hello.queued] as unknown[] | undefined) ?? [];
  }
  return [];
}

function tap(tab: number, node: number): void {
  command('input', { tab, kind: 'click', node });
}

describe('an input the link drops', () => {
  it('is kept for the next connection rather than lost', async () => {
    const sock = await connect();

    // The link goes without the worker being told: the socket refuses, and
    // nothing has closed it. This is the ordinary shape of a radio link
    // dropping — the failure is discovered by writing to it.
    FakeSocket.refuseSends = true;
    tap(1, 1166);
    await settle();

    expect(sock.sent.length, 'the tap cannot have crossed a link that refused it').toBe(0);

    // Reconnect, and the tap has to be in the Hello. The outbox exists so that
    // what the reader did during an outage survives it; an outage that began
    // mid-send is the same outage.
    FakeSocket.refuseSends = false;
    sock.close();
    await settle(1000);
    const next = FakeSocket.all[FakeSocket.all.length - 1];
    next.accept();
    await settle();

    expect(queuedInHello(next).length,
      'the tap the link refused was dropped: the reader tapped, nothing happened, ' +
      'and nothing anywhere recorded that they had').toBe(1);
  });

  it('still rides the Hello when the worker already knew it was offline', async () => {
    const sock = await connect();
    sock.close();
    await settle();

    tap(1, 1166);
    await settle();

    await settle(1000);
    const next = FakeSocket.all[FakeSocket.all.length - 1];
    next.accept();
    await settle();

    expect(queuedInHello(next).length).toBe(1);
  });
});
