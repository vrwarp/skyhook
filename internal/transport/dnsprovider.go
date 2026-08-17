package transport

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// ExecDNSProvider answers a dns-01 challenge by running a program the operator
// supplies.
//
// Every DNS provider has its own API and none of them belong in this repository:
// a personal server should not carry a matrix of cloud SDKs so that one of them
// can be used. A command is the smallest thing that covers all of them, is
// testable with a shell script, and lets somebody who edits a zone file by hand
// do exactly that.
//
// The contract is deliberately dull. The command is run twice per record:
//
//	<command...> present <fqdn> <value>
//	<command...> cleanup <fqdn> <value>
//
// and the same three facts arrive in the environment, because half the scripts
// people already have read one and half read the other:
//
//	SKYHOOK_ACME_ACTION=present|cleanup
//	SKYHOOK_ACME_FQDN=_acme-challenge.skyhook.example.com
//	SKYHOOK_ACME_VALUE=<the TXT value>
//
// A non-zero exit fails the challenge, and whatever the command wrote is
// included in the error — a provider's own message about a bad token is the
// most useful thing anyone could be shown at that moment.
//
// `present` must *add* a value rather than replace what is there. One order can
// need two records at the same name, and a script that overwrites will make the
// first challenge fail in a way that looks like a propagation problem.
type ExecDNSProvider struct {
	// Command is the program and its fixed arguments.
	Command []string
	// Timeout bounds one invocation.
	Timeout time.Duration
	// Logger receives what the command wrote when it succeeds; on failure it
	// goes into the error instead.
	Logger *slog.Logger
}

// NewExecDNSProvider checks the command is runnable now rather than in the
// middle of an order, when the certificate is already half-requested and the
// record is already published.
func NewExecDNSProvider(command []string, timeout time.Duration, log *slog.Logger) (*ExecDNSProvider, error) {
	if len(command) == 0 || strings.TrimSpace(command[0]) == "" {
		return nil, errors.New("acme: dns-01 needs a command that publishes the challenge record")
	}
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	if log == nil {
		log = slog.Default()
	}
	path, err := exec.LookPath(command[0])
	if err != nil {
		return nil, fmt.Errorf("acme: dns command %q: %w", command[0], err)
	}
	out := append([]string(nil), command...)
	out[0] = path
	return &ExecDNSProvider{Command: out, Timeout: timeout, Logger: log}, nil
}

// Describe names the provider for a log line.
func (p *ExecDNSProvider) Describe() string { return p.Command[0] }

// Present adds the record.
func (p *ExecDNSProvider) Present(ctx context.Context, fqdn, value string) error {
	return p.run(ctx, "present", fqdn, value)
}

// CleanUp removes it.
func (p *ExecDNSProvider) CleanUp(ctx context.Context, fqdn, value string) error {
	return p.run(ctx, "cleanup", fqdn, value)
}

func (p *ExecDNSProvider) run(ctx context.Context, action, fqdn, value string) error {
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()

	args := append(append([]string(nil), p.Command[1:]...), action, fqdn, value)
	cmd := exec.CommandContext(ctx, p.Command[0], args...) //nolint:gosec // operator-supplied by design
	// Killing the command is not enough to get its output back.
	//
	// A hook is almost always a shell script, and a shell script that runs
	// `curl` hands it the same stdout. Cancelling kills the shell and leaves
	// curl holding the pipe open, and reading that pipe to the end is what
	// CombinedOutput is doing — so without this, a hook with a hanging child
	// blocks for as long as the child feels like, whatever timeout was set.
	// WaitDelay is the deadline on the reading rather than on the process.
	cmd.WaitDelay = 5 * time.Second
	cmd.Env = append(os.Environ(),
		"SKYHOOK_ACME_ACTION="+action,
		"SKYHOOK_ACME_FQDN="+fqdn,
		"SKYHOOK_ACME_VALUE="+value,
	)
	out, err := cmd.CombinedOutput()
	trimmed := strings.TrimSpace(string(out))
	if err != nil {
		if trimmed != "" {
			return fmt.Errorf("%s %s: %w: %s", action, fqdn, err, trimmed)
		}
		return fmt.Errorf("%s %s: %w", action, fqdn, err)
	}
	if trimmed != "" {
		p.Logger.Debug("dns command", "action", action, "fqdn", fqdn, "output", trimmed)
	}
	return nil
}

