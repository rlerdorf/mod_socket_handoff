# Makefile for streaming daemon benchmarks
#
# Usage:
#   make build          - Build all daemons and dependencies
#   make test-quick     - Quick test with 100 connections
#   make bench-python   - Benchmark Python asyncio daemon
#   make bench-php      - Benchmark PHP pre-fork daemon
#   make bench-go       - Benchmark Go daemon
#   make bench-rust     - Benchmark Rust daemon
#   make bench-all      - Run all benchmarks
#   make clean          - Clean build artifacts

SHELL := /bin/bash
.PHONY: all build build-rust clean test-quick bench-python bench-php bench-go bench-rust bench-all run analyze help check-go check-php check-composer check-python check-cargo

# Directories
ROOT_DIR := $(shell dirname $(realpath $(lastword $(MAKEFILE_LIST))))
LOAD_GEN := $(ROOT_DIR)/load-generator/load-generator
SCRIPTS := $(ROOT_DIR)/scripts
RESULTS := $(ROOT_DIR)/results
EXAMPLES := $(ROOT_DIR)/../examples

# Daemon paths
PYTHON_DAEMON := $(EXAMPLES)/streaming_daemon_async.py
PHP_DAEMON := $(EXAMPLES)/streaming-daemon-amp/streaming_daemon.php
GO_DAEMON := $(EXAMPLES)/streaming-daemon-go/streaming-daemon
RUST_DAEMON := $(EXAMPLES)/streaming-daemon-rs/target/release/streaming-daemon-rs

# Socket paths
PYTHON_SOCKET := /var/run/streaming-daemon-py.sock
PHP_SOCKET := /var/run/streaming-daemon-amp.sock
GO_SOCKET := /var/run/streaming-daemon.sock
RUST_SOCKET := /var/run/streaming-daemon-rs.sock

# Default test parameters
CONNECTIONS ?= 1000
RAMP_UP ?= 10s
HOLD ?= 60s

# Timestamp for results
TIMESTAMP := $(shell date +%Y-%m-%d-%H%M%S)

all: build

# Check for required tools
check-go:
	@command -v go >/dev/null 2>&1 || { echo "Error: 'go' is required but not installed."; exit 1; }

check-php:
	@command -v php >/dev/null 2>&1 || { echo "Error: 'php' is required but not installed."; exit 1; }

check-composer:
	@command -v composer >/dev/null 2>&1 || { echo "Error: 'composer' is required but not installed. Install via: https://getcomposer.org/download/"; exit 1; }

check-python:
	@command -v python3 >/dev/null 2>&1 || { echo "Error: 'python3' is required but not installed."; exit 1; }

check-cargo:
	@command -v cargo >/dev/null 2>&1 || { echo "Error: 'cargo' is required but not installed. Install via: https://rustup.rs/"; exit 1; }

build: check-go check-php check-composer check-python
	@echo "Building load generator..."
	cd $(ROOT_DIR)/load-generator && go build -o load-generator .
	@echo "Building Go daemon..."
	cd $(EXAMPLES)/streaming-daemon-go && go build -o streaming-daemon .
	@echo "Installing PHP AMP daemon dependencies..."
	cd $(EXAMPLES)/streaming-daemon-amp && composer install --no-interaction --prefer-dist
	@echo ""
	@echo "Checking Python dependencies..."
	@python3 -c "import uvloop" 2>/dev/null && echo "  uvloop: installed (good for performance)" || \
		echo "  uvloop: not installed (optional, 2-4x faster event loop). Install with: pip install uvloop"
	@echo ""
	@# Build Rust daemon if cargo is available
	@if command -v cargo >/dev/null 2>&1; then \
		echo "Building Rust daemon (release mode)..."; \
		cd $(EXAMPLES)/streaming-daemon-rs && cargo build --release; \
		echo "Rust daemon built."; \
	else \
		echo "Note: cargo not found, skipping Rust daemon build."; \
		echo "      Install Rust via https://rustup.rs/ to build the Rust daemon."; \
	fi
	@echo ""
	@echo "Build complete."

build-rust: check-cargo
	@echo "Building Rust daemon (release mode)..."
	cd $(EXAMPLES)/streaming-daemon-rs && cargo build --release
	@echo "Rust daemon built: $(RUST_DAEMON)"

