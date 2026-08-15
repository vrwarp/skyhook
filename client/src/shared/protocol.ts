/**
 * Wire protocol definitions, mirroring internal/protocol in the Go server.
 *
 * Frames are CBOR maps with integer keys. The two implementations are kept in
 * step by a cross-language conformance test: Go encodes fixtures, this code
 * decodes them, and vice versa.
 */

export const PROTOCOL_VERSION = 1;

export enum Channel {
  Ctrl = 0,
  Input = 1,
  Dom = 2,
  Media = 3,
  Bulk = 4,
  Telemetry = 5,
}

export enum FrameType {
  Hello = 1,
  Welcome = 2,
  Ping = 3,
  Pong = 4,
  Ack = 5,
  Resync = 6,
  TabOpen = 7,
  TabClose = 8,
  Navigate = 9,
  TabState = 10,
  Snapshot = 11,
  Mutation = 12,
  Input = 13,
  Scroll = 14,
  ImageMeta = 15,
  ImageData = 16,
  ImageWant = 17,
  Stats = 18,
  Error = 19,
  AdapterEvent = 20,
  AdapterCmd = 21,
  Dict = 22,
  // 23 was Speculative, a prefetched snapshot. Prefetch is gone; the number
  // stays retired rather than reused.
  Kill = 24,
  Integrity = 25,
  Viewport = 26,
  Capture = 27,
  CapturePart = 28,
  CaptureDone = 29,
}

/** Why a diagnostic capture was taken. Matches the server's constants. */
export const CaptureReason = {
  Manual: 'manual',
  Divergence: 'divergence',
  Resync: 'resync',
} as const;

/**
 * Why the server hung up. Mirrors the close codes in internal/protocol.
 *
 * The distinction that matters is whether reconnecting could go any better. A
 * dropped link is worth retrying forever; a refused credential is not, and
 * retrying it is how a client ends up alternating between "offline" and
 * "connected" every couple of seconds without ever saying what is wrong.
 */
export enum CloseCode {
  Normal = 0,
  BadHello = 1,
  Unauthorized = 2,
  VersionMismatch = 3,
  SetupFailed = 4,
  /** A newer connection took the session over. Reconnecting would take it back. */
  Replaced = 5,
}

/** Why the server will not have this client, in the terms the shell shows. */
export type Refusal = 'unauthorized' | 'version' | 'replaced';

/** WebSocket carries the server's code in the private 4000-4999 range. */
export function closeCodeFromSocket(code: number): CloseCode {
  if (code > 4000 && code < 5000) return (code - 4000) as CloseCode;
  return CloseCode.Normal;
}

/**
 * Whether a close is one that reconnecting cannot fix.
 *
 * Replaced belongs here for a different reason than the other two. Nothing is
 * wrong with this client and nothing is wrong with the link — but the session
 * now belongs to a newer connection, and taking it back would only hand the
 * same close to whoever took it, who would take it back in turn.
 */
export function isFatalClose(code: CloseCode | undefined): boolean {
  return code === CloseCode.Unauthorized
    || code === CloseCode.VersionMismatch
    || code === CloseCode.Replaced;
}

export enum NodeKind {
  Element = 1,
  Text = 3,
  Comment = 8,
  Doctype = 10,
}

export const NodeFlags = {
  Editable: 1,
  Image: 2,
  ScrollDiv: 4,
  Shadow: 8,
  Canvas: 16,
} as const;

export enum OpCode {
  Insert = 1,
  Remove = 2,
  Attr = 3,
  Text = 4,
  Move = 5,
  Splice = 6,
  Style = 7,
  Image = 8,
  Focus = 9,
  Scroll = 10,
  DocInfo = 11,
}

/** Semantic input kinds, matching the server's constants. */
export const InputKind = {
  Click: 'click',
  DblClick: 'dblclick',
  Context: 'contextmenu',
  Key: 'key',
  Text: 'text',
  Submit: 'submit',
  Focus: 'focus',
  Blur: 'blur',
  Select: 'select',
  Hover: 'hover',
  Paste: 'paste',
  SetValue: 'setvalue',
  Wheel: 'wheel',
  Drag: 'drag',
} as const;

export interface Frame {
  type: FrameType;
  tab: number;
  seq: number;
  base: number;
  body: unknown;
  cause: number;
}

export interface MirrorNode {
  id: number;
  parent: number;
  kind: NodeKind;
  ref: number;
  attrs: number[];
  flags: number;
}

export interface Snapshot {
  strings: string[];
  nodes: MirrorNode[];
  css: string[];
  url: string;
  title: string;
  viewport: Viewport;
  images: ImageMeta[];
  scrollX: number;
  scrollY: number;
}

export interface MutationOp {
  op: OpCode;
  node: number;
  parent: number;
  before: number;
  ref: number;
  ref2: number;
  nodes: MirrorNode[];
  off: number;
  del: number;
  add: string[];
  drop: number[];
  x: number;
  y: number;
  str: string;
}

export interface Mutation {
  strings: string[];
  ops: MutationOp[];
  docHash: number;
  flush: boolean;
}

export interface Viewport {
  w: number;
  h: number;
  dpr: number;
  mobile: boolean;
}

export interface ImageMeta {
  node: number;
  hash: string;
  w: number;
  h: number;
  blur: string;
  mime: string;
  bytes: number;
  priority: number;
  alt: string;
  /**
   * Where a region shot belongs inside its element: [x, y, w, h] in CSS
   * pixels, relative to the element's border box. Empty means the image
   * covers the element, which is what an ordinary <img> means.
   */
  box: number[];
}

