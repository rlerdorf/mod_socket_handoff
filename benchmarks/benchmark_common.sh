#!/bin/bash
#
# Common functions and setup for streaming daemon benchmarks.
#
# This library is sourced by run_full_benchmark.sh and run_capacity_benchmark.sh.
# Do not run this script directly.
#

# Guard against direct execution
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    echo "Error: This script is meant to be sourced, not executed directly."
    echo "Use run_full_benchmark.sh or run_capacity_benchmark.sh instead."
    exit 1
fi

#=============================================================================
# DAEMON SELECTION
#=============================================================================

# Check if daemon should be run
# Usage: should_run <daemon_name>
should_run() {
    local daemon=$1
    for d in "${DAEMONS_TO_RUN[@]}"; do
        if [ "$d" = "$daemon" ]; then
            return 0
        fi
    done
    return 1
}

#=============================================================================
# BINARY DISCOVERY
#=============================================================================

# Find binaries that may not be in sudo's PATH
find_binary() {
    local name=$1
    if command -v "$name" >/dev/null 2>&1; then
        command -v "$name"
        return
    fi
    for dir in "$HOME/.cargo/bin" "/home/$SUDO_USER/.cargo/bin" \
               "$HOME/go/bin" "/home/$SUDO_USER/go/bin" \
               "/usr/local/go/bin" "/usr/local/bin" "/usr/bin"; do
        if [ -x "$dir/$name" ]; then
            echo "$dir/$name"
            return
        fi
    done
    echo ""
}

#=============================================================================
# SYSTEM SETUP
#=============================================================================

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m' # No Color

# Check for root
check_root() {
    IS_ROOT=false
    if [ "$EUID" -eq 0 ]; then
        IS_ROOT=true
    fi
}

# Set up file descriptor limits
# Sets: DESIRED_FD_LIMIT
setup_fd_limits() {
    DESIRED_FD_LIMIT=500000
    if ! ulimit -n $DESIRED_FD_LIMIT 2>/dev/null; then
        DESIRED_FD_LIMIT=65536
        if ! ulimit -n $DESIRED_FD_LIMIT 2>/dev/null; then
            DESIRED_FD_LIMIT=$(ulimit -n)
            echo "Warning: Could not increase fd limit (current: $DESIRED_FD_LIMIT)"
            echo "         High connection counts may fail. Run with sudo for full benchmarks."
        fi
    fi
    echo "File descriptor limit set to: $(ulimit -n)"
}

# Calculate max connections based on system fd limits
# Sets: HARD_FD_LIMIT, MAX_CONNECTIONS
calculate_max_connections() {
    HARD_FD_LIMIT=$(ulimit -Hn)
    if [ "$HARD_FD_LIMIT" = "unlimited" ] || ! [[ "$HARD_FD_LIMIT" =~ ^[0-9]+$ ]]; then
        HARD_FD_LIMIT=1048576
    fi
    MAX_CONNECTIONS=$(( (HARD_FD_LIMIT - 1000) / 3 ))
    if [ "$MAX_CONNECTIONS" -lt 150000 ]; then
        MAX_CONNECTIONS=150000
    fi
    echo "System fd hard limit: $HARD_FD_LIMIT"
    echo "Max connections for daemons: $MAX_CONNECTIONS"
}

# Sysctl tuning for high concurrency (requires root)
# Sets: ORIG_MAX_MAP_COUNT, ORIG_SOMAXCONN
setup_sysctl() {
    ORIG_MAX_MAP_COUNT=""
    ORIG_SOMAXCONN=""

    if $IS_ROOT; then
        ORIG_MAX_MAP_COUNT=$(sysctl -n vm.max_map_count)
        sysctl -w vm.max_map_count=262144 >/dev/null
        echo "vm.max_map_count set to: $(sysctl -n vm.max_map_count) (was: $ORIG_MAX_MAP_COUNT)"

        ORIG_SOMAXCONN=$(sysctl -n net.core.somaxconn)
        sysctl -w net.core.somaxconn=65535 >/dev/null
        echo "net.core.somaxconn set to: $(sysctl -n net.core.somaxconn) (was: $ORIG_SOMAXCONN)"
    else
        echo "Note: Running without root. Some daemons may fail at high connection counts."
    fi
}

