package appver_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/appver"
)

func write(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, appver.StampFile), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReadsTheStamp(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, `{"version":"0.1.0","build":"abc123","built":"1700000000"}`)

	got := appver.NewReader(dir).Stamp()
	if got.Version != "0.1.0" || got.Build != "abc123" {
		t.Fatalf("stamp = %+v", got)
	}
	if !got.Known() {
		t.Error("a stamp with a build id reported itself unknown")
	}
	if got.String() != "0.1.0 (abc123)" {
		t.Errorf("String() = %q", got.String())
	}
}

// A deploy replaces the web root under a running server. A stamp read once at
// startup would then describe an app that is no longer there — and would go on
// telling every client it was current, or stale, forever after.
func TestRereadsAfterADeploy(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, `{"version":"0.1.0","build":"before"}`)
	r := appver.NewReader(dir)
	if got := r.Stamp().Build; got != "before" {
		t.Fatalf("build = %q", got)
	}

	// Same size, different contents, and possibly the same second: the check has
	// to notice a modification time that moved, so the write is stamped forward
	// rather than trusted to land on a different tick.
	write(t, dir, `{"version":"0.1.0","build":"aafter"}`)
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(dir, appver.StampFile), future, future); err != nil {
		t.Fatal(err)
	}
	if got := r.Stamp().Build; got != "aafter" {
		t.Fatalf("after a deploy the reader still said %q", got)
	}
}

// Silence is the only safe answer to "which build do you serve" when the file
// is missing or unreadable: a blank build id is never compared, and a client
// that cannot be told is left alone rather than told it is out of date.
func TestUnknownRatherThanWrong(t *testing.T) {
	cases := map[string]func(t *testing.T) string{
		"no file": func(t *testing.T) string { return t.TempDir() },
		"not json": func(t *testing.T) string {
			dir := t.TempDir()
			write(t, dir, "<!DOCTYPE html>")
			return dir
		},
		"no build id": func(t *testing.T) string {
			dir := t.TempDir()
			write(t, dir, `{"version":"0.1.0"}`)
			return dir
		},
		"no web root": func(_ *testing.T) string { return "" },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			if got := appver.NewReader(setup(t)).Stamp(); got.Known() {
				t.Fatalf("stamp = %+v, want unknown", got)
			}
		})
	}
}

// A nil reader stands in for a manager built without a web root at all.
func TestNilReaderIsUnknown(t *testing.T) {
	var r *appver.Reader
	if r.Stamp().Known() {
		t.Fatal("a nil reader claimed to know a build")
	}
}
