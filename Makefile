.PHONY: all build build-control build-agent build-web test test-go test-web lint clean

VERSION ?= dev
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo unknown)
LDFLAGS = -s -w -X github.com/l4d2-panel/l4d2panel/internal/version.Version=$(VERSION) -X github.com/l4d2-panel/l4d2panel/internal/version.Commit=$(COMMIT) -X github.com/l4d2-panel/l4d2panel/internal/version.Date=$(BUILD_DATE)

all: test build

build: build-control build-agent

build-control: build-web
	go build -trimpath -ldflags="$(LDFLAGS)" -o bin/l4d2panel-control ./cmd/control

build-agent:
	go build -trimpath -ldflags="$(LDFLAGS)" -o bin/l4d2panel-agent ./cmd/agent

build-web:
	cd web && npm ci && npm run build
	rm -rf internal/control/static/assets
	cp web/dist/index.html internal/control/static/index.html
	cp -R web/dist/assets internal/control/static/assets
	cp -f web/dist/*.png web/dist/*.svg web/dist/*.ico internal/control/static/ 2>/dev/null || true

test: test-go test-web

test-go:
	go test ./...

test-web:
	cd web && npm test -- --run

lint:
	go vet ./...
	cd web && npm run typecheck

clean:
	go clean
	rm -rf bin web/dist coverage
