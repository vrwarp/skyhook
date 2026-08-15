/**
 * The network worker: owns the single connection to the server and does all
 * frame decoding off the UI thread.
 *
 * In the Electron client this was a hidden BrowserWindow, because WebTransport
 * is unavailable in Node. A dedicated worker is the natural fit in a PWA and
 * costs less: no second renderer, and CBOR plus zstd decoding never competes
 * with painting the mirror.
 *
 * Image bytes are written straight into Cache Storage from here, so the service
 * worker can serve them to the mirror frame without another hop through the
 * page.
 */
import {
  ackBody, adapterCommandBody, capturePartBody, captureRequestBody, decodeAdapterBatch,
  decodeCaptureDone, decodeCaptureRequest, decodeError, decodeFrame, decodeImageData,
  decodeImageMeta, decodeMutation, decodeSnapshot, decodeStats, decodeTabState, decodeWelcome,
  encodeFrame, frameMessage, helloBody, imageWantBody, inputBody, navigateBody, resyncBody,
  scrollBody, unframeMessage, viewportBody, InputEventInit,
} from '../shared/codec.js';
import { IMAGE_CACHE, imageCacheKey } from '../shared/caches.js';
import {
  Channel, CloseCode, FrameType, isFatalClose, Refusal, Viewport,
} from '../shared/protocol.js';

import { Transport, TransportConfig } from '../app/transport.js';

/** Everything the worker needs to reach the server. */
export interface Pairing extends TransportConfig {
  token: string;
}


interface TabProgress {
  seq: number;
  hash: number;
}

let transport: Transport | null = null;
let pairing: Pairing | null = null;
let sessionId = '';
let viewport: Viewport = { w: 1280, h: 800, dpr: 1, mobile: false };
const progress = new Map<number, TabProgress>();
/** Input frames captured while offline, replayed in the next Hello. */
const outbox: Map<number, unknown>[] = [];
let online = false;
let keepalive: ReturnType<typeof setInterval> | null = null;
let statsTick: ReturnType<typeof setInterval> | null = null;
/** The last stats the server sent; queue depth and loss are only knowable there. */
let serverStats: Record<string, unknown> = {};
let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
let reconnectDelay = 500;
/**
 * Set when the server refused this client outright. Reconnecting on the same
 * credential produces the same refusal, so the loop stops here and waits for a
 * new pairing or an explicit retry — otherwise the app spends forever
 * alternating between "offline" and "connected" a second apart.
 */
let refused: Refusal | null = null;

function refusalFor(code: CloseCode | undefined): Refusal {
  if (code === CloseCode.Unauthorized) return 'unauthorized';
  if (code === CloseCode.VersionMismatch) return 'version';
  return 'replaced';
}

/**
 * Cancels a pending reconnect.
 *
 * This is the whole of a bug that made the app unusable behind a reverse proxy.
 * A reconnect was armed whenever the link went down and cancelled only by
 * firing, so anything that closed the link and then dialled straight away — the
 * `reconnect` command below, which every `visibilitychange` sends — left a timer
 * behind that came due half a second after the new connection was already up.
 * It dialled a second one. The server gives the session to the newest connection
 * and hangs up on the one it replaced, that hang-up read as an outage, and the
 * outage armed the next timer: a connection per second, every tab resynced on
 * each one, for as long as the page stayed open.
 */
function clearReconnect(): void {
  if (!reconnectTimer) return;
  clearTimeout(reconnectTimer);
  reconnectTimer = null;
}

function post(kind: string, args: Record<string, unknown>, transfer: Transferable[] = []): void {
  (self as unknown as Worker).postMessage({ kind, args }, transfer);
}

