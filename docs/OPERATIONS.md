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

The image ships Chromium, `avifenc`, `cwebp` and Xvfb, and runs Chromium
headful under Xvfb by default, because headless Chromium announces itself to
every site it visits. `SKYHOOK_HEADFUL=0` opts out: lighter, and conspicuous.

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

## Setting it up by answering questions

```sh
scripts/setup.sh          # from a checkout
skyhookd -setup           # or against an installed binary
```

`-setup` asks what this deployment is, writes a configuration file, and — this
is the part worth having — **checks each answer while the person who typed it is
still there**. It connects to the browser you said to attach to, resolves the
name you gave, tries to bind the challenge port, and runs your DNS hook for real
against a throwaway record. Every one of those otherwise fails minutes later, in
somebody else's vocabulary, at a moment when the cause is no longer on screen.

It is a conversation and not a form: how many questions you get depends on the
answers, and each choice says what it costs before you make it. Nothing is
written until it has shown you the whole plan, so an abandoned run leaves the
disk exactly as it was, and an existing config is moved to `.bak` rather than
overwritten. At the end it offers to do the work of `-init` — create the data
directory, settle the token, get the certificate — because that is the step that
proves the answers were right.

It needs a terminal. For an unattended install, write the config however you
like and use `skyhookd -init`, which does the same work without the questions.

Re-run it whenever the deployment changes: it is the quickest way to move
between the four shapes below, and it will tell you what each one gives up.

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
  "acme": { "enabled": false, "agreeTos": false, "email": "", "domains": [], "dns": {} },
  "headless": false,
  "lang": "en-US",
  "sessionTtl": "12h",
  "compression": true,
  "maxTabs": 8,
  "imageQuality": 40,
  "imageMaxBytes": 1048576,
  "imageCacheBytes": 536870912,
  "canvasStreamEvery": "0",
  "homeUrl": "",
  "blockUrls": { "reddit.com": [] },
  "adapters": ["googlechat"],
  "adapterConfig": "/var/lib/skyhook/adapters.json",
  "logLevel": "info",
  "captureKeep": 20,
  "captureScreenshots": true,
  "captureText": false,
  "captureOnDivergence": false,
  "captureInterval": "5m",
  "captureMaxBytes": 67108864,
  "captureClientBytes": 4194304,
  "journalBytes": 2097152,
  "logLines": 2000
}
```

Environment overrides: `SKYHOOK_LISTEN`, `SKYHOOK_FALLBACK_LISTEN`,
`SKYHOOK_DATA_DIR`, `SKYHOOK_TOKEN`, `SKYHOOK_CHROME`, `SKYHOOK_CHROME_ATTACH`,
`SKYHOOK_HOSTS`, `SKYHOOK_HEADLESS`, `SKYHOOK_LANG`, `SKYHOOK_ADAPTERS`,
`SKYHOOK_WEB_ROOT`,
`SKYHOOK_INSECURE_LOOPBACK`, `SKYHOOK_CHROME_ARGS`, `SKYHOOK_LOG_LEVEL`,
`SKYHOOK_PUBLIC_URL`, `SKYHOOK_BEHIND_PROXY`, `SKYHOOK_CAPTURE_KEEP`,
`SKYHOOK_CAPTURE_TEXT`, `SKYHOOK_CAPTURE_ON_DIVERGENCE`, `SKYHOOK_ACME`,
`SKYHOOK_ACME_DOMAINS`, `SKYHOOK_ACME_EMAIL`, `SKYHOOK_ACME_AGREE_TOS`,
`SKYHOOK_ACME_DIRECTORY`, `SKYHOOK_ACME_CHALLENGE`,
`SKYHOOK_ACME_HTTP_LISTEN`, `SKYHOOK_ACME_DNS_COMMAND`,
`SKYHOOK_ACME_DNS_RESOLVERS`.

`acme` has the server get its own certificate and keep it renewed — see
[a certificate of its own](#a-certificate-of-its-own).

The `capture*` and `journal*` settings are the diagnostic bundles — see
[diagnosing the mirror](#diagnosing-the-mirror).

`imageMaxBytes` is the most one picture may cost on the link, and it is a cap
with a fallback rather than a refusal: a picture whose honest encode comes out
larger is re-encoded to WebP at whatever quality — and then whatever size — gets
it under the number. The default is 1 MB, which is thirty-two seconds at
250 kbps, and it is deliberately generous: nearly every image on a page is
already resized into the box the page lays it out in and never reaches this at
all. Lower it on a link where one hero image is still too much; set it negative
to turn it off and ship whatever the first encode produced. Installing `cwebp`
(Debian/Ubuntu: `apt install webp`) is what makes the re-encode WebP; without it
the server falls back to JPEG, or to PNG for a picture with transparency to
keep, which converges more slowly and sometimes not at all — a picture that is
still over the cap is shipped anyway rather than dropped.

`publicUrl` and `behindProxy` are for deployments where the client does not
reach the server where the server listens — see
[behind a reverse proxy](#behind-a-reverse-proxy).

`chromeArgs` (or `SKYHOOK_CHROME_ARGS`, space separated) is appended to
Chromium's command line. It exists for sandboxing: Chromium isolates itself with
user namespaces, some container runtimes refuse those, and then Chromium does
not start at all. The container image probes for this at startup and falls back
to `--no-sandbox` with a loud log line; set the variable yourself to override
the probe either way.

`webRoot` is the built client (`client/dist`), and is usually not needed. The
server looks in three places, in order: `webRoot`, then `<dataDir>/webapp`, then
`client/dist` in the checkout the running binary came out of. That last one is
why `go run ./cmd/skyhookd` from a working copy serves the app with no
configuration at all — it used to serve nothing, and the fix was a step nobody
could guess. It cannot fire on a real deployment: the container image and the
systemd unit both set `webRoot`, and neither `/usr/local/bin` nor `/` has a
`client/dist` above it.

Set it explicitly when the build lives somewhere else, or copy the build to
`<dataDir>/webapp`. With none of the three present the server serves a page
explaining how to build it, rather than nothing at all.

A missing token is generated on first start and kept in `<dataDir>/token`, so a
restart comes back with the credential its clients already hold. It is written
back to the config file as well when one was supplied. Set `token` (or
`SKYHOOK_TOKEN`) to pin it yourself; that always wins.

Deleting `<dataDir>/token` re-pairs the deployment: the next start generates a
new token and logs a new pairing link, and every client paired with the old one
is refused until it opens that link.

### Driving a browser that is already running

By default the server launches Chromium itself and owns it. Point
`chromeAttach` (or `SKYHOOK_CHROME_ATTACH`) at the DevTools endpoint of a
browser that is already up, and it drives that one instead:

```sh
# the browser, started however you like — this is the important flag
google-chrome --remote-debugging-port=9222 --remote-allow-origins='*'

