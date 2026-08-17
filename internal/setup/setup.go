// Package setup is the interactive `skyhookd -setup`: a handful of questions,
// a configuration file, and a check after each answer that the thing just
// described actually works.
//
// It exists because the pieces that have to line up before a first run are not
// discoverable from any one of them. The client is a separate build that the
// server has to be pointed at; a browser you already run needs two flags it was
// not started with; a certificate needs a name, a challenge and a port or a DNS
// hook, and the failures all arrive minutes later in somebody else's
// vocabulary. Every one of those is a question with a right answer this program
// can find or test — so it asks, and then it looks.
//
// Two rules run through it. Nothing is written until the whole plan has been
// shown and agreed, so an abandoned run leaves no trace. And every answer that
// can be checked is checked while the person who typed it is still here.
package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/vrwarp/skyhook/internal/cdp"
	"github.com/vrwarp/skyhook/internal/config"
	"github.com/vrwarp/skyhook/internal/server"
	skyhooksession "github.com/vrwarp/skyhook/internal/session"
	"github.com/vrwarp/skyhook/internal/transport"
)

// Options configures a run.
type Options struct {
	In  io.Reader
	Out io.Writer
	// ConfigPath is where the configuration goes. Empty asks.
	ConfigPath string
	// Log is handed to the checks that want one; the conversation itself goes
	// to Out rather than through a logger.
	Log *slog.Logger
}

// Run asks the questions and writes the answers.
func Run(ctx context.Context, opts Options) error {
	if opts.In == nil {
		opts.In = os.Stdin
	}
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := &session{ui: newUI(opts.In, opts.Out), log: opts.Log, cfg: config.Default()}

	err := s.run(ctx, opts.ConfigPath)
	if errors.Is(err, errCancelled) {
		s.ui.blank()
		s.ui.say("Nothing was written.")
		return nil
	}
	return err
}

type session struct {
	ui  *ui
	log *slog.Logger
	cfg config.Config

	// repo is the checkout this binary came out of, when there is one. It is
	// what makes "build the client for me" possible.
	repo string
	// notes are the things to say at the end: what to run, what still has to be
	// true, what was deliberately left undone.
	notes []string
}

func (s *session) run(ctx context.Context, configPath string) error {
	s.repo = repoRoot()

	s.ui.say("Skyhook setup")
	s.ui.blank()
	s.ui.note(
		"This asks what your deployment looks like, checks each answer, and writes",
		"a configuration file. Nothing is written until you have seen the whole",
		"plan and said yes, so stopping now costs nothing.",
	)

	if err := s.askDataDir(); err != nil {
		return err
	}
	if err := s.askBrowser(ctx); err != nil {
		return err
	}
	if err := s.askReach(ctx); err != nil {
		return err
	}
	if err := s.askClient(ctx); err != nil {
		return err
	}
	path, err := s.confirm(configPath)
	if err != nil {
		return err
	}
	return s.finish(ctx, path)
}

// ---------------------------------------------------------------- data dir

func (s *session) askDataDir() error {
	s.ui.heading("Where Skyhook keeps its things")
	s.ui.note(
		"The browser profile lives here — real cookies, real logins — along with",
		"the pairing file, the certificate and the image cache. Put it somewhere",
		"encrypted, and somewhere that survives a restart.",
		"",
	)
	dir, err := s.ui.ask("Data directory", s.cfg.DataDir)
	if err != nil {
		return err
	}
	abs, err := expand(dir)
	if err != nil {
		return err
	}
	s.cfg.DataDir = abs
	if st, err := os.Stat(abs); err == nil && st.IsDir() {
		s.ui.good("%s exists", abs)
		if _, err := os.Stat(filepath.Join(abs, "token")); err == nil {
			s.ui.note("", "  It already holds a token, so clients paired with this deployment",
				"  stay paired. Delete <dataDir>/token to re-pair everything.")
		}
	} else {
		s.ui.good("%s will be created", abs)
	}
	return nil
}

// ---------------------------------------------------------------- browser

