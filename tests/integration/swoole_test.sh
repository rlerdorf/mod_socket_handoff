#!/bin/bash
# Integration tests for PHP streaming daemon (Swoole)

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/common.sh"

# Parse arguments
FORMAT="${1:-human}"
CATEGORY="${2:-all}"

log_info "=== PHP Daemon Integration Tests (Swoole) ==="

check_prerequisites
check_command php

# Check for Swoole extension - try loading explicitly if not already loaded
PHP_CMD="php"
if ! php -m 2>/dev/null | grep -qi swoole; then
    # Try loading Swoole extension explicitly
    if php -d extension=swoole.so -m 2>/dev/null | grep -qi swoole; then
        PHP_CMD="php -d extension=swoole.so"
        log_info "Loading Swoole extension explicitly"
    else
        log_error "Swoole extension not installed"
        exit 1
    fi
fi

# Check PHP daemon
PHP_DAEMON_DIR="$EXAMPLES_DIR/streaming-daemon-swoole"
PHP_DAEMON="$PHP_DAEMON_DIR/streaming_daemon.php"

if [ ! -f "$PHP_DAEMON" ]; then
    log_error "Swoole daemon not found: $PHP_DAEMON"
    exit 1
fi

# Start mock API with TLS to test HTTP/2 ALPN path
start_mock_api_tls

# Start Swoole daemon with TLS + HTTP/2
log_info "Starting Swoole daemon on $DAEMON_SOCKET..."
rm -f "$DAEMON_SOCKET"

OPENAI_API_BASE="https://127.0.0.1:$MOCK_API_PORT/v1" \
OPENAI_INSECURE_SSL="true" \
    $PHP_CMD "$PHP_DAEMON" \
        --socket "$DAEMON_SOCKET" \
        --mode 0666 \
        --backend openai &
DAEMON_PID=$!

if ! wait_for_socket "$DAEMON_SOCKET" 10; then
    log_error "Swoole daemon failed to start"
    exit 1
fi

log_info "Swoole daemon running (PID $DAEMON_PID)"

# Run tests
run_tests "swoole" "$FORMAT" "$CATEGORY"
EXIT_CODE=$?

log_info "Tests complete (exit code: $EXIT_CODE)"
exit $EXIT_CODE
