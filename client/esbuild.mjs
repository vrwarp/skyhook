/**
 * Build script for the PWA.
 *
 * Three bundles — the app shell, the network worker, and the service worker —
 * plus the static files copied verbatim. esbuild rather than a bundler with a
 * plugin ecosystem: the whole build should take less time than a single round
 * trip on the link this thing exists for.
 */
import { build, context } from 'esbuild';
import { createHash } from 'node:crypto';
import { cp, mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import { deflateSync } from 'node:zlib';

const watch = process.argv.includes('--watch');
const dev = watch || process.argv.includes('--dev');

/** The two bundles the page loads. The service worker is built after them,
 *  because what goes into it is a hash of everything else. */
const targets = [
  { in: 'src/app/main.ts', out: 'dist/app.js', format: 'esm' },
  { in: 'src/worker/net.worker.ts', out: 'dist/net.worker.js', format: 'esm' },
];

// The service worker is a module worker; Chrome has supported that since 91.
const swTarget = { in: 'src/sw/service-worker.ts', out: 'dist/sw.js', format: 'esm' };

/** Everything the service worker precaches, and therefore everything whose
 *  contents decide which generation of the shell this is. */
const SHELL = ['index.html', 'app.css', 'app.js', 'net.worker.js', 'manifest.webmanifest'];

const staticFiles = [
  'index.html',
  'app.css',
  'manifest.webmanifest',
  'icon.svg',
];

/** The version the app calls itself, from package.json. */
const pkg = JSON.parse(await readFile('package.json', 'utf8'));

/**
 * The identity every bundle is stamped with.
 *
 * `SKYHOOK_BUILD` is the generation of the shell; `SKYHOOK_VERSION` is the
 * human-facing number beside it. Both are compiled in rather than fetched,
 * because a fetch for "which build am I" is answered by the service worker out
 * of the very cache whose staleness is the question.
 */
function stampDefines(id) {
  return {
    SKYHOOK_BUILD: JSON.stringify(id),
    SKYHOOK_VERSION: JSON.stringify(pkg.version),
  };
}

function optionsFor(t, define) {
  return {
    entryPoints: [t.in],
    outfile: t.out,
    bundle: true,
    platform: 'browser',
    format: t.format,
    // Chrome-only client: WebTransport, module workers and dialog elements
    // are all assumed present, so nothing is transpiled down.
    target: 'chrome126',
    sourcemap: dev ? 'inline' : true,
    minify: !dev,
    logLevel: 'info',
    ...(define ? { define } : {}),
  };
}

/**
 * The id the first pass compiles in, so the hash below is taken over bytes that
 * do not yet know what they hash to. Every build uses the same placeholder, so
 * identical sources still produce an identical hash and therefore an identical
 * id — the property the whole scheme rests on.
 */
const UNSTAMPED = 'unstamped';

async function bundle(id) {
  for (const t of targets) {
    const options = optionsFor(t, stampDefines(id));
    if (watch) {
      const ctx = await context(options);
      await ctx.watch();
    } else {
      await build(options);
    }
  }
}

/**
 * Which build this is: a hash of the files the service worker precaches, as
 * they are before the id is compiled into them.
 *
 * Content rather than a timestamp or a version number, for two reasons. A build
 * that changed nothing produces the same id, so a redeploy of identical bytes
 * does not evict a cache or make a phone re-fetch a shell it already holds. And
 * nobody has to remember to bump it — the failure mode of a hand-maintained
 * version is that it stays at `v1` through every deploy, which is exactly what
 * happened.
 */
async function buildId() {
  // Under --watch the shell is rebuilt continuously and the worker is built
  // once, so a content hash would go stale immediately. A per-run id means each
  // `npm run watch` gets a worker the browser treats as new.
  if (watch) return `dev-${Date.now().toString(36)}`;
  const hash = createHash('sha256');
  for (const file of SHELL) {
    hash.update(file);
    hash.update(await readFile(`dist/${file}`));
  }
  return hash.digest('hex').slice(0, 16);
}

/**
 * The service worker, stamped with the build it precaches.
 *
 * Built last, and deliberately not part of its own hash: a browser installs a
 * new worker when this file's bytes differ, so this file has to differ exactly
 * when the rest of the shell does.
 */
async function bundleServiceWorker(id) {
  await build(optionsFor(swTarget, stampDefines(id)));
}

/**
 * Emits a square PNG of one solid colour with a lighter mark, so the manifest
 * has real raster icons without committing binaries. Chrome wants PNG for the
 * install prompt; the SVG covers everything else.
 */
function png(size, rgb, mark) {
  const raw = Buffer.alloc((size * 3 + 1) * size);
  const inset = Math.round(size * 0.28);
  for (let y = 0; y < size; y++) {
    const row = y * (size * 3 + 1);
    raw[row] = 0; // filter type: none
    for (let x = 0; x < size; x++) {
      // A simple hook-ish glyph: a vertical stem with a foot, matching icon.svg.
      const stem = Math.abs(x - size / 2) < size * 0.05 && y > inset && y < size - inset;
      const foot = y > size - inset - size * 0.06 && y < size - inset &&
        x > size / 2 - size * 0.18 && x < size / 2;
      const colour = stem || foot ? mark : rgb;
      const p = row + 1 + x * 3;
      raw[p] = colour[0];
      raw[p + 1] = colour[1];
      raw[p + 2] = colour[2];
    }
  }

  const chunk = (type, data) => {
    const len = Buffer.alloc(4);
    len.writeUInt32BE(data.length, 0);
    const body = Buffer.concat([Buffer.from(type, 'latin1'), data]);
    const crc = Buffer.alloc(4);
    crc.writeUInt32BE(crc32(body) >>> 0, 0);
    return Buffer.concat([len, body, crc]);
  };

  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(size, 0);
  ihdr.writeUInt32BE(size, 4);
  ihdr[8] = 8; // bit depth
  ihdr[9] = 2; // colour type: truecolour
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk('IHDR', ihdr),
    chunk('IDAT', deflateSync(raw, { level: 9 })),
    chunk('IEND', Buffer.alloc(0)),
  ]);
}

