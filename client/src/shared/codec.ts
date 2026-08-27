/**
 * Message framing and CBOR coding for the client.
 *
 * Message layout is identical on both transports and matches the Go codec:
 *   byte 0     channel
 *   byte 1     codec (0 raw, 1 zstd, 2 zstd + dictionary)
 *   bytes 2-5  dictionary id (codec 2 only)
 *   rest       CBOR payload
 *
 * The client only ever *decompresses*: everything it sends is a handful of
 * bytes (an input event, an ack), where a zstd header would cost more than it
 * saves.
 */
import { decode as cborDecode, Encoder } from 'cbor-x';
import { decompress as zstdDecompress } from 'fzstd';

import {
  CaptureDone, CaptureRequest, Channel, ClipboardRelay, Download, DownloadPart, F, FileAsk,
  Frame, FrameType, ImageMeta, MirrorNode, Mutation, MutationOp, NodeKind, OpCode, Snapshot,
  Stats, TabState, Viewport, Welcome, AdapterRecord, TabRef,
} from './protocol.js';

export const CODEC_RAW = 0;
export const CODEC_ZSTD = 1;
export const CODEC_ZSTD_DICT = 2;

// cbor-x is configured to hand back plain objects with numeric keys, which is
// what the field-number decoders below expect.
// The server decodes plain CBOR maps with integer keys. cbor-x would otherwise
// wrap every JS Map in tag 259, which fxamacker/cbor rejects, so that behaviour
// is turned off explicitly (the option is not in cbor-x's published types).
const encoder = new Encoder({
  mapsAsObjects: true,
  useRecords: false,
  tagUint8Array: false,
  useTag259ForMaps: false,
} as ConstructorParameters<typeof Encoder>[0]);

/** Raw CBOR map with integer keys, as decoded from the wire. */
type Fields = Record<number, unknown>;

/**
 * Clamps a number into the range cbor-x encodes as a CBOR *integer*.
 *
 * Above 2^32-1 it switches to float64, and the server's decoder refuses to put
 * a float into an int64 field — which silently drops the whole frame. Every
 * integer field on the wire goes through here, and callers keep their values
 * small on purpose (timestamps are monotonic milliseconds, not wall clock).
 */
export function safeInt(v: number): number {
  if (!Number.isFinite(v)) return 0;
  const n = Math.trunc(v);
  if (n > 0xffffffff) return 0xffffffff;
  if (n < -0x80000000) return -0x80000000;
  return n;
}

function num(f: Fields | undefined, key: number, dflt = 0): number {
  const v = f?.[key];
  if (typeof v === 'number') return v;
  if (typeof v === 'bigint') return Number(v);
  return dflt;
}

function str(f: Fields | undefined, key: number, dflt = ''): string {
  const v = f?.[key];
  return typeof v === 'string' ? v : dflt;
}

function bool(f: Fields | undefined, key: number): boolean {
  return f?.[key] === true;
}

function arr<T>(f: Fields | undefined, key: number): T[] {
  const v = f?.[key];
  return Array.isArray(v) ? (v as T[]) : [];
}

function fields(f: Fields | undefined, key: number): Fields | undefined {
  const v = f?.[key];
  return v && typeof v === 'object' ? (v as Fields) : undefined;
}

/** Wraps a CBOR payload in the channel header. */
export function frameMessage(channel: Channel, payload: Uint8Array): Uint8Array {
  const out = new Uint8Array(payload.length + 2);
  out[0] = channel;
  out[1] = CODEC_RAW;
  out.set(payload, 2);
  return out;
}

/** Splits a wire message into its channel and decompressed CBOR payload. */
export function unframeMessage(msg: Uint8Array): { channel: Channel; payload: Uint8Array } {
  if (msg.length < 2) throw new Error('short message');
  const channel = (msg[0] & 0x7f) as Channel;
  switch (msg[1]) {
    case CODEC_RAW:
      return { channel, payload: msg.subarray(2) };
    case CODEC_ZSTD:
      return { channel, payload: zstdDecompress(msg.subarray(2)) };
    case CODEC_ZSTD_DICT:
      // Dictionary support is negotiated in Hello; the client never advertises
      // it, so reaching here means the server ignored our capability list.
      throw new Error('server used a zstd dictionary the client did not advertise');
    default:
      throw new Error(`unknown codec ${msg[1]}`);
  }
}

