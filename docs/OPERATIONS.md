# Running Skyhook

Deployment, pairing, security posture, and what to do when something breaks.
This is a single-user system: there is one VPS, one token, one profile.

## What you need

- A VPS with ~10 ms RTT to the open internet, 2 vCPU and 4 GB RAM. Chromium is
  the memory floor; image transcoding is the only CPU spike.
- A UDP port open for QUIC (4433 by default) and, optionally, a TCP port for the
  WebSocket fallback (4434). Some in-flight providers block UDP outright, which
  is the only reason the fallback exists.
- An encrypted volume for the data directory. It holds the browser profile:
  real cookies, real logins.

## Install

### Docker (recommended)

```sh
git clone https://github.com/vrwarp/skyhook && cd skyhook
SKYHOOK_HOSTS=vps.example.com docker compose -f deploy/docker-compose.yml up -d
docker compose -f deploy/docker-compose.yml exec skyhook \
  skyhookctl pairing -file /data/pairing.json
```

The image ships Chromium, `avifenc`, `cwebp` and Xvfb. Set `SKYHOOK_HEADFUL=1`
to run Chromium headful under Xvfb, which is the mitigation for sites with
aggressive bot detection.

It runs as uid **10001**, and `/data` in the image is owned by it. A named
volume inherits that; a **bind mount does not**, so a host directory needs
`chown 10001:10001` or the server cannot write its own profile.

### systemd on bare metal

```sh
go build -o /usr/local/bin/skyhookd ./cmd/skyhookd
go build -o /usr/local/bin/skyhookctl ./cmd/skyhookctl
useradd --system --home /var/lib/skyhook --create-home skyhook
install -m 0644 deploy/skyhook.service /etc/systemd/system/
systemctl enable --now skyhook
```

The unit runs as a dedicated user with `ProtectSystem=strict` and a writable
path limited to the data directory, which is the "Chromium profile under a
dedicated Unix user" mitigation from the design.

## Configuration

A JSON file, passed with `-config` or `SKYHOOK_CONFIG`. Everything has a usable
default; this is the whole surface:

```json
{
  "listen": ":4433",
  "webRoot": "/usr/share/skyhook/webapp",
  "insecureLoopback": false,
  "fallbackListen": ":4434",
  "dataDir": "/var/lib/skyhook",
  "hosts": ["vps.example.com"],
  "token": "",
  "headless": true,
  "sessionTtl": "12h",
  "compression": true,
  "prefetch": true,
  "maxTabs": 8,
  "imageQuality": 40,
  "imageCacheBytes": 536870912,
  "homeUrl": "",
  "adapters": ["googlechat"],
  "adapterConfig": "/var/lib/skyhook/adapters.json",
  "logLevel": "info"
}
```

Environment overrides: `SKYHOOK_LISTEN`, `SKYHOOK_FALLBACK_LISTEN`,
`SKYHOOK_DATA_DIR`, `SKYHOOK_TOKEN`, `SKYHOOK_CHROME`, `SKYHOOK_HOSTS`,
`SKYHOOK_HEADLESS`, `SKYHOOK_ADAPTERS`, `SKYHOOK_WEB_ROOT`,
`SKYHOOK_INSECURE_LOOPBACK`, `SKYHOOK_CHROME_ARGS`, `SKYHOOK_LOG_LEVEL`,
`SKYHOOK_PUBLIC_URL`, `SKYHOOK_BEHIND_PROXY`.

