package server

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

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
		"font-src 'self'",
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

// resolveWebRoot picks the directory the app is served from, preferring an
// explicit setting and falling back to the data directory.
func resolveWebRoot(cfg config.Config) string {
	candidates := []string{cfg.WebRoot, filepath.Join(cfg.DataDir, "webapp")}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if st, err := os.Stat(filepath.Join(c, "index.html")); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}
