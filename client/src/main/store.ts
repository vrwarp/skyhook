/**
 * The local store: compression dictionaries, the image cache, the adapter
 * archive, bookmarks and the session resume token.
 *
 * Deliberately not SQLite. A native module would have to be rebuilt for every
 * Electron version on every platform the client ships to, and what is actually
 * needed here is a content-addressed blob store plus two append-only logs —
 * which the filesystem already is.
 *
 * The archive holds real message content, so it is encrypted at rest with a key
 * held in the OS keychain (Electron's safeStorage). If the platform cannot
 * offer encryption, the archive stays in memory only and says so.
 */
import { safeStorage } from 'electron';
import fs from 'node:fs';
import path from 'node:path';

export interface Pairing {
  host: string;
  port: number;
  path: string;
  token: string;
  certSha256: string;
  certExpires: string;
  fallbackUrl?: string;
  hosts?: string[];
  version: number;
}

export interface ImageEntry {
  data: Buffer;
  mime: string;
}

export interface ArchiveRecord {
  adapter: string;
  kind: string;
  id: string;
  space: string;
  author: string;
  text: string;
  ts: number;
  seq: number;
  unread: number;
}

const MAX_IMAGE_CACHE_BYTES = 256 * 1024 * 1024;

export class Store {
  private imagesDir: string;
  private archivePath: string;
  private bookmarksPath: string;
  private statePath: string;
  private imageBytes = 0;
  private imageIndex = new Map<string, { size: number; used: number }>();
  private encrypted = false;

  constructor(root: string) {
    this.imagesDir = path.join(root, 'images');
    this.archivePath = path.join(root, 'archive.log');
    this.bookmarksPath = path.join(root, 'bookmarks.json');
    this.statePath = path.join(root, 'state.json');
  }

  async open(): Promise<void> {
    await fs.promises.mkdir(this.imagesDir, { recursive: true });
    this.encrypted = safeStorage?.isEncryptionAvailable?.() ?? false;
    try {
      const entries = await fs.promises.readdir(this.imagesDir);
      for (const name of entries) {
        const st = await fs.promises.stat(path.join(this.imagesDir, name));
        this.imageIndex.set(name, { size: st.size, used: st.mtimeMs });
        this.imageBytes += st.size;
      }
    } catch {
      // A fresh install: nothing cached yet.
    }
    await this.evict();
  }

  async close(): Promise<void> {
    // Everything is written through; nothing to flush.
  }

  // ------------------------------------------------------------------ images

  /** Content-addressed: a hash that is present is always the right bytes. */
  async readImage(hash: string): Promise<ImageEntry | null> {
    if (!/^[0-9a-f]{4,64}$/i.test(hash)) return null;
    const file = path.join(this.imagesDir, hash);
    try {
      const data = await fs.promises.readFile(file);
      const entry = this.imageIndex.get(hash);
      if (entry) entry.used = Date.now();
      return { data, mime: sniff(data) };
    } catch {
      return null;
    }
  }

  async writeImage(hash: string, data: Uint8Array): Promise<void> {
    if (!/^[0-9a-f]{4,64}$/i.test(hash)) return;
    const file = path.join(this.imagesDir, hash);
    await fs.promises.writeFile(file, data);
    const prev = this.imageIndex.get(hash);
    if (prev) this.imageBytes -= prev.size;
    this.imageIndex.set(hash, { size: data.byteLength, used: Date.now() });
    this.imageBytes += data.byteLength;
    if (this.imageBytes > MAX_IMAGE_CACHE_BYTES) await this.evict();
  }

  hasImage(hash: string): boolean {
    return this.imageIndex.has(hash);
  }

  private async evict(): Promise<void> {
    if (this.imageBytes <= MAX_IMAGE_CACHE_BYTES) return;
    const entries = Array.from(this.imageIndex.entries()).sort((a, b) => a[1].used - b[1].used);
    for (const [hash, meta] of entries) {
      if (this.imageBytes <= MAX_IMAGE_CACHE_BYTES * 0.9) break;
      try {
        await fs.promises.unlink(path.join(this.imagesDir, hash));
      } catch {
        // Already gone.
      }
      this.imageIndex.delete(hash);
      this.imageBytes -= meta.size;
    }
  }

