#!/bin/bash
#
# Daemon Capacity Discovery Benchmark
#
# Uses binary search to find the maximum sustainable connection count for each
# daemon implementation, producing a report like:
#
#   "PHP daemon: max 28,000 concurrent connections with p99 TTFB < 100ms, < 1GB RAM"
#
# Unlike run_full_benchmark.sh which tests fixed connection levels (100, 1K, 10K, etc),
# this script finds the actual breaking point for each daemon.
#
# Usage:
#   sudo ./run_capacity_benchmark.sh              # Test all daemons
#   sudo ./run_capacity_benchmark.sh rust         # Test only Rust daemon
#   sudo ./run_capacity_benchmark.sh rust-http2   # Test Rust with HTTP/2 multiplexing
#   sudo ./run_capacity_benchmark.sh go-http2     # Test Go with HTTP/2 multiplexing
#   sudo ./run_capacity_benchmark.sh --help       # Show help
#
# Available daemons: php, amp-http2, swoole, swoole-http2, swow, swow-http2, go, go-http2, rust, rust-http2, uring, uring-http2, python-http2
#

set -e

# Source common functions
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/benchmark_common.sh"

#=============================================================================
# CONFIGURABLE THRESHOLDS
# Adjust these to define what "sustainable" means for your use case
#=============================================================================

# Pass/fail thresholds
MAX_FAILURE_RATE=0              # 0% failures allowed (strict requirement)
MAX_TTFB_P99_MS=100.0           # p99 TTFB must be under 100ms
MAX_RSS_KB=1048576              # 1GB RAM limit (1048576 KB)
MAX_CPU_PCT=30                  # 30% CPU limit
MAX_BACKLOG=1000                # Kernel accept queue limit (generous)

# Binary search parameters
INITIAL_HIGH=10000              # Starting upper bound (will expand if passes)
GRANULARITY=500                 # Stop searching when range < this
MAX_ITERATIONS=20               # Safety limit on search iterations
STABILITY_RUNS=3                # Number of confirmation runs at final capacity

# Test timing
WARMUP_CONNECTIONS=100          # Warmup run before search starts
WARMUP_HOLD_SECONDS=10          # Hold time for warmup

#=============================================================================
# END CONFIGURABLE THRESHOLDS
#=============================================================================

