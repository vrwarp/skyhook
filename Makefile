# Convenience targets. CI runs the same commands.
SHELL := /bin/bash
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build test test-e2e test-slow test-parity parity-baseline lint client client-test fmt docker clean

all: build client

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/skyhookd ./cmd/skyhookd
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/skyhookctl ./cmd/skyhookctl

test:
	go test -race -count=1 $$(go list ./... | grep -v '/test$$')

# How many e2e tests run at once, and so how many shaped lanes test-slow builds
# — a test leases one, so the two numbers have to match.
#
# Eight because the shaped suite is waiting, not working: measured over the
# emulated link, the tests total ~2135s of wall clock but only ~439s of that is
# CPU, and eight of them at once need about 1.6 of the runner's 4 cores. The
# unshaped suite already sustains 2.66 cores, so this is headroom that has been
# demonstrated rather than hoped for. Past eight the returns thin out: the
# longest single test is ~91s and becomes the floor.
LANES ?= 8
LANE_BASE ?= 21123

# Needs a Chromium; skips without one unless SKYHOOK_E2E=1.
#
# Four rather than $(LANES): nothing is shaped here, so these tests are working
# rather than waiting and the runner's cores are the limit. At four they
# already keep 2.66 of them busy, and oversubscribing buys little.
#
# -skip keeps the parity suite out: it has its own target and its own CI job,
# and this suite's quoted timings were measured over exactly the tests that
# remain. The set here is byte-identical to what it was before parity existed.
test-e2e:
	SKYHOOK_E2E=1 go test -count=1 -timeout 20m -parallel 4 -skip '^TestParity' -v ./test

# The link the whole project exists for: 1.2s RTT, 250 kbps, 2% loss.
#
# One shaped lane per concurrent test rather than one shaped port between them:
# a netem qdisc's rate is a budget for everything queued into it, so tests
# sharing a port would divide the 250 kbit and finish no sooner than they would
# have one after another. See the comment in scripts/netem.sh.
test-slow:
	sudo scripts/netem.sh lanes $(LANE_BASE) $(LANES) 1200 250 2
	SKYHOOK_E2E=1 SKYHOOK_SLOW_LINK=1 \
		SKYHOOK_TEST_PORTS=$(LANE_BASE)-$$(( $(LANE_BASE) + $(LANES) - 1 )) \
		go test -count=1 -timeout 25m -parallel $(LANES) -skip '^TestParity' -v ./test; \
		status=$$?; sudo scripts/netem.sh down; exit $$status

# The rendering-parity suite: the corpus under test/parity/corpus, measured
# through the real client and held against the checked-in baselines. Unshaped
# on purpose — parity is a correctness property, the settled document is the
# same at any link speed, and the netem job runs at its measured capacity.
# Needs a browser and client/dist, like test-e2e.
#
# Wall clock: measure on the first CI run and record it here.
test-parity:
	SKYHOOK_E2E=1 go test -count=1 -timeout 30m -parallel 4 -run '^TestParity' -v ./test

# Locks in a deliberate parity change: reruns the suite writing baselines
# instead of holding them, then regenerates the registry table in
# docs/PARITY.md. Review the diff — the diff is the change.
parity-baseline:
	SKYHOOK_E2E=1 SKYHOOK_UPDATE_PARITY=1 go test -count=1 -timeout 30m -parallel 4 -run '^TestParity' -v ./test
	SKYHOOK_UPDATE_PARITY_DOCS=1 go test -count=1 ./internal/parity -run Registry

lint:
	go vet ./...
	gofmt -l . | tee /dev/stderr | (! read)
	cd client && npm run lint && npm run typecheck

fmt:
	gofmt -w .

client:
	cd client && npm ci && npm run build

client-test:
	cd client && npm test

fixtures:
	SKYHOOK_UPDATE_FIXTURES=1 go test ./internal/protocol -run Conformance

docker:
	docker build -f deploy/Dockerfile -t skyhook:$(VERSION) --build-arg VERSION=$(VERSION) .

clean:
	rm -rf bin client/dist client/release .devdata
