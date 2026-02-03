#!/bin/bash
# Integration tests for PHP streaming daemon (Swow)

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/common.sh"

# Parse arguments
FORMAT="${1:-human}"
CATEGORY="${2:-all}"

log_info "=== PHP Daemon Integration Tests (Swow) ==="

check_prerequisites
check_command php

# Check for Swow extension - try loading explicitly if not already loaded
PHP_CMD="php"
if ! php -m 2>/dev/null | grep -qi swow; then
    # Try loading Swow extension explicitly
    if php -d extension=swow.so -m 2>/dev/null | grep -qi swow; then
        PHP_CMD="php -d extension=swow.so"
        log_info "Loading Swow extension explicitly"
    else
        log_error "Swow extension not installed"
        exit 1
    fi
fi

# Check PHP daemon
PHP_DAEMON_DIR="$EXAMPLES_DIR/streaming-daemon-swow"
PHP_DAEMON="$PHP_DAEMON_DIR/streaming_daemon.php"

if [ ! -f "$PHP_DAEMON" ]; then
    log_error "Swow daemon not found: $PHP_DAEMON"
    exit 1
fi

# Start mock API with TLS for HTTP/2 multiplexing support
start_mock_api_tls

# Start Swow daemon
log_info "Starting Swow daemon on $DAEMON_SOCKET..."
rm -f "$DAEMON_SOCKET"

# Use HTTPS for HTTP/2 connection multiplexing (Swow's curl_multi fix works with TLS)
OPENAI_API_BASE="https://127.0.0.1:$MOCK_API_PORT/v1" \
    $PHP_CMD "$PHP_DAEMON" \
        --socket "$DAEMON_SOCKET" \
        --mode 0666 \
        --backend openai &
DAEMON_PID=$!

if ! wait_for_socket "$DAEMON_SOCKET" 10; then
    log_error "Swow daemon failed to start"
    exit 1
fi

log_info "Swow daemon running (PID $DAEMON_PID)"

# Run tests
run_tests "swow" "$FORMAT" "$CATEGORY"
EXIT_CODE=$?

log_info "Tests complete (exit code: $EXIT_CODE)"
exit $EXIT_CODE
