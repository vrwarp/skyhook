package session

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vrwarp/skyhook/internal/diag"
	"github.com/vrwarp/skyhook/internal/mirror"
	"github.com/vrwarp/skyhook/internal/protocol"
)

// A capture is the one thing in Skyhook that deliberately spends the link.
//
// Everything else in this codebase is arranged around not sending bytes; a
// capture sends a screenshot in each direction because a rendering bug that
// nobody can see a picture of is a rendering bug nobody fixes. That trade is
// only defensible if it happens rarely and on purpose, which is what the rate
// limit, the byte budget and the "somebody asked for this" default are for.

// CaptureOptions configures diagnostic bundles.
type CaptureOptions struct {
	// Dir is where bundles are written. Empty disables captures.
	Dir string
	// Keep is how many bundles survive on disk.
	Keep int
	// MaxBytes caps one bundle's uncompressed contents.
	MaxBytes int64
	// ClientBytes caps what the plane side is asked to send up.
	ClientBytes int
	// Screenshots asks both halves for a picture.
	Screenshots bool
	// Text writes what the reader typed verbatim instead of as a digest.
	Text bool
	// OnDivergence takes a capture when the integrity check finds a mismatch.
	// Off unless an operator asks for it: it puts page content on disk without
	// anybody present having decided to.
	OnDivergence bool
	// Interval is the shortest gap between automatic captures.
	Interval time.Duration
	// JournalBytes bounds each tab's record of frames already sent.
	JournalBytes int
	// Wait is how long the server holds a bundle open for the plane side. It is
	// generous on purpose: this link's round trips are seconds, and a bundle
	// with no plane-side half is a bundle with no bug in it.
	Wait time.Duration
	// Logs is the server's recent log, teed here at startup.
	Logs *diag.Ring
}

// Enabled reports whether captures are configured.
func (o CaptureOptions) Enabled() bool { return o.Dir != "" && o.Keep > 0 }

func (o CaptureOptions) wait() time.Duration {
	if o.Wait <= 0 {
		return 90 * time.Second
	}
	return o.Wait
}

func (o CaptureOptions) clientBytes() int {
	if o.ClientBytes <= 0 {
		return 4 << 20
	}
	return o.ClientBytes
}

// ErrCapturesDisabled means this server was told not to write bundles.
var ErrCapturesDisabled = errors.New("session: captures are disabled")

// ErrCaptureBusy means one is already in flight. Two captures at once would
// interleave two clients' answers into one zip, and the second would mostly be
// a picture of the first one running.
var ErrCaptureBusy = errors.New("session: a capture is already in progress")

// ErrCaptureThrottled means an automatic capture came too soon after the last.
var ErrCaptureThrottled = errors.New("session: capture throttled")

// capture is one bundle being assembled.
type capture struct {
	id     string
	reason string
	note   string
	start  time.Time
	bundle *diag.Bundle
	sess   *Session
	// frozen is the evidence that does not survive being looked at later.
	frozen []frozenTab
	// asked records whether a client was there to ask.
	asked bool

	mu       sync.Mutex
	partial  map[string][]byte
	received int
	files    int
	planeErr []string

	closeOnce sync.Once
	planeDone chan struct{}
}

// frozenTab is everything about a tab that a repair would destroy.
//
// The capture that matters most is the one taken at a divergence, and the very
// next thing the server does about a divergence is resync the tab — which
// starts a new snapshot, empties the frame journal, and replaces the client's
// diverged document with a correct one. Gathering any of that afterwards
// produces a beautiful bundle describing a mirror that is working.
type frozenTab struct {
	id           uint32
	entries      []JournalEntry
	complete     bool
	dropped      int
	acked        uint64
	clientHash   uint64
	seq          uint64
	url, title   string
	ringFrames   int
	ringBytes    int
	journalBound bool
	// resyncDropped is how many repeat resync requests this tab absorbed. A
	// client asking faster than the link can answer is a real condition with no
	// other trace: the storm it used to cause is gone, and so is the evidence.
	resyncDropped int
}

