/**
 * Rejoining the session across a page load.
 *
 * A Skyhook tab is a real Chromium tab on the VPS. It outlives the page that
 * was showing it, and it is the only copy: nothing about it is reconstructible
 * plane-side, and re-fetching one costs a page over a link where a page costs
 * seconds. So the one thing a load must not do is arrive as a stranger.
 *
 * The client stored the session id from the moment there was one, and never
 * read it back. Every load therefore sent a Hello with no session, was given a
 * fresh and empty one, and left the reader's tabs running landside with nothing
 * able to reach them — an empty strip and a blank frame, until the 12 h TTL
 * eventually collected the session they belonged to.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { decode as cborDecode } from 'cbor-x';

import { encodeFrame, frameMessage, unframeMessage } from '../src/shared/codec.js';
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

/** What the shell sends once it has read its pairing, and its session, back. */
function configure(sessionId = ''): void {
  command('configure', {
    pairing: {
      url: 'https://skyhook.example.com/skyhook',
      fallbackUrl: 'wss://skyhook.example.com/skyhook',
      token: 'a-token',
      preferFallback: true,
    },
    sessionId,
    viewport: { w: 1280, h: 800, dpr: 1, mobile: false },
  });
}

/** A Welcome naming a session and the tabs it is holding. */
function welcome(sessionId: string, tabs: number[] = []): Uint8Array {
  return frameMessage(Channel.Ctrl, encodeFrame(FrameType.Welcome, 0, new Map<number, unknown>([
    [F.welcome.version, PROTOCOL_VERSION],
    [F.welcome.sessionId, sessionId],
    [F.welcome.resumed, true],
    [F.welcome.keepaliveMs, 15000],
    [F.welcome.tabs, tabs.map((tab) => new Map<number, unknown>([
      [F.tabRef.tab, tab],
      [F.tabRef.url, `https://example.com/${tab}`],
      [F.tabRef.active, tab === tabs[0]],
    ]))],
  ])));
}

/** The session id in the first Hello a socket was given, '' if it named none. */
function helloSession(s: FakeSocket): string {
  const frame = cborDecode(unframeMessage(s.sent[0]).payload) as Record<number, unknown>;
  const body = frame[F.frame.body] as Record<number, unknown>;
  return (body[F.hello.sessionId] as string | undefined) ?? '';
}

/** Which tabs the worker asked for a snapshot of, cold. */
function resyncedTabs(): number[] {
  const s = FakeSocket.all[0];
  return s.sent
    .map((msg) => cborDecode(unframeMessage(msg).payload) as Record<number, unknown>)
    .filter((f) => f[F.frame.type] === FrameType.Resync)
    .map((f) => f[F.frame.tab] as number);
}

describe('a page load rejoins the session it left behind', () => {
  it('names the stored session in its first Hello', async () => {
    configure('session-from-a-previous-load');
    await vi.advanceTimersByTimeAsync(0);
    FakeSocket.all[0].accept();
    await vi.advanceTimersByTimeAsync(0);

    expect(helloSession(FakeSocket.all[0])).toBe('session-from-a-previous-load');
  });

  it('asks for a snapshot of every tab the session hands back', async () => {
    configure('session-from-a-previous-load');
    await vi.advanceTimersByTimeAsync(0);
    FakeSocket.all[0].accept();
    await vi.advanceTimersByTimeAsync(0);
    FakeSocket.all[0].deliver(welcome('session-from-a-previous-load', [1, 4, 7]));
    await vi.advanceTimersByTimeAsync(0);

    // A load holds no document for any of them, and a diff cannot build one.
    expect(resyncedTabs()).toEqual([1, 4, 7]);
    const w = posted.find((p) => p.kind === 'welcome');
    expect((w?.args.tabs as { tab: number }[]).map((t) => t.tab)).toEqual([1, 4, 7]);
  });

  it('sends no session at all when it has never had one', async () => {
    configure('');
    await vi.advanceTimersByTimeAsync(0);
    FakeSocket.all[0].accept();
    await vi.advanceTimersByTimeAsync(0);

    // Not the empty string on the wire: the field is omitted, so a first-ever
    // load costs nothing and the server reads it as "give me a new session".
    expect(helloSession(FakeSocket.all[0])).toBe('');
  });

  /**
   * The shell re-configures the worker whenever a pairing is pasted, and it
   * offers the session it read at startup each time. By then the worker may
   * have been welcomed into a newer one — the id it holds is what replaced the
   * stored one, and reverting to it would ask the server for a session that has
   * already been superseded.
   */
  it('keeps the session it was welcomed into over the one the shell stored', async () => {
    configure('stale-session');
    await vi.advanceTimersByTimeAsync(0);
    FakeSocket.all[0].accept();
    await vi.advanceTimersByTimeAsync(0);
    FakeSocket.all[0].deliver(welcome('live-session'));
    await vi.advanceTimersByTimeAsync(0);

    configure('stale-session');
    await vi.advanceTimersByTimeAsync(0);
    const dialled = FakeSocket.all[FakeSocket.all.length - 1];
    dialled.accept();
    await vi.advanceTimersByTimeAsync(0);

    expect(helloSession(dialled)).toBe('live-session');
  });
});
