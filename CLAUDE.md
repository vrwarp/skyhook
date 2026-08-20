# Working on Skyhook's tests

This file is about writing and debugging the tests. For what Skyhook is, read
[README.md](README.md); for what is built and why it diverges from the design,
[docs/IMPLEMENTATION.md](docs/IMPLEMENTATION.md).

## The three suites

| | where | what it needs | run it with |
|---|---|---|---|
| unit | `internal/...`, `cmd/...` | nothing | `make test` |
| end-to-end | `test/` (package `e2e`) | a Chromium, and `client/dist` for the PWA tests | `make test-e2e` |
| parity | `test/` (`TestParity*`), data in `test/parity/` | a Chromium and `client/dist` | `make test-parity` |

`make test` deliberately excludes `./test` — the e2e package needs a browser and
runs in its own CI job. It also runs with `-race`, which the e2e suite does not.
The parity tests live in the same package but run as their own suite and CI
job: `test-e2e` and `test-slow` carry `-skip '^TestParity'` so their measured
timings stay measured over the same set of tests.

The e2e tests **skip** when there is no Chromium, so `go test ./...` stays useful
on a machine without one. `SKYHOOK_E2E=1` turns that skip into a failure, which
is what CI sets: skipping for want of a browser is a failure there, not a pass.
The PWA tests skip on the same terms when `client/dist` is missing — build it
with `cd client && npm ci && npm run build`.

Cross-language wire fixtures live in `testdata/`. If you change the protocol,
regenerate them with `make fixtures` and expect the conformance test to be the
thing that tells you the wire format drifted.

## Running the e2e suite over the bad link

The link is the whole point of the project, so a change to the mirror, the
protocol or the transport is not tested until it has run against 1.2 s RTT /
250 kbps / 2% loss:

```sh
make test-slow            # shapes eight lanes, runs eight-wide, unshapes
LANES=4 make test-slow    # narrower, on a smaller box
```

This needs `tc` (`iproute2`) and root for the shaping. Kernels without
`sch_prio`/`sch_netem` — some containers and microVMs — cannot run it at all,
and fail with `Specified qdisc kind is unknown`. That is the environment, not
your change; use CI to exercise it.

## Writing an e2e test

**Start from a harness constructor.** Never build the pieces by hand:

```go
func TestSomethingArrives(t *testing.T) {
    h := newHarness(t)                                     // not t.Parallel()
    ctx, cancel := context.WithTimeout(context.Background(),
        budget(120*time.Second))                            // always budget()
    defer cancel()
    cl := h.connect(ctx, "")
    tab := h.openFixture(ctx, cl)
    ...
}
```

| constructor | for |
|---|---|
| `newHarness(t)` | almost everything |
| `newHarnessWith(t, tweak)` | when you need to adjust `session.ManagerOptions` |
| `newPWAHarness(t)` | driving the real client app in a real browser |
| `newPWAHarnessAt(t, dist)` | serving a *particular* build of the app, for deploy/version tests |
| `newSerialHarness(t)` | only for an assertion that measures this machine |

The rules that are not obvious, each of which has already cost a debugging
session:

- **Do not call `t.Parallel()` yourself.** The harness does it, so that a test
  cannot forget — a test that forgets does not fail, it quietly serialises
  itself and costs a shaped lane, and nothing reports that. Calling it twice on
  one `*testing.T` panics.

- **Do not open a `context` before the harness.** `t.Parallel()` parks the test
  until the serial phase finishes; a deadline started before that is already
  running down while the test is parked. Harness first, `context` second.

- **Wrap every timeout in `budget()`.** It multiplies by 3 when
  `SKYHOOK_SLOW_LINK=1`, which is the difference between a test that passes over
  the emulated link and one that is a coin toss. `slowLink()` is there if you
  need to branch on it outright.

- **Take the logger from the harness.** `h.log` for anything you start
  yourself. Never `os.Stderr`, never `slog.Default()` — both bypass the
  per-test log and put unattributed lines into a stream shared with seven other
  tests. `cdp.Launch` falls back to `slog.Default()` when given no `Logger`, so
  pass one.

- **Use `newSerialHarness` only for wall-clock assertions about the machine**,
  like `TestOneUsedCSSPassStaysCheap`'s 400 ms bound. A box running eight
  browsers would report the runner as a regression. Byte-count assertions are
  fine in parallel; millisecond ones are not.

- **Everything else is already isolated.** Each test gets its own Chromium,
  fixture and CDN servers on ephemeral ports, manager, image pipeline, and
  `t.TempDir()` for every directory. Do not add shared state, and do not reach
  for a fixed port: the only fixed ports are the shaped lanes, and the harness
  leases those for you.

