/**
 * Turning a pairing — from a link fragment, from a pasted pairing.json, or from
 * IndexedDB — into the two URLs the transport dials.
 *
 * This is its own module because the interesting cases are all about where the
 * server is, and that is not always where it listens. Behind a reverse proxy
 * the client reaches one origin on one port for everything: the app, the
 * socket, and nothing else. There is no QUIC listener to find, and no
 * certificate to pin, because the certificate the browser validated belongs to
 * the proxy. A pairing that names the container's own ports is unreachable from
 * the plane side, which is exactly the failure it is worth being careful about.
 */

/** The parts of `location` that decide where the server is. */
export interface PageOrigin {
  /** "https:" or "http:". */
  protocol: string;
  /** Hostname without the port. */
  hostname: string;
  /** Port, or "" when the URL carries the scheme default. */
  port: string;
}

export interface PairingInput {
  host: string;
  port?: number;
  path?: string;
  token: string;
  certSha256?: string;
  fallbackUrl?: string;
  /** Skip WebTransport entirely: set when a proxy terminates TLS in front. */
  preferFallback?: boolean;
}

export interface TransportUrls {
  url: string;
  fallbackUrl: string;
  certHash?: string;
  preferFallback: boolean;
}

const DEFAULT_PATH = '/skyhook';

/** The port a page is on, including the one its scheme implies. */
export function portOf(loc: PageOrigin): number {
  if (loc.port) return Number(loc.port);
  return loc.protocol === 'https:' ? 443 : 80;
}

/**
 * Reads a pairing out of a one-time link fragment (`#token=...&host=...`).
 *
 * Anything the fragment leaves out is taken from the page's own origin rather
 * than from a built-in default: a link served by the server the client should
 * talk to already says where that is.
 */
export function pairingFromFragment(hash: string, loc: PageOrigin): PairingInput | undefined {
  const params = new URLSearchParams(hash.replace(/^#/, ''));
  const token = params.get('token');
  if (!token) return undefined;
  const port = params.get('port');
  return {
    host: params.get('host') || loc.hostname,
    port: port ? Number(port) : portOf(loc),
    path: params.get('path') || DEFAULT_PATH,
    token,
    certSha256: params.get('cert') || undefined,
    fallbackUrl: params.get('fallback') || undefined,
    preferFallback: isTrue(params.get('preferFallback')),
  };
}

/** Builds the URLs the transport dials, filling in whatever the pairing omits. */
export function transportUrls(pairing: PairingInput, loc: PageOrigin): TransportUrls {
  const path = pairing.path || DEFAULT_PATH;
  const host = pairing.host || loc.hostname;
  const port = pairing.port && pairing.port > 0 ? pairing.port : portOf(loc);
  const secure = loc.protocol === 'https:';

  return {
    url: `https://${authority(host, port, 443)}${path}`,
    // A pairing written by a proxied server carries the socket URL outright,
    // because only it knows what the proxy answers on. Without one, the socket
    // lives wherever the pairing says the server is.
    fallbackUrl: pairing.fallbackUrl
      ?? `${secure ? 'wss' : 'ws'}://${authority(host, port, secure ? 443 : 80)}${path}`,
    certHash: pairing.certSha256,
    // WebTransport needs a secure origin, so plain HTTP — development, and
    // nothing else — goes straight to the socket instead of waiting out a
    // handshake that cannot succeed. A proxied server says the same thing for
    // its own reason: there is no QUIC listener on the other side of it.
    preferFallback: pairing.preferFallback === true || !secure,
  };
}

/** host:port, dropping the port when it is the scheme's default. */
function authority(host: string, port: number, implied: number): string {
  const bracketed = host.includes(':') && !host.startsWith('[') ? `[${host}]` : host;
  return port === implied ? bracketed : `${bracketed}:${port}`;
}

function isTrue(v: string | null): boolean {
  return v === '1' || v === 'true';
}
