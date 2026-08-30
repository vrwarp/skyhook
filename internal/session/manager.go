package session

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/vrwarp/skyhook/internal/adapter"
	"github.com/vrwarp/skyhook/internal/appver"
	"github.com/vrwarp/skyhook/internal/cdp"
	"github.com/vrwarp/skyhook/internal/imgproc"
	"github.com/vrwarp/skyhook/internal/mirror"
	"github.com/vrwarp/skyhook/internal/protocol"
	"github.com/vrwarp/skyhook/internal/transport"
)

// ManagerOptions configures the session manager.
type ManagerOptions struct {
	Logger *slog.Logger
	// Token is the shared secret from the pairing file. There is exactly one
	// user, so there is exactly one credential.
	Token string
	// TTL is how long a session survives without a client. The design calls for
	// 12 hours: long enough to cover a flight plus a taxi.
	TTL time.Duration
	// RingBytes bounds the per-tab replay buffer.
	RingBytes int
	// Compression enables zstd on the wire.
	Compression bool
	// ProfileDir is the Chromium user data directory, wiped by the kill switch.
	ProfileDir string
	// UserAgent overrides the browser default.
	UserAgent string
	// AcceptLanguage rides with the user agent override.
	AcceptLanguage string
	// Blocked is the landside request denylist, per host.
	Blocked mirror.Blocklist
	// MaxTabs caps concurrent tabs.
	MaxTabs int
	// Adapters builds the adapters started for each session.
	Adapters []adapter.Factory
	// HomeURL is opened in the first tab of a fresh session.
	HomeURL string
	// CanvasStream keeps following a canvas that animates with nobody touching
	// it. Zero, the default, means a canvas is photographed only because of
	// something the reader did. See mirror.Options.StreamEvery.
	CanvasStream time.Duration
	// Capture configures the diagnostic bundles this server writes.
	Capture CaptureOptions
	// IntegrityInterval is how often each tab's document is checked against the
	// client's. Zero means the default; a test that would otherwise wait out
	// two ticks turns it down.
	IntegrityInterval time.Duration
	// WebRoot is the directory the plane-side app is served from. The manager
	// reads its build stamp so every Welcome can say which build of the client
	// this server would hand out today; the client compares that against its
	// own and offers the reader the upgrade. Empty means no app is served, and
	// then the question simply goes unanswered.
	WebRoot string
	// UploadDir is where the reader's files land on their way into a page's
	// file input (P-007). Created and emptied at construction — staged files
	// from a previous run belong to asks nobody holds any more — and wiped by
	// the kill switch. Empty means uploads are off.
	UploadDir string
}

// Manager owns the browser and the set of sessions.
type Manager struct {
	opts    ManagerOptions
	log     *slog.Logger
	browser *cdp.Browser
	images  *imgproc.Pipeline
	trainer *protocol.DictTrainer

	// clientApp reports the build stamp of the plane-side app on disk, re-read
	// when a deploy replaces it.
	clientApp *appver.Reader

	mu       sync.Mutex
	sessions map[string]*Session
	closed   bool

	// captureMu guards the rate limit on automatic captures, which is a
	// property of the server rather than of any one session: the disk they
	// would fill is shared.
	captureMu       sync.Mutex
	lastAutoCapture time.Time

	// dlMu guards the download shelf — manager property like the capture rate
	// limit, because the disk the files land on is shared. downloads is nil
	// until EnableDownloads has run, which is what "downloads are off" means.
	// See downloads.go.
	dlMu        sync.Mutex
	downloads   map[string]*download
	dlOrder     []string
	downloadDir string
}

// NewManager builds the manager around an already-launched browser.
func NewManager(br *cdp.Browser, images *imgproc.Pipeline, opts ManagerOptions) *Manager {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.TTL == 0 {
		opts.TTL = 12 * time.Hour
	}
	if opts.MaxTabs == 0 {
		opts.MaxTabs = 8
	}
	m := &Manager{
		opts: opts, log: opts.Logger, browser: br, images: images,
		trainer:   protocol.NewDictTrainer(),
		sessions:  map[string]*Session{},
		clientApp: appver.NewReader(opts.WebRoot),
	}
	if opts.UploadDir != "" {
		if err := os.MkdirAll(opts.UploadDir, 0o700); err != nil {
			opts.Logger.Warn("uploads are off: staging dir unusable", "dir", opts.UploadDir, "err", err)
			m.opts.UploadDir = ""
		} else {
			emptyDir(opts.UploadDir)
		}
	}
	go m.janitor()
	return m
}

// emptyDir deletes a directory's contents, best-effort.
func emptyDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}

// Images exposes the shared image pipeline.
func (m *Manager) Images() *imgproc.Pipeline { return m.images }

// Trainer exposes the dictionary trainer.
func (m *Manager) Trainer() *protocol.DictTrainer { return m.trainer }

// resumeBudget bounds one piece of the work a reconnect does before the
// connection starts reading. A resync is a whole document re-serialised, which
// is the same job the tab queue gives three minutes; this is that, on a
// goroutine where overrunning costs the reader every tab at once rather than
// one, so it gets the shorter end of the same argument.
const resumeBudget = 60 * time.Second

