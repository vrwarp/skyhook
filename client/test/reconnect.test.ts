/**
 * The reconnect loop, driven against a server that behaves like the real one.
 *
 * These exist because of a failure that took the app apart behind a reverse
 * proxy and looked like a bad link: the client connected and disconnected once
 * a second, for as long as the page stayed open, resyncing every tab each time.
 * Nothing about it was a link problem. The client was dialling a second
 * connection on top of a working one, the server was handing the session to the
 * newcomer and hanging up on the incumbent, and the client was reading that
 * hang-up as an outage worth reconnecting from.
 *
 * So the property under test is not "it reconnects" — it always did — but how
 * many connections it holds, and whether the loop can sustain itself.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { encodeFrame, frameMessage } from '../src/shared/codec.js';
import { Channel, CloseCode, F, FrameType, PROTOCOL_VERSION } from '../src/shared/protocol.js';

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

  /** The client closing it. The server hanging up is `hangUp`. */
  close(): void {
    this.hangUp(1000, '');
  }

  hangUp(code: number, reason: string): void {
    if (this.readyState === 3) return;
    this.readyState = 3;
    this.onclose?.({ code, reason });
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

  get live(): boolean {
    return this.readyState !== 3;
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

function live(): FakeSocket[] {
  return FakeSocket.all.filter((s) => s.live);
}

function statusFlips(): unknown[] {
  return posted.filter((p) => p.kind === 'status').map((p) => p.args.online);
}

/** The Welcome a server sends once the Hello checks out. */
function welcome(): Uint8Array {
  return frameMessage(Channel.Ctrl, encodeFrame(FrameType.Welcome, 0, new Map<number, unknown>([
    [F.welcome.version, PROTOCOL_VERSION],
    [F.welcome.sessionId, 'session-1'],
    [F.welcome.resumed, true],
    [F.welcome.keepaliveMs, 15000],
  ])));
}

/**
 * Session.Attach, landside: the newest connection gets the session and the one
 * it replaced is hung up on. Every connection that gets this far is welcomed,
 * which is the part that pinned the client's backoff at its floor.
 */
function serverAccept(s: FakeSocket, evictionCode = 4000 + CloseCode.Replaced): void {
  for (const other of FakeSocket.all) {
    if (other !== s && other.live) other.hangUp(evictionCode, 'replaced by newer connection');
  }
  s.accept();
  s.deliver(welcome());
}

/** Answers whatever has dialled in, for `seconds` of wall clock. */
async function runServer(seconds: number, evictionCode?: number): Promise<void> {
  for (let i = 0; i < seconds * 10; i++) {
    for (const s of FakeSocket.all) {
      if (s.readyState === 0) serverAccept(s, evictionCode);
    }
    await settle(100);
  }
}

function configure(): void {
  command('configure', {
    pairing: {
      url: 'https://skyhook.example.com/skyhook',
      fallbackUrl: 'wss://skyhook.example.com/skyhook',
      token: 'a-token',
      preferFallback: true,
    },
    viewport: { w: 1280, h: 800, dpr: 1, mobile: false },
  });
}

describe('the network worker holds exactly one connection', () => {
  it('does not dial a second one behind a reconnect that already worked', async () => {
    configure();
    await settle();
    serverAccept(FakeSocket.all[0]);
    await settle();
    expect(live()).toHaveLength(1);

    // The command the shell sends when the reader comes back to a link it
    // thinks is down. It closes, which arms a reconnect, and then dials at
    // once — so the armed one has a live connection to land on top of.
    command('reconnect');
    await runServer(30);

    expect(FakeSocket.all).toHaveLength(2);
    expect(live()).toHaveLength(1);
  });

  it('recovers from an ordinary drop with a single dial', async () => {
    configure();
    await settle();
    serverAccept(FakeSocket.all[0]);
    await settle();

    FakeSocket.all[0].hangUp(1006, ''); // the link, dying the way links die
    await runServer(30);

    expect(FakeSocket.all).toHaveLength(2);
    expect(live()).toHaveLength(1);
  });

  it('stops rather than taking a session back from whoever replaced it', async () => {
    configure();
    await settle();
    serverAccept(FakeSocket.all[0]);
    await settle();

    // Another window — the installed app beside the browser tab — resumes the
    // same session. Nothing here dialled it, so the guard above cannot help.
    const other = new FakeSocket('wss://skyhook.example.com/skyhook');
    serverAccept(other);
    await runServer(30);

    expect(live()).toEqual([other]);
    const last = posted.filter((p) => p.kind === 'status').pop();
    expect(last?.args.refused).toBe('replaced');
  });

  /**
   * The shape of the original bug, kept as its own case: a session traded back
   * and forth turns over roughly once a second and never decays, because every
   * turn reaches Welcome and resets the backoff.
   */
  it('does not trade a session back and forth once a second', async () => {
    configure();
    await settle();
    serverAccept(FakeSocket.all[0]);
    await settle();

    command('reconnect');
    await runServer(60);

    expect(FakeSocket.all.length).toBeLessThan(4);
    expect(statusFlips().length).toBeLessThan(6);
  });

  /**
   * A server that still says CloseNormal — an older build, or a proxy that
   * loses the code — must not be able to start the loop either. The guard
   * against dialling a duplicate is what holds when the close code does not.
   */
  it('survives an eviction it cannot tell from a dropped link', async () => {
    configure();
    await settle();
    serverAccept(FakeSocket.all[0], 1000);
    await settle();

    command('reconnect');
    await runServer(60, 1000);

    expect(FakeSocket.all.length).toBeLessThan(4);
  });
});