// freezeTabs copies the perishable state of every tab, right now.
func (s *Session) freezeTabs() []frozenTab {
	s.mu.Lock()
	states := make(map[uint32]*tabState, len(s.tabs))
	for id, ts := range s.tabs {
		states[id] = ts
	}
	s.mu.Unlock()

	out := make([]frozenTab, 0, len(states))
	for id, ts := range states {
		entries, complete := ts.journal.Entries()
		f := frozenTab{
			id: id, entries: entries, complete: complete, dropped: ts.journal.Dropped(),
			seq: ts.tab.Seq(), url: ts.tab.URL(), title: ts.tab.Title(),
			ringFrames: ts.ring.Len(), ringBytes: ts.ring.Bytes(),
			journalBound: ts.journal.Enabled(),
		}
		s.mu.Lock()
		f.acked, f.clientHash = ts.acked, ts.lastHash
		f.resyncDropped = ts.resyncDropped
		s.mu.Unlock()
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

func newCaptureID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return time.Now().UTC().Format("20060102-150405.000")
	}
	return time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}

// StartCapture opens a bundle, asks the plane side for its half, and gathers
// the landside half while it waits. It returns as soon as the bundle exists;
// the work happens on its own goroutine, because the caller is usually the
// frame dispatch loop and a capture takes as long as the link does.
func (s *Session) StartCapture(reason, note string, automatic bool) (string, error) {
	opts := s.mgr.opts.Capture
	if !opts.Enabled() {
		return "", ErrCapturesDisabled
	}
	if automatic && !s.mgr.allowAutoCapture(opts.Interval) {
		return "", ErrCaptureThrottled
	}
	id := newCaptureID()
	b, err := diag.NewBundle(opts.Dir, id, opts.MaxBytes)
	if err != nil {
		return "", err
	}
	c := &capture{
		id: id, reason: reason, note: note, start: time.Now(),
		bundle: b, sess: s, partial: map[string][]byte{},
		planeDone: make(chan struct{}),
	}
	if !s.capture.CompareAndSwap(nil, c) {
		b.Abort()
		return "", ErrCaptureBusy
	}
	// Before anything that takes time, and before returning to a caller whose
	// next act may be to repair the very thing being captured.
	c.frozen = s.freezeTabs()

	// Likewise the request to the plane side: it is queued on the ctrl channel,
	// which outranks the dom channel a resync would use, so the client hears
	// "capture" before it hears "here is a new document".
	if s.Online() {
		c.asked = true
		s.Send(protocol.ChCtrl, protocol.TypeCapture, 0, protocol.CaptureRequest{
			ID: c.id, Reason: c.reason, Note: c.note, Tabs: c.tabIDs(),
			MaxBytes: opts.clientBytes(), Screenshots: opts.Screenshots,
		})
	}

	s.log.Info("capture started", "capture", id, "reason", reason, "automatic", automatic)
	s.events.Add("capture", 0, map[string]any{"id": id, "reason": reason})
	go c.run()
	return id, nil
}

// captureDivergence opens a bundle for a mismatch the integrity check found.
// It is best effort by design: a capture that cannot be taken must never stop
// the resync that repairs the tab.
func (s *Session) captureDivergence(tab uint32) {
	if !s.mgr.opts.Capture.OnDivergence {
		// Said here rather than only in the documentation, because this line
		// lands beside the divergence itself — which is where an operator is
		// looking, and the moment they would have wanted the bundle.
		s.log.Info("no capture taken: set captureOnDivergence to bundle both halves "+
			"the next time this happens", "tab", tab)
		return
	}
	note := fmt.Sprintf("automatic: the integrity check found tab %d holding a "+
		"different document plane-side and landside", tab)
	if _, err := s.StartCapture(protocol.CaptureDivergence, note, true); err != nil &&
		!errors.Is(err, ErrCaptureThrottled) && !errors.Is(err, ErrCapturesDisabled) &&
		!errors.Is(err, ErrCaptureBusy) {
		s.log.Warn("divergence capture failed to start", "tab", tab, "err", err)
	}
}