// CheckDNSHook publishes a throwaway record, waits for it to become visible,
// and takes it down again — the whole dns-01 conversation except the part that
// involves a certificate authority.
//
// It is what `skyhookd -setup` runs before writing a configuration, because
// every way a hook can be wrong is invisible until an order is already in
// flight: a token that cannot write, a zone that is not the one serving the
// name, a script that replaces instead of appending. Finding out here costs a
// few seconds; finding out during issuance costs a failed authorization and a
// message written for somebody implementing ACME.
//
// The value is not a real challenge response — nothing is being proved to
// anybody — so a leftover record is inert. It is still cleaned up.
func CheckDNSHook(ctx context.Context, p DNSProvider, wait DNSWait, domain string, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	fqdn := "_acme-challenge." + strings.TrimSuffix(strings.TrimPrefix(domain, "*."), ".")
	value, err := throwawayValue()
	if err != nil {
		return err
	}
	if err := p.Present(ctx, fqdn, value); err != nil {
		return fmt.Errorf("publishing a test record: %w", err)
	}
	defer func() {
		cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		if err := p.CleanUp(cleanCtx, fqdn, value); err != nil {
			log.Warn("the test record could not be retracted; remove it by hand",
				"name", fqdn, "value", value, "err", err)
		}
	}()
	verified, err := wait.forTXT(ctx, fqdn, []string{value}, log)
	if err != nil {
		return err
	}
	if !verified {
		// The hook exited 0 and nothing could confirm it. That is precisely the
		// case this check exists for, so it is a failure here even though
		// issuance would carry on and let the authority decide.
		return fmt.Errorf("%w: nothing could be asked about %s, because no nameserver "+
			"was found for that zone", ErrDNSUnverifiable, fqdn)
	}
	return nil
}

// ErrDNSUnverifiable means the record could not be checked at all — no
// nameserver answered for the zone — as opposed to being checked and found
// missing. The two want different things next: this one is a name that is not
// delegated where this machine can see it, and is often answered by naming the
// server to ask instead.
var ErrDNSUnverifiable = errors.New("acme: the challenge record could not be checked")

// throwawayValue looks like a challenge response — 32 random bytes, base64url,
// unpadded — so a provider that validates the shape of what it is given is
// tested rather than tripped over.
func throwawayValue() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// DNSWait is how long, and against whom, the issuer waits for a published
// record to become visible.
type DNSWait struct {
	// Timeout gives up on a record that never appears.
	Timeout time.Duration
	// Settle is held after the record is first seen. Seeing it once is not the
	// same as every server having it, and the authority may ask a different one.
	Settle time.Duration
	// Resolvers are asked instead of the zone's own nameservers. Empty is the
	// right answer for a public zone; this exists for a split horizon, or for a
	// provider whose delegated servers are not the ones actually serving.
	Resolvers []string
}

func (w DNSWait) withDefaults() DNSWait {
	if w.Timeout <= 0 {
		w.Timeout = 5 * time.Minute
	}
	switch {
	case w.Settle == 0:
		w.Settle = 15 * time.Second
	case w.Settle < 0:
		// An explicit negative is how to ask for none, since zero has to mean
		// "unset" for a setting nobody filled in to get the useful default.
		w.Settle = 0
	}
	return w
}

