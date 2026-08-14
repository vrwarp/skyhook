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
`SKYHOOK_HEADLESS`, `SKYHOOK_ADAPTERS`, `SKYHOOK_LOG_LEVEL`.

A missing token is generated on first start and written back to the config file
when one was supplied.

## Pairing

The server writes `pairing.json` into its data directory:

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

### Certificate rotation

Chromium refuses pinned certificates whose validity exceeds 14 days, so the
self-signed certificate is short-lived by necessity. The server mints a new one
when the old is within a day of expiry, rewrites `pairing.json`, and logs
loudly — but the listener keeps serving the old certificate until it restarts.
Restart the service (a nightly `systemctl restart skyhook` is fine; sessions
rebuild from the profile) and re-pair the client with the new fingerprint.

If you have a real certificate for the host, set `tlsCert` and `tlsKey` and none
of this applies: no pinning, no rotation dance.

## Security posture

The model is **"the VPS is me."** All cookies, passwords and sessions live
landside; passwords you type transit (encrypted) to the VPS and are entered into
the real page there. This is acceptable for a personal deployment and should
never be offered to a third party.

What the implementation does about it:

- **One credential, constant-time compared.** No other authentication surface.
- **Certificate pinning** in both the QUIC and fallback paths.
- **The client has no internet access.** Every renderer request that is not
  `skyhook://` is cancelled at the session level, and mirror tabs are sandboxed
  with a CSP of `default-src 'none'; script-src 'none'`. Page JavaScript cannot
  exist plane-side.
- **The client archive is encrypted at rest** with an OS-keychain-wrapped key
  (Electron `safeStorage`); it contains real message content.
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
| `unauthorized` on connect | Token mismatch: re-read `pairing.json`. |
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
