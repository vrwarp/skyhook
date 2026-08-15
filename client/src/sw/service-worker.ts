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

/**
 * The build these bytes were made by: a hash of the shell files themselves,
 * substituted by esbuild (see esbuild.mjs).
 *
 * It is what makes an upgrade happen at all. A browser decides whether to
 * install a new service worker by byte-comparing this script, and this script
 * had no reason to differ between builds — so a deploy that changed every other
 * file in the shell left the worker, and therefore the cache, exactly as it
 * was. The stamp is in the bytes, so the worker differs precisely when the
 * shell it precaches differs, and not otherwise: an unchanged build produces an
 * unchanged worker and no reinstall.
 */
declare const SKYHOOK_BUILD: string;

/**
 * One cache per build, so a generation of the shell is swapped in whole.
 *
 * The name used to be fixed, which made the swap impossible even in principle:
 * there was one cache, the new files had to be written into it one at a time,
 * and every load in between got some of each. What that looks like on a phone
 * is the previous stylesheet drawing the current markup — a desktop chrome, on
 * a screen that has no room for one, with controls it has no rules for.
 */
const SHELL_CACHE = `skyhook-shell-${SKYHOOK_BUILD}`;

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
    // Nothing has been served from this cache yet — it is a generation that
    // does not exist until every file in it does. That is the whole difference
    // between an upgrade and a mixture.
    await self.skipWaiting();
  })());
});

self.addEventListener('activate', (event) => {
  event.waitUntil((async () => {
    const names = await caches.keys();
    await Promise.all(names.map((n) => {
      // Image cache survives upgrades: it is the cross-flight cache, and
      // throwing it away would cost a whole flight's worth of bytes. Every
      // other shell generation goes, which is what keeps one build's worth of
      // files from being reachable once the next one is live.
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

/**
 * Cache-first for the shell: a cold start on a dead link must still work, which
 * is the entire reason this worker exists.
 *
 * A hit is served and left alone. It used to be refreshed in the background —
 * "so the next start is current" — and that is the line that produced a shell
 * made of two builds. Each file was fetched and replaced on its own, whenever
 * it happened to be asked for, inside a cache that outlived every deploy. The
 * next start was current one file at a time, in whatever order the page had
 * requested them, so the ordinary state after a deploy was one generation's
 * markup drawn with another's stylesheet — and there is no version of that
 * which is merely cosmetic.
 *
 * A generation is now swapped whole, by installing a worker whose cache has a
 * different name. Which leaves this function with one job and no opinions about
 * freshness.
 */
async function serveShell(req: Request): Promise<Response> {
  const cache = await caches.open(SHELL_CACHE);
  const hit = await cache.match(req, { ignoreSearch: true });
  if (hit) return hit;
  try {
    const res = await fetch(req);
    // Anything outside the precached shell — an icon, the version stamp — is
    // kept as it is met. It belongs to this generation because this generation
    // is what asked for it.
    if (res.ok && req.method === 'GET') await cache.put(req, res.clone());
    return res;
  } catch {
    const fallback = await cache.match('/index.html');
    if (fallback) return fallback;
    return new Response('offline and not cached', { status: 504 });
  }
}

self.addEventListener('message', (event) => {
  const data = event.data as { kind?: string } | undefined;
  if (data?.kind === 'skip-waiting') void self.skipWaiting();
});

export {};
