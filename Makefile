# Convenience targets. CI runs the same commands.
SHELL := /bin/bash
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build test test-e2e test-slow lint client client-test fmt docker clean

all: build client

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/skyhookd ./cmd/skyhookd
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/skyhookctl ./cmd/skyhookctl

test:
	go test -race -count=1 $$(go list ./... | grep -v '/test$$')

# How many e2e tests run at once. Each owns a Chromium and the PWA ones own
# two, so the ceiling is the box; under test-slow it must also equal the number
# of shaped lanes, because that is what a test leases.
LANES ?= 4
LANE_BASE ?= 45123

# Needs a Chromium; skips without one unless SKYHOOK_E2E=1.
test-e2e:
	SKYHOOK_E2E=1 go test -count=1 -timeout 20m -parallel $(LANES) -v ./test

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
		go test -count=1 -timeout 25m -parallel $(LANES) -v ./test; \
		status=$$?; sudo scripts/netem.sh down; exit $$status

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
