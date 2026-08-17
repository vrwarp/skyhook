package e2e

import (
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/vrwarp/skyhook/internal/diag"
)

/*
Where a test's server log goes, now that tests run at the same time.

Eight concurrent tests writing slog records straight to os.Stderr produced one
undebuggable stream: 1716 of 2637 lines in a CI run were server log with
nothing on them to say which test they belonged to, interleaved line by line
with seven other tests. `go test` could not help, because it only groups output
that goes through t.Log — anything written directly to stderr is outside its
knowledge.

Volume made it worse. The level was tied to testing.Verbose(), so -v — which CI
passes to get test names and per-test timings — also turned on DEBUG, and DEBUG
is mostly Chromium repeating that it cannot reach a dbus that does not exist on
a headless runner. 1411 DEBUG and 302 INFO lines buried 3 WARN lines that were
the only ones anybody wanted.

So the split is by audience rather than by level alone:

  - The ring keeps everything, at DEBUG, exactly as it did. It is bounded and
    mutex-guarded, Capture bundles it, and busy_test reads it, so it is left
    byte-identical — the test= attribute below goes on the stderr handler
    rather than on the logger for that reason.

  - stderr carries WARN and worse, which was three lines for a 99-test run, and
    tags them with the test they came from. Something going wrong live is still
    visible while the suite runs.

  - A test that fails prints its own ring through t.Logf, which `go test` shows
    under that test and nowhere else — contiguous, attributed, and complete
    down to DEBUG. A test that passes prints nothing at all.

The dump is registered before the harness's other cleanups so that it runs
after them, and so carries the shutdown records too. Writing to a ring from a
goroutine that outlives the test is safe, which is why the ring is dumped at
cleanup rather than the log being routed through t.Log directly: t.Log after a
test completes panics, and this harness has background goroutines that outlive
every test body.

A hung test still prints nothing here, and that is what `go test -timeout` and
its goroutine dump are for.
*/

// liveLevel is the level that reaches stderr while the suite runs. WARN keeps
// it to a few lines a run; SKYHOOK_TEST_LOG=debug puts the old firehose back
// for working on a single test.
func liveLevel(t *testing.T) slog.Level {
	t.Helper()
	spec := strings.TrimSpace(os.Getenv("SKYHOOK_TEST_LOG"))
	if spec == "" {
		return slog.LevelWarn
	}
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(spec)); err != nil {
		t.Fatalf("SKYHOOK_TEST_LOG=%q is not a level (debug, info, warn, error): %v", spec, err)
	}
	return lv
}

// testLogger builds the logger a harness hands to the server, and returns the
// ring behind it so the harness can pass it to Capture. The caller gets the
// failure dump registered for free.
func testLogger(t *testing.T) (*slog.Logger, *diag.Ring) {
	t.Helper()
	ring := diag.NewRing(500)

	// The attribute is attached to this handler rather than to the logger, so
	// that what lands in the ring — which Capture bundles and busy_test reads —
	// is unchanged by any of this.
	live := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: liveLevel(t)}).
		WithAttrs([]slog.Attr{slog.String("test", t.Name())})

	// Teed the way skyhookd does it, so a capture taken by these tests carries
	// a server log — and so the tee itself is exercised.
	log := slog.New(diag.Tee(
		live,
		slog.NewTextHandler(ring, &slog.HandlerOptions{Level: slog.LevelDebug}),
	))

	dumpOnFailure(t, ring)
	return log, ring
}

// dumpOnFailure prints the whole ring under a test that failed, and nothing
// under one that did not.
func dumpOnFailure(t *testing.T, ring *diag.Ring) {
	t.Helper()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		text := strings.TrimRight(string(ring.Text()), "\n")
		if text == "" {
			return
		}
		// One Logf rather than one per line: t.Log stamps its caller's file and
		// line on every call, and that would be this file, ninety times over.
		t.Logf("server log for %s (last %d records, DEBUG and up):\n%s",
			t.Name(), len(ring.Lines()), text)
	})
}

// The ring is what Capture bundles and what busy_test reads, so what lands in
// it has to be exactly what landed in it before any of this. In particular the
// test= attribute belongs to the stderr handler alone: putting it on the logger
// would have written it into every captured record too.
func TestTheRingIsUnchangedByTheLiveStream(t *testing.T) {
	t.Parallel()
	log, ring := testLogger(t)

	log.Debug("a debug record", "k", "v")
	log.Warn("a warn record")

	text := string(ring.Text())
	if !strings.Contains(text, "a debug record") {
		t.Errorf("the ring dropped a DEBUG record, so a capture would too:\n%s", text)
	}
	if !strings.Contains(text, "a warn record") {
		t.Errorf("the ring dropped a WARN record:\n%s", text)
	}
	if strings.Contains(text, "test=") {
		t.Errorf("the live stream's test= attribute reached the ring, which changes "+
			"what Capture bundles and what busy_test reads:\n%s", text)
	}
}

// DEBUG has to keep reaching the ring whatever the live level is, because the
// ring is the whole of what a failing test prints.
func TestTheRingKeepsDebugWhateverStderrIsSetTo(t *testing.T) {
	t.Setenv("SKYHOOK_TEST_LOG", "error")
	log, ring := testLogger(t)
	log.Debug("still recorded")
	if !strings.Contains(string(ring.Text()), "still recorded") {
		t.Error("raising the live level silenced the ring, which would leave a " +
			"failing test with nothing to print")
	}
}

func TestLiveLevelReadsTheEnvironment(t *testing.T) {
	for _, tc := range []struct {
		set  string
		want slog.Level
	}{
		{"", slog.LevelWarn}, // the default: three lines for a 99-test run
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{" info ", slog.LevelInfo},
		{"error", slog.LevelError},
	} {
		t.Run("SKYHOOK_TEST_LOG="+tc.set, func(t *testing.T) {
			t.Setenv("SKYHOOK_TEST_LOG", tc.set)
			if got := liveLevel(t); got != tc.want {
				t.Errorf("liveLevel() = %v, want %v", got, tc.want)
			}
		})
	}
}