// ErrUnauthorized means the client presented the wrong token.
var ErrUnauthorized = errors.New("session: unauthorized")

// Serve owns a connection for its lifetime. It is the transport handler.
func (m *Manager) Serve(conn transport.Conn) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := m.log.With("peer", conn.RemoteAddr(), "transport", conn.Kind())
	log.Info("client connected")

	// The first frame must be a Hello; nothing else is decodable before the
	// session's codec capabilities are known.
	bootstrap, err := protocol.NewCodec(false, 0)
	if err != nil {
		return
	}
	defer bootstrap.Close()

	helloCtx, helloCancel := context.WithTimeout(ctx, 30*time.Second)
	msg, err := conn.Recv(helloCtx)
	helloCancel()
	if err != nil {
		log.Warn("no hello", "err", err)
		_ = conn.Close(protocol.CloseBadHello, "no hello")
		return
	}
	_, frame, err := bootstrap.DecodeFrame(msg.Payload)
	if err != nil || frame.Type != protocol.TypeHello {
		log.Warn("bad hello", "err", err)
		_ = conn.Close(protocol.CloseBadHello, "bad hello")
		return
	}
	var hello protocol.Hello
	if err := frame.DecodeBody(&hello); err != nil {
		_ = conn.Close(protocol.CloseBadHello, "bad hello body")
		return
	}
	if !m.authorize(hello.Token) {
		log.Warn("unauthorized client")
		_ = conn.Close(protocol.CloseUnauthorized, "unauthorized")
		return
	}
	if hello.Version != protocol.Version {
		log.Warn("protocol mismatch", "client", hello.Version, "server", protocol.Version)
		_ = conn.Close(protocol.CloseVersionMismatch, "protocol version mismatch")
		return
	}

	// The build of the app this server would serve today, which is not
	// necessarily the build that is talking: a PWA runs out of its own cache
	// until something makes it upgrade. Nothing is refused over it — the wire
	// format is what has to match, and the protocol version above is what says
	// whether it does — but both halves say so, here in the log and in the
	// Welcome below, so the reader can be offered the newer one.
	served := m.clientApp.Stamp()
	if served.Known() && hello.Build != "" && hello.Build != served.Build {
		log.Info("client is running an older build of the app",
			"client", hello.Build, "served", served.Build)
	}

	sess, resumed, err := m.resolve(ctx, hello)
	if err != nil {
		log.Error("session setup failed", "err", err)
		_ = conn.Close(protocol.CloseSetupFailed, err.Error())
		return
	}
	sess.SetClient(hello.Client, hello.Build)
	sess.Attach(conn)
	defer sess.Detach(conn)

	welcome := protocol.Welcome{
		Version: protocol.Version, SessionID: sess.ID, Resumed: resumed,
		Tabs: sess.TabRefs(), KeepaliveMS: 15000, Server: Version(),
		Adapters:      sess.AdapterNames(),
		ClientVersion: served.Version, ClientBuild: served.Build,
	}
	if m.opts.Compression {
		welcome.Caps = append(welcome.Caps, "zstd")
	}
	sess.Send(protocol.ChCtrl, protocol.TypeWelcome, 0, welcome)

	// The download shelf is server state, so whatever landed while this client
	// was away is told now rather than replayed: one frame per file.
	m.sendDownloads(sess)

	// Replay input the client queued while it was offline, before any resync,
	// so the page state the client resyncs to already contains their typing.
	for i := range hello.Queued {
		qf := hello.Queued[i]
		if err := sess.Dispatch(ctx, protocol.ChInput, &qf); err != nil {
			log.Debug("queued input replay failed", "err", err)
		}
	}
	// Everything from here to the read loop talks to the browser, serially,
	// once per tab — and it does it on the goroutine that is about to become
	// this connection's only reader. Unbounded, one tab whose renderer will
	// not answer means the client is attached, has its Welcome, and is never
	// heard from again: it looks connected on the plane side, and every
	// reconnect it makes leaves another goroutine stuck in the same place.
	// Per tab rather than for the set, so one slow page cannot spend the
	// budget the others needed. See Session.Dispatch, which is the same
	// argument for the frames that arrive after this.
	setup := func(fn func(context.Context)) {
		sctx, cancel := context.WithTimeout(ctx, resumeBudget)
		defer cancel()
		fn(sctx)
	}
	for _, ta := range hello.Resume {
		sess.Ack(ta.Tab, ta.Seq, ta.Hash, ta.Epoch)
		// A client that missed nothing gets nothing: for a quiet tab the ring
		// is empty after that ack, and the resync below would answer with a
		// whole document to close a gap that does not exist — the single
		// largest avoidable cost on a link that reconnects every few minutes.
		if sess.TabCurrent(ta) {
			log.Debug("tab resumed current; no resync", "tab", ta.Tab, "seq", ta.Seq)
			continue
		}
		setup(func(c context.Context) { sess.Resync(c, ta.Tab, ta.Seq, "reconnect") })
	}
	if resumed {
		// The client may be rejoining after nothing worse than a reconnect, or it
		// may have just been loaded from scratch and know only what the Welcome
		// above carried. Telling both is one frame per tab and settles it.
		setup(func(c context.Context) { sess.RefreshTabs(c) })
		setup(func(c context.Context) { sess.replayAdapterBacklog(c) })
	}

	for {
		msg, err := conn.Recv(ctx)
		if err != nil {
			log.Info("client disconnected", "err", err)
			return
		}
		ch, f, err := sess.codec.DecodeFrame(msg.Payload)
		if err != nil {
			log.Warn("undecodable frame", "err", err)
			continue
		}
		if err := sess.Dispatch(ctx, ch, f); err != nil {
			log.Debug("dispatch failed", "type", f.Type, "err", err)
			sess.Send(protocol.ChCtrl, protocol.TypeError, f.Tab,
				protocol.ErrorBody{Code: "dispatch", Message: err.Error()})
		}
	}
}