SKYHOOK_CHROME_ATTACH=http://127.0.0.1:9222 skyhookd
```

The profile is shared, so whatever that browser is logged into is what mirrored
pages see; there is no second profile and no second login. Because it is
somebody's browser and not ours, Skyhook keeps to itself:

- **Its own window.** The first tab opens a new window titled *Skyhook*, and
  every later tab joins it. Nothing is ever added to a window you had open —
  including while you are working in that window, which is when Chromium would
  otherwise put the new tab there.
- **Its own tabs only.** Tabs that were open when it attached are never
  attached to, driven, navigated, closed or listed. Mirrored pages get no
  `window.opener`, so a page cannot reach back at Skyhook's window either.
- **Its own shutdown.** Stopping the server closes the Skyhook window and
  leaves the browser running. It never calls `Browser.close`.

`chrome` and `chromeArgs` describe a browser the server starts, so they are
refused alongside `chromeAttach` rather than silently ignored. Two caveats:
the debugging port is an unauthenticated full-control channel over loopback, so
do not open it to a network; and if the server is killed rather than stopped,
the Skyhook window is left behind for you to close.

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

### A certificate of its own

If the box has a name, the server can get a real certificate for it from Let's
Encrypt and keep it renewed. Two settings and a DNS record:

```json
{
  "hosts": ["skyhook.example.com"],
  "acme": { "enabled": true, "agreeTos": true, "email": "you@example.com" }
}
```

or `SKYHOOK_ACME=1`, `SKYHOOK_ACME_DOMAINS=skyhook.example.com`,
`SKYHOOK_ACME_AGREE_TOS=1`, which is what `deploy/docker-compose.acme.yml` sets:

```sh
SKYHOOK_ACME_DOMAINS=skyhook.example.com SKYHOOK_ACME_EMAIL=you@example.com \
  docker compose -f deploy/docker-compose.acme.yml up -d
