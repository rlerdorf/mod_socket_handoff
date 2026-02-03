#!/bin/bash
# Integration tests for C io_uring streaming daemon

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/common.sh"

# Parse arguments
FORMAT="${1:-human}"
CATEGORY="${2:-all}"

log_info "=== io_uring Daemon Integration Tests ==="

check_prerequisites

# Build uring daemon if needed
URING_DAEMON_DIR="$EXAMPLES_DIR/streaming-daemon-uring"
URING_DAEMON_BIN="$URING_DAEMON_DIR/streaming-daemon-uring"

if [ ! -d "$URING_DAEMON_DIR" ]; then
    log_error "io_uring daemon directory not found: $URING_DAEMON_DIR"
    exit 1
fi

if [ ! -x "$URING_DAEMON_BIN" ]; then
    log_info "Building io_uring daemon with OpenAI backend..."
    make -C "$URING_DAEMON_DIR" clean >/dev/null 2>&1 || true
    make -C "$URING_DAEMON_DIR" BACKEND=openai -j$(nproc)
fi

# Start mock API with TLS
start_mock_api_tls

# Start uring daemon
log_info "Starting io_uring daemon on $DAEMON_SOCKET..."
rm -f "$DAEMON_SOCKET"

OPENAI_API_BASE="https://127.0.0.1:$MOCK_API_PORT/v1" \
OPENAI_API_KEY="test-key" \
OPENAI_INSECURE_SSL="true" \
    "$URING_DAEMON_BIN" --socket "$DAEMON_SOCKET" --socket-mode 0666 &
DAEMON_PID=$!

if ! wait_for_socket "$DAEMON_SOCKET" 10; then
    log_error "io_uring daemon failed to start"
    exit 1
fi

log_info "io_uring daemon running (PID $DAEMON_PID)"

# Run tests (capture exit code even with set -e)
run_tests "uring" "$FORMAT" "$CATEGORY" && EXIT_CODE=0 || EXIT_CODE=$?

log_info "Tests complete (exit code: $EXIT_CODE)"
exit $EXIT_CODE
