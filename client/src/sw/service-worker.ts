/**
 * The service worker: offline shell, image cache, and the egress denial that
 * replaces Electron's session-level request blocking.
 *
 * Three jobs:
 *
 *  1. Precache the app so it starts with no network at all — the whole point,
 *     given the client is opened at 35,000 feet.
 *  2. Serve /img/<hash> out of Cache Storage, for anything that asks for one
 *     by URL. The mirror frame does not: it is sandboxed, so it is not a
 *     client of this worker, and the shell hands it a blob instead.
 *  3. Refuse every cross-origin request. The plane-side client makes exactly
 *     one connection — the WebTransport session to the VPS — and that is not a
 *     fetch, so it is unaffected by anything here.
 */
/// <reference lib="webworker" />
import { IMAGE_CACHE } from '../shared/caches.js';

declare const self: ServiceWorkerGlobalScope;

const VERSION = 'v1';
const SHELL_CACHE = `skyhook-shell-${VERSION}`;

/** Files that must be present for a cold, offline start. */
const SHELL = [
  '/',
  '/index.html',
  '/app.css',
  '/app.js',
  '/net.worker.js',
  '/manifest.webmanifest',
];

/** A 1x1 transparent PNG, served for an image we do not hold yet. */
const TRANSPARENT_PNG = Uint8Array.from(atob(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==',
), (c) => c.charCodeAt(0));

self.addEventListener('install', (event) => {
  event.waitUntil((async () => {
    const cache = await caches.open(SHELL_CACHE);
    // Individual failures must not abort the install: a missing optional asset
    // is better than no service worker at all.
    await Promise.all(SHELL.map(async (url) => {
      try {
        await cache.add(new Request(url, { cache: 'reload' }));
      } catch {
        // Left uncached; the network path still works while online.
      }
    }));
    await self.skipWaiting();
  })());
});

self.addEventListener('activate', (event) => {
  event.waitUntil((async () => {
    const names = await caches.keys();
    await Promise.all(names.map((n) => {
      // Image cache survives upgrades: it is the cross-flight cache, and
      // throwing it away would cost a whole flight's worth of bytes.
      if (n === IMAGE_CACHE || n === SHELL_CACHE) return Promise.resolve(false);
      return caches.delete(n);
    }));
    await self.clients.claim();
  })());
});

self.addEventListener('fetch', (event) => {
  const req = event.request;
  const url = new URL(req.url);

  // Egress denial. Nothing in this app has any business talking to another
  // origin, and the mirror renders content from arbitrary sites.
  if (url.origin !== self.location.origin) {
    event.respondWith(new Response('blocked by skyhook', {
      status: 403,
      statusText: 'Forbidden',
    }));
    return;
  }

  if (url.pathname.startsWith('/img/')) {
    event.respondWith(serveImage(url));
    return;
  }

  if (req.method !== 'GET') return;

  event.respondWith(serveShell(req));
});

/** Images come from the cache or not at all: the mirror never fetches remotely. */
async function serveImage(url: URL): Promise<Response> {
  const cache = await caches.open(IMAGE_CACHE);
  // The page appends a cache-buster when bytes arrive; match without it.
  const key = `/img/${url.pathname.slice('/img/'.length)}`;
  const hit = await cache.match(key);
  if (hit) return hit;
  // A miss is answered rather than 404'd, so an image whose bytes are still
  // crossing the link draws nothing rather than a broken-image marker — but it
  // says so, because a placeholder is not the picture.
  return new Response(TRANSPARENT_PNG, {
    headers: {
      'content-type': 'image/png',
      'cache-control': 'no-store',
      'x-skyhook-miss': '1',
    },
  });
}

/** Cache-first for the shell: a cold start on a dead link must still work. */
async function serveShell(req: Request): Promise<Response> {
  const cache = await caches.open(SHELL_CACHE);
  const hit = await cache.match(req, { ignoreSearch: true });
  if (hit) {
    // Refresh in the background so the next start is current.
    void refresh(cache, req);
    return hit;
  }
  try {
    const res = await fetch(req);
    if (res.ok && req.method === 'GET') await cache.put(req, res.clone());
    return res;
  } catch {
    const fallback = await cache.match('/index.html');
    if (fallback) return fallback;
    return new Response('offline and not cached', { status: 504 });
  }
}

async function refresh(cache: Cache, req: Request): Promise<void> {
  try {
    const res = await fetch(req);
    if (res.ok) await cache.put(req, res);
  } catch {
    // Offline: the cached copy stands.
  }
}

self.addEventListener('message', (event) => {
  const data = event.data as { kind?: string } | undefined;
  if (data?.kind === 'skip-waiting') void self.skipWaiting();
});

export {};
