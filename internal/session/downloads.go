package session

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/vrwarp/skyhook/internal/protocol"
)

/*
Downloads (P-108).

A click on a download link used to end with the file writing itself onto the
VPS and nobody told: Chromium downloaded it wherever its profile pointed, the
mirror showed nothing, and the reader concluded the link was broken. The other
option — proxying the bytes straight over the link — would spend minutes of a
250 kbps channel on a file the reader may only have wanted to know the size of.

So a download lands on the server first, at datacenter speed, into a directory
of our own with each file named by Chromium's GUID for it (allowAndName: the
suggested filename is display data, never a path). Every state change is
announced to every session — the shelf belongs to the user, not to a tab — and
the bytes cross the link only when the reader asks, with the size in front of
them, chunked on the bulk channel where they cannot head-of-line-block a page,
resumable from any offset, and stoppable mid-flight.

The shelf lives in memory and its files are wiped at startup and by the kill
switch: a download fetched with the reader's cookies is the reader's, and this
host is a relay for it, not an archive of it.
*/

// downloadKeep is how many downloads the shelf remembers. Beyond it the oldest
// finished entry — bytes and all — makes room; whatever is still landing is
// never pruned out from under Chromium.
const downloadKeep = 20

// downloadChunk is how much of a fetched file rides in one bulk frame. The
// same figure as a capture chunk, for the same reason: a frame is indivisible
// on the wire, and at 250 kbps a 32 kB one occupies the link for about a
// second, which is as long as an acknowledgement can be asked to wait.
const downloadChunk = 32 << 10

// downloadWindow is how many chunks a fetch may have queued and unsent. It is
// what paces the file reader to the link instead of to the disk: past this,
// the stream waits for the writer rather than piling megabytes into a queue.
const downloadWindow = 8

// download is one shelf entry. lastSent throttles progress announcements:
// landside progress moves at datacenter speed, and every tick of it is not
// worth a control frame.
type download struct {
	protocol.Download
	lastSent time.Time
}

/*
EnableDownloads points the browser's downloads at dir and starts relaying
their state. Called once at startup; until it is, the shelf is nil and
downloads keep their old P-108 behavior.

Files already in dir are deleted first. The shelf is memory, so a file from
the previous run is one no client can ever ask for again — and one this host
has no business keeping.
*/
func (m *Manager) EnableDownloads(ctx context.Context, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			_ = os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
	m.dlMu.Lock()
	m.downloadDir = dir
	m.downloads = map[string]*download{}
	m.dlMu.Unlock()
	// Handlers before the enabling call, so no event can fall in between.
	m.browser.OnDownload(m.downloadBegan, m.downloadMoved)
	return m.browser.EnableDownloads(ctx, dir)
}

