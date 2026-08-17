package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// builtClient makes the run believe there is a client build already, which is
// what most of these tests want to be true and none of them should have to
// depend on. Whether this machine has run `npm run build` decides how many
// questions get asked, and a scripted answer list that is right on a developer's
// laptop and wrong on a build machine is not a test of anything.
func builtClient(t *testing.T) string {
	t.Helper()
	dist := filepath.Join(t.TempDir(), "dist")
	if err := os.MkdirAll(dist, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dist, "index.html"), []byte("<!doctype html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	setWorld(t, dist, filepath.Dir(dist), nil)
	return dist
}

// setWorld fixes what the machine looks like for one test: a build or not, a
// checkout or not, npm or not.
func setWorld(t *testing.T, dist, repo string, npmErr error) {
	t.Helper()
	oldDist, oldRepo, oldNPM := findClientDist, findRepoRoot, findNPM
	findClientDist = func() string { return dist }
	findRepoRoot = func() string { return repo }
	findNPM = func() error { return npmErr }
	t.Cleanup(func() { findClientDist, findRepoRoot, findNPM = oldDist, oldRepo, oldNPM })
}

// drive runs a whole session from a script of answers and hands back what the
// operator would have seen. Every test here is "somebody typed this, and got
// that", which is the only useful way to test a conversation.
func drive(t *testing.T, answers ...string) (transcript string, dir string) {
	t.Helper()
	dir = t.TempDir()
	var out bytes.Buffer
	in := strings.NewReader(strings.Join(answers, "\n") + "\n")
	err := Run(context.Background(), Options{
		In: in, Out: &out,
		ConfigPath: filepath.Join(dir, "config.json"),
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out.String())
	}
	return out.String(), dir
}

func loadWritten(t *testing.T, dir string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("no config was written: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("the config it wrote is not JSON: %v", err)
	}
	return cfg
}

func mustContain(t *testing.T, transcript string, phrases ...string) {
	t.Helper()
	for _, p := range phrases {
		if !strings.Contains(transcript, p) {
			t.Errorf("the transcript never says %q:\n%s", p, transcript)
		}
	}
}

// The shortest useful run: everything on one machine. Four answers and the
// server can start.
func TestLoopbackNeedsFourAnswers(t *testing.T) {
	builtClient(t)
	transcript, dir := drive(t,
		"",  // data directory: the default
		"",  // browser: Skyhook starts one
		"1", // reached at: this machine only
		"y", // go ahead
		"n", // do not create anything yet
	)
	cfg := loadWritten(t, dir)
	if cfg["insecureLoopback"] != true {
		t.Errorf("insecureLoopback = %v", cfg["insecureLoopback"])
	}
	if cfg["listen"] != "127.0.0.1:4433" {
		t.Errorf("listen = %v, want loopback", cfg["listen"])
	}
	// The one thing that used to need a step nobody could guess.
	mustContain(t, transcript, "found a built client")
	if cfg["webRoot"] != "" {
		t.Errorf("webRoot = %v, want it left empty when the checkout's build is found",
			cfg["webRoot"])
	}
}

// The case that prompted all this: driving a browser you already have open.
// Setup asks for the endpoint, and then actually connects to it — so the two
// flags that browser needed are discovered here rather than at first run.
func TestAttachingToARunningBrowserIsChecked(t *testing.T) {
	builtClient(t)
	devtools := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/version" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"Browser":"Chrome/141.0.0.0"}`))
	}))
	defer devtools.Close()

	transcript, dir := drive(t,
		"",           // data directory
		"2",          // attach to yours
		devtools.URL, // the endpoint
		"1",          // this machine only
		"y",          // go ahead
		"n",          // nothing to create
	)
	mustContain(t, transcript,
		"--remote-debugging-port=9222", // the flags it needs
		"--remote-allow-origins",
		"attached: Chrome/141.0.0.0", // and proof it worked
	)
	cfg := loadWritten(t, dir)
	if cfg["chromeAttach"] != devtools.URL {
		t.Errorf("chromeAttach = %v", cfg["chromeAttach"])
	}
}

