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
  ackBody, adapterCommandBody, decodeAdapterBatch, decodeError, decodeFrame, decodeImageData,
  decodeImageMeta, decodeMutation, decodeSnapshot, decodeStats, decodeTabState, decodeWelcome,
  encodeFrame, frameMessage, helloBody, imageWantBody, inputBody, navigateBody, resyncBody,
  scrollBody, unframeMessage, viewportBody, InputEventInit,
} from '../shared/codec.js';
import { IMAGE_CACHE, imageCacheKey } from '../shared/caches.js';
import { Channel, CloseCode, FrameType, isFatalClose, Viewport } from '../shared/protocol.js';
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
let refused: 'unauthorized' | 'version' | null = null;

function post(kind: string, args: Record<string, unknown>, transfer: Transferable[] = []): void {
  (self as unknown as Worker).postMessage({ kind, args }, transfer);
}

async function connect(): Promise<void> {
  if (!pairing || refused) return;
  transport = new Transport(pairing, {
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
      if (!online) return;
      online = false;
      if (statsTick) {
        clearInterval(statsTick);
        statsTick = null;
      }
      if (isFatalClose(code)) {
        refused = code === CloseCode.Unauthorized ? 'unauthorized' : 'version';
        post('status', { online: false, kind: 'none', reason, refused });
        return;
      }
      post('status', { online: false, kind: 'none', reason });
      scheduleReconnect();
    },
    onMessage: (_channel, msg) => handleMessage(msg),
  });
  try {
    await transport.connect();
  } catch (err) {
    post('status', { online: false, kind: 'none', reason: String(err) });
    scheduleReconnect();
  }
}

function scheduleReconnect(): void {
  if (reconnectTimer) return;
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
    case FrameType.Error:
      post('log', { message: `server error: ${JSON.stringify(decodeError(frame.body))}` });
      break;
    default:
      break;
  }
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
      // A new pairing is a new credential, which is exactly what a refusal was
      // waiting for.
      refused = null;
      reconnectDelay = 500;
      void connect();
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
    case 'kill':
      void send(Channel.Ctrl, encodeFrame(FrameType.Kill, 0));
      break;
    case 'reconnect':
      // Asking for it by hand overrides a refusal: the server may have been
      // restarted, or repaired, since it last said no.
      refused = null;
      reconnectDelay = 500;
      transport?.close();
      void connect();
      break;
    case 'disconnect':
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