// downloadBegan is the browser telling us a page started a download.
func (m *Manager) downloadBegan(guid, rawURL, name string) {
	// The GUID becomes a filename in our directory. Chromium's are hex-and-
	// dashes; anything else is not a download we can safely hold.
	if guid == "" || guid == "." || guid == ".." || strings.ContainsAny(guid, `/\`) {
		return
	}
	m.dlMu.Lock()
	if m.downloads == nil {
		m.dlMu.Unlock()
		return
	}
	if _, ok := m.downloads[guid]; ok {
		m.dlMu.Unlock()
		return
	}
	d := &download{Download: protocol.Download{
		ID: guid, URL: rawURL, Name: safeFilename(name),
		State: protocol.DownloadLanding,
	}, lastSent: time.Now()}
	m.downloads[guid] = d
	m.dlOrder = append(m.dlOrder, guid)
	pruned := m.pruneDownloadsLocked()
	body := d.Download
	m.dlMu.Unlock()
	m.log.Info("download landing", "id", guid, "name", body.Name, "url", rawURL)
	m.announceDownload(body)
	for _, p := range pruned {
		m.announceDownload(p)
	}
}

// downloadMoved is the browser reporting bytes arriving, or the end of them.
func (m *Manager) downloadMoved(guid string, total, received int64, state string) {
	m.dlMu.Lock()
	d := m.downloads[guid]
	if d == nil || d.State == protocol.DownloadGone {
		m.dlMu.Unlock()
		return
	}
	d.Total, d.Received = total, received
	was := d.State
	switch state {
	case "completed":
		d.State = protocol.DownloadReady
		if d.Total < received {
			// The origin never said, or lied; what landed is the size.
			d.Total = received
		}
	case "canceled":
		d.State = protocol.DownloadFailed
	}
	if d.State == was && time.Since(d.lastSent) < time.Second {
		m.dlMu.Unlock()
		return
	}
	d.lastSent = time.Now()
	body := d.Download
	dir := m.downloadDir
	m.dlMu.Unlock()
	if body.State == protocol.DownloadFailed {
		// A canceled download can leave its partial bytes behind.
		_ = os.Remove(filepath.Join(dir, guid))
	}
	if body.State != was {
		m.log.Info("download "+body.State, "id", guid, "name", body.Name, "bytes", body.Received)
	}
	m.announceDownload(body)
}

// pruneDownloadsLocked drops the oldest finished entries once the shelf is
// over downloadKeep, files and all, and returns their "gone" announcements
// for the caller to send once the lock is off.
func (m *Manager) pruneDownloadsLocked() []protocol.Download {
	var gone []protocol.Download
	for len(m.dlOrder) > downloadKeep {
		victim := -1
		for i, id := range m.dlOrder {
			if d := m.downloads[id]; d == nil || d.State != protocol.DownloadLanding {
				victim = i
				break
			}
		}
		if victim < 0 {
			return gone // everything is still landing; over-full beats interfering
		}
		id := m.dlOrder[victim]
		m.dlOrder = append(m.dlOrder[:victim], m.dlOrder[victim+1:]...)
		if d := m.downloads[id]; d != nil {
			delete(m.downloads, id)
			if d.State != protocol.DownloadGone {
				_ = os.Remove(filepath.Join(m.downloadDir, id))
				d.State = protocol.DownloadGone
				gone = append(gone, d.Download)
			}
		}
	}
	return gone
}

// announceDownload tells every session. The shelf is the user's, not a tab's:
// whichever device is connected gets to know a file is waiting.
func (m *Manager) announceDownload(d protocol.Download) {
	for _, s := range m.Sessions() {
		s.Send(protocol.ChCtrl, protocol.TypeDownload, 0, d)
	}
}

// sendDownloads catches one client up on the shelf, oldest first. Called on
// every attach: the announcements a client missed while offline are state, and
// state is re-told rather than replayed.
func (m *Manager) sendDownloads(s *Session) {
	m.dlMu.Lock()
	list := make([]protocol.Download, 0, len(m.dlOrder))
	for _, id := range m.dlOrder {
		if d := m.downloads[id]; d != nil && d.State != protocol.DownloadGone {
			list = append(list, d.Download)
		}
	}
	m.dlMu.Unlock()
	for _, d := range list {
		s.Send(protocol.ChCtrl, protocol.TypeDownload, 0, d)
	}
}

// downloadFile resolves a ready download to the file holding its bytes.
func (m *Manager) downloadFile(id string) (string, protocol.Download, error) {
	m.dlMu.Lock()
	defer m.dlMu.Unlock()
	d := m.downloads[id]
	if d == nil {
		return "", protocol.Download{}, fmt.Errorf("session: no download %q on the shelf", id)
	}
	if d.State != protocol.DownloadReady {
		return "", protocol.Download{}, fmt.Errorf("session: download %q is %s, not ready", id, d.State)
	}
	return filepath.Join(m.downloadDir, id), d.Download, nil
}

// DiscardDownload deletes one download's bytes and tells everyone. A download
// still landing is cancelled in the browser first.
func (m *Manager) DiscardDownload(id string) error {
	m.dlMu.Lock()
	d := m.downloads[id]
	if d == nil {
		m.dlMu.Unlock()
		return fmt.Errorf("session: no download %q on the shelf", id)
	}
	landing := d.State == protocol.DownloadLanding
	d.State = protocol.DownloadGone
	d.lastSent = time.Now()
	body := d.Download
	dir := m.downloadDir
	m.dlMu.Unlock()
	if landing {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = m.browser.CancelDownload(ctx, id)
		cancel()
	}
	_ = os.Remove(filepath.Join(dir, id))
	m.log.Info("download discarded", "id", id, "name", body.Name)
	m.announceDownload(body)
	return nil
}

// WipeDownloads empties the shelf, files and all. The kill switch calls it:
// the files were fetched with the reader's cookies and are theirs to destroy.
func (m *Manager) WipeDownloads() {
	m.dlMu.Lock()
	dir := m.downloadDir
	if m.downloads != nil {
		m.downloads = map[string]*download{}
	}
	m.dlOrder = nil
	m.dlMu.Unlock()
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}

// safeFilename reduces an origin-suggested filename to something safe to
// display and to save under: the basename, control characters out, bounded.
func safeFilename(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	if name == "." || name == string(filepath.Separator) {
		name = ""
	}
	if len(name) > 128 {
		name = name[:128]
	}
	if name == "" {
		name = "download"
	}
	return name
}

// ------------------------------------------------------------------ fetching

// downloadCmd answers a client's ask about one download.
func (s *Session) downloadCmd(cmd protocol.DownloadCmd) error {
	switch cmd.Cmd {
	case "fetch":
		s.fetchDownload(cmd.ID, cmd.Offset)
		return nil
	case "stop":
		s.stopDownloadSend(cmd.ID)
		return nil
	case "discard":
		s.stopDownloadSend(cmd.ID)
		return s.mgr.DiscardDownload(cmd.ID)
	default:
		return fmt.Errorf("session: unknown download command %q", cmd.Cmd)
	}
}

// fetchDownload starts streaming one landed file to this client. Refusals —
// the wrong state, a vanished file — travel as an Err part rather than a
// generic error frame, so the client's transfer, not just its log, hears.
func (s *Session) fetchDownload(id string, offset int64) {
	path, d, err := s.mgr.downloadFile(id)
	if err != nil {
		s.Send(protocol.ChBulk, protocol.TypeDownloadPart, 0,
			protocol.DownloadPart{ID: id, Err: err.Error()})
		return
	}
	s.dlMu.Lock()
	if s.dlSends == nil {
		s.dlSends = map[string]context.CancelFunc{}
	}
	if _, busy := s.dlSends[id]; busy {
		s.dlMu.Unlock()
		return // already on its way; those parts answer this ask too
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.dlSends[id] = cancel
	s.dlMu.Unlock()
	s.events.Add("download-fetch", 0, map[string]any{"id": id, "name": d.Name, "from": offset})
	go s.sendDownload(ctx, id, path, offset)
}

// stopDownloadSend ends a fetch mid-flight. The client keeps what has arrived
// and can fetch again from that offset.
func (s *Session) stopDownloadSend(id string) {
	s.dlMu.Lock()
	cancel := s.dlSends[id]
	s.dlMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

/*
sendDownload streams one file as bulk parts, at most downloadWindow chunks
queued at a time.

The window is the flow control. A slot is taken before each chunk is queued
and given back by the writer's onSent, so a client that stops draining — or a
link that goes away — parks this goroutine on the slot acquire rather than
letting it pile the whole file into the outbound queue. The session resuming
its writer resumes the stream, across reconnects, with nothing re-sent.

Parts and the final Done travel the same per-tab FIFO, so order on the wire is
the order here. A chunk the queue refused or dropped is a hole the client
cannot repair from later parts; the stream says so once, with an Err part, and
stops — fetching again resumes from the client's own count of what arrived.
*/
func (s *Session) sendDownload(ctx context.Context, id, path string, offset int64) {
	defer func() {
		s.dlMu.Lock()
		delete(s.dlSends, id)
		s.dlMu.Unlock()
	}()
	fail := func(msg string) {
		s.Send(protocol.ChBulk, protocol.TypeDownloadPart, 0,
			protocol.DownloadPart{ID: id, Err: msg})
	}
	f, err := os.Open(path) //nolint:gosec // inside our own downloads dir
	if err != nil {
		fail("the file is no longer on the server")
		return
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		fail(err.Error())
		return
	}
	size := st.Size()
	if offset < 0 || offset > size {
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		fail(err.Error())
		return
	}

	slots := make(chan struct{}, downloadWindow)
	var undelivered atomic.Bool
	for off := offset; off < size; {
		select {
		case <-ctx.Done():
			return
		case <-s.closed:
			return
		case slots <- struct{}{}:
		}
		if undelivered.Load() {
			fail("the link dropped a chunk; fetch again to resume")
			return
		}
		want := size - off
		if want > downloadChunk {
			want = downloadChunk
		}
		buf := make([]byte, want)
		if _, err := io.ReadFull(f, buf); err != nil {
			fail(err.Error())
			return
		}
		frame, err := protocol.NewFrame(protocol.TypeDownloadPart, 0,
			protocol.DownloadPart{ID: id, Off: off, Data: buf})
		if err != nil {
			fail(err.Error())
			return
		}
		msg, err := s.codec.EncodeFrame(protocol.ChBulk, frame)
		if err != nil {
			fail(err.Error())
			return
		}
		if !s.enqueue(outbound{
			ch: protocol.ChBulk, tab: 0, msg: msg,
			onSent: func(ok bool) {
				if !ok {
					undelivered.Store(true)
				}
				<-slots
			},
		}, false) {
			<-slots
			fail("the link would not take the bytes; fetch again to resume")
			return
		}
		off += int64(len(buf))
	}
	select {
	case <-ctx.Done():
		return
	case <-s.closed:
		return
	default:
	}
	s.Send(protocol.ChBulk, protocol.TypeDownloadPart, 0,
		protocol.DownloadPart{ID: id, Done: true, Size: size})
}