func (s *session) askBrowser(ctx context.Context) error {
	s.ui.heading("The browser that does the actual browsing")
	s.ui.note(
		"Skyhook drives a real Chromium landside. It can start one of its own, or",
		"drive the one you already have open — which shares your logins, and is",
		"why every mirrored page sees what you see.",
		"",
	)

	own := choice{
		label:  "Skyhook starts one",
		detail: "its own profile; logs in to nothing",
	}
	if bin, err := cdp.FindChromium(""); err == nil {
		own.found = "found " + bin
	} else {
		own.found = "no chromium found on this machine yet"
	}
	attach := choice{
		label:  "Attach to yours",
		detail: "shares your profile, so it is already logged in",
		found:  "needs two flags on the browser you start",
	}

	pick, err := s.ui.choose("Which browser?", []choice{own, attach}, 1)
	if err != nil {
		return err
	}
	if pick == 1 {
		if _, err := cdp.FindChromium(""); err != nil {
			s.ui.warn("no chromium binary found; install one, or set \"chrome\" in the config")
			s.notes = append(s.notes,
				"Install Chromium or Chrome — the server will not start without one.")
		}
		return nil
	}
	return s.askAttach(ctx)
}

func (s *session) askAttach(ctx context.Context) error {
	s.ui.blank()
	s.ui.note(
		"Your browser has to be listening for it. Start it — or restart it — with:",
		"",
		"  "+attachCommand(),
		"",
		"Keep that port on loopback: it is unauthenticated control of the whole",
		"browser. Skyhook keeps its tabs in a window of its own and never touches",
		"one it did not open.",
		"",
	)
	for {
		endpoint, err := s.ui.ask("DevTools endpoint", "http://127.0.0.1:9222")
		if err != nil {
			return err
		}
		s.cfg.ChromeAttach = endpoint
		product, err := probeDevTools(ctx, endpoint)
		if err == nil {
			s.ui.good("attached: %s", product)
			return nil
		}
		s.ui.bad("nothing answered there: %v", err)
		retry, err := s.ui.yes("Start the browser with those flags and try again?", true)
		if err != nil {
			return err
		}
		if !retry {
			s.notes = append(s.notes,
				"Start your browser with "+attachCommand()+" before running the server.")
			return nil
		}
	}
}

// attachCommand is the line to paste, named for the platform's own binary.
func attachCommand() string {
	bin := "google-chrome"
	switch runtime.GOOS {
	case "darwin":
		bin = `"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"`
	case "windows":
		bin = "chrome.exe"
	}
	return bin + " --remote-debugging-port=9222 --remote-allow-origins='*'"
}

func probeDevTools(ctx context.Context, endpoint string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(endpoint, "/")+"/json/version", nil)
	if err != nil {
		return "", err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("devtools answered %d", res.StatusCode)
	}
	var v struct {
		Browser string `json:"Browser"`
	}
	if err := json.NewDecoder(res.Body).Decode(&v); err != nil {
		return "", err
	}
	if v.Browser == "" {
		return "", errors.New("that is not a devtools endpoint")
	}
	return v.Browser, nil
}

// ---------------------------------------------------------------- reachability

func (s *session) askReach(ctx context.Context) error {
	s.ui.heading("How the plane side reaches this server")
	s.ui.note(
		"This is the decision everything else follows from, and the one that is",
		"worth reading twice, because two of these give up something you may not",
		"discover you wanted until you are offline.",
		"",
	)
	pick, err := s.ui.choose("Which is this?", []choice{
		{
			label:  "This machine only",
			detail: "plain HTTP on 127.0.0.1",
			found:  "everything works, nothing is reachable from anywhere else",
		},
		{
			label:  "A name, our certificate",
			detail: "Let's Encrypt, obtained and renewed here",
			found:  "the only one that keeps WebTransport AND an installable app",
		},
		{
			label:  "A name, self-signed",
			detail: "the client pins the fingerprint",
			found:  "no install and no offline start: Chrome refuses the worker",
		},
		{
			label:  "Behind a reverse proxy",
			detail: "nginx, Caddy, Traefik, a tunnel",
			found:  "no WebTransport: no HTTP proxy forwards HTTP/3",
		},
	}, 2)
	if err != nil {
		return err
	}
	switch pick {
	case 1:
		return s.reachLoopback()
	case 2:
		return s.reachACME(ctx)
	case 3:
		return s.reachSelfSigned()
	default:
		return s.reachProxy()
	}
}

