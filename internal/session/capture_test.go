package session

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/protocol"
	"github.com/vrwarp/skyhook/internal/transport"
)

// newTestSession builds a session with no browser behind it. Everything the
// capture orchestration does — opening a bundle, freezing state, reassembling
// the plane side, sealing, pruning — is reachable without one; the per-tab
// landside artifacts need a real Chromium and are covered by the e2e suite.
func newTestSession(t *testing.T, opts CaptureOptions) *Session {
	t.Helper()
	if opts.Dir == "" && opts.Keep > 0 {
		opts.Dir = t.TempDir()
	}
	m := NewManager(nil, nil, ManagerOptions{
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Capture: opts,
	})
	s, err := newSession("test-session", m, Options{
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Viewport: protocol.Viewport{W: 1280, H: 800, DPR: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(context.Background()) })
	return s
}

// fakeConn is a connection that accepts everything and answers nothing. It
// exists so a session can be Online without a link: whether the server asks the
// plane side for its half, and whether it then waits for it, both turn on that.
type fakeConn struct {
	mu          sync.Mutex
	sent        [][]byte
	done        chan struct{}
	closeCode   uint32
	closeReason string
}

func newFakeConn() *fakeConn { return &fakeConn{done: make(chan struct{})} }

func (c *fakeConn) Send(_ protocol.Channel, msg []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, append([]byte(nil), msg...))
	return nil
}

func (c *fakeConn) SendObject(ch protocol.Channel, msg []byte) error { return c.Send(ch, msg) }
func (c *fakeConn) SendDatagram([]byte) error                        { return nil }

func (c *fakeConn) Recv(ctx context.Context) (transport.Message, error) {
	<-ctx.Done()
	return transport.Message{}, ctx.Err()
}

func (c *fakeConn) Stats() transport.Stats { return transport.Stats{} }
func (c *fakeConn) Kind() string           { return "websocket" }
func (c *fakeConn) RemoteAddr() string     { return "test" }
func (c *fakeConn) Done() <-chan struct{}  { return c.done }

func (c *fakeConn) Close(code uint32, reason string) error {
	c.mu.Lock()
	c.closeCode, c.closeReason = code, reason
	c.mu.Unlock()
	select {
	case <-c.done:
	default:
		close(c.done)
	}
	return nil
}

// closedWith reports how this connection was hung up on.
func (c *fakeConn) closedWith() (uint32, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeCode, c.closeReason
}

func (c *fakeConn) isClosed() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// frames returns every frame the server sent, decoded.
func (c *fakeConn) frames(t *testing.T, codec *protocol.Codec) []*protocol.Frame {
	t.Helper()
	c.mu.Lock()
	msgs := append([][]byte(nil), c.sent...)
	c.mu.Unlock()
	out := make([]*protocol.Frame, 0, len(msgs))
	for _, m := range msgs {
		_, f, err := codec.DecodeFrame(m)
		if err != nil {
			continue
		}
		out = append(out, f)
	}
	return out
}

func defaultCaptureOptions(t *testing.T) CaptureOptions {
	t.Helper()
	return CaptureOptions{
		Dir: t.TempDir(), Keep: 5, MaxBytes: 8 << 20, ClientBytes: 1 << 20,
		JournalBytes: 1 << 20, Wait: 2 * time.Second, Interval: time.Minute,
	}
}

// waitForBundle polls for the sealed zip. The capture runs on its own
// goroutine, because it waits on a link that may be several seconds wide.
func waitForBundle(t *testing.T, dir string) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(dir)
		if err == nil {
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), ".zip") {
					return filepath.Join(dir, e.Name())
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no bundle appeared in %s", dir)
	return ""
}

func bundleFiles(t *testing.T, path string) map[string][]byte {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("open bundle: %v", err)
	}
	defer func() { _ = zr.Close() }()
	out := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		out[f.Name] = data
	}
	return out
}