async function connect(): Promise<void> {
  if (!pairing || refused) return;
  // A connection in hand — or one already on its way — is never improved by a
  // second. The server hands the session to the newest connection and hangs up
  // on the one it replaced, so a duplicate does not merely waste a socket: it
  // evicts the socket that was working.
  if (transport) return;
  clearReconnect();

  const t = new Transport(pairing, {
    onOpen: (kind) => {
      online = true;
      // Note that the backoff is *not* reset here. An open socket is not a
      // working session: a server that rejects the token accepts the socket
      // first and hangs up a moment later, and resetting on open turns that
      // into a tight loop. The reset lives in the Welcome handler, where the
      // connection has actually got somewhere.
      post('status', { online: true, kind });
      sendHello();
    },
    onClose: (reason, code) => {
      // A socket we have already let go of has nothing left to say, and what it
      // would say — "offline" — is wrong: the link it is talking about is not
      // the one the app is on.
      if (transport !== t) return;
      transport = null;
      stopTimers();
      const wasOnline = online;
      online = false;
      if (isFatalClose(code)) {
        refused = refusalFor(code);
        post('status', { online: false, kind: 'none', reason, refused });
        return;
      }
      // An attempt that never opened is the connect() call's to report: it is
      // still on the stack, holding the error that says what went wrong.
      if (!wasOnline) return;
      post('status', { online: false, kind: 'none', reason });
      scheduleReconnect();
    },
    onNotice: (message) => post('log', { message }),
    onMessage: (_channel, msg) => handleMessage(msg),
  });
  transport = t;

  try {
    await t.connect();
  } catch (err) {
    if (transport === t) {
      transport = null;
      stopTimers();
    }
    post('status', { online: false, kind: 'none', reason: String(err) });
    scheduleReconnect();
  }
}

/**
 * Drops whatever connection is up and dials again now.
 *
 * Closing runs `onClose`, which arms a reconnect; that timer is cancelled here
 * rather than left to come due behind the connection this is about to make.
 */
function redial(): void {
  transport?.close();
  clearReconnect();
  void connect();
}

/** Stops everything that only makes sense while a connection is up. */
function stopTimers(): void {
  if (statsTick) {
    clearInterval(statsTick);
    statsTick = null;
  }
  if (keepalive) {
    clearInterval(keepalive);
    keepalive = null;
  }
}

function scheduleReconnect(): void {
  if (reconnectTimer || refused) return;
  // Aggressive at first, then backing off: outages on this link are usually
  // seconds, occasionally minutes.
  reconnectTimer = setTimeout(() => {
    reconnectTimer = null;
    void connect();
  }, reconnectDelay);
  reconnectDelay = Math.min(reconnectDelay * 2, 5000);
}

function sendHello(): void {
  if (!pairing) return;
  const resume = Array.from(progress.entries()).map(([tab, p]) => ({
    tab, seq: p.seq, hash: p.hash,
  }));
  const queued = outbox.splice(0, outbox.length);
  void send(Channel.Ctrl, encodeFrame(FrameType.Hello, 0, helloBody({
    token: pairing.token,
    sessionId: sessionId || undefined,
    caps: ['zstd'],
    viewport,
    resume,
    queued,
    client: 'skyhook-pwa',
  })));
}

/**
 * Publishes the HUD's numbers. Bytes come from this side, because they are the
 * bytes that actually crossed the link and because the server only speaks on
 * its keepalive — a byte counter that moves once every fifteen seconds reads as
 * broken, which is the opposite of what the HUD is for.
 */
function publishStats(): void {
  post('stats', { ...serverStats, ...transport?.stats() });
}

async function send(channel: Channel, payload: Uint8Array): Promise<void> {
  if (!transport || !online) return;
  try {
    await transport.send(channel, frameMessage(channel, payload));
  } catch (err) {
    post('status', { online: false, kind: 'none', reason: String(err) });
  }
}

