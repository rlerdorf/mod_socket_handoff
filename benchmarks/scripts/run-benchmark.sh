#!/bin/bash
# Run complete benchmark suite for a single daemon.
#
# This script:
# 1. Validates the daemon is running
# 2. Starts resource collection in the background
# 3. Runs the load generator
# 4. Stops resource collection
# 5. Generates a quick summary
#
# Usage: ./run-benchmark.sh <daemon> [connections] [hold_time] [ramp_up]
#   daemon:      go, rust, python, php
#   connections: number of concurrent connections (default: 1000)
#   hold_time:   hold duration (default: 30s)
#   ramp_up:     ramp-up duration (default: 10s)
#
# Example:
#   ./run-benchmark.sh go 1000 30s
#   ./run-benchmark.sh rust 5000 60s 20s

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BENCHMARK_DIR="$(dirname "$SCRIPT_DIR")"
LOAD_GEN="$BENCHMARK_DIR/load-generator/load-generator"

DAEMON=$1
CONNECTIONS=${2:-1000}
HOLD_TIME=${3:-30s}
RAMP_UP=${4:-10s}

if [ -z "$DAEMON" ]; then
    echo "Usage: $0 <daemon> [connections] [hold_time] [ramp_up]"
    echo ""
    echo "Arguments:"
    echo "  daemon       - Daemon to benchmark: go, rust, python, php"
    echo "  connections  - Number of concurrent connections (default: 1000)"
    echo "  hold_time    - Hold duration at target connections (default: 30s)"
    echo "  ramp_up      - Ramp-up duration (default: 10s)"
    echo ""
    echo "Examples:"
    echo "  $0 go 1000 30s"
    echo "  $0 rust 5000 60s 20s"
    exit 1
fi

# Check load generator exists
if [ ! -x "$LOAD_GEN" ]; then
    echo "Error: Load generator not found at $LOAD_GEN"
    echo "Run 'make build' in the benchmarks directory first."
    exit 1
fi

# Determine socket and metrics port based on daemon type
case $DAEMON in
    go)
        SOCKET="/var/run/streaming-daemon.sock"
        METRICS_PORT=9090
        DAEMON_PATTERN="streaming-daemon"
        ;;
    rust)
        SOCKET="/var/run/streaming-daemon-rs.sock"
        METRICS_PORT=9091
        DAEMON_PATTERN="streaming-daemon-rs"
        ;;
    python)
        SOCKET="/var/run/streaming-daemon-py.sock"
        METRICS_PORT=""
        DAEMON_PATTERN="streaming_daemon_async.py"
        ;;
    php)
        SOCKET="/var/run/streaming-daemon-amp.sock"
        METRICS_PORT=""
        DAEMON_PATTERN="streaming_daemon.php"
        ;;
    *)
        echo "Error: Unknown daemon type: $DAEMON"
        echo "Supported: go, rust, python, php"
        exit 1
        ;;
esac

# Create results directory
TIMESTAMP=$(date +%Y-%m-%d-%H%M%S)
RESULTS_DIR="$BENCHMARK_DIR/results/${TIMESTAMP}/${DAEMON}"
mkdir -p "$RESULTS_DIR"

echo "================================================================================"
echo "BENCHMARKING $DAEMON DAEMON"
echo "================================================================================"
echo ""
echo "Socket:       $SOCKET"
echo "Connections:  $CONNECTIONS"
echo "Ramp-up:      $RAMP_UP"
echo "Hold:         $HOLD_TIME"
echo "Results:      $RESULTS_DIR"
echo ""

# Check socket exists
if [ ! -S "$SOCKET" ]; then
    echo "Error: Socket not found: $SOCKET"
    echo "Make sure the $DAEMON daemon is running."
    exit 1
fi

# Find daemon PID
DAEMON_PID=$(pgrep -f "$DAEMON_PATTERN" | head -1)
if [ -z "$DAEMON_PID" ]; then
    echo "Warning: Could not find daemon PID for pattern '$DAEMON_PATTERN'"
    echo "Resource collection will be skipped."
    COLLECT_RESOURCES=false