export interface TabState {
  url: string;
  title: string;
  loading: boolean;
  canBack: boolean;
  canForward: boolean;
  faviconId: string;
  closed: boolean;
  error: string;
}

export interface Welcome {
  version: number;
  sessionId: string;
  resumed: boolean;
  tabs: TabRef[];
  caps: string[];
  server: string;
  keepaliveMs: number;
  adapters: string[];
  /**
   * The version and build of the plane-side app the *server* is serving, which
   * is not necessarily the one reading this frame: a PWA runs from its own
   * cache until something makes it upgrade. Comparing `clientBuild` against
   * this app's own build id is how the reader finds out there is a newer one,
   * and it is the only channel that can tell them — every other route to the
   * question goes through the cache being asked about.
   */
  clientVersion: string;
  clientBuild: string;
}

export interface TabRef {
  tab: number;
  url: string;
  title: string;
  seq: number;
  active: boolean;
  loading: boolean;
}

export interface Stats {
  rttMicros: number;
  sendRateBps: number;
  lossPct: number;
  queueDepth: number;
  bytesSent: number;
  bytesRecv: number;
  tabs: number;
  pendingImages: number;
}

export interface AdapterRecord {
  adapter: string;
  kind: string;
  id: string;
  space: string;
  author: string;
  text: string;
  ts: number;
  seq: number;
  unread: number;
  extra: Record<string, string>;
}

/**
 * The server asking for this client's half of a diagnostic capture: the
 * mirrored DOM, what the patcher believes about it, and a picture of what the
 * reader is actually looking at.
 */
export interface CaptureRequest {
  id: string;
  reason: string;
  note: string;
  tabs: number[];
  /** Ceiling on what this client should send up, in bytes. */
  maxBytes: number;
  screenshots: boolean;
}

/** Where the bundle landed, once the server has sealed it. */
export interface CaptureDone {
  id: string;
  path: string;
  bytes: number;
  error: string;
}

/** Field numbers, kept next to the decoders that use them. */
export const F = {
  frame: { type: 1, tab: 2, seq: 3, base: 4, body: 5, cause: 6 },
  hello: {
    version: 1, token: 2, sessionId: 3, caps: 4, viewport: 5, resume: 6, queued: 7, client: 8,
    build: 9,
  },
  welcome: {
    version: 1, sessionId: 2, resumed: 3, tabs: 4, caps: 5, server: 6, keepaliveMs: 7,
    adapters: 8, clientVersion: 9, clientBuild: 10,
  },
  tabRef: { tab: 1, url: 2, title: 3, seq: 4, active: 5, loading: 6 },
  tabAck: { tab: 1, seq: 2, hash: 3 },
  viewport: { w: 1, h: 2, dpr: 3, mobile: 4 },
  resync: { tab: 1, haveTo: 2, reason: 3 },
  navigate: { url: 1, action: 2 },
  tabState: { url: 1, title: 2, loading: 3, canBack: 4, canForward: 5, faviconId: 6, closed: 7, error: 8 },
  stats: {
    rttMicros: 1, sendRateBps: 2, lossPct: 3, queueDepth: 4,
    bytesSent: 5, bytesRecv: 6, tabs: 7, pendingImages: 8,
  },
  error: { code: 1, message: 2, fatal: 3 },
  node: { id: 1, parent: 2, kind: 3, ref: 4, attrs: 5, flags: 6 },
  snapshot: {
    strings: 1, nodes: 2, css: 3, url: 4, title: 5, viewport: 6,
    images: 7, scrollX: 8, scrollY: 9, docHash: 11, baseUrl: 12,
  },
  op: {
    op: 1, node: 2, parent: 3, before: 4, ref: 5, ref2: 6, nodes: 7,
    off: 8, del: 9, add: 10, drop: 11, image: 12, x: 13, y: 14, str: 15,
  },
  mutation: { strings: 1, ops: 2, docHash: 3, flush: 4 },
  imageMeta: {
    node: 1, hash: 2, w: 3, h: 4, blur: 5, mime: 6, bytes: 7, priority: 8, alt: 9, box: 10,
  },
  imageData: { hash: 1, mime: 2, data: 3 },
  imageWant: { hashes: 1, have: 2 },
  input: {
    kind: 1, node: 2, seq: 3, text: 4, key: 5, modifiers: 6, button: 7,
    x: 8, y: 9, fields: 10, expectSeq: 11, ts: 12, start: 13, end: 14, repeat: 16,
    hold: 17, point: 18, path: 19,
  },
  scroll: { tab: 1, x: 2, y: 3, h: 4, docH: 5, node: 6, seq: 7, visible: 8 },
  adapterRecord: {
    adapter: 1, kind: 2, id: 3, space: 4, author: 5, text: 6, ts: 7, seq: 8, unread: 9, extra: 10,
  },
  adapterBatch: { records: 1, backlog: 2 },
  adapterCommand: { adapter: 1, cmd: 2, space: 3, text: 4, localId: 5, since: 6 },
  captureRequest: { id: 1, reason: 2, note: 3, tabs: 4, maxBytes: 5, screenshots: 6 },
  capturePart: { id: 1, name: 2, data: 3, more: 4, done: 5, error: 6 },
  captureDone: { id: 1, path: 2, bytes: 3, error: 4 },
} as const;
