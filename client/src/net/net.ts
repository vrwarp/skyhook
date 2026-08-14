/**
 * The network worker: a hidden renderer that owns the single connection to the
 * server and relays decoded frames to the main process.
 *
 * It lives in a renderer because WebTransport is a browser API; Electron's main
 * process has no implementation. Everything it does is protocol work — no UI,
 * no DOM.
 */
import {
  ackBody, adapterCommandBody, decodeAdapterBatch, decodeError, decodeFrame, decodeImageData,
  decodeImageMeta, decodeMutation, decodeSnapshot, decodeStats, decodeTabState, decodeWelcome,
  encodeFrame, frameMessage, helloBody, imageWantBody, inputBody, navigateBody, resyncBody,
  scrollBody, unframeMessage, viewportBody, InputEventInit,
} from '../shared/codec.js';
import { Channel, FrameType, Viewport } from '../shared/protocol.js';
import { Transport, TransportConfig } from './transport.js';

interface Pairing extends TransportConfig {
  token: string;
}

interface TabProgress {
  seq: number;
  hash: number;
}

const api = window.skyhookNet;

let transport: Transport | null = null;
let pairing: Pairing | null = null;
let sessionId = '';
let viewport: Viewport = { w: 1280, h: 800, dpr: 1, mobile: false };
const progress = new Map<number, TabProgress>();
/** Input frames sent while offline, replayed in the next Hello. */
const outbox: Map<number, unknown>[] = [];
let online = false;
let keepalive: ReturnType<typeof setInterval> | null = null;
let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
let reconnectDelay = 500;

