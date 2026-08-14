/**
 * Build script for the PWA.
 *
 * Three bundles — the app shell, the network worker, and the service worker —
 * plus the static files copied verbatim. esbuild rather than a bundler with a
 * plugin ecosystem: the whole build should take less time than a single round
 * trip on the link this thing exists for.
 */
import { build, context } from 'esbuild';
import { cp, mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import { deflateSync } from 'node:zlib';

const watch = process.argv.includes('--watch');
const dev = watch || process.argv.includes('--dev');

const targets = [
  { in: 'src/app/main.ts', out: 'dist/app.js', format: 'esm' },
  { in: 'src/worker/net.worker.ts', out: 'dist/net.worker.js', format: 'esm' },
  // The service worker is a module worker; Chrome has supported that since 91.
  { in: 'src/sw/service-worker.ts', out: 'dist/sw.js', format: 'esm' },
];

const staticFiles = [
  'index.html',
  'app.css',
  'manifest.webmanifest',
  'icon.svg',
];

async function bundle() {
  for (const t of targets) {
    const options = {
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
    };
    if (watch) {
      const ctx = await context(options);
      await ctx.watch();
    } else {
      await build(options);
    }
  }
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

  // A build stamp the service worker can key its cache on, so a deploy does
  // not serve half of one version and half of another.
  const pkg = JSON.parse(await readFile('package.json', 'utf8'));
  await writeFile('dist/version.json', JSON.stringify({
    version: pkg.version,
    built: process.env.SOURCE_DATE_EPOCH ?? '',
  }));
}

async function run() {
  // A stale bundle from an earlier layout would be precached by the service
  // worker and served for a very long time.
  if (!watch) await rm('dist', { recursive: true, force: true });
  await bundle();
  await assets();
  if (watch) console.log('watching for changes');
}

run().catch((err) => {
  console.error(err);
  process.exit(1);
});
