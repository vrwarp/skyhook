/**
 * The plane-side half of a diagnostic capture.
 *
 * The server asks; this gathers what only this side knows — the document the
 * patcher actually built, what it believes about it, and a picture of it — and
 * sends it up. The bundle is written landside, so nothing here is stored: every
 * byte produced here crosses the link, which is why the work is done in two
 * distinct phases.
 *
 * **Freeze**, synchronously, the moment the request arrives: the mirrored DOM
 * and the patcher's state. It has to be synchronous because a capture is
 * usually taken because the server just noticed a divergence, and the very next
 * frame it sends is the resync that repairs it. An `await` here is long enough
 * to lose the evidence.
 *
 * **Render and send**, afterwards and slowly, from the frozen copy: rasterise
 * a screenshot, gzip anything textual, chunk it all up.
 *
 * The screenshot is the part with no obvious implementation. There is no API
 * that hands a page a picture of itself: `getDisplayMedia` needs a permission
 * prompt and a user gesture, and a canvas cannot draw a live DOM. What works is
 * the old trick — an SVG `<foreignObject>` wrapping the serialised markup,
 * loaded as an image and drawn onto a canvas — with two catches this file
 * spends most of its length on. The markup must be well-formed XML, and real
 * pages carry attribute names (`@click`, `:class`, `x-on:keyup.enter`) that are
 * not; and an SVG image may not load anything external, so every mirrored image
 * has to be inlined as a data URI first, which it can be, because the bytes are
 * already plane-side in Cache Storage.
 */
import { IMAGE_CACHE, imageCacheKey } from '../shared/caches.js';
import type { CaptureRequest } from '../shared/protocol.js';
import type { MirrorFreeze } from '../mirror/host.js';
import * as clientlog from './clientlog.js';

/** One finished artifact, ready for the wire. */
export interface CaptureArtifact {
  /** Path inside the bundle, under `planeside/`. A `.gz` suffix means the
   *  server should decompress it before storing. */
  name: string;
  data: Uint8Array;
}

/** What the shell hands in, so this module never touches globals it cannot see. */
export interface CaptureInput {
  request: CaptureRequest;
  /** Frames frozen synchronously, before anything awaited. */
  frozen: MirrorFreeze[];
  /** Whatever the shell knows about itself. */
  shell: Record<string, unknown>;
}

/** Widest screenshot produced, in CSS pixels. Wider is not more legible. */
const MAX_SHOT_WIDTH = 1400;
/** Tallest screenshot produced. A very long page is cropped, and says so. */
const MAX_SHOT_HEIGHT = 4000;
/** Ceiling on image bytes inlined into one screenshot. */
const MAX_INLINE_IMAGE_BYTES = 3 << 20;

/**
 * Gathers this side's artifacts, cheapest and most valuable first.
 *
 * Order is a real decision, not a stylistic one. The link may die halfway
 * through, and a partial capture should be missing the screenshot rather than
 * missing the DOM: a bundle with the mirrored document and no picture explains
 * the bug, and a bundle with a picture and no document only illustrates it.
 */
