import { describe, expect, it } from 'vitest';

import { pairingFromFragment, transportUrls } from '../src/app/pairing.js';

/** A stand-in for `location`, which is what these functions really read. */
const at = (url: string) => {
  const u = new URL(url);
  return { protocol: u.protocol, hostname: u.hostname, port: u.port };
};

const direct = at('https://vps.example.com:4434/');
const proxied = at('https://skyhook.example.com/');
const local = at('http://127.0.0.1:4434/');

describe('pairingFromFragment', () => {
  it('reads a direct pairing link', () => {
    const p = pairingFromFragment(
      '#token=t&host=vps.example.com&port=4433&path=/skyhook&cert=aGFzaA%3D%3D'
      + '&fallback=wss%3A%2F%2Fvps.example.com%3A4434%2Fskyhook',
      direct,
    );
    expect(p).toEqual({
      host: 'vps.example.com',
      port: 4433,
      path: '/skyhook',
      token: 't',
      certSha256: 'aGFzaA==',
      fallbackUrl: 'wss://vps.example.com:4434/skyhook',
      preferFallback: false,
    });
  });

  it('reads a proxied pairing link', () => {
    const p = pairingFromFragment(
      '#token=t&host=skyhook.example.com&port=443&path=/skyhook&preferFallback=1'
      + '&fallback=wss%3A%2F%2Fskyhook.example.com%2Fskyhook',
      proxied,
    );
    expect(p?.port).toBe(443);
    expect(p?.certSha256).toBeUndefined();
    expect(p?.preferFallback).toBe(true);
  });

  it('falls back to the page origin, not to a built-in port', () => {
    // A link that only carries a token was served by the server it pairs with,
    // so the page's own origin is the right answer — guessing 4433 would point
    // the client at a port a proxy never exposes.
    const p = pairingFromFragment('#token=t', proxied);
    expect(p).toMatchObject({ host: 'skyhook.example.com', port: 443, path: '/skyhook' });
  });

  it('ignores a fragment with no token', () => {
    expect(pairingFromFragment('#state=xyz', proxied)).toBeUndefined();
  });
});

describe('transportUrls', () => {
  it('keeps the pinned WebTransport URL for a direct server', () => {
    const urls = transportUrls({
      host: 'vps.example.com',
      port: 4433,
      path: '/skyhook',
      token: 't',
      certSha256: 'aGFzaA==',
      fallbackUrl: 'wss://vps.example.com:4434/skyhook',
    }, direct);
    expect(urls.url).toBe('https://vps.example.com:4433/skyhook');
    expect(urls.certHash).toBe('aGFzaA==');
    expect(urls.preferFallback).toBe(false);
  });

  it('dials the proxy on its own port and never tries WebTransport', () => {
    const urls = transportUrls({
      host: 'skyhook.example.com',
      port: 443,
      path: '/skyhook',
      token: 't',
      fallbackUrl: 'wss://skyhook.example.com/skyhook',
      preferFallback: true,
    }, proxied);
    expect(urls.fallbackUrl).toBe('wss://skyhook.example.com/skyhook');
    expect(urls.certHash).toBeUndefined();
    expect(urls.preferFallback).toBe(true);
  });

  it('derives a socket URL when a pasted pairing file has none', () => {
    const urls = transportUrls(
      { host: 'skyhook.example.com', port: 443, path: '/skyhook', token: 't' },
      proxied,
    );
    // Port 443 is implied by wss:, and a proxy will not answer on an explicit
    // one it was never given.
    expect(urls.fallbackUrl).toBe('wss://skyhook.example.com/skyhook');
  });

  it('uses the page port when the pairing omits one', () => {
    const urls = transportUrls({ host: 'vps.example.com', path: '/skyhook', token: 't' }, direct);
    expect(urls.fallbackUrl).toBe('wss://vps.example.com:4434/skyhook');
  });

  it('goes straight to a plain socket on a plain-HTTP page', () => {
    const urls = transportUrls(
      { host: '127.0.0.1', port: 4434, path: '/skyhook', token: 't' },
      local,
    );
    expect(urls.fallbackUrl).toBe('ws://127.0.0.1:4434/skyhook');
    expect(urls.preferFallback).toBe(true);
  });
});