func (s *session) reachLoopback() error {
	s.cfg.InsecureLoopback = true
	s.cfg.Hosts = []string{"127.0.0.1"}
	s.cfg.Listen = "127.0.0.1:4433"
	s.cfg.FallbackListen = "127.0.0.1:4434"
	s.ui.good("loopback only, no TLS; the server refuses to bind anything else this way")
	s.ui.note("", "  127.0.0.1 is a secure origin whatever the scheme, so the app installs",
		"  and starts offline. Do not run a real deployment like this: the token",
		"  would cross an unencrypted socket.")
	return nil
}

func (s *session) reachSelfSigned() error {
	host, err := s.ui.ask("Hostname or address clients will use", "")
	if err != nil {
		return err
	}
	s.cfg.Hosts = []string{host}
	s.ui.good("a short-lived certificate will be generated and pinned")
	s.notes = append(s.notes,
		"The certificate is pinned, so the app cannot install or start offline. "+
			"Re-run setup and pick a real certificate when you want that.")
	return nil
}

func (s *session) reachProxy() error {
	s.ui.note("", "  The server cannot infer where the proxy answers, and everything it",
		"  hands the client is built from that.", "")
	raw, err := s.ui.ask("Public URL (an origin, no path)", "https://skyhook.example.com")
	if err != nil {
		return err
	}
	ep, perr := config.ParsePublicURL(raw)
	if perr != nil {
		s.ui.bad("%v", perr)
		return s.reachProxy()
	}
	s.cfg.PublicURL = ep.String()
	s.cfg.BehindProxy = true
	s.cfg.WebSocketFallback = true
	s.ui.good("upstream will be plain HTTP on %s, so no proxy needs to trust a "+
		"self-signed certificate", s.cfg.FallbackListen)
	s.notes = append(s.notes,
		"Point the proxy at http://<this host>"+s.cfg.FallbackListen+", pass the "+
			"Upgrade headers through, and raise its idle timeout — a mirrored tab "+
			"is idle for exactly as long as its reader is.")
	return nil
}

// ---------------------------------------------------------------- acme

func (s *session) reachACME(ctx context.Context) error {
	s.cfg.ACME.Enabled = true

	domain, err := s.ui.ask("The name clients will use", "")
	if err != nil {
		return err
	}
	s.cfg.ACME.Domains = []string{strings.ToLower(strings.TrimSpace(domain))}
	s.cfg.Hosts = s.cfg.ACME.Domains
	s.checkResolves(ctx, s.cfg.ACME.Domains[0])

	email, err := s.ui.askOptional("Email for expiry warnings (worth setting: it is the " +
		"only notice you get if renewal quietly stops working)")
	if err != nil {
		return err
	}
	s.cfg.ACME.Email = email

	s.ui.blank()
	agreed, err := s.ui.yes(
		"Accept Let's Encrypt's subscriber agreement (https://letsencrypt.org/repository/)?",
		false)
	if err != nil {
		return err
	}
	if !agreed {
		s.ui.bad("nothing can be requested without it")
		return errCancelled
	}
	s.cfg.ACME.AgreeTOS = true

	if err := s.askPorts(); err != nil {
		return err
	}
	return s.askChallenge(ctx)
}

// checkResolves says whether the name points anywhere. It is advisory: DNS may
// be about to be set up, and dns-01 does not care where the name points — but a
// typo is the single most common reason the rest of this fails.
func (s *session) checkResolves(ctx context.Context, name string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupHost(ctx, name)
	switch {
	case err != nil:
		s.ui.warn("%s does not resolve yet (%v)", name, dnsWhy(err))
	case len(addrs) > 0:
		s.ui.good("%s resolves to %s", name, strings.Join(addrs, ", "))
	}
}

func dnsWhy(err error) string {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.Err != "" {
		return dnsErr.Err
	}
	return err.Error()
}

func (s *session) askPorts() error {
	s.ui.blank()
	s.ui.note(
		"Which ports should the server answer on? 443 gives the tidiest address",
		"and lets the certificate be renewed without a second listener; the high",
		"ports need no privileges.",
		"",
	)
	pick, err := s.ui.choose("Ports", []choice{
		{label: "443", detail: "https://" + s.cfg.Hosts[0], found: bindReport(443)},
		{label: "4433/4434", detail: "https://" + s.cfg.Hosts[0] + ":4434", found: "the default"},
	}, 1)
	if err != nil {
		return err
	}
	if pick == 1 {
		s.cfg.Listen = ":443"
		s.cfg.FallbackListen = ":443"
		s.notes = append(s.notes,
			"Binding 443 needs privileges: AmbientCapabilities=CAP_NET_BIND_SERVICE "+
				"in the systemd unit, or a published container port.")
	}
	return nil
}

