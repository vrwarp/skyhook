package e2e

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/protocol"
)

/*
A file the reader picks reaches the page's chooser (P-007).

A file input was a dead end twice over: the replayed click asked headless
Chromium for a dialog it does not have, and the mirrored input opened the
reader's own picker into a document whose value can never cross. Now the
landside chooser is intercepted, the ask crosses as TypeFileAsk naming the
mirrored input, the bytes come back chunked on bulk, and setFileInputFiles
fires the change event the page was waiting for — this test's page reads the
file's own text, so the assertion is on bytes reaching page JavaScript.

The second input pins the other half of the ask vocabulary (multiple) and the
cancel path: a dismissed picker leaves the page exactly where a dismissed
chooser does, and deletes what it staged.
*/
func TestAFileReachesThePagesChooser(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	if err := cl.OpenTab(h.site.URL + "/upload"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "no file yet", budget(45*time.Second)); err != nil {
		t.Fatalf("mirror never delivered the page: %v", err)
	}

	pick := cl.Model(tab).Find("input", "id", "pick")
	if pick == nil {
		t.Fatal("no file input in the mirrored page")
	}
	if err := cl.Click(tab, pick.ID); err != nil {
		t.Fatalf("click: %v", err)
	}

	// The interception turns the click into an ask instead of a dialog.
	ask := awaitAsk(t, cl, tab, 1)
	if ask.Multiple {
		t.Error("a single-file input asked for many")
	}
	if ask.Node != pick.ID {
		t.Errorf("the ask names node %d, the input is %d", ask.Node, pick.ID)
	}

	// The answer: one small file, and the page reading it back is the pin.
	content := "hello from the plane"
	if err := cl.Upload(tab, ask.ID, "notes.txt", "text/plain", []byte(content)); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if err := cl.WaitForText(ctx, tab,
		"received notes.txt (20 bytes): hello from the plane", budget(30*time.Second)); err != nil {
		t.Fatalf("the page never read the file: %v", err)
	}

	// The staged copy stays while the page might still read it.
	if n := stagedAsks(t, h.uploadDir); n != 1 {
		t.Errorf("expected 1 staged ask while the tab lives, found %d", n)
	}

	// The multiple input, dismissed: the page sees nothing and the staging
	// is deleted.
	many := cl.Model(tab).Find("input", "id", "many")
	if many == nil {
		t.Fatal("no multiple input in the mirrored page")
	}
	if err := cl.Click(tab, many.ID); err != nil {
		t.Fatalf("click: %v", err)
	}
	ask2 := awaitAsk(t, cl, tab, 2)
	if !ask2.Multiple {
		t.Error("a multiple input asked for one")
	}
	if err := cl.CancelUpload(tab, ask2.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	deadline := time.Now().Add(budget(15 * time.Second))
	for time.Now().Before(deadline) && stagedAsks(t, h.uploadDir) != 1 {
		time.Sleep(100 * time.Millisecond)
	}
	if n := stagedAsks(t, h.uploadDir); n != 1 {
		t.Errorf("a dismissed ask left its staging behind: %d dirs", n)
	}

	// Closing the tab is when the page can no longer read anything: the
	// reader's files go with it.
	if err := cl.CloseTab(tab); err != nil {
		t.Fatalf("close tab: %v", err)
	}
	deadline = time.Now().Add(budget(20 * time.Second))
	for time.Now().Before(deadline) && stagedAsks(t, h.uploadDir) != 0 {
		time.Sleep(100 * time.Millisecond)
	}
	if n := stagedAsks(t, h.uploadDir); n != 0 {
		t.Errorf("the closed tab left %d staged ask(s) behind", n)
	}
}

// awaitAsk waits for the nth file ask on a tab.
func awaitAsk(t *testing.T, cl interface {
	FileAsks(uint32) []protocol.FileAsk
}, tab uint32, n int) protocol.FileAsk {
	t.Helper()
	deadline := time.Now().Add(budget(30 * time.Second))
	for time.Now().Before(deadline) {
		if asks := cl.FileAsks(tab); len(asks) >= n {
			return asks[n-1]
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("file ask %d never arrived", n)
	return protocol.FileAsk{}
}

// stagedAsks counts ask directories in the upload staging dir.
func stagedAsks(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read staging dir: %v", err)
	}
	return len(entries)
}
