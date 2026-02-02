#!/bin/bash
#
# Comprehensive Streaming Daemon Benchmark
#
# Usage:
#   sudo ./run_full_benchmark.sh              # Run all daemons with mock LLM API
#   sudo ./run_full_benchmark.sh rust         # Run only Rust daemon
#   sudo ./run_full_benchmark.sh php go       # Run PHP and Go daemons
#   sudo ./run_full_benchmark.sh --backend delay rust  # Use artificial delays
#   sudo ./run_full_benchmark.sh --http2 rust # Test Rust with HTTP/2 multiplexing
#   sudo ./run_full_benchmark.sh --help       # Show help
#
# Available daemons: php, python, go, go-http2, rust, rust-http2, uring
#
# Tests daemon implementations at multiple connection levels
# and measures TTFB, memory, CPU, and peak concurrency.
#

set -e

# Source common functions
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/benchmark_common.sh"

# Backend mode: llm-api (default) or delay
BACKEND_MODE="llm-api"

# Message delay for delay mode (ms)
MESSAGE_DELAY_MS=5625  # ~96 second streams with 18 messages (17 delays × 5625ms)

# PHP max connections (limit to prevent OOM)
PHP_MAX_CONNECTIONS=${PHP_MAX_CONNECTIONS:-20000}

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
            echo "  daemon    One or more daemons to benchmark:"
            echo "            php, python, go, go-http2, rust, rust-http2, uring"
            echo ""
            echo "            go        - Go daemon with HTTP/1.1 (Unix socket to API)"
            echo "            go-http2  - Go daemon with HTTP/2 multiplexing (TCP h2c)"
            echo "                        (requires --backend llm-api, skipped otherwise)"
            echo "            rust      - Rust daemon with HTTP/1.1 (Unix socket to API)"
            echo "            rust-http2 - Rust daemon with HTTP/2 multiplexing (TCP h2c)"
            echo "                        (requires --backend llm-api, skipped otherwise)"
            echo ""
            echo "            If not specified, all daemons are benchmarked."
            echo ""
            echo "Examples:"
            echo "  $0 rust              # Benchmark Rust daemon (HTTP/1.1)"
            echo "  $0 rust-http2        # Benchmark Rust daemon with HTTP/2"
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
        php|python|go|go-http2|rust|rust-http2|uring)
            DAEMONS_TO_RUN+=("$1")
            shift
            ;;
        *)
            echo "Error: Unknown argument '$1'"
            echo "Valid daemons: php, python, go, go-http2, rust, rust-http2, uring"
            echo "Use --help for usage information."
            exit 1
            ;;
    esac
done

