package cdp

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

/*
The port Chromium actually bound, read from where Chromium writes it.

This exists instead of picking a number, because picking one means binding a
socket, closing it and handing the number over — and the gap between the close
and Chromium's bind is wide enough to lose on a machine running eight browsers
and three servers per test. The file is the only account of the port that cannot
be stale by the time it is read.
*/
func TestTheActivePortIsReadFromTheProfile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "DevToolsActivePort")

	if _, err := readActivePort(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a profile with no port file yet gave %v, want a not-exist", err)
	}

	// Chromium creates the file and then writes it, so a read can land on it
	// empty or half-written. Both are "come back", not "this browser is broken".
	for _, half := range []string{"", "\n", "455"} {
		if err := os.WriteFile(path, []byte(half), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readActivePort(path); half == "455" {
			if err != nil {
				t.Errorf("a complete first line without its newline was refused: %v", err)
			}
		} else if err == nil {
			t.Errorf("a half-written file (%q) was read as a port", half)
		}
	}

	if err := os.WriteFile(path,
		[]byte("45123\n/devtools/browser/6f0e-4c1a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readActivePort(path)
	if err != nil {
		t.Fatalf("reading a written port: %v", err)
	}
	if got != 45123 {
		t.Errorf("port = %d, want the 45123 on the first line", got)
	}
}

/*
A browser that dies is reported as having died.

Launch used to spend the whole forty-five second deadline dialling a port
nothing was listening on and then report the silence, which says nothing about
why. The cause is usually worth knowing and sometimes is the whole story — an
out-of-memory kill on a loaded runner says "signal: killed" here and said
"chromium did not expose devtools" before.
*/
func TestLaunchSaysWhenTheBrowserDiedRatherThanWaitingOutTheDeadline(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "not-a-browser")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 3\n"), 0o700); err != nil { //nolint:gosec // a test fixture that must be executable
		t.Fatal(err)
	}

	start := time.Now()
	_, err := Launch(context.Background(), BrowserOptions{
		ExecPath:    bin,
		UserDataDir: t.TempDir(),
		Headless:    true,
		Logger:      slog.New(slog.DiscardHandler),
	})
	if err == nil {
		t.Fatal("a binary that exits at once launched successfully")
	}
	if !strings.Contains(err.Error(), "exited before it was ready") {
		t.Errorf("error = %v, want it to say the browser exited", err)
	}
	// The deadline is forty-five seconds. Anything near it means the exit was
	// not noticed and the dial loop ran its course.
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("noticing a dead browser took %v, want it to be immediate", took)
	}
}

/*
Last flight's port is not this flight's port.

A profile that has been used before still holds the number from the run that
used it. Reading that is the same bug as choosing a number, with an older number
in it, so the file is cleared before the browser is started and the only thing
that can be read afterwards is what this browser wrote.
*/
func TestAStaleActivePortIsClearedBeforeLaunch(t *testing.T) {
	profile := t.TempDir()
	stale := filepath.Join(profile, "DevToolsActivePort")
	if err := os.WriteFile(stale, []byte("9222\n/devtools/browser/old\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(t.TempDir(), "not-a-browser")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 3\n"), 0o700); err != nil { //nolint:gosec // a test fixture that must be executable
		t.Fatal(err)
	}
	_, _ = Launch(context.Background(), BrowserOptions{
		ExecPath:    bin,
		UserDataDir: profile,
		Headless:    true,
		Logger:      slog.New(slog.DiscardHandler),
	})

	if _, err := os.Stat(stale); !errors.Is(err, fs.ErrNotExist) {
		t.Error("the previous run's port file survived a launch; a dial against " +
			"it would reach whatever holds that port now, or nothing at all")
	}
}

/*
The browser's opinion of the machine is not the log.

A headless Chromium writes dozens of lines about the absent system bus and the
absent GPU in its first second, on every machine this runs on, and none of them
has ever been the answer to anything. The diagnostic ring is a fixed number of
records; in the e2e harness, where every test starts a browser, those lines had
taken it over — a failing test's dump came out 93% dbus, and what the mirror
had said was gone.

What must survive is everything else, including a real line that arrived in the
same read as the noise.
*/
func TestTheBrowsersComplaintsAboutTheMachineAreNotLogged(t *testing.T) {
	for _, line := range []string{
		"[41:69:0821/074808.455593:ERROR:dbus/bus.cc:405] Failed to connect to the bus: " +
			"Failed to connect to socket /run/dbus/system_bus_socket: No such file or directory",
		"[41:41:0821/074809.561190:ERROR:dbus/object_proxy.cc:572] Failed to call method: " +
			"org.freedesktop.DBus.NameHasOwner: object_path= /org/freedesktop/DBus",
		"[82:82:0821/074809.323125:ERROR:.../viz_main_impl.cc:190] Exiting GPU process due " +
			"to errors during initialization",
		"[139:156:0821/074809.862741:ERROR:command_buffer_proxy_impl.cc:285] " +
			"ContextResult::kTransientFailure: Failed to send GpuControl.CreateCommandBuffer.",
		"[41:72:0821/074811.824383:ERROR:registration_request.cc:291] Registration response " +
			"error message: DEPRECATED_ENDPOINT",
	} {
		if worthLogging(line) {
			t.Errorf("a line the browser writes on every start was kept:\n  %s", line)
		}
	}
	for _, line := range []string{
		"DevTools listening on ws://127.0.0.1:40332/devtools/browser/75b68566",
		"[1:1:ERROR:renderer_host.cc:99] Renderer process crashed",
		"Fatal error: out of memory",
	} {
		if !worthLogging(line) {
			t.Errorf("a line worth reading was dropped:\n  %s", line)
		}
	}
}