export async function gather(input: CaptureInput): Promise<CaptureArtifact[]> {
  const out: CaptureArtifact[] = [];
  const budget = new Budget(input.request.maxBytes || 4 << 20);
  const notes: string[] = [];

  const push = async (name: string, body: string | Uint8Array): Promise<void> => {
    const raw = typeof body === 'string' ? new TextEncoder().encode(body) : body;
    const compress = typeof body === 'string';
    const packed = compress ? await gzip(raw) : raw;
    const finalName = packed === raw || !compress ? name : `${name}.gz`;
    if (!budget.take(packed.length)) {
      notes.push(`${name} was left out: the ${budget.limit} byte upload budget was spent`);
      return;
    }
    out.push({ name: finalName, data: packed });
  };

  await push('client.json', JSON.stringify(clientReport(input), null, 2));

  for (const frame of input.frozen) {
    const base = `tabs/${frame.tab}`;
    if (frame.error) notes.push(`tab ${frame.tab}: ${frame.error}`);
    await push(`${base}/state.json`, JSON.stringify(frame.state, null, 2));
    await push(`${base}/fingerprint.json`, JSON.stringify(frame.fingerprint));
    if (frame.html) await push(`${base}/mirror.html`, frame.html);
  }

  if (input.request.screenshots) {
    for (const frame of input.frozen) {
      if (!frame.html) continue;
      try {
        const shot = await screenshot(frame);
        if (shot.data) {
          await push(`tabs/${frame.tab}/screenshot.webp`, shot.data);
        }
        // Beside the picture, and even when there is no picture: what was
        // attempted is worth as much as what came out.
        if (shot.meta) {
          await push(`tabs/${frame.tab}/screenshot.json`, JSON.stringify(shot.meta, null, 2));
        }
        if (shot.note) notes.push(`tab ${frame.tab}: ${shot.note}`);
      } catch (err) {
        notes.push(`tab ${frame.tab}: the screenshot failed: ${String(err)}`);
      }
    }
  }

  // Last, because it is the one artifact that can describe its own gathering.
  for (const note of notes) clientlog.record('warn', `capture: ${note}`);
  await push('client.log', clientlog.text());
  return out;
}

/** Tracks how much of the server's upload budget is left. */
class Budget {
  readonly limit: number;
  private spent = 0;

  constructor(limit: number) {
    this.limit = limit;
  }

  take(n: number): boolean {
    if (this.spent + n > this.limit) return false;
    this.spent += n;
    return true;
  }
}

/** What this device and this build are, which the server cannot know. */
function clientReport(input: CaptureInput): Record<string, unknown> {
  const nav = navigator as Navigator & { deviceMemory?: number; connection?: unknown };
  return {
    captureId: input.request.id,
    reason: input.request.reason,
    note: input.request.note,
    takenAt: new Date().toISOString(),
    sinceLoadMs: Math.round(performance.now()),
    userAgent: nav.userAgent,
    languages: nav.languages,
    hardwareConcurrency: nav.hardwareConcurrency,
    deviceMemoryGb: nav.deviceMemory,
    online: nav.onLine,
    // Not the pairing: a bundle is a thing people send to each other, and the
    // token in it would be the whole credential.
    origin: location.origin,
    screen: {
      w: screen.width, h: screen.height, dpr: devicePixelRatio,
      innerW: innerWidth, innerH: innerHeight,
    },
    visibility: document.visibilityState,
    droppedLogLines: clientlog.droppedCount(),
    shell: input.shell,
  };
}

// ------------------------------------------------------------------ screenshot

interface Shot {
  data?: Uint8Array;
  note?: string;
  /**
   * What the picture is a picture *of*, written into the bundle beside it.
   *
   * This one covers the top of the document up to MAX_SHOT_HEIGHT; the
   * landside one covers the whole scrollable page, or — past its own limit —
   * only the viewport. Two images of the same tab, at different scales, over
   * different regions, invite exactly one mistake: diffing them and believing
   * the result. Each says what it holds so that comparison can be made
   * honestly, or knowingly not made.
   */
  meta?: ShotMeta;
}

interface ShotMeta {
  /** "page" when the whole document is in the image, "top" when it is cropped. */
  covers: 'page' | 'top';
  format: string;
  /** The region drawn, in CSS pixels. */
  width: number;
  height: number;
  /** The document's full size, of which the above is what fitted. */
  pageWidth: number;
  pageHeight: number;
  dpr: number;
  bytes: number;
  /** Images the rasteriser could not draw, and so are blank in the picture. */
  imagesMissing: number;
  imagesSkipped: number;
}

/**
 * Rasterises a frozen mirror document.
 *
 * Everything is done from the frozen HTML rather than from the live frame, so
 * the picture is of the document at the instant the capture was asked for, not
 * of whatever replaced it while this was running.
 */