function handleMessage(msg: Uint8Array): void {
  let payload: Uint8Array;
  try {
    payload = unframeMessage(msg).payload;
  } catch (err) {
    post('log', { message: `undecodable message: ${String(err)}` });
    return;
  }
  const frame = decodeFrame(payload);
  switch (frame.type) {
    case FrameType.Welcome: {
      const w = decodeWelcome(frame.body);
      sessionId = w.sessionId;
      // A session that reached Welcome is the only evidence that connecting
      // works, so this is where the backoff earns its reset.
      reconnectDelay = 500;
      post('welcome', w as unknown as Record<string, unknown>);
      // A resumed session hands back tabs that kept running while we were
      // gone; ask for whatever we do not already hold.
      for (const t of w.tabs) {
        if (!progress.has(t.tab)) {
          void send(Channel.Ctrl,
            encodeFrame(FrameType.Resync, t.tab, resyncBody(t.tab, 0, 'cold')));
        }
      }
      if (keepalive) clearInterval(keepalive);
      const ping = (): void => {
        void send(Channel.Ctrl, encodeFrame(FrameType.Ping, 0));
      };
      // The server answers a ping with stats, so without this first one the HUD
      // shows no round trip and no bytes for a full keepalive interval — while
      // someone is staring at it to find out whether the link is working.
      ping();
      keepalive = setInterval(ping, w.keepaliveMs || 15000);
      if (statsTick) clearInterval(statsTick);
      statsTick = setInterval(publishStats, 2000);
      break;
    }
    case FrameType.Snapshot:
      progress.set(frame.tab, { seq: 0, hash: 0 });
      post('snapshot', { tab: frame.tab, snapshot: decodeSnapshot(frame.body) });
      break;
    case FrameType.Mutation: {
      const have = progress.get(frame.tab);
      if (!have) {
        void send(Channel.Ctrl,
          encodeFrame(FrameType.Resync, frame.tab, resyncBody(frame.tab, 0, 'cold')));
        break;
      }
      if (frame.base && frame.base > have.seq) {
        void send(Channel.Ctrl,
          encodeFrame(FrameType.Resync, frame.tab, resyncBody(frame.tab, have.seq, 'gap')));
        break;
      }
      if (frame.seq <= have.seq) break; // duplicate from a replay
      post('mutation', {
        tab: frame.tab, seq: frame.seq, cause: frame.cause,
        mutation: decodeMutation(frame.body),
      });
      break;
    }
    case FrameType.TabState:
      post('tabState', { tab: frame.tab, state: decodeTabState(frame.body) });
      break;
    case FrameType.ImageMeta:
      post('imageMeta', { tab: frame.tab, meta: decodeImageMeta(frame.body) });
      break;
    case FrameType.ImageData: {
      const d = decodeImageData(frame.body);
      void storeImage(d.hash, d.mime, d.data).then(() => {
        post('imageData', { tab: frame.tab, hash: d.hash });
      });
      break;
    }
    case FrameType.AdapterEvent: {
      const b = decodeAdapterBatch(frame.body);
      post('adapter', { records: b.records, backlog: b.backlog });
      break;
    }
    case FrameType.Stats:
      serverStats = decodeStats(frame.body) as unknown as Record<string, unknown>;
      publishStats();
      break;
    case FrameType.Capture: {
      const req = decodeCaptureRequest(frame.body);
      // Straight through to the shell, and nothing awaited on the way: the
      // shell has to freeze its mirror frames before the resync that usually
      // follows a capture request lands on the dom channel behind this one.
      post('capture', { request: req as unknown as Record<string, unknown> });
      // The worker's own state goes up from here rather than through the shell,
      // because this is the half that knows it: what has been acknowledged, how
      // the reconnect loop is doing, and what the transport thinks of the link.
      queueCapturePart({
        id: req.id,
        name: 'worker.json',
        data: new TextEncoder().encode(JSON.stringify(workerReport(), null, 2)),
      });
      break;
    }
    case FrameType.CaptureDone:
      post('captureDone', decodeCaptureDone(frame.body) as unknown as Record<string, unknown>);
      break;
    case FrameType.Error:
      post('log', { message: `server error: ${JSON.stringify(decodeError(frame.body))}` });
      break;
    default:
      break;
  }
}

// ------------------------------------------------------------------- capture

/**
 * How much of one artifact rides in a single frame.
 *
 * The bulk channel is a message stream, so a 400 kB screenshot sent whole sits
 * in front of everything behind it until the link has cleared all of it. At
 * 250 kbps that is thirteen seconds during which an acknowledgement cannot get
 * out. Chunked, the capture yields between pieces.
 */
const CAPTURE_CHUNK = 32 * 1024;

