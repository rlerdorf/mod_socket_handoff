#!/bin/bash
#
# Smoke test for LLM backend integration
#
# Tests each daemon with the mock LLM API server to verify SSE responses
# contain actual LLM content instead of demo messages.
#
# Usage:
#   ./test_llm_backend.sh              # Test all daemons
#   ./test_llm_backend.sh rust go      # Test specific daemons
#

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
EXAMPLES_DIR="$(cd "$SCRIPT_DIR/../examples" && pwd)"
SOCKET="/tmp/test-streaming-daemon.sock"
MOCK_API_DIR="$EXAMPLES_DIR/mock-llm-api"
MOCK_API_BIN="$MOCK_API_DIR/target/release/mock-llm-api"
MOCK_API_PID=""
MOCK_API_PORT=""
TEST_CONNECTIONS=5
CHUNK_DELAY_MS=50  # Fast for testing

# Find cargo binary (may not be in sudo's PATH)
find_cargo() {
    if command -v cargo >/dev/null 2>&1; then
        command -v cargo
        return
    fi
    for dir in "$HOME/.cargo/bin" "/home/$SUDO_USER/.cargo/bin" "/usr/local/bin" "/usr/bin"; do
        if [ -x "$dir/cargo" ]; then
            echo "$dir/cargo"
            return
        fi
    done
    echo ""
}
CARGO_BIN="$(find_cargo)"

# Find an available port starting from a base port
find_available_port() {
    local port=${1:-8080}
    local max_port=$((port + 100))
    while [ $port -lt $max_port ]; do
        # Check if port is in use (match :PORT followed by space)
        if ss -tln 2>/dev/null | grep -qE ":${port}[[:space:]]"; then
            # Port in use, try next
            port=$((port + 1))
        else
            # Port available
            echo $port
            return 0
        fi
    done
    echo ""
    return 1
}

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

# Parse arguments
DAEMONS_TO_TEST=()
while [[ $# -gt 0 ]]; do
    case $1 in
        -h|--help)
            echo "Usage: $0 [daemon ...]"
            echo ""
            echo "Smoke test LLM backend integration for streaming daemons."
            echo ""
            echo "Arguments:"
            echo "  daemon    One or more daemons to test: php, go, rust, uring"
            echo "            If not specified, all daemons are tested."
            echo ""
            echo "Examples:"
            echo "  $0              # Test all daemons"
            echo "  $0 rust         # Test only Rust"
            echo "  $0 php go       # Test PHP and Go"
            exit 0
            ;;
        php|go|rust|uring)
            DAEMONS_TO_TEST+=("$1")
            shift
            ;;
        *)
            echo "Error: Unknown daemon '$1'"
            echo "Valid daemons: php, go, rust, uring"
            exit 1
            ;;
    esac
done

