#!/bin/bash
# Collect memory and CPU statistics for a process.
#
# Usage: ./collect-resources.sh <pid> <output.csv> [metrics_port]
#
# Samples /proc/PID/stat and /proc/PID/status every second, calculating
# CPU percentage and optionally scraping active connections from Prometheus.
#
# Output CSV columns:
#   timestamp,rss_kb,vsz_kb,threads,utime,stime,cpu_pct,active_conns

set -e

PID=$1
OUTPUT=$2
METRICS_PORT=${3:-}  # Optional Prometheus port for active connections

if [ -z "$PID" ] || [ -z "$OUTPUT" ]; then
    echo "Usage: $0 <pid> <output.csv> [metrics_port]"
    echo ""
    echo "Arguments:"
    echo "  pid          - Process ID to monitor"
    echo "  output.csv   - Output file for collected data"
    echo "  metrics_port - Optional Prometheus metrics port (e.g., 9090 for Go, 9091 for Rust)"
    exit 1
fi

if [ ! -d "/proc/$PID" ]; then
    echo "Process $PID not found"
    exit 1
fi

INTERVAL=1

# Write header
echo "timestamp,rss_kb,vsz_kb,threads,utime,stime,cpu_pct,active_conns" > "$OUTPUT"

# Get number of CPU cores for percentage calculation
NUM_CORES=$(nproc)
CLK_TCK=$(getconf CLK_TCK)  # Usually 100

# Initial CPU reading
if [ -f "/proc/$PID/stat" ]; then
    read PREV_UTIME PREV_STIME < <(awk '{print $14, $15}' /proc/$PID/stat 2>/dev/null || echo "0 0")
else
    PREV_UTIME=0
    PREV_STIME=0
fi
PREV_TIME=$(date +%s.%N)

echo "Collecting resources for PID $PID -> $OUTPUT"
echo "  Cores: $NUM_CORES, CLK_TCK: $CLK_TCK"
if [ -n "$METRICS_PORT" ]; then
    echo "  Prometheus metrics: localhost:$METRICS_PORT"
fi

while [ -d "/proc/$PID" ]; do
    TIMESTAMP=$(date +%s.%N)

    # Memory from /proc/PID/status
    if [ -f "/proc/$PID/status" ]; then
        RSS=$(awk '/VmRSS/{print $2}' /proc/$PID/status 2>/dev/null || echo 0)
        VSZ=$(awk '/VmSize/{print $2}' /proc/$PID/status 2>/dev/null || echo 0)
        THREADS=$(awk '/Threads/{print $2}' /proc/$PID/status 2>/dev/null || echo 0)
    else
        RSS=0
        VSZ=0
        THREADS=0
    fi

    # CPU times from /proc/PID/stat (field 14=utime, 15=stime in clock ticks)
    if [ -f "/proc/$PID/stat" ]; then
        read CURR_UTIME CURR_STIME < <(awk '{print $14, $15}' /proc/$PID/stat 2>/dev/null || echo "0 0")
    else
        CURR_UTIME=0
        CURR_STIME=0
    fi

    # Calculate CPU percentage since last sample
    DELTA_UTIME=$((CURR_UTIME - PREV_UTIME))
    DELTA_STIME=$((CURR_STIME - PREV_STIME))
    DELTA_TIME=$(echo "$TIMESTAMP - $PREV_TIME" | bc 2>/dev/null || echo "1")

    # Prevent division by zero
    if [ "$(echo "$DELTA_TIME > 0" | bc 2>/dev/null || echo "0")" = "1" ]; then
        # CPU% = (ticks / CLK_TCK) / elapsed_seconds / num_cores * 100
        CPU_PCT=$(echo "scale=1; ($DELTA_UTIME + $DELTA_STIME) / $CLK_TCK / $DELTA_TIME / $NUM_CORES * 100" | bc 2>/dev/null || echo "0")
    else
        CPU_PCT=0
    fi

    # Active connections from Prometheus (if available)
    if [ -n "$METRICS_PORT" ]; then
        # Try both metric names (Go uses daemon_*, Rust uses streaming_daemon_*)
        CONNS=$(curl -s "localhost:$METRICS_PORT/metrics" 2>/dev/null | \
                grep -E '^(daemon_active_connections|streaming_daemon_active_connections) ' | \
                awk '{print $2}' | head -1)
        CONNS=${CONNS:-0}
    else
        CONNS=""
    fi

    echo "$TIMESTAMP,$RSS,$VSZ,$THREADS,$CURR_UTIME,$CURR_STIME,$CPU_PCT,$CONNS" >> "$OUTPUT"

    # Update previous values
    PREV_UTIME=$CURR_UTIME
    PREV_STIME=$CURR_STIME
    PREV_TIME=$TIMESTAMP

    sleep $INTERVAL
done

echo "Process $PID exited, stopping collection"