func (c *capture) tabIDs() []uint32 {
	out := make([]uint32, 0, len(c.frozen))
	for _, f := range c.frozen {
		out = append(out, f.id)
	}
	return out
}

// run drives one capture to a sealed zip. Nothing in here is allowed to fail
// the whole bundle: an artifact that cannot be gathered becomes a note, because
// a bundle that is missing the screenshot is still a bundle with the frames in
// it.
func (c *capture) run() {
	s := c.sess
	opts := s.mgr.opts.Capture
	defer s.capture.CompareAndSwap(c, nil)

	// The plane side was asked before this goroutine started. It has to
	// serialise a document, rasterise it and push the result up a slow link, so
	// it is the long pole; the landside gathering here runs while that is
	// happening rather than after it.
	if !c.asked {
		c.bundle.Note("no client was connected: this bundle is the landside half only")
	}

	c.gatherLandside()

	if c.asked {
		select {
		case <-c.planeDone:
		case <-time.After(opts.wait()):
			c.bundle.Note("the plane side did not finish within %s; "+
				"its artifacts are whatever arrived before the deadline", opts.wait())
		case <-s.closed:
			c.bundle.Note("the session closed before the plane side answered")
		}
	}
	c.flushPartials()

	c.mu.Lock()
	files, received, planeErrs := c.files, c.received, append([]string(nil), c.planeErr...)
	c.mu.Unlock()
	for _, e := range planeErrs {
		c.bundle.Note("plane side: %s", e)
	}

	_ = c.bundle.AddJSON("manifest.json", c.manifest(files, received))

	path, size, err := c.bundle.Close()
	if err != nil {
		s.log.Error("capture could not be sealed", "capture", c.id, "err", err)
		s.Send(protocol.ChCtrl, protocol.TypeCaptureDone, 0, protocol.CaptureDone{
			ID: c.id, Error: err.Error(),
		})
		return
	}
	s.log.Info("capture written", "capture", c.id, "path", path, "bytes", size,
		"planeFiles", files, "took", time.Since(c.start).Round(time.Millisecond))
	if err := diag.Prune(opts.Dir, opts.Keep); err != nil {
		s.log.Warn("capture pruning failed", "err", err)
	}
	s.Send(protocol.ChCtrl, protocol.TypeCaptureDone, 0, protocol.CaptureDone{
		ID: c.id, Path: path, Bytes: size,
	})
}

// manifest is the first thing anybody opening a bundle reads: what this is,
// when it was taken, what asked for it, and which halves are actually in it.
func (c *capture) manifest(planeFiles, planeBytes int) map[string]any {
	s := c.sess
	clientApp, clientBuild := s.Client()
	served := s.mgr.clientApp.Stamp()
	return map[string]any{
		"id":              c.id,
		"reason":          c.reason,
		"note":            c.note,
		"takenAt":         c.start.UTC().Format(time.RFC3339Nano),
		"tookMs":          time.Since(c.start).Milliseconds(),
		"protocolVersion": protocol.Version,
		"serverVersion":   Version(),
		// Which plane-side build drew the half of this bundle that came up the
		// link, and which one the server would have served. When they differ,
		// that is the first thing to know about a mirror that looks wrong: the
		// patcher in the bundle is not the patcher in the tree.
		"clientApp":         clientApp,
		"clientBuild":       clientBuild,
		"servedClientBuild": served.Build,
		"agentHash":         mirror.AgentHash(),
		"goVersion":         runtime.Version(),
		"platform":          runtime.GOOS + "/" + runtime.GOARCH,
		"sessionId":         s.ID,
		"tabs":              c.tabIDs(),
		"clientOnline":      s.Online(),
		"planeSide": map[string]any{
			"files": planeFiles,
			"bytes": planeBytes,
		},
		"textRedacted": !s.mgr.opts.Capture.Text,
		"readMe": "landside/ is what the server had; planeside/ is what the client " +
			"showed. tabs/<id>/expected.html is the client's document reconstructed " +
			"from the frames actually sent — diff it against planeside/tabs/<id>/mirror.html. " +
			"If the documents agree and the styling does not, read " +
			"landside/tabs/<id>/css-rejected.txt: the rules that were deliberately not " +
			"sent. Read both screenshot.json files before comparing the pictures — the " +
			"two halves photograph different regions at different scales.",
	}
}