# Default to all daemons if none specified
if [ ${#DAEMONS_TO_TEST[@]} -eq 0 ]; then
    DAEMONS_TO_TEST=(rust go php uring)
fi

cleanup() {
    # Kill mock API
    if [ -n "$MOCK_API_PID" ]; then
        kill -TERM "$MOCK_API_PID" 2>/dev/null || true
        wait "$MOCK_API_PID" 2>/dev/null || true
    fi
    pkill -9 -f "mock-llm-api" 2>/dev/null || true

    # Kill daemons
    pkill -9 -f "streaming-daemon-go/streaming-daemon" 2>/dev/null || true
    pkill -9 -f "streaming-daemon-rs" 2>/dev/null || true
    pkill -9 -f "streaming_daemon.php" 2>/dev/null || true
    pkill -9 -f "streaming-daemon-uring" 2>/dev/null || true

    rm -f "$SOCKET"
}

trap cleanup EXIT

start_mock_api() {
    echo "Starting mock LLM API server..."

    # Build if needed
    if [ ! -x "$MOCK_API_BIN" ]; then
        if [ -z "$CARGO_BIN" ]; then
            echo -e "${RED}ERROR: cargo not found. Please build mock-llm-api first:${NC}"
            echo "  cd $MOCK_API_DIR && cargo build --release"
            exit 1
        fi
        echo "Building mock-llm-api..."
        (cd "$MOCK_API_DIR" && "$CARGO_BIN" build --release)
    fi

    # Find an available port
    MOCK_API_PORT=$(find_available_port 8080)
    if [ -z "$MOCK_API_PORT" ]; then
        echo -e "${RED}ERROR: Could not find available port for mock API${NC}"
        exit 1
    fi

    "$MOCK_API_BIN" --listen "127.0.0.1:$MOCK_API_PORT" --chunk-delay-ms "$CHUNK_DELAY_MS" --chunk-count 5 --quiet &
    MOCK_API_PID=$!

    # Wait for ready
    for i in {1..30}; do
        if curl -s "http://127.0.0.1:$MOCK_API_PORT/health" > /dev/null 2>&1; then
            echo -e "${GREEN}Mock API ready on port $MOCK_API_PORT${NC}"
            return 0
        fi
        sleep 0.1
    done

    echo -e "${RED}Mock API failed to start${NC}"
    exit 1
}

# Run load generator and verify results
LOAD_GEN="$SCRIPT_DIR/load-generator/load-generator"

test_connections() {
    local output

    # Build load generator if needed
    if [ ! -x "$LOAD_GEN" ]; then
        echo "Building load generator..."
        (cd "$SCRIPT_DIR/load-generator" && go build -o load-generator .) || {
            echo -e "${RED}ERROR: Failed to build load generator${NC}"
            return 1
        }
    fi

    # Run load generator with test connections
    output=$("$LOAD_GEN" -socket "$SOCKET" -connections $TEST_CONNECTIONS \
        -ramp-up 1s -hold 5s -stream-timeout 30s \
        -data '{"user_id":123,"prompt":"test prompt"}' 2>&1)

    # Check results from JSON output
    local completed=$(echo "$output" | grep -o '"connections_completed": [0-9]*' | grep -o '[0-9]*')
    local failed=$(echo "$output" | grep -o '"connections_failed": [0-9]*' | grep -o '[0-9]*')
    local bytes=$(echo "$output" | grep -o '"bytes_received": [0-9]*' | grep -o '[0-9]*')

    if [ -z "$completed" ] || [ -z "$failed" ]; then
        echo -e "${RED}ERROR: Could not parse load generator output${NC}"
        echo "$output"
        return 1
    fi

    if [ "$completed" -eq "$TEST_CONNECTIONS" ] && [ "$failed" -eq 0 ] && [ "$bytes" -gt 100 ]; then
        echo -e "${GREEN}PASS: $completed/$TEST_CONNECTIONS connections, $bytes bytes received${NC}"
        return 0
    else
        echo -e "${RED}FAIL: $completed completed, $failed failed, $bytes bytes${NC}"
        echo "$output"
        return 1
    fi
}

test_daemon() {
    local name=$1
    local start_cmd=$2
    local pattern=$3

    echo ""
    echo -e "${YELLOW}Testing: $name${NC}"

    cleanup
    rm -f "$SOCKET"

    # Start mock API
    start_mock_api

    # Start daemon
    eval "$start_cmd" > /tmp/daemon_${name}.log 2>&1 &

    # Wait for socket
    for i in {1..30}; do
        if [ -S "$SOCKET" ]; then
            break
        fi
        sleep 0.1
    done

    if [ ! -S "$SOCKET" ]; then
        echo -e "${RED}FAIL: Socket not created${NC}"
        cat /tmp/daemon_${name}.log
        return 1
    fi

    # Set permissions for testing
    chmod 666 "$SOCKET" 2>/dev/null || true

    # Run test connections using load generator
    local result=0
    if ! test_connections; then
        echo "Daemon log:"
        cat /tmp/daemon_${name}.log | head -50
        result=1
    fi

    # Stop daemon
    pkill -TERM -f "$pattern" 2>/dev/null || true
    sleep 1

    return $result
}

echo "==================================="
echo "LLM Backend Smoke Test"
echo "==================================="
echo "Daemons to test: ${DAEMONS_TO_TEST[*]}"

PASSED=0
FAILED=0

# Test Rust daemon
# Note: Use single quotes for API port so it's expanded at eval time (after start_mock_api)
# Include /v1 in base URL to match standard OpenAI API pattern
if [[ " ${DAEMONS_TO_TEST[*]} " =~ " rust " ]]; then
    if test_daemon "rust" \
        'RUST_LOG=warn OPENAI_API_BASE=http://127.0.0.1:$MOCK_API_PORT/v1 OPENAI_API_KEY=test '"$EXAMPLES_DIR"'/streaming-daemon-rs/target/release/streaming-daemon-rs -s $SOCKET -b openai' \
        "streaming-daemon-rs"; then
        PASSED=$((PASSED + 1))
    else
        FAILED=$((FAILED + 1))
    fi
fi

# Test Go daemon
if [[ " ${DAEMONS_TO_TEST[*]} " =~ " go " ]]; then
    if test_daemon "go" \
        'OPENAI_API_KEY=test '"$EXAMPLES_DIR"'/streaming-daemon-go/streaming-daemon -socket $SOCKET -socket-mode 0666 -backend openai -openai-base http://127.0.0.1:$MOCK_API_PORT/v1 -metrics-addr=' \
        "streaming-daemon-go/streaming-daemon"; then
        PASSED=$((PASSED + 1))
    else
        FAILED=$((FAILED + 1))
    fi
fi

# Test PHP daemon
if [[ " ${DAEMONS_TO_TEST[*]} " =~ " php " ]]; then
    if test_daemon "php" \
        'php '"$EXAMPLES_DIR"'/streaming-daemon-amp/streaming_daemon.php -s $SOCKET -m 0666 --backend openai --openai-base http://127.0.0.1:$MOCK_API_PORT/v1' \
        "streaming_daemon.php"; then
        PASSED=$((PASSED + 1))
    else
        FAILED=$((FAILED + 1))
    fi
fi

# Test C io_uring daemon (requires OpenAI build)
if [[ " ${DAEMONS_TO_TEST[*]} " =~ " uring " ]]; then
    URING_DIR="$EXAMPLES_DIR/streaming-daemon-uring"
    URING_BIN="$URING_DIR/streaming-daemon-uring"

    # Check if built with OpenAI backend
    if [ -x "$URING_BIN" ]; then
        # Rebuild with OpenAI backend for test
        echo "Building C io_uring daemon with OpenAI backend..."
        make -C "$URING_DIR" clean >/dev/null 2>&1
        make -C "$URING_DIR" BACKEND=openai -j$(nproc) >/dev/null 2>&1
    fi

    if [ -x "$URING_BIN" ]; then
        if test_daemon "uring" \
            'OPENAI_API_BASE=http://127.0.0.1:$MOCK_API_PORT/v1 OPENAI_API_KEY=test '"$URING_BIN"' --socket $SOCKET --socket-mode 0666 --thread-pool' \
            "streaming-daemon-uring"; then
            PASSED=$((PASSED + 1))
        else
            FAILED=$((FAILED + 1))
        fi
    else
        echo -e "${YELLOW}Skipping uring: binary not found${NC}"
    fi
fi

echo ""
echo "==================================="
echo "Results: $PASSED passed, $FAILED failed"
echo "==================================="

if [ $FAILED -gt 0 ]; then
    exit 1
fi