const CRC_TABLE = (() => {
  const table = new Int32Array(256);
  for (let n = 0; n < 256; n++) {
    let c = n;
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    table[n] = c;
  }
  return table;
})();

function crc32(buf) {
  let c = -1;
  for (let i = 0; i < buf.length; i++) c = CRC_TABLE[(c ^ buf[i]) & 0xff] ^ (c >>> 8);
  return c ^ -1;
}

async function assets() {
  await mkdir('dist', { recursive: true });
  for (const file of staticFiles) {
    await cp(`public/${file}`, `dist/${file}`);
  }
  await writeFile('dist/icon-192.png', png(192, [17, 24, 39], [56, 189, 248]));
  await writeFile('dist/icon-512.png', png(512, [17, 24, 39], [56, 189, 248]));

}

/**
 * The build stamp, written where a person can read it.
 *
 * The same id the service worker keys its cache on, so "which build is that
 * phone actually running" is answerable from the outside — by fetching
 * /version.json — rather than by guessing from symptoms.
 */
async function stamp(id) {
  await writeFile('dist/version.json', JSON.stringify({
    version: pkg.version,
    build: id,
    built: process.env.SOURCE_DATE_EPOCH ?? '',
  }));
}

async function run() {
  // A stale bundle from an earlier layout would be precached by the service
  // worker and served for a very long time.
  if (!watch) await rm('dist', { recursive: true, force: true });
  if (watch) {
    // Watch mode has no content hash to compute — the id is per-run — so the
    // bundles can be stamped on the first and only pass.
    const id = await buildId();
    await bundle(id);
    await assets();
    await bundleServiceWorker(id);
    await stamp(id);
    console.log('watching for changes');
    return;
  }
  await bundle(UNSTAMPED);
  await assets();
  // In this order: the id is a hash of what the two steps above just wrote.
  const id = await buildId();
  // And then the two bundles are built again, carrying it. The app has to know
  // which build it is — it is the half of the comparison the server cannot make
  // for it — and it cannot read that from a file, because every file it could
  // read is answered from the service worker's cache, which is precisely the
  // thing whose age is in question. So the id is compiled in.
  //
  // Stamping after hashing rather than before is what keeps the id honest: the
  // hash is taken over the unstamped output, so it changes when the source
  // changes and not because the last build had a different id — which would be
  // a value that changed on every build and meant nothing.
  await bundle(id);
  await bundleServiceWorker(id);
  await stamp(id);
}

run().catch((err) => {
  console.error(err);
  process.exit(1);
});