clean:
	rm -f $(LOAD_GEN)
	rm -f $(GO_DAEMON)
	rm -rf $(RESULTS)/*
	rm -rf $(EXAMPLES)/streaming-daemon-amp/vendor
	rm -f $(EXAMPLES)/streaming-daemon-amp/composer.lock
	rm -rf $(EXAMPLES)/streaming-daemon-rs/target

# Quick test to verify load generator works
test-quick: build
	@echo "Quick test with 100 connections to Go daemon..."
	@echo "Starting Go daemon..."
	sudo $(GO_DAEMON) -max-connections 1000 &
	@sleep 2
	$(LOAD_GEN) -socket $(GO_SOCKET) -connections 100 -ramp-up 5s -hold 10s -verbose
	@sudo pkill -f "$(GO_DAEMON)" || true
	@echo "Quick test complete."

# Benchmark Python asyncio daemon
bench-python: build
	@echo "=== Benchmarking Python asyncio daemon ==="
	@mkdir -p $(RESULTS)/$(TIMESTAMP)/python
	@echo "Starting Python daemon..."
	sudo python3 $(PYTHON_DAEMON) --socket $(PYTHON_SOCKET) --mode 0666 &
	@sleep 2
	@echo "Collecting resources (memory, CPU)..."
	$(SCRIPTS)/collect-resources.sh $$(pgrep -f "streaming_daemon_async" | head -1) $(RESULTS)/$(TIMESTAMP)/python/resources.csv &
	@echo "Running load test..."
	$(LOAD_GEN) -socket $(PYTHON_SOCKET) \
		-connections $(CONNECTIONS) \
		-ramp-up $(RAMP_UP) \
		-hold $(HOLD) \
		-output $(RESULTS)/$(TIMESTAMP)/python/latency.json
	@sleep 3
	@pkill -f "collect-resources.sh" || true
	@sudo pkill -f "streaming_daemon_async" || true
	@echo "Python benchmark complete."

# Benchmark PHP AMP daemon
bench-php: build
	@echo "=== Benchmarking PHP AMP daemon ==="
	@mkdir -p $(RESULTS)/$(TIMESTAMP)/php
	@echo "Starting PHP AMP daemon..."
	cd $(EXAMPLES)/streaming-daemon-amp && sudo php streaming_daemon.php --socket $(PHP_SOCKET) --mode 0666 &
	@sleep 2
	@echo "Collecting resources (memory, CPU)..."
	$(SCRIPTS)/collect-resources.sh $$(pgrep -f "streaming_daemon.php" | head -1) $(RESULTS)/$(TIMESTAMP)/php/resources.csv &
	@echo "Running load test..."
	$(LOAD_GEN) -socket $(PHP_SOCKET) \
		-connections $(CONNECTIONS) \
		-ramp-up $(RAMP_UP) \
		-hold $(HOLD) \
		-output $(RESULTS)/$(TIMESTAMP)/php/latency.json
	@sleep 3
	@pkill -f "collect-resources.sh" || true
	@sudo pkill -f "streaming_daemon.php" || true
	@echo "PHP benchmark complete."

# Benchmark Go daemon
bench-go: build
	@echo "=== Benchmarking Go daemon ==="
	@mkdir -p $(RESULTS)/$(TIMESTAMP)/go
	@echo "Starting Go daemon..."
	sudo $(GO_DAEMON) -max-connections 100000 -pprof-addr localhost:6060 -socket-mode 0666 &
	@sleep 2
	@echo "Collecting resources (memory, CPU, connections)..."
	$(SCRIPTS)/collect-resources.sh $$(pgrep -f "streaming-daemon" | head -1) $(RESULTS)/$(TIMESTAMP)/go/resources.csv 9090 &
	@echo "Running load test..."
	$(LOAD_GEN) -socket $(GO_SOCKET) \
		-connections $(CONNECTIONS) \
		-ramp-up $(RAMP_UP) \
		-hold $(HOLD) \
		-output $(RESULTS)/$(TIMESTAMP)/go/latency.json
	@sleep 3
	@pkill -f "collect-resources.sh" || true
	@sudo pkill -f "streaming-daemon" || true
	@echo "Go benchmark complete."

# Benchmark Rust daemon
bench-rust:
	@echo "=== Benchmarking Rust daemon ==="
	@mkdir -p $(RESULTS)/$(TIMESTAMP)/rust
	@if [ ! -f $(RUST_DAEMON) ]; then \
		echo "Building Rust daemon..."; \
		cd $(EXAMPLES)/streaming-daemon-rs && cargo build --release; \
	fi
	@echo "Starting Rust daemon..."
	sudo DAEMON_MAX_CONNECTIONS=100000 DAEMON_BACKEND_PROVIDER=mock \
		$(RUST_DAEMON) &
	@sleep 2
	@echo "Collecting resources (memory, CPU, connections)..."
	$(SCRIPTS)/collect-resources.sh $$(pgrep -f "streaming-daemon-rs" | head -1) $(RESULTS)/$(TIMESTAMP)/rust/resources.csv 9091 &
	@echo "Running load test..."
	$(LOAD_GEN) -socket $(RUST_SOCKET) \
		-connections $(CONNECTIONS) \
		-ramp-up $(RAMP_UP) \
		-hold $(HOLD) \
		-output $(RESULTS)/$(TIMESTAMP)/rust/latency.json
	@sleep 3
	@pkill -f "collect-resources.sh" || true
	@sudo pkill -f "streaming-daemon-rs" || true
	@echo "Rust benchmark complete."

# Run all benchmarks
bench-all: bench-python bench-php bench-go bench-rust
	@echo "=== All benchmarks complete ==="
	@echo "Results in $(RESULTS)/$(TIMESTAMP)/"

# Analyze results
analyze:
	@if [ -z "$(DIR)" ]; then \
		echo "Usage: make analyze DIR=results/YYYY-MM-DD-HHMMSS"; \
		exit 1; \
	fi
	python3 $(SCRIPTS)/analyze.py $(DIR)

# Run benchmark using the orchestration script (preferred method)
# Usage: make run DAEMON=go CONNECTIONS=1000 HOLD=30s
run:
	@if [ -z "$(DAEMON)" ]; then \
		echo "Usage: make run DAEMON=<go|rust|python|php> [CONNECTIONS=1000] [HOLD=30s] [RAMP_UP=10s]"; \
		exit 1; \
	fi
	$(SCRIPTS)/run-benchmark.sh $(DAEMON) $(CONNECTIONS) $(HOLD) $(RAMP_UP)

help:
	@echo "Streaming Daemon Benchmark Suite"
	@echo ""
	@echo "Targets:"
	@echo "  build        - Build all daemons (Go, PHP deps, Rust if cargo available), check Python"
	@echo "  build-rust   - Build Rust daemon only (release mode)"
	@echo "  test-quick   - Quick 100-connection test"
	@echo "  run          - Run benchmark for a single daemon (recommended)"
	@echo "                 Usage: make run DAEMON=go CONNECTIONS=1000 HOLD=30s"
	@echo "  bench-python - Benchmark Python asyncio daemon (starts daemon)"
	@echo "  bench-php    - Benchmark PHP AMP daemon (starts daemon)"
	@echo "  bench-go     - Benchmark Go daemon (starts daemon)"
	@echo "  bench-rust   - Benchmark Rust daemon (starts daemon)"
	@echo "  bench-all    - Run all benchmarks"
	@echo "  analyze      - Analyze results (DIR=path/to/results)"
	@echo "  clean        - Clean build artifacts (including vendor dirs, Rust target)"
	@echo ""
	@echo "Variables:"
	@echo "  DAEMON       - Daemon to benchmark: go, rust, python, php"
	@echo "  CONNECTIONS  - Target connections (default: 1000)"
	@echo "  RAMP_UP      - Ramp-up time (default: 10s)"
	@echo "  HOLD         - Hold time (default: 60s)"
	@echo ""
	@echo "Example:"
	@echo "  # Start daemon manually, then run benchmark"
	@echo "  sudo ./examples/streaming-daemon-go/streaming-daemon -socket-mode 0666 &"
	@echo "  make run DAEMON=go CONNECTIONS=500 HOLD=30s"
