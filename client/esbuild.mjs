/**
 * Build script. esbuild rather than a bundler with a plugin ecosystem: the
 * client is a handful of entry points and the whole build should take less time
 * than a single round trip on the link this thing exists for.
 */
import { build, context } from 'esbuild';
import { cp, mkdir } from 'node:fs/promises';

const watch = process.argv.includes('--watch');

/** Entry points, each with the platform it actually runs on. */
const targets = [
  { in: 'src/main/index.ts', out: 'dist/main/index.js', platform: 'node', format: 'esm' },
  { in: 'src/preload/shim.ts', out: 'dist/preload/shim.js', platform: 'node', format: 'cjs' },
  { in: 'src/preload/net-preload.ts', out: 'dist/preload/net-preload.js', platform: 'node', format: 'cjs' },
  { in: 'src/preload/ui-preload.ts', out: 'dist/preload/ui-preload.js', platform: 'node', format: 'cjs' },
  { in: 'src/net/net.ts', out: 'dist/net/net.js', platform: 'browser', format: 'esm' },
  { in: 'src/ui/ui.ts', out: 'dist/ui/ui.js', platform: 'browser', format: 'esm' },
];

async function run() {
  await mkdir('dist', { recursive: true });
  for (const t of targets) {
    const options = {
      entryPoints: [t.in],
      outfile: t.out,
      bundle: true,
      platform: t.platform,
      format: t.format,
      target: t.platform === 'node' ? 'node20' : 'chrome126',
      sourcemap: true,
      logLevel: 'info',
      // Electron provides these; bundling them would break the preload bridge.
      external: ['electron', 'node:*', 'fs', 'path', 'url'],
    };
    if (watch) {
      const ctx = await context(options);
      await ctx.watch();
    } else {
      await build(options);
    }
  }
  await cp('src/ui/index.html', 'dist/ui/index.html');
  await cp('src/ui/ui.css', 'dist/ui/ui.css');
  await cp('src/net/net.html', 'dist/net/net.html');
  if (watch) console.log('watching for changes');
}

run().catch((err) => {
  console.error(err);
  process.exit(1);
});
