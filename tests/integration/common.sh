#!/bin/bash
# Common utilities for daemon integration tests

set -e

# Paths
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TESTS_DIR="$(dirname "$SCRIPT_DIR")"
PROJECT_ROOT="$(dirname "$TESTS_DIR")"
EXAMPLES_DIR="$PROJECT_ROOT/examples"
TEST_RUNNER="$TESTS_DIR/test-runner"

# Default ports and sockets
# Use port 28080 for tests to avoid conflicts with benchmarks (which use 8080)
MOCK_API_PORT="${MOCK_API_PORT:-28080}"
MOCK_API_SOCKET="${MOCK_API_SOCKET:-/tmp/mock-llm-api-test.sock}"
DAEMON_SOCKET="${DAEMON_SOCKET:-/tmp/streaming-daemon-test.sock}"

# Kill any stray processes from previous runs
cleanup_stale_processes() {
    # Kill processes using the mock API port
    # Try fuser first (works without root for owned processes)
    if fuser -k "$MOCK_API_PORT/tcp" 2>/dev/null; then
        log_warn "Killed stale process on port $MOCK_API_PORT"
        sleep 0.5
    fi

    # Also try pkill for any lingering mock-llm-api or streaming-daemon
    pkill -f "mock-llm-api.*$MOCK_API_PORT" 2>/dev/null || true
    pkill -f "streaming-daemon.*$DAEMON_SOCKET" 2>/dev/null || true

    # Remove stale sockets
    rm -f "$DAEMON_SOCKET" "$MOCK_API_SOCKET" 2>/dev/null || true
}

# PIDs for cleanup
MOCK_API_PID=""
DAEMON_PID=""

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
NC='\033[0m' # No Color

log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Cleanup function
cleanup() {
    log_info "Cleaning up..."

    if [ -n "$DAEMON_PID" ] && kill -0 "$DAEMON_PID" 2>/dev/null; then
        log_info "Stopping daemon (PID $DAEMON_PID)"
        kill "$DAEMON_PID" 2>/dev/null || true
        wait "$DAEMON_PID" 2>/dev/null || true
    fi

    if [ -n "$MOCK_API_PID" ] && kill -0 "$MOCK_API_PID" 2>/dev/null; then
        log_info "Stopping mock API (PID $MOCK_API_PID)"
        kill "$MOCK_API_PID" 2>/dev/null || true
        wait "$MOCK_API_PID" 2>/dev/null || true
    fi

    # Clean up socket files
    rm -f "$DAEMON_SOCKET" "$MOCK_API_SOCKET" 2>/dev/null || true
}

# Set up trap for cleanup
trap cleanup EXIT

# Wait for a socket to become available
wait_for_socket() {
    local socket="$1"
    local timeout="${2:-10}"
    local elapsed=0

    while [ ! -S "$socket" ] && [ $elapsed -lt $timeout ]; do
        sleep 0.1
        elapsed=$((elapsed + 1))
    done

    if [ ! -S "$socket" ]; then
        log_error "Socket $socket not available after ${timeout}s"
        return 1
    fi

    return 0
}

# Wait for a TCP port to become available
wait_for_port() {
    local port="$1"
    local timeout="${2:-10}"
    local elapsed=0

    while ! nc -z 127.0.0.1 "$port" 2>/dev/null && [ $elapsed -lt $timeout ]; do
        sleep 0.1
        elapsed=$((elapsed + 1))
    done

    if ! nc -z 127.0.0.1 "$port" 2>/dev/null; then
        log_error "Port $port not available after ${timeout}s"
        return 1
    fi

    return 0
}

# Start mock LLM API (HTTP)
start_mock_api() {
    local mock_api_bin="$EXAMPLES_DIR/mock-llm-api/target/release/mock-llm-api"

    if [ ! -x "$mock_api_bin" ]; then
        log_info "Building mock-llm-api..."
        (cd "$EXAMPLES_DIR/mock-llm-api" && cargo build --release)
    fi

    # Clean up any stale processes first
    cleanup_stale_processes

    log_info "Starting mock API on port $MOCK_API_PORT..."
    "$mock_api_bin" \
        --listen "127.0.0.1:$MOCK_API_PORT" \
        --chunk-delay-ms 10 \
        --quiet &
    MOCK_API_PID=$!

    if ! wait_for_port "$MOCK_API_PORT" 10; then
        log_error "Mock API failed to start"
        return 1
    fi

    log_info "Mock API running (PID $MOCK_API_PID)"
}

# Start mock LLM API with TLS (HTTPS)
# Uses auto-generated self-signed certificate for testing
start_mock_api_tls() {
    local mock_api_bin="$EXAMPLES_DIR/mock-llm-api/target/release/mock-llm-api"

    if [ ! -x "$mock_api_bin" ]; then
        log_info "Building mock-llm-api..."
        (cd "$EXAMPLES_DIR/mock-llm-api" && cargo build --release)
    fi

    # Clean up any stale processes first
    cleanup_stale_processes

    log_info "Starting mock API with TLS on port $MOCK_API_PORT..."
    "$mock_api_bin" \
        --listen "127.0.0.1:$MOCK_API_PORT" \
        --tls \
        --chunk-delay-ms 10 \
        --quiet &
    MOCK_API_PID=$!

    if ! wait_for_port "$MOCK_API_PORT" 10; then
        log_error "Mock API (TLS) failed to start"
        return 1
    fi

    log_info "Mock API (TLS) running (PID $MOCK_API_PID)"
}

# Build test runner if needed
build_test_runner() {
    if [ ! -x "$TEST_RUNNER" ]; then
        log_info "Building test runner..."
        (cd "$TESTS_DIR" && go build -o test-runner ./runner)
    fi
}

# Run tests
run_tests() {
    local daemon_name="$1"
    local format="${2:-human}"
    local category="${3:-all}"

    build_test_runner

    log_info "Running tests for $daemon_name..."
    "$TEST_RUNNER" \
        -socket "$DAEMON_SOCKET" \
        -backend "http://127.0.0.1:$MOCK_API_PORT" \
        -daemon "$daemon_name" \
        -format "$format" \
        -category "$category"
}

# Check if a command exists
check_command() {
    local cmd="$1"
    if ! command -v "$cmd" &>/dev/null; then
        log_error "Required command not found: $cmd"
        return 1
    fi
}

# Verify prerequisites
check_prerequisites() {
    check_command go
    # Only require cargo if mock-llm-api needs to be built
    local mock_api_bin="$EXAMPLES_DIR/mock-llm-api/target/release/mock-llm-api"
    if [ ! -x "$mock_api_bin" ]; then
        check_command cargo
    fi
    check_command nc
}
