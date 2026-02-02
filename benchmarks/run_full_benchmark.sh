#!/bin/bash
#
# Comprehensive Streaming Daemon Benchmark
#
# Usage:
#   sudo ./run_full_benchmark.sh              # Run all daemons with mock LLM API
#   sudo ./run_full_benchmark.sh rust         # Run only Rust daemon
#   sudo ./run_full_benchmark.sh php go       # Run PHP and Go daemons
#   sudo ./run_full_benchmark.sh --backend delay rust  # Use artificial delays
#   sudo ./run_full_benchmark.sh --help       # Show help
#
# Available daemons: php, python, go, rust, uring
#
# Tests daemon implementations at multiple connection levels
# and measures TTFB, memory, CPU, and peak concurrency.
#

set -e

# Backend mode: llm-api (default) or delay
BACKEND_MODE="llm-api"

# Parse arguments
DAEMONS_TO_RUN=()
while [[ $# -gt 0 ]]; do
    case $1 in
        -h|--help)
            echo "Usage: $0 [--backend MODE] [daemon ...]"
            echo ""
            echo "Run benchmarks for streaming daemons."
            echo ""
            echo "Options:"
            echo "  --backend MODE  Backend mode: 'llm-api' (default) or 'delay'"
            echo "                  llm-api: Uses mock LLM API server for realistic benchmarks"
            echo "                  delay: Uses artificial delays (legacy mode)"
            echo ""
            echo "Arguments:"
            echo "  daemon    One or more daemons to benchmark: php, python, go, rust, uring"
            echo "            If not specified, all daemons are benchmarked."
            echo ""
            echo "Examples:"
            echo "  $0 rust              # Benchmark Rust daemon"
            echo "  $0 --backend delay go  # Use artificial delays"
            echo "  sudo $0              # Full benchmark (100k connections, requires root)"
            echo ""
            echo "Note: Running without sudo works for lower connection counts."
            echo "      High concurrency (50k+) requires root for ulimit and sysctl."
            exit 0
            ;;
        --backend)
            shift
            if [[ "$1" != "llm-api" && "$1" != "delay" ]]; then
                echo "Error: Invalid backend mode '$1'. Must be 'llm-api' or 'delay'."
                exit 1
            fi
            BACKEND_MODE="$1"
            shift
            ;;
        php|python|go|rust|uring)
            DAEMONS_TO_RUN+=("$1")
            shift
            ;;
        *)
            echo "Error: Unknown argument '$1'"
            echo "Valid daemons: php, python, go, rust, uring"
            echo "Use --help for usage information."
            exit 1
            ;;
    esac
done