async function screenshot(frame: MirrorFreeze): Promise<Shot> {
  const notes: string[] = [];
  const parsed = new DOMParser().parseFromString(frame.html, 'text/html');
  if (!parsed.body) return { note: 'the mirrored document did not re-parse' };

  const inlined = await inlineImages(parsed, frame.images);
  if (inlined.missing > 0) {
    notes.push(`${inlined.missing} image(s) were not in the plane-side cache and `
      + 'are blank in the screenshot');
  }
  if (inlined.skipped > 0) {
    notes.push(`${inlined.skipped} image(s) were left out of the screenshot to stay `
      + `under ${MAX_INLINE_IMAGE_BYTES} bytes of inlined data`);
  }

  const width = Math.min(frame.width, MAX_SHOT_WIDTH);
  const full = Math.max(frame.docHeight, frame.height, 1);
  const height = Math.min(full, MAX_SHOT_HEIGHT);
  if (height < full) {
    notes.push(`the page is ${full}px tall; the screenshot is the top ${height}px`);
  }

  const wrapper = parsed.createElement('div');
  wrapper.setAttribute('style',
    `width:${width}px; background:#fff; color:#111; font:14px system-ui, sans-serif;`);
  for (const style of Array.from(parsed.head?.querySelectorAll('style') ?? [])) {
    wrapper.appendChild(style.cloneNode(true));
  }
  while (parsed.body.firstChild) wrapper.appendChild(parsed.body.firstChild);
  makeXMLSafe(wrapper);

  const markup = new XMLSerializer().serializeToString(wrapper);
  const svg = `<svg xmlns="http://www.w3.org/2000/svg" width="${width}" height="${height}">`
    + `<foreignObject x="0" y="0" width="${width}" height="${height}">${markup}</foreignObject>`
    + '</svg>';

  const image = new Image();
  // A data URL rather than a blob URL: a canvas that has drawn a blob-backed
  // SVG is tainted in some builds, and a tainted canvas cannot be read back —
  // which would fail at toBlob, after all the work, with a security error.
  image.src = `data:image/svg+xml;charset=utf-8,${encodeURIComponent(svg)}`;
  try {
    await image.decode();
  } catch (err) {
    return {
      note: 'the mirrored document could not be rendered into an image '
        + `(${String(err)}); mirror.html is the reliable artifact`,
    };
  }

  const canvas = document.createElement('canvas');
  canvas.width = width;
  canvas.height = height;
  const ctx = canvas.getContext('2d');
  if (!ctx) return { note: 'this browser gave no 2d canvas context' };
  ctx.fillStyle = '#fff';
  ctx.fillRect(0, 0, width, height);
  ctx.drawImage(image, 0, 0);

  const blob = await new Promise<Blob | null>((resolve) => {
    canvas.toBlob(resolve, 'image/webp', 0.72);
  });
  if (!blob) return { note: 'the canvas produced no image' };
  const data = new Uint8Array(await blob.arrayBuffer());
  return {
    data,
    note: notes.length ? notes.join('; ') : undefined,
    meta: {
      covers: height < full ? 'top' : 'page',
      format: 'webp',
      width,
      height,
      pageWidth: frame.width,
      pageHeight: full,
      dpr: 1,
      bytes: data.length,
      imagesMissing: inlined.missing,
      imagesSkipped: inlined.skipped,
    },
  };
}

/**
 * Replaces every mirrored image's source with a data URI from Cache Storage.
 *
 * An SVG image is not allowed to load anything external, and the `src` a
 * mirrored `<img>` carries is a blob URL — which is external to the SVG and
 * says nothing about its content besides. So the hash is read from
 * `data-skyhook-img`, the same attribute `hashFromImage` reads, and the bytes
 * are inlined outright. They are available because the network worker put
 * every image this client has ever been sent into Cache Storage, which is the
 * same fact that lets a warm client paint a page it never downloaded.
 */