```

**This is the arrangement to want.** It is the only one that keeps both halves
of what makes the client work:

* **WebTransport**, because TLS terminates in this process rather than at a
  proxy, and no HTTP proxy forwards HTTP/3 upstream.
* **An installable app**, because the certificate is one Chrome already trusts.
  Chrome will not register a service worker behind a self-signed certificate, so
  the pinned deployment can pair and mirror pages but can never start with no
  network — which is the situation the whole client was written for.

The self-signed path keeps the first and loses the second; a reverse proxy keeps
the second and loses the first. This keeps both, and it also ends the
fortnightly rotation dance below: the certificate is answered per handshake out
of `<dataDir>/acme`, so a renewal is simply what the next handshake gets. No
restart, no re-pairing, no new fingerprint for anybody to copy.

What it costs:

* **A DNS record.** An A or AAAA for each name in `domains`, pointing here — the
  authority checks by connecting to it from the outside. (`dns-01` does not care
  where the names point, but the client still has to reach the server, so in
  practice they point here too.) There is no way to certify an address, and no
  way to certify `localhost`. `domains` defaults to
  `hosts`, and the certified names then *become* `hosts`, so the pairing link,
  `pairing.json` and the app's `connect-src` all name the certificate's names.
* **A reachable challenge port** — unless you use `dns-01`, which needs none.
  Port 80 by default (`http-01`); if the fallback listener is already on 443,
  `tls-alpn-01` is chosen instead and answers on that listener with nothing
  extra bound. See [answering in DNS instead](#answering-in-dns-instead) for
  the third option.
* **Accepting the subscriber agreement.** `agreeTos` has to be set, because
  agreeing to <https://letsencrypt.org/repository/> on somebody's behalf is not
  this program's decision to make. Nothing is requested without it.

The challenge port is the part worth checking twice, because it is the one that
fails for reasons outside this process. Port 80 and port 443 are what the
authority *dials*; they need not be what this process *binds*. The container
publishes `80:8080` and binds 8080, because an unprivileged uid cannot have port
80; a systemd unit can bind it directly with the `AmbientCapabilities` line in
`deploy/skyhook.service`. When the two differ the server says so at startup, and
then it is on you to make the forwarding real.

One thing in the log is expected and not a fault: with `http-01`, the client
library tries `tls-alpn-01` first and falls back when it fails, so a refused
challenge appears once per issuance before the certificate arrives. It costs a
few seconds and is well inside any authority's failure allowance. Putting the
fallback listener on 443 — which selects `tls-alpn-01` by default — avoids it.

#### Answering in DNS instead

`dns-01` proves the name by publishing a TXT record rather than by being
connected to, so it needs **no inbound port at all**. That is the answer for a
machine behind a NAT, on a link that filters 80 and 443, or with both already
spoken for — and the only way to get a wildcard.

Skyhook has no list of DNS providers and will not grow one: every API is
different, and a personal browser has no business carrying a matrix of cloud
SDKs. It runs a command you supply instead.

```json
{
  "hosts": ["skyhook.example.com"],
  "acme": {
    "enabled": true, "agreeTos": true, "email": "you@example.com",
    "challenge": "dns-01",
    "dns": { "command": ["/usr/local/bin/skyhook-dns-hook"] }
  }
}
```

The command is run twice per record, with the same three facts as arguments and
in the environment — use whichever your script finds convenient:

```sh
<command...> present <fqdn> <value>     # SKYHOOK_ACME_ACTION=present
<command...> cleanup <fqdn> <value>     # SKYHOOK_ACME_FQDN=_acme-challenge.skyhook.example.com
                                        # SKYHOOK_ACME_VALUE=<the TXT value>