else
    echo "Daemon PID:   $DAEMON_PID"
    COLLECT_RESOURCES=true
fi
echo ""

# Start resource collection in background
if [ "$COLLECT_RESOURCES" = true ]; then
    echo "Starting resource collection..."
    "$SCRIPT_DIR/collect-resources.sh" "$DAEMON_PID" "$RESULTS_DIR/resources.csv" "$METRICS_PORT" &
    COLLECTOR_PID=$!
    sleep 2  # Let collector establish baseline
fi

# Run the benchmark
echo "Running benchmark..."
echo ""
"$LOAD_GEN" \
    -socket "$SOCKET" \
    -connections "$CONNECTIONS" \
    -ramp-up "$RAMP_UP" \
    -hold "$HOLD_TIME" \
    -output "$RESULTS_DIR/latency.json"

# Let resources settle
echo ""
echo "Waiting for resources to settle..."
sleep 3

# Stop resource collection
if [ "$COLLECT_RESOURCES" = true ]; then
    kill $COLLECTOR_PID 2>/dev/null || true
    wait $COLLECTOR_PID 2>/dev/null || true
fi

echo ""
echo "================================================================================"
echo "BENCHMARK COMPLETE"
echo "================================================================================"
echo ""
echo "Results saved to: $RESULTS_DIR"
echo ""

# Show quick summary if latency.json exists
if [ -f "$RESULTS_DIR/latency.json" ]; then
    echo "Quick Summary:"
    echo "--------------"
    # Extract key metrics using Python (more portable than jq)
    python3 -c "
import json
with open('$RESULTS_DIR/latency.json') as f:
    data = json.load(f)

started = data.get('connections_started', 0)
completed = data.get('connections_completed', 0)
failed = data.get('connections_failed', 0)
success_pct = (completed / started * 100) if started > 0 else 0

print(f'  Connections: {started} started, {completed} completed, {failed} failed ({success_pct:.1f}% success)')

if 'handoff_latency_ms' in data:
    h = data['handoff_latency_ms']
    print(f'  Handoff latency: p50={h.get(\"p50\", 0):.2f}ms, p99={h.get(\"p99\", 0):.2f}ms')

if 'ttfb_latency_ms' in data:
    t = data['ttfb_latency_ms']
    print(f'  TTFB: p50={t.get(\"p50\", 0):.2f}ms, p99={t.get(\"p99\", 0):.2f}ms')

bytes_recv = data.get('bytes_received', 0)
print(f'  Bytes received: {bytes_recv:,}')
" 2>/dev/null || echo "  (Run analyze.py for detailed report)"
fi

# Show resource summary if resources.csv exists
if [ -f "$RESULTS_DIR/resources.csv" ]; then
    echo ""
    echo "Resource Summary:"
    echo "-----------------"
    python3 -c "
import csv
rss_values = []
cpu_values = []
conn_values = []
with open('$RESULTS_DIR/resources.csv') as f:
    reader = csv.DictReader(f)
    for row in reader:
        try:
            rss_values.append(int(row['rss_kb']))
            cpu_values.append(float(row['cpu_pct']))
            if row.get('active_conns'):
                conn_values.append(int(float(row['active_conns'])))
        except (ValueError, KeyError):
            pass

if rss_values:
    min_mb = min(rss_values) / 1024
    max_mb = max(rss_values) / 1024
    delta_mb = max_mb - min_mb
    print(f'  Memory: {min_mb:.1f} MB (baseline) -> {max_mb:.1f} MB (peak), delta: {delta_mb:.1f} MB')

if cpu_values:
    avg_cpu = sum(cpu_values) / len(cpu_values)
    max_cpu = max(cpu_values)
    print(f'  CPU: {avg_cpu:.1f}% avg, {max_cpu:.1f}% peak')

if conn_values:
    max_conns = max(conn_values)
    print(f'  Peak connections: {max_conns}')
" 2>/dev/null || echo "  (Run analyze.py for detailed report)"
fi

echo ""
echo "To analyze results:"
echo "  python3 $SCRIPT_DIR/analyze.py $BENCHMARK_DIR/results/$TIMESTAMP"