# Restore sysctl settings
restore_sysctl() {
    if [ -n "$ORIG_MAX_MAP_COUNT" ]; then
        sysctl -w vm.max_map_count="$ORIG_MAX_MAP_COUNT" >/dev/null 2>&1 || true
    fi
    if [ -n "$ORIG_SOMAXCONN" ]; then
        sysctl -w net.core.somaxconn="$ORIG_SOMAXCONN" >/dev/null 2>&1 || true
    fi
}

#=============================================================================
# PATH SETUP
#=============================================================================

# Initialize common paths
# Sets: SOCKET, SCRIPT_DIR, LOAD_GEN, EXAMPLES_DIR, MOCK_API_DIR, MOCK_API_BIN,
#       MOCK_API_SOCKET, MOCK_API_TCP_ADDR, CARGO_BIN, GO_BIN
setup_paths() {
    local script_dir=$1

    # Socket path depends on root status
    if $IS_ROOT; then
        SOCKET="/var/run/streaming-daemon.sock"
    else
        SOCKET="/tmp/streaming-daemon-bench.sock"
    fi

    SCRIPT_DIR="$script_dir"
    LOAD_GEN="$SCRIPT_DIR/load-generator/load-generator"
    EXAMPLES_DIR="$(cd "$SCRIPT_DIR/../examples" && pwd)"
    MOCK_API_DIR="$EXAMPLES_DIR/mock-llm-api"
    MOCK_API_BIN="$MOCK_API_DIR/target/release/mock-llm-api"

    # Mock API endpoints:
    # - Unix socket for HTTP/1.1 daemons (avoids TCP port exhaustion)
    # - TCP for HTTP/2 daemons (enables multiplexing)
    MOCK_API_SOCKET="/tmp/mock-llm-api-benchmark.sock"
    MOCK_API_HOST="127.0.0.1"
    MOCK_API_PORT="18080"
    MOCK_API_TCP_ADDR="${MOCK_API_HOST}:${MOCK_API_PORT}"

    # Find build tools
    CARGO_BIN="$(find_binary cargo)"
    GO_BIN="$(find_binary go)"
}

#=============================================================================
# MOCK LLM API MANAGEMENT
#=============================================================================

MOCK_API_PID=""

# Calculate optimal chunk delay for a given connection count
# We want stream duration >= ramp-up time to achieve peak concurrency
# With 18 chunks, there are 17 inter-chunk delays
calculate_chunk_delay() {
    local conns=$1
    local rampup_ms

    if [ "$conns" -le 10000 ]; then
        rampup_ms=10000   # 10s ramp-up
    elif [ "$conns" -le 100000 ]; then
        rampup_ms=30000   # 30s ramp-up
    else
        rampup_ms=60000   # 60s ramp-up
    fi

    # delay = ramp_up_ms * 1.2 / 17 inter-chunk delays
    echo $(( (rampup_ms * 12 / 10) / 17 ))
}

# Build mock LLM API if needed
build_mock_api() {
    if [ ! -x "$MOCK_API_BIN" ]; then
        if [ -z "$CARGO_BIN" ]; then
            echo -e "${RED}ERROR: cargo not found. Please build mock-llm-api first:${NC}"
            echo "  cd $MOCK_API_DIR && cargo build --release"
            exit 1
        fi
        echo "Building mock-llm-api..."
        (cd "$MOCK_API_DIR" && "$CARGO_BIN" build --release)
    fi
}

