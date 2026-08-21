package server

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/vrwarp/skyhook/internal/appver"
	"github.com/vrwarp/skyhook/internal/config"
)

// webapp serves the plane-side client: a Chrome-targeted PWA that the browser
// installs and then runs from its own cache.
//
// Serving it from the same process as the mirror is deliberate. It gives the
// app one origin to talk to, lets the content security policy be as narrow as
// `connect-src` to exactly this server, and means the pairing link and the
// thing it pairs with can never drift apart.
type webapp struct {
	root string
	log  *slog.Logger
	cfg  config.Config
}

// The client makes exactly one network connection — the WebTransport session —
// and everything else it needs is local. The policy says so.
func (w *webapp) csp() string {
	connect := []string{"connect-src", "'self'"}
	if ep, ok := w.cfg.Public(); ok {
		// Behind a proxy the only address the browser can use is the public
		// one, and the only transport that survives a proxy is the socket.
		connect = append(connect, ep.SocketOrigin())
	} else {
		host := firstHost(w.cfg.Hosts)
		wt := fmt.Sprintf("https://%s:%d", host, portOf(w.cfg.Listen))
		sockScheme := "wss"
		if w.cfg.InsecureLoopback {
			// No QUIC listener exists in loopback mode, and the socket is plain.
			wt = ""
			sockScheme = "ws"
		}
		if wt != "" {
			connect = append(connect, wt)
		}
		if w.cfg.WebSocketFallback {
			connect = append(connect,
				fmt.Sprintf("%s://%s:%d", sockScheme, host, portOf(w.cfg.FallbackListen)))
		}
	}
	return strings.Join([]string{
		"default-src 'none'",
		"script-src 'self'",
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' blob: data:",
		// blob: for the same reason img-src has it: the client rebuilds a
		// page's @font-face sources from bytes it fetched over the protocol
		// (host.ts resolves src to object URLs), and font-src 'self' alone
		// silently refused every one of them at render (P-115) — the
		// stylesheet loaded, the face registered, and the glyphs drew in a
		// substitute while the console counted violations.
		//
		// data: for the half of that P-115 missed. A page may inline a face in
		// its own stylesheet, and an icon font is exactly the size that invites
		// it: Google Chat ships the subset that draws its toolbar as a
		// data:font/ttf URI, and nothing this side rewrites it, so it arrived
		// as itself and was refused as itself. The failure is silent in the
		// same way — the ligature fires, the glyph draws nothing, and the
		// reader sees a gap where an icon goes (P-133).
		"font-src 'self' blob: data:",
		"worker-src 'self'",
		"manifest-src 'self'",
		// The mirror frames are same-origin about:blank documents.
		"frame-src 'self'",
		strings.Join(connect, " "),
		"form-action 'none'",
		"base-uri 'none'",
		"frame-ancestors 'none'",
	}, "; ")
}

// NewWebApp builds the client-app handler. The server mounts it on the TLS
// listener; the end-to-end tests mount it on a plain loopback listener, where
// Chrome still treats the origin as secure.
func NewWebApp(cfg config.Config, root string, log *slog.Logger) http.Handler {
	return &webapp{root: root, cfg: cfg, log: log}
}

func (w *webapp) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	clean := path(r.URL.Path)
	if clean == "" {
		clean = "index.html"
	}

	if w.root == "" {
		w.serveStub(rw)
		return
	}
	file := filepath.Join(w.root, clean)
	if !strings.HasPrefix(file, filepath.Clean(w.root)+string(os.PathSeparator)) &&
		file != filepath.Clean(w.root) {
		http.Error(rw, "forbidden", http.StatusForbidden)
		return
	}
	data, err := os.ReadFile(file) //nolint:gosec // path is cleaned and rooted above
	if err != nil {
		// A PWA owns its own routing; unknown paths fall back to the shell.
		data, err = os.ReadFile(filepath.Join(w.root, "index.html")) //nolint:gosec // fixed path
		if err != nil {
			w.serveStub(rw)
			return
		}
		clean = "index.html"
	}

	w.headers(rw, clean)
	_, _ = rw.Write(data)
}

func (w *webapp) headers(rw http.ResponseWriter, name string) {
	rw.Header().Set("Content-Type", contentTypeOf(name))
	rw.Header().Set("Content-Security-Policy", w.csp())
	rw.Header().Set("X-Content-Type-Options", "nosniff")
	rw.Header().Set("Referrer-Policy", "no-referrer")
	rw.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	switch {
	case name == "sw.js":
		// Without this the service worker may only control its own directory,
		// and it has to control the whole origin to deny cross-origin fetches.
		rw.Header().Set("Service-Worker-Allowed", "/")
		rw.Header().Set("Cache-Control", "no-cache")
	case strings.HasSuffix(name, ".html") || strings.HasSuffix(name, ".webmanifest"):
		rw.Header().Set("Cache-Control", "no-cache")
	case name == appver.StampFile:
		// The one file whose whole purpose is to say what is being served right
		// now. Cached for a week — which is where it landed by default, being
		// neither markup nor the worker — it would answer with the build that
		// was current when somebody last asked, which is the single wrong
		// answer to the only question it is ever asked.
		rw.Header().Set("Cache-Control", "no-store")
	default:
		// The service worker revalidates its own precache, so assets can be
		// cached hard: a cold start in flight must not wait on the network.
		rw.Header().Set("Cache-Control", "public, max-age=604800")
	}
}

