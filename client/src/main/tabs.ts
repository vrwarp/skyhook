/**
 * Tab management and message routing.
 *
 * Each mirror tab is a BrowserWindow child view loading skyhook://mirror, with
 * the shim as its preload. The main process is the switchboard: frames from the
 * network worker go to the right tab, input from a tab goes to the worker, and
 * the chrome UI sees a summarised view of both.
 */
import type { BrowserWindow, IpcMain, IpcMainEvent } from 'electron';
import { WebContentsView } from 'electron';

import type { Pairing, Store } from './store.js';

interface Deps {
  store: Store;
  getChromeWindow(): BrowserWindow | null;
  getNetWindow(): BrowserWindow | null;
  preload: string;
}

interface Tab {
  id: number;
  view: WebContentsView;
  url: string;
  title: string;
  loading: boolean;
  ready: boolean;
  /** Frames that arrived before the shim finished loading. */
  backlog: unknown[];
}

/** Height of the chrome UI strip above the mirror area. */
const CHROME_HEIGHT = 88;

export class TabManager {
  private deps: Deps;
  private tabs = new Map<number, Tab>();
  private active = 0;
  private pairing: Pairing | null = null;
  /** Speculative snapshots keyed by URL, dropped LRU-style. */
  private speculations = new Map<string, unknown>();

  constructor(deps: Deps) {
    this.deps = deps;
  }

  configure(pairing: Pairing): void {
    this.pairing = pairing;
    const url = `https://${pairing.host}:${pairing.port}${pairing.path}`;
    this.toNet('configure', {
      pairing: {
        url,
        fallbackUrl: pairing.fallbackUrl,
        certHash: pairing.certSha256,
        token: pairing.token,
      },
      viewport: this.viewport(),
    });
  }

  private viewport(): { w: number; h: number; dpr: number; mobile: boolean } {
    const win = this.deps.getChromeWindow();
    const bounds = win?.getContentBounds() ?? { width: 1280, height: 800 };
    return {
      w: Math.max(320, bounds.width),
      h: Math.max(320, bounds.height - CHROME_HEIGHT),
      dpr: 1,
      mobile: false,
    };
  }

  registerIpc(ipc: IpcMain): void {
    // ---- from the network worker
    ipc.on('skyhook:net', (_e: IpcMainEvent, msg: { kind: string; args: Record<string, unknown> }) => {
      this.fromNet(msg.kind, msg.args);
    });

    // ---- from a mirror tab's shim
    ipc.on('skyhook:input', (_e, ev: Record<string, unknown>) => {
      this.toNet('input', ev);
    });
    ipc.on('skyhook:scroll', (_e, ev: Record<string, unknown>) => {
      this.toNet('scroll', ev);
    });
    ipc.on('skyhook:applied', (_e, ev: Record<string, unknown>) => {
      this.toNet('ack', ev);
    });
    ipc.on('skyhook:want-image', async (_e, ev: { tab: number; hashes: string[] }) => {
      // Only ask for what the cross-flight cache does not already hold.
      const missing = ev.hashes.filter((h) => !this.deps.store.hasImage(h));
      if (missing.length) this.toNet('wantImage', { tab: ev.tab, hashes: missing });
      for (const hash of ev.hashes) {
        if (this.deps.store.hasImage(hash)) this.sendToTab(ev.tab, { kind: 'imageData', hash });
      }
    });
    ipc.on('skyhook:shim-ready', (e) => {
      for (const tab of this.tabs.values()) {
        if (tab.view.webContents.id !== e.sender.id) continue;
        tab.ready = true;
        for (const frame of tab.backlog) tab.view.webContents.send('skyhook:frame', frame);
        tab.backlog = [];
      }
    });

    // ---- from the chrome UI
    ipc.handle('skyhook:ui', async (_e, msg: { action: string; args: Record<string, unknown> }) => {
      return this.fromUI(msg.action, msg.args ?? {});
    });
  }

  private async fromUI(action: string, args: Record<string, unknown>): Promise<unknown> {
    switch (action) {
      case 'openTab':
        this.toNet('openTab', { url: String(args.url ?? '') });
        return null;
      case 'closeTab':
        this.toNet('closeTab', { tab: Number(args.tab) });
        this.destroyTab(Number(args.tab));
        return null;
      case 'navigate': {
        const tab = Number(args.tab ?? this.active);
        const url = String(args.url ?? '');
        const spec = this.speculations.get(url);
        if (spec) {
          // The speculation is already here: paint it now and let the real
          // navigation reconcile. This is the zero-round-trip link follow.
          this.speculations.delete(url);
          this.sendToTab(tab, { kind: 'snapshot', tab, snapshot: spec });
        }
        this.toNet('navigate', { tab, url, action: String(args.action ?? '') });
        return null;
      }
      case 'selectTab':
        this.setActive(Number(args.tab));
        return null;
      case 'pair': {
        const pairing = args.pairing as Pairing;
        await this.deps.store.writePairing(pairing);
        this.configure(pairing);
        return null;
      }
      case 'pairing':
        return this.deps.store.readPairing();
      case 'archive':
        return this.deps.store.readArchive();
      case 'bookmarks':
        return this.deps.store.readBookmarks();
      case 'setBookmarks':
        await this.deps.store.writeBookmarks(args.bookmarks as { title: string; url: string }[]);
        return null;
      case 'adapter':
        this.toNet('adapter', args);
        return null;
      case 'kill':
        this.toNet('kill', {});
        return null;
      case 'reconnect':
        this.toNet('disconnect', {});
        if (this.pairing) this.configure(this.pairing);
        return null;
      case 'layout':
        this.layout();
        return null;
      default:
        return null;
    }
  }

