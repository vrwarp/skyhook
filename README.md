# Skyhook

A split-architecture browser for links that are slow, lossy and far away —
in-flight wifi, satellite, anywhere the round trip is measured in seconds.

A real Chromium runs **landside** on a VPS with a fast path to the open
internet. It executes all page JavaScript and performs all origin fanout. The
**plane-side** client executes zero page JavaScript and makes zero requests to
the internet: it receives a compressed, incrementally-updated mirror of the
rendered document over a single QUIC connection, and sends back semantic input
events. Every interaction costs at most one round trip over the bad link; most
cost none.

Fidelity is traded for usability, deliberately. See [docs/DESIGN.md](docs/DESIGN.md)
for the full product requirements and technical design, and
[docs/IMPLEMENTATION.md](docs/IMPLEMENTATION.md) for what is built, what is
partial, and where the implementation diverges from the design (with reasons).

```
PLANE SIDE                                LANDSIDE (VPS)
┌────────────────────────┐                ┌──────────────────────────────┐
│ Electron shell         │                │ skyhookd (Go)                │
│  ├ chrome UI + HUD     │   one QUIC /   │  ├ headless Chromium via CDP │
│  ├ mirror tabs         │◄──WebTransport─┤  ├ injected mirror agent     │
│  │   (patcher + echo)  │   connection   │  ├ used-CSS extraction       │
│  ├ network worker      │                │  ├ image transcoder          │
│  └ local store         │                │  ├ session manager (12h TTL) │
└────────────────────────┘                │  └ per-app adapters          │
                                          └──────────────────────────────┘
```

## Quick start

### Landside (the VPS)

```sh
# With Docker
docker compose -f deploy/docker-compose.yml up -d
docker compose -f deploy/docker-compose.yml exec skyhook \
  skyhookctl pairing -file /data/pairing.json

# Or from source
go build -o skyhookd ./cmd/skyhookd
SKYHOOK_DATA_DIR=~/.skyhook ./skyhookd
```

On first run the server generates a pairing token and a short-lived,
self-signed ECDSA certificate, and writes `pairing.json` into its data
directory. That file is what the client needs: host, port, token, and the
certificate fingerprint it will pin.

### Plane side (the laptop)

```sh
cd client
npm ci
npm run build
npm start          # development
npm run dist       # AppImage / dmg / nsis for a real install
```

Paste the contents of `pairing.json` into the client's pairing dialog (or drop
it at the client's data directory as `state.json`'s `pairing` field). The
client connects, opens a tab, and mirrors.

### Diagnostics from anywhere

`skyhookctl` speaks the real protocol, so it is both a debugging tool and the
end-to-end probe CI uses:

```sh
skyhookctl probe -pairing ~/.skyhook/pairing.json \
  -url https://news.ycombinator.com/ -expect "comments" -json
```

## Testing against the link that matters

Every milestone is measured against an emulated 1.2 s RTT / 250 kbps / 2% loss
link, not against a LAN:

```sh
sudo scripts/netem.sh port 45123 1200 250 2   # shape only the Skyhook port
SKYHOOK_E2E=1 SKYHOOK_SLOW_LINK=1 SKYHOOK_TEST_PORT=45123 go test ./test -v
sudo scripts/netem.sh down
```

`sudo scripts/netem.sh outage 60` drops the link entirely for a minute, which
is how the reconnect-and-resync path is exercised.

## Repository layout

| Path | What lives there |
|---|---|
| `cmd/skyhookd` | The landside server binary |
| `cmd/skyhookctl` | Headless client: probe, pairing, kill switch, chat |
| `internal/protocol` | Wire format: CBOR frames, zstd codec, dictionaries |
| `internal/transport` | WebTransport/QUIC and the WebSocket fallback |
| `internal/cdp` | A small Chrome DevTools Protocol client and launcher |
| `internal/mirror` | The injected agent, snapshot/mutation pipeline, input replay |
| `internal/imgproc` | Image transcoding, blurhash, the landside cache |
| `internal/session` | Sessions, replay ring, resync, speculative prefetch |
| `internal/adapter` | Adapter framework and the Google Chat adapter |
| `internal/client` | A headless Go client, used by tests and `skyhookctl` |
| `client/` | The Electron client: patcher, local echo, chrome UI |
| `test/` | End-to-end tests against a real Chromium |
| `deploy/`, `scripts/` | Container, systemd unit, link emulation |

## Testing

```sh
go test ./...                    # unit tests; e2e skips without a browser
SKYHOOK_E2E=1 go test ./test -v  # end-to-end against real Chromium
cd client && npm test            # patcher, echo, codec, conformance
```

The wire format is pinned by cross-language fixtures in `testdata/`: Go writes
them, the TypeScript client's test suite decodes them, and CI fails if the two
ever disagree.

## Security posture

The security model is **"the VPS is me."** All cookies, passwords and sessions
live landside; typed passwords transit (encrypted) to the VPS. This is
acceptable for a personal, self-hosted deployment and unacceptable to offer to
anyone else. The mitigations, the kill switch, and the residual risks are in
[docs/OPERATIONS.md](docs/OPERATIONS.md).

## Licence

MIT. See [LICENSE](LICENSE).
