package e2e

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/protocol"
)

// reportBytes is the download fixture: 200 kB of deterministic pattern, big
// enough to cross as several bulk chunks and small enough to not be the test's
// wall clock.
func reportBytes() []byte {
	out := make([]byte, 200<<10)
	for i := range out {
		out[i] = byte(i*7 + i>>8)
	}
	return out
}

/*
A download lands on the server and crosses the link only on ask (P-108).

Clicking a link the origin answers with Content-Disposition used to end with
the file writing itself onto the VPS and the reader seeing nothing happen at
all. Now the landing is announced with a name and a size, the bytes cross the
link chunked on the bulk channel only when the client asks — from any offset,
so a second device or an interrupted fetch pays only for what it is missing —
and a discard deletes the landside copy.

This clicks the real link in the mirrored page, so it also pins the whole
route: click replay, Chromium turning the navigation into a download,
Browser.setDownloadBehavior putting the file where the shelf expects it.
*/
func TestDownloadsLandAndCrossOnAsk(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	if err := cl.OpenTab(h.site.URL + "/dl"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "get the report", budget(45*time.Second)); err != nil {
		t.Fatalf("mirror never delivered the page: %v", err)
	}
	link := cl.Model(tab).Find("a", "id", "get")
	if link == nil {
		t.Fatal("no download link in the mirrored page")
	}
	if err := cl.Click(tab, link.ID); err != nil {
		t.Fatalf("click: %v", err)
	}

	// The shelf announces the landing and then the file being ready, without
	// the client asking for anything.
	want := reportBytes()
	var dl protocol.Download
	deadline := time.Now().Add(budget(45 * time.Second))
	for time.Now().Before(deadline) {
		for _, d := range cl.Downloads() {
			if d.Name == "flight-report.bin" {
				dl = d
			}
		}
		if dl.State == protocol.DownloadReady {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if dl.State != protocol.DownloadReady {
		t.Fatalf("the download never became ready; shelf holds %+v", cl.Downloads())
	}
	if dl.Total != int64(len(want)) {
		t.Errorf("the announcement says %d bytes, the origin served %d", dl.Total, len(want))
	}
	landed := filepath.Join(h.downloadDir, dl.ID)
	if _, err := os.Stat(landed); err != nil {
		t.Fatalf("the announced file is not on the shelf: %v", err)
	}

	// Nothing crossed yet: announcements are control frames, the bytes wait.
	if data, _ := cl.DownloadData(dl.ID); len(data) != 0 {
		t.Fatalf("%d bytes crossed before anybody asked", len(data))
	}

	// The ask, and the whole file, chunked and in order.
	if err := cl.FetchDownload(dl.ID, 0); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	var got []byte
	var done bool
	deadline = time.Now().Add(budget(60 * time.Second))
	for time.Now().Before(deadline) {
		if got, done = cl.DownloadData(dl.ID); done {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !done {
		t.Fatalf("the fetch never finished: %d of %d bytes, err=%q",
			len(got), len(want), cl.DownloadErr(dl.ID))
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the fetched bytes are not the origin's: got %d bytes, want %d", len(got), len(want))
	}

	// A second device sees the same shelf on connect — the announcements are
	// state, re-told to every client — and can fetch just the tail it wants.
	cl2 := h.connect(ctx, "")
	defer func() { _ = cl2.Close() }()
	deadline = time.Now().Add(budget(30 * time.Second))
	for time.Now().Before(deadline) {
		if d, ok := cl2.Downloads()[dl.ID]; ok && d.State == protocol.DownloadReady {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if d, ok := cl2.Downloads()[dl.ID]; !ok || d.State != protocol.DownloadReady {
		t.Fatalf("a fresh client was not told about the shelf; it holds %+v", cl2.Downloads())
	}
	tail := int64(len(want) - 50<<10)
	if err := cl2.FetchDownload(dl.ID, tail); err != nil {
		t.Fatalf("tail fetch: %v", err)
	}
	deadline = time.Now().Add(budget(45 * time.Second))
	done = false
	for time.Now().Before(deadline) {
		if got, done = cl2.DownloadData(dl.ID); done {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !done {
		t.Fatalf("the tail fetch never finished: %d bytes, err=%q", len(got), cl2.DownloadErr(dl.ID))
	}
	if !bytes.Equal(got, want[tail:]) {
		t.Fatalf("the tail is not the file's: got %d bytes from %d", len(got), tail)
	}

	// Asking for a download the shelf does not hold answers on the same
	// surface the bytes would have used, so the transfer hears, not a log.
	if err := cl.FetchDownload("no-such-file", 0); err != nil {
		t.Fatalf("fetch of a missing id: %v", err)
	}
	deadline = time.Now().Add(budget(20 * time.Second))
	for time.Now().Before(deadline) {
		if cl.DownloadErr("no-such-file") != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if cl.DownloadErr("no-such-file") == "" {
		t.Error("a fetch of a missing download never came back with its refusal")
	}

	// Discard deletes the landside copy and tells every client.
	if err := cl.DiscardDownload(dl.ID); err != nil {
		t.Fatalf("discard: %v", err)
	}
	deadline = time.Now().Add(budget(30 * time.Second))
	for time.Now().Before(deadline) {
		d1 := cl.Downloads()[dl.ID]
		d2 := cl2.Downloads()[dl.ID]
		if d1.State == protocol.DownloadGone && d2.State == protocol.DownloadGone {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if d := cl2.Downloads()[dl.ID]; d.State != protocol.DownloadGone {
		t.Errorf("the other client still believes the file is %s", d.State)
	}
	if _, err := os.Stat(landed); !os.IsNotExist(err) {
		t.Errorf("the discarded file is still on the shelf: %v", err)
	}
}