func (s *session) askChallenge(ctx context.Context) error {
	s.ui.blank()
	s.ui.note(
		"Let's Encrypt has to satisfy itself that you control the name. The only",
		"real question is which of these this machine can actually do.",
		"",
	)
	http01 := choice{
		label: "http-01", detail: "the authority connects to port 80",
		found: bindReport(80),
	}
	alpn01 := choice{
		label: "tls-alpn-01", detail: "the authority connects to port 443",
		found: bindReport(443),
	}
	dns01 := choice{
		label: "dns-01", detail: "you publish a DNS record",
		found: "no inbound port at all — for a NAT, a filtered link, or busy ports",
	}
	def := 1
	if fallbackOn443(s.cfg) {
		def = 2
	}
	pick, err := s.ui.choose("How should it check?", []choice{http01, alpn01, dns01}, def)
	if err != nil {
		return err
	}
	switch pick {
	case 1:
		s.cfg.ACME.Challenge = config.ChallengeHTTP01
		s.cfg.ACME.HTTPListen = ":80"
		if bindable(80) != nil {
			addr, err := s.ui.ask("Port 80 is not free here; bind the challenge where instead?", ":8080")
			if err != nil {
				return err
			}
			s.cfg.ACME.HTTPListen = addr
			s.notes = append(s.notes,
				"The authority dials port 80. Forward it to "+addr+" — a published "+
					"container port, or a rule on the host.")
		}
	case 2:
		s.cfg.ACME.Challenge = config.ChallengeTLSALPN01
		if !fallbackOn443(s.cfg) {
			s.notes = append(s.notes,
				"The authority dials port 443. Forward it to "+s.cfg.FallbackListen+".")
		}
	default:
		return s.askDNSHook(ctx)
	}
	return nil
}

func (s *session) askDNSHook(ctx context.Context) error {
	s.cfg.ACME.Challenge = config.ChallengeDNS01
	s.ui.blank()
	s.ui.note(
		"Skyhook publishes the record by running a command you supply — every DNS",
		"provider has its own API, and none of them belong inside a browser. It is",
		"called twice per record:",
		"",
		"  <command> present <fqdn> <value>",
		"  <command> cleanup <fqdn> <value>",
		"",
	)
	if s.repo != "" {
		s.ui.note("A working Cloudflare one to copy:",
			"  "+filepath.Join(s.repo, "deploy", "acme-dns-hook.example.sh"), "")
	}
	for {
		path, err := s.ui.ask("Path to the hook", "")
		if err != nil {
			return err
		}
		fields := strings.Fields(path)
		provider, perr := transport.NewExecDNSProvider(fields, 0, s.log)
		if perr != nil {
			s.ui.bad("%v", perr)
			continue
		}
		s.cfg.ACME.DNS.Command = fields
		s.ui.good("%s is executable", provider.Describe())

		test, err := s.ui.yes(
			"Try it now? It publishes a throwaway record, waits for it, and removes it", true)
		if err != nil {
			return err
		}
		if !test {
			s.notes = append(s.notes,
				"The DNS hook was not tested. `skyhookd -setup` can do it, and so can "+
					"a staging run: SKYHOOK_ACME_DIRECTORY=staging skyhookd -init.")
			return nil
		}
		if s.runHookCheck(ctx, provider) {
			return nil
		}
		again, err := s.ui.yes("Fix it and try again?", true)
		if err != nil {
			return err
		}
		if !again {
			s.notes = append(s.notes,
				"The DNS hook did not work. Until it does, no certificate can be issued.")
			return nil
		}
	}
}