# Start mock LLM API server with specified chunk delay
# Usage: start_mock_api [chunk_delay_ms] [tls]
# Uses a single instance with dual listeners:
# - Unix socket: Serves HTTP/2 with prior knowledge (h2c); HTTP/1.1 daemons disable HTTP/2 (e.g. reqwest http1_only())
# - TCP: HTTP/2 with optional TLS (HTTPS for Swow which requires TLS for curl_multi multiplexing,
#        h2c for Go/Rust/io_uring which support cleartext HTTP/2)
start_mock_api() {
    local chunk_delay=${1:-706}
    local use_tls=${2:-false}

    # Stop any existing mock API first
    stop_mock_api

    # Remove any stale socket
    rm -f "$MOCK_API_SOCKET"

    # Start single instance with both Unix socket and TCP listeners
    local tls_flag=""
    if [ "$use_tls" = "true" ]; then
        tls_flag="--tls"
    fi
    "$MOCK_API_BIN" --socket "$MOCK_API_SOCKET" --listen "$MOCK_API_TCP_ADDR" \
        $tls_flag --chunk-delay-ms "$chunk_delay" --chunk-count 18 --quiet &
    MOCK_API_PID=$!

    # Wait until both Unix socket and TCP health endpoints are ready.
    local tries=0
    local socket_ready=false
    local tcp_ready=false
    local tcp_scheme="http"
    if [ "$use_tls" = "true" ]; then
        tcp_scheme="https"
    fi
    while [ $tries -lt 50 ]; do
        # Check Unix socket health endpoint
        if [ -S "$MOCK_API_SOCKET" ] && ! $socket_ready; then
            if curl -s --http2-prior-knowledge --unix-socket "$MOCK_API_SOCKET" http://localhost/health > /dev/null 2>&1; then
                socket_ready=true
            fi
        fi
        # Check TCP health endpoint
        if ! $tcp_ready; then
            if [ "$use_tls" = "true" ]; then
                # Use -k for self-signed cert
                if curl --http2 -sk "${tcp_scheme}://${MOCK_API_TCP_ADDR}/health" > /dev/null 2>&1; then
                    tcp_ready=true
                fi
            else
                if curl -s --http2-prior-knowledge "${tcp_scheme}://${MOCK_API_TCP_ADDR}/health" > /dev/null 2>&1; then
                    tcp_ready=true
                fi
            fi
        fi
        if $socket_ready && $tcp_ready; then
            local tls_note=""
            if [ "$use_tls" = "true" ]; then
                tls_note=" [TLS]"
            fi
            echo "Mock API ready (unix:$MOCK_API_SOCKET + ${tcp_scheme}:$MOCK_API_TCP_ADDR${tls_note}, ${chunk_delay}ms delay, ~$((chunk_delay * 17 / 1000))s streams)"
            return 0
        fi
        sleep 0.1
        tries=$((tries + 1))
    done

    echo -e "${RED}ERROR: Mock LLM API server failed to start (socket=$socket_ready, tcp=$tcp_ready)${NC}"
    stop_mock_api
    exit 1
}

# Stop mock LLM API server
stop_mock_api() {
    if [ -n "$MOCK_API_PID" ]; then
        kill -TERM "$MOCK_API_PID" 2>/dev/null || true
        wait "$MOCK_API_PID" 2>/dev/null || true
        MOCK_API_PID=""
    fi
    # Also kill any orphaned mock-llm-api processes
    pkill -9 -f "mock-llm-api" 2>/dev/null || true
    # Clean up socket file
    rm -f "$MOCK_API_SOCKET"
}

#=============================================================================
# DAEMON MANAGEMENT
#=============================================================================

# Cleanup daemons (preserves mock API)
cleanup_daemons() {
    # Kill load generator first
    pkill -9 -f "load-generator" 2>/dev/null || true

    # Get PIDs of actual daemon binaries
    local go_pids=$(pgrep -f "streaming-daemon-go/streaming-daemon" 2>/dev/null || true)
    local rust_pids=$(pgrep -f "streaming-daemon-rs" 2>/dev/null || true)
    local py_pids=$(pgrep -f "streaming_daemon_async.py" 2>/dev/null || true)
    local php_pids=$(pgrep -f "streaming_daemon.php" 2>/dev/null || true)
    local uring_pids=$(pgrep -f "streaming-daemon-uring" 2>/dev/null || true)

    # Send SIGTERM first for graceful shutdown
    for pid in $go_pids $rust_pids $py_pids $php_pids $uring_pids; do
        if [ -n "$pid" ]; then
            kill -TERM "$pid" 2>/dev/null || true
        fi
    done

    # Wait up to 5 seconds for graceful shutdown
    local tries=0
    while [ $tries -lt 5 ]; do
        local still_running=false
        for pid in $go_pids $rust_pids $py_pids $php_pids $uring_pids; do
            if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
                still_running=true
                break
            fi
        done
        if ! $still_running; then
            break
        fi
        sleep 1
        tries=$((tries + 1))
    done

    # Force kill any remaining processes
    for pid in $go_pids $rust_pids $py_pids $php_pids $uring_pids; do
        if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
            echo "Force killing PID $pid..."
            kill -9 "$pid" 2>/dev/null || true
        fi
    done

    # Also kill any bash wrapper processes
    pkill -9 -f "sudo.*streaming-daemon" 2>/dev/null || true

    # Remove socket file
    rm -f "$SOCKET"
    sleep 1
}