/** Encodes a frame with a body already expressed as a Map of field numbers. */
export function encodeFrame(
  type: FrameType,
  tab: number,
  body?: Map<number, unknown>,
  extra?: { seq?: number; base?: number; cause?: number },
): Uint8Array {
  const m = new Map<number, unknown>();
  m.set(F.frame.type, type);
  if (tab) m.set(F.frame.tab, safeInt(tab));
  if (extra?.seq) m.set(F.frame.seq, safeInt(extra.seq));
  if (extra?.base) m.set(F.frame.base, safeInt(extra.base));
  if (extra?.cause) m.set(F.frame.cause, safeInt(extra.cause));
  // The server decodes the body as cbor.RawMessage: the body item is spliced
  // into the envelope directly, not wrapped in a byte string.
  if (body && body.size > 0) m.set(F.frame.body, body);
  return encoder.encode(m) as Uint8Array;
}

/** Decodes a frame envelope; the body stays as raw CBOR bytes. */
export function decodeFrame(payload: Uint8Array): Frame {
  const f = cborDecode(payload) as Fields;
  return {
    type: num(f, F.frame.type) as FrameType,
    tab: num(f, F.frame.tab),
    seq: num(f, F.frame.seq),
    base: num(f, F.frame.base),
    cause: num(f, F.frame.cause),
    body: f[F.frame.body],
  };
}

function bodyFields(body: unknown): Fields | undefined {
  if (!body) return undefined;
  if (body instanceof Uint8Array) return cborDecode(body) as Fields;
  if (typeof body === 'object') return body as Fields;
  return undefined;
}

export function decodeWelcome(body: unknown): Welcome {
  const f = bodyFields(body);
  return {
    version: num(f, F.welcome.version),
    sessionId: str(f, F.welcome.sessionId),
    resumed: bool(f, F.welcome.resumed),
    caps: arr<string>(f, F.welcome.caps),
    server: str(f, F.welcome.server),
    keepaliveMs: num(f, F.welcome.keepaliveMs, 15000),
    adapters: arr<string>(f, F.welcome.adapters),
    clientVersion: str(f, F.welcome.clientVersion),
    clientBuild: str(f, F.welcome.clientBuild),
    tabs: arr<Fields>(f, F.welcome.tabs).map((t): TabRef => ({
      tab: num(t, F.tabRef.tab),
      url: str(t, F.tabRef.url),
      title: str(t, F.tabRef.title),
      seq: num(t, F.tabRef.seq),
      active: bool(t, F.tabRef.active),
      loading: bool(t, F.tabRef.loading),
    })),
  };
}

function decodeNode(n: Fields): MirrorNode {
  return {
    id: num(n, F.node.id),
    parent: num(n, F.node.parent),
    kind: num(n, F.node.kind) as NodeKind,
    ref: num(n, F.node.ref),
    attrs: arr<number>(n, F.node.attrs),
    flags: num(n, F.node.flags),
  };
}

function decodeImage(i: Fields): ImageMeta {
  return {
    node: num(i, F.imageMeta.node),
    hash: str(i, F.imageMeta.hash),
    w: num(i, F.imageMeta.w),
    h: num(i, F.imageMeta.h),
    blur: str(i, F.imageMeta.blur),
    mime: str(i, F.imageMeta.mime),
    bytes: num(i, F.imageMeta.bytes),
    priority: num(i, F.imageMeta.priority),
    alt: str(i, F.imageMeta.alt),
    box: arr<number>(i, F.imageMeta.box).map(Number),
    missing: bool(i, F.imageMeta.missing),
    anim: bool(i, F.imageMeta.anim),
  };
}

export function decodeSnapshot(body: unknown): Snapshot {
  const f = bodyFields(body);
  const vp = fields(f, F.snapshot.viewport);
  return {
    strings: arr<string>(f, F.snapshot.strings),
    nodes: arr<Fields>(f, F.snapshot.nodes).map(decodeNode),
    css: arr<string>(f, F.snapshot.css),
    scoped: arr<Fields>(f, F.snapshot.scoped).map((sc) => ({
      root: num(sc, F.scopedCSS.root),
      rules: arr<string>(sc, F.scopedCSS.rules),
    })),
    url: str(f, F.snapshot.url),
    title: str(f, F.snapshot.title),
    images: arr<Fields>(f, F.snapshot.images).map(decodeImage),
    scrollX: num(f, F.snapshot.scrollX),
    scrollY: num(f, F.snapshot.scrollY),
    epoch: num(f, F.snapshot.epoch),
    quirks: bool(f, F.snapshot.quirks),
    scrolls: arr<Fields>(f, F.snapshot.scrolls).map((sc) => ({
      node: num(sc, F.nodeScroll.node),
      x: num(sc, F.nodeScroll.x),
      y: num(sc, F.nodeScroll.y),
    })),
    viewport: {
      w: num(vp, F.viewport.w),
      h: num(vp, F.viewport.h),
      dpr: num(vp, F.viewport.dpr, 1),
      mobile: bool(vp, F.viewport.mobile),
    },
  };
}

