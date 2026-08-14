/**
 * The plane-side transport.
 *
 * WebTransport is preferred for every reason the design lists: 0-RTT resumes on
 * a link that drops every few minutes, stream independence so a stalled image
 * cannot block a DOM diff, and datagrams for telemetry. This code runs in a
 * hidden renderer because that is where Chromium's WebTransport lives; Node has
 * no implementation of it.
 *
 * The WebSocket fallback exists for networks that block UDP outright — some
 * in-flight providers do — and is wire-compatible above the framing layer.
 */
import { Channel, CloseCode, closeCodeFromSocket } from '../shared/protocol.js';

export interface TransportEvents {
  onMessage(channel: Channel, payload: Uint8Array): void;
  onOpen(kind: 'webtransport' | 'websocket'): void;
  /**
   * The link went away. `code` is the server's reason for hanging up when it
   * gave one, which is what tells a reconnect worth making from one that will
   * be refused exactly the same way.
   */
  onClose(reason: string, code?: CloseCode): void;
  onStats?(stats: { bytesSent: number; bytesRecv: number; rttMs: number }): void;
}

export interface TransportConfig {
  /** https://host:port/skyhook for WebTransport. */
  url: string;
  /** wss://host:port/skyhook for the fallback. */
  fallbackUrl?: string;
  /** Base64 SHA-256 of the server certificate, from the pairing file. */
  certHash?: string;
  /** Skip WebTransport entirely (used when a network eats UDP). */
  preferFallback?: boolean;
}

const OBJECT_STREAM_FLAG = 0x80;

/** One multiplexed connection to the server. */
export class Transport {
  private cfg: TransportConfig;
  private events: TransportEvents;
  private wt: WebTransport | null = null;
  private ws: WebSocket | null = null;
  private writers = new Map<Channel, WritableStreamDefaultWriter<Uint8Array>>();
  private closed = false;
  private bytesSent = 0;
  private bytesRecv = 0;
  kind: 'webtransport' | 'websocket' | 'none' = 'none';

  constructor(cfg: TransportConfig, events: TransportEvents) {
    this.cfg = cfg;
    this.events = events;
  }

  async connect(): Promise<void> {
    this.closed = false;
    if (!this.cfg.preferFallback && typeof WebTransport !== 'undefined') {
      try {
        await this.connectWebTransport();
        return;
      } catch (err) {
        // Falling back is normal, not exceptional: some networks drop UDP.
        this.events.onClose(`webtransport unavailable: ${String(err)}`);
      }
    }
    await this.connectWebSocket();
  }

  private async connectWebTransport(): Promise<void> {
    const options: WebTransportOptions = {
      allowPooling: false,
      congestionControl: 'low-latency',
    };
    if (this.cfg.certHash) {
      // Pinning the exact certificate is stronger than trusting the public CA
      // set, and it is what lets a personal server run without a public name.
      options.serverCertificateHashes = [{
        algorithm: 'sha-256',
        value: base64ToBuffer(this.cfg.certHash).buffer as ArrayBuffer,
      }];
    }
    const wt = new WebTransport(this.cfg.url, options);
    await wt.ready;
    this.wt = wt;
    this.kind = 'webtransport';
    this.events.onOpen('webtransport');

    void this.readIncomingStreams(wt);
    void this.readDatagrams(wt);
    void wt.closed
      .then((info: { closeCode?: number; reason?: string } | undefined) => {
        const code = (info?.closeCode ?? CloseCode.Normal) as CloseCode;
        const why = info?.reason ? `: ${info.reason}` : '';
        this.handleClose(`server closed the session${why}`, code);
      })
      .catch((err: unknown) => this.handleClose(String(err)));
  }

  private async connectWebSocket(): Promise<void> {
    const url = this.cfg.fallbackUrl ?? this.cfg.url.replace(/^https:/, 'wss:');
    await new Promise<void>((resolve, reject) => {
      const ws = new WebSocket(url);
      ws.binaryType = 'arraybuffer';
      ws.onopen = () => {
        this.ws = ws;
        this.kind = 'websocket';
        this.events.onOpen('websocket');
        resolve();
      };
      ws.onerror = () => reject(new Error(`websocket connect failed: ${url}`));
      ws.onclose = (ev) => this.handleClose(
        ev.reason ? `websocket closed: ${ev.reason}` : `websocket closed: ${ev.code}`,
        closeCodeFromSocket(ev.code),
      );
      ws.onmessage = (ev) => {
        const data = new Uint8Array(ev.data as ArrayBuffer);
        this.bytesRecv += data.length;
        this.deliver(data);
      };
    });
  }

