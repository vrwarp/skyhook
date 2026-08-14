/**
 * Cache Storage names, shared by everything that touches them.
 *
 * The network worker writes image bytes, the service worker serves them, and
 * the shell reads them directly — three contexts, one name, so a rename cannot
 * quietly orphan a flight's worth of images.
 */

/** Transcoded image bytes, keyed by `/img/<content hash>`. Survives flights. */
export const IMAGE_CACHE = 'skyhook-img-v1';

/** The URL an image's bytes are cached under. */
export function imageCacheKey(hash: string): string {
  return `/img/${hash}`;
}
