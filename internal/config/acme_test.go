package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// acmeConfig writes a config file and loads it, which is the path an operator
// actually takes — defaulting and validation both live in Load.
func acmeConfig(t *testing.T, cfg map[string]any) (Config, error) {
	t.Helper()
	base := map[string]any{
		"dataDir": t.TempDir(),
		"token":   "test-token",
	}
	for k, v := range cfg {
		base[k] = v
	}
	raw, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

// The shortest configuration that should work. Everything else is inferred:
// the names from `hosts`, the challenge from where the listeners already are,
// and the port from the challenge.
func TestACMENeedsOnlyDomainsAndAgreement(t *testing.T) {
	cfg, err := acmeConfig(t, map[string]any{
		"hosts": []string{"skyhook.example.com"},
		"acme":  map[string]any{"enabled": true, "agreeTos": true},
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.ACME.Domains; len(got) != 1 || got[0] != "skyhook.example.com" {
		t.Errorf("domains = %v, want them taken from hosts", got)
	}
	if cfg.ACME.Challenge != ChallengeHTTP01 {
		t.Errorf("challenge = %q, want %q with the fallback listener off 443",
			cfg.ACME.Challenge, ChallengeHTTP01)
	}
	if cfg.ACME.HTTPListen != ":80" {
		t.Errorf("httpListen = %q", cfg.ACME.HTTPListen)
	}
	if cfg.ACME.Directory != "" {
		t.Errorf("directory = %q, want Let's Encrypt production", cfg.ACME.Directory)
	}
}

// The certified names are the names the server answers on. Everything built
// from `hosts` — the pairing file, the pairing link, the app's connect-src —
// has to agree with them, or the client is sent somewhere the certificate does
// not cover and the browser refuses the connection it was told to make.
func TestACMEDomainsBecomeTheHosts(t *testing.T) {
	cfg, err := acmeConfig(t, map[string]any{
		"acme": map[string]any{
			"enabled":  true,
			"agreeTos": true,
			"domains":  []string{"Skyhook.Example.COM"},
		},
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Hosts) != 1 || cfg.Hosts[0] != "skyhook.example.com" {
		t.Errorf("hosts = %v, want the certified name (lowercased)", cfg.Hosts)
	}
	p := cfg.PairingFor("", "")
	if p.Host != "skyhook.example.com" {
		t.Errorf("pairing host = %q, want the name on the certificate", p.Host)
	}
	if p.CertSHA256 != "" {
		t.Error("a real certificate must not be pinned")
	}
	if p.PreferFallback {
		t.Error("a real certificate on our own listener keeps WebTransport")
	}
}

// The challenge is really a choice of port, so a deployment already on 443 gets
// the one that needs no second listener.
func TestACMEChallengeFollowsTheListeners(t *testing.T) {
	cfg, err := acmeConfig(t, map[string]any{
		"fallbackListen": ":443",
		"acme": map[string]any{
			"enabled": true, "agreeTos": true, "domains": []string{"skyhook.example.com"},
		},
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.ACME.Challenge != ChallengeTLSALPN01 {
		t.Errorf("challenge = %q, want %q when the fallback listener is on 443",
			cfg.ACME.Challenge, ChallengeTLSALPN01)
	}
}

func TestACMEStagingAlias(t *testing.T) {
	cfg, err := acmeConfig(t, map[string]any{
		"acme": map[string]any{
			"enabled": true, "agreeTos": true,
			"domains": []string{"skyhook.example.com"}, "directory": "staging",
		},
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.ACME.Directory != ACMEStagingURL {
		t.Errorf("directory = %q, want the staging URL", cfg.ACME.Directory)
	}
}

// Each of these is a deployment that would start, run, and never get a
// certificate. The message has to name the fix, because the failure otherwise
// surfaces minutes later as an authority error written for somebody
// implementing ACME.
func TestACMERefusesConfigurationsThatCannotWork(t *testing.T) {
	good := map[string]any{
		"enabled": true, "agreeTos": true, "domains": []string{"skyhook.example.com"},
	}
	with := func(over map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range good {
			out[k] = v
		}
		for k, v := range over {
			out[k] = v
		}
		return out
	}
	cases := []struct {
		name string
		cfg  map[string]any
		want string
	}{
		{
			"no agreement",
			map[string]any{"acme": with(map[string]any{"agreeTos": false})},
			"agreeTos",
		},
		{
			"a certificate from somewhere else as well",
			map[string]any{"acme": good, "tlsCert": "/etc/ssl/cert.pem", "tlsKey": "/etc/ssl/key.pem"},
			"two answers",
		},
		{
			"behind a proxy that already terminates TLS",
			map[string]any{
				"acme": good, "behindProxy": true,
				"publicUrl": "https://skyhook.example.com",
			},
			"behindProxy",
		},
		{
			"the loopback demo",
			map[string]any{"acme": good, "insecureLoopback": true},
			"insecureLoopback",
		},
		{
			"an address instead of a name",
			map[string]any{"acme": with(map[string]any{"domains": []string{"203.0.113.7"}})},
			"it is an address",
		},
		{
			"the default hosts",
			map[string]any{"acme": map[string]any{"enabled": true, "agreeTos": true}},
			"public suffix",
		},
		{
			"tls-alpn-01 with no TLS listener to answer on",
			map[string]any{
				"acme":              with(map[string]any{"challenge": "tls-alpn-01"}),
				"webSocketFallback": false,
			},
			"webSocketFallback",
		},
		{
			"a challenge nothing implements",
			map[string]any{"acme": with(map[string]any{"challenge": "email-01"})},
			"challenge",
		},
		{
			"a directory that is not a URL",
			map[string]any{"acme": with(map[string]any{"directory": "acme.example.com"})},
			"https URL",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := acmeConfig(t, tc.cfg)
			if err == nil {
				t.Fatal("accepted a configuration that cannot get a certificate")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// A container deployment sets everything by environment, so the whole feature
// has to be reachable without a config file at all.
func TestACMEFromTheEnvironment(t *testing.T) {
	t.Setenv("SKYHOOK_DATA_DIR", t.TempDir())
	t.Setenv("SKYHOOK_TOKEN", "test-token")
	t.Setenv("SKYHOOK_ACME", "1")
	t.Setenv("SKYHOOK_ACME_DOMAINS", "skyhook.example.com,www.skyhook.example.com")
	t.Setenv("SKYHOOK_ACME_EMAIL", "ops@example.com")
	t.Setenv("SKYHOOK_ACME_AGREE_TOS", "true")
	t.Setenv("SKYHOOK_ACME_DIRECTORY", "staging")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.ACME.Enabled || !cfg.ACME.AgreeTOS {
		t.Fatal("acme did not come on")
	}
	if len(cfg.ACME.Domains) != 2 {
		t.Errorf("domains = %v", cfg.ACME.Domains)
	}
	if cfg.ACME.Email != "ops@example.com" {
		t.Errorf("email = %q", cfg.ACME.Email)
	}
	if cfg.ACME.Directory != ACMEStagingURL {
		t.Errorf("directory = %q", cfg.ACME.Directory)
	}
}

// Leaving it off must change nothing, including for the deployments that were
// already working before any of this existed.
func TestACMEOffChangesNothing(t *testing.T) {
	cfg, err := acmeConfig(t, map[string]any{"hosts": []string{"localhost"}})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.ACME.Enabled {
		t.Error("acme is on by default")
	}
	if len(cfg.Hosts) != 1 || cfg.Hosts[0] != "localhost" {
		t.Errorf("hosts = %v, want them untouched", cfg.Hosts)
	}
}

// A container publishes the world's port 80 on some other number inside, and an
// unprivileged process on a bare-metal box often cannot have port 80 at all.
// Both are ordinary; refusing them would refuse the deployment this feature is
// most useful in. The server says what it bound at startup instead.
func TestACMEAcceptsAForwardedChallengePort(t *testing.T) {
	cfg, err := acmeConfig(t, map[string]any{
		"acme": map[string]any{
			"enabled": true, "agreeTos": true,
			"domains": []string{"skyhook.example.com"}, "httpListen": ":8080",
		},
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.ACME.HTTPListen != ":8080" {
		t.Errorf("httpListen = %q", cfg.ACME.HTTPListen)
	}
}

// dns-01 is the challenge for a machine that cannot be connected to at all —
// behind a NAT, on a link that filters 80 and 443, or with both already spoken
// for. It asks for no ports, so none of the port reasoning applies to it.
func TestACMEDNS01NeedsNoPorts(t *testing.T) {
	cfg, err := acmeConfig(t, map[string]any{
		"acme": map[string]any{
			"enabled": true, "agreeTos": true,
			"domains":   []string{"skyhook.example.com"},
			"challenge": "dns-01",
			"dns":       map[string]any{"command": []string{"/bin/true"}},
		},
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.ACME.Challenge != ChallengeDNS01 {
		t.Errorf("challenge = %q", cfg.ACME.Challenge)
	}
	if len(cfg.ACME.DNS.Command) != 1 || cfg.ACME.DNS.Command[0] != "/bin/true" {
		t.Errorf("command = %v", cfg.ACME.DNS.Command)
	}
	// The fallback listener is on 4434 and nothing is on 80, which would be a
	// problem for either of the other two and is none here.
	if cfg.ACME.HTTPListen != "" {
		t.Errorf("dns-01 reserved a challenge port: %q", cfg.ACME.HTTPListen)
	}
}

// It is never chosen for anybody, because it cannot be: the record has to be
// published by a command only the operator can write.
func TestACMEDNS01IsNeverTheDefault(t *testing.T) {
	for _, fallback := range []string{":4434", ":443"} {
		cfg, err := acmeConfig(t, map[string]any{
			"fallbackListen": fallback,
			"acme": map[string]any{
				"enabled": true, "agreeTos": true,
				"domains": []string{"skyhook.example.com"},
			},
		})
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if cfg.ACME.Challenge == ChallengeDNS01 {
			t.Errorf("fallbackListen %s chose dns-01 by itself", fallback)
		}
	}
}

func TestACMEDNS01RefusesWhatItCannotDo(t *testing.T) {
	dns01 := func(over map[string]any) map[string]any {
		acme := map[string]any{
			"enabled": true, "agreeTos": true,
			"domains": []string{"skyhook.example.com"}, "challenge": "dns-01",
			"dns": map[string]any{"command": []string{"/bin/true"}},
		}
		for k, v := range over {
			acme[k] = v
		}
		return map[string]any{"acme": acme}
	}
	cases := []struct {
		name string
		cfg  map[string]any
		want string
	}{
		{
			"no way to publish the record",
			dns01(map[string]any{"dns": map[string]any{}}),
			"acme.dns.command",
		},
		{
			"a resolver with no port",
			dns01(map[string]any{"dns": map[string]any{
				"command": []string{"/bin/true"}, "resolvers": []string{"1.1.1.1"},
			}}),
			"needs a port",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := acmeConfig(t, tc.cfg)
			if err == nil {
				t.Fatal("accepted a dns-01 configuration that cannot work")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// A wildcard is proved by asking the zone, so dns-01 is the only challenge that
// can get one — and a wildcard is not a host anybody can dial, so it is
// certified and then kept out of everything built from `hosts`.
func TestACMEWildcardIsCertifiedButNeverDialled(t *testing.T) {
	cfg, err := acmeConfig(t, map[string]any{
		"acme": map[string]any{
			"enabled": true, "agreeTos": true, "challenge": "dns-01",
			"domains": []string{"skyhook.example.com", "*.skyhook.example.com"},
			"dns":     map[string]any{"command": []string{"/bin/true"}},
		},
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.ACME.Domains) != 2 {
		t.Errorf("domains = %v, want the wildcard certified", cfg.ACME.Domains)
	}
	if len(cfg.Hosts) != 1 || cfg.Hosts[0] != "skyhook.example.com" {
		t.Errorf("hosts = %v, want the wildcard kept out of it", cfg.Hosts)
	}
	if p := cfg.PairingFor("", ""); p.Host != "skyhook.example.com" {
		t.Errorf("pairing host = %q; a client cannot dial a wildcard", p.Host)
	}

	// Nothing but wildcards names no server at all.
	_, err = acmeConfig(t, map[string]any{
		"acme": map[string]any{
			"enabled": true, "agreeTos": true, "challenge": "dns-01",
			"domains": []string{"*.skyhook.example.com"},
			"dns":     map[string]any{"command": []string{"/bin/true"}},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "all wildcards") {
		t.Errorf("error = %v, want a refusal naming the problem", err)
	}
}