// runHookCheck is the most valuable thing in this program. Every way a DNS hook
// can be wrong is invisible until an order is already in flight, and the error
// then arrives from a certificate authority in a vocabulary nobody deploying a
// browser has any reason to know.
func (s *session) runHookCheck(ctx context.Context, provider transport.DNSProvider) bool {
	domain := s.cfg.ACME.Domains[0]
	s.ui.say("  … publishing a test record at _acme-challenge.%s", domain)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	wait := transport.DNSWait{
		Timeout:   2 * time.Minute,
		Settle:    -1,
		Resolvers: s.cfg.ACME.DNS.Resolvers,
	}
	// The check reports itself through the UI below, in whole sentences. Its
	// own logger is discarded so a structured warning does not land in the
	// middle of a conversation saying the same thing twice, once badly.
	err := transport.CheckDNSHook(ctx, provider, wait, domain, quiet())
	if errors.Is(err, transport.ErrDNSUnverifiable) {
		// Nothing was proved either way. Usually the name is wrong; sometimes
		// the zone is real and simply not visible from here, and then naming the
		// server to ask is the whole fix.
		s.ui.bad("%v", err)
		ask, aerr := s.ui.yes("Is there a nameserver to ask directly?", false)
		if aerr != nil {
			return false
		}
		if !ask {
			return false
		}
		addr, aerr := s.ui.ask("Nameserver (host:port)", "")
		if aerr != nil {
			return false
		}
		if _, _, perr := net.SplitHostPort(addr); perr != nil {
			addr = net.JoinHostPort(addr, "53")
		}
		s.cfg.ACME.DNS.Resolvers = []string{addr}
		return s.runHookCheck(ctx, provider)
	}
	if err != nil {
		s.ui.bad("%v", err)
		s.ui.note("",
			"  The usual causes, in order:",
			"    · the token cannot write to that zone",
			"    · the hook wrote to a zone that is not the one serving the name",
			"      (a delegated subdomain, or a registrar zone no longer authoritative)",
			"    · the record is there but the TTL is enormous",
			"",
			"  Run it by hand to see which:",
			fmt.Sprintf("    %s present _acme-challenge.%s testvalue",
				strings.Join(s.cfg.ACME.DNS.Command, " "), domain),
			fmt.Sprintf("    dig +short TXT _acme-challenge.%s", domain),
			"")
		return false
	}
	s.ui.good("published, visible in DNS, and retracted again")
	return true
}

// ---------------------------------------------------------------- client app

func (s *session) askClient(ctx context.Context) error {
	s.ui.heading("The app the plane side runs")
	s.ui.note(
		"The client is a separate build that this server serves. Without it the",
		"server comes up and every page is a note explaining that it did not.",
		"",
	)

	if root := server.RepoClientDist(); root != "" {
		s.ui.good("found a built client at %s", root)
		s.ui.note("", "  It is in the checkout this binary came from, so the server finds it",
			"  by itself and webRoot can stay empty.")
		return nil
	}
	if s.repo == "" {
		s.ui.warn("no built client found, and this is not a checkout to build one from")
		for {
			path, err := s.ui.askOptional("Path to a built client")
			if err != nil {
				return err
			}
			if path == "" {
				s.notes = append(s.notes,
					"Build the client (cd client && npm ci && npm run build) and point "+
						"webRoot at client/dist, or copy it to "+
						filepath.Join(s.cfg.DataDir, "webapp")+".")
				return nil
			}
			abs, err := expand(path)
			if err != nil {
				s.ui.bad("%v", err)
				continue
			}
			if _, err := os.Stat(filepath.Join(abs, "index.html")); err != nil {
				// Checked rather than taken on trust: a wrong path here is the
				// original complaint, and it surfaces as a server that starts
				// fine and serves an explanation to every request.
				s.ui.bad("no index.html in %s", abs)
				continue
			}
			s.cfg.WebRoot = abs
			s.ui.good("%s", abs)
			return nil
		}
	}

	s.ui.warn("the client has not been built yet")
	if _, err := exec.LookPath("npm"); err != nil {
		s.ui.bad("npm is not installed, so it cannot be built here")
		s.notes = append(s.notes,
			"Build the client where npm is available and copy client/dist across, "+
				"or set webRoot to point at it.")
		return nil
	}
	build, err := s.ui.yes("Build it now? (npm ci && npm run build, a minute or two)", true)
	if err != nil {
		return err
	}
	if !build {
		s.notes = append(s.notes,
			"Build the client before starting: cd client && npm ci && npm run build.")
		return nil
	}
	return s.buildClient(ctx)
}

