#!/bin/bash
# Integration tests for Go streaming daemon

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/common.sh"

# Parse arguments
FORMAT="${1:-human}"
CATEGORY="${2:-all}"

log_info "=== Go Daemon Integration Tests ==="

check_prerequisites

# Build Go daemon if needed
GO_DAEMON_DIR="$EXAMPLES_DIR/streaming-daemon-go"
GO_DAEMON_BIN="$GO_DAEMON_DIR/streaming-daemon"

if [ ! -d "$GO_DAEMON_DIR" ]; then
    log_error "Go daemon directory not found: $GO_DAEMON_DIR"
    exit 1
fi

if [ ! -x "$GO_DAEMON_BIN" ]; then
    log_info "Building Go daemon..."
    (cd "$GO_DAEMON_DIR" && go build -o streaming-daemon streaming_daemon.go)
fi

# Start mock API with TLS
start_mock_api_tls

# Start Go daemon
log_info "Starting Go daemon on $DAEMON_SOCKET..."
rm -f "$DAEMON_SOCKET"

OPENAI_API_BASE="https://127.0.0.1:$MOCK_API_PORT/v1" \
OPENAI_API_KEY="test-key" \
OPENAI_INSECURE_SSL="true" \
    "$GO_DAEMON_BIN" -socket "$DAEMON_SOCKET" -socket-mode 0666 -backend openai &
DAEMON_PID=$!

if ! wait_for_socket "$DAEMON_SOCKET" 10; then
    log_error "Go daemon failed to start"
    exit 1
fi

log_info "Go daemon running (PID $DAEMON_PID)"

# Run tests
run_tests "go" "$FORMAT" "$CATEGORY"
EXIT_CODE=$?

log_info "Tests complete (exit code: $EXIT_CODE)"
exit $EXIT_CODE