export function decodeMutation(body: unknown): Mutation {
  const f = bodyFields(body);
  return {
    strings: arr<string>(f, F.mutation.strings),
    docHash: num(f, F.mutation.docHash),
    flush: bool(f, F.mutation.flush),
    ops: arr<Fields>(f, F.mutation.ops).map((o): MutationOp => ({
      op: num(o, F.op.op) as OpCode,
      node: num(o, F.op.node),
      parent: num(o, F.op.parent),
      before: num(o, F.op.before),
      ref: num(o, F.op.ref),
      ref2: num(o, F.op.ref2),
      nodes: arr<Fields>(o, F.op.nodes).map(decodeNode),
      off: num(o, F.op.off),
      del: num(o, F.op.del),
      add: arr<string>(o, F.op.add),
      drop: arr<number>(o, F.op.drop),
      x: num(o, F.op.x),
      y: num(o, F.op.y),
      str: str(o, F.op.str),
    })),
  };
}

export function decodeTabState(body: unknown): TabState {
  const f = bodyFields(body);
  return {
    url: str(f, F.tabState.url),
    title: str(f, F.tabState.title),
    loading: bool(f, F.tabState.loading),
    canBack: bool(f, F.tabState.canBack),
    canForward: bool(f, F.tabState.canForward),
    faviconId: str(f, F.tabState.faviconId),
    closed: bool(f, F.tabState.closed),
    error: str(f, F.tabState.error),
    ref: str(f, F.tabState.ref),
  };
}

export function decodeStats(body: unknown): Stats {
  const f = bodyFields(body);
  return {
    rttMicros: num(f, F.stats.rttMicros),
    sendRateBps: num(f, F.stats.sendRateBps),
    lossPct: num(f, F.stats.lossPct),
    queueDepth: num(f, F.stats.queueDepth),
    bytesSent: num(f, F.stats.bytesSent),
    bytesRecv: num(f, F.stats.bytesRecv),
    tabs: num(f, F.stats.tabs),
    pendingImages: num(f, F.stats.pendingImages),
  };
}

export function decodeImageMeta(body: unknown): ImageMeta {
  return decodeImage(bodyFields(body) ?? {});
}

export function decodeImageData(body: unknown): { hash: string; mime: string; data: Uint8Array } {
  const f = bodyFields(body);
  const raw = f?.[F.imageData.data];
  return {
    hash: str(f, F.imageData.hash),
    mime: str(f, F.imageData.mime),
    data: raw instanceof Uint8Array ? raw : new Uint8Array(0),
  };
}

export function decodeAdapterBatch(body: unknown): { records: AdapterRecord[]; backlog: boolean } {
  const f = bodyFields(body);
  return {
    backlog: bool(f, F.adapterBatch.backlog),
    records: arr<Fields>(f, F.adapterBatch.records).map((r): AdapterRecord => ({
      adapter: str(r, F.adapterRecord.adapter),
      kind: str(r, F.adapterRecord.kind),
      id: str(r, F.adapterRecord.id),
      space: str(r, F.adapterRecord.space),
      author: str(r, F.adapterRecord.author),
      text: str(r, F.adapterRecord.text),
      ts: num(r, F.adapterRecord.ts),
      seq: num(r, F.adapterRecord.seq),
      unread: num(r, F.adapterRecord.unread),
      extra: (fields(r, F.adapterRecord.extra) ?? {}) as unknown as Record<string, string>,
    })),
  };
}

export function decodeError(body: unknown): { code: string; message: string; fatal: boolean } {
  const f = bodyFields(body);
  return {
    code: str(f, F.error.code),
    message: str(f, F.error.message),
    fatal: bool(f, F.error.fatal),
  };
}