// forTXT blocks until every value is visible at fqdn, or the timeout runs out.
// It reports whether the record was actually seen, which is not the same thing
// as returning no error: when there is no nameserver to ask, there is nothing
// to check against and saying so is the only honest answer.
//
// The zone's own nameservers are asked directly, not the machine's resolver.
// A recursive resolver caches the answer it got a moment ago — including the
// empty one from just before the record was published — and a negative cache
// entry is precisely the thing standing between a freshly written record and a
// challenge that would now pass.
func (w DNSWait) forTXT(ctx context.Context, fqdn string, values []string, log *slog.Logger) (verified bool, err error) {
	w = w.withDefaults()
	ctx, cancel := context.WithTimeout(ctx, w.Timeout)
	defer cancel()

	servers := w.Resolvers
	if len(servers) == 0 {
		servers = authoritativeFor(ctx, fqdn, log)
	}
	if len(servers) == 0 {
		// Nothing was checked. Issuance carries on regardless — a transient NS
		// lookup failure is no reason to abandon an order the authority is
		// perfectly able to judge for itself — but it must not be reported as a
		// record that was seen, or the hook self-test would bless a hook that
		// publishes nothing at all.
		log.Warn("no nameserver to check the challenge record against; "+
			"the authority will be the first to look", "name", fqdn)
		return false, sleep(ctx, w.Settle)
	}
	log.Info("waiting for the challenge record to be visible",
		"name", fqdn, "servers", servers)

	for attempt := 0; ; attempt++ {
		missing := ""
		for _, server := range servers {
			got, err := lookupTXTAt(ctx, server, fqdn)
			if err != nil {
				missing = fmt.Sprintf("%s: %s", server, dnsReason(err))
				break
			}
			if !containsAll(got, values) {
				missing = fmt.Sprintf("%s has %v", server, got)
				break
			}
		}
		if missing == "" {
			log.Info("challenge record is visible", "name", fqdn,
				"settling", w.Settle.String())
			return true, sleep(ctx, w.Settle)
		}
		if ctx.Err() != nil {
			return false, fmt.Errorf("acme: %s did not appear in DNS within %s (%s); "+
				"the record was published, so this is propagation or a zone the "+
				"command wrote to that is not the one serving the name",
				fqdn, w.Timeout, missing)
		}
		if attempt == 3 {
			log.Info("still waiting on the challenge record", "name", fqdn, "why", missing)
		}
		if err := sleep(ctx, 5*time.Second); err != nil {
			return false, fmt.Errorf("acme: %s did not appear in DNS within %s (%s)",
				fqdn, w.Timeout, missing)
		}
	}
}

// authoritativeFor finds the nameservers for the zone the record lives in,
// walking up from the record name until something answers. `_acme-challenge.x`
// is not itself a zone, so the first label or two are expected to fail.
func authoritativeFor(ctx context.Context, fqdn string, log *slog.Logger) []string {
	labels := strings.Split(strings.TrimSuffix(fqdn, "."), ".")
	for i := 0; i < len(labels)-1; i++ {
		zone := strings.Join(labels[i:], ".")
		ns, err := net.DefaultResolver.LookupNS(ctx, zone)
		if err != nil || len(ns) == 0 {
			continue
		}
		out := make([]string, 0, len(ns))
		for _, n := range ns {
			out = append(out, net.JoinHostPort(strings.TrimSuffix(n.Host, "."), "53"))
		}
		sort.Strings(out)
		log.Debug("found the zone's nameservers", "zone", zone, "servers", out)
		return out
	}
	return nil
}

// lookupTXTAt asks one nameserver, bypassing whatever the machine would
// normally use and whatever that has cached.
func lookupTXTAt(ctx context.Context, server, fqdn string) ([]string, error) {
	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 10 * time.Second}
			return d.DialContext(ctx, network, server)
		},
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return r.LookupTXT(ctx, fqdn)
}

// dnsReason strips the server the standard library names from a lookup error.
//
// That server is read from the machine's resolv.conf, and this code deliberately
// does not use it: the query went to a nameserver of our choosing through a
// custom dialer. Leaving the stock message intact would print "lookup … on
// 8.8.8.8:53" under a line naming the server actually asked, and the one
// failure this diagnoses — a record published into a zone that is not the one
// serving the name — is precisely where somebody would then go and blame the
// wrong resolver.
func dnsReason(err error) string {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.Err != "" {
		if dnsErr.IsNotFound {
			return dnsErr.Err + " (no such record)"
		}
		return dnsErr.Err
	}
	return err.Error()
}

func containsAll(got, want []string) bool {
	have := make(map[string]bool, len(got))
	for _, g := range got {
		have[g] = true
	}
	for _, w := range want {
		if !have[w] {
			return false
		}
	}
	return true
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