// serveStub explains itself when the client has not been built. Silently
// serving nothing would look like a broken server.
func (w *webapp) serveStub(rw http.ResponseWriter) {
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.Header().Set("Cache-Control", "no-store")
	rw.WriteHeader(http.StatusServiceUnavailable)
	_, _ = rw.Write([]byte(`<!DOCTYPE html><html><head><meta charset="utf-8">
<title>Skyhook: client not built</title>
<style>body{font:15px/1.6 system-ui,sans-serif;max-width:44rem;margin:4rem auto;padding:0 1rem;
background:#111827;color:#e5e7eb}code{background:#1f2937;padding:2px 5px;border-radius:4px}</style>
</head><body>
<h1>The client has not been built</h1>
<p>The server is running, but there is no plane-side app to serve. Build it and
point <code>webRoot</code> at the output:</p>
<pre><code>cd client &amp;&amp; npm ci &amp;&amp; npm run build</code></pre>
<p>Then set <code>"webRoot": "/path/to/client/dist"</code> in the server config,
or copy the build into <code>&lt;dataDir&gt;/webapp</code>, and restart.</p>
</body></html>`))
}

// PairingLink builds the one-time link that hands the client its credential.
// The token rides in the fragment, which browsers never send to a server.
//
// It is the pairing file rendered as a URL, deliberately: the link an operator
// clicks and the file they can paste have to describe the same server.
func PairingLink(cfg config.Config, certHash string) string {
	return cfg.PairingFor(certHash, "").Link()
}

func path(p string) string {
	p = strings.TrimPrefix(p, "/")
	p = filepath.Clean("/" + p)
	return strings.TrimPrefix(p, "/")
}

func contentTypeOf(name string) string {
	switch {
	case strings.HasSuffix(name, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(name, ".js"):
		return "text/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".webmanifest"):
		return "application/manifest+json"
	case strings.HasSuffix(name, ".json"):
		return "application/json"
	case strings.HasSuffix(name, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(name, ".png"):
		return "image/png"
	case strings.HasSuffix(name, ".map"):
		return "application/json"
	}
	return "application/octet-stream"
}

// resolveWebRoot picks the directory the app is served from: an explicit
// setting, then the data directory, then the build sitting in the checkout this
// binary came out of.
//
// That last one is why running from source used to need a step nobody could
// guess. `go run ./cmd/skyhookd` in a repository with a built client served
// nothing at all, and the fix — copy or symlink client/dist into
// <dataDir>/webapp, or set webRoot — is only obvious to somebody who has read
// this function. A build sitting twenty metres away in the same checkout is
// what the operator meant, so it is found.
//
// It cannot fire anywhere it should not. A container and a systemd install both
// set webRoot, which wins; and neither /usr/local/bin nor / has a client/dist
// above it to find.
func resolveWebRoot(cfg config.Config) string {
	candidates := []string{cfg.WebRoot, filepath.Join(cfg.DataDir, "webapp")}
	if repo := RepoClientDist(); repo != "" {
		candidates = append(candidates, repo)
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if isWebRoot(c) {
			return c
		}
	}
	return ""
}

// RepoClientDist finds client/dist in the checkout this process is running
// from, by looking up from the working directory and from the binary. It
// returns "" when there is no such thing, which is every deployment that is not
// somebody's working copy.
func RepoClientDist() string {
	var starts []string
	if wd, err := os.Getwd(); err == nil {
		starts = append(starts, wd)
	}
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		starts = append(starts, filepath.Dir(exe))
	}
	for _, start := range starts {
		if found := repoDistFrom(start); found != "" {
			return found
		}
	}
	return ""
}

// repoDistFrom walks up from one directory looking for the module this binary
// belongs to, and the client build beside it.
//
// Four levels covers `go run` from a subdirectory and ./bin/skyhookd from the
// root, and stops well short of wandering into a home directory that happens to
// contain an unrelated client/dist. The go.mod is what makes it a checkout
// rather than a coincidence.
func repoDistFrom(start string) string {
	dir := start
	for i := 0; i < 4; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			cand := filepath.Join(dir, "client", "dist")
			if isWebRoot(cand) {
				return cand
			}
			return ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	return ""
}

func isWebRoot(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, "index.html"))
	return err == nil && !st.IsDir()
}