```

A non-zero exit fails the challenge, and whatever the command printed is quoted
back in the server's error — so print the provider's own message about a bad
token rather than "failed". Anything in `command` after the program itself is
passed before those arguments, so one script can serve several zones by
dispatching on its own first argument.

`deploy/acme-dns-hook.example.sh` is a working hook for Cloudflare. Two things
in it are worth copying whatever your provider is, because both fail in ways
that look like something else:

* **`present` must add, never replace.** A certificate covering a host *and* a
  wildcard over it needs two values at the same record name. A hook that
  overwrites leaves one, and the first challenge then fails in a way
  indistinguishable from slow propagation.
* **`cleanup` must remove only the value it was given**, and succeed when there
  is nothing to remove — it runs after failures too.

Keep the provider credential out of the script and in the environment
(`Environment=` or `EnvironmentFile=` in the unit, `-e` on the container). Scope
it to editing DNS on the one zone; it is a credential for everything that name
resolves to.

Before accepting a challenge, Skyhook waits for the record to actually be
visible. It finds the zone's own nameservers and asks them directly rather than
asking the machine's resolver, because a recursive resolver caches the empty
answer from just before the record was published — and that cached "no" is
exactly what stands between a correctly written record and a challenge that
would now pass. `propagationTimeout` (5m) gives up; `settle` (15s) is held after
the record first appears, since seeing it on one server is not the same as every
server having it. `resolvers` overrides which servers are asked, for a split
horizon or when the delegated nameservers are not the ones actually serving.

Renewal is unattended, so the hook has to keep working without anybody watching
— a rotated API token is the usual way this breaks, months later. `email` is
what gets you the authority's warning before that becomes an outage.

Work it out against staging first if there is any doubt. Its certificates are
refused by every browser, and its rate limits are generous enough to fail
against all afternoon:

```sh
SKYHOOK_ACME_DIRECTORY=staging skyhookd -init
```

`-init` is the short way to find out: it creates the data directory, gets the
certificate and exits, with no browser and no listeners in the way. A challenge
that fails is an error there rather than a warning, so it either works or it
tells you what did not.

Once it is working, the certificate lives in `<dataDir>/acme` along with the
ACME account key, and **that directory has to persist**. Losing it means
registering again and re-issuing from scratch on every start, which is the
shortest route to a rate limit. It is inside the data directory for exactly that
reason: the data directory is the one thing every deployment already keeps.

The client is not given a pin here. `pairing.json` carries no `certSha256`, and
that is deliberate rather than an omission: WebTransport refuses a pin whose
certificate is valid for longer than 14 days, which is every certificate a
public authority issues, so pinning one would fail every QUIC dial and quietly
demote the connection to the socket. Ordinary TLS trust is what a real
certificate is for. The same now applies to `tlsCert`/`tlsKey`, which used to
hand out a pin the browser could not use.

`acme` is refused alongside `behindProxy` (the certificate that matters there is
the proxy's), alongside `tlsCert`/`tlsKey` (two answers to one question), and
alongside `insecureLoopback` (no TLS, and no authority certifies `127.0.0.1`).

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

Set it. Without it the server keeps its own self-signed certificate on the
upstream port, and the proxy has to be told to trust it — `proxy_ssl_verify
off` in nginx, `tls_insecure_skip_verify` in Caddy, and in an appliance UI
(Synology's DSM among them) usually nothing at all, because the setting is not
exposed. `behindProxy` removes the problem rather than working around it: point
the proxy at `http://<host>:4434` and there is no upstream certificate to
verify. It also stops the client being handed a pairing that sends it looking
for a QUIC listener on the far side of a proxy that cannot carry one — every
reconnect paying for a WebTransport handshake that cannot succeed.

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

