#!/bin/bash
# Integration tests for Python HTTP/2 streaming daemon
#
# Tests the Python asyncio daemon with HTTP/2 multiplexing support via PycURL.
# Requires pycurl with nghttp2 support installed.
# Uses HTTPS (TLS) for proper HTTP/2 multiplexing.

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/common.sh"

# Parse arguments
FORMAT="${1:-human}"
CATEGORY="${2:-all}"

log_info "=== Python HTTP/2 Daemon Integration Tests (TLS) ==="

check_prerequisites

# Check Python prerequisites
if ! command -v python3 &>/dev/null; then
    log_error "python3 not found"
    exit 1
fi

# Check if pycurl with HTTP/2 is available
if ! python3 -c "import pycurl; vi = pycurl.version_info(); assert vi[4] & (1 << 16), 'No HTTP/2'" 2>/dev/null; then
    log_error "pycurl with HTTP/2 support not available"
    log_error "Install with: pip install pycurl (requires libcurl with nghttp2)"
    exit 1
fi

log_info "pycurl HTTP/2 support verified"

PYTHON_DAEMON="$EXAMPLES_DIR/streaming_daemon_async.py"

if [ ! -f "$PYTHON_DAEMON" ]; then
    log_error "Python daemon not found: $PYTHON_DAEMON"
    exit 1
fi

# Start mock API with TLS for proper HTTP/2 multiplexing
start_mock_api_tls

# Start Python daemon with HTTP/2 enabled (using HTTPS)
log_info "Starting Python HTTP/2 daemon on $DAEMON_SOCKET..."
rm -f "$DAEMON_SOCKET"

OPENAI_HTTP2_ENABLED=true \
OPENAI_INSECURE_SSL=true \
OPENAI_API_BASE="https://127.0.0.1:$MOCK_API_PORT/v1" \
OPENAI_API_KEY="test-key" \
    python3 "$PYTHON_DAEMON" -s "$DAEMON_SOCKET" -m 0666 --backend openai &
DAEMON_PID=$!

if ! wait_for_socket "$DAEMON_SOCKET" 10; then
    log_error "Python HTTP/2 daemon failed to start"
    exit 1
fi

log_info "Python HTTP/2 daemon running (PID $DAEMON_PID)"

# Run tests (capture exit code even with set -e)
run_tests "python-http2" "$FORMAT" "$CATEGORY" && EXIT_CODE=0 || EXIT_CODE=$?

log_info "Tests complete (exit code: $EXIT_CODE)"
exit $EXIT_CODE