// ------------------------------------------------------------------ landside

func (c *capture) gatherLandside() {
	s := c.sess
	opts := s.mgr.opts.Capture

	if opts.Logs != nil {
		if log := opts.Logs.Text(); len(log) > 0 {
			_ = c.bundle.Add("server.log", log)
		} else {
			c.bundle.Note("the server log ring was empty")
		}
	}
	_ = c.bundle.AddJSON("session/session.json", s.report())
	_ = c.bundle.AddJSON("session/events.json", map[string]any{
		"dropped": s.events.Dropped(),
		"events":  s.events.Events(),
	})

	if s.mgr.browser != nil {
		brCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		browser := map[string]any{"attached": s.mgr.browser.Attached()}
		if v, err := s.mgr.browser.Version(brCtx); err == nil {
			browser["product"] = v
		} else {
			browser["versionError"] = err.Error()
		}
		cancel()
		_ = c.bundle.AddJSON("landside/browser.json", browser)
	}

	for i := range c.frozen {
		c.gatherTab(&c.frozen[i])
	}
}

func (c *capture) gatherTab(f *frozenTab) {
	s := c.sess
	s.mu.Lock()
	ts := s.tabs[f.id]
	s.mu.Unlock()

	base := fmt.Sprintf("landside/tabs/%d", f.id)
	report := map[string]any{
		"tab":        f.id,
		"url":        f.url,
		"title":      f.title,
		"seq":        f.seq,
		"acked":      f.acked,
		"clientHash": f.clientHash,
		"ringFrames": f.ringFrames,
		"ringBytes":  f.ringBytes,
		// Zero is the ordinary answer. Anything large says the client was
		// asking for repairs faster than the link could deliver them.
		"resyncDropped": f.resyncDropped,
	}
	fail := map[string]string{}

	// The live page is asked for its side of the story. A resync running
	// alongside this does not change what it says: a resync re-serialises the
	// same document, it does not alter it.
	if ts != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		if h, err := ts.tab.DocHash(ctx); err != nil {
			fail["docHash"] = err.Error()
		} else {
			report["serverHash"] = h
			// clientHash is what the client reported for the last frame it
			// acknowledged; this one is the page as it stands now. Those are
			// the same document only when the client is caught up, and a
			// bundle that claims they disagree when it is merely behind sends
			// the reader after a divergence that was never there.
			if f.acked == f.seq {
				report["hashesAgree"] = h == f.clientHash
			} else {
				report["hashesComparable"] = false
				c.bundle.Note("tab %d: the client had acknowledged frame %d of %d when this "+
					"was taken, so its hash and the live page's describe different instants; "+
					"the bundle claims no agreement either way", f.id, f.acked, f.seq)
			}
		}
		if d, err := ts.tab.AgentDiag(ctx); err != nil {
			fail["agent"] = err.Error()
		} else {
			_ = c.bundle.Add(base+"/agent.json", d)
		}
		// The used-CSS filter's other half. Without it a rule dropped in error
		// and a rule the site never wrote are the same artifact: nothing.
		if rej, err := ts.tab.RejectedCSS(ctx); err != nil {
			fail["rejectedCss"] = err.Error()
		} else {
			report["cssSeen"] = rej.Seen
			report["cssRejected"] = rej.Rejected
			header := fmt.Sprintf(
				"# %d of %d style rules matched nothing in this document and were not sent.\n"+
					"# One selector per line, from the filter's most recent pass.\n",
				rej.Rejected, rej.Seen)
			if rej.Truncated {
				header += fmt.Sprintf("# Capped at %d; the rest are not listed.\n", len(rej.Selectors))
			}
			_ = c.bundle.AddText(base+"/css-rejected.txt",
				header+strings.Join(rej.Selectors, "\n")+"\n")
		}
		if sheets, err := ts.tab.SheetStatus(ctx); err != nil {
			fail["sheets"] = err.Error()
		} else {
			report["sheetsRecovered"] = sheets.Recovered
			report["sheetsBlocked"] = sheets.Blocked
			if len(sheets.Blocked) > 0 {
				c.bundle.Note("tab %d: %d stylesheet(s) could not be read at all — "+
					"cross-origin, and not recovered over the protocol either. Any rule "+
					"missing from this page may simply never have arrived", f.id, len(sheets.Blocked))
			}
		}
		if fp, err := ts.tab.Fingerprint(ctx, 20000); err != nil {
			fail["fingerprint"] = err.Error()
		} else {
			_ = c.bundle.Add(base+"/fingerprint.json", fp)
		}
		if html, err := ts.tab.PageHTML(ctx); err != nil {
			fail["pageHtml"] = err.Error()
		} else {
			_ = c.bundle.AddText(base+"/page.html", html)
		}
		cancel()

		if s.mgr.opts.Capture.Screenshots {
			shotCtx, shotCancel := context.WithTimeout(context.Background(), 30*time.Second)
			shot, err := ts.tab.Screenshot(shotCtx, "webp", 70)
			shotCancel()
			switch {
			case err != nil:
				fail["screenshot"] = err.Error()
			case len(shot.Data) == 0:
				fail["screenshot"] = "the browser returned an empty image"
			default:
				_ = c.bundle.Add(base+"/screenshot."+shot.Format, shot.Data)
				// What the picture covers travels with it. The two halves of a
				// bundle photograph different regions at different scales, and
				// the reader's first instinct is to diff them.
				_ = c.bundle.AddJSON(base+"/screenshot.json", shot)
				if shot.Covers != "page" {
					c.bundle.Note("tab %d: the landside picture is the %dx%d viewport of a "+
						"%dpx-tall page, not the whole document: past %dpx a full-page render "+
						"is how a headless browser runs out of memory",
						f.id, shot.Width, shot.Height, shot.PageHeight, mirror.MaxShotHeight)
				}
			}
		}
	} else {
		c.bundle.Note("tab %d closed while the capture was being taken; only the "+
			"frames it had already sent are in this bundle", f.id)
	}

	c.writeJournal(base, f, report)
	if len(fail) > 0 {
		report["errors"] = fail
		for what, err := range fail {
			c.bundle.Note("tab %d: %s could not be gathered: %s", f.id, what, err)
		}
	}
	_ = c.bundle.AddJSON(base+"/state.json", report)
}