/**
 * Capture parts are sent strictly in order.
 *
 * Everywhere else in this worker a frame is fired off with `void send(...)`,
 * which is fine for frames that stand alone. These do not: a chunk that
 * overtakes its predecessor reassembles into a corrupt artifact landside, and
 * the artifact is a screenshot, so the corruption would look like a rendering
 * bug — in a bundle taken to diagnose a rendering bug.
 */
let captureQueue: Promise<void> = Promise.resolve();

interface CapturePartInput {
  id: string;
  name?: string;
  data?: Uint8Array;
  done?: boolean;
  error?: string;
}

function queueCapturePart(part: CapturePartInput): void {
  captureQueue = captureQueue.then(() => sendCapturePart(part)).catch((err: unknown) => {
    post('log', { message: `capture part failed: ${String(err)}` });
  });
}

async function sendCapturePart(part: CapturePartInput): Promise<void> {
  const data = part.data ?? new Uint8Array(0);
  if (part.name && data.length > CAPTURE_CHUNK) {
    for (let off = 0; off < data.length; off += CAPTURE_CHUNK) {
      // slice, not subarray: a view carries its whole backing buffer into the
      // encoder, and a chunk that ships the entire artifact behind it defeats
      // the point of chunking.
      const slice = data.slice(off, Math.min(off + CAPTURE_CHUNK, data.length));
      const more = off + CAPTURE_CHUNK < data.length;
      await send(Channel.Bulk, encodeFrame(FrameType.CapturePart, 0, capturePartBody({
        id: part.id, name: part.name, data: slice, more,
      })));
    }
  } else if (part.name || part.error || part.done) {
    await send(Channel.Bulk, encodeFrame(FrameType.CapturePart, 0, capturePartBody({
      id: part.id, name: part.name, data, error: part.error,
    })));
  }
  if (part.done) {
    await send(Channel.Bulk, encodeFrame(FrameType.CapturePart, 0,
      capturePartBody({ id: part.id, done: true })));
  }
}

/** What this worker knows that neither the shell nor the server does. */
function workerReport(): Record<string, unknown> {
  return {
    sessionId,
    online,
    refused,
    reconnectDelayMs: reconnectDelay,
    queuedInputFrames: outbox.length,
    transport: transport?.kind ?? 'none',
    transportStats: transport?.stats() ?? null,
    serverStats,
    viewport,
    // Per tab: the sequence this client has acknowledged and the document hash
    // it reported for it. Put beside the server's view of the same two numbers,
    // this is where a divergence is either explained or narrowed.
    progress: Array.from(progress.entries()).map(([tab, p]) => ({
      tab, appliedSeq: p.seq, docHash: p.hash,
    })),
  };
}

/** Puts transcoded bytes where the shell and the service worker will find them. */
async function storeImage(hash: string, mime: string, data: Uint8Array): Promise<void> {
  if (!hash || !data.length) return;
  try {
    const cache = await caches.open(IMAGE_CACHE);
    // Copy into a plain ArrayBuffer: a view over the worker's transfer buffer
    // is not a valid Response body.
    const body = new Uint8Array(data.byteLength);
    body.set(data);
    await cache.put(imageCacheKey(hash), new Response(body.buffer as ArrayBuffer, {
      headers: {
        'content-type': mime || 'application/octet-stream',
        'cache-control': 'no-store',
      },
    }));
  } catch (err) {
    post('log', { message: `image cache write failed: ${String(err)}` });
  }
}

/** Reports which of a set of hashes are already cached across flights. */
async function cachedHashes(hashes: string[]): Promise<string[]> {
  const cache = await caches.open(IMAGE_CACHE);
  const found: string[] = [];
  for (const h of hashes) {
    if (await cache.match(imageCacheKey(h))) found.push(h);
  }
  return found;
}

// ------------------------------------------------------- commands from the app