None of this applies once the certificate is a real one. Turn on
[`acme`](#a-certificate-of-its-own) and the server gets and renews one itself,
with no restart and no re-pairing; set `tlsCert` and `tlsKey` if you already
have one from elsewhere; or stand behind a reverse proxy, where the certificate
that matters is the proxy's. In all three the client uses ordinary TLS trust and
there is nothing to pin.

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
- **Diagnostic bundles hold page content, and are written accordingly.** A
  capture contains whatever was on screen on both sides, so bundles are `0600`
  files in a `0700` directory and the oldest are deleted (`captureKeep`). Typed
  text is reduced to a length and a digest unless `captureText` is on, form
  submissions record field names without values, and the pairing token is never
  in one. Sharing a bundle is still a decision: see
  [diagnosing the mirror](#diagnosing-the-mirror). `captureKeep: 0` disables the
  feature entirely.

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

**Being recognised as a person.** Most of this is now the default rather than
something to switch on: Chromium runs headful under a virtual display, images
are fetched by the browser rather than beside it, the user agent and its client
hints tell one story, the denylist no longer suppresses the telemetry a real
visit produces, and clicks are replayed with the timing and aim the reader's
own pointer had.

Two things are still yours to get right, and they matter more than any of the
above:

- **Where the traffic comes from.** A VPS address is a datacenter address, and
  that is the strongest signal about you that any origin has. Egress through
  your home connection — a WireGuard tunnel or a Tailscale exit node — puts the
  browser back on a network with your history on it.
- **Being logged in, on a profile with age.** The persistent profile is the
  point of the landside browser. Do first logins on the ground, where a captcha
  is an annoyance rather than a trip-ruining one, and remember that
  `skyhookctl kill -yes` wipes the profile: it costs the account's warmth, not
  just its cookies.

If a site still fights it, use the mirror for reading and do the awkward part
on the ground.

**What the browser refuses to fetch.** `blockUrls` is keyed by host, with `"*"`
for the default. The built-in default blocks ad and creative networks, because
their iframes are DOM the mirror would have to ship over the bad link. It does
not block analytics or webfonts: those bytes are paid for landside, where there
is bandwidth to spare, and a browser that renders a page and never reports
anything back is not a shape a real visitor has. Naming a host with an empty
list turns blocking off there entirely:

```json
{ "blockUrls": { "reddit.com": [], "*": ["*://*.doubleclick.net/*"] } }
```

**Following a canvas that animates on its own.** A canvas is photographed
landside when the page loads and after the reader does something, and the
photographs continue until the picture stops changing — so a tile slide or a
map easing to a halt arrives complete without any setting. What that does not
cover is a canvas that animates with nobody touching it: a clock face, a game
loop, a chart that ticks. `canvasStreamEvery` keeps photographing one at a
fixed interval:

```json
{ "canvasStreamEvery": "2s" }
```

Off by default, and worth leaving off unless the page needs it: this is the
only setting here that spends the link on a page nobody is interacting with.
Below about a second the frames will not fit down a bad link anyway — the
follow-up pass skips a round whenever the send queues are already deep, so a
rate the link cannot carry quietly becomes a slower one.

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

## Versions and updates

Both halves ship together, and only one of them can be counted on to be current.
The server is whatever binary is running. The plane-side app is a PWA: it is
served by its own service worker out of a cache it filled on some earlier flight,
which is deliberate — it has to start with no network at all — and the
consequence is that a deploy landside changes nothing in a browser that already
holds a copy. Reloading does not help, because the reload is answered from that
same cache. A reader can spend weeks on a build the server replaced, with no
symptom except bugs that were fixed a fortnight ago.

So each half says what it is, in the handshake:

- The client's **build id** is a hash of the shell files, computed by
  `client/esbuild.mjs`, compiled into the app's own bytes, used as the service
  worker's cache name, and written to `dist/version.json`. Nothing has to be
  bumped by hand — the failure mode of a hand-maintained version is that it
  stays at `v1` through every deploy.
- The **server** reads `version.json` under its web root and names that build in
  every `Welcome`, re-reading the file when a deploy replaces it. It is
  therefore stating what it would hand a browser asking for the app right now,
  which is exactly what the running client cannot find out for itself.

Where they differ the client says so once, in a notice with a way out, and the
shell menu (right-click anywhere, or ⋯ on a phone) changes from *Skyhook
versions…* to *Update Skyhook…*. Both open a dialog showing this app's version
and build, the server's version, the build the server is serving, and the
protocol version. Nothing updates on its own: it is a fresh download of the app
over a link that charges seconds for it, so the reader chooses the moment.

*Update now* re-fetches the service worker, which installs the new shell whole
under a new cache name, takes the page over, and reloads onto it. It needs the
link. If the two halves disagree about the *protocol* version the server refuses
the connection outright — the wire format is the one thing that has to match —
and the same dialog is what the refusal opens, because an update is the only
thing that can fix it.

To find out which build something is without asking the person holding it:

```sh
curl -s https://skyhook.example.com/version.json          # what the server serves
skyhookctl probe -pairing ~/.skyhook/pairing.json -json   # serverVersion, servedClient
```

The server's log says which app it found at startup (`client app served`) and
notes any client that connects on an older one (`client is running an older
build of the app`). A diagnostic bundle records the client's build, the served
build and the server's version in its manifest — which is the first thing to
read when a mirror looks wrong, because the patcher that drew it may not be the
patcher in your tree.

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| Client cannot connect over QUIC | UDP blocked. The client falls back to WebSocket automatically; check the HUD, which shows which transport is live. |
| The app shows "The client has not been built" | `webRoot` is unset or wrong. Build `client/` and point at `client/dist`. |
| The app loads but never installs | Chrome requires a secure origin: a real certificate, or `localhost`. A pinned self-signed certificate is enough for WebTransport but not for an install prompt. Turn on [`acme`](#a-certificate-of-its-own). |
| ACME: the log says `could not get a certificate` | The authority could not reach the challenge port. Check DNS resolves each name here, that port 80 (or 443 for `tls-alpn-01`) reaches this process from the outside, and that a published container port is mapped to what the server bound — the startup log says when those two numbers differ. Reproduce against `SKYHOOK_ACME_DIRECTORY=staging` rather than burning production quota. With no usable ports at all, switch to [`dns-01`](#answering-in-dns-instead). |
| ACME dns-01: `did not appear in DNS within…` | The hook exited 0 and the record is not visible. Usually it wrote to a zone that is not the one serving the name (a delegated subdomain, or a registrar's zone that is no longer authoritative), or the TTL is enormous. Run the hook by hand — `hook present _acme-challenge.<name> testvalue` — then `dig +short TXT _acme-challenge.<name> @<the zone's nameserver>`. Raise `propagationTimeout` for a genuinely slow provider; set `resolvers` if the delegated nameservers are not the ones actually answering. |
| ACME dns-01: a host-plus-wildcard order times out on the record they share | The hook replaced instead of appending. Both names answer at the same `_acme-challenge.<zone>`, and both values have to be there at once. Use an add API (Cloudflare: POST, not PUT). |
| ACME dns-01: worked at setup, failed months later | The provider credential expired or was rotated, and renewal is unattended. Set `acme.email` so the authority warns you before the certificate actually lapses. |
| ACME: worked once, now the authority refuses | Almost always a lost `<dataDir>/acme`: without the account key the server registers and re-issues on every start, until a rate limit stops it. Persist the data directory. Rate limits are per week; staging has its own. |
| ACME: browsers say the certificate is not trusted | `directory` is still `staging`. Clear it, delete `<dataDir>/acme` so the staging certificate is not reused, and restart. |
| Stale UI after a deploy | Expected: the app runs from the service worker's cache and a reload is answered from it. The client compares builds with the server on every connection and offers *Update Skyhook…* in the shell menu; see [versions and updates](#versions-and-updates). |
| `unauthorized` on connect | Token mismatch: re-read `pairing.json`. The client says `unpaired` in the HUD and opens its pairing dialog rather than retrying. |
| HUD alternates between offline and connected every second or two | The server is refusing the token on every attempt. Older builds retried it forever; check the server log for `unauthorized client`, and for a restart just before it — a server that generated a fresh token has un-paired every client. Pair again from the link in the log. |
| HUD flaps once a second, log shows `client connected`/`client disconnected` pairs and a resync of every tab on each one | Two connections trading one session. The log tells you which: the same `session=` on every line, and a `client connected` about 120 ms before each disconnect. Normally that means the app is open twice on the same origin — an installed window beside a browser tab, sharing the stored session — and the copy you bring to the front takes it back; the other now says `taken over` rather than reconnecting. Older builds had no way to say that, and traded the session back and forth indefinitely, resyncing every tab each pass. |
| Server restarts on its own, log ends in a `panic` | Whatever the trace names, and worth reporting. The restart itself is the visible part: sessions are gone and the browser starts cold. |
| Behind a proxy: the app loads, the HUD stays offline | The pairing names the container's ports, not the proxy's. Set `publicUrl` (and `behindProxy`), restart, and re-pair with the new link — the old one is stored in IndexedDB until it is replaced. |
| Behind a proxy: connects, then drops after ~60s of idle | The proxy's idle timeout, not the link. Raise `proxy_read_timeout` (nginx); see [behind a reverse proxy](#behind-a-reverse-proxy). |
| Behind a proxy: console shows a `connect-src` violation | `publicUrl` is not the origin the page was opened on. They have to match exactly, port included. |
| Connects, then drops immediately | Pinned certificate expired or rotated. Re-pair. |
| HUD says `stale`, and the versions dialog opens by itself | The two halves speak different protocol versions, so the server refused the connection. Press *Update now*; a reload cannot fix it, because the reload is served from the app's own cache. |
| Blank mirror, server logs "isolated world setup failed" | The page navigated during setup; a resync fixes it. Persistent failures mean Chromium is wedged — restart the service. |
| Images never arrive | Check `avifenc`/`cwebp` are installed, and look for "image transcode failed" in the logs. The transcoder degrades to JPEG/PNG, so persistent silence means the *fetch* failed (authenticated asset, expired cookie). |
| One page's images never arrive, and the log says "no decoder for this format" | A format Go cannot read. Those go back to Chromium to be decoded ("image decoded by the landside browser" at debug level), so a *persistent* failure means the tab had already closed, or the source is over the 4 MB this path will carry. |
| Some images are blank, and the log says "image abandoned" | The server gave up on those keys and told the client so; the elements show their alt text rather than waiting. The line names the URL and the underlying error — a 403, a 404, a source over the size cap, a queue that was full. A capture lists them under `missingImages` in `planeside/tabs/<id>/state.json`, which is how to tell "given up on" from the `pendingImages` beside it, which are still on their way. |
| Images are blank and the log says "the fetch succeeded and returned no bytes" | The origin answered with an empty body — usually a CDN rule, sometimes an asset that needs a referer the browser did not send. It is a fetch problem, not a codec one. |
| After a restart the server re-fetches every image it had already transcoded | Expected only if the cache directory was cleared or `imageCacheBytes` is smaller than the working set. Entries from a build older than the one that added per-entry descriptions are dropped on first read and re-transcoded once; after that a restart should be quiet. |
| The app answers nothing at all for a minute or more, in every tab | A page that has been accepted and not answered. `Page.navigate` returns when the navigation commits, so a tab loading such a page used to hold the connection's whole read loop — including the close meant to end it. Fixed (IMPLEMENTATION.md §41): tabs have their own inbound queues, and stop and close cancel what is in flight rather than queueing behind it. On an older build the only way out is to reload the app, which drops the connection and abandons the call. |
| One tab loads and every other tab goes quiet | A background tab spending the whole link. The scheduler now rotates between tabs inside each priority class, with the tab on screen first; before that a single FIFO per class meant whichever tab filled it first was served first and completely. `queueDepth` in the HUD is the whole session's backlog, so it is high in both cases — the tell is a tab whose acks stop moving while another's arrive. |
| A tab was closed and the link stays busy | Older builds left the closed tab's queued frames on the wire. A close now discards them and logs `closing a tab took back what it had queued` with the frames and bytes it dropped. Bytes already handed to the socket cannot be recalled: one message, up to a few hundred kB, still has to drain. |
| A tab opens, stays blank, and the log repeats "the client never reached the frame it was checked against" for it | Its first snapshot never arrived. A snapshot is frame 0 and the plane side finds a missing frame only when a later one does not fit, so nothing plane-side will ever ask for it; the server's own check repairs it on the second sweep (a minute) by re-snapshotting. If it repeats on every tab, the emit path is dropping frames for tabs it thinks are gone — see IMPLEMENTATION.md §41. |
| Log says "a tab is not keeping up with what the reader is doing" | That tab has 64 pieces of inbound work queued — clicks, keystrokes, resyncs — and its browser side is not draining them. Usually a wedged renderer. Stop and close both skip the queue, so the reader can still get out of it; if it repeats on every page, restart the service. |
| Mirror looks stale | Look for "mirror divergence" in the logs: the integrity check found a hash mismatch and resynced. To find out *why* next time, set `captureOnDivergence: true` and leave it running — or take one by hand from the client while it still looks wrong. See [diagnosing the mirror](#diagnosing-the-mirror). |
| High landside CPU, in the *server* process | Image transcoding. Lower `imageWorkers`, or raise `imageQuality` (higher quality is *cheaper* for the fallback encoders). |
| High landside CPU, in a *Chromium renderer*, on a page nobody is touching | The used-CSS filter re-tests a stylesheet after every batch of DOM changes, so a page that mutates on a timer pays for its bundle over and over. It is bounded now (IMPLEMENTATION.md §35), but a site with an enormous sheet and a busy feed is still the shape that costs the most; `__skyhook.diag()` in a capture reports `cssSeen` and `cssRejected` for the last pass. |
| `no space left` in the container | The image cache. Lower `imageCacheBytes`; it evicts LRU but only up to its own limit. |

## Diagnosing the mirror

The split renderer has an awkward failure mode: both halves look fine on their
own. Landside, Chromium rendered the page and the agent serialised it;
plane-side, the patcher applied every frame it was given and reports no error.
What went wrong is only visible in the gap between them, and by the time anybody
looks, the tab has moved on.

A **capture** freezes both halves at one instant and writes them to a zip in
`<dataDir>/captures`. It lives landside because that is the half with a disk, a
clock and somewhere to put things — the plane-side device may be a phone on a
seat-back wifi, and asking it to hold a file is asking for the file to be lost.

### Taking one

- **The reader**, from the client: right-click → *Report a rendering problem…*,
  or **Ctrl/⌘+Shift+D**. It asks what looked wrong; that note is the one thing
  in a bundle no amount of instrumentation can reconstruct.
- **The server**, by itself, when the integrity check finds the two halves
  holding different documents — but **only with `captureOnDivergence: true`**,
  which is off by default. That moment is the one worth having and it is over
  before anybody can ask for it by hand, which is the argument for turning it
  on; it also writes whatever page was on screen to disk with nobody present to
  decide that, which is the argument for it being a decision. Turn it on while
  chasing a mirror bug. Rate-limited to one per `captureInterval` when it is on,
  because a page that diverges once usually diverges every thirty seconds.

  The server says so either way: a divergence with captures off logs
  `no capture taken: set captureOnDivergence to bundle both halves the next time
  this happens`, next to the `mirror divergence` line itself.
- **From a terminal**, which is also the way to reproduce one in CI:

  ```sh
  skyhookctl capture -pairing ~/.skyhook/pairing.json \
    -url https://news.ycombinator.com/ -note "the comment tree renders empty"
  ```

  `skyhookctl` keeps the same DOM replica the real patcher builds, so a
  divergence it can reproduce is a divergence in the *frames*, not in a browser.

### What is in one

```
manifest.json                    what, when, why, which halves are present, and
                                 which build each of them was
NOTES.txt                        what is missing from this bundle, and why
server.log                       the server's last few thousand lines, at debug
session/session.json             session, viewport, link stats
session/events.json              the timeline: navigations, input, resyncs, divergences
landside/browser.json            the Chromium behind it
landside/tabs/<id>/page.html     the real document, as Chromium has it
landside/tabs/<id>/screenshot.webp
landside/tabs/<id>/screenshot.json  what that picture covers: page or viewport, at what scale
landside/tabs/<id>/agent.json    what the injected agent believes about itself
landside/tabs/<id>/fingerprint.json
landside/tabs/<id>/css-rejected.txt the selectors the used-CSS filter turned down
landside/tabs/<id>/frames/       the wire frames actually sent, plus an index
landside/tabs/<id>/expected.html the client's document, replayed from those frames
planeside/client.json            device, build, shell state
planeside/worker.json            what the client acknowledged, and its link
planeside/client.log             the client's own log, including uncaught errors
planeside/tabs/<id>/mirror.html  the document the reader was actually looking at
planeside/tabs/<id>/screenshot.webp
planeside/tabs/<id>/screenshot.json  what *that* picture covers, which is not the same region
planeside/tabs/<id>/fingerprint.json
planeside/tabs/<id>/state.json
```

### Reading one

Start with `NOTES.txt`. It lists what could not be gathered and why — a bundle
that silently omits things is worse than one that admits it.

Then the three-way split, which is what makes the bug locatable:

| Compare | What a difference means |
|---|---|
| `landside/…/page.html` vs `landside/…/expected.html` | The **agent** dropped or mangled something on its way from the real DOM into frames. |
| `landside/…/expected.html` vs `planeside/…/mirror.html` | The **patcher** did not apply what it was sent — or did not receive it. `frames/index.json` says which frames existed and when. |
| the two `screenshot.webp` files | Both documents agree and they still look different: CSS. Used-CSS extraction missed a rule, or a substituted element lost the selector that sized it. |

**Read each `screenshot.json` before comparing the pictures.** They are not
taken the same way and often do not cover the same thing: the landside one is
the whole scrollable page, or — past `MaxShotHeight` — only the viewport, while
the plane-side one is the top of the document up to its own limit, at its own
scale. Two pictures of one tab over two different regions look exactly like a
rendering bug.

When both documents agree and the styling does not, `css-rejected.txt` is the
next file. It lists the selectors the used-CSS filter found nothing for on its
last pass — the rules that were deliberately *not* sent. A rule you expected to
see is either in there (the filter judged it unused, and the question is why) or
it is not (nothing on the page ever offered it, and `agent.json`'s
`blockedSheets` says whether a stylesheet could not be read at all).

`state.json` on each side carries the document hash, and the landside one also
carries `expectedHash` — the hash of the replay. When all three agree, the DOM
is not the problem. When `clientHash` differs, diff the two `fingerprint.json`
files: they list exactly the `(id, kind, value, flags)` quadruples the hash is
computed over, so a mismatch becomes a list of the specific nodes responsible —
and the flags say which of those nodes host a shadow root, are editable, or
stand in for an image or a canvas.

`hashesAgree` is only present when the client had acknowledged the newest frame:
its hash and the live page's describe the same document only then. When it is
behind, the bundle says `hashesComparable: false` rather than claiming a
disagreement between two different instants.

`session/events.json` is the reproduction steps: what the reader clicked and
typed, in order, with each resync and divergence in place.

### What a bundle costs, and what it will not contain

Everything is bounded. `captureMaxBytes` caps a bundle; `captureKeep` bounds how
many survive; `journalBytes` bounds the per-tab record of frames already sent.
`captureClientBytes` is the only one the reader pays for directly — it caps what
crosses the link upward, and the client gathers cheapest-and-most-valuable
first, so a capture cut short by an outage is missing its screenshot rather than
its DOM.

**Typed text is redacted by default.** Input is recorded either way — the
keystrokes are the reproduction steps — but as a length and a short digest, so a
bundle can be handed to somebody without handing them a password. Form
submissions record field *names* only. Set `captureText: true` to keep the
contents, and treat the bundles accordingly.

A bundle still contains the mirrored page: whatever was on screen, including
anything you were logged into. Bundles are files on your server with `0600`
permissions in a `0700` directory; sharing one is a decision, not a default.

Turn the whole thing off with `captureKeep: 0`. That also stops the frame
journals, which are the only cost captures impose when nobody is taking one.

## Measuring the link

The HUD in the client shows transport, RTT, queue depth and bytes. For a
repeatable measurement, use the probe:

```sh
skyhookctl probe -pairing ~/.skyhook/pairing.json \
  -url https://news.ycombinator.com/ -expect "comments" -json
```

It reports first-useful-paint, node and rule counts, and bytes on the wire —
the numbers the design's goals are written in.