// A capture with nobody connected still has to produce a readable bundle: the
// landside half alone is often enough, and a capture that fails because the
// link is down is a capture that fails exactly when it is needed.
func TestCaptureWithNoClientStillSeals(t *testing.T) {
	opts := defaultCaptureOptions(t)
	s := newTestSession(t, opts)
	s.events.Add("divergence", 4, map[string]any{"clientHash": 1, "serverHash": 2})

	id, err := s.StartCapture(protocol.CaptureManual, "the page looked stale", false)
	if err != nil {
		t.Fatal(err)
	}
	files := bundleFiles(t, waitForBundle(t, opts.Dir))

	var manifest map[string]any
	if err := json.Unmarshal(files["manifest.json"], &manifest); err != nil {
		t.Fatalf("manifest is not readable JSON: %v", err)
	}
	if manifest["id"] != id {
		t.Errorf("manifest names capture %v, want %s", manifest["id"], id)
	}
	if manifest["note"] != "the page looked stale" {
		t.Errorf("the reader's note did not reach the bundle: %v", manifest["note"])
	}
	if manifest["clientOnline"] != false {
		t.Errorf("the bundle claims a client was connected: %v", manifest["clientOnline"])
	}
	if !strings.Contains(string(files["NOTES.txt"]), "no client was connected") {
		t.Errorf("the missing half was not explained: %s", files["NOTES.txt"])
	}
	if _, ok := files["session/session.json"]; !ok {
		t.Error("the bundle has no session report")
	}
	if !strings.Contains(string(files["session/events.json"]), "divergence") {
		t.Errorf("the event timeline did not reach the bundle: %s", files["session/events.json"])
	}
}

