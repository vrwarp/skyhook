package transport

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func quietACME(t *testing.T, opts ACMEOptions) *ACME {
	t.Helper()
	opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	if opts.CacheDir == "" {
		opts.CacheDir = t.TempDir()
	}
	a, err := NewACME(opts)
	if err != nil {
		t.Fatalf("NewACME: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

// The certificate is answered per handshake, not held. That is what makes a
// renewal invisible — and it is also the property the rest of the server has to
// respect, so the bundle says so out loud.
func TestManagedBundleIsAnsweredPerHandshake(t *testing.T) {
	a := quietACME(t, ACMEOptions{
		Domains:   []string{"skyhook.example.com"},
		Challenge: ChallengeHTTP01,
		HTTPAddr:  "127.0.0.1:0",
	})
	b := a.Bundle()
	if !b.Managed {
		t.Error("a managed certificate must say it is managed")
	}
	if b.TLS.GetCertificate == nil {
		t.Error("no GetCertificate: nothing would serve the renewed certificate")
	}
	if len(b.TLS.Certificates) != 0 {
		t.Error("a static certificate would freeze whatever was current at startup")
	}
	if len(b.TLS.NextProtos) == 0 || b.TLS.NextProtos[0] != "h3" {
		t.Errorf("QUIC ALPN = %v, want h3 first", b.TLS.NextProtos)
	}
}

// Handing the client the fingerprint of a real certificate is worse than
// handing it nothing: WebTransport refuses any pin whose certificate is valid
// for more than 14 days, so every dial would fail and quietly fall back to the
// socket — the transport this project exists to keep.
func TestOnlyTheSelfSignedCertificateIsPinned(t *testing.T) {
	managed := quietACME(t, ACMEOptions{
		Domains:   []string{"skyhook.example.com"},
		Challenge: ChallengeHTTP01,
		HTTPAddr:  "127.0.0.1:0",
	}).Bundle()
	if _, _, ok := managed.Pin(); ok {
		t.Error("a managed certificate must not be pinned")
	}

	dir := t.TempDir()
	selfSigned, err := GenerateSelfSigned(dir, []string{"skyhook.example.com"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	sha, expires, ok := selfSigned.Pin()
	if !ok {
		t.Fatal("the self-signed certificate is the one the client does pin")
	}
	if sha == "" || expires == "" {
		t.Errorf("pin = %q %q, want both set", sha, expires)
	}

	// The same certificate read back off disk as an operator-supplied pair —
	// tlsCert/tlsKey — is no longer ours to rotate, and is not pinned either.
	loaded, err := LoadCert(dir+"/cert.pem", dir+"/key.pem")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := loaded.Pin(); ok {
		t.Error("a certificate the operator supplied must not be pinned")
	}
}

// The TLS-ALPN-01 challenge arrives on the fallback listener as a handshake
// asking for one protocol no browser ever asks for. A listener that does not
// offer it aborts, and the authority reports a name it could not verify.
func TestFallbackListenerOffersTheChallengeProtocolOnlyWhenItAnswersOne(t *testing.T) {
	alpn01 := quietACME(t, ACMEOptions{
		Domains:   []string{"skyhook.example.com"},
		Challenge: ChallengeTLSALPN01,
	}).Bundle()
	got := alpn01.TLSForWS().NextProtos
	if !contains(got, ACMEALPN) {
		t.Errorf("fallback ALPN = %v, want it to include %q", got, ACMEALPN)
	}
	if !contains(got, "http/1.1") {
		t.Errorf("fallback ALPN = %v, dropped the protocol the client speaks", got)
	}

	http01 := quietACME(t, ACMEOptions{
		Domains:   []string{"skyhook.example.com"},
		Challenge: ChallengeHTTP01,
		HTTPAddr:  "127.0.0.1:0",
	}).Bundle()
	if contains(http01.TLSForWS().NextProtos, ACMEALPN) {
		t.Error("offering a challenge protocol nothing here answers invites a handshake " +
			"that cannot complete")
	}
}

// Port 80 is open for the authority whether or not anybody uses it, so what a
// person who types the bare name gets is worth choosing. Autocert's own default
// redirects to https on the same host, which is the wrong port in every Skyhook
// deployment that does not serve the app on 443.
func TestChallengePortRedirectsToTheApp(t *testing.T) {
	a := quietACME(t, ACMEOptions{
		Domains:    []string{"skyhook.example.com"},
		Challenge:  ChallengeHTTP01,
		HTTPAddr:   "127.0.0.1:0",
		RedirectTo: "https://skyhook.example.com:4434",
	})
	if err := a.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if a.Addr() == "" {
		t.Fatal("no bound address")
	}

	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	res, err := client.Get("http://" + a.Addr() + "/some/page") //nolint:noctx // test
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want a redirect", res.StatusCode)
	}
	if loc := res.Header.Get("Location"); loc != "https://skyhook.example.com:4434/some/page" {
		t.Errorf("Location = %q", loc)
	}
}

// With nowhere to send them, the port says what it is for. A visitor being told
// "not found" by a server that is running is confusing; being told the port
// answers challenges is not.
func TestChallengePortExplainsItselfWithNoAppToPointAt(t *testing.T) {
	a := quietACME(t, ACMEOptions{
		Domains:   []string{"skyhook.example.com"},
		Challenge: ChallengeHTTP01,
		HTTPAddr:  "127.0.0.1:0",
	})
	if err := a.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	res, err := http.Get("http://" + a.Addr() + "/") //nolint:noctx // test
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d", res.StatusCode)
	}
	if !strings.Contains(string(body), "certificate challenges") {
		t.Errorf("body = %q", body)
	}
}

// The directory URL has to be the one that is used. A setting that is accepted
// and then ignored would send a deployment meant for staging — or for a private
// authority — at Let's Encrypt's production quota instead, and the only symptom
// would be a rate limit weeks later.
func TestTheConfiguredDirectoryIsTheOneContacted(t *testing.T) {
	var hits atomic.Int32
	ca := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		// A problem document the client will not retry, so the test asserts on
		// the first request rather than on a minute of backoff.
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(
			`{"type":"urn:ietf:params:acme:error:malformed","detail":"not a real authority"}`))
	}))
	defer ca.Close()

	a := quietACME(t, ACMEOptions{
		Domains:    []string{"skyhook.example.com"},
		Challenge:  ChallengeHTTP01,
		HTTPAddr:   "127.0.0.1:0",
		Directory:  ca.URL,
		HTTPClient: ca.Client(),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := a.Ensure(ctx)
	if err == nil {
		t.Fatal("an authority answering 500 is not a certificate")
	}
	if !strings.Contains(err.Error(), "skyhook.example.com") {
		t.Errorf("error does not name the domain it failed on: %v", err)
	}
	if hits.Load() == 0 {
		t.Fatal("the configured directory was never contacted")
	}
}

// Ensure has to come back. It is called at startup, and an authority that
// accepts the connection and then says nothing would otherwise hold the server
// short of its listeners for as long as the network cared to wait.
func TestEnsureHonoursItsDeadline(t *testing.T) {
	block := make(chan struct{})
	ca := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	// Closed in this order: the handler has to be released before the server
	// will admit that the request it is still serving has finished.
	defer ca.Close()
	defer close(block)

	a := quietACME(t, ACMEOptions{
		Domains:    []string{"skyhook.example.com"},
		Challenge:  ChallengeHTTP01,
		HTTPAddr:   "127.0.0.1:0",
		Directory:  ca.URL,
		HTTPClient: ca.Client(),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := a.Ensure(ctx)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want the deadline", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Ensure did not come back")
	}
}

// Refusing at configuration time is the difference between a sentence naming
// the fix and an authority error some minutes later.
func TestACMEDomainsAreCheckedBeforeAnythingIsRequested(t *testing.T) {
	cases := []struct {
		name      string
		domains   []string
		challenge ACMEChallenge
		want      string
	}{
		{"an address", []string{"203.0.113.7"}, ChallengeHTTP01, "not a name"},
		{"a v6 address", []string{"2001:db8::1"}, ChallengeHTTP01, "not a name"},
		{"a bare name", []string{"localhost"}, ChallengeHTTP01, "not a public name"},
		{"a bare wildcard", []string{"*.localhost"}, ChallengeDNS01, "not a public name"},
		{"nothing at all", []string{"", "  "}, ChallengeHTTP01, "no domains"},
		// A wildcard is proved by asking the zone, so the challenges that work
		// by connecting to a host cannot get one however the operator asks.
		{"a wildcard over http-01", []string{"*.example.com"}, ChallengeHTTP01, "only the dns-01"},
		{"a wildcard over tls-alpn-01", []string{"*.example.com"}, ChallengeTLSALPN01, "only the dns-01"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NormalizeACMEDomains(tc.domains, tc.challenge)
			if err == nil {
				t.Fatalf("%v was accepted", tc.domains)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}

	got, err := NormalizeACMEDomains(
		[]string{" Skyhook.Example.COM. ", "skyhook.example.com"}, ChallengeHTTP01)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "skyhook.example.com" {
		t.Errorf("normalised = %v", got)
	}

	// dns-01 is the one challenge that can prove a wildcard, so it is the one
	// that may ask for one.
	got, err = NormalizeACMEDomains(
		[]string{"skyhook.example.com", "*.example.com"}, ChallengeDNS01)
	if err != nil {
		t.Fatalf("dns-01 refused a wildcard: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("normalised = %v", got)
	}
}

func TestACMEOptionsAreChecked(t *testing.T) {
	base := ACMEOptions{
		Domains:   []string{"skyhook.example.com"},
		Challenge: ChallengeHTTP01,
		HTTPAddr:  ":80",
		CacheDir:  t.TempDir(),
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	mutate := func(f func(*ACMEOptions)) ACMEOptions {
		o := base
		f(&o)
		return o
	}
	cases := []struct {
		name string
		opts ACMEOptions
		want string
	}{
		{"no cache", mutate(func(o *ACMEOptions) { o.CacheDir = "" }), "survive a restart"},
		{"unknown challenge", mutate(func(o *ACMEOptions) { o.Challenge = "email-01" }), "unknown challenge"},
		{"dns-01 with no way to publish", mutate(func(o *ACMEOptions) {
			o.Challenge = ChallengeDNS01
		}), "publish the challenge record"},
		{"no http address", mutate(func(o *ACMEOptions) { o.HTTPAddr = "" }), "listen address"},
		{"plain directory", mutate(func(o *ACMEOptions) { o.Directory = "http://ca.example" }), "https URL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewACME(tc.opts); err == nil ||
				!strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