export function decodeCaptureRequest(body: unknown): CaptureRequest {
  const f = bodyFields(body);
  return {
    id: str(f, F.captureRequest.id),
    reason: str(f, F.captureRequest.reason),
    note: str(f, F.captureRequest.note),
    tabs: arr<number>(f, F.captureRequest.tabs).map((t) => Number(t)),
    maxBytes: num(f, F.captureRequest.maxBytes),
    screenshots: bool(f, F.captureRequest.screenshots),
  };
}

export function decodeCaptureDone(body: unknown): CaptureDone {
  const f = bodyFields(body);
  return {
    id: str(f, F.captureDone.id),
    path: str(f, F.captureDone.path),
    bytes: num(f, F.captureDone.bytes),
    error: str(f, F.captureDone.error),
  };
}

export function decodeDownload(body: unknown): Download {
  const f = bodyFields(body);
  return {
    id: str(f, F.download.id),
    url: str(f, F.download.url),
    name: str(f, F.download.name),
    total: num(f, F.download.total),
    received: num(f, F.download.received),
    state: (str(f, F.download.state) || 'landing') as Download['state'],
  };
}

export function decodeClipboard(body: unknown): ClipboardRelay {
  const f = bodyFields(body);
  return {
    text: str(f, F.clipboard.text),
    cause: num(f, F.clipboard.cause),
  };
}

export function decodeFileAsk(body: unknown): FileAsk {
  const f = bodyFields(body);
  return {
    id: num(f, F.fileAsk.id),
    node: num(f, F.fileAsk.node),
    multiple: bool(f, F.fileAsk.multiple),
  };
}

export function decodeDownloadPart(body: unknown): DownloadPart {
  const f = bodyFields(body);
  const data = f?.[F.downloadPart.data];
  return {
    id: str(f, F.downloadPart.id),
    off: num(f, F.downloadPart.off),
    data: data instanceof Uint8Array ? data : undefined,
    done: bool(f, F.downloadPart.done),
    size: num(f, F.downloadPart.size),
    error: str(f, F.downloadPart.error),
  };
}

// --------------------------------------------------------------- body builders

export function helloBody(opts: {
  token: string;
  sessionId?: string;
  caps: string[];
  viewport: Viewport;
  resume?: { tab: number; seq: number; hash: number; epoch?: number }[];
  queued?: Map<number, unknown>[];
  client: string;
  build: string;
}): Map<number, unknown> {
  const m = new Map<number, unknown>();
  m.set(F.hello.version, 1);
  m.set(F.hello.token, opts.token);
  if (opts.sessionId) m.set(F.hello.sessionId, opts.sessionId);
  m.set(F.hello.caps, opts.caps);
  m.set(F.hello.viewport, viewportBody(opts.viewport));
  if (opts.resume?.length) {
    m.set(F.hello.resume, opts.resume.map((r) => {
      const t = new Map<number, unknown>();
      t.set(F.tabAck.tab, safeInt(r.tab));
      t.set(F.tabAck.seq, safeInt(r.seq));
      if (r.hash) t.set(F.tabAck.hash, safeInt(r.hash));
      // Which document those numbers are about: the server answers a
      // current tab with silence instead of a snapshot, but only when the
      // claim names the page (see Session.TabCurrent).
      if (r.epoch) t.set(F.tabAck.epoch, safeInt(r.epoch));
      return t;
    }));
  }
  // Queued input frames are replayed by the server before any resync, which is
  // what makes "typed while offline" survive an outage. They are carried as
  // whole frame maps so the server can dispatch them unchanged.
  if (opts.queued?.length) m.set(F.hello.queued, opts.queued);
  m.set(F.hello.client, opts.client);
  // Which bytes of the app are speaking. The server logs it, a diagnostic
  // bundle records it, and it is what lets the server say — in the Welcome
  // coming back — whether this is the build it would serve today.
  if (opts.build) m.set(F.hello.build, opts.build);
  return m;
}

export function viewportBody(v: Viewport): Map<number, unknown> {
  const m = new Map<number, unknown>();
  m.set(F.viewport.w, safeInt(v.w));
  m.set(F.viewport.h, safeInt(v.h));
  m.set(F.viewport.dpr, v.dpr);
  if (v.mobile) m.set(F.viewport.mobile, true);
  if (v.scheme) m.set(F.viewport.scheme, v.scheme);
  if (v.touch) m.set(F.viewport.touch, true);
  return m;
}

