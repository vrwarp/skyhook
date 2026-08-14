/**
 * The Electron main process.
 *
 * It owns the window layout, the local store, the skyhook:// protocol, and the
 * routing between the network worker and each mirror tab. The security posture
 * is aggressive on purpose: mirror renderers are sandboxed, have no Node
 * integration, and every network request they could possibly make is denied.
 * The only bytes that reach a mirror tab come from the local store.
 */
import { app, BrowserWindow, ipcMain, net, protocol, session, shell } from 'electron';
import { fileURLToPath } from 'node:url';
import path from 'node:path';
import fs from 'node:fs';

import { Store } from './store.js';
import { TabManager } from './tabs.js';

const dirname = path.dirname(fileURLToPath(import.meta.url));
const rootDir = path.resolve(dirname, '..');

// skyhook: is the only scheme a mirror renderer may load. Registering it as
// standard + secure lets the mirror document have an origin, which CSP needs.
protocol.registerSchemesAsPrivileged([{
  scheme: 'skyhook',
  privileges: { standard: true, secure: true, supportFetchAPI: true, bypassCSP: false },
}]);

const store = new Store(path.join(app.getPath('userData'), 'store'));
let chromeWindow: BrowserWindow | null = null;
let netWindow: BrowserWindow | null = null;
let tabs: TabManager | null = null;

/** The mirror document shell: an empty page with a CSP that forbids scripts.
 *  The preload runs in an isolated world, which CSP does not apply to. */
const MIRROR_SHELL = `<!DOCTYPE html>
<html><head><meta charset="utf-8">
<meta http-equiv="Content-Security-Policy"
  content="default-src 'none'; img-src skyhook: data:; style-src 'unsafe-inline'; font-src skyhook:; script-src 'none'; connect-src 'none'; form-action 'none'">
<style>
  html, body { margin: 0; padding: 0; background: #fff; color: #111; }
  .skyhook-ghost { opacity: .55; font-style: italic; }
  html.skyhook-offline::before {
    content: "offline - showing cached page";
    position: fixed; inset: 0 0 auto 0; z-index: 2147483647;
    background: #b45309; color: #fff; font: 12px/1.8 system-ui, sans-serif;
    text-align: center;
  }
  img { background-repeat: no-repeat; background-size: cover; }
  [data-skyhook-static] {
    background: repeating-linear-gradient(45deg, #eee, #eee 8px, #e5e5e5 8px, #e5e5e5 16px);
  }
</style></head><body></body></html>`;

function registerProtocol(): void {
  protocol.handle('skyhook', async (request) => {
    const url = new URL(request.url);
    // skyhook://img/<hash> serves the local image cache; a miss returns a
    // transparent pixel and the shim asks the server for the bytes.
    if (url.hostname === 'img') {
      const hash = url.pathname.replace(/^\//, '');
      const entry = await store.readImage(hash);
      if (entry) {
        return new Response(new Uint8Array(entry.data), {
          headers: { 'content-type': entry.mime || 'application/octet-stream', 'cache-control': 'no-store' },
        });
      }
      return new Response(new Uint8Array(TRANSPARENT_PNG), {
        headers: { 'content-type': 'image/png', 'cache-control': 'no-store' },
      });
    }
    if (url.hostname === 'mirror') {
      return new Response(MIRROR_SHELL, {
        headers: { 'content-type': 'text/html; charset=utf-8' },
      });
    }
    if (url.hostname === 'ui' || url.hostname === 'net') {
      const file = path.join(rootDir, url.hostname, url.pathname.replace(/^\//, '') || 'index.html');
      if (!file.startsWith(rootDir)) return new Response('forbidden', { status: 403 });
      try {
        const data = await fs.promises.readFile(file);
        return new Response(new Uint8Array(data), { headers: { 'content-type': contentType(file) } });
      } catch {
        return new Response('not found', { status: 404 });
      }
    }
    return new Response('not found', { status: 404 });
  });
}

const TRANSPARENT_PNG = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==',
  'base64',
);

function contentType(file: string): string {
  if (file.endsWith('.html')) return 'text/html; charset=utf-8';
  if (file.endsWith('.js')) return 'text/javascript; charset=utf-8';
  if (file.endsWith('.css')) return 'text/css; charset=utf-8';
  if (file.endsWith('.svg')) return 'image/svg+xml';
  return 'application/octet-stream';
}

/**
 * Blocks every network egress a renderer could attempt. The plane-side client
 * makes exactly one network connection — the QUIC session to the VPS, from the
 * network worker — and nothing else is permitted to reach the open internet.
 */
function lockDownNetwork(): void {
  const ses = session.defaultSession;
  ses.webRequest.onBeforeRequest({ urls: ['*://*/*'] }, (details, callback) => {
    const allowed = details.url.startsWith('skyhook://') ||
      details.url.startsWith('devtools://') ||
      details.url.startsWith('blob:') ||
      details.url.startsWith('data:');
    callback({ cancel: !allowed });
  });
  ses.setPermissionRequestHandler((_wc, _permission, callback) => callback(false));
}

function createChromeWindow(): BrowserWindow {
  const win = new BrowserWindow({
    width: 1280,
    height: 860,
    title: 'Skyhook',
    backgroundColor: '#111827',
    webPreferences: {
      preload: path.join(rootDir, 'preload', 'ui-preload.js'),
      contextIsolation: true,
      nodeIntegration: false,
      sandbox: false,
      webviewTag: false,
    },
  });
  void win.loadURL('skyhook://ui/index.html');
  win.on('closed', () => {
    chromeWindow = null;
  });
  win.webContents.setWindowOpenHandler(({ url }) => {
    // Nothing plane-side opens an external browser; a stray link would leak.
    void shell.openExternal;
    void url;
    return { action: 'deny' };
  });
  return win;
}

function createNetWindow(): BrowserWindow {
  const win = new BrowserWindow({
    show: false,
    webPreferences: {
      preload: path.join(rootDir, 'preload', 'net-preload.js'),
      contextIsolation: true,
      nodeIntegration: false,
      sandbox: false,
      backgroundThrottling: false,
    },
  });
  void win.loadURL('skyhook://net/net.html');
  return win;
}

app.on('window-all-closed', () => {
  if (process.platform !== 'darwin') app.quit();
});

void app.whenReady().then(async () => {
  registerProtocol();
  lockDownNetwork();
  await store.open();

  netWindow = createNetWindow();
  chromeWindow = createChromeWindow();
  tabs = new TabManager({
    store,
    getChromeWindow: () => chromeWindow,
    getNetWindow: () => netWindow,
    preload: path.join(rootDir, 'preload', 'shim.js'),
  });
  tabs.registerIpc(ipcMain);

  const pairing = await store.readPairing();
  if (pairing) {
    tabs.configure(pairing);
  }

  app.on('activate', () => {
    if (BrowserWindow.getAllWindows().length === 0) chromeWindow = createChromeWindow();
  });
});

app.on('before-quit', () => {
  void store.close();
});

// Electron's net module is unused but imported deliberately: referencing it
// here documents that no other code may use it, since renderers are the only
// thing that could want the network and they are denied it above.
void net;