// writeJournal stores the frames this tab actually sent, and — when the record
// is complete — the document those frames add up to.
//
// That reconstruction is the point of the whole exercise. The client's document
// is not knowable from the server, but the client's document *as the frames
// specify it* is, and a mirror bug is precisely a disagreement between the two.
func (c *capture) writeJournal(base string, f *frozenTab, report map[string]any) {
	entries, complete := f.entries, f.complete
	report["journalFrames"] = len(entries)
	report["journalDropped"] = f.dropped
	report["journalComplete"] = complete
	if len(entries) == 0 {
		if f.journalBound {
			c.bundle.Note("%s: no frames were journalled (nothing had been sent yet)", base)
		} else {
			c.bundle.Note("%s: frame journalling is off (journalBytes is 0), so this "+
				"bundle cannot say what the client was actually sent", base)
		}
		return
	}

	index := make([]map[string]any, 0, len(entries))
	model := mirror.NewModel()
	replayOK := complete
	var replayErr string

	for i, e := range entries {
		raw, err := protocol.Marshal(e.Frame)
		if err != nil {
			continue
		}
		name := fmt.Sprintf("%s/frames/%04d-%s.cbor", base, i, frameName(e.Frame.Type))
		_ = c.bundle.Add(name, raw)

		row := map[string]any{
			"i":     i,
			"at":    e.At.UTC().Format(time.RFC3339Nano),
			"type":  frameName(e.Frame.Type),
			"seq":   e.Frame.Seq,
			"base":  e.Frame.Base,
			"bytes": e.Size,
		}
		if e.Frame.Cause != 0 {
			row["causedByInput"] = e.Frame.Cause
		}
		switch e.Frame.Type {
		case protocol.TypeSnapshot:
			var snap protocol.Snapshot
			if err := e.Frame.DecodeBody(&snap); err == nil {
				row["nodes"] = len(snap.Nodes)
				row["strings"] = len(snap.Strings)
				row["cssRules"] = len(snap.CSS)
				row["url"] = snap.URL
				if replayOK {
					if err := model.ApplySnapshot(&snap); err != nil {
						replayOK, replayErr = false, err.Error()
					}
				}
			}
		case protocol.TypeMutation:
			var mut protocol.Mutation
			if err := e.Frame.DecodeBody(&mut); err == nil {
				row["ops"] = len(mut.Ops)
				row["opKinds"] = opHistogram(mut.Ops)
				if mut.Flush {
					row["flush"] = true
				}
				if replayOK {
					if err := model.ApplyMutation(&mut, e.Frame.Seq); err != nil {
						replayOK, replayErr = false, err.Error()
					}
				}
			}
		}
		index = append(index, row)
	}
	_ = c.bundle.AddJSON(base+"/frames/index.json", index)

	switch {
	case !complete:
		c.bundle.Note("%s: the frame journal had to drop %d frame(s), so the client's "+
			"document cannot be reconstructed from it; expected.html is not in this bundle",
			base, f.dropped)
	case !replayOK:
		report["replayError"] = replayErr
		c.bundle.Note("%s: replaying the journalled frames failed (%s). That is itself a "+
			"finding: the server sent a stream its own replica cannot apply", base, replayErr)
	default:
		_ = c.bundle.AddText(base+"/expected.html", model.HTML())
		_ = c.bundle.AddText(base+"/expected.css", model.Stylesheet())
		report["expectedHash"] = model.Hash()
		report["expectedNodes"] = len(model.Nodes)
	}
}

