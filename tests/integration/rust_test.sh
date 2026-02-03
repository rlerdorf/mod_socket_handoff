#!/bin/bash
# Integration tests for Rust streaming daemon

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/common.sh"

# Parse arguments
FORMAT="${1:-human}"
CATEGORY="${2:-all}"

log_info "=== Rust Daemon Integration Tests ==="

check_prerequisites

# Build Rust daemon if needed
RUST_DAEMON_DIR="$EXAMPLES_DIR/streaming-daemon-rs"
RUST_DAEMON_BIN="$RUST_DAEMON_DIR/target/release/streaming-daemon-rs"

if [ ! -d "$RUST_DAEMON_DIR" ]; then
    log_error "Rust daemon directory not found: $RUST_DAEMON_DIR"
    exit 1
fi

if [ ! -x "$RUST_DAEMON_BIN" ]; then
    log_info "Building Rust daemon..."
    (cd "$RUST_DAEMON_DIR" && cargo build --release)
fi

# Start mock API with TLS
start_mock_api_tls

# Start Rust daemon
log_info "Starting Rust daemon on $DAEMON_SOCKET..."
rm -f "$DAEMON_SOCKET"

OPENAI_API_BASE="https://127.0.0.1:$MOCK_API_PORT/v1" \
OPENAI_API_KEY="test-key" \
OPENAI_INSECURE_SSL="true" \
DAEMON_SOCKET_MODE=0666 \
    "$RUST_DAEMON_BIN" --socket "$DAEMON_SOCKET" --backend openai &
DAEMON_PID=$!

if ! wait_for_socket "$DAEMON_SOCKET" 10; then
    log_error "Rust daemon failed to start"
    exit 1
fi

log_info "Rust daemon running (PID $DAEMON_PID)"

# Run tests
run_tests "rust" "$FORMAT" "$CATEGORY"
EXIT_CODE=$?

log_info "Tests complete (exit code: $EXIT_CODE)"
exit $EXIT_CODE