// The plane side's artifacts arrive chunked and gzipped. Both have to be undone
// on the way in, or a bundle is full of files nobody can open.
func TestCaptureReassemblesThePlaneSide(t *testing.T) {
	opts := defaultCaptureOptions(t)
	opts.Wait = 10 * time.Second
	opts.Screenshots = true
	s := newTestSession(t, opts)
	conn := newFakeConn()
	s.Attach(conn)

	id, err := s.StartCapture(protocol.CaptureManual, "", false)
	if err != nil {
		t.Fatal(err)
	}

	// The request has to go out before StartCapture returns, on ctrl. A
	// divergence capture is followed immediately by a resync on the dom
	// channel, and the client only holds the diverged document until it
	// applies that.
	var asked *protocol.CaptureRequest
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && asked == nil {
		for _, f := range conn.frames(t, s.codec) {
			if f.Type != protocol.TypeCapture {
				continue
			}
			var req protocol.CaptureRequest
			if err := f.DecodeBody(&req); err == nil {
				asked = &req
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if asked == nil {
		t.Fatal("the server never asked the plane side for its half")
	}
	if asked.ID != id {
		t.Errorf("the request names capture %q, want %q", asked.ID, id)
	}
	if asked.MaxBytes != opts.ClientBytes || !asked.Screenshots {
		t.Errorf("the client was given the wrong budget: %+v", *asked)
	}

	html := strings.Repeat("<div class=row>mirrored</div>", 400)
	var packed bytes.Buffer
	zw := gzip.NewWriter(&packed)
	if _, err := zw.Write([]byte(html)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	// Chunked exactly as the worker chunks it.
	data := packed.Bytes()
	const chunk = 64
	for off := 0; off < len(data); off += chunk {
		end := min(off+chunk, len(data))
		if err := s.CapturePart(protocol.CapturePart{
			ID: id, Name: "tabs/1/mirror.html.gz", Data: data[off:end], More: end < len(data),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CapturePart(protocol.CapturePart{
		ID: id, Name: "tabs/1/screenshot.webp", Data: []byte("RIFFfake"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.CapturePart(protocol.CapturePart{
		ID: id, Error: "3 image(s) were not in the plane-side cache", Done: true,
	}); err != nil {
		t.Fatal(err)
	}

	files := bundleFiles(t, waitForBundle(t, opts.Dir))
	// Stored decompressed and under its real name: a zip full of gzip members
	// is a zip nobody can read without unpacking it twice.
	if got := string(files["planeside/tabs/1/mirror.html"]); got != html {
		t.Errorf("the mirrored document did not survive reassembly (%d of %d bytes)",
			len(got), len(html))
	}
	if _, ok := files["planeside/tabs/1/mirror.html.gz"]; ok {
		t.Error("the gzip wrapper was stored instead of being undone")
	}
	if string(files["planeside/tabs/1/screenshot.webp"]) != "RIFFfake" {
		t.Error("the screenshot did not survive")
	}
	if !strings.Contains(string(files["NOTES.txt"]), "not in the plane-side cache") {
		t.Errorf("what the plane side could not gather was not recorded: %s", files["NOTES.txt"])
	}

	var manifest map[string]any
	if err := json.Unmarshal(files["manifest.json"], &manifest); err != nil {
		t.Fatal(err)
	}
	plane, _ := manifest["planeSide"].(map[string]any)
	if plane == nil || plane["files"].(float64) != 2 {
		t.Errorf("the manifest miscounted the plane-side artifacts: %v", manifest["planeSide"])
	}
}

// An artifact cut short by an outage is still evidence. Dropping it silently
// would hide the very failure that cut it short.
func TestCaptureKeepsAPartialArtifact(t *testing.T) {
	opts := defaultCaptureOptions(t)
	// Long enough that the chunk below is certainly in hand before the deadline
	// fires. A tighter window would be racing the capture's own goroutine, and
	// this test is about what happens after the part arrives, not about whether
	// it does.
	opts.Wait = 2 * time.Second
	s := newTestSession(t, opts)
	s.Attach(newFakeConn())

	id, err := s.StartCapture(protocol.CaptureManual, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CapturePart(protocol.CapturePart{
		ID: id, Name: "tabs/1/mirror.html", Data: []byte("<div>half a doc"), More: true,
	}); err != nil {
		t.Fatal(err)
	}
	// Stated rather than assumed: everything below is about a partial artifact
	// the capture is holding, so a test that silently stopped holding one would
	// pass for the wrong reason.
	if c := s.capture.Load(); c == nil {
		t.Fatal("the capture sealed before its first part arrived")
	} else {
		c.mu.Lock()
		held := len(c.partial["tabs/1/mirror.html"])
		c.mu.Unlock()
		if held != len("<div>half a doc") {
			t.Fatalf("the capture is holding %d bytes of the artifact, want %d",
				held, len("<div>half a doc"))
		}
	}
	// No further parts, and no Done: exactly what a link dying mid-upload looks
	// like from here.

	files := bundleFiles(t, waitForBundle(t, opts.Dir))
	if got := string(files["planeside/tabs/1/mirror.html.partial"]); got != "<div>half a doc" {
		t.Errorf("the partial artifact was lost: %q", got)
	}
	notes := string(files["NOTES.txt"])
	if !strings.Contains(notes, "arrived only partially") {
		t.Errorf("the truncation was not recorded: %s", notes)
	}
	if !strings.Contains(notes, "did not finish within") {
		t.Errorf("the timeout was not recorded: %s", notes)
	}
}

func TestCaptureRefusedWhenDisabled(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	if _, err := s.StartCapture(protocol.CaptureManual, "", false); err != ErrCapturesDisabled {
		t.Errorf("a server with captures off returned %v, want ErrCapturesDisabled", err)
	}
}

// A page that diverges once usually diverges every thirty seconds. Without a
// rate limit that is a bundle every thirty seconds, which fills a disk with
// many copies of one bug.
func TestAutomaticCapturesAreThrottled(t *testing.T) {
	opts := defaultCaptureOptions(t)
	opts.Interval = time.Hour
	s := newTestSession(t, opts)

	if _, err := s.StartCapture(protocol.CaptureDivergence, "", true); err != nil {
		t.Fatal(err)
	}
	waitForBundle(t, opts.Dir)
	if _, err := s.StartCapture(protocol.CaptureDivergence, "", true); err != ErrCaptureThrottled {
		t.Errorf("the second automatic capture returned %v, want ErrCaptureThrottled", err)
	}
	// A person asking is never throttled: they are the one paying for it, and
	// they asked because something is wrong now.
	if _, err := s.StartCapture(protocol.CaptureManual, "", false); err != nil {
		t.Errorf("a manual capture was refused after an automatic one: %v", err)
	}
}

// The divergence trigger has to be genuinely optional: an operator who does not
// want bundles appearing unasked must get none, and one who does must not have
// a failed capture interfere with the resync that repairs the tab.
func TestDivergenceCaptureHonoursTheSetting(t *testing.T) {
	off := defaultCaptureOptions(t)
	off.OnDivergence = false
	s := newTestSession(t, off)
	s.captureDivergence(1)
	if entries, _ := os.ReadDir(off.Dir); len(entries) != 0 {
		t.Errorf("captureOnDivergence was off but a bundle was written: %v", entries)
	}

	on := defaultCaptureOptions(t)
	on.OnDivergence = true
	s2 := newTestSession(t, on)
	s2.captureDivergence(1)
	files := bundleFiles(t, waitForBundle(t, on.Dir))
	var manifest map[string]any
	if err := json.Unmarshal(files["manifest.json"], &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest["reason"] != protocol.CaptureDivergence {
		t.Errorf("the bundle does not say it was taken at a divergence: %v", manifest["reason"])
	}
	if !strings.Contains(manifest["note"].(string), "different document") {
		t.Errorf("the automatic note does not explain itself: %v", manifest["note"])
	}
}

func TestCaptureRetainsOnlyTheNewestBundles(t *testing.T) {
	opts := defaultCaptureOptions(t)
	opts.Keep = 2
	s := newTestSession(t, opts)

	for i := 0; i < 4; i++ {
		if _, err := s.StartCapture(protocol.CaptureManual, "", false); err != nil {
			t.Fatal(err)
		}
		// Sealed before the next one starts: only one capture runs at a time.
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) && s.capture.Load() != nil {
			time.Sleep(10 * time.Millisecond)
		}
	}
	entries, err := os.ReadDir(opts.Dir)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".zip") {
			count++
		}
	}
	if count != 2 {
		t.Errorf("%d bundles survived pruning, want 2", count)
	}
}

// Typed text is recorded as a shape by default. A bundle is a thing people send
// to each other, and it must not be a way to send a password.
func TestInputTextIsRedactedByDefault(t *testing.T) {
	s := newTestSession(t, defaultCaptureOptions(t))
	s.recordInput(1, &protocol.InputEvent{
		Kind: protocol.InText, Node: 7, Seq: 3, Text: "hunter2",
		Fields: map[string]string{"password": "hunter2"},
	})
	events := s.events.Events()
	if len(events) != 1 {
		t.Fatalf("input was not recorded")
	}
	blob, err := json.Marshal(events[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "hunter2") {
		t.Errorf("the typed text reached the timeline verbatim: %s", blob)
	}
	if !strings.Contains(string(blob), "7 chars") {
		t.Errorf("the shape of the typed text was lost: %s", blob)
	}
	// Field names stay: they say which control was submitted, which is the
	// diagnostic half of a form submission.
	if !strings.Contains(string(blob), "password") {
		t.Errorf("the field name was dropped along with its value: %s", blob)
	}
}

func TestInputTextKeptWhenTheOperatorAsks(t *testing.T) {
	opts := defaultCaptureOptions(t)
	opts.Text = true
	s := newTestSession(t, opts)
	s.recordInput(1, &protocol.InputEvent{Kind: protocol.InText, Text: "hunter2"})

	blob, err := json.Marshal(s.events.Events()[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(blob), "hunter2") {
		t.Errorf("captureText was on but the text was still redacted: %s", blob)
	}
}
