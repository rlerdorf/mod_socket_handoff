# Test Framework Makefile
#
# Usage:
#   make test          - Run all daemon tests (Go, Rust, PHP, Swoole, Swow, Python, io_uring)
#   make test-go       - Test Go daemon only
#   make test-rust     - Test Rust daemon only
#   make test-php      - Test PHP AMP daemon only
#   make test-swoole   - Test PHP Swoole daemon only
#   make test-swow     - Test PHP Swow daemon only
#   make test-python-http2 - Test Python HTTP/2 daemon only
#   make test-uring    - Test C io_uring daemon only
#   make test-category CATEGORY=unicode  - Run specific category across all daemons
#   make build         - Build test runner
#   make clean         - Clean build artifacts
#   make list          - List available test scenarios

.PHONY: all build test test-go test-rust test-php test-swoole test-swow test-python-http2 test-uring test-category list clean help

# Configuration
FORMAT ?= human
CATEGORY ?= all
TEST_RUNNER = ./test-runner

all: build

# Build the test runner
build:
	go build -o test-runner ./runner

# Run all daemon tests (HTTP/2 with TLS)
test: build test-go test-rust test-php test-swoole test-swow test-python-http2 test-uring
	@echo "All daemon tests complete"

# Test Go daemon
test-go: build
	@chmod +x integration/go_test.sh
	integration/go_test.sh $(FORMAT) $(CATEGORY)

# Test Rust daemon
test-rust: build
	@chmod +x integration/rust_test.sh
	integration/rust_test.sh $(FORMAT) $(CATEGORY)

# Test PHP daemon (AMPHP)
test-php: build
	@chmod +x integration/php_test.sh
	integration/php_test.sh $(FORMAT) $(CATEGORY)

# Test PHP daemon (Swoole)
test-swoole: build
	@chmod +x integration/swoole_test.sh
	integration/swoole_test.sh $(FORMAT) $(CATEGORY)

# Test PHP daemon (Swow)
test-swow: build
	@chmod +x integration/swow_test.sh
	integration/swow_test.sh $(FORMAT) $(CATEGORY)

# Test Python HTTP/2 daemon
test-python-http2: build
	@chmod +x integration/python_http2_test.sh
	integration/python_http2_test.sh $(FORMAT) $(CATEGORY)

# Test C io_uring daemon
test-uring: build
	@chmod +x integration/uring_test.sh
	integration/uring_test.sh $(FORMAT) $(CATEGORY)

# Run tests for a specific category across all daemons
test-category: build
	@echo "Running $(CATEGORY) tests..."
	@chmod +x integration/*.sh
	integration/go_test.sh $(FORMAT) $(CATEGORY) || true
	integration/rust_test.sh $(FORMAT) $(CATEGORY) || true
	integration/php_test.sh $(FORMAT) $(CATEGORY) || true
	integration/swoole_test.sh $(FORMAT) $(CATEGORY) || true
	integration/swow_test.sh $(FORMAT) $(CATEGORY) || true
	integration/python_http2_test.sh $(FORMAT) $(CATEGORY) || true
	integration/uring_test.sh $(FORMAT) $(CATEGORY) || true

# List available test scenarios
list: build
	$(TEST_RUNNER) -list

# Clean build artifacts
clean:
	rm -f test-runner
	rm -f /tmp/mock-llm-api-test.sock
	rm -f /tmp/streaming-daemon-test.sock

# TAP output for CI
test-tap: FORMAT=tap
test-tap: test

# JSON output for analysis
test-json: FORMAT=json
test-json: test

help:
	@echo "Test Framework Makefile"
	@echo ""
	@echo "Targets:"
	@echo "  make test          - Run all daemon tests"
	@echo "  make test-go       - Test Go daemon only"
	@echo "  make test-rust     - Test Rust daemon only"
	@echo "  make test-php      - Test PHP daemon (AMPHP) only"
	@echo "  make test-swoole   - Test PHP daemon (Swoole) only"
	@echo "  make test-swow     - Test PHP daemon (Swow) only"
	@echo "  make test-python-http2 - Test Python HTTP/2 daemon only"
	@echo "  make test-uring    - Test C io_uring daemon only"
	@echo "  make test-tap      - Run tests with TAP output (for CI)"
	@echo "  make test-json     - Run tests with JSON output (for analysis)"
	@echo "  make list          - List available test scenarios"
	@echo "  make build         - Build test runner"
	@echo "  make clean         - Clean build artifacts"
	@echo ""
	@echo "Variables:"
	@echo "  FORMAT=human|tap|json  - Output format (default: human)"
	@echo "  CATEGORY=all|basic|unicode|chunks|abort|finish"
	@echo ""
	@echo "Examples:"
	@echo "  make test-go CATEGORY=unicode"
	@echo "  make test FORMAT=tap"