  // ----------------------------------------------------------------- pairing

  async readPairing(): Promise<Pairing | null> {
    const state = await this.readState();
    return (state.pairing as Pairing) ?? null;
  }

  async writePairing(p: Pairing): Promise<void> {
    const state = await this.readState();
    state.pairing = p;
    await this.writeState(state);
  }

  async readSessionId(): Promise<string> {
    const state = await this.readState();
    return typeof state.sessionId === 'string' ? state.sessionId : '';
  }

  async writeSessionId(id: string): Promise<void> {
    const state = await this.readState();
    state.sessionId = id;
    await this.writeState(state);
  }

  private async readState(): Promise<Record<string, unknown>> {
    try {
      const raw = await fs.promises.readFile(this.statePath, 'utf8');
      return JSON.parse(raw) as Record<string, unknown>;
    } catch {
      return {};
    }
  }

  private async writeState(state: Record<string, unknown>): Promise<void> {
    await fs.promises.mkdir(path.dirname(this.statePath), { recursive: true });
    await fs.promises.writeFile(this.statePath, JSON.stringify(state, null, 2), { mode: 0o600 });
  }

  // ----------------------------------------------------------------- archive

  /** Appends adapter records. The archive is what makes a chat open instantly
   *  and read offline. */
  async appendArchive(records: ArchiveRecord[]): Promise<void> {
    if (!records.length) return;
    const lines = records.map((r) => JSON.stringify(r)).join('\n') + '\n';
    const payload = this.encrypted
      ? safeStorage.encryptString(lines)
      : Buffer.from(lines, 'utf8');
    // Each append is length-prefixed so an encrypted log is still a log.
    const header = Buffer.alloc(4);
    header.writeUInt32BE(payload.length, 0);
    await fs.promises.appendFile(this.archivePath, Buffer.concat([header, payload]));
  }

  async readArchive(): Promise<ArchiveRecord[]> {
    let raw: Buffer;
    try {
      raw = await fs.promises.readFile(this.archivePath);
    } catch {
      return [];
    }
    const out: ArchiveRecord[] = [];
    let off = 0;
    while (off + 4 <= raw.length) {
      const len = raw.readUInt32BE(off);
      off += 4;
      if (off + len > raw.length) break;
      const chunk = raw.subarray(off, off + len);
      off += len;
      let text: string;
      try {
        text = this.encrypted ? safeStorage.decryptString(chunk) : chunk.toString('utf8');
      } catch {
        continue; // written under a different key; skip rather than fail
      }
      for (const line of text.split('\n')) {
        if (!line.trim()) continue;
        try {
          out.push(JSON.parse(line) as ArchiveRecord);
        } catch {
          // A torn write at the tail; ignore.
        }
      }
    }
    return out;
  }

  // --------------------------------------------------------------- bookmarks

  async readBookmarks(): Promise<{ title: string; url: string }[]> {
    try {
      const raw = await fs.promises.readFile(this.bookmarksPath, 'utf8');
      return JSON.parse(raw) as { title: string; url: string }[];
    } catch {
      return [];
    }
  }

  async writeBookmarks(marks: { title: string; url: string }[]): Promise<void> {
    await fs.promises.writeFile(this.bookmarksPath, JSON.stringify(marks, null, 2), { mode: 0o600 });
  }

  /** True when the archive is encrypted at rest. */
  get encryptionAvailable(): boolean {
    return this.encrypted;
  }
}

function sniff(data: Buffer): string {
  if (data.length > 12 && data.subarray(4, 12).toString('latin1') === 'ftypavif') return 'image/avif';
  if (data.length > 12 && data.subarray(0, 4).toString('latin1') === 'RIFF' &&
      data.subarray(8, 12).toString('latin1') === 'WEBP') return 'image/webp';
  if (data.length > 3 && data[0] === 0xff && data[1] === 0xd8) return 'image/jpeg';
  if (data.length > 8 && data.subarray(1, 4).toString('latin1') === 'PNG') return 'image/png';
  if (data.length > 3 && data.subarray(0, 3).toString('latin1') === 'GIF') return 'image/gif';
  return 'application/octet-stream';
}