# Parse arguments
DAEMONS_TO_RUN=()
while [[ $# -gt 0 ]]; do
    case $1 in
        -h|--help)
            echo "Usage: $0 [daemon ...]"
            echo ""
            echo "Discover maximum sustainable connection count for streaming daemons."
            echo ""
            echo "Arguments:"
            echo "  daemon    One or more daemons to test:"
            echo "            php, amp-http2, swoole, swoole-http2, swow, swow-http2, go, go-http2, rust, rust-http2, uring, uring-http2, python-http2"
            echo ""
            echo "            php       - AMPHP daemon with HTTP/1.1"
            echo "            amp-http2 - AMPHP daemon with HTTP/2 multiplexing (HTTPS)"
            echo "            swoole    - Swoole daemon with HTTP/1.1"
            echo "            swoole-http2 - Swoole daemon with HTTP/2 multiplexing (HTTPS)"
            echo "            swow      - Swow daemon with HTTP/1.1"
            echo "            swow-http2 - Swow daemon with HTTP/2 multiplexing (HTTPS via curl_multi)"
            echo "            go        - Go daemon with HTTP/1.1 (Unix socket to API)"
            echo "            go-http2  - Go daemon with HTTP/2 multiplexing (HTTPS TLS)"
            echo "            rust      - Rust daemon with HTTP/1.1 (Unix socket to API)"
            echo "            rust-http2 - Rust daemon with HTTP/2 multiplexing (HTTPS TLS)"
            echo "            python-http2 - Python asyncio daemon with HTTP/2 via PycURL"
            echo ""
            echo "            If not specified, all daemons are tested."
            echo ""
            echo "Thresholds (edit script to change):"
            echo "  MAX_FAILURE_RATE=$MAX_FAILURE_RATE (failures allowed)"
            echo "  MAX_TTFB_P99_MS=$MAX_TTFB_P99_MS ms"
            echo "  MAX_RSS_KB=$MAX_RSS_KB KB ($(echo "scale=0; $MAX_RSS_KB/1024" | bc) MB)"
            echo "  MAX_CPU_PCT=$MAX_CPU_PCT%"
            echo "  MAX_BACKLOG=$MAX_BACKLOG"
            echo ""
            echo "Examples:"
            echo "  sudo $0              # Test all HTTP/2 daemons (default)"
            echo "  sudo $0 rust go      # Test Rust and Go only"
            echo "  sudo $0 amp-http2    # Test AMPHP with HTTP/2 multiplexing"
            echo "  sudo $0 swoole-http2 # Test Swoole with HTTP/2 multiplexing"
            echo "  sudo $0 swow-http2   # Test Swow with HTTP/2 multiplexing"
            echo "  sudo $0 rust-http2   # Test Rust with HTTP/2 multiplexing"
            echo "  sudo $0 go-http2     # Test Go with HTTP/2 multiplexing"
            echo "  sudo $0 uring-http2  # Test io_uring with HTTP/2 multiplexing"
            echo "  sudo $0 python-http2 # Test Python with HTTP/2 via PycURL"
            echo ""
            exit 0
            ;;
        php|amp-http2|swoole|swoole-http2|swow|swow-http2|go|go-http2|rust|rust-http2|uring|uring-http2|python-http2)
            DAEMONS_TO_RUN+=("$1")
            shift
            ;;
        *)
            echo "Error: Unknown argument '$1'"
            echo "Valid daemons: php, amp-http2, swoole, swoole-http2, swow, swow-http2, go, go-http2, rust, rust-http2, uring, uring-http2, python-http2"
            echo "Use --help for usage information."
            exit 1
            ;;
    esac
done

