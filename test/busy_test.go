package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/session"
)

// tickerPage changes a text node several times a second, for ever, with nobody
// touching it. A news front page, a feed, a stock ticker: the ordinary case,
// and the one where the landside document and the last frame the client
// acknowledged are never the same document.
const tickerPage = `<!DOCTYPE html><html><head><title>Ticker</title></head>
<body>
  <h1>the ticker</h1>
  <p id="tick">0</p>
<script>
  var n = 0;
  setInterval(function () {
    n++;
    document.getElementById('tick').textContent = 'tick ' + n;
  }, 30);
</script>
</body></html>`

/*
A page that keeps changing is not a page that has gone wrong.

The integrity check used to compare the client's hash for the last frame it
acknowledged against the landside page as it stood at the moment of asking. On
anything that changes faster than the link's round trip those are two different
documents by construction, so the check reported a divergence every time it ran
— and each one cost a resync, which competed with the traffic that had made the
client late to begin with.
*/
func TestABusyPageIsNotMistakenForADivergedOne(t *testing.T) {
	h := newHarnessWith(t, func(o *session.ManagerOptions) {
		// The real cadence is thirty seconds. Turned down, a handful of checks
		// happen while the test watches rather than after it has finished.
		o.IntegrityInterval = 2 * time.Second
	})
	ctx, cancel := context.WithTimeout(context.Background(), budget(180*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	tab := h.openPage(ctx, cl, "/ticker", "the ticker")
	if err := cl.WaitForText(ctx, tab, "tick ", budget(30*time.Second)); err != nil {
		t.Fatalf("the page never started ticking in the mirror: %v", err)
	}
	// Long enough for several checks to run against a document that has moved
	// on between every one of them.
	time.Sleep(budget(12 * time.Second))

	logs := string(h.logs.Text())
	if strings.Contains(logs, "mirror divergence") {
		t.Errorf("a page that was merely busy was reported as diverged, and resynced for it:\n%s",
			divergenceLines(logs))
	}
	// Without this the test would also pass if the check had never run at all.
	if !strings.Contains(logs, "integrity check passed") {
		t.Fatalf("the integrity check never reached a conclusion about a healthy tab:\n%s",
			integrityLines(logs))
	}
}

func divergenceLines(logs string) string { return grepLines(logs, "divergence") }

func integrityLines(logs string) string { return grepLines(logs, "integrity check") }

func grepLines(logs, want string) string {
	var out []string
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, want) {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		return "(the log says nothing about it)"
	}
	return strings.Join(out, "\n")
}