export function ackBody(
  tab: number, seq: number, hash: number, epoch: number,
): Map<number, unknown> {
  const m = new Map<number, unknown>();
  m.set(F.tabAck.tab, safeInt(tab));
  m.set(F.tabAck.seq, safeInt(seq));
  if (hash) m.set(F.tabAck.hash, safeInt(hash >>> 0));
  // Which document the hash is about. Without it the server cannot tell an
  // answer about frame zero of this document from one about frame zero of the
  // document before it, and a page that is merely building itself is reported
  // as a diverged mirror.
  if (epoch) m.set(F.tabAck.epoch, safeInt(epoch));
  return m;
}

export function resyncBody(tab: number, haveTo: number, reason: string): Map<number, unknown> {
  const m = new Map<number, unknown>();
  m.set(F.resync.tab, safeInt(tab));
  m.set(F.resync.haveTo, safeInt(haveTo));
  m.set(F.resync.reason, reason);
  return m;
}

export function navigateBody(
  url: string,
  action = '',
  open: { ref?: string; background?: boolean } = {},
): Map<number, unknown> {
  const m = new Map<number, unknown>();
  if (url) m.set(F.navigate.url, url);
  if (action) m.set(F.navigate.action, action);
  if (open.ref) m.set(F.navigate.ref, open.ref);
  if (open.background) m.set(F.navigate.background, true);
  return m;
}

export interface InputEventInit {
  kind: string;
  node?: number;
  seq: number;
  text?: string;
  key?: string;
  modifiers?: number;
  button?: number;
  x?: number;
  y?: number;
  fields?: Record<string, string>;
  expectSeq?: number;
  ts?: number;
  start?: number;
  end?: number;
  repeat?: number;
  /** How long the button was held, in milliseconds. */
  hold?: number;
  /** Where in the target's box the pointer was, in permille: [x, y]. */
  point?: number[];
  /** The approach: (x, y, dt) triplets, viewport permille and milliseconds. */
  path?: number[];
  /** The pointer's kind: 0 mouse, 1 touch, 2 pen — so the landside replay
   *  can speak the modality the reader used. */
  pt?: number;
  /** The element a drag finished on, and where in its box, permille. The
   *  path says how the gesture moved; these say what it landed on. */
  node2?: number;
  point2?: number[];
  /** A second finger's path, sampled at the same instants as `path`: what
   *  makes a drag a pinch. */
  path2?: number[];
}

export function inputBody(ev: InputEventInit): Map<number, unknown> {
  const m = new Map<number, unknown>();
  m.set(F.input.kind, ev.kind);
  if (ev.node) m.set(F.input.node, safeInt(ev.node));
  m.set(F.input.seq, safeInt(ev.seq));
  if (ev.text) m.set(F.input.text, ev.text);
  if (ev.key) m.set(F.input.key, ev.key);
  if (ev.modifiers) m.set(F.input.modifiers, safeInt(ev.modifiers));
  if (ev.button) m.set(F.input.button, safeInt(ev.button));
  if (ev.x) m.set(F.input.x, safeInt(ev.x));
  if (ev.y) m.set(F.input.y, safeInt(ev.y));
  if (ev.fields) m.set(F.input.fields, ev.fields);
  if (ev.expectSeq) m.set(F.input.expectSeq, safeInt(ev.expectSeq));
  if (ev.ts) m.set(F.input.ts, safeInt(ev.ts));
  if (ev.start) m.set(F.input.start, safeInt(ev.start));
  if (ev.end) m.set(F.input.end, safeInt(ev.end));
  if (ev.repeat) m.set(F.input.repeat, safeInt(ev.repeat));
  // Real pointer measurements, so the server replays what happened rather than
  // synthesising a plausible imitation of it.
  if (ev.hold) m.set(F.input.hold, safeInt(ev.hold));
  if (ev.point?.length === 2) m.set(F.input.point, ev.point.map(safeInt));
  if (ev.path?.length) m.set(F.input.path, ev.path.map(safeInt));
  if (ev.pt) m.set(F.input.pt, safeInt(ev.pt));
  if (ev.node2) m.set(F.input.node2, safeInt(ev.node2));
  if (ev.point2?.length === 2) m.set(F.input.point2, ev.point2.map(safeInt));
  if (ev.path2?.length) m.set(F.input.path2, ev.path2.map(safeInt));
  return m;
}

