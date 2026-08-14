package diag_test

import (
	"archive/zip"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/diag"
)

func readBundle(t *testing.T, path string) map[string]string {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("open bundle: %v", err)
	}
	defer func() { _ = zr.Close() }()
	out := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %s: %v", f.Name, err)
		}
		data, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("read %s: %v", f.Name, err)
		}
		out[f.Name] = string(data)
	}
	return out
}

func TestBundleRoundTrip(t *testing.T) {
	dir := t.TempDir()
	b, err := diag.NewBundle(dir, "20260101-000000-abcd", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.AddText("landside/tabs/1/page.html", "<html></html>"); err != nil {
		t.Fatal(err)
	}
	if err := b.AddJSON("manifest.json", map[string]any{"id": "abcd"}); err != nil {
		t.Fatal(err)
	}
	b.Note("the plane side never answered")

	path, size, err := b.Close()
	if err != nil {
		t.Fatal(err)
	}
	if size == 0 {
		t.Fatal("bundle has no size")
	}
	if !strings.HasSuffix(path, ".zip") {
		t.Fatalf("bundle kept its working name: %s", path)
	}
	// The .part file is what a reader must never find: it means "still being
	// written", and a bundle that keeps it after Close is one that looks
	// unfinished forever.
	if _, err := os.Stat(path + ".part"); !os.IsNotExist(err) {
		t.Fatalf("the partial file survived Close: %v", err)
	}

	files := readBundle(t, path)
	if files["landside/tabs/1/page.html"] != "<html></html>" {
		t.Errorf("page.html did not survive: %q", files["landside/tabs/1/page.html"])
	}
	if !strings.Contains(files["manifest.json"], `"id": "abcd"`) {
		t.Errorf("manifest did not survive: %q", files["manifest.json"])
	}
	if !strings.Contains(files["NOTES.txt"], "the plane side never answered") {
		t.Errorf("notes did not survive: %q", files["NOTES.txt"])
	}
}

// A bundle that quietly drops an artifact is worse than one that says it did:
// the reader would conclude the artifact was never produced.
func TestBundleBudgetIsNoted(t *testing.T) {
	b, err := diag.NewBundle(t.TempDir(), "budget", 64)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.AddText("small.txt", "tiny"); err != nil {
		t.Fatal(err)
	}
	if err := b.AddText("huge.bin", strings.Repeat("x", 4096)); err != nil {
		t.Fatal(err)
	}
	path, _, err := b.Close()
	if err != nil {
		t.Fatal(err)
	}
	files := readBundle(t, path)
	if _, ok := files["huge.bin"]; ok {
		t.Error("an over-budget artifact was stored anyway")
	}
	if files["small.txt"] != "tiny" {
		t.Error("the artifact that fit was not stored")
	}
	if !strings.Contains(files["NOTES.txt"], "huge.bin") {
		t.Errorf("the omission was not explained: %q", files["NOTES.txt"])
	}
}

func TestBundleRefusesPathTraversal(t *testing.T) {
	b, err := diag.NewBundle(t.TempDir(), "traversal", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.AddText("../../etc/passwd", "nope"); err != nil {
		t.Fatal(err)
	}
	path, _, err := b.Close()
	if err != nil {
		t.Fatal(err)
	}
	for name := range readBundle(t, path) {
		if strings.Contains(name, "..") || strings.HasPrefix(name, "/") {
			t.Errorf("entry escapes the archive: %q", name)
		}
	}
}

// Two artifacts landing on one name is a bug worth seeing in the bundle, not
// one worth hiding by losing one of them.
func TestBundleKeepsBothOnNameCollision(t *testing.T) {
	b, err := diag.NewBundle(t.TempDir(), "collide", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	_ = b.AddText("state.json", "first")
	_ = b.AddText("state.json", "second")
	path, _, err := b.Close()
	if err != nil {
		t.Fatal(err)
	}
	files := readBundle(t, path)
	if files["state.json"] != "first" {
		t.Errorf("the first artifact was lost: %q", files["state.json"])
	}
	if files["state-1.json"] != "second" {
		t.Errorf("the second artifact was lost: %v", files)
	}
}

func TestPruneKeepsTheNewest(t *testing.T) {
	dir := t.TempDir()
	for i, name := range []string{"a", "b", "c", "d"} {
		p := filepath.Join(dir, name+diag.Ext)
		if err := os.WriteFile(p, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		// Ordered in time so "newest" is unambiguous.
		when := time.Now().Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
	}
	stale := filepath.Join(dir, "old.zip.part")
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	if err := diag.Prune(dir, 2); err != nil {
		t.Fatal(err)
	}
	left := map[string]bool{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		left[e.Name()] = true
	}
	if len(left) != 2 || !left["c.zip"] || !left["d.zip"] {
		t.Errorf("prune kept the wrong bundles: %v", left)
	}
}

func TestPruneOnMissingDirectoryIsFine(t *testing.T) {
	if err := diag.Prune(filepath.Join(t.TempDir(), "nope"), 5); err != nil {
		t.Errorf("pruning a directory that does not exist should be a no-op: %v", err)
	}
}

func TestRingKeepsTheLastLines(t *testing.T) {
	r := diag.NewRing(3)
	for _, line := range []string{"one", "two", "three", "four", "five"} {
		if _, err := r.Write([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	got := r.Lines()
	want := []string{"three", "four", "five"}
	if len(got) != len(want) {
		t.Fatalf("ring holds %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ring holds %v, want %v", got, want)
		}
	}
}

// The ring exists to be pointed at by a slog handler, so the shape that matters
// is a real logger writing real records through it.
func TestTeeFeedsBothHandlers(t *testing.T) {
	ring := diag.NewRing(50)
	var stderr strings.Builder
	log := slog.New(diag.Tee(
		slog.NewTextHandler(&stderr, &slog.HandlerOptions{Level: slog.LevelWarn}),
		slog.NewTextHandler(ring, &slog.HandlerOptions{Level: slog.LevelDebug}),
	))
	log.With("tab", 3).Debug("resync by replay", "frames", 2)
	log.Warn("mirror divergence", "tab", 3)

	lines := strings.Join(ring.Lines(), "\n")
	if !strings.Contains(lines, "resync by replay") || !strings.Contains(lines, "tab=3") {
		t.Errorf("the ring missed the debug record (and its attributes): %q", lines)
	}
	if !strings.Contains(lines, "mirror divergence") {
		t.Errorf("the ring missed the warning: %q", lines)
	}
	// The whole point of the debug-level ring: what stderr suppressed is
	// exactly what a capture wants.
	if strings.Contains(stderr.String(), "resync by replay") {
		t.Errorf("stderr should have suppressed the debug record: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "mirror divergence") {
		t.Errorf("stderr missed the warning: %q", stderr.String())
	}
}

func TestRedactHidesTheValueAndKeepsTheShape(t *testing.T) {
	a := diag.Redact("hunter2")
	if strings.Contains(a, "hunter2") {
		t.Fatalf("redaction leaked the value: %s", a)
	}
	if !strings.Contains(a, "7 chars") {
		t.Errorf("redaction lost the length: %s", a)
	}
	if diag.Redact("hunter2") != a {
		t.Error("redaction is not stable, so two occurrences cannot be matched up")
	}
	if diag.Redact("hunter3") == a {
		t.Error("two different values redacted to the same thing")
	}
	if diag.Redact("") != "" {
		t.Error("an empty string should stay empty rather than become a digest")
	}
}