# Default to all daemons if none specified
if [ ${#DAEMONS_TO_RUN[@]} -eq 0 ]; then
    DAEMONS_TO_RUN=(python-http2 amp-http2 swow-http2 swoole-http2 go-http2 rust-http2 uring-http2)
fi

# Initialize benchmark environment
init_benchmark "$SCRIPT_DIR"

# Set up results directory
RESULTS_DIR="$SCRIPT_DIR/capacity-results-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$RESULTS_DIR"
echo "Results will be saved to: $RESULTS_DIR"
echo ""

# Set up cleanup trap - use abort_handler for INT to kill current test immediately
trap 'cleanup; restore_sysctl' EXIT
trap 'abort_handler' INT TERM

#=============================================================================
# SINGLE TEST EXECUTION
#=============================================================================

# Run a single test at N connections and collect metrics
# Returns JSON file path with results
# Usage: run_single_test <daemon_name> <start_cmd> <pattern> <connections> <test_id> [use_tls]
run_single_test() {
    set +e  # Disable errexit for this function to ensure we always output result path
    local name=$1
    local start_cmd=$2
    local pattern=$3
    local conns=$4
    local test_id=$5
    local use_tls=${6:-false}

    local result_prefix="$RESULTS_DIR/${name}_${conns}_${test_id}"
    local daemon_log="${result_prefix}_daemon.log"
    local lg_output="${result_prefix}_load.json"
    local result_json="${result_prefix}_result.json"

    cleanup_daemons

    # Calculate timing
    local rampup=$((conns <= 10000 ? 10 : (conns <= 100000 ? 30 : 60)))
    local chunk_delay=$(calculate_chunk_delay $conns)
    local stream_duration_s=$((chunk_delay * 17 / 1000))
    local hold=$((stream_duration_s + 10))

    # Restart mock API with appropriate timing (TLS for Swow HTTP/2)
    start_mock_api "$chunk_delay" "$use_tls"

    # Start daemon (explicit subshell to capture all output including kill message)
    ( eval "$start_cmd" ) > "$daemon_log" 2>&1 &
    sleep 3

    local pid=$(pgrep -nf "$pattern" 2>/dev/null | head -1)
    if [ -z "$pid" ]; then
        CURRENT_DAEMON_PID=""
        echo '{"error": "daemon_failed_to_start", "connections_failed": '$conns'}' > "$result_json"
        echo "$result_json"
        return 1
    fi
    CURRENT_DAEMON_PID="$pid"

    if [ ! -S "$SOCKET" ]; then
        CURRENT_DAEMON_PID=""
        echo '{"error": "socket_not_created", "connections_failed": '$conns'}' > "$result_json"
        echo "$result_json"
        return 1
    fi

    # Run load generator in background so Ctrl+C can interrupt via trap
    "$LOAD_GEN" -socket "$SOCKET" \
        -connections "$conns" \
        -ramp-up "${rampup}s" \
        -hold "${hold}s" \
        > "$lg_output" 2>&1 &
    CURRENT_LG_PID=$!

    # Wait for load generator (this allows signals to interrupt)
    wait $CURRENT_LG_PID 2>/dev/null || true
    CURRENT_LG_PID=""

    # Get metrics while daemon is still running
    local peak_rss=$(awk '/VmHWM/{print $2}' /proc/$pid/status 2>/dev/null || echo 0)
    local avg_cpu=$(ps -p $pid -o %cpu= 2>/dev/null | tr -d ' ' || echo 0)
    local peak_backlog=$(ss -xl 2>/dev/null | awk -v sock="$SOCKET" '$5 ~ sock {print $3; exit}' || echo 0)
    [ -z "$peak_backlog" ] && peak_backlog=0

    # Kill daemon with SIGKILL (SIGTERM may be ignored when busy)
    kill -9 $pid 2>/dev/null || true
    pkill -9 -f "$pattern" 2>/dev/null || true
    CURRENT_DAEMON_PID=""

    # Extract JSON from load generator
    local json_tmp="${result_prefix}_lg.json"
    sed -n '/^{/,/^}/p' "$lg_output" > "$json_tmp"

    # Parse results
    local completed=$(jq -r '.connections_completed // 0' "$json_tmp" 2>/dev/null || echo 0)
    local failed=$(jq -r '.connections_failed // 0' "$json_tmp" 2>/dev/null || echo 0)
    local ttfb_p50=$(jq -r '.ttfb_latency_ms.p50 // 0' "$json_tmp" 2>/dev/null || echo 0)
    local ttfb_p99=$(jq -r '.ttfb_latency_ms.p99 // 0' "$json_tmp" 2>/dev/null || echo 0)

    # Build combined result JSON
    cat > "$result_json" << EOF
{
    "connections": $conns,
    "completed": $completed,
    "failed": $failed,
    "ttfb_p50_ms": $ttfb_p50,
    "ttfb_p99_ms": $ttfb_p99,
    "peak_rss_kb": $peak_rss,
    "avg_cpu_pct": $avg_cpu,
    "peak_backlog": $peak_backlog
}
EOF

    echo "$result_json"
    set -e  # Re-enable errexit
    return 0
}

#=============================================================================
# EVALUATION FUNCTION
#=============================================================================

# Evaluate if a test run passed all thresholds
# Returns 0 for PASS, 1 for FAIL
# Sets global FAIL_REASON with the limiting factor
evaluate_run() {
    local json_file=$1
    FAIL_REASON=""

    if [ ! -f "$json_file" ]; then
        FAIL_REASON="no_result_file"
        return 1
    fi

    # Check for errors
    local error=$(jq -r '.error // empty' "$json_file" 2>/dev/null)
    if [ -n "$error" ]; then
        FAIL_REASON="$error"
        return 1
    fi

    # Extract metrics (use timeout to prevent hangs)
    local conns=$(timeout 5 jq -r '.connections' "$json_file" 2>/dev/null || echo 0)
    local failed=$(timeout 5 jq -r '.failed' "$json_file" 2>/dev/null || echo 0)
    local ttfb_p99=$(timeout 5 jq -r '.ttfb_p99_ms' "$json_file" 2>/dev/null || echo 0)
    local peak_rss=$(timeout 5 jq -r '.peak_rss_kb' "$json_file" 2>/dev/null || echo 0)
    local avg_cpu=$(timeout 5 jq -r '.avg_cpu_pct' "$json_file" 2>/dev/null || echo 0)
    local peak_backlog=$(timeout 5 jq -r '.peak_backlog' "$json_file" 2>/dev/null || echo 0)

    # Calculate failure rate
    local failure_rate=0
    if [ "$conns" -gt 0 ]; then
        failure_rate=$(echo "scale=4; $failed * 100 / $conns" | bc)
    fi

    # Check thresholds (order matters - first failure is the limiting factor)
    if (( $(echo "$failure_rate > $MAX_FAILURE_RATE" | bc -l) )); then
        FAIL_REASON="failures (${failure_rate}% > ${MAX_FAILURE_RATE}%)"
        return 1
    fi

    if (( $(echo "$ttfb_p99 > $MAX_TTFB_P99_MS" | bc -l) )); then
        FAIL_REASON="p99_ttfb (${ttfb_p99}ms > ${MAX_TTFB_P99_MS}ms)"
        return 1
    fi

    if [ "$peak_rss" -gt "$MAX_RSS_KB" ]; then
        local rss_mb=$(echo "scale=0; $peak_rss/1024" | bc)
        local max_mb=$(echo "scale=0; $MAX_RSS_KB/1024" | bc)
        FAIL_REASON="memory (${rss_mb}MB > ${max_mb}MB)"
        return 1
    fi

    if (( $(echo "$avg_cpu > $MAX_CPU_PCT" | bc -l) )); then
        FAIL_REASON="cpu (${avg_cpu}% > ${MAX_CPU_PCT}%)"
        return 1
    fi

    if [ "$peak_backlog" -gt "$MAX_BACKLOG" ]; then
        FAIL_REASON="backlog ($peak_backlog > $MAX_BACKLOG)"
        return 1
    fi

    return 0
}

#=============================================================================
# BINARY SEARCH ALGORITHM
#=============================================================================

# Find maximum sustainable connections for a daemon
# Usage: find_capacity <daemon_name> <start_cmd> <pattern> [use_tls]
# Returns the maximum connections that pass all thresholds
find_capacity() {
    local name=$1
    local start_cmd=$2
    local pattern=$3
    local use_tls=${4:-false}

    echo -e "${CYAN}=========================================="
    echo "Finding capacity for: $name"
    echo -e "==========================================${NC}"

    # Warmup run
    echo -e "${YELLOW}Running warmup ($WARMUP_CONNECTIONS connections)...${NC}"
    run_single_test "$name" "$start_cmd" "$pattern" "$WARMUP_CONNECTIONS" "warmup" "$use_tls" > /dev/null 2>&1
    cleanup_daemons
    sleep 2

    local low=100
    local high=$INITIAL_HIGH
    local iteration=0
    local last_pass=0
    local last_pass_json=""
    local first_fail=0
    local first_fail_reason=""
    local result_tmp="$RESULTS_DIR/.result_path_$$"

    # Phase 1: Find an upper bound that fails
    echo -e "${YELLOW}Phase 1: Finding upper bound...${NC}"
    while [ $iteration -lt $MAX_ITERATIONS ]; do
        iteration=$((iteration + 1))
        echo -n "  Testing $high connections... "

        # Run test and get result path (filter to last line in case of stray output)
        run_single_test "$name" "$start_cmd" "$pattern" "$high" "expand_$iteration" "$use_tls" > "$result_tmp" 2>/dev/null
        local result_json=$(tail -1 "$result_tmp")
        rm -f "$result_tmp"

        if evaluate_run "$result_json"; then
            echo -e "${GREEN}PASS${NC}"
            last_pass=$high
            last_pass_json="$result_json"
            low=$high

            # Double the upper bound
            high=$((high * 2))
            if [ $high -gt $MAX_CONNECTIONS ]; then
                high=$MAX_CONNECTIONS
                if [ $low -eq $MAX_CONNECTIONS ]; then
                    echo "  Reached system limit at $MAX_CONNECTIONS connections"
                    break
                fi
            fi
        else
            echo -e "${RED}FAIL${NC} ($FAIL_REASON)"
            first_fail=$high
            first_fail_reason="$FAIL_REASON"
            break
        fi
    done

    # If we never found a failure, the max is our highest passing value
    if [ $first_fail -eq 0 ]; then
        echo "  Never found failure point - capacity >= $last_pass"
        FOUND_CAPACITY=$last_pass
        FOUND_JSON="$last_pass_json"
        LIMITING_FACTOR="none (system limit)"
        return
    fi

    # Phase 2: Binary search between low and high
    echo -e "${YELLOW}Phase 2: Binary search between $low and $high...${NC}"
    while [ $((high - low)) -ge $GRANULARITY ] && [ $iteration -lt $MAX_ITERATIONS ]; do
        iteration=$((iteration + 1))
        local mid=$(( (low + high) / 2 ))

        # Round to nearest 100 for cleaner numbers
        mid=$(( (mid / 100) * 100 ))

        echo -n "  Testing $mid connections... "

        run_single_test "$name" "$start_cmd" "$pattern" "$mid" "search_$iteration" "$use_tls" > "$result_tmp" 2>/dev/null
        local result_json=$(tail -1 "$result_tmp")
        rm -f "$result_tmp"

        if evaluate_run "$result_json"; then
            echo -e "${GREEN}PASS${NC}"
            last_pass=$mid
            last_pass_json="$result_json"
            low=$mid
        else
            echo -e "${RED}FAIL${NC} ($FAIL_REASON)"
            first_fail=$mid
            first_fail_reason="$FAIL_REASON"
            high=$mid
        fi
    done

    # Phase 3: Stability confirmation
    if [ $last_pass -gt 0 ]; then
        echo -e "${YELLOW}Phase 3: Confirming stability at $last_pass connections...${NC}"
        local pass_count=0
        for i in $(seq 1 $STABILITY_RUNS); do
            echo -n "  Confirmation run $i/$STABILITY_RUNS... "
            run_single_test "$name" "$start_cmd" "$pattern" "$last_pass" "confirm_$i" "$use_tls" > "$result_tmp" 2>/dev/null
            local result_json=$(tail -1 "$result_tmp")
            rm -f "$result_tmp"

            if evaluate_run "$result_json"; then
                echo -e "${GREEN}PASS${NC}"
                pass_count=$((pass_count + 1))
                last_pass_json="$result_json"
            else
                echo -e "${RED}FAIL${NC} ($FAIL_REASON)"
            fi
        done

        if [ $pass_count -lt $STABILITY_RUNS ]; then
            echo "  Stability check failed ($pass_count/$STABILITY_RUNS). Reducing capacity."
            # Reduce by granularity and report that
            last_pass=$((last_pass - GRANULARITY))
            if [ $last_pass -lt 100 ]; then
                last_pass=100
            fi
        fi
    fi

    FOUND_CAPACITY=$last_pass
    FOUND_JSON="$last_pass_json"
    LIMITING_FACTOR="$first_fail_reason"

    echo ""
    echo -e "${GREEN}Maximum sustainable capacity: $FOUND_CAPACITY connections${NC}"
    echo "First failure at: $first_fail connections ($first_fail_reason)"
}

#=============================================================================
# BUILD DEPENDENCIES
#=============================================================================

build_load_generator
build_mock_api

if should_run uring || should_run uring-http2; then
    build_uring_daemon "openai"
fi

if should_run go || should_run go-http2; then
    build_go_daemon
fi

#=============================================================================
# MAIN BENCHMARK LOOP
#=============================================================================

cleanup

echo ""
echo "Starting capacity benchmark at $(date)"
echo "Daemons to test: ${DAEMONS_TO_RUN[*]}"
echo ""
echo "Thresholds:"
echo "  Max failures: ${MAX_FAILURE_RATE}%"
echo "  Max p99 TTFB: ${MAX_TTFB_P99_MS}ms"
echo "  Max RSS: $(echo "scale=0; $MAX_RSS_KB/1024" | bc)MB"
echo "  Max CPU: ${MAX_CPU_PCT}%"
echo "  Max backlog: ${MAX_BACKLOG}"
echo ""

# Store results for report
declare -A CAPACITY_RESULTS
declare -A LIMITING_FACTORS
declare -A RESULT_JSONS

# Test each daemon
if should_run php; then
    find_capacity "php" "$(get_php_cmd "llm-api" 0 50000)" "streaming_daemon.php"
    CAPACITY_RESULTS[php]=$FOUND_CAPACITY
    LIMITING_FACTORS[php]="$LIMITING_FACTOR"
    RESULT_JSONS[php]="$FOUND_JSON"
fi

if should_run amp-http2; then
    # AMPHP HTTP/2 uses HTTPS (TLS) for real-world performance testing
    find_capacity "amp-http2" "$(get_amp_http2_cmd "llm-api" 0 50000)" "streaming-daemon-amp/streaming_daemon.php" "true"
    CAPACITY_RESULTS[amp-http2]=$FOUND_CAPACITY
    LIMITING_FACTORS[amp-http2]="$LIMITING_FACTOR"
    RESULT_JSONS[amp-http2]="$FOUND_JSON"
fi

if should_run swoole; then
    find_capacity "swoole" "$(get_swoole_cmd "llm-api" 0 "http1" 50000)" "streaming-daemon-swoole/streaming_daemon.php"
    CAPACITY_RESULTS[swoole]=$FOUND_CAPACITY
    LIMITING_FACTORS[swoole]="$LIMITING_FACTOR"
    RESULT_JSONS[swoole]="$FOUND_JSON"
fi

if should_run swoole-http2; then
    # Swoole HTTP/2 uses HTTPS (TLS) for real-world performance testing
    find_capacity "swoole-http2" "$(get_swoole_cmd "llm-api" 0 "http2" 50000)" "streaming-daemon-swoole/streaming_daemon.php" "true"
    CAPACITY_RESULTS[swoole-http2]=$FOUND_CAPACITY
    LIMITING_FACTORS[swoole-http2]="$LIMITING_FACTOR"
    RESULT_JSONS[swoole-http2]="$FOUND_JSON"
fi

if should_run swow; then
    find_capacity "swow" "$(get_swow_cmd "llm-api" 0 "http1" 50000)" "streaming-daemon-swow/streaming_daemon.php"
    CAPACITY_RESULTS[swow]=$FOUND_CAPACITY
    LIMITING_FACTORS[swow]="$LIMITING_FACTOR"
    RESULT_JSONS[swow]="$FOUND_JSON"
fi

if should_run swow-http2; then
    # Swow HTTP/2 requires TLS for proper curl_multi connection reuse
    find_capacity "swow-http2" "$(get_swow_cmd "llm-api" 0 "http2" 50000)" "streaming-daemon-swow/streaming_daemon.php" "true"
    CAPACITY_RESULTS[swow-http2]=$FOUND_CAPACITY
    LIMITING_FACTORS[swow-http2]="$LIMITING_FACTOR"
    RESULT_JSONS[swow-http2]="$FOUND_JSON"
fi

if should_run go; then
    find_capacity "go" "$(get_go_cmd "llm-api" 0 "http1")" "streaming-daemon-go/streaming-daemon"
    CAPACITY_RESULTS[go]=$FOUND_CAPACITY
    LIMITING_FACTORS[go]="$LIMITING_FACTOR"
    RESULT_JSONS[go]="$FOUND_JSON"
fi

if should_run go-http2; then
    find_capacity "go-http2" "$(get_go_cmd "llm-api" 0 "http2")" "streaming-daemon-go/streaming-daemon" "true"
    CAPACITY_RESULTS[go-http2]=$FOUND_CAPACITY
    LIMITING_FACTORS[go-http2]="$LIMITING_FACTOR"
    RESULT_JSONS[go-http2]="$FOUND_JSON"
fi

if should_run rust; then
    find_capacity "rust" "$(get_rust_cmd "llm-api" 0 "http1")" "streaming-daemon-rs"
    CAPACITY_RESULTS[rust]=$FOUND_CAPACITY
    LIMITING_FACTORS[rust]="$LIMITING_FACTOR"
    RESULT_JSONS[rust]="$FOUND_JSON"
fi

if should_run rust-http2; then
    find_capacity "rust-http2" "$(get_rust_cmd "llm-api" 0 "http2")" "streaming-daemon-rs" "true"
    CAPACITY_RESULTS[rust-http2]=$FOUND_CAPACITY
    LIMITING_FACTORS[rust-http2]="$LIMITING_FACTOR"
    RESULT_JSONS[rust-http2]="$FOUND_JSON"
fi

if should_run uring; then
    find_capacity "uring" "$(get_uring_cmd "llm-api" 0 "http1")" "streaming-daemon-uring"
    CAPACITY_RESULTS[uring]=$FOUND_CAPACITY
    LIMITING_FACTORS[uring]="$LIMITING_FACTOR"
    RESULT_JSONS[uring]="$FOUND_JSON"
fi

if should_run uring-http2; then
    # io_uring HTTP/2 uses HTTPS (TLS) for proper HTTP/2 multiplexing
    find_capacity "uring-http2" "$(get_uring_cmd "llm-api" 0 "http2")" "streaming-daemon-uring" "true"
    CAPACITY_RESULTS[uring-http2]=$FOUND_CAPACITY
    LIMITING_FACTORS[uring-http2]="$LIMITING_FACTOR"
    RESULT_JSONS[uring-http2]="$FOUND_JSON"
fi

if should_run python-http2; then
    # Python HTTP/2 requires TLS for proper curl_multi connection reuse
    find_capacity "python-http2" "$(get_python_http2_cmd "llm-api" 0)" "streaming_daemon_async.py" "true"
    CAPACITY_RESULTS[python-http2]=$FOUND_CAPACITY
    LIMITING_FACTORS[python-http2]="$LIMITING_FACTOR"
    RESULT_JSONS[python-http2]="$FOUND_JSON"
fi

#=============================================================================
# GENERATE REPORT
#=============================================================================

echo ""
echo -e "${YELLOW}=========================================="
echo "Generating Capacity Report"
echo -e "==========================================${NC}"

REPORT_FILE="$RESULTS_DIR/CAPACITY_REPORT.md"

cat > "$REPORT_FILE" << EOF
# Daemon Capacity Report

Generated: $(date)

## Thresholds

| Metric | Limit |
|--------|-------|
| Max Failures | ${MAX_FAILURE_RATE}% |
| Max p99 TTFB | ${MAX_TTFB_P99_MS}ms |
| Max RSS Memory | $(echo "scale=0; $MAX_RSS_KB/1024" | bc)MB |
| Max CPU | ${MAX_CPU_PCT}% |
| Max Accept Queue Backlog | ${MAX_BACKLOG} |

## Summary

| Daemon | Max Connections | TTFB p50 | TTFB p99 | Peak RSS | Avg CPU | Limiting Factor |
|--------|-----------------|----------|----------|----------|---------|-----------------|
EOF

for daemon in rust rust-http2 amp-http2 swoole swoole-http2 swow swow-http2 go go-http2 uring uring-http2 php python-http2; do
    if [ -n "${CAPACITY_RESULTS[$daemon]}" ]; then
        json="${RESULT_JSONS[$daemon]}"
        capacity="${CAPACITY_RESULTS[$daemon]}"
        limiting="${LIMITING_FACTORS[$daemon]}"

        if [ -f "$json" ]; then
            ttfb_p50=$(jq -r '.ttfb_p50_ms // 0' "$json")
            ttfb_p99=$(jq -r '.ttfb_p99_ms // 0' "$json")
            peak_rss=$(jq -r '.peak_rss_kb // 0' "$json")
            avg_cpu=$(jq -r '.avg_cpu_pct // 0' "$json")
            rss_mb=$(echo "scale=1; $peak_rss/1024" | bc)

            printf "| %s | %s | %.3fms | %.3fms | %sMB | %s%% | %s |\n" \
                "${daemon^}" "$capacity" "$ttfb_p50" "$ttfb_p99" "$rss_mb" "$avg_cpu" "$limiting" >> "$REPORT_FILE"
        else
            printf "| %s | %s | - | - | - | - | %s |\n" \
                "${daemon^}" "$capacity" "$limiting" >> "$REPORT_FILE"
        fi
    fi
done

cat >> "$REPORT_FILE" << 'EOF'

## Notes

- **Max Connections**: Maximum sustainable concurrent connections that pass all thresholds
- **TTFB**: Time to First Byte (socket handoff to first SSE message received)
- **Peak RSS**: Peak Resident Set Size from /proc/PID/status
- **Limiting Factor**: The metric that caused the first failure above the max capacity
- Results confirmed with stability runs to ensure consistency

## HTTP/2 Mode

- **php**: Uses AMPHP with HTTP/1.1 via Unix socket
- **amp-http2**: Uses AMPHP with HTTP/2 via HTTPS (TLS) and amphp/http-client
- **swoole**: Uses HTTP/1.1 with TCP connection to mock API
- **swoole-http2**: Uses HTTP/2 with HTTPS (TLS) for stream multiplexing
- **swow**: Uses HTTP/1.1 with TCP connection to mock API (via curl)
- **swow-http2**: Uses HTTP/2 with HTTPS (TLS) via curl_multi (Swow requires TLS for connection reuse)
- **go**: Uses HTTP/1.1 with Unix socket for connection pooling to mock API
- **go-http2**: Uses HTTP/2 with HTTPS (TLS) for stream multiplexing
- **rust**: Uses HTTP/1.1 with Unix socket for connection pooling to mock API
- **rust-http2**: Uses HTTP/2 with HTTPS (TLS) for stream multiplexing
- **uring**: Uses HTTP/1.1 with Unix socket for connection pooling to mock API
- **uring-http2**: Uses HTTP/2 with HTTPS (TLS) via libcurl multi interface
- **python-http2**: Uses HTTP/2 with HTTPS (TLS) via PycURL curl_multi integration with asyncio
  - With HTTP/2, ~100 streams share each TCP connection vs 1 stream per connection with HTTP/1.1
  - This reduces connection overhead from 100k to ~1k connections for 100k concurrent streams

## Methodology

1. **Warmup**: Run 100 connections to warm up daemon (JIT, caches)
2. **Expansion**: Start at 10,000 connections, double until failure
3. **Binary Search**: Narrow down between last pass and first fail
4. **Confirmation**: Run 3 tests at found capacity to confirm stability
EOF

echo ""
echo "Capacity benchmark complete!"
echo "Results saved to: $RESULTS_DIR"
echo ""
echo "Summary:"
cat "$REPORT_FILE"