export function scrollBody(o: {
  tab: number; x: number; y: number; h: number; docH: number; node?: number; seq?: number;
  anchor?: number; anchorY?: number;
}): Map<number, unknown> {
  const m = new Map<number, unknown>();
  m.set(F.scroll.tab, safeInt(o.tab));
  if (o.x) m.set(F.scroll.x, safeInt(o.x));
  if (o.y) m.set(F.scroll.y, safeInt(o.y));
  if (o.h) m.set(F.scroll.h, safeInt(o.h));
  if (o.docH) m.set(F.scroll.docH, safeInt(o.docH));
  if (o.node) m.set(F.scroll.node, safeInt(o.node));
  if (o.seq) m.set(F.scroll.seq, safeInt(o.seq));
  if (o.anchor) m.set(F.scroll.anchor, safeInt(o.anchor));
  // Zero is a real offset here — the anchor's edge exactly at the viewport
  // top — so the anchor's presence is the gate, not the value's truthiness.
  if (o.anchor && o.anchorY !== undefined) m.set(F.scroll.anchorY, safeInt(o.anchorY));
  return m;
}

export function imageWantBody(hashes: string[], have: string[] = []): Map<number, unknown> {
  const m = new Map<number, unknown>();
  m.set(F.imageWant.hashes, hashes);
  if (have.length) m.set(F.imageWant.have, have);
  return m;
}

export function adapterCommandBody(o: {
  adapter: string; cmd: string; space?: string; text?: string; localId?: string; since?: number;
}): Map<number, unknown> {
  const m = new Map<number, unknown>();
  m.set(F.adapterCommand.adapter, o.adapter);
  m.set(F.adapterCommand.cmd, o.cmd);
  if (o.space) m.set(F.adapterCommand.space, o.space);
  if (o.text) m.set(F.adapterCommand.text, o.text);
  if (o.localId) m.set(F.adapterCommand.localId, o.localId);
  if (o.since) m.set(F.adapterCommand.since, safeInt(o.since));
  return m;
}

/** Asks the server for a diagnostic capture. The ID comes back in its reply. */
export function captureRequestBody(o: { reason: string; note?: string }): Map<number, unknown> {
  const m = new Map<number, unknown>();
  m.set(F.captureRequest.reason, o.reason);
  if (o.note) m.set(F.captureRequest.note, o.note);
  return m;
}

/**
 * One plane-side artifact on its way up, or a chunk of one.
 *
 * `data` is a byte string on the wire. cbor-x is configured with
 * `tagUint8Array: false`, so a Uint8Array encodes as a plain CBOR byte string,
 * which is what the Go decoder expects for a `[]byte` field.
 */
export function uploadPartBody(o: {
  ask: number; name?: string; mime?: string; size?: number; off?: number;
  data?: Uint8Array; last?: boolean; done?: boolean; error?: string;
}): Map<number, unknown> {
  const m = new Map<number, unknown>();
  m.set(F.uploadPart.ask, safeInt(o.ask));
  if (o.name) m.set(F.uploadPart.name, o.name);
  if (o.mime) m.set(F.uploadPart.mime, o.mime);
  if (o.size) m.set(F.uploadPart.size, safeInt(o.size));
  if (o.off) m.set(F.uploadPart.off, safeInt(o.off));
  if (o.data?.length) m.set(F.uploadPart.data, o.data);
  if (o.last) m.set(F.uploadPart.last, true);
  if (o.done) m.set(F.uploadPart.done, true);
  if (o.error) m.set(F.uploadPart.error, o.error);
  return m;
}

export function downloadCmdBody(o: {
  id: string; cmd: 'fetch' | 'stop' | 'discard'; offset?: number;
}): Map<number, unknown> {
  const m = new Map<number, unknown>();
  m.set(F.downloadCmd.id, o.id);
  m.set(F.downloadCmd.cmd, o.cmd);
  if (o.offset) m.set(F.downloadCmd.offset, safeInt(o.offset));
  return m;
}

export function capturePartBody(o: {
  id: string; name?: string; data?: Uint8Array; more?: boolean; done?: boolean; error?: string;
}): Map<number, unknown> {
  const m = new Map<number, unknown>();
  m.set(F.capturePart.id, o.id);
  if (o.name) m.set(F.capturePart.name, o.name);
  if (o.data?.length) m.set(F.capturePart.data, o.data);
  if (o.more) m.set(F.capturePart.more, true);
  if (o.done) m.set(F.capturePart.done, true);
  if (o.error) m.set(F.capturePart.error, o.error);
  return m;
}