# Full cleanup including mock API
cleanup() {
    echo ""
    echo "Cleaning up..."
    stop_mock_api
    cleanup_daemons
}

# Abort handler for Ctrl+C - kills current test immediately
# Usage: Set CURRENT_LG_PID before running load generator
CURRENT_LG_PID=""
CURRENT_DAEMON_PID=""

# Terminate a process gracefully (SIGTERM first, then SIGKILL after timeout)
terminate_process() {
    local pid="$1"
    if [ -z "$pid" ] || ! kill -0 "$pid" 2>/dev/null; then
        return
    fi

    # Try graceful termination first
    kill "$pid" 2>/dev/null || true
    for _ in {1..5}; do
        if ! kill -0 "$pid" 2>/dev/null; then
            break
        fi
        sleep 0.2
    done

    # Force kill if still running
    if kill -0 "$pid" 2>/dev/null; then
        kill -9 "$pid" 2>/dev/null || true
    fi

    # Reap the process to avoid zombies
    wait "$pid" 2>/dev/null || true
}

abort_handler() {
    echo ""
    echo -e "${RED}Interrupted! Cleaning up...${NC}"

    terminate_process "$CURRENT_LG_PID"
    terminate_process "$CURRENT_DAEMON_PID"

    cleanup
    restore_sysctl
    echo "Aborted."
    exit 130
}

#=============================================================================
# BUILD DEPENDENCIES
#=============================================================================

# Build load generator if needed
build_load_generator() {
    if [ ! -x "$LOAD_GEN" ]; then
        if [ -z "$GO_BIN" ]; then
            echo -e "${RED}ERROR: go not found. Please build load generator first:${NC}"
            echo "  cd $SCRIPT_DIR/load-generator && go build -o load-generator ."
            exit 1
        fi
        echo "Building load generator..."
        (cd "$SCRIPT_DIR/load-generator" && "$GO_BIN" build -o load-generator .)
    fi
}

# Build Go daemon if needed
build_go_daemon() {
    local daemon_path="$EXAMPLES_DIR/streaming-daemon-go/streaming-daemon"
    if [ ! -x "$daemon_path" ]; then
        if [ -z "$GO_BIN" ]; then
            echo -e "${RED}ERROR: go not found. Please build Go daemon first.${NC}"
            exit 1
        fi
        echo "Building Go daemon..."
        (cd "$EXAMPLES_DIR/streaming-daemon-go" && "$GO_BIN" build -o streaming-daemon .)
    fi
}

# Build uring daemon if needed
# Usage: build_uring_daemon <backend>  (backend: mock or openai)
build_uring_daemon() {
    local desired_backend=${1:-openai}
    local uring_dir="$EXAMPLES_DIR/streaming-daemon-uring"
    local uring_bin="$uring_dir/streaming-daemon-uring"
    local uring_marker="$uring_dir/.backend"

    local current_backend=""
    if [ -f "$uring_marker" ]; then
        current_backend=$(cat "$uring_marker")
    fi

    if [ ! -x "$uring_bin" ] || [ "$current_backend" != "$desired_backend" ]; then
        echo "Building C io_uring daemon with $desired_backend backend..."
        make -C "$uring_dir" clean >/dev/null 2>&1 || true
        make -C "$uring_dir" BACKEND=$desired_backend -j$(nproc)
        echo "$desired_backend" > "$uring_marker"
    fi
}