`publicUrl` and `behindProxy` are for deployments where the client does not
reach the server where the server listens — see
[behind a reverse proxy](#behind-a-reverse-proxy).

`chromeArgs` (or `SKYHOOK_CHROME_ARGS`, space separated) is appended to
Chromium's command line. It exists for sandboxing: Chromium isolates itself with
user namespaces, some container runtimes refuse those, and then Chromium does
not start at all. The container image probes for this at startup and falls back
to `--no-sandbox` with a loud log line; set the variable yourself to override
the probe either way.

`webRoot` is the built client (`client/dist`). The container image builds it and
sets `SKYHOOK_WEB_ROOT` already; a bare-metal install should either set the path
or copy the build to `<dataDir>/webapp`. With neither present the server serves a
page explaining how to build it, rather than nothing at all.

A missing token is generated on first start and written back to the config file
when one was supplied.

### Loopback demo mode

`skyhookd -demo` (or `scripts/demo.sh`, which also builds the client) serves the
app and the mirror connection over plain HTTP on `127.0.0.1`, with no TLS, no
QUIC and no pairing certificate. It exists because Chrome refuses to register a
service worker behind a self-signed certificate, so a local demo over TLS cannot
install or start offline; `127.0.0.1` is a secure origin whatever the scheme.

The server refuses to bind anything but a loopback address in this mode, and
`-demo-for 10m` stops it on its own. Do not run a real deployment this way: the
token would cross an unencrypted socket, and the whole point of the pinned
certificate is that it does not.

## Pairing

On startup the server logs a one-time pairing link:

```
level=INFO msg="pair the client by opening this link once" url="https://vps.example.com:4434/#token=…&cert=…"
```

Open it once in Chrome on the plane-side machine. The token and certificate pin
ride in the URL *fragment*, which browsers never send to a server, so the
credential reaches the app without touching any log. The app stores it in
IndexedDB, strips it from the address bar, and offers itself for install.

The same information is on disk as `pairing.json` in the data directory, and can
be pasted into the app's pairing dialog instead:

```json
{
  "host": "vps.example.com",
  "port": 4433,
  "path": "/skyhook",
  "token": "…64 hex chars…",
  "certSha256": "base64 SHA-256 of the server certificate",
  "certExpires": "2026-08-27T00:00:00Z",
  "fallbackUrl": "wss://vps.example.com:4434/skyhook"
}
```

The client pins `certSha256` through WebTransport's `serverCertificateHashes`,
which is stronger than trusting the public CA set and is what lets a personal
server run without a public certificate. **Treat this file as a credential**: it
carries the token.

### Behind a reverse proxy

Terminating TLS somewhere else — nginx, Caddy, Traefik, a Cloudflare tunnel —
changes where the client has to look, and the server cannot infer that. Left
unset, everything it hands the client names its own listeners: the link points
at port 4434, the fragment sends the transport to port 4433, the pairing file
pins a certificate the browser never sees, and `connect-src` allows origins the
browser cannot reach. Each one fails on its own; together they look like
"pairing is broken".

Tell it where it is answered:

```json
{
  "publicUrl": "https://skyhook.example.com",
  "behindProxy": true
}
```

or `SKYHOOK_PUBLIC_URL` and `SKYHOOK_BEHIND_PROXY=1`, which is what
`deploy/docker-compose.proxy.yml` sets. `deploy/nginx.example.conf` and
`deploy/Caddyfile.example` are working proxy configurations for it.

`publicUrl` is the origin the browser uses. Every address the server hands out
is then built from it: the pairing link, `pairing.json`, and the content
security policy. It must be an origin, not a sub-path — the client app owns the
root of its origin, because its service worker has to — and the server refuses
to start rather than half-work if it is given one.

`behindProxy` says TLS terminates in front. The fallback listener then serves
plain HTTP, so no proxy needs `proxy_ssl_verify off` to talk to it, and the
QUIC listener is not started at all. It requires `publicUrl`, and it requires
`webSocketFallback`: the socket is the only transport left.

Two things are genuinely lost, and no configuration recovers them:

* **WebTransport.** It rides HTTP/3 over UDP, and no HTTP reverse proxy
  forwards it upstream. Everything falls back to the WebSocket — same wire
  format, one TCP connection — which gives up stream independence (a slow image
  can head-of-line block a DOM diff) and 0-RTT resume after an outage. On a
  link that drops every few minutes you will feel it. Reach the server
  directly, on its own hostname and ports, if that matters more than sharing a
  port with everything else on the box.
* **The certificate pin.** The certificate the browser validates is the
  proxy's, so `pairing.json` carries no `certSha256` and the client uses
  ordinary TLS trust. The proxy therefore needs a real certificate; a
  self-signed one there stops the PWA from installing at all. In exchange the
  fortnightly rotation dance below stops applying.

The pairing link is unchanged in kind — open it once, on the plane-side
machine — but it now points at the proxy and carries `preferFallback=1` instead
of a pin:

```
https://skyhook.example.com/#token=…&host=skyhook.example.com&port=443&path=/skyhook&preferFallback=1&fallback=wss%3A%2F%2Fskyhook.example.com%2Fskyhook
```

To print it again later, from the pairing file rather than the startup log:

```sh
docker compose -f deploy/docker-compose.proxy.yml exec skyhook \
  skyhookctl pairing -file /data/pairing.json -link
```

Whatever proxy you use, three settings matter:

* **Pass the upgrade through.** The mirror is a WebSocket on `path`
  (`/skyhook`). nginx needs `proxy_http_version 1.1` with the `Upgrade` and
  `Connection` headers; Caddy and Traefik do it by themselves.
* **Do not idle-timeout it.** A mirrored tab is idle for exactly as long as its
  reader is, and nginx's default `proxy_read_timeout` of 60s will cut a
  connection out from under someone who is reading. Plane-side that is
  indistinguishable from the link dropping.
* **Serve one whole origin.** The app registers a service worker at `/` and
  asks for `/sw.js` and `/net.worker.js` by absolute path. A proxy that mounts
  it under a sub-path, or strips `Service-Worker-Allowed`, breaks offline start
  — which is the entire point of the client.

If the app loads but never connects, look in the browser console for a CSP
`connect-src` violation naming a port you did not expect: that is `publicUrl`
disagreeing with where the page was actually opened.

### Certificate rotation

Chromium refuses pinned certificates whose validity exceeds 14 days, so the
self-signed certificate is short-lived by necessity. The server mints a new one
when the old is within a day of expiry, rewrites `pairing.json`, and logs
loudly — but the listener keeps serving the old certificate until it restarts.
Restart the service (a nightly `systemctl restart skyhook` is fine; sessions
rebuild from the profile) and re-pair the client with the new fingerprint.

If you have a real certificate for the host, set `tlsCert` and `tlsKey` and none
of this applies: no pinning, no rotation dance. The same is true behind a
reverse proxy, where the certificate that matters is the proxy's.

## Security posture

The model is **"the VPS is me."** All cookies, passwords and sessions live
landside; passwords you type transit (encrypted) to the VPS and are entered into
the real page there. This is acceptable for a personal deployment and should
never be offered to a third party.

What the implementation does about it:

- **One credential, constant-time compared.** No other authentication surface.
- **Certificate pinning** in both the QUIC and fallback paths.
- **The client has no internet access.** The service worker answers every
  cross-origin request with a 403 instead of fetching it, and the server serves
  the app under a CSP with no `connect-src` beyond its own origin. Egress from
  the app is the one QUIC connection and nothing else.
- **Page JavaScript cannot run plane-side, and the browser enforces it.**
  Mirrored documents live in an iframe carrying `sandbox="allow-same-origin"`
  with no `allow-scripts`, so script execution inside the mirror is impossible
  by construction rather than by policy. The frame also has no `allow-forms`,
  `allow-popups` or `allow-top-navigation`, so a mirrored page cannot navigate
  or submit anything on its own.
- **Password fields are never mirrored back.** The agent sends live field values
  so a resync restores what you typed, but not for `type="password"`, not for
  fields whose `autocomplete` says `current-password`, `new-password`,
  `one-time-code`, `cc-number` or `cc-csc`, and not for anything carrying
  `data-sky-mask`. Those characters are already plane-side; echoing them would
  only add copies in the replay ring and in every resync.
- **The client archive is encrypted at rest** with a non-extractable AES-GCM
  WebCrypto key held in IndexedDB; it contains real message content. This is
  weaker than an OS keychain — anything that can run script on the app's origin
  can ask the key to decrypt — but it does mean the bytes on disk are useless
  on their own.
- **The landside agent runs in an isolated world**, so page script cannot see or
  tamper with the mirror.
- **Kill switch**: `skyhookctl kill -yes` (or the client's kill command) tears
  down every session and wipes the browser profile. Use it if the laptop is lost.

Accepted residual risk: VPS compromise is full account compromise. Put the data
directory on a LUKS volume, keep the firewall to QUIC + SSH, and do first logins
on the ground where a captcha is a minor annoyance rather than a trip-ruining one.

## Operating notes

**Sessions outlive connections.** A session survives 12 hours without a client
by default. Its tabs keep running, websockets stay connected, chats keep
accumulating. That is the feature that makes a reconnect after an outage feel
instant.

**Restarts cost a resnapshot.** systemd restarts rebuild sessions from the
profile on disk; the client re-snapshots its tabs. Logins survive; open page
state does not.

**Bot detection.** Set `SKYHOOK_HEADFUL=1` (Docker) or `headless: false` plus a
`DISPLAY` (systemd + Xvfb). Persisting a real profile is the other half of the
mitigation. If a site still fights it, use the mirror for reading and do the
awkward part on the ground.

**Adapter selectors.** `adapterConfig` points at a JSON file of per-adapter
overrides, so a Chat redesign is a config edit rather than a rebuild:

```json
{ "googlechat": { "messageItem": "[data-new-selector]", "pollMs": 8000 } }
```

## Publishing the image

`.github/workflows/docker.yml` builds `deploy/Dockerfile` on every pull request
and publishes it on every push to `main` and every `v*` tag.

A pull request is a **dry run**: it builds for amd64, loads the image, runs
`skyhookd -init` inside it, and checks the client build is actually present.
Nothing is pushed. A Dockerfile only breaks when someone builds it, and finding
that out on the pull request is the whole point.

A push publishes multi-arch (amd64 + arm64) to GHCR always, and to Docker Hub
when these are set:

| Secret or variable | Purpose |
|---|---|
| `DOCKERHUB_USERNAME` (secret) | Docker Hub account |
| `DOCKERHUB_TOKEN` (secret) | Access token, not the password |
| `DOCKERHUB_REPO` (variable) | Optional; defaults to `<owner>/<repo>` |

With the secrets unset the job still runs and publishes to GHCR, and says in
the log that it skipped Docker Hub. A fork should not fail because it has no
credentials to a registry it does not own.

## Deploying from CI

`.github/workflows/deploy.yml` is a manual (`workflow_dispatch`) deploy over
SSH. It needs these repository secrets:

| Secret | Purpose |
|---|---|
| `SKYHOOK_SSH_HOST` | VPS hostname |
| `SKYHOOK_SSH_USER` | SSH user with docker access |
| `SKYHOOK_SSH_KEY` | Private key (ed25519) |
| `SKYHOOK_SSH_KNOWN_HOSTS` | Optional; keyscanned if absent |
| `SKYHOOK_REMOTE_DIR` | Optional; defaults to `/opt/skyhook` |

Deployment is manual on purpose: restarting the server drops every open tab's
page state, and that should be your decision rather than a merge's.

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| Client cannot connect over QUIC | UDP blocked. The client falls back to WebSocket automatically; check the HUD, which shows which transport is live. |
| The app shows "The client has not been built" | `webRoot` is unset or wrong. Build `client/` and point at `client/dist`. |
| The app loads but never installs | Chrome requires a secure origin: a real certificate, or `localhost`. A pinned self-signed certificate is enough for WebTransport but not for an install prompt. |
| Stale UI after a deploy | The service worker serves its cache first and refreshes behind you. Reload twice, or use the browser's "Update on reload". |
| `unauthorized` on connect | Token mismatch: re-read `pairing.json`. |
| Behind a proxy: the app loads, the HUD stays offline | The pairing names the container's ports, not the proxy's. Set `publicUrl` (and `behindProxy`), restart, and re-pair with the new link — the old one is stored in IndexedDB until it is replaced. |
| Behind a proxy: connects, then drops after ~60s of idle | The proxy's idle timeout, not the link. Raise `proxy_read_timeout` (nginx); see [behind a reverse proxy](#behind-a-reverse-proxy). |
| Behind a proxy: console shows a `connect-src` violation | `publicUrl` is not the origin the page was opened on. They have to match exactly, port included. |
| Connects, then drops immediately | Pinned certificate expired or rotated. Re-pair. |
| Blank mirror, server logs "isolated world setup failed" | The page navigated during setup; a resync fixes it. Persistent failures mean Chromium is wedged — restart the service. |
| Images never arrive | Check `avifenc`/`cwebp` are installed, and look for "image transcode failed" in the logs. The transcoder degrades to JPEG/PNG, so persistent silence means the *fetch* failed (authenticated asset, expired cookie). |
| Mirror looks stale | Look for "mirror divergence" in the logs: the integrity check found a hash mismatch and resynced. If it repeats on one site, that is a protocol bug worth a fixture. |
| High landside CPU | Image transcoding. Lower `imageWorkers`, or raise `imageQuality` (higher quality is *cheaper* for the fallback encoders). |
| `no space left` in the container | The image cache. Lower `imageCacheBytes`; it evicts LRU but only up to its own limit. |

## Measuring the link

The HUD in the client shows transport, RTT, queue depth and bytes. For a
repeatable measurement, use the probe:

```sh
skyhookctl probe -pairing ~/.skyhook/pairing.json \
  -url https://news.ycombinator.com/ -expect "comments" -json
```

It reports first-useful-paint, node and rule counts, and bytes on the wire —
the numbers the design's goals are written in.
