#!/bin/bash
#
# Comprehensive Streaming Daemon Benchmark
# Run as: sudo ./run_full_benchmark.sh
#
# Tests all 4 daemon implementations at multiple connection levels
# and measures TTFB, memory, CPU, and peak concurrency.
#

set -e

# Check for root
if [ "$EUID" -ne 0 ]; then
    echo "This script must be run as root (sudo)."
    echo "Usage: sudo $0"
    exit 1
fi

# Increase file descriptor limit for this script and all child processes
# The load generator needs many fds for concurrent connections
ulimit -n 500000
echo "File descriptor limit set to: $(ulimit -n)"

# Increase max memory mappings for PHP fibers (each fiber uses multiple mappings)
# Default is 65530 which is not enough for 50k+ concurrent fibers
ORIG_MAX_MAP_COUNT=$(sysctl -n vm.max_map_count)
sysctl -w vm.max_map_count=262144 >/dev/null
echo "vm.max_map_count set to: $(sysctl -n vm.max_map_count) (was: $ORIG_MAX_MAP_COUNT)"

# Restore vm.max_map_count on exit
restore_max_map_count() {
    sysctl -w vm.max_map_count="$ORIG_MAX_MAP_COUNT" >/dev/null 2>&1 || true
}
trap 'cleanup; restore_max_map_count' EXIT INT TERM

SOCKET="/var/run/streaming-daemon.sock"
LOAD_GEN="$(dirname "$0")/load-generator/load-generator"
RESULTS_DIR="$(dirname "$0")/results-$(date +%Y%m%d-%H%M%S)"
MESSAGE_DELAY_MS=5625  # ~96 second streams with 18 messages (17 delays × 5625ms)

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

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

cleanup() {
    # Kill load generator first
    pkill -9 -f "load-generator" 2>/dev/null || true

    # Get PIDs of actual daemon binaries (not shell wrappers)
    # Use pgrep with more specific patterns to avoid matching shell processes
    local go_pids=$(pgrep -f "streaming-daemon-go/streaming-daemon" 2>/dev/null || true)
    local rust_pids=$(pgrep -f "streaming-daemon-rs" 2>/dev/null || true)
    local py_pids=$(pgrep -f "streaming_daemon_async.py" 2>/dev/null || true)
    local php_pids=$(pgrep -f "streaming_daemon.php" 2>/dev/null || true)

    # Send SIGTERM first for graceful shutdown (allows socket cleanup)
    for pid in $go_pids $rust_pids $py_pids $php_pids; do
        if [ -n "$pid" ]; then
            kill -TERM "$pid" 2>/dev/null || true
        fi
    done

    # Wait up to 5 seconds for graceful shutdown
    local tries=0
    while [ $tries -lt 5 ]; do
        local still_running=false
        for pid in $go_pids $rust_pids $py_pids $php_pids; do
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
    for pid in $go_pids $rust_pids $py_pids $php_pids; do
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

        cleanup

        # Calculate ramp-up time based on connection count
        # Hold time must be >= stream duration (~96s) to allow all streams to complete
        local rampup=$((conns <= 10000 ? 10 : (conns <= 100000 ? 30 : 60)))
        local hold=100

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
        local peak_concurrent=$(grep -o "Peak concurrent streams: [0-9]*" "$daemon_log" 2>/dev/null | grep -o '[0-9]*' | tail -1 || echo "-")

        # Stop daemon and get final summary
        pkill -TERM -f "$pattern" 2>/dev/null || true
        sleep 2

        # If peak_concurrent not found yet, check again after shutdown
        if [ "$peak_concurrent" = "-" ] || [ -z "$peak_concurrent" ]; then
            peak_concurrent=$(grep -o "Peak concurrent streams: [0-9]*" "$daemon_log" 2>/dev/null | grep -o '[0-9]*' | tail -1 || echo "0")
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

    cleanup
}

# Make sure load generator exists
if [ ! -x "$LOAD_GEN" ]; then
    echo "Building load generator..."
    cd "$(dirname "$0")/load-generator"
    go build -o load-generator .
    cd -
fi

cleanup

echo "Starting comprehensive benchmark at $(date)"
echo ""

# Test PHP
test_daemon "php" \
    "php -dextension=ev.so /home/rlerdorf/mod_socket_handoff/examples/streaming-daemon-amp/streaming_daemon.php -s $SOCKET -m 0666 -d $MESSAGE_DELAY_MS --benchmark" \
    "streaming_daemon.php"

# Test Python (using uvloop + 500 thread workers for high concurrency)
test_daemon "python" \
    "python3 /home/rlerdorf/mod_socket_handoff/examples/streaming_daemon_async.py -s $SOCKET -m 0666 -d $MESSAGE_DELAY_MS -b 8192 -w 500 --benchmark" \
    "streaming_daemon_async"

# Test Go (disable metrics to avoid port conflicts between test runs)
test_daemon "go" \
    "/home/rlerdorf/mod_socket_handoff/examples/streaming-daemon-go/streaming-daemon -benchmark -message-delay ${MESSAGE_DELAY_MS}ms -max-connections ${MAX_CONNECTIONS} -socket-mode 0666 -metrics-addr=" \
    "streaming-daemon-go/streaming-daemon"

# Test Rust
test_daemon "rust" \
    "RUST_LOG=warn DAEMON_TOKEN_DELAY_MS=$MESSAGE_DELAY_MS DAEMON_SOCKET_MODE=0666 DAEMON_METRICS_ENABLED=false /home/rlerdorf/mod_socket_handoff/examples/streaming-daemon-rs/target/release/streaming-daemon-rs --benchmark -s $SOCKET -b mock" \
    "streaming-daemon-rs"


# Generate summary report
echo ""
echo -e "${YELLOW}=========================================="
echo "Generating Summary Report"
echo -e "==========================================${NC}"

cat > "$RESULTS_DIR/REPORT.md" << 'EOF'
# Streaming Daemon Benchmark Report

Generated: DATE_PLACEHOLDER

## Configuration
- Stream duration: ~96 seconds (18 messages x 5625ms delay)
- Hold time: 100 seconds after ramp-up
- Connection levels: 100, 1,000, 10,000, 50,000, 100,000

## Results

EOF

sed -i "s/DATE_PLACEHOLDER/$(date)/" "$RESULTS_DIR/REPORT.md"

for daemon in php python go rust; do
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