func (m *Manager) authorize(token string) bool {
	if m.opts.Token == "" {
		return true // unauthenticated mode, for loopback development only
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(m.opts.Token)) == 1
}

func (m *Manager) resolve(ctx context.Context, hello protocol.Hello) (*Session, bool, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, false, errors.New("session: server shutting down")
	}
	if hello.SessionID != "" {
		if s, ok := m.sessions[hello.SessionID]; ok {
			m.mu.Unlock()
			s.SetViewport(ctx, hello.Viewport)
			for _, c := range hello.Caps {
				s.mu.Lock()
				s.caps[c] = true
				s.mu.Unlock()
			}
			return s, true, nil
		}
	}
	m.mu.Unlock()

	id := newID()
	s, err := newSession(id, m, Options{
		Logger:      m.log.With("session", id),
		Viewport:    hello.Viewport,
		RingBytes:   m.opts.RingBytes,
		Compression: m.opts.Compression && hasCap(hello.Caps, "zstd"),
	})
	if err != nil {
		return nil, false, err
	}
	for _, c := range hello.Caps {
		s.caps[c] = true
	}
	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()

	for _, f := range m.opts.Adapters {
		a, err := f(m.browser, m.log.With("adapter", true))
		if err != nil {
			m.log.Warn("adapter construction failed", "err", err)
			continue
		}
		if err := s.StartAdapter(ctx, a); err != nil {
			m.log.Warn("adapter start failed", "adapter", a.Name(), "err", err)
		}
	}
	if m.opts.HomeURL != "" {
		if _, err := s.OpenTab(ctx, protocol.Navigate{URL: m.opts.HomeURL}); err != nil {
			m.log.Warn("home tab failed", "err", err)
		}
	}
	return s, false, nil
}

func hasCap(caps []string, want string) bool {
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}

// Session looks up a live session.
func (m *Manager) Session(id string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

// Sessions lists live sessions.
func (m *Manager) Sessions() []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s)
	}
	return out
}

// WipeProfile deletes the browser profile. Used by the kill switch.
func (m *Manager) WipeProfile(ctx context.Context) error {
	if m.opts.ProfileDir == "" {
		return errors.New("session: no profile directory configured")
	}
	m.log.Warn("kill switch: wiping profile", "dir", m.opts.ProfileDir)
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for id, s := range m.sessions {
		sessions = append(sessions, s)
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	for _, s := range sessions {
		s.Close(ctx)
	}
	if err := m.browser.Close(); err != nil {
		m.log.Warn("browser close during wipe", "err", err)
	}
	// The shelf goes with the profile: its files were fetched with the same
	// cookies the wipe exists to destroy. Staged uploads are the reader's own
	// files and go for the same reason.
	m.WipeDownloads()
	if m.opts.UploadDir != "" {
		emptyDir(m.opts.UploadDir)
	}
	entries, err := os.ReadDir(m.opts.ProfileDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(m.opts.ProfileDir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// Close shuts every session down.
func (m *Manager) Close(ctx context.Context) {
	m.mu.Lock()
	m.closed = true
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.sessions = map[string]*Session{}
	m.mu.Unlock()
	for _, s := range sessions {
		s.Close(ctx)
	}
}

// janitor expires sessions whose client has been gone longer than the TTL.
func (m *Manager) janitor() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return
		}
		var expired []*Session
		for id, s := range m.sessions {
			if s.Online() {
				continue
			}
			if time.Since(s.LastSeen()) > m.opts.TTL {
				expired = append(expired, s)
				delete(m.sessions, id)
			}
		}
		m.mu.Unlock()
		for _, s := range expired {
			m.log.Info("session expired",
				"session", s.ID, "age", time.Since(s.Created()).Round(time.Second))
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			s.Close(ctx)
			cancel()
		}
	}
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return time.Now().UTC().Format("20060102150405.000000000")
	}
	return hex.EncodeToString(b[:])
}

// NewToken mints a pairing token.
func NewToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func originOf(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// version is stamped by the build.
var version = "dev"

// Version reports the server build string.
func Version() string { return version }

// SetVersion is called from main with the linker-provided version.
func SetVersion(v string) {
	if v != "" {
		version = v
	}
}