async function inlineImages(
  doc: Document, hashes: string[],
): Promise<{ missing: number; skipped: number }> {
  const urls = new Map<string, string>();
  let spent = 0;
  let missing = 0;
  let skipped = 0;

  if (hashes.length && typeof caches !== 'undefined') {
    let cache: Cache | undefined;
    try {
      cache = await caches.open(IMAGE_CACHE);
    } catch {
      cache = undefined;
    }
    for (const hash of hashes) {
      if (!cache) break;
      if (spent >= MAX_INLINE_IMAGE_BYTES) {
        skipped += 1;
        continue;
      }
      const res = await cache.match(imageCacheKey(hash));
      if (!res) {
        missing += 1;
        continue;
      }
      const blob = await res.blob();
      if (spent + blob.size > MAX_INLINE_IMAGE_BYTES) {
        skipped += 1;
        continue;
      }
      spent += blob.size;
      urls.set(hash, await toDataURL(blob));
    }
  }

  for (const el of Array.from(doc.querySelectorAll('img'))) {
    const hash = el.getAttribute('data-skyhook-img') ?? '';
    const url = hash ? urls.get(hash) : undefined;
    if (url) {
      el.setAttribute('src', url);
    } else {
      // An <img> with a src that will not resolve renders as a broken-image
      // glyph, which is misleading: the mirror showed a picture there, and the
      // screenshot should show that something was there rather than that
      // something failed.
      el.removeAttribute('src');
      el.setAttribute('style',
        `${el.getAttribute('style') ?? ''};background:#e9e9e9;border:1px dashed #bbb`);
    }
    // Background blurhashes are inline styles and survive on their own.
  }
  return { missing, skipped };
}

function toDataURL(blob: Blob): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(String(reader.result));
    reader.onerror = () => reject(reader.error ?? new Error('unreadable blob'));
    reader.readAsDataURL(blob);
  });
}

/** XML names, which is a narrower set than HTML attribute names. */
const XML_NAME = /^[A-Za-z_][\w.-]*(:[A-Za-z_][\w.-]*)?$/;

/**
 * Makes a subtree serialisable as XML.
 *
 * `XMLSerializer` will happily emit `<div @click="go()">`, and the SVG parser
 * will then reject the whole document — so one Vue attribute anywhere on the
 * page costs the entire screenshot. Framework directives are exactly what a
 * mirrored page is full of, so they are dropped here rather than gambled on.
 * Comments containing `--` are the same problem in a different place.
 */
function makeXMLSafe(root: Element): void {
  const walker = root.ownerDocument.createTreeWalker(
    root, NodeFilter.SHOW_ELEMENT | NodeFilter.SHOW_COMMENT,
  );
  const doomed: Node[] = [];
  for (let n: Node | null = walker.currentNode; n; n = walker.nextNode()) {
    if (n.nodeType === Node.COMMENT_NODE) {
      if ((n.nodeValue ?? '').includes('--')) doomed.push(n);
      continue;
    }
    const el = n as Element;
    for (const attr of Array.from(el.attributes)) {
      if (!XML_NAME.test(attr.name) || attr.name.startsWith('xmlns')) {
        el.removeAttribute(attr.name);
      }
    }
  }
  for (const n of doomed) n.parentNode?.removeChild(n);
}

// ---------------------------------------------------------------- compression

/**
 * Compresses a textual artifact before it goes up.
 *
 * This is the one place in Skyhook where the client pays for bytes, and a
 * mirrored document is mostly repeated class names: gzip takes a megabyte of
 * HTML down to about eighty kilobytes, which on this link is the difference
 * between four seconds and forty. Returns the input unchanged where
 * CompressionStream is unavailable, and the caller names the artifact
 * accordingly.
 */
async function gzip(data: Uint8Array): Promise<Uint8Array> {
  if (typeof CompressionStream === 'undefined') return data;
  try {
    const stream = new Blob([data as BlobPart]).stream()
      .pipeThrough(new CompressionStream('gzip'));
    const packed = new Uint8Array(await new Response(stream).arrayBuffer());
    return packed.length < data.length ? packed : data;
  } catch {
    return data;
  }
}