# Default to all daemons if none specified
if [ ${#DAEMONS_TO_RUN[@]} -eq 0 ]; then
    DAEMONS_TO_RUN=(php python go rust uring)
fi

# Check for required dependencies
if ! command -v jq >/dev/null 2>&1; then
    echo "Error: jq is required but not installed"
    echo "Install with: apt install jq (Debian/Ubuntu) or brew install jq (macOS)"
    exit 1
fi

# Helper function to check if a daemon should be run
should_run() {
    local daemon=$1
    for d in "${DAEMONS_TO_RUN[@]}"; do
        if [ "$d" = "$daemon" ]; then
            return 0
        fi
    done
    return 1
}

# Determine if we're running as root
IS_ROOT=false
if [ "$EUID" -eq 0 ]; then
    IS_ROOT=true
fi

# Try to increase file descriptor limit
# High limits (500k) typically require root, but lower limits might work
DESIRED_FD_LIMIT=500000
if ! ulimit -n $DESIRED_FD_LIMIT 2>/dev/null; then
    # Try a lower limit that might work without root
    DESIRED_FD_LIMIT=65536
    if ! ulimit -n $DESIRED_FD_LIMIT 2>/dev/null; then
        DESIRED_FD_LIMIT=$(ulimit -n)
        echo "Warning: Could not increase fd limit (current: $DESIRED_FD_LIMIT)"
        echo "         High connection counts may fail. Run with sudo for full benchmarks."
    fi
fi
echo "File descriptor limit set to: $(ulimit -n)"

# Increase max memory mappings for PHP fibers (each fiber uses multiple mappings)
# Default is 65530 which is not enough for 50k+ concurrent fibers
ORIG_MAX_MAP_COUNT=""
ORIG_SOMAXCONN=""
restore_sysctl() {
    if [ -n "$ORIG_MAX_MAP_COUNT" ]; then
        sysctl -w vm.max_map_count="$ORIG_MAX_MAP_COUNT" >/dev/null 2>&1 || true
    fi
    if [ -n "$ORIG_SOMAXCONN" ]; then
        sysctl -w net.core.somaxconn="$ORIG_SOMAXCONN" >/dev/null 2>&1 || true
    fi
}
if $IS_ROOT; then
    ORIG_MAX_MAP_COUNT=$(sysctl -n vm.max_map_count)
    sysctl -w vm.max_map_count=262144 >/dev/null
    echo "vm.max_map_count set to: $(sysctl -n vm.max_map_count) (was: $ORIG_MAX_MAP_COUNT)"

    # Increase listen backlog limit for high concurrency
    # Default somaxconn of 4096 caps listen() backlog, causing accept queue overflow
    ORIG_SOMAXCONN=$(sysctl -n net.core.somaxconn)
    sysctl -w net.core.somaxconn=65535 >/dev/null
    echo "net.core.somaxconn set to: $(sysctl -n net.core.somaxconn) (was: $ORIG_SOMAXCONN)"
else
    echo "Note: Running without root. PHP may fail at high connection counts (vm.max_map_count)."
fi
trap 'cleanup; restore_sysctl' EXIT INT TERM

# Use /tmp for socket when not root, /var/run when root
if $IS_ROOT; then
    SOCKET="/var/run/streaming-daemon.sock"
else
    SOCKET="/tmp/streaming-daemon-bench.sock"
fi
LOAD_GEN="$(dirname "$0")/load-generator/load-generator"
RESULTS_DIR="$(dirname "$0")/results-$(date +%Y%m%d-%H%M%S)"
EXAMPLES_DIR="$(cd "$(dirname "$0")/../examples" && pwd)"
MOCK_API_DIR="$EXAMPLES_DIR/mock-llm-api"
MOCK_API_BIN="$MOCK_API_DIR/target/release/mock-llm-api"
MOCK_API_PID=""
# Mock API endpoints:
# - Unix socket for HTTP/1.1 daemons (eliminates ephemeral port limits at 100k+ connections)
# - TCP for HTTP/2 daemons (enables multiplexing for uring)
MOCK_API_SOCKET="/tmp/mock-llm-api-benchmark.sock"
MOCK_API_TCP_ADDR="127.0.0.1:18080"
MESSAGE_DELAY_MS=5625  # ~96 second streams with 18 messages (17 delays × 5625ms)
PHP_MAX_CONNECTIONS=${PHP_MAX_CONNECTIONS:-20000}  # Limit concurrent connections to prevent OOM

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

# Find binaries that may not be in sudo's PATH
find_binary() {
    local name=$1
    # Check if already in PATH
    if command -v "$name" >/dev/null 2>&1; then
        command -v "$name"
        return
    fi
    # Check common locations (including user's home via SUDO_USER)
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
CARGO_BIN="$(find_binary cargo)"
GO_BIN="$(find_binary go)"

# Connection levels to test
CONNECTIONS=(100 1000 10000 50000 100000)

# Calculate max connections based on system fd limits
# Each connection needs ~3 fds, leave 1000 for system overhead
HARD_FD_LIMIT=$(ulimit -Hn)

# Handle "unlimited" or non-numeric values
if [ "$HARD_FD_LIMIT" = "unlimited" ] || ! [[ "$HARD_FD_LIMIT" =~ ^[0-9]+$ ]]; then
    HARD_FD_LIMIT=1048576
fi

MAX_CONNECTIONS=$(( (HARD_FD_LIMIT - 1000) / 3 ))
if [ "$MAX_CONNECTIONS" -lt 150000 ]; then
    MAX_CONNECTIONS=150000
fi
echo "System fd hard limit: $HARD_FD_LIMIT"
echo "Max connections for daemons: $MAX_CONNECTIONS"

mkdir -p "$RESULTS_DIR"
echo "Results will be saved to: $RESULTS_DIR"
echo ""

# Clean up any leftover processes from previous runs (before starting anything)
echo "Cleaning up any leftover processes..."
pkill -9 -f "mock-llm-api" 2>/dev/null || true
pkill -9 -f "load-generator" 2>/dev/null || true
rm -f "$MOCK_API_SOCKET" "$SOCKET"
sleep 1

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Build mock LLM API if needed
build_mock_api() {
    if [ "$BACKEND_MODE" != "llm-api" ]; then
        return 0
    fi

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

# Calculate optimal chunk delay for a given connection count
# We want stream duration >= ramp-up time to achieve peak concurrency
# With 18 chunks, there are 17 inter-chunk delays
# Adding 20% buffer to ensure streams don't complete right at ramp-up end
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

# Start mock LLM API server with specified chunk delay
# Usage: start_mock_api [chunk_delay_ms]
# Uses a single instance with dual listeners:
# - Unix socket for HTTP/1.1 daemons (eliminates ephemeral port limits at 100k+)
# - TCP for HTTP/2 daemons (enables h2c multiplexing for uring)
start_mock_api() {
    if [ "$BACKEND_MODE" != "llm-api" ]; then
        return 0
    fi

    local chunk_delay=${1:-$MESSAGE_DELAY_MS}

    # Stop any existing mock API first
    stop_mock_api

    # Remove any stale socket
    rm -f "$MOCK_API_SOCKET"

    # Start single instance with both Unix socket and TCP listeners
    # - Unix socket: HTTP/1.1 for PHP, Go, Rust daemons
    # - TCP: HTTP/2 (h2c) for uring daemon with multiplexing
    "$MOCK_API_BIN" --socket "$MOCK_API_SOCKET" --listen "$MOCK_API_TCP_ADDR" \
        --chunk-delay-ms "$chunk_delay" --chunk-count 18 --quiet &
    MOCK_API_PID=$!

    # Wait for both endpoints to be ready
    local tries=0
    local socket_ready=false
    local tcp_ready=false
    while [ $tries -lt 50 ]; do
        # Check Unix socket
        if [ -S "$MOCK_API_SOCKET" ] && ! $socket_ready; then
            if curl -s --unix-socket "$MOCK_API_SOCKET" http://localhost/health > /dev/null 2>&1; then
                socket_ready=true
            fi
        fi
        # Check TCP endpoint (uses h2c - HTTP/2 prior knowledge)
        if ! $tcp_ready; then
            if curl -s --http2-prior-knowledge "http://${MOCK_API_TCP_ADDR}/health" > /dev/null 2>&1; then
                tcp_ready=true
            fi
        fi
        if $socket_ready && $tcp_ready; then
            echo "Mock API ready (unix:$MOCK_API_SOCKET + tcp:$MOCK_API_TCP_ADDR, ${chunk_delay}ms delay, ~$((chunk_delay * 17 / 1000))s streams)"
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

# Cleanup function that preserves the mock API (for use between tests)
cleanup_daemons() {
    # Kill load generator first
    pkill -9 -f "load-generator" 2>/dev/null || true

    # Get PIDs of actual daemon binaries (not shell wrappers)
    local go_pids=$(pgrep -f "streaming-daemon-go/streaming-daemon" 2>/dev/null || true)
    local rust_pids=$(pgrep -f "streaming-daemon-rs" 2>/dev/null || true)
    local py_pids=$(pgrep -f "streaming_daemon_async.py" 2>/dev/null || true)
    local php_pids=$(pgrep -f "streaming_daemon.php" 2>/dev/null || true)
    local uring_pids=$(pgrep -f "streaming-daemon-uring" 2>/dev/null || true)

    # Send SIGTERM first for graceful shutdown (allows socket cleanup)
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

    # Also kill any bash wrapper processes that might be lingering
    pkill -9 -f "sudo.*streaming-daemon" 2>/dev/null || true

    # Remove socket file (in case daemon didn't clean up)
    rm -f "$SOCKET"
    sleep 1
}

# Full cleanup including mock API (for script exit)
cleanup() {
    # Stop mock API
    stop_mock_api
    # Kill load generator
    pkill -9 -f "load-generator" 2>/dev/null || true

    # Get PIDs of actual daemon binaries (not shell wrappers)
    # Use pgrep with more specific patterns to avoid matching shell processes
    local go_pids=$(pgrep -f "streaming-daemon-go/streaming-daemon" 2>/dev/null || true)
    local rust_pids=$(pgrep -f "streaming-daemon-rs" 2>/dev/null || true)
    local py_pids=$(pgrep -f "streaming_daemon_async.py" 2>/dev/null || true)
    local php_pids=$(pgrep -f "streaming_daemon.php" 2>/dev/null || true)
    local uring_pids=$(pgrep -f "streaming-daemon-uring" 2>/dev/null || true)

    # Send SIGTERM first for graceful shutdown (allows socket cleanup)
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

    # Also kill any bash wrapper processes that might be lingering
    pkill -9 -f "sudo.*streaming-daemon" 2>/dev/null || true

    # Remove socket file (in case daemon didn't clean up)
    rm -f "$SOCKET"
    sleep 1
}

# Note: EXIT trap already set above with cleanup and restore_max_map_count

# Function to test a single daemon
test_daemon() {
    local name=$1
    local start_cmd=$2
    local pattern=$3

    echo -e "${YELLOW}=========================================="
    echo "Testing: $name"
    echo -e "==========================================${NC}"

    local daemon_results="$RESULTS_DIR/${name}.csv"
    echo "connections,completed,failed,ttfb_p50,ttfb_p99,peak_rss_kb,avg_cpu,peak_concurrent,peak_backlog" > "$daemon_results"

    for conns in "${CONNECTIONS[@]}"; do
        echo ""
        echo "--- $name @ $conns connections ---"

        cleanup_daemons

        # Calculate timing based on connection count
        local rampup=$((conns <= 10000 ? 10 : (conns <= 100000 ? 30 : 60)))

        # For LLM API mode, restart mock API with optimal delay for this connection level
        # Hold time = stream duration + 10s buffer for completion
        if [ "$BACKEND_MODE" = "llm-api" ]; then
            local chunk_delay=$(calculate_chunk_delay $conns)
            local stream_duration_s=$((chunk_delay * 17 / 1000))
            local hold=$((stream_duration_s + 10))
            start_mock_api "$chunk_delay"
        else
            # Delay mode: hold time based on MESSAGE_DELAY_MS
            local hold=$((MESSAGE_DELAY_MS * 17 / 1000 + 10))
        fi

        # Start daemon (separate log per connection level)
        local daemon_log="$RESULTS_DIR/${name}_${conns}_daemon.log"
        eval "$start_cmd" > "$daemon_log" 2>&1 &
        sleep 3

        local pid=$(pgrep -nf "$pattern" 2>/dev/null | head -1)
        if [ -z "$pid" ]; then
            echo -e "${RED}ERROR: $name failed to start (no process)${NC}"
            cat "$daemon_log"
            echo "$conns,0,0,0,0,0,0,0" >> "$daemon_results"
            continue
        fi

        # Verify socket exists
        if [ ! -S "$SOCKET" ]; then
            echo -e "${RED}ERROR: $name socket not created${NC}"
            cat "$daemon_log"
            echo "$conns,0,0,0,0,0,0,0" >> "$daemon_results"
            continue
        fi

        # Get initial memory
        local before_rss=$(awk '/VmHWM/{print $2}' /proc/$pid/status 2>/dev/null || echo 0)

        # Start memory/CPU/backlog monitoring in background
        local monitor_file="$RESULTS_DIR/${name}_${conns}_monitor.csv"
        echo "timestamp,rss_kb,cpu_pct,backlog" > "$monitor_file"
        (
            while kill -0 $pid 2>/dev/null; do
                ts=$(date +%s)
                rss=$(awk '/VmRSS/{print $2}' /proc/$pid/status 2>/dev/null || echo 0)
                cpu=$(ps -p $pid -o %cpu= 2>/dev/null | tr -d ' ' || echo 0)
                # Get accept queue depth (Recv-Q) from ss for our socket
                # Format: Netid State Recv-Q Send-Q Local...
                backlog=$(ss -xl 2>/dev/null | awk -v sock="$SOCKET" '$5 ~ sock {print $3; exit}' || echo 0)
                [ -z "$backlog" ] && backlog=0
                echo "$ts,$rss,$cpu,$backlog" >> "$monitor_file"
                sleep 1
            done
        ) &
        local monitor_pid=$!

        # Run load generator
        local lg_output="$RESULTS_DIR/${name}_${conns}_load.json"
        "$LOAD_GEN" -socket "$SOCKET" \
            -connections "$conns" \
            -ramp-up "${rampup}s" \
            -hold "${hold}s" \
            > "$lg_output" 2>&1

        # Stop monitor
        kill $monitor_pid 2>/dev/null || true
        wait $monitor_pid 2>/dev/null || true

        # Get peak memory (HWM = high water mark)
        local peak_rss=$(awk '/VmHWM/{print $2}' /proc/$pid/status 2>/dev/null || echo 0)

        # Calculate average CPU and peak backlog from monitor data
        local avg_cpu=$(awk -F',' 'NR>1 && $3!="" {sum+=$3; n++} END{if(n>0) printf "%.1f", sum/n; else print "0"}' "$monitor_file")
        local peak_backlog=$(awk -F',' 'NR>1 && $4!="" && $4>max {max=$4} END{print max+0}' "$monitor_file")

        # Extract JSON from load generator output (skip log lines)
        local json_file="$RESULTS_DIR/${name}_${conns}_result.json"
        sed -n '/^{/,/^}/p' "$lg_output" > "$json_file"

        # Parse results using jq
        local completed=$(jq -r '.connections_completed // 0' "$json_file" 2>/dev/null || echo 0)
        local failed=$(jq -r '.connections_failed // 0' "$json_file" 2>/dev/null || echo 0)
        local ttfb_p50=$(jq -r '.ttfb_latency_ms.p50 // 0' "$json_file" 2>/dev/null || echo 0)
        local ttfb_p99=$(jq -r '.ttfb_latency_ms.p99 // 0' "$json_file" 2>/dev/null || echo 0)

        # Get peak concurrent from daemon log (if available)
        # Match both "Peak concurrent streams:" (Go) and "Peak concurrent:" (uring)
        local peak_concurrent=$(grep -oE "Peak concurrent( streams)?: [0-9]+" "$daemon_log" 2>/dev/null | grep -o '[0-9]*' | tail -1 || echo "-")

        # Stop daemon and get final summary
        pkill -TERM -f "$pattern" 2>/dev/null || true
        sleep 2

        # If peak_concurrent not found yet, check again after shutdown
        if [ "$peak_concurrent" = "-" ] || [ -z "$peak_concurrent" ]; then
            peak_concurrent=$(grep -oE "Peak concurrent( streams)?: [0-9]+" "$daemon_log" 2>/dev/null | grep -o '[0-9]*' | tail -1 || echo "0")
        fi

        # Print results
        if [ "$failed" = "0" ]; then
            echo -e "${GREEN}Completed: $completed, Failed: $failed${NC}"
        else
            echo -e "${RED}Completed: $completed, Failed: $failed${NC}"
        fi
        echo "TTFB p50: ${ttfb_p50}ms, p99: ${ttfb_p99}ms"
        echo "Peak RSS: ${peak_rss}KB ($(echo "scale=1; $peak_rss/1024" | bc)MB), Avg CPU: ${avg_cpu}%"
        echo "Peak concurrent: $peak_concurrent, Peak backlog: $peak_backlog"

        # Save to CSV
        echo "$conns,$completed,$failed,$ttfb_p50,$ttfb_p99,$peak_rss,$avg_cpu,$peak_concurrent,$peak_backlog" >> "$daemon_results"
    done

    cleanup_daemons
}

# Make sure load generator exists
if [ ! -x "$LOAD_GEN" ]; then
    if [ -z "$GO_BIN" ]; then
        echo -e "${RED}ERROR: go not found. Please build load generator first:${NC}"
        echo "  cd $(dirname "$0")/load-generator && go build -o load-generator ."
        exit 1
    fi
    echo "Building load generator..."
    (cd "$(dirname "$0")/load-generator" && "$GO_BIN" build -o load-generator .)
fi

# Build uring daemon with appropriate backend if needed
if should_run uring; then
    URING_DIR="$EXAMPLES_DIR/streaming-daemon-uring"
    URING_BIN="$URING_DIR/streaming-daemon-uring"
    URING_BACKEND_MARKER="$URING_DIR/.backend"
    if [ "$BACKEND_MODE" = "llm-api" ]; then
        DESIRED_BACKEND="openai"
    else
        DESIRED_BACKEND="mock"
    fi
    # Check if we need to rebuild (binary missing or wrong backend)
    CURRENT_BACKEND=""
    if [ -f "$URING_BACKEND_MARKER" ]; then
        CURRENT_BACKEND=$(cat "$URING_BACKEND_MARKER")
    fi
    if [ ! -x "$URING_BIN" ] || [ "$CURRENT_BACKEND" != "$DESIRED_BACKEND" ]; then
        echo "Building C io_uring daemon with $DESIRED_BACKEND backend..."
        make -C "$URING_DIR" clean >/dev/null 2>&1
        make -C "$URING_DIR" BACKEND=$DESIRED_BACKEND -j$(nproc)
        echo "$DESIRED_BACKEND" > "$URING_BACKEND_MARKER"
    fi
fi

cleanup

echo "Starting benchmark at $(date)"
echo "Backend mode: $BACKEND_MODE"
echo "Daemons to test: ${DAEMONS_TO_RUN[*]}"
echo ""

# Build mock API if using llm-api backend (started per-connection-level in test_daemon)
build_mock_api

# Test PHP
# Uses custom minimal php.ini with zend.max_allowed_stack_size=-1 (INI_SYSTEM setting).
# The -n flag ignores the system php.ini, using only our custom config.
# StreamSelectDriver is used (ev extension causes stack overflows with PHP 8.4+).
if should_run php; then
    if [ "$BACKEND_MODE" = "llm-api" ]; then
        # Use Unix socket for high-concurrency (eliminates ephemeral port limits)
        PHP_BACKEND_ARGS="--backend openai --openai-base http://localhost/v1 --openai-socket $MOCK_API_SOCKET"
    else
        PHP_BACKEND_ARGS="-d $MESSAGE_DELAY_MS"
    fi
    test_daemon "php" \
        "php -n -dextension=ev.so -c $EXAMPLES_DIR/streaming-daemon-amp/php.ini $EXAMPLES_DIR/streaming-daemon-amp/streaming_daemon.php -s $SOCKET -m 0666 --max-connections $PHP_MAX_CONNECTIONS $PHP_BACKEND_ARGS --benchmark" \
        "streaming_daemon.php"
fi

# Test Python (using uvloop + 500 thread workers for high concurrency)
# Note: Python daemon's OpenAI backend has async I/O issues under load.
# The daemon is a demo - use Go/Rust/PHP for production benchmarks.
if should_run python; then
    if [ "$BACKEND_MODE" = "llm-api" ]; then
        echo -e "${YELLOW}Skipping Python daemon (async I/O issues with OpenAI backend under load)${NC}"
    else
        test_daemon "python" \
            "python3 $EXAMPLES_DIR/streaming_daemon_async.py -s $SOCKET -m 0666 -d $MESSAGE_DELAY_MS -b 8192 -w 500 --benchmark" \
            "streaming_daemon_async"
    fi
fi

# Test Go (disable metrics to avoid port conflicts between test runs)
if should_run go; then
    if [ "$BACKEND_MODE" = "llm-api" ]; then
        # Use Unix socket for high-concurrency (eliminates ephemeral port limits)
        GO_BACKEND_ARGS="-backend openai -openai-base http://localhost/v1 -openai-socket $MOCK_API_SOCKET"
        GO_ENV="OPENAI_API_KEY=benchmark-test"
    else
        GO_BACKEND_ARGS="-message-delay ${MESSAGE_DELAY_MS}ms"
        GO_ENV=""
    fi
    test_daemon "go" \
        "$GO_ENV $EXAMPLES_DIR/streaming-daemon-go/streaming-daemon -socket $SOCKET -benchmark $GO_BACKEND_ARGS -max-connections ${MAX_CONNECTIONS} -socket-mode 0660 -metrics-addr=" \
        "streaming-daemon-go/streaming-daemon"
fi

# Test Rust
# Note: Uses Unix socket for mock API to eliminate ephemeral port limits at 100k+ connections
if should_run rust; then
    if [ "$BACKEND_MODE" = "llm-api" ]; then
        RUST_BACKEND="-b openai"
        # Use Unix socket for API connections (eliminates ephemeral port limits)
        RUST_ENV="RUST_LOG=warn DAEMON_SOCKET_MODE=0666 DAEMON_METRICS_ENABLED=false OPENAI_API_BASE=http://localhost/v1 OPENAI_API_SOCKET=$MOCK_API_SOCKET OPENAI_API_KEY=benchmark-test"
    else
        RUST_BACKEND="-b mock"
        RUST_ENV="RUST_LOG=warn DAEMON_TOKEN_DELAY_MS=$MESSAGE_DELAY_MS DAEMON_SOCKET_MODE=0666 DAEMON_METRICS_ENABLED=false"
    fi
    test_daemon "rust" \
        "$RUST_ENV $EXAMPLES_DIR/streaming-daemon-rs/target/release/streaming-daemon-rs --benchmark -s $SOCKET $RUST_BACKEND" \
        "streaming-daemon-rs"
fi

# Test C io_uring
# Note: When using delay mode, delay=0 because blocking nanosleep stalls the single-threaded event loop
# Note: max-connections capped at 110000 to limit total memory consumption
# Note: Backend is selected at compile time (see uring daemon build above)
# For realistic stream timing benchmarks, compare with Go/Rust which use per-connection tasks
if should_run uring; then
    URING_MAX_CONNS=$((MAX_CONNECTIONS > 110000 ? 110000 : MAX_CONNECTIONS))
    if [ "$BACKEND_MODE" = "llm-api" ]; then
        # OpenAI backend uses env vars for configuration, no delay needed (streaming is real)
        # Use TCP for HTTP/2 multiplexing (h2c prior knowledge) - more efficient than Unix socket
        # curl_manager auto-detects http:// URLs and uses CURL_HTTP_VERSION_2_PRIOR_KNOWLEDGE
        URING_ENV="OPENAI_API_BASE=http://${MOCK_API_TCP_ADDR}/v1 OPENAI_API_KEY=benchmark-test"
        URING_ARGS=""
    else
        # Mock backend with no delay (would stall event loop)
        URING_ENV=""
        URING_ARGS="--delay 0"
    fi
    test_daemon "uring" \
        "$URING_ENV $EXAMPLES_DIR/streaming-daemon-uring/streaming-daemon-uring --socket $SOCKET --socket-mode 0660 --max-connections ${URING_MAX_CONNS} $URING_ARGS --benchmark" \
        "streaming-daemon-uring"
fi


# Generate summary report
echo ""
echo -e "${YELLOW}=========================================="
echo "Generating Summary Report"
echo -e "==========================================${NC}"

cat > "$RESULTS_DIR/REPORT.md" << EOF
# Streaming Daemon Benchmark Report

Generated: $(date)

## Configuration
- Backend mode: $BACKEND_MODE
- Stream duration: ~96 seconds (18 messages x 5625ms delay)
- Hold time: 100 seconds after ramp-up
- Connection levels: 100, 1,000, 10,000, 50,000, 100,000

## Results

EOF

for daemon in php python go rust uring; do
    if [ -f "$RESULTS_DIR/${daemon}.csv" ]; then
        echo "### ${daemon^}" >> "$RESULTS_DIR/REPORT.md"
        echo "" >> "$RESULTS_DIR/REPORT.md"
        echo "| Connections | Completed | Failed | TTFB p50 (ms) | TTFB p99 (ms) | Peak RSS (MB) | Avg CPU (%) | Peak Concurrent | Peak Backlog |" >> "$RESULTS_DIR/REPORT.md"
        echo "|-------------|-----------|--------|---------------|---------------|---------------|-------------|-----------------|--------------|" >> "$RESULTS_DIR/REPORT.md"

        tail -n +2 "$RESULTS_DIR/${daemon}.csv" | while IFS=',' read -r conns completed failed ttfb50 ttfb99 rss cpu peak backlog; do
            rss_mb=$(echo "scale=1; $rss/1024" | bc 2>/dev/null || echo "$rss")
            printf "| %s | %s | %s | %.3f | %.3f | %s | %s | %s | %s |\n" \
                "$conns" "$completed" "$failed" "$ttfb50" "$ttfb99" "$rss_mb" "$cpu" "$peak" "$backlog" >> "$RESULTS_DIR/REPORT.md"
        done
        echo "" >> "$RESULTS_DIR/REPORT.md"
    fi
done

cat >> "$RESULTS_DIR/REPORT.md" << 'EOF'
## Notes
- TTFB = Time to First Byte (handoff to first SSE message)
- Peak RSS = Peak Resident Set Size from /proc/PID/status VmHWM
- Peak Concurrent = Maximum simultaneous active streams (from daemon's benchmark summary)
- Peak Backlog = Maximum pending connections in kernel accept queue (from ss Recv-Q)
EOF

echo ""
echo "Benchmark complete!"
echo "Results saved to: $RESULTS_DIR"
echo ""
echo "Summary:"
cat "$RESULTS_DIR/REPORT.md"