# Default to all daemons if none specified
if [ ${#DAEMONS_TO_RUN[@]} -eq 0 ]; then
    DAEMONS_TO_RUN=(php python go rust uring)
fi

# Initialize benchmark environment
init_benchmark "$SCRIPT_DIR"

# Set up results directory
RESULTS_DIR="$SCRIPT_DIR/results-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$RESULTS_DIR"
echo "Results will be saved to: $RESULTS_DIR"
echo ""

# Set up cleanup trap - use abort_handler for INT to kill current test immediately
trap 'cleanup; restore_sysctl' EXIT
trap 'abort_handler' INT TERM

# Clean up any leftover processes from previous runs
echo "Cleaning up any leftover processes..."
pkill -9 -f "mock-llm-api" 2>/dev/null || true
pkill -9 -f "load-generator" 2>/dev/null || true
rm -f "$MOCK_API_SOCKET" "$SOCKET"
sleep 1

# Connection levels to test
CONNECTIONS=(100 1000 10000 50000 100000)

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
            CURRENT_DAEMON_PID=""
            echo -e "${RED}ERROR: $name failed to start (no process)${NC}"
            cat "$daemon_log"
            echo "$conns,0,0,0,0,0,0,0" >> "$daemon_results"
            continue
        fi
        CURRENT_DAEMON_PID="$pid"

        # Verify socket exists
        if [ ! -S "$SOCKET" ]; then
            CURRENT_DAEMON_PID=""
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
                backlog=$(ss -xl 2>/dev/null | awk -v sock="$SOCKET" '$5 ~ sock {print $3; exit}' || echo 0)
                [ -z "$backlog" ] && backlog=0
                echo "$ts,$rss,$cpu,$backlog" >> "$monitor_file"
                sleep 1
            done
        ) &
        local monitor_pid=$!

        # Run load generator in background so Ctrl+C can interrupt via trap
        local lg_output="$RESULTS_DIR/${name}_${conns}_load.json"
        "$LOAD_GEN" -socket "$SOCKET" \
            -connections "$conns" \
            -ramp-up "${rampup}s" \
            -hold "${hold}s" \
            > "$lg_output" 2>&1 &
        CURRENT_LG_PID=$!

        # Wait for load generator (this allows signals to interrupt)
        wait $CURRENT_LG_PID 2>/dev/null || true
        CURRENT_LG_PID=""

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
        local peak_concurrent=$(grep -oE "Peak concurrent( streams)?: [0-9]+" "$daemon_log" 2>/dev/null | grep -o '[0-9]*' | tail -1 || echo "-")

        # Stop daemon and get final summary
        pkill -TERM -f "$pattern" 2>/dev/null || true
        CURRENT_DAEMON_PID=""
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

# Build dependencies
build_load_generator

if [ "$BACKEND_MODE" = "llm-api" ]; then
    build_mock_api
fi

if should_run uring; then
    if [ "$BACKEND_MODE" = "llm-api" ]; then
        build_uring_daemon "openai"
    else
        build_uring_daemon "mock"
    fi
fi

if should_run go || should_run go-http2; then
    build_go_daemon
fi

cleanup

echo "Starting benchmark at $(date)"
echo "Backend mode: $BACKEND_MODE"
echo "Daemons to test: ${DAEMONS_TO_RUN[*]}"
echo ""

# Test PHP
if should_run php; then
    test_daemon "php" \
        "$(get_php_cmd "$BACKEND_MODE" "$MESSAGE_DELAY_MS" "$PHP_MAX_CONNECTIONS")" \
        "streaming_daemon.php"
fi

# Test Python (using uvloop + 500 thread workers for high concurrency)
# Note: Python daemon's OpenAI backend has async I/O issues under load.
if should_run python; then
    if [ "$BACKEND_MODE" = "llm-api" ]; then
        echo -e "${YELLOW}Skipping Python daemon (async I/O issues with OpenAI backend under load)${NC}"
    else
        test_daemon "python" \
            "python3 $EXAMPLES_DIR/streaming_daemon_async.py -s $SOCKET -m 0666 -d $MESSAGE_DELAY_MS -b 8192 -w 500 --benchmark" \
            "streaming_daemon_async"
    fi
fi

# Test Go (HTTP/1.1 with Unix socket)
if should_run go; then
    test_daemon "go" \
        "$(get_go_cmd "$BACKEND_MODE" "$MESSAGE_DELAY_MS" "http1")" \
        "streaming-daemon-go/streaming-daemon"
fi

# Test Go (HTTP/2 with TCP h2c multiplexing)
if should_run go-http2; then
    if [ "$BACKEND_MODE" != "llm-api" ]; then
        echo -e "${YELLOW}Skipping go-http2: HTTP/2 requires TCP (llm-api backend), not Unix sockets used by delay backend${NC}"
    else
        test_daemon "go-http2" \
            "$(get_go_cmd "$BACKEND_MODE" "$MESSAGE_DELAY_MS" "http2")" \
            "streaming-daemon-go/streaming-daemon"
    fi
fi

# Test Rust (HTTP/1.1 with Unix socket)
if should_run rust; then
    test_daemon "rust" \
        "$(get_rust_cmd "$BACKEND_MODE" "$MESSAGE_DELAY_MS" "http1")" \
        "streaming-daemon-rs"
fi

# Test Rust (HTTP/2 with TCP h2c multiplexing)
if should_run rust-http2; then
    if [ "$BACKEND_MODE" != "llm-api" ]; then
        echo -e "${YELLOW}Skipping rust-http2 (only available with --backend llm-api)${NC}"
    else
        test_daemon "rust-http2" \
            "$(get_rust_cmd "$BACKEND_MODE" "$MESSAGE_DELAY_MS" "http2")" \
            "streaming-daemon-rs"
    fi
fi

# Test C io_uring
if should_run uring; then
    test_daemon "uring" \
        "$(get_uring_cmd "$BACKEND_MODE" "$MESSAGE_DELAY_MS")" \
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

for daemon in php python go go-http2 rust rust-http2 uring; do
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

## HTTP/2 Mode
- go: Uses HTTP/1.1 with Unix socket for connection pooling to mock API
- go-http2: Uses HTTP/2 with TCP h2c (prior knowledge) for stream multiplexing
- rust: Uses HTTP/1.1 with Unix socket for connection pooling to mock API
- rust-http2: Uses HTTP/2 with TCP h2c (prior knowledge) for stream multiplexing
  - With HTTP/2, ~100 streams share each TCP connection vs 1 stream per connection with HTTP/1.1
  - This reduces connection overhead from 100k to ~1k connections for 100k concurrent streams
EOF

echo ""
echo "Benchmark complete!"
echo "Results saved to: $RESULTS_DIR"
echo ""
echo "Summary:"
cat "$RESULTS_DIR/REPORT.md"