#=============================================================================
# DAEMON COMMAND GENERATORS
#=============================================================================

# Generate PHP daemon command (AMPHP with HTTP/1.1)
# Usage: get_php_cmd <backend_mode> <message_delay_ms> [max_connections]
get_php_cmd() {
    local backend_mode=$1
    local message_delay_ms=$2
    local max_conns=${3:-50000}

    local base_cmd="php -n -dextension=ev.so -c $EXAMPLES_DIR/streaming-daemon-amp/php.ini $EXAMPLES_DIR/streaming-daemon-amp/streaming_daemon.php -s $SOCKET -m 0666 --max-connections $max_conns"

    if [ "$backend_mode" = "llm-api" ]; then
        echo "$base_cmd --backend openai --openai-base http://localhost/v1 --openai-socket $MOCK_API_SOCKET --benchmark"
    else
        echo "$base_cmd -d $message_delay_ms --benchmark"
    fi
}

# Generate PHP AMPHP daemon command with HTTP/2
# Usage: get_amp_http2_cmd <backend_mode> <message_delay_ms> [max_connections]
get_amp_http2_cmd() {
    local backend_mode=$1
    local message_delay_ms=$2
    local max_conns=${3:-50000}

    local base_cmd="php -n -dextension=ev.so -c $EXAMPLES_DIR/streaming-daemon-amp/php.ini $EXAMPLES_DIR/streaming-daemon-amp/streaming_daemon.php -s $SOCKET -m 0666 --max-connections $max_conns"

    if [ "$backend_mode" = "llm-api" ]; then
        # Use HTTPS for HTTP/2 connection multiplexing
        echo "OPENAI_INSECURE_SSL=true OPENAI_HTTP2_ENABLED=true $base_cmd --backend openai --openai-base https://${MOCK_API_TCP_ADDR}/v1 --benchmark"
    else
        echo "$base_cmd -d $message_delay_ms --benchmark"
    fi
}

# Generate Go daemon command
# Usage: get_go_cmd <backend_mode> <message_delay_ms> [http_mode]
# http_mode: "http1" (default, uses Unix socket) or "http2" (uses TCP with h2c multiplexing)
get_go_cmd() {
    local backend_mode=$1
    local message_delay_ms=$2
    local http_mode=${3:-http1}

    local base_cmd="$EXAMPLES_DIR/streaming-daemon-go/streaming-daemon -socket $SOCKET -benchmark -max-connections ${MAX_CONNECTIONS} -socket-mode 0660 -metrics-addr="

    if [ "$backend_mode" = "llm-api" ]; then
        if [ "$http_mode" = "http2" ]; then
            # HTTP/2 mode: Use HTTPS with TLS for proper HTTP/2 multiplexing
            # OPENAI_INSECURE_SSL=true to accept self-signed certificates from mock server
            echo "OPENAI_API_KEY=benchmark-test OPENAI_HTTP2_ENABLED=true OPENAI_INSECURE_SSL=true $base_cmd -backend openai -openai-base https://${MOCK_API_TCP_ADDR}/v1"
        else
            # HTTP/1.1 mode: Use Unix socket for connection pooling
            echo "OPENAI_API_KEY=benchmark-test OPENAI_HTTP2_ENABLED=false $base_cmd -backend openai -openai-base http://localhost/v1 -openai-socket $MOCK_API_SOCKET"
        fi
    else
        echo "$base_cmd -message-delay ${message_delay_ms}ms"
    fi
}