func (s *session) buildClient(ctx context.Context) error {
	dir := filepath.Join(s.repo, "client")
	steps := [][]string{{"npm", "ci"}, {"npm", "run", "build"}}
	if _, err := os.Stat(filepath.Join(dir, "node_modules")); err == nil {
		steps = steps[1:]
	}
	for _, step := range steps {
		s.ui.say("  … %s", strings.Join(step, " "))
		ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		cmd := exec.CommandContext(ctx, step[0], step[1:]...) //nolint:gosec // fixed argv
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			s.ui.bad("%s failed: %v", strings.Join(step, " "), err)
			if trimmed := strings.TrimSpace(string(out)); trimmed != "" {
				s.ui.say("%s", lastLines(trimmed, 12))
			}
			s.notes = append(s.notes, "The client build failed; the server will serve a "+
				"placeholder page until it succeeds.")
			return nil
		}
	}
	if root := server.RepoClientDist(); root != "" {
		s.ui.good("built: %s", root)
	} else {
		s.ui.warn("the build reported success but produced no index.html")
	}
	return nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i, l := range lines {
		lines[i] = "    " + l
	}
	return strings.Join(lines, "\n")
}

// ---------------------------------------------------------------- confirm

func (s *session) confirm(configPath string) (string, error) {
	if configPath == "" {
		configPath = filepath.Join(s.cfg.DataDir, "config.json")
	}
	s.ui.heading("What this will do")
	for _, line := range s.summary() {
		s.ui.note(line)
	}
	s.ui.blank()
	s.ui.note("Write " + configPath)
	if _, err := os.Stat(configPath); err == nil {
		s.ui.note("  (the file there now is kept as " + configPath + ".bak)")
	}
	s.ui.blank()

	ok, err := s.ui.yes("Go ahead?", true)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errCancelled
	}
	return configPath, nil
}

func (s *session) summary() []string {
	lines := []string{
		"Data directory   " + s.cfg.DataDir,
	}
	if s.cfg.ChromeAttach != "" {
		lines = append(lines, "Browser          your own, at "+s.cfg.ChromeAttach)
	} else {
		lines = append(lines, "Browser          Chromium, started by Skyhook")
	}
	switch {
	case s.cfg.InsecureLoopback:
		lines = append(lines, "Reached at       http://127.0.0.1:4434 (this machine only)")
	case s.cfg.BehindProxy:
		lines = append(lines, "Reached at       "+s.cfg.PublicURL+" (via your proxy)")
	case s.cfg.ACME.Enabled:
		lines = append(lines,
			"Reached at       https://"+s.cfg.Hosts[0]+portSuffix(s.cfg.FallbackListen),
			"Certificate      Let's Encrypt, "+s.cfg.ACME.Challenge)
		if s.cfg.ACME.Challenge == config.ChallengeDNS01 {
			lines = append(lines, "DNS hook         "+strings.Join(s.cfg.ACME.DNS.Command, " "))
		}
	default:
		lines = append(lines,
			"Reached at       https://"+s.cfg.Hosts[0]+portSuffix(s.cfg.FallbackListen),
			"Certificate      self-signed, pinned by the client")
	}
	if s.cfg.WebRoot != "" {
		lines = append(lines, "Client app       "+s.cfg.WebRoot)
	} else if root := server.RepoClientDist(); root != "" {
		lines = append(lines, "Client app       "+root+" (found automatically)")
	}
	return lines
}

func portSuffix(listen string) string {
	_, port, err := net.SplitHostPort(listen)
	if err != nil || port == "443" {
		return ""
	}
	return ":" + port
}

// ---------------------------------------------------------------- write

func (s *session) finish(ctx context.Context, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// Checked before it is installed, by the same loader the server uses. A
	// setup that writes a file the server then refuses would be worse than no
	// setup at all.
	if err := s.validate(path); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		if err := os.Rename(path, path+".bak"); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return err
	}
	s.ui.blank()
	s.ui.good("wrote %s", path)

	if err := s.prepare(path); err != nil {
		return err
	}
	s.epilogue(path)
	return nil
}

// validate runs the written configuration through config.Load from a scratch
// copy, so a refusal is reported here rather than on the first start.
func (s *session) validate(path string) error {
	tmp, err := os.CreateTemp("", "skyhook-setup-*.json")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	data, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if _, err := config.Load(tmp.Name()); err != nil {
		s.ui.bad("%v", err)
		return fmt.Errorf("setup: the configuration it built is not one the server accepts: %w", err)
	}
	return nil
}

