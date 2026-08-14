/** Ambient declarations for the bridges the preloads expose. */
import type { AdapterRecord, ImageMeta, Mutation, Snapshot, Stats, TabState, Welcome } from './protocol.js';

declare global {
  interface Window {
    skyhookNet: {
      ready(): void;
      status(s: { online: boolean; kind: string; reason?: string }): void;
      log(message: string): void;
      welcome(w: Welcome): void;
      snapshot(tab: number, snapshot: Snapshot): void;
      speculative(tab: number, snapshot: Snapshot): void;
      mutation(tab: number, seq: number, cause: number, mutation: Mutation): void;
      tabState(tab: number, state: TabState): void;
      imageMeta(tab: number, meta: ImageMeta): void;
      imageData(tab: number, hash: string, mime: string, data: Uint8Array): void;
      adapter(records: AdapterRecord[], backlog: boolean): void;
      stats(stats: Partial<Stats> & { bytesSent?: number; bytesRecv?: number; rttMs?: number }): void;
      onCommand(fn: (cmd: { name: string; args: Record<string, unknown> }) => void): void;
    };
  }
}

export {};
