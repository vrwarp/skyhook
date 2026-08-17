package transport

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// hookScript writes a shell script that records how it was called, so the tests
// can assert on the contract an operator's own script has to satisfy.
func hookScript(t *testing.T, body string) (path, logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	path = filepath.Join(dir, "hook.sh")
	script := "#!/bin/sh\nLOG=" + logPath + "\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	return path, logPath
}

// The contract is what somebody writes a script against, so it is worth pinning
// exactly: an action, a name and a value, as arguments and in the environment.
func TestTheDNSHookIsToldWhatToPublishTwiceOver(t *testing.T) {
	hook, logPath := hookScript(t, `echo "args:$1 $2 $3" >> $LOG
echo "env:$SKYHOOK_ACME_ACTION $SKYHOOK_ACME_FQDN $SKYHOOK_ACME_VALUE" >> $LOG`)

	p, err := NewExecDNSProvider([]string{hook}, time.Minute, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := p.Present(ctx, "_acme-challenge.skyhook.example.com", "abc123"); err != nil {
		t.Fatalf("present: %v", err)
	}
	if err := p.CleanUp(ctx, "_acme-challenge.skyhook.example.com", "abc123"); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	want := `args:present _acme-challenge.skyhook.example.com abc123
env:present _acme-challenge.skyhook.example.com abc123
args:cleanup _acme-challenge.skyhook.example.com abc123
env:cleanup _acme-challenge.skyhook.example.com abc123
`
	if string(got) != want {
		t.Errorf("the hook was called as:\n%s\nwant:\n%s", got, want)
	}
}

// Fixed arguments come before the ones Skyhook adds, so a single script can
// serve several zones or several providers by dispatching on its own first
// argument.
func TestTheDNSHookKeepsItsOwnArgumentsFirst(t *testing.T) {
	hook, logPath := hookScript(t, `echo "$@" >> $LOG`)
	p, err := NewExecDNSProvider([]string{hook, "cloudflare", "--zone=example.com"},
		time.Minute, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Present(context.Background(), "_acme-challenge.example.com", "v"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(logPath)
	want := "cloudflare --zone=example.com present _acme-challenge.example.com v\n"
	if string(got) != want {
		t.Errorf("called with %q, want %q", got, want)
	}
}

// What the provider's API said is the most useful thing anyone could be shown
// at the moment a challenge fails, so it goes in the error rather than into a
// log line nobody correlates.
func TestAFailingDNSHookExplainsItselfInTheError(t *testing.T) {
	hook, _ := hookScript(t, `echo "cloudflare: invalid API token" >&2; exit 3`)
	p, err := NewExecDNSProvider([]string{hook}, time.Minute, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	err = p.Present(context.Background(), "_acme-challenge.example.com", "v")
	if err == nil {
		t.Fatal("a hook that exited 3 reported success")
	}
	if !strings.Contains(err.Error(), "invalid API token") {
		t.Errorf("error = %v, want the command's own message in it", err)
	}
	if !strings.Contains(err.Error(), "_acme-challenge.example.com") {
		t.Errorf("error = %v, want the record it failed on", err)
	}
}

// A command that is not there is a typo in the configuration, and the moment to
// say so is startup — not two minutes into an order with a record already
// published and an authority already waiting.
func TestAMissingDNSHookIsRefusedUpFront(t *testing.T) {
	if _, err := NewExecDNSProvider(
		[]string{"/nonexistent/skyhook-dns-hook"}, time.Minute, quietLog()); err == nil {
		t.Fatal("a command that does not exist was accepted")
	}
	if _, err := NewExecDNSProvider(nil, time.Minute, quietLog()); err == nil {
		t.Fatal("no command at all was accepted")
	}
}

// A hook that hangs must not hang the server with it — and killing the hook is
// not enough on its own. This script's `sleep` is a *child* holding the same
// stdout, which is the shape of every hook that shells out to curl: without a
// deadline on reading the pipe as well, the call blocks for the child's whole
// lifetime however short its own timeout was.
func TestADNSHookThatHangsIsCutOff(t *testing.T) {
	const childLives = 30 * time.Second
	hook, _ := hookScript(t, `sleep 30`)
	p, err := NewExecDNSProvider([]string{hook}, 300*time.Millisecond, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := p.Present(context.Background(), "_acme-challenge.example.com", "v"); err == nil {
		t.Fatal("a hook that never returned reported success")
	}
	if elapsed := time.Since(start); elapsed >= childLives-10*time.Second {
		t.Errorf("waited %s for a hook with a 300ms timeout: "+
			"this is waiting out the orphaned child, not the command", elapsed)
	}
}

// The settle wait is not decoration. Accepting a challenge the instant the
// provider's API returns is the classic way to have an authorization refused:
// the API has taken the change and the nameservers have not finished agreeing
// about it. With no server to ask, the wait is still served.
func TestPropagationWaitGivesUpRatherThanHanging(t *testing.T) {
	w := DNSWait{
		Timeout: 300 * time.Millisecond,
		Settle:  -1,
		// Nothing is listening here, so the record can never be seen.
		Resolvers: []string{"127.0.0.1:1"},
	}
	start := time.Now()
	_, err := w.forTXT(context.Background(), "_acme-challenge.example.com",
		[]string{"expected"}, quietLog())
	if err == nil {
		t.Fatal("a record that never appeared was reported as visible")
	}
	if !strings.Contains(err.Error(), "_acme-challenge.example.com") {
		t.Errorf("error = %v, want the record name in it", err)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("waited %s past a 300ms timeout", elapsed)
	}
}

// A cancelled context has to come back out, because the whole wait runs inside
// server startup.
func TestPropagationWaitHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := DNSWait{Timeout: time.Minute, Resolvers: []string{"127.0.0.1:1"}}
	if _, err := w.forTXT(ctx, "_acme-challenge.example.com", []string{"v"}, quietLog()); err == nil {
		t.Fatal("a cancelled wait reported success")
	}
}

// Every configured value has to be present, not just one of them: a host and a
// wildcard over it answer at the same record name, and a provider hook that
// replaces instead of appending leaves exactly one of them there.
func TestPropagationWaitWantsEveryValue(t *testing.T) {
	if !containsAll([]string{"a", "b"}, []string{"a", "b"}) {
		t.Error("both present should satisfy")
	}
	if containsAll([]string{"a"}, []string{"a", "b"}) {
		t.Error("a hook that overwrote the first value would have been accepted")
	}
	if !containsAll([]string{"a", "b", "c"}, []string{"b"}) {
		t.Error("other values at the same name are not a problem")
	}
}

// The settle sleep is the one part of the wait that runs even when there is
// nothing to ask, so it has to respect the deadline it was given.
func TestSleepStopsAtTheDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := sleep(ctx, time.Hour); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the deadline", err)
	}
	if err := sleep(context.Background(), time.Millisecond); err != nil {
		t.Errorf("a short sleep failed: %v", err)
	}
}

// The lookup goes to a nameserver of our choosing through a custom dialer, but
// the standard library's error still names whatever is in resolv.conf. Printing
// that under a line naming the server actually asked sends somebody debugging
// the one failure this diagnoses — a record published into the wrong zone — off
// to blame a resolver that was never consulted.
func TestALookupFailureDoesNotNameAResolverWeNeverAsked(t *testing.T) {
	err := &net.DNSError{
		Err:        "lame referral",
		Name:       "_acme-challenge.example.com",
		Server:     "8.8.8.8:53",
		IsNotFound: false,
	}
	got := dnsReason(err)
	if strings.Contains(got, "8.8.8.8") {
		t.Errorf("reason = %q, want the resolv.conf server left out", got)
	}
	if !strings.Contains(got, "lame referral") {
		t.Errorf("reason = %q, want the actual reason kept", got)
	}
	if got := dnsReason(&net.DNSError{Err: "no such host", IsNotFound: true}); !strings.Contains(got, "no such record") {
		t.Errorf("reason = %q, want a missing record said plainly", got)
	}
	if got := dnsReason(errors.New("something else")); got != "something else" {
		t.Errorf("reason = %q", got)
	}
}

// A hook that exits 0 and publishes nothing is the commonest broken one, and
// the self-test exists to catch exactly that. When there is no nameserver to
// ask — a name that is not delegated, usually a typo — nothing was checked, and
// reporting success would bless the hook this check was written to catch.
func TestTheHookSelfTestWillNotBlessWhatItCouldNotCheck(t *testing.T) {
	hook, _ := hookScript(t, `exit 0`)
	p, err := NewExecDNSProvider([]string{hook}, time.Minute, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	// .invalid is reserved and delegated nowhere, so no nameserver can be found.
	err = CheckDNSHook(context.Background(), p,
		DNSWait{Timeout: 10 * time.Second, Settle: -1}, "skyhook.invalid", quietLog())
	if err == nil {
		t.Fatal("a hook that published nothing was reported as working")
	}
	if !strings.Contains(err.Error(), "no nameserver was found") {
		t.Errorf("error = %v, want it to say what could not be checked", err)
	}
}
