package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/vrwarp/skyhook/internal/config"
)

// Preparing a data directory must not depend on Chromium. It used to: `-init`
// went through the full server constructor, which launches a browser, so a
// container that could not start Chromium could not even mint a token — and
// the error it printed was about a devtools port, which explains nothing.
func TestPrepareNeedsNoBrowser(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.Hosts = []string{"vps.example.com"}
	cfg.Token = "prepare-token"
	cfg.Chrome = filepath.Join(dir, "no-such-browser")

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cert, err := Prepare(cfg, log)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if cert.FingerprintHex() == "" {
		t.Error("no certificate fingerprint to pin")
	}

	raw, err := os.ReadFile(cfg.PairingPath())
	if err != nil {
		t.Fatalf("no pairing file: %v", err)
	}
	var p config.Pairing
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("pairing file does not parse: %v", err)
	}
	if p.Token != cfg.Token {
		t.Errorf("pairing token = %q", p.Token)
	}
	if p.CertSHA256 == "" {
		t.Error("pairing file carries no certificate fingerprint")
	}

	// The directories the server will want later should exist now; that is the
	// other half of what preparing means.
	for _, d := range []string{cfg.ProfileDir(), cfg.CertDir(), cfg.ImageCacheDir()} {
		if st, err := os.Stat(d); err != nil || !st.IsDir() {
			t.Errorf("%s was not created", d)
		}
	}
}

// The client pins the fingerprint of a self-signed certificate, so the server
// has to keep serving the same one. Regenerating on every start invalidates
// that pin, and the client — which is doing exactly what it was told — refuses
// the server it paired with until someone pairs it again.
func TestCertificateSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.Hosts = []string{"vps.example.com"}
	cfg.Token = "prepare-token"
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	first, err := loadOrCreateCert(cfg, log)
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	second, err := loadOrCreateCert(cfg, log)
	if err != nil {
		t.Fatalf("second start: %v", err)
	}
	if first.FingerprintB64() != second.FingerprintB64() {
		t.Fatalf("the pin changed across a restart: %s then %s",
			first.FingerprintHex(), second.FingerprintHex())
	}
	if !second.SelfSigned {
		t.Error("a reloaded self-signed certificate must still rotate")
	}
}

// Reuse stops where it would be wrong: a certificate that does not name the
// host the server now answers on is not the one to serve.
func TestCertificateIsReplacedWhenHostsChange(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.Hosts = []string{"vps.example.com"}
	cfg.Token = "prepare-token"
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	first, err := loadOrCreateCert(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Hosts = []string{"other.example.com"}
	second, err := loadOrCreateCert(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	if first.FingerprintB64() == second.FingerprintB64() {
		t.Fatal("reused a certificate that does not cover the configured host")
	}
	if !second.Covers([]string{"other.example.com"}) {
		t.Error("the replacement does not cover the host either")
	}
}