async function connect(): Promise<void> {
  if (!pairing) return;
  transport = new Transport(pairing, {
    onOpen: (kind) => {
      online = true;
      reconnectDelay = 500;
      api.status({ online: true, kind });
      sendHello();
    },
    onClose: (reason) => {
      if (!online) return;
      online = false;
      api.status({ online: false, kind: 'none', reason });
      scheduleReconnect();
    },
    onMessage: (channel, msg) => handleMessage(channel, msg),
  });
  try {
    await transport.connect();
  } catch (err) {
    api.status({ online: false, kind: 'none', reason: String(err) });
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
  if (!pairing || !transport) return;
  const resume = Array.from(progress.entries()).map(([tab, p]) => ({
    tab, seq: p.seq, hash: p.hash,
  }));
  const queued = outbox.splice(0, outbox.length);
  const body = helloBody({
    token: pairing.token,
    sessionId: sessionId || undefined,
    caps: ['zstd'],
    viewport,
    resume,
    queued,
    client: 'skyhook-electron',
  });
  void send(Channel.Ctrl, encodeFrame(FrameType.Hello, 0, body));
}

async function send(channel: Channel, payload: Uint8Array): Promise<void> {
  const msg = frameMessage(channel, payload);
  if (!transport || !online) {
    return;
  }
  try {
    await transport.send(channel, msg);
  } catch (err) {
    api.status({ online: false, kind: 'none', reason: String(err) });
  }
}

function handleMessage(_channel: Channel, msg: Uint8Array): void {
  let payload: Uint8Array;
  try {
    payload = unframeMessage(msg).payload;
  } catch (err) {
    api.log(`undecodable message: ${String(err)}`);
    return;
  }
  const frame = decodeFrame(payload);
  switch (frame.type) {
    case FrameType.Welcome: {
      const w = decodeWelcome(frame.body);
      sessionId = w.sessionId;
      api.welcome(w);
      // Tabs that survived the outage need whatever we do not already hold.
      for (const t of w.tabs) {
        if (!progress.has(t.tab)) {
          void send(Channel.Ctrl, encodeFrame(FrameType.Resync, t.tab, resyncBody(t.tab, 0, 'cold')));
        }
      }
      if (keepalive) clearInterval(keepalive);
      keepalive = setInterval(() => {
        void send(Channel.Ctrl, encodeFrame(FrameType.Ping, 0));
      }, w.keepaliveMs || 15000);
      break;
    }
    case FrameType.Snapshot:
      api.snapshot(frame.tab, decodeSnapshot(frame.body));
      progress.set(frame.tab, { seq: 0, hash: 0 });
      break;
    case FrameType.Speculative:
      api.speculative(frame.tab, decodeSnapshot(frame.body));
      break;
    case FrameType.Mutation: {
      const have = progress.get(frame.tab);
      if (!have) {
        void send(Channel.Ctrl, encodeFrame(FrameType.Resync, frame.tab, resyncBody(frame.tab, 0, 'cold')));
        break;
      }
      if (frame.base && frame.base > have.seq) {
        void send(Channel.Ctrl,
          encodeFrame(FrameType.Resync, frame.tab, resyncBody(frame.tab, have.seq, 'gap')));
        break;
      }
      if (frame.seq <= have.seq) break; // duplicate from a replay
      api.mutation(frame.tab, frame.seq, frame.cause, decodeMutation(frame.body));
      break;
    }
    case FrameType.TabState:
      api.tabState(frame.tab, decodeTabState(frame.body));
      break;
    case FrameType.ImageMeta:
      api.imageMeta(frame.tab, decodeImageMeta(frame.body));
      break;
    case FrameType.ImageData: {
      const d = decodeImageData(frame.body);
      api.imageData(frame.tab, d.hash, d.mime, d.data);
      break;
    }
    case FrameType.AdapterEvent: {
      const b = decodeAdapterBatch(frame.body);
      api.adapter(b.records, b.backlog);
      break;
    }
    case FrameType.Stats:
      api.stats({ ...decodeStats(frame.body), ...transport?.stats() });
      break;
    case FrameType.Error:
      api.log(`server error: ${JSON.stringify(decodeError(frame.body))}`);
      break;
    case FrameType.Pong:
      break;
    default:
      break;
  }
}

// ------------------------------------------------------- main-process commands

api.onCommand((cmd: { name: string; args: Record<string, unknown> }) => {
  switch (cmd.name) {
    case 'configure':
      pairing = cmd.args.pairing as Pairing;
      viewport = (cmd.args.viewport as Viewport) ?? viewport;
      void connect();
      break;
    case 'openTab':
      void send(Channel.Ctrl,
        encodeFrame(FrameType.TabOpen, 0, navigateBody(String(cmd.args.url ?? ''))));
      break;
    case 'closeTab':
      void send(Channel.Ctrl, encodeFrame(FrameType.TabClose, Number(cmd.args.tab)));
      break;
    case 'navigate':
      void send(Channel.Ctrl, encodeFrame(FrameType.Navigate, Number(cmd.args.tab),
        navigateBody(String(cmd.args.url ?? ''), String(cmd.args.action ?? ''))));
      break;
    case 'input': {
      const ev = cmd.args as unknown as InputEventInit & { tab: number };
      const frame = encodeFrame(FrameType.Input, ev.tab, inputBody(ev));
      if (!online) {
        // Queue rather than drop: typing during an outage must survive it.
        outbox.push(inputFrameMap(ev));
        api.log('queued input while offline');
        break;
      }
      void send(Channel.Input, frame);
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
      const seq = Number(cmd.args.seq);
      const hash = Number(cmd.args.hash);
      progress.set(tab, { seq, hash });
      void send(Channel.Ctrl, encodeFrame(FrameType.Ack, tab, ackBody(tab, seq, hash)));
      break;
    }
    case 'wantImage':
      void send(Channel.Ctrl, encodeFrame(FrameType.ImageWant, Number(cmd.args.tab),
        imageWantBody(cmd.args.hashes as string[])));
      break;
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
    case 'disconnect':
      transport?.close();
      break;
    default:
      api.log(`unknown command ${cmd.name}`);
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

api.ready();
