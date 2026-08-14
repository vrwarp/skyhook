import { afterEach, describe, expect, it, vi } from 'vitest';

import { Transport } from '../src/app/transport.js';
import { CloseCode, closeCodeFromSocket, isFatalClose } from '../src/shared/protocol.js';

/**
 * Why the server hung up has to survive the trip through the transport.
 *
 * Without it a refused credential is indistinguishable from a dropped link, and
 * the client answers both the same way: reconnect. Against a server that will
 * never accept this token, that is an unbounded loop — connected, offline,
 * connected — with the actual problem never stated anywhere.
 */

/** The parts of WebSocket this code touches, and nothing else. */
class FakeSocket {
  static last: FakeSocket | null = null;
  binaryType = '';
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  onclose: ((ev: { code: number; reason: string }) => void) | null = null;
  onmessage: ((ev: { data: ArrayBuffer }) => void) | null = null;
  sent: Uint8Array[] = [];

  constructor(public url: string) {
    FakeSocket.last = this;
  }

  send(msg: Uint8Array): void {
    this.sent.push(msg);
  }

  close(): void {
    this.onclose?.({ code: 1000, reason: '' });
  }
}

afterEach(() => {
  vi.unstubAllGlobals();
  FakeSocket.last = null;
});

/** Connects a Transport over a fake socket, and reports what it was told. */
async function connected(): Promise<{
  closes: { reason: string; code?: CloseCode }[];
  socket: FakeSocket;
}> {
  vi.stubGlobal('WebSocket', FakeSocket);
  const closes: { reason: string; code?: CloseCode }[] = [];
  const t = new Transport(
    { url: 'https://server.example.com/skyhook', preferFallback: true },
    {
      onMessage: () => undefined,
      onOpen: () => undefined,
      onClose: (reason, code) => closes.push({ reason, code }),
    },
  );
  const opening = t.connect();
  const socket = FakeSocket.last;
  if (!socket) throw new Error('no socket was opened');
  socket.onopen?.();
  await opening;
  return { closes, socket };
}

describe('socket close codes', () => {
  it('reads the server code out of the private range', () => {
    expect(closeCodeFromSocket(4002)).toBe(CloseCode.Unauthorized);
    expect(closeCodeFromSocket(4003)).toBe(CloseCode.VersionMismatch);
  });

  it('treats an ordinary close as no reason at all', () => {
    expect(closeCodeFromSocket(1000)).toBe(CloseCode.Normal);
    expect(closeCodeFromSocket(1006)).toBe(CloseCode.Normal);
  });

  it('marks only the closes a reconnect cannot fix', () => {
    expect(isFatalClose(CloseCode.Unauthorized)).toBe(true);
    expect(isFatalClose(CloseCode.VersionMismatch)).toBe(true);
    expect(isFatalClose(CloseCode.SetupFailed)).toBe(false);
    expect(isFatalClose(CloseCode.Normal)).toBe(false);
    expect(isFatalClose(undefined)).toBe(false);
  });
});

describe('Transport', () => {
  it('reports a rejected token as a rejection, not as an outage', async () => {
    const { closes, socket } = await connected();
    socket.onclose?.({ code: 4002, reason: 'unauthorized' });

    expect(closes).toHaveLength(1);
    expect(closes[0].code).toBe(CloseCode.Unauthorized);
    expect(closes[0].reason).toContain('unauthorized');
  });

  it('reports a dropped link as retryable', async () => {
    const { closes, socket } = await connected();
    socket.onclose?.({ code: 1006, reason: '' });

    expect(closes).toHaveLength(1);
    expect(isFatalClose(closes[0].code)).toBe(false);
  });

  it('only reports the first close', async () => {
    const { closes, socket } = await connected();
    socket.onclose?.({ code: 4002, reason: 'unauthorized' });
    socket.onclose?.({ code: 1006, reason: '' });

    expect(closes).toHaveLength(1);
  });
});