func frameName(t protocol.Type) string {
	switch t {
	case protocol.TypeSnapshot:
		return "snapshot"
	case protocol.TypeMutation:
		return "mutation"
	default:
		return fmt.Sprintf("type%d", t)
	}
}

func opHistogram(ops []protocol.Op) map[string]int {
	names := map[uint8]string{
		protocol.OpInsert: "insert", protocol.OpRemove: "remove", protocol.OpAttr: "attr",
		protocol.OpText: "text", protocol.OpMove: "move", protocol.OpSplice: "splice",
		protocol.OpStyle: "style", protocol.OpImage: "image", protocol.OpFocus: "focus",
		protocol.OpScroll: "scroll", protocol.OpDocInfo: "docinfo",
	}
	out := map[string]int{}
	for i := range ops {
		name, ok := names[ops[i].Op]
		if !ok {
			name = fmt.Sprintf("op%d", ops[i].Op)
		}
		out[name]++
	}
	return out
}

// report summarises the session itself: what it is, how long it has been up,
// and what the link has been doing.
func (s *Session) report() map[string]any {
	s.mu.Lock()
	caps := make([]string, 0, len(s.caps))
	for c := range s.caps {
		caps = append(caps, c)
	}
	vp := s.viewport
	tabCount := len(s.tabs)
	lastSeen := s.lastSeen
	s.mu.Unlock()
	sort.Strings(caps)

	stats := s.Stats()
	return map[string]any{
		"sessionId": s.ID,
		"createdAt": s.created.UTC().Format(time.RFC3339),
		"ageSec":    int(time.Since(s.created).Seconds()),
		"lastSeen":  lastSeen.UTC().Format(time.RFC3339),
		"online":    s.Online(),
		"caps":      caps,
		"adapters":  s.AdapterNames(),
		"activeTab": s.activeTab.Load(),
		"tabCount":  tabCount,
		"viewport": map[string]any{
			"w": vp.W, "h": vp.H, "dpr": vp.DPR, "mobile": vp.Mobile,
		},
		"stats": map[string]any{
			"rttMicros":  stats.RTTMicros,
			"queueDepth": stats.QueueDepth,
			"bytesSent":  stats.BytesSent,
			"bytesRecv":  stats.BytesRecv,
			"lossPct":    stats.LossPct,
		},
	}
}

