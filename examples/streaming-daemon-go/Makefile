# Makefile for streaming-daemon-go
#
# Automatically ensures a suitable Go toolchain (>= 1.25.7) is available,
# downloading one from go.dev if needed.  Status messages go to stderr;
# the Go binary path is captured from stdout.
#
# Usage:
#   make                  - Build the daemon binary
#   make test             - Run tests with -race
#   make fmt              - Run go fmt
#   make vet              - Run go vet
#   make clean            - Remove the built binary
#   make clean-go         - Remove downloaded Go toolchain (.goroot/)
#   make clean-all        - Remove binary and downloaded Go toolchain
#   make install          - Install to PREFIX (default /usr/local)
#   make uninstall        - Remove installed binary

MIN_GO_VERSION ?= 1.25.7
PREFIX         ?= /usr/local
BINARY         ?= streaming-daemon

# resolve_go is expanded in each recipe (not at parse time) so that
# targets like `clean` never trigger a Go download.
resolve_go = $(shell ./ensure-go.sh $(MIN_GO_VERSION))

PGO_PROFILE ?= default.pgo

.PHONY: all build build-pgo profile test fmt vet clean clean-go clean-all install uninstall

all: build

build:
	$(eval GO := $(resolve_go))
	@test -n "$(GO)" || { echo "error: ensure-go.sh failed to resolve a Go binary" >&2; exit 1; }
	$(GO) build -ldflags="-s -w" -o $(BINARY) .

# Build with Profile-Guided Optimization (requires a collected CPU profile)
build-pgo: $(PGO_PROFILE)
	$(eval GO := $(resolve_go))
	@test -n "$(GO)" || { echo "error: ensure-go.sh failed to resolve a Go binary" >&2; exit 1; }
	$(GO) build -pgo=$(PGO_PROFILE) -ldflags="-s -w" -o $(BINARY) .

# Print instructions for collecting a CPU profile for PGO
profile:
	@echo "Collect a CPU profile from a running daemon:"
	@echo "  curl -o $(PGO_PROFILE) 'http://localhost:6060/debug/pprof/profile?seconds=30'"
	@echo "Then build with: make build-pgo"

# Use /var/tmp because /tmp is often mounted noexec
test:
	$(eval GO := $(resolve_go))
	@test -n "$(GO)" || { echo "error: ensure-go.sh failed to resolve a Go binary" >&2; exit 1; }
	TMPDIR=$${TMPDIR:-/var/tmp} $(GO) test -race -v ./...

fmt:
	$(eval GO := $(resolve_go))
	@test -n "$(GO)" || { echo "error: ensure-go.sh failed to resolve a Go binary" >&2; exit 1; }
	$(GO) fmt ./...

vet:
	$(eval GO := $(resolve_go))
	@test -n "$(GO)" || { echo "error: ensure-go.sh failed to resolve a Go binary" >&2; exit 1; }
	$(GO) vet ./...

clean:
	rm -f $(BINARY)

clean-go:
	rm -rf .goroot

clean-all: clean clean-go

install: build
	install -d $(DESTDIR)$(PREFIX)/bin
	install -m 755 $(BINARY) $(DESTDIR)$(PREFIX)/bin/

uninstall:
	rm -f $(DESTDIR)$(PREFIX)/bin/$(BINARY)
