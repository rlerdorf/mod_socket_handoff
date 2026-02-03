#!/bin/bash
# Integration tests for PHP streaming daemon (AMPHP)

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/common.sh"

# Parse arguments
FORMAT="${1:-human}"
CATEGORY="${2:-all}"

log_info "=== PHP Daemon Integration Tests (AMPHP) ==="

check_prerequisites
check_command php

# Check PHP daemon dependencies
PHP_DAEMON_DIR="$EXAMPLES_DIR/streaming-daemon-amp"
PHP_DAEMON="$PHP_DAEMON_DIR/streaming_daemon.php"

if [ ! -f "$PHP_DAEMON" ]; then
    log_error "PHP daemon not found: $PHP_DAEMON"
    exit 1
fi

# Check/install composer dependencies
if [ ! -d "$PHP_DAEMON_DIR/vendor" ]; then
    log_info "Installing PHP dependencies..."
    (cd "$PHP_DAEMON_DIR" && composer install --no-dev)
fi

# Start mock API with TLS for HTTP/2 multiplexing support
start_mock_api_tls

# Start PHP daemon with HTTP/2 over HTTPS
log_info "Starting PHP daemon on $DAEMON_SOCKET..."
rm -f "$DAEMON_SOCKET"

# Use HTTPS for HTTP/2 connection multiplexing
OPENAI_API_BASE="https://127.0.0.1:$MOCK_API_PORT/v1" \
    OPENAI_INSECURE_SSL=true \
    php "$PHP_DAEMON" \
        --socket "$DAEMON_SOCKET" \
        --mode 0666 \
        --backend openai &
DAEMON_PID=$!

if ! wait_for_socket "$DAEMON_SOCKET" 10; then
    log_error "PHP daemon failed to start"
    exit 1
fi

log_info "PHP daemon running (PID $DAEMON_PID)"

# Run tests
run_tests "php" "$FORMAT" "$CATEGORY"
EXIT_CODE=$?

log_info "Tests complete (exit code: $EXIT_CODE)"
exit $EXIT_CODE