// An endpoint nothing answers on is the common case — the browser is running
// without the flags. Setup has to say so and let it be fixed in place, rather
// than writing a configuration that will fail at first run.
func TestAnUnreachableBrowserIsReportedAndRetried(t *testing.T) {
	builtClient(t)
	devtools := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"Browser":"Chrome/141.0.0.0"}`))
	}))
	defer devtools.Close()

	transcript, _ := drive(t,
		"",                   // data directory
		"2",                  // attach
		"http://127.0.0.1:1", // nothing is there
		"y",                  // try again
		devtools.URL,         // now it is
		"1",                  // this machine only
		"y",                  // go ahead
		"n",                  // nothing to create
	)
	mustContain(t, transcript, "nothing answered there", "attached: Chrome")
}

// Declining the retry still writes a configuration — the browser can be started
// later — but the transcript has to end with the thing that is still missing.
func TestGivingUpOnTheBrowserLeavesAReminder(t *testing.T) {
	builtClient(t)
	transcript, _ := drive(t,
		"", "2", "http://127.0.0.1:1", "n", "1", "y", "n",
	)
	mustContain(t, transcript, "Still to do:", "--remote-debugging-port=9222")
}

// The pinned deployment is the one whose cost is invisible until somebody is
// offline, so setup says it at the moment of choosing and again at the end.
func TestSelfSignedSaysWhatItCosts(t *testing.T) {
	builtClient(t)
	transcript, dir := drive(t,
		"",                    // data directory
		"1",                   // Skyhook's own browser
		"3",                   // a name, self-signed
		"skyhook.example.com", // the name
		"y",                   // go ahead
		"n",                   // nothing to create
	)
	mustContain(t, transcript, "no install and no offline start", "Still to do:")
	cfg := loadWritten(t, dir)
	hosts, _ := cfg["hosts"].([]any)
	if len(hosts) != 1 || hosts[0] != "skyhook.example.com" {
		t.Errorf("hosts = %v", cfg["hosts"])
	}
	if acme, _ := cfg["acme"].(map[string]any); acme["enabled"] == true {
		t.Error("acme should be off here")
	}
}

func TestBehindAProxyWritesBothSettingsThatHaveToAgree(t *testing.T) {
	builtClient(t)
	_, dir := drive(t,
		"", "1", "4",
		"https://skyhook.example.com",
		"y", "n",
	)
	cfg := loadWritten(t, dir)
	if cfg["publicUrl"] != "https://skyhook.example.com" {
		t.Errorf("publicUrl = %v", cfg["publicUrl"])
	}
	if cfg["behindProxy"] != true {
		t.Errorf("behindProxy = %v; without it the proxy has to trust a self-signed upstream",
			cfg["behindProxy"])
	}
}

// Saying no at the summary has to leave the disk exactly as it was. That is the
// promise made in the first paragraph of the run.
func TestSayingNoWritesNothing(t *testing.T) {
	builtClient(t)
	dir := t.TempDir()
	var out bytes.Buffer
	err := Run(context.Background(), Options{
		In:         strings.NewReader(strings.Join([]string{"", "1", "1", "n"}, "\n") + "\n"),
		Out:        &out,
		ConfigPath: filepath.Join(dir, "config.json"),
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); !os.IsNotExist(err) {
		t.Error("a refused summary still wrote a config")
	}
	mustContain(t, out.String(), "Nothing was written")
}

// Ctrl-D at any question is a cancellation, not a crash, and nothing is left
// behind either.
func TestClosingTheInputCancels(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	err := Run(context.Background(), Options{
		In:         strings.NewReader(""), // closed straight away
		Out:        &out,
		ConfigPath: filepath.Join(dir, "config.json"),
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("a closed input should not be an error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); !os.IsNotExist(err) {
		t.Error("a cancelled run wrote a config")
	}
}

// An existing configuration is moved aside rather than overwritten. Somebody
// re-running setup to change one answer should not lose the file they had.
func TestAnExistingConfigIsKept(t *testing.T) {
	builtClient(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"logLevel":"debug"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := Run(context.Background(), Options{
		In:         strings.NewReader(strings.Join([]string{"", "1", "1", "y", "n"}, "\n") + "\n"),
		Out:        &out,
		ConfigPath: path,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("the old config was not kept: %v", err)
	}
	if !strings.Contains(string(old), "debug") {
		t.Errorf("the backup is not the old file: %s", old)
	}
}

// Refusing the subscriber agreement stops the run rather than writing a
// configuration that can never get a certificate.
func TestRefusingTheAgreementStops(t *testing.T) {
	builtClient(t)
	dir := t.TempDir()
	var out bytes.Buffer
	err := Run(context.Background(), Options{
		In: strings.NewReader(strings.Join([]string{
			"", "1", "2", "skyhook.example.com", "", "n",
		}, "\n") + "\n"),
		Out:        &out,
		ConfigPath: filepath.Join(dir, "config.json"),
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); !os.IsNotExist(err) {
		t.Error("a config was written despite the agreement being refused")
	}
	mustContain(t, out.String(), "nothing can be requested without it")
}

// The dns-01 branch: the hook is tested for real before anything is written,
// because every way it can be wrong is otherwise invisible until an order is
// already in flight.
func TestTheDNSHookIsTestedBeforeAnythingIsWritten(t *testing.T) {
	builtClient(t)
	// A hook that reports success and publishes nothing — the commonest shape of
	// a broken one, and the one an untested configuration hides.
	hookDir := t.TempDir()
	hook := filepath.Join(hookDir, "hook.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil { //nolint:gosec // fixture
		t.Fatal(err)
	}

	dir := t.TempDir()
	var out bytes.Buffer
	err := Run(context.Background(), Options{
		In: strings.NewReader(strings.Join([]string{
			"",                // data directory
			"1",               // Skyhook's own browser
			"2",               // a name, our certificate
			"skyhook.invalid", // a name that resolves nowhere
			"-",               // no email
			"y",               // accept the agreement
			"2",               // the high ports
			"3",               // dns-01
			hook,              // the hook
			"y",               // test it
			"n",               // no other nameserver to ask
			"n",               // it failed; do not try again
			"y",               // write anyway
			"n",               // do not fetch a certificate
		}, "\n") + "\n"),
		Out:        &out,
		ConfigPath: filepath.Join(dir, "config.json"),
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out.String())
	}
	transcript := out.String()
	mustContain(t, transcript,
		"publishing a test record", // it tried
		"could not be checked",     // and said exactly what it could not establish
		"Still to do:",             // and carried the consequence to the end
	)
	cfg := loadWritten(t, dir)
	acme, _ := cfg["acme"].(map[string]any)
	if acme["challenge"] != "dns-01" {
		t.Errorf("challenge = %v", acme["challenge"])
	}
	dns, _ := acme["dns"].(map[string]any)
	cmd, _ := dns["command"].([]any)
	if len(cmd) != 1 || cmd[0] != hook {
		t.Errorf("command = %v", dns["command"])
	}
}

// Whatever the answers, the file has to be one the server will actually load.
// A setup that writes a configuration the server then refuses would be worse
// than no setup at all.
func TestEveryPathWritesAConfigTheServerAccepts(t *testing.T) {
	paths := map[string][]string{
		"loopback":    {"", "1", "1", "y", "n"},
		"self-signed": {"", "1", "3", "skyhook.example.com", "y", "n"},
		"proxy":       {"", "1", "4", "https://skyhook.example.com", "y", "n"},
	}
	for name, answers := range paths {
		t.Run(name, func(t *testing.T) {
			builtClient(t)
			_, dir := drive(t, answers...)
			// config.Load is what the server uses; setup runs it before writing,
			// so reaching this point at all means it passed. Re-read to be sure
			// the file on disk is the one that was checked.
			raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
			if err != nil {
				t.Fatal(err)
			}
			var cfg map[string]any
			if err := json.Unmarshal(raw, &cfg); err != nil {
				t.Fatalf("not JSON: %v", err)
			}
			if cfg["dataDir"] == "" {
				t.Error("no data directory")
			}
		})
	}
}

// A hook that fails outright gets the other branch: the command's own message,
// and the three things that are wrong when a DNS hook is wrong, in the order
// they are worth checking.
func TestAFailingDNSHookIsExplainedWithSomethingToTry(t *testing.T) {
	builtClient(t)
	hookDir := t.TempDir()
	hook := filepath.Join(hookDir, "hook.sh")
	script := "#!/bin/sh\necho 'cloudflare: Invalid API token (9109)' >&2\nexit 1\n"
	if err := os.WriteFile(hook, []byte(script), 0o700); err != nil { //nolint:gosec // fixture
		t.Fatal(err)
	}
	transcript, dir := drive(t,
		"",                    // data directory
		"1",                   // Skyhook's own browser
		"2",                   // a name, our certificate
		"skyhook.example.com", // the name
		"",                    // no email
		"y",                   // accept the agreement
		"2",                   // the high ports
		"3",                   // dns-01
		hook,                  // the hook
		"y",                   // test it
		"n",                   // do not try again
		"y",                   // write anyway
		"n",                   // do not fetch a certificate
	)
	mustContain(t, transcript,
		"Invalid API token (9109)", // the provider's own words, not ours
		"the token cannot write",   // the causes, likeliest first
		"dig +short TXT",           // and the command to run by hand
	)
	// The configuration is still written: the hook can be fixed in place, and
	// losing the eleven answers before it would be its own punishment.
	cfg := loadWritten(t, dir)
	if acme, _ := cfg["acme"].(map[string]any); acme["challenge"] != "dns-01" {
		t.Errorf("challenge = %v", acme["challenge"])
	}
}

// How many questions the client section asks depends on what is on the machine,
// and all three shapes are real: a developer with a build, a fresh checkout, and
// an installed binary with neither. CI is the middle one, which is how it found
// that the tests only knew about the first.
func TestTheClientSectionFitsWhateverIsOnTheMachine(t *testing.T) {
	t.Run("already built: nothing to ask", func(t *testing.T) {
		dist := builtClient(t)
		transcript, dir := drive(t, "", "1", "1", "y", "n")
		mustContain(t, transcript, "found a built client at "+dist)
		if cfg := loadWritten(t, dir); cfg["webRoot"] != "" {
			t.Errorf("webRoot = %v, want it left to discovery", cfg["webRoot"])
		}
	})

	t.Run("a checkout with no build: offers to build it", func(t *testing.T) {
		setWorld(t, "", t.TempDir(), nil)
		transcript, _ := drive(t,
			"", "1", "1",
			"n", // do not build it now
			"y", "n",
		)
		mustContain(t, transcript,
			"the client has not been built yet",
			"Build it now?",
			"npm ci && npm run build", // the note that says what to do instead
		)
	})

	t.Run("a checkout with no npm: says so instead of offering", func(t *testing.T) {
		setWorld(t, "", t.TempDir(), errors.New("not found"))
		transcript, _ := drive(t, "", "1", "1", "y", "n")
		mustContain(t, transcript, "npm is not installed", "Still to do:")
		if strings.Contains(transcript, "Build it now?") {
			t.Error("offered to run a build with no npm to run it with")
		}
	})

	t.Run("no checkout: asks for a path, and checks it", func(t *testing.T) {
		setWorld(t, "", "", nil)
		dist := filepath.Join(t.TempDir(), "dist")
		if err := os.MkdirAll(dist, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dist, "index.html"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		transcript, cfgDir := drive(t,
			"", "1", "1",
			"/nowhere/at/all", // refused: nothing there
			dist,              // accepted
			"y", "n",
		)
		mustContain(t, transcript, "no index.html in /nowhere/at/all")
		if cfg := loadWritten(t, cfgDir); cfg["webRoot"] != dist {
			t.Errorf("webRoot = %v, want %q", cfg["webRoot"], dist)
		}
	})

	t.Run("no checkout, skipped: leaves a note rather than a wrong path", func(t *testing.T) {
		setWorld(t, "", "", nil)
		transcript, dir := drive(t, "", "1", "1", "", "y", "n")
		mustContain(t, transcript, "Still to do:", "npm ci && npm run build")
		if cfg := loadWritten(t, dir); cfg["webRoot"] != "" {
			t.Errorf("webRoot = %v, want empty", cfg["webRoot"])
		}
	})
}
