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

# Needs a Chromium; skips without one unless SKYHOOK_E2E=1.
test-e2e:
	SKYHOOK_E2E=1 go test -count=1 -timeout 20m -v ./test

# The link the whole project exists for: 1.2s RTT, 250 kbps, 2% loss.
test-slow:
	sudo scripts/netem.sh port 45123 1200 250 2
	SKYHOOK_E2E=1 SKYHOOK_SLOW_LINK=1 SKYHOOK_TEST_PORT=45123 \
		go test -count=1 -timeout 25m -v ./test; \
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
