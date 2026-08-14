import { describe, expect, it } from 'vitest';

import { blurhashAverageColour, decodeBlurhash, decodeBlurhashToCSS } from '../src/shared/blurhash.js';

describe('blurhash', () => {
  const hash = 'LEHV6nWB2yk8pyo0adR*.7kCMdnj';

  it('decodes to the requested pixel dimensions', () => {
    const pixels = decodeBlurhash(hash, 8, 8);
    expect(pixels.length).toBe(8 * 8 * 4);
    expect(pixels[3]).toBe(255);
  });

  it('reports an average colour without decoding everything', () => {
    expect(blurhashAverageColour(hash)).toMatch(/^rgb\(\d+, \d+, \d+\)$/);
  });

  it('produces a CSS value usable as a background', () => {
    expect(decodeBlurhashToCSS(hash, 4, 4)).toContain('linear-gradient');
  });

  it('degrades to a flat colour rather than throwing on a bad hash', () => {
    expect(decodeBlurhashToCSS('!!', 4, 4)).toContain('linear-gradient');
  });

  it('rejects a hash whose length does not match its component count', () => {
    expect(() => decodeBlurhash('LEHV6nWB2yk8pyo0adR*', 4, 4)).toThrow();
  });
});