  private fromNet(kind: string, args: Record<string, unknown>): void {
    switch (kind) {
      case 'welcome': {
        const w = args as unknown as { sessionId: string; tabs: { tab: number; url: string }[] };
        void this.deps.store.writeSessionId(w.sessionId);
        for (const t of w.tabs ?? []) this.ensureTab(t.tab);
        this.toUI('welcome', args);
        break;
      }
      case 'snapshot': {
        const tab = Number(args.tab);
        this.ensureTab(tab);
        this.sendToTab(tab, { kind: 'snapshot', tab, snapshot: args.snapshot });
        break;
      }
      case 'speculative': {
        const snap = args.snapshot as { url?: string } | undefined;
        if (snap?.url) {
          this.speculations.set(snap.url, snap);
          // Bounded: speculation must never crowd out real state.
          if (this.speculations.size > 8) {
            const first = this.speculations.keys().next().value;
            if (first) this.speculations.delete(first);
          }
        }
        break;
      }
      case 'mutation': {
        const tab = Number(args.tab);
        this.sendToTab(tab, {
          kind: 'mutation', tab, seq: Number(args.seq), cause: Number(args.cause),
          mutation: args.mutation,
        });
        break;
      }
      case 'imageMeta':
        this.sendToTab(Number(args.tab), { kind: 'imageMeta', tab: Number(args.tab), meta: args.meta });
        break;
      case 'imageData': {
        const hash = String(args.hash);
        const data = args.data as Uint8Array;
        void this.deps.store.writeImage(hash, data).then(() => {
          this.sendToTab(Number(args.tab), { kind: 'imageData', tab: Number(args.tab), hash });
        });
        break;
      }
      case 'tabState': {
        const tab = this.tabs.get(Number(args.tab));
        const st = args.state as { url: string; title: string; loading: boolean; closed: boolean };
        if (st?.closed) {
          this.destroyTab(Number(args.tab));
        } else if (tab && st) {
          tab.url = st.url || tab.url;
          tab.title = st.title || tab.title;
          tab.loading = st.loading;
        }
        this.toUI('tabState', args);
        break;
      }
      case 'adapter': {
        const records = args.records as never[];
        void this.deps.store.appendArchive(records);
        this.toUI('adapter', args);
        break;
      }
      case 'status':
        for (const tab of this.tabs.values()) {
          this.sendToTab(tab.id, { kind: 'offline', tab: tab.id, offline: args.online === false });
        }
        this.toUI('status', args);
        break;
      case 'stats':
      case 'log':
        this.toUI(kind, args);
        break;
      default:
        break;
    }
  }

  private ensureTab(id: number): Tab {
    const existing = this.tabs.get(id);
    if (existing) return existing;
    const view = new WebContentsView({
      webPreferences: {
        preload: this.deps.preload,
        contextIsolation: true,
        nodeIntegration: false,
        sandbox: true,
        javascript: true, // the shim only: page script is blocked by CSP
        webSecurity: true,
        spellcheck: true,
      },
    });
    void view.webContents.loadURL(`skyhook://mirror/${id}`);
    const tab: Tab = { id, view, url: '', title: '', loading: true, ready: false, backlog: [] };
    this.tabs.set(id, tab);
    const win = this.deps.getChromeWindow();
    win?.contentView.addChildView(view);
    this.setActive(id);
    this.layout();
    return tab;
  }

  private destroyTab(id: number): void {
    const tab = this.tabs.get(id);
    if (!tab) return;
    this.tabs.delete(id);
    const win = this.deps.getChromeWindow();
    win?.contentView.removeChildView(tab.view);
    tab.view.webContents.close();
    if (this.active === id) {
      const next = this.tabs.keys().next().value;
      this.active = typeof next === 'number' ? next : 0;
      this.layout();
    }
    this.toUI('tabClosed', { tab: id });
  }

  private setActive(id: number): void {
    if (!this.tabs.has(id)) return;
    this.active = id;
    this.layout();
    this.toUI('activeTab', { tab: id });
  }

  /** Lays the active mirror view out below the chrome strip. */
  layout(): void {
    const win = this.deps.getChromeWindow();
    if (!win) return;
    const bounds = win.getContentBounds();
    for (const [id, tab] of this.tabs) {
      if (id === this.active) {
        tab.view.setBounds({
          x: 0, y: CHROME_HEIGHT,
          width: bounds.width, height: Math.max(0, bounds.height - CHROME_HEIGHT),
        });
        tab.view.setVisible(true);
      } else {
        tab.view.setVisible(false);
      }
    }
  }

  private sendToTab(id: number, frame: Record<string, unknown>): void {
    const tab = this.tabs.get(id) ?? (frame.kind === 'snapshot' ? this.ensureTab(id) : undefined);
    if (!tab) return;
    if (!tab.ready) {
      tab.backlog.push(frame);
      return;
    }
    tab.view.webContents.send('skyhook:frame', frame);
  }

  private toNet(name: string, args: Record<string, unknown>): void {
    this.deps.getNetWindow()?.webContents.send('skyhook:command', { name, args });
  }

  private toUI(kind: string, args: Record<string, unknown>): void {
    this.deps.getChromeWindow()?.webContents.send('skyhook:ui-event', { kind, args });
  }
}