### If you touch the shaped-lane machinery

A test leases one shaped port for its lifetime from a pool sized by
`SKYHOOK_TEST_PORTS`, and returns it in a cleanup. **The lease must happen after
`t.Parallel()`.** A test that leases before it parks holds a lane while waiting
to be resumed, and with as many such tests as there are lanes, nothing is left
to hand one back — the suite deadlocks rather than failing. `leaseShapedAddr` is
called from inside the harness for exactly this reason.

## Working on the parity suite

[docs/PARITY.md](docs/PARITY.md) explains the system; the corpus authoring
rules are in [test/parity/README.md](test/parity/README.md). The workflow
rules that are not obvious:

- **Never edit a baseline by hand.** `test/parity/baselines/*.json` are
  measurements. `make parity-baseline` rewrites them (and the registry table
  in docs/PARITY.md); the diff of that run is the review artifact.
- **A gated failure is never just accepted.** Either it is a regression to
  fix, or it is a real gap: give it an id in `test/parity/gaps.json`, an
  `expectedFail` entry in the page's manifest, and let the ratchet pin its
  shape. The runner enforces this — an uncatalogued failure fails the suite
  even when the baseline agrees with it.
- **An expected failure that starts passing is a fix.** The suite fails with
  a lock-in message: flip the gap's status, drop the `expectedFail`, and
  re-baseline in the same commit as the code change.
- **Corpus pages are static by rule.** The settle barrier treats a page that
  never quiets as a bug, not a flake; a page that needs to move does it in
  manifest `interactions`, which re-settle between every step.

`TestParityBundleTools` is also a conformance test: it captures a real bundle
through the real client UI and asserts `skyhookctl bundle triage` reads it
back clean, so a new agent drop rule or client synthetic attribute shows up
there as a triage divergence — update the tables in
`internal/parity/triage.go` when it does.

## Debugging a failing test

**Read the failure dump first.** A test that fails prints its own server log,
complete and down to DEBUG, indented under its own name, with or without `-v`.
A test that passes prints nothing. This is the whole reason the log is not
written to stderr: eight concurrent tests interleaved line by line is not a log.

```
--- FAIL: TestThing (12.30s)
    thing_test.go:42: the mirror never arrived
    logging_test.go:108: server log for TestThing (last 37 records, DEBUG and up):
        time=... level=DEBUG msg=...
```

Then, in roughly this order:

```sh
# One test, with the full log live rather than only on failure.
SKYHOOK_TEST_LOG=debug go test ./test -run TestThing -v

# The same test over the bad link, which is where timing bugs actually live.
sudo scripts/netem.sh lanes 21123 1 1200 250 2
SKYHOOK_E2E=1 SKYHOOK_SLOW_LINK=1 SKYHOOK_TEST_PORTS=21123 \
  go test ./test -run TestThing -v
sudo scripts/netem.sh down
```

`SKYHOOK_TEST_LOG` takes `debug`, `info`, `warn` (the default) or `error`, and
sets only what reaches stderr live. While a suite runs, stderr carries WARN and
worse, tagged `test=<name>` so a line can always be traced back.

Other things worth knowing when a test misbehaves:

- **A hung test prints nothing**, because the dump runs at cleanup and cleanup
  never comes. `go test -timeout` is the tool: it panics with a goroutine dump
  for every live goroutine, which is what you want for a hang anyway. Do not
  raise the timeout to make a hang go away.

- **`h.logs` is the ring** the dump prints from, and a test can read it directly
  with `h.logs.Text()` to assert on what the server logged — `busy_test.go` does.
  It holds the last 500 records. `Capture` bundles the same ring, so **do not
  change what goes into it**: per-test attributes belong on the stderr handler,
  not on the logger.

- **Reproducing a parallel-only failure** means keeping the concurrency:
  `-run 'TestA|TestB' -parallel 2`. Running the test alone is the fastest way to
  make an ordering bug disappear without fixing it.

- **Flakiness is a diagnosis of last resort.** Re-running is the fix only when
  the job died before any test body ran. Everything else gets root-caused, and
  no test gets skipped or quarantined to get to green.

## Before you push

```sh
make lint          # go vet, gofmt, and the client's lint + typecheck
make test          # unit tests, with -race
make test-e2e      # needs a browser
make test-parity   # when the mirror, protocol, client or corpus changed
```

CI runs the same commands and then some: `make test-slow`'s shaped suite,
`golangci-lint` (which `make lint` does not run), shellcheck over
`scripts/*.sh deploy/*.sh`, and CodeQL.

Timing numbers quoted in comments (`ci.yml`, `README.md`) are measurements, not
estimates. If you change the shape of the suite, re-measure and say which run
the number came from.