// ----------------------------------------------------------------- plane side

// maxPlaneArtifact caps one plane-side file. It is enforced here rather than
// trusted to the client: a bundle is written by the server, and the server does
// not let a peer decide how much of its disk to use.
const maxPlaneArtifact = 24 << 20

// CapturePart takes one artifact (or a chunk of one) from the plane side.
//
// A part for a bundle that has already been sealed is dropped quietly. It is
// the ordinary consequence of a capture timing out on a slow link, and the one
// thing that must not happen is an error frame per chunk going back down: a
// stale screenshot is thirty chunks, and thirty error frames on a link that was
// already too slow to finish the capture is how a diagnostic becomes an outage.
func (s *Session) CapturePart(part protocol.CapturePart) error {
	c := s.capture.Load()
	if c == nil || c.id != part.ID {
		s.log.Debug("dropping a capture part for a bundle that is no longer open",
			"capture", part.ID, "name", part.Name)
		return nil
	}
	c.add(part)
	return nil
}

func (c *capture) add(part protocol.CapturePart) {
	// complete is the reassembled artifact when this part finished one, and nil
	// while chunks are still arriving. It is resolved under the lock and stored
	// outside it, because storing means compressing into the zip and there is no
	// reason to hold the reassembly state while that happens.
	var complete []byte
	finished, oversize := false, false

	c.mu.Lock()
	if part.Error != "" {
		c.planeErr = append(c.planeErr, part.Error)
	}
	if part.Name != "" {
		buf := append(c.partial[part.Name], part.Data...)
		c.received += len(part.Data)
		switch {
		case len(buf) > maxPlaneArtifact:
			delete(c.partial, part.Name)
			oversize = true
		case part.More:
			c.partial[part.Name] = buf
		default:
			delete(c.partial, part.Name)
			c.files++
			// finished rather than a nil check on complete: an artifact the
			// client could genuinely produce nothing for still belongs in the
			// bundle, as evidence that it produced nothing.
			complete, finished = buf, true
		}
	}
	c.mu.Unlock()

	switch {
	case oversize:
		c.bundle.Note("plane side sent more than %d bytes for %q; it was dropped",
			maxPlaneArtifact, part.Name)
	case finished:
		c.store(part.Name, complete)
	}
	if part.Done {
		c.closeOnce.Do(func() { close(c.planeDone) })
	}
}