# Generate Rust daemon command
# Usage: get_rust_cmd <backend_mode> <message_delay_ms> [http_mode]
# http_mode: "http1" (default, uses Unix socket) or "http2" (uses TCP with h2c multiplexing)
get_rust_cmd() {
    local backend_mode=$1
    local message_delay_ms=$2
    local http_mode=${3:-http1}

    local base_env="RUST_LOG=warn DAEMON_SOCKET_MODE=0666 DAEMON_METRICS_ENABLED=false"
    local base_cmd="$EXAMPLES_DIR/streaming-daemon-rs/target/release/streaming-daemon-rs --benchmark -s $SOCKET"

    if [ "$backend_mode" = "llm-api" ]; then
        if [ "$http_mode" = "http2" ]; then
            # HTTP/2 mode: Use HTTPS with TLS for proper HTTP/2 multiplexing
            # OPENAI_INSECURE_SSL=true to accept self-signed certificates from mock server
            echo "$base_env OPENAI_API_BASE=https://${MOCK_API_TCP_ADDR}/v1 OPENAI_API_KEY=benchmark-test OPENAI_INSECURE_SSL=true $base_cmd -b openai"
        else
            # HTTP/1.1 mode: Use Unix socket for connection pooling
            echo "$base_env OPENAI_HTTP2_ENABLED=false OPENAI_API_BASE=http://localhost/v1 OPENAI_API_SOCKET=$MOCK_API_SOCKET OPENAI_API_KEY=benchmark-test $base_cmd -b openai"
        fi
    else
        echo "$base_env DAEMON_TOKEN_DELAY_MS=$message_delay_ms $base_cmd -b mock"
    fi
}

# Generate Swoole daemon command
# Usage: get_swoole_cmd <backend_mode> <message_delay_ms> [http_mode] [max_connections]
# http_mode: "http1" or "http2" (default: http2)
# Note: HTTP/2 mode uses HTTPS (TLS) for real-world performance testing.
# Note: Swoole extension is explicitly loaded to avoid conflict with Swow.
get_swoole_cmd() {
    local backend_mode=$1
    local message_delay_ms=$2
    local http_mode=${3:-http2}
    local max_conns=${4:-50000}

    # Explicitly load Swoole extension (disabled by default to avoid Swow conflict)
    local base_cmd="php -d extension=swoole.so $EXAMPLES_DIR/streaming-daemon-swoole/streaming_daemon.php -s $SOCKET -m 0666 --max-connections $max_conns"

    if [ "$backend_mode" = "llm-api" ]; then
        if [ "$http_mode" = "http2" ]; then
            # HTTP/2 mode: Use HTTPS for real-world TLS performance testing
            # OPENAI_INSECURE_SSL=true to accept self-signed certificates from mock server
            echo "OPENAI_HTTP2_ENABLED=true OPENAI_INSECURE_SSL=true OPENAI_API_KEY=benchmark-test $base_cmd --backend openai --openai-base https://${MOCK_API_TCP_ADDR}/v1 --benchmark"
        else
            # HTTP/1.1 mode
            echo "OPENAI_HTTP2_ENABLED=false OPENAI_API_KEY=benchmark-test $base_cmd --backend openai --openai-base http://${MOCK_API_TCP_ADDR}/v1 --benchmark"
        fi
    else
        echo "$base_cmd -d $message_delay_ms --backend mock --benchmark"
    fi
}

# Generate Swow daemon command
# Usage: get_swow_cmd <backend_mode> <message_delay_ms> [http_mode] [max_connections]
# http_mode: "http1" or "http2" (default: http2)
# Note: Swow HTTP/2 requires HTTPS (TLS) for proper curl_multi connection reuse.
# h2c (cleartext HTTP/2) does not work correctly with Swow's curl hooks.
# Note: Swow extension is explicitly loaded to avoid conflict with Swoole.
get_swow_cmd() {
    local backend_mode=$1
    local message_delay_ms=$2
    local http_mode=${3:-http2}
    local max_conns=${4:-50000}

    # Explicitly load Swow extension (disabled by default to avoid Swoole conflict)
    local base_cmd="php -d extension=swow.so $EXAMPLES_DIR/streaming-daemon-swow/streaming_daemon.php -s $SOCKET -m 0666 --max-connections $max_conns"

    if [ "$backend_mode" = "llm-api" ]; then
        if [ "$http_mode" = "http2" ]; then
            # HTTP/2 mode: Use HTTPS for proper curl_multi multiplexing
            # Swow's curl hooks require TLS for HTTP/2 connection reuse
            # Use 100 streams per handle (OpenAI's limit, also stable with Swow)
            echo "OPENAI_HTTP2_ENABLED=true OPENAI_API_KEY=benchmark-test $base_cmd --backend openai --openai-base https://${MOCK_API_TCP_ADDR}/v1 --benchmark"
        else
            # HTTP/1.1 mode
            echo "OPENAI_HTTP2_ENABLED=false OPENAI_API_KEY=benchmark-test $base_cmd --backend openai --openai-base http://${MOCK_API_TCP_ADDR}/v1 --benchmark"
        fi
    else
        echo "$base_cmd -d $message_delay_ms --backend mock --benchmark"
    fi
}

