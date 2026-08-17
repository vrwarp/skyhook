/**
 * What a closed tab costs, which has to be nothing.
 *
 * From a capture of a phone on a 6.6 s link: reddit opened in a second tab, the
 * app stopped answering, the reader closed the offending tab — and waited two
 * more minutes, during which the tab they kept acknowledged nothing at all.
 *
 * Closing is a request, and its answer is a round trip away. Everything the tab
 * had already sent is still arriving throughout, and everything the server had
 * queued for it keeps coming until the close lands. Each of those frames used
 * to be a frame for a tab this side no longer held, which is the definition of
 * a cold client: the worker asked for a resync, and the server answered with a
 * whole document — for a page that had been closed. On this link that is
 * minutes of the reader's link spent arguing with them.
 *
 * So the properties here are about silence: nothing for a closed tab is
 * decoded, forwarded, acknowledged or asked after again, and a close that could
 * not be sent is said as soon as there is a link to say it on.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { decodeFrame, encodeFrame, frameMessage, unframeMessage } from '../src/shared/codec.js';
import { Channel, F, FrameType, PROTOCOL_VERSION } from '../src/shared/protocol.js';

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

const KEPT = 1;
const KILLED = 2;

function welcomeFrame(sessionId: string, tabs: unknown[] = []): Uint8Array {
  return frameMessage(Channel.Ctrl, encodeFrame(FrameType.Welcome, 0, new Map<number, unknown>([
    [F.welcome.version, PROTOCOL_VERSION],
    [F.welcome.sessionId, sessionId],
    [F.welcome.resumed, true],
    [F.welcome.keepaliveMs, 15000],
    [F.welcome.tabs, tabs],
  ])));
}

function tabRef(tab: number, url: string): Map<number, unknown> {
  return new Map<number, unknown>([
    [F.tabRef.tab, tab],
    [F.tabRef.url, url],
    [F.tabRef.title, 'a page'],
  ]);
}

async function connect(sessionId = 'session-1', tabs: unknown[] = []): Promise<FakeSocket> {
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
  sock.deliver(welcomeFrame(sessionId, tabs));
  await settle();
  sock.sent.length = 0;
  posted.length = 0;
  return sock;
}

/** A snapshot for one tab: the frame a resync answer is made of. */
function snapshot(tab: number): Uint8Array {
  const body = new Map<number, unknown>();
  body.set(F.snapshot.strings, ['html']);
  body.set(F.snapshot.nodes, [
    new Map<number, unknown>([[F.node.id, 1], [F.node.kind, 1], [F.node.ref, 0]]),
  ]);
  body.set(F.snapshot.url, 'https://example.com/');
  return frameMessage(Channel.Dom, encodeFrame(FrameType.Snapshot, tab, body));
}

function mutation(tab: number, seq: number, base: number): Uint8Array {
  const body = new Map<number, unknown>();
  body.set(F.mutation.strings, ['x']);
  body.set(F.mutation.ops, [
    new Map<number, unknown>([[F.op.op, 5], [F.op.node, 1], [F.op.ref, 0]]),
  ]);
  return frameMessage(Channel.Dom, encodeFrame(FrameType.Mutation, tab, body, { seq, base }));
}

function sentTypes(sock: FakeSocket): { type: number; tab: number }[] {
  return sock.sent
    .map((m) => decodeFrame(unframeMessage(m).payload))
    .map((f) => ({ type: f.type, tab: f.tab }));
}

describe('a tab the reader closed', () => {
  it('costs nothing more, whatever the server goes on sending about it', async () => {
    const sock = await connect();
    sock.deliver(snapshot(KILLED));
    await settle();
    posted.length = 0;

    command('closeTab', { tab: KILLED });
    await settle();

    // The document that was already on its way, and the batches behind it.
    sock.deliver(snapshot(KILLED));
    sock.deliver(mutation(KILLED, 1, 0));
    sock.deliver(mutation(KILLED, 2, 1));
    // And a batch for the tab the reader kept, which must still get through.
    sock.deliver(snapshot(KEPT));
    await settle();

    const forKilled = posted.filter((p) => Number(p.args.tab) === KILLED);
    expect(forKilled).toEqual([]);
    expect(posted.filter((p) => p.kind === 'snapshot').map((p) => p.args.tab)).toEqual([KEPT]);
  });

  it('is never asked after again, however far behind it looks', async () => {
    const sock = await connect();
    command('closeTab', { tab: KILLED });
    await settle();
    sock.sent.length = 0;

    // A batch with no snapshot before it is what a cold client sees, and a cold
    // client asks for a whole document. This one must ask for nothing.
    sock.deliver(mutation(KILLED, 7, 6));
    sock.deliver(mutation(KILLED, 8, 7));
    await settle(2000);

    expect(sentTypes(sock).filter((f) => f.type === FrameType.Resync)).toEqual([]);
  });

  it('does not answer for a page that is gone', async () => {
    const sock = await connect();
    command('closeTab', { tab: KILLED });
    await settle();
    sock.sent.length = 0;

    // The shell applied one last batch before the close reached it. Its ack is
    // about a document neither half has any more, and it would put the tab back
    // in this worker's books — which is what the next Hello resumes from.
    command('ack', { tab: KILLED, seq: 3, hash: 99 });
    await settle();

    expect(sentTypes(sock).filter((f) => f.type === FrameType.Ack)).toEqual([]);
  });
});

describe('a close that could not be sent', () => {
  it('is said again on the next link, instead of the tab coming back', async () => {
    const sock = await connect('session-1', [tabRef(KEPT, 'https://news.ycombinator.com/')]);

    // The link goes down, and the reader closes the tab that was drowning them.
    // The frame is dropped: the worker cannot send what it has no socket for.
    sock.close();
    await settle();
    command('closeTab', { tab: KILLED });
    await settle();

    // Reconnecting to the same session, which still has both tabs.
    await settle(1000);
    const next = FakeSocket.all[FakeSocket.all.length - 1];
    next.accept();
    next.deliver(welcomeFrame('session-1', [
      tabRef(KEPT, 'https://news.ycombinator.com/'),
      tabRef(KILLED, 'https://www.reddit.com/'),
    ]));
    await settle();

    const sent = sentTypes(next);
    expect(sent).toContainEqual({ type: FrameType.TabClose, tab: KILLED });
    // And emphatically not a cold resync, which is a whole document for a page
    // the reader closed minutes ago.
    expect(sent.filter((f) => f.type === FrameType.Resync && f.tab === KILLED)).toEqual([]);

    // The shell is never told about it either: a tab that comes back from an
    // outage after the reader closed it is the app arguing with them.
    const welcomed = posted.find((p) => p.kind === 'welcome');
    const tabs = (welcomed?.args.tabs ?? []) as { tab: number }[];
    expect(tabs.map((t) => t.tab)).toEqual([KEPT]);
  });

  it('is forgotten when the session is not the one it was closed in', async () => {
    const sock = await connect();
    command('closeTab', { tab: KILLED });
    await settle();

    // A restarted server numbers its tabs from one again, so tab 2 in the new
    // session is a tab the reader has never seen — and ignoring it would leave
    // a tab in the strip that never draws anything.
    sock.close();
    await settle();
    const next = FakeSocket.all[FakeSocket.all.length - 1];
    next.accept();
    next.deliver(welcomeFrame('session-2', []));
    await settle();
    posted.length = 0;

    next.deliver(snapshot(KILLED));
    await settle();

    expect(posted.filter((p) => p.kind === 'snapshot').map((p) => p.args.tab)).toEqual([KILLED]);
  });
});