// store writes one finished plane-side artifact into the bundle.
//
// A name ending in ".gz" means the client compressed it before sending — which
// it does for anything textual, because a mirrored document is 90% air and this
// is the one path in Skyhook where the client is the one paying for bytes. The
// bundle stores the plain file: a zip full of gzip members is a zip nobody can
// read without unpacking it twice.
func (c *capture) store(name string, data []byte) {
	if strings.HasSuffix(name, ".gz") {
		plain, err := gunzip(data)
		if err != nil {
			c.bundle.Note("plane-side artifact %q did not decompress (%v); "+
				"stored as-is", name, err)
			_ = c.bundle.Add("planeside/"+name, data)
			return
		}
		name, data = strings.TrimSuffix(name, ".gz"), plain
	}
	_ = c.bundle.Add("planeside/"+name, data)
}

// flushPartials keeps half-arrived artifacts rather than discarding them: the
// first 40 kB of a mirrored document is still evidence, and a capture cut short
// by an outage is exactly the case worth having evidence from.
func (c *capture) flushPartials() {
	c.mu.Lock()
	names := make([]string, 0, len(c.partial))
	for name := range c.partial {
		names = append(names, name)
	}
	sort.Strings(names)
	partial := c.partial
	c.partial = map[string][]byte{}
	c.mu.Unlock()

	for _, name := range names {
		c.bundle.Note("plane-side artifact %q arrived only partially (%d bytes)",
			name, len(partial[name]))
		c.store(name+".partial", partial[name])
	}
}

func gunzip(data []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()
	out, err := io.ReadAll(io.LimitReader(zr, maxPlaneArtifact))
	if err != nil {
		return nil, err
	}
	return out, nil
}

// recordInput puts one input event on the session's timeline.
//
// What the reader did is the reproduction steps, and no amount of DOM state
// substitutes for it — "the field diverged" and "the field diverged after these
// six keystrokes" are different bug reports. What they typed is a different
// question: it is sometimes a password, so the default is its shape, and the
// contents are an operator's explicit choice (captureText).
func (s *Session) recordInput(tab uint32, ev *protocol.InputEvent) {
	detail := map[string]any{"kind": ev.Kind, "seq": ev.Seq}
	if ev.Node != 0 {
		detail["node"] = ev.Node
	}
	if ev.Key != "" {
		detail["key"] = ev.Key
	}
	if ev.Modifiers != 0 {
		detail["modifiers"] = ev.Modifiers
	}
	if ev.ExpectSeq != 0 {
		detail["expectSeq"] = ev.ExpectSeq
	}
	if ev.Hold != 0 {
		detail["holdMs"] = ev.Hold
	}
	// Where in the node the pointer actually was, in permille of its box. A
	// click that landed on the edge of a control is a plausible reason for the
	// landside replay to have hit something else.
	if len(ev.Point) > 0 {
		detail["point"] = ev.Point
	}
	if ev.Text != "" {
		if s.mgr.opts.Capture.Text {
			detail["text"] = ev.Text
		} else {
			detail["text"] = diag.Redact(ev.Text)
		}
	}
	if len(ev.Fields) > 0 {
		names := make([]string, 0, len(ev.Fields))
		for k := range ev.Fields {
			names = append(names, k)
		}
		sort.Strings(names)
		// Field names, never values: a form submission is the single most
		// likely place for a bundle to pick up a credential.
		detail["fields"] = names
	}
	s.events.Add("input", tab, detail)
}

// ------------------------------------------------------------- rate limiting

// allowAutoCapture spaces automatic captures out. A page that diverges once
// usually diverges every thirty seconds until it is navigated away from, and a
// bundle per divergence would be a bundle per thirty seconds — filling a disk
// with thirty copies of one bug.
func (m *Manager) allowAutoCapture(interval time.Duration) bool {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	m.captureMu.Lock()
	defer m.captureMu.Unlock()
	if !m.lastAutoCapture.IsZero() && time.Since(m.lastAutoCapture) < interval {
		return false
	}
	m.lastAutoCapture = time.Now()
	return true
}

// captureSlot is the session's single in-flight capture.
type captureSlot = atomic.Pointer[capture]
