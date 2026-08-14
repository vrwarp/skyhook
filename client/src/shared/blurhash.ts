/**
 * Blurhash decoding, matching the encoder in internal/imgproc/blurhash.go.
 *
 * The decoded result is turned into a data: URL so it can be dropped straight
 * into a CSS background, which is the cheapest way to show something in an
 * <img> box while the real bytes are still crossing the link.
 */

const BASE83 = '0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz#$%*+,-.:;=?@[]^_{|}~';

function decode83(str: string): number {
  let value = 0;
  for (const ch of str) {
    const idx = BASE83.indexOf(ch);
    if (idx < 0) throw new Error('invalid blurhash character');
    value = value * 83 + idx;
  }
  return value;
}

function srgbToLinear(value: number): number {
  const v = value / 255;
  return v <= 0.04045 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4);
}

function linearToSRGB(value: number): number {
  const v = Math.max(0, Math.min(1, value));
  return v <= 0.0031308
    ? Math.round(v * 12.92 * 255 + 0.5)
    : Math.round((1.055 * Math.pow(v, 1 / 2.4) - 0.055) * 255 + 0.5);
}

function signPow(value: number, exp: number): number {
  return Math.sign(value) * Math.pow(Math.abs(value), exp);
}

function decodeDC(value: number): [number, number, number] {
  return [srgbToLinear(value >> 16), srgbToLinear((value >> 8) & 255), srgbToLinear(value & 255)];
}

function decodeAC(value: number, maxValue: number): [number, number, number] {
  const r = Math.floor(value / (19 * 19));
  const g = Math.floor(value / 19) % 19;
  const b = value % 19;
  return [
    signPow((r - 9) / 9, 2.0) * maxValue,
    signPow((g - 9) / 9, 2.0) * maxValue,
    signPow((b - 9) / 9, 2.0) * maxValue,
  ];
}

/** Decodes a blurhash into RGBA pixels. */
export function decodeBlurhash(hash: string, width: number, height: number): Uint8ClampedArray {
  if (hash.length < 6) throw new Error('blurhash too short');
  const sizeFlag = decode83(hash[0]);
  const numY = Math.floor(sizeFlag / 9) + 1;
  const numX = (sizeFlag % 9) + 1;
  const quantisedMax = decode83(hash[1]);
  const maxValue = (quantisedMax + 1) / 166;
  if (hash.length !== 4 + 2 * numX * numY) throw new Error('blurhash length mismatch');

  const colours: [number, number, number][] = [];
  for (let i = 0; i < numX * numY; i++) {
    colours.push(i === 0
      ? decodeDC(decode83(hash.slice(2, 6)))
      : decodeAC(decode83(hash.slice(4 + i * 2, 6 + i * 2)), maxValue));
  }

  const pixels = new Uint8ClampedArray(width * height * 4);
  for (let y = 0; y < height; y++) {
    for (let x = 0; x < width; x++) {
      let r = 0;
      let g = 0;
      let b = 0;
      for (let j = 0; j < numY; j++) {
        for (let i = 0; i < numX; i++) {
          const basis = Math.cos((Math.PI * x * i) / width) * Math.cos((Math.PI * y * j) / height);
          const colour = colours[i + j * numX];
          r += colour[0] * basis;
          g += colour[1] * basis;
          b += colour[2] * basis;
        }
      }
      const p = 4 * (x + y * width);
      pixels[p] = linearToSRGB(r);
      pixels[p + 1] = linearToSRGB(g);
      pixels[p + 2] = linearToSRGB(b);
      pixels[p + 3] = 255;
    }
  }
  return pixels;
}

/** Average colour of a blurhash: the DC term, and a one-line placeholder. */
export function blurhashAverageColour(hash: string): string {
  try {
    const [r, g, b] = decodeDC(decode83(hash.slice(2, 6)));
    return `rgb(${linearToSRGB(r)}, ${linearToSRGB(g)}, ${linearToSRGB(b)})`;
  } catch {
    return 'rgb(200, 200, 200)';
  }
}

/**
 * Renders a blurhash as a CSS background value. A tiny PNG is built by hand
 * because a canvas is not available in every context the shim runs in, and a
 * gradient approximation is both smaller and good enough at placeholder size.
 */
export function decodeBlurhashToCSS(hash: string, width = 8, height = 8): string {
  let pixels: Uint8ClampedArray;
  try {
    pixels = decodeBlurhash(hash, width, height);
  } catch {
    return `linear-gradient(${blurhashAverageColour(hash)}, ${blurhashAverageColour(hash)})`;
  }
  // A stack of horizontal gradients: one per row, blended vertically. This
  // renders in any engine without canvas, image decoding or a data URL.
  const rows: string[] = [];
  for (let y = 0; y < height; y++) {
    const stops: string[] = [];
    for (let x = 0; x < width; x++) {
      const p = 4 * (x + y * width);
      const pct = Math.round((x / (width - 1 || 1)) * 100);
      stops.push(`rgba(${pixels[p]}, ${pixels[p + 1]}, ${pixels[p + 2]}, ${(1 / height).toFixed(3)}) ${pct}%`);
    }
    rows.push(`linear-gradient(90deg, ${stops.join(', ')})`);
  }
  return rows.join(', ');
}