// prepare creates the directories, settles the token and gets the certificate —
// the work of `-init`, offered here because it is the step that proves the
// answers were right.
func (s *session) prepare(path string) error {
	s.ui.blank()
	what := "Create the data directory and the certificate now?"
	if s.cfg.ACME.Enabled {
		what = "Get the certificate now? This is the step that proves the answers were right."
	}
	do, err := s.ui.yes(what, true)
	if err != nil {
		return err
	}
	if !do {
		s.notes = append(s.notes, "Run `skyhookd -config "+path+" -init` when you are ready.")
		return nil
	}

	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	if _, err := cfg.EnsureToken(skyhooksession.NewToken); err != nil {
		s.ui.warn("the token could not be written: %v", err)
	}
	if err := cfg.Save(path); err != nil {
		s.ui.warn("the token could not be saved to the config: %v", err)
	}
	if s.cfg.ACME.Enabled {
		s.ui.say("  … asking the authority for a certificate (this can take a minute)")
	}
	cert, err := server.Prepare(cfg, s.log)
	if err != nil {
		s.ui.bad("%v", err)
		s.notes = append(s.notes,
			"The certificate was not obtained. Everything else is written, so fix the "+
				"cause and re-run `skyhookd -config "+path+" -init`.")
		return nil
	}
	if s.cfg.InsecureLoopback {
		// Prepare mints one anyway, and nothing serves it in this mode. Saying
		// "certificate: self-signed, pinned …" here would describe a deployment
		// this is not.
		s.ui.good("data directory ready; no TLS in loopback mode")
	} else {
		s.ui.good("%s", cert.Describe())
	}
	if link := server.PairingLink(cfg, pinOf(cert)); link != "" {
		s.ui.blank()
		s.ui.note("Pair the plane side by opening this once, in Chrome, on that machine:",
			"", "  "+link, "")
	}
	return nil
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fallbackOn443 reports whether the listener that answers a tls-alpn-01
// challenge is on the port the authority dials.
func fallbackOn443(cfg config.Config) bool {
	_, port, err := net.SplitHostPort(cfg.FallbackListen)
	return err == nil && port == "443"
}

func pinOf(cert *transport.CertBundle) string {
	sha, _, _ := cert.Pin()
	return sha
}

func (s *session) epilogue(path string) {
	s.ui.heading("Starting it")
	s.ui.note("  skyhookd -config " + path)
	if s.cfg.InsecureLoopback {
		s.ui.note("", "That is the demo shape; `scripts/demo.sh` does the same thing and stops",
			"on its own after ten minutes.")
	}
	if len(s.notes) > 0 {
		s.ui.blank()
		s.ui.say("Still to do:")
		for _, n := range s.notes {
			s.ui.note("· " + n)
		}
	}
	s.ui.blank()
	s.ui.note("docs/OPERATIONS.md has the rest: the security posture, what a capture",
		"is for, and what to do when the mirror looks wrong.")
}

// ---------------------------------------------------------------- helpers

// bindable reports whether this process could take a port. It answers a
// different question from "can the internet reach it", and the choose() detail
// says so — but a port already in use is the failure that happens first, and
// the only one findable from here.
func bindable(port int) error {
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		return err
	}
	return ln.Close()
}

func bindReport(port int) string {
	if err := bindable(port); err != nil {
		return fmt.Sprintf("port %d cannot be bound here (%s)", port, bindWhy(err))
	}
	return fmt.Sprintf("port %d is free", port)
}

func bindWhy(err error) string {
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Err != nil {
		return opErr.Err.Error()
	}
	return err.Error()
}

// repoRoot finds the checkout this binary was run from, which is what makes
// building the client and pointing at the example hook possible.
func repoRoot() string {
	if dist := server.RepoClientDist(); dist != "" {
		return filepath.Dir(filepath.Dir(dist))
	}
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	dir := wd
	for i := 0; i < 4; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			if _, err := os.Stat(filepath.Join(dir, "client", "package.json")); err == nil {
				return dir
			}
			return ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

func expand(p string) (string, error) {
	if strings.HasPrefix(p, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, strings.TrimPrefix(p, "~")), nil
	}
	return filepath.Abs(p)
}