  private async readIncomingStreams(wt: WebTransport): Promise<void> {
    const reader = wt.incomingUnidirectionalStreams.getReader();
    for (;;) {
      const { value, done } = await reader.read();
      if (done || !value) return;
      void this.readStream(value as unknown as ReadableStream<Uint8Array>);
    }
  }

  /** Reads length-prefixed records off one channel stream. */
  private async readStream(stream: ReadableStream<Uint8Array>): Promise<void> {
    const reader = stream.getReader();
    let buf: Uint8Array = new Uint8Array(0);
    let channel: Channel | null = null;
    let single = false;
    for (;;) {
      const { value, done } = await reader.read();
      if (value && value.length) {
        buf = concat(buf, value);
        this.bytesRecv += value.length;
      }
      for (;;) {
        if (channel === null) {
          if (buf.length < 1) break;
          channel = (buf[0] & ~OBJECT_STREAM_FLAG) as Channel;
          single = (buf[0] & OBJECT_STREAM_FLAG) !== 0;
          buf = buf.subarray(1);
          continue;
        }
        if (buf.length < 4) break;
        const len = (buf[0] << 24) | (buf[1] << 16) | (buf[2] << 8) | buf[3];
        if (buf.length < 4 + len) break;
        const record = buf.subarray(4, 4 + len);
        buf = buf.subarray(4 + len);
        this.deliverRecord(channel, record);
        if (single) return;
      }
      if (done) return;
    }
  }

  private async readDatagrams(wt: WebTransport): Promise<void> {
    const reader = wt.datagrams.readable.getReader();
    for (;;) {
      const { value, done } = await reader.read();
      if (done) return;
      if (value) {
        this.bytesRecv += value.length;
        this.deliver(value as Uint8Array);
      }
    }
  }

  /** Messages carry their own channel byte in the header. */
  private deliver(msg: Uint8Array): void {
    if (msg.length < 2) return;
    this.events.onMessage((msg[0] & ~OBJECT_STREAM_FLAG) as Channel, msg);
  }

  private deliverRecord(_channel: Channel, record: Uint8Array): void {
    this.deliver(record);
  }

  /** Sends a message reliably on a channel. */
  async send(channel: Channel, msg: Uint8Array): Promise<void> {
    this.bytesSent += msg.length;
    if (this.ws) {
      this.ws.send(msg);
      return;
    }
    if (!this.wt) throw new Error('transport not connected');
    const writer = await this.writerFor(channel);
    const header = new Uint8Array(4);
    header[0] = (msg.length >>> 24) & 0xff;
    header[1] = (msg.length >>> 16) & 0xff;
    header[2] = (msg.length >>> 8) & 0xff;
    header[3] = msg.length & 0xff;
    await writer.write(header);
    await writer.write(msg);
  }

  /** Sends telemetry unreliably; drops are expected and fine. */
  async sendDatagram(msg: Uint8Array): Promise<void> {
    if (this.ws) {
      this.ws.send(msg);
      return;
    }
    if (!this.wt) return;
    const writer = this.wt.datagrams.writable.getWriter();
    try {
      await writer.write(msg);
    } finally {
      writer.releaseLock();
    }
  }

  private async writerFor(channel: Channel): Promise<WritableStreamDefaultWriter<Uint8Array>> {
    const existing = this.writers.get(channel);
    if (existing) return existing;
    if (!this.wt) throw new Error('transport not connected');
    const stream = await this.wt.createUnidirectionalStream();
    const writer = stream.getWriter();
    await writer.write(new Uint8Array([channel]));
    this.writers.set(channel, writer);
    return writer;
  }

  stats(): { bytesSent: number; bytesRecv: number; rttMs: number } {
    let rttMs = 0;
    const stats = (this.wt as unknown as { getStats?: () => unknown })?.getStats;
    void stats;
    return { bytesSent: this.bytesSent, bytesRecv: this.bytesRecv, rttMs };
  }

  private handleClose(reason: string, code?: CloseCode): void {
    if (this.closed) return;
    this.closed = true;
    this.writers.clear();
    this.wt = null;
    this.ws = null;
    this.kind = 'none';
    this.events.onClose(reason, code);
  }

  close(): void {
    try {
      this.wt?.close();
      this.ws?.close();
    } catch {
      // Already gone; the close handler has run or will.
    }
    this.handleClose('closed locally');
  }
}

function concat(a: Uint8Array, b: Uint8Array): Uint8Array {
  if (a.length === 0) return b;
  const out = new Uint8Array(a.length + b.length);
  out.set(a, 0);
  out.set(b, a.length);
  return out;
}

export function base64ToBuffer(b64: string): Uint8Array<ArrayBuffer> {
  const bin = atob(b64);
  const out = new Uint8Array(new ArrayBuffer(bin.length));
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}