# Generate uring daemon command
# Usage: get_uring_cmd <backend_mode> <message_delay_ms> [http_mode]
# http_mode: "http1" (default, uses h2c) or "http2" (uses HTTPS with TLS multiplexing)
get_uring_cmd() {
    local backend_mode=$1
    local message_delay_ms=$2
    local http_mode=${3:-http1}

    local uring_max_conns=$((MAX_CONNECTIONS > 110000 ? 110000 : MAX_CONNECTIONS))
    local base_cmd="$EXAMPLES_DIR/streaming-daemon-uring/streaming-daemon-uring --socket $SOCKET --socket-mode 0660 --max-connections ${uring_max_conns} --benchmark"

    if [ "$backend_mode" = "llm-api" ]; then
        if [ "$http_mode" = "http2" ]; then
            # HTTP/2 mode: Use HTTPS with TLS for proper HTTP/2 multiplexing
            # OPENAI_INSECURE_SSL=true to accept self-signed certificates from mock server
            echo "OPENAI_API_BASE=https://${MOCK_API_TCP_ADDR}/v1 OPENAI_API_KEY=benchmark-test OPENAI_INSECURE_SSL=true OPENAI_HTTP2_ENABLED=true $base_cmd"
        else
            # HTTP/1.1 mode: Use Unix socket for connection pooling
            echo "OPENAI_API_BASE=http://localhost/v1 OPENAI_API_SOCKET=$MOCK_API_SOCKET OPENAI_API_KEY=benchmark-test OPENAI_HTTP2_ENABLED=false $base_cmd"
        fi
    else
        # Mock backend with no delay (would stall event loop)
        echo "$base_cmd --delay 0"
    fi
}

# Generate Python HTTP/2 daemon command
# Usage: get_python_http2_cmd <backend_mode> <message_delay_ms>
# Note: HTTP/2 requires pycurl with nghttp2 support
# Uses HTTPS (TLS) for proper HTTP/2 multiplexing (like swow-http2)
get_python_http2_cmd() {
    local backend_mode=$1
    local message_delay_ms=$2

    local base_cmd="python3 $EXAMPLES_DIR/streaming_daemon_async.py -s $SOCKET -m 0666 -w 500"

    if [ "$backend_mode" = "llm-api" ]; then
        # HTTP/2 mode: Use HTTPS for proper TLS-based HTTP/2 multiplexing
        # OPENAI_INSECURE_SSL=true to accept self-signed certificates
        echo "OPENAI_HTTP2_ENABLED=true OPENAI_INSECURE_SSL=true OPENAI_API_BASE=https://${MOCK_API_TCP_ADDR}/v1 OPENAI_API_KEY=benchmark-test $base_cmd --backend openai --benchmark"
    else
        # Mock backend doesn't use HTTP/2
        echo "$base_cmd -d $message_delay_ms --benchmark"
    fi
}

#=============================================================================
# INITIALIZATION
#=============================================================================

# Full initialization for benchmark scripts
# Usage: init_benchmark <script_dir>
init_benchmark() {
    local script_dir=$1

    # Check jq dependency
    if ! command -v jq >/dev/null 2>&1; then
        echo "Error: jq is required but not installed"
        echo "Install with: apt install jq (Debian/Ubuntu) or brew install jq (macOS)"
        exit 1
    fi

    check_root
    setup_fd_limits
    setup_paths "$script_dir"
    calculate_max_connections
    setup_sysctl
}