self.addEventListener('message', (event: MessageEvent) => {
  const cmd = event.data as { name: string; args: Record<string, unknown> };
  switch (cmd.name) {
    case 'configure':
      pairing = cmd.args.pairing as Pairing;
      viewport = (cmd.args.viewport as Viewport) ?? viewport;
      // The session the shell remembers from a previous load of the page, which
      // is what makes a reload rejoin its tabs instead of being handed a fresh
      // and empty session. Only ever adopted when this worker has none of its
      // own: a session it has been welcomed into is the newer truth, and the
      // stored one is what that replaced.
      if (!sessionId) sessionId = String(cmd.args.sessionId ?? '');
      // A new pairing is a new credential, which is exactly what a refusal was
      // waiting for.
      refused = null;
      reconnectDelay = 500;
      redial();
      break;
    case 'openTab':
      void send(Channel.Ctrl,
        encodeFrame(FrameType.TabOpen, 0, navigateBody(String(cmd.args.url ?? ''))));
      break;
    case 'closeTab':
      progress.delete(Number(cmd.args.tab));
      void send(Channel.Ctrl, encodeFrame(FrameType.TabClose, Number(cmd.args.tab)));
      break;
    case 'navigate':
      void send(Channel.Ctrl, encodeFrame(FrameType.Navigate, Number(cmd.args.tab),
        navigateBody(String(cmd.args.url ?? ''), String(cmd.args.action ?? ''))));
      break;
    case 'input': {
      const ev = cmd.args as unknown as InputEventInit & { tab: number };
      if (!online) {
        // Queue rather than drop: typing during an outage must survive it.
        outbox.push(inputFrameMap(ev));
        break;
      }
      void send(Channel.Input, encodeFrame(FrameType.Input, ev.tab, inputBody(ev)));
      break;
    }
    case 'scroll':
      void send(Channel.Telemetry, encodeFrame(FrameType.Scroll, Number(cmd.args.tab),
        scrollBody(cmd.args as unknown as {
          tab: number; x: number; y: number; h: number; docH: number;
        })));
      break;
    case 'ack': {
      const tab = Number(cmd.args.tab);
      progress.set(tab, { seq: Number(cmd.args.seq), hash: Number(cmd.args.hash) });
      void send(Channel.Ctrl, encodeFrame(FrameType.Ack, tab,
        ackBody(tab, Number(cmd.args.seq), Number(cmd.args.hash))));
      break;
    }
    case 'wantImage': {
      const tab = Number(cmd.args.tab);
      const hashes = cmd.args.hashes as string[];
      void cachedHashes(hashes).then((have) => {
        // Anything the cross-flight cache already holds is free; tell the page
        // so it can paint, and only ask the server for the rest.
        for (const h of have) post('imageData', { tab, hash: h });
        const missing = hashes.filter((h) => !have.includes(h));
        if (missing.length) {
          void send(Channel.Ctrl,
            encodeFrame(FrameType.ImageWant, tab, imageWantBody(missing, have)));
        }
      });
      break;
    }
    case 'viewport':
      viewport = cmd.args.viewport as Viewport;
      void send(Channel.Ctrl, encodeFrame(FrameType.Viewport, 0, viewportBody(viewport)));
      break;
    case 'adapter':
      void send(Channel.Bulk, encodeFrame(FrameType.AdapterCmd, 0,
        adapterCommandBody(cmd.args as unknown as { adapter: string; cmd: string })));
      break;
    case 'capture':
      void send(Channel.Ctrl, encodeFrame(FrameType.Capture, 0, captureRequestBody({
        reason: String(cmd.args.reason ?? 'manual'),
        note: String(cmd.args.note ?? ''),
      })));
      break;
    case 'capturePart':
      queueCapturePart(cmd.args as unknown as CapturePartInput);
      break;
    case 'kill':
      void send(Channel.Ctrl, encodeFrame(FrameType.Kill, 0));
      break;
    case 'reconnect':
      // Asking for it by hand overrides a refusal: the server may have been
      // restarted, or repaired, since it last said no.
      refused = null;
      reconnectDelay = 500;
      redial();
      break;
    case 'disconnect':
      clearReconnect();
      transport?.close();
      break;
    default:
      post('log', { message: `unknown command ${cmd.name}` });
  }
});

/** Rebuilds an input frame as a map, for replay in the next Hello. */
function inputFrameMap(ev: InputEventInit & { tab: number }): Map<number, unknown> {
  const m = new Map<number, unknown>();
  m.set(1, FrameType.Input);
  if (ev.tab) m.set(2, ev.tab);
  m.set(5, inputBody(ev));
  return m;
}

post('ready', {});
