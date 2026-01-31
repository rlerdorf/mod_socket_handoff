# Streaming Daemon Benchmarks

Benchmark suite for comparing concurrency and memory usage across four streaming daemon implementations, each using a different concurrency model.

## Daemons Tested

| Daemon | Language | Concurrency Model | Location |
|--------|----------|-------------------|----------|
| Go | Go 1.21+ | Goroutine per connection | `examples/streaming-daemon-go/` |
| Python | Python 3.11+ | asyncio event loop | `examples/streaming_daemon_async.py` |
| Rust | Rust (Tokio) | Async tasks | `examples/streaming-daemon-rs/` |
| PHP | PHP 8.1+ (AMP) | Fibers + event loop | `examples/streaming-daemon-amp/` |

## Test Methodology

### What We're Measuring

1. **Handoff Latency**: Time to send the client fd from load generator to daemon via SCM_RIGHTS
2. **Time to First Byte (TTFB)**: Time from handoff completion to receiving first response byte
3. **Stream Duration**: Total time from handoff to connection close
4. **Success Rate**: Percentage of connections that complete without error

### Test Configuration

All daemons are configured identically for fair comparison:

- **SSE Messages**: 18 messages per connection
- **Delay per Message**: 5625ms (for ~100 second streams) or 50ms (for quick tests)
- **Expected Stream Duration**: ~96 seconds (18 × 5625ms) or ~900ms (18 × 50ms)
- **Handoff Data Size**: 40 bytes (default: `{"prompt":"benchmark test","user_id":1}`)

### Load Generator

The custom load generator (`benchmarks/load-generator/`) simulates Apache's SCM_RIGHTS handoff behavior:

1. Creates a Unix socket pair to simulate client connections
2. Connects to the daemon's Unix socket
3. Sends the client fd via SCM_RIGHTS with JSON handoff data
4. Reads the SSE response from the "client" side of the socket pair
5. Records latency metrics (handoff, TTFB, stream duration)

Standard HTTP load testing tools (wrk, ab, hey) cannot be used because they don't support SCM_RIGHTS fd passing.

## Quick Start

### Prerequisites

```bash
# Increase file descriptor limits (for high concurrency tests)
ulimit -n 200000

# Verify Go is installed
go version  # Requires Go 1.21+

# Verify PHP extensions
php -m | grep -E 'sockets|pcntl'

# Install PHP AMP dependencies
cd examples/streaming-daemon-amp && composer install
```

### Build Everything

```bash
cd benchmarks
make build
```

This builds:
- `benchmarks/load-generator/load-generator` - The load generator
- `examples/streaming-daemon-go/streaming-daemon` - Go daemon

### Run a Quick Test

```bash
# Start Go daemon
sudo ./examples/streaming-daemon-go/streaming-daemon -socket-mode 0666 &

# Run 100 connections
./benchmarks/load-generator/load-generator \
  -socket /var/run/streaming-daemon.sock \
  -connections 100 \
  -ramp-up 5s \
  -hold 30s

# Stop daemon
sudo pkill streaming-daemon
```

## Running the Full Benchmark Suite

### Automated Full Suite

The `run_full_benchmark.sh` script runs all four daemons through multiple connection levels (100, 1000, 10000, 50000, 100000) and generates a comparison report:

```bash
cd benchmarks
sudo ./run_full_benchmark.sh
```

This script:
1. Tests each daemon (PHP, Python, Go, Rust) sequentially
2. Monitors memory, CPU, and accept queue backlog
3. Captures peak concurrent connections from each daemon
4. Generates a markdown report in `results-YYYYMMDD-HHMMSS/REPORT.md`

### Manual Testing

For manual testing of individual daemons:

### 1. Go Daemon

```bash
# Start daemon
sudo rm -f /var/run/streaming-daemon.sock
sudo ./examples/streaming-daemon-go/streaming-daemon \
  -max-connections 10000 \
  -socket-mode 0666 &

# Run benchmark
./benchmarks/load-generator/load-generator \
  -socket /var/run/streaming-daemon.sock \
  -connections 1000 \
  -ramp-up 10s \
  -hold 30s \
  -output results/go.json

# Stop daemon
sudo pkill streaming-daemon
```

### 2. Python asyncio Daemon

```bash
# Start daemon
sudo rm -f /var/run/streaming-daemon-py.sock
sudo python3 examples/streaming_daemon_async.py \
  -s /var/run/streaming-daemon-py.sock \
  -m 0666 \
  -w 100 &

# Run benchmark
./benchmarks/load-generator/load-generator \
  -socket /var/run/streaming-daemon-py.sock \
  -connections 1000 \
  -ramp-up 10s \
  -hold 30s \
  -output results/python.json

# Stop daemon
sudo pkill -f streaming_daemon_async
```

### 3. Rust Daemon

```bash
# Build if needed
cd examples/streaming-daemon-rs && cargo build --release && cd ../..

# Start daemon (use different metrics port to avoid conflict)
sudo rm -f /var/run/streaming-daemon-rs.sock
sudo DAEMON_BACKEND_PROVIDER=mock \
     DAEMON_METRICS_ADDR=127.0.0.1:9091 \
     ./examples/streaming-daemon-rs/target/release/streaming-daemon-rs &

# Fix permissions
sleep 2 && sudo chmod 666 /var/run/streaming-daemon-rs.sock

# Run benchmark
./benchmarks/load-generator/load-generator \
  -socket /var/run/streaming-daemon-rs.sock \
  -connections 1000 \
  -ramp-up 10s \
  -hold 30s \
  -output results/rust.json

# Stop daemon
sudo pkill streaming-daemon-rs
```

### 4. PHP AMP Daemon

```bash
# Install dependencies if needed
cd examples/streaming-daemon-amp && composer install && cd ../..

# Start daemon
sudo rm -f /var/run/streaming-daemon-amp.sock
cd examples/streaming-daemon-amp
sudo php streaming_daemon.php \
  -s /var/run/streaming-daemon-amp.sock \
  -m 0666 &
cd ../..

# Run benchmark
./benchmarks/load-generator/load-generator \
  -socket /var/run/streaming-daemon-amp.sock \
  -connections 1000 \
  -ramp-up 10s \
  -hold 30s \
  -output results/php.json

# Stop daemon
sudo pkill -f streaming_daemon.php
```

## Load Generator Options

```
./load-generator [options]

Options:
  -socket string        Unix socket path (default "/var/run/streaming-daemon.sock")
  -connections int      Number of concurrent connections (default 100)
  -ramp-up duration     Time to reach target connections (default 10s)
  -hold duration        Time to hold at target level (default 60s)
  -stream-timeout dur   Timeout for reading response (default 120s)
  -data string          JSON handoff data to send
  -output string        Output file for results (JSON)
  -verbose              Enable verbose per-connection logging
```

### Example: Large Handoff Data Test

```bash
# Test with 10KB prompt
./load-generator \
  -socket /var/run/streaming-daemon.sock \
  -connections 1000 \
  -data '{"prompt":"'"$(python3 -c "print('test ' * 2000)")"'","user_id":1}'
```

## Interpreting Results

### Sample Output

```json
{
  "connections_started": 1000,
  "connections_completed": 1000,
  "connections_failed": 0,
  "bytes_received": 683000,
  "errors": {
    "connect": 0,
    "send": 0,
    "receive": 0,
    "timeout": 0,
    "rejected": 0
  },
  "handoff_latency_ms": {
    "p50": 0.020,
    "p95": 0.027,
    "p99": 0.037
  },
  "ttfb_latency_ms": {
    "p50": 0.100,
    "p95": 0.152,
    "p99": 0.199
  },
  "stream_duration_ms": {
    "p50": 405.15,
    "p95": 406.71,
    "p99": 407.30
  }
}
```

### Key Metrics

| Metric | What It Measures | Good Value |
|--------|------------------|------------|
| `connections_failed` | Connection errors | 0 |
| `handoff_latency_ms.p50` | SCM_RIGHTS overhead | < 0.1ms |
| `ttfb_latency_ms.p50` | Daemon response time | < 1ms |
| `stream_duration_ms.p50` | Total stream time | ~96s (with 18×5625ms delays) |

### Error Categories

- `connect`: Failed to connect to daemon socket
- `send`: Failed to send fd via SCM_RIGHTS
- `receive`: Error reading response
- `timeout`: Stream read timeout
- `rejected`: Connection rejected by daemon (at capacity)

## Benchmark Results (Reference)

Results from testing on a GCP n2-standard-8 instance (8 vCPU, 32GB RAM):

### 100,000 Concurrent Connections

| Metric | Go | Rust | PHP (AMP) | Python |
|--------|-----|------|-----------|--------|
| Success Rate | 100% | 100% | 100% | 78% |
| TTFB p50 | 0.060ms | 0.114ms | 0.098ms | 0.304ms |
| TTFB p99 | 2.88ms | 4.04ms | 0.63ms | 38,795ms |
| Peak RSS | 883 MB | 350 MB | 1114 MB | 458 MB |
| Avg CPU | 21.6% | 17.3% | 16.6% | 31.1% |

### Observations

1. **Go, Rust, and PHP achieve 100% success at 100k concurrent connections**
2. **Memory efficiency**: Rust is most efficient (3.5 KB/conn), followed by Go (8.8 KB/conn), PHP (11.1 KB/conn)
3. **TTFB**: PHP has best p99 latency (0.63ms), Go and Rust around 3-4ms
4. **CPU efficiency**: PHP uses least CPU (16.6%), followed by Rust (17.3%)
5. **Python struggles at high concurrency**: asyncio backlog saturates, causing 22% failure rate

## Daemon Configuration Reference

### Go Daemon Flags

```
-max-connections int    Maximum concurrent connections (default 1000)
-socket-mode uint       Unix socket permissions (default 0660)
-metrics-addr string    Prometheus metrics address (default "127.0.0.1:9090")
-pprof-addr string      pprof server address (empty to disable)
```

### Python Daemon Options

```
-s, --socket PATH       Unix socket path
-m, --mode MODE         Socket permissions (octal)
-w, --workers NUM       ThreadPoolExecutor workers for blocking recvmsg
-d, --delay MS          Delay between SSE messages (default 50)
-b, --backlog NUM       Listen backlog (default 1024)
```

### Rust Daemon Environment Variables

```
DAEMON_SOCKET_PATH          Unix socket path
DAEMON_SOCKET_MODE          Socket permissions
DAEMON_MAX_CONNECTIONS      Max concurrent connections (default 100000)
DAEMON_BACKEND_PROVIDER     Backend: "mock" or "openai"
DAEMON_METRICS_ADDR         Prometheus metrics address
DAEMON_METRICS_ENABLED      Enable metrics server (default true)
```

### PHP AMP Daemon Options

```
-s, --socket PATH       Unix socket path
-m, --mode MODE         Socket permissions (octal)
```

## Troubleshooting

### "Socket already in use"

```bash
sudo rm -f /var/run/streaming-daemon*.sock
sudo pkill -9 streaming
```

### "Permission denied" connecting to socket

```bash
sudo chmod 666 /var/run/streaming-daemon.sock
```

### "Metrics server: Address already in use"

The Go and Rust daemons both use port 9090 for Prometheus metrics. Use different ports:

```bash
# Go: use default 9090
# Rust: use 9091
sudo DAEMON_METRICS_ADDR=127.0.0.1:9091 ./streaming-daemon-rs &
```

### "Too many open files"

```bash
ulimit -n 200000
```

### Load generator shows all connections failed

1. Check daemon is running: `ps aux | grep streaming`
2. Check socket exists: `ls -la /var/run/streaming-daemon*.sock`
3. Check socket permissions: should be `srw-rw-rw-` for testing

## Resource Collection & Analysis

The benchmark suite includes scripts for comprehensive resource collection and analysis.

### Resource Collection Script

`scripts/collect-resources.sh` collects memory, CPU, and connection metrics:

```bash
# Collect resources for a running daemon
./scripts/collect-resources.sh <pid> <output.csv> [metrics_port]

# Example: Go daemon with Prometheus on port 9090
./scripts/collect-resources.sh $(pgrep streaming-daemon) results/go/resources.csv 9090
```

Output CSV columns:
- `timestamp` - Unix timestamp with nanoseconds
- `rss_kb` - Resident Set Size (physical memory)
- `vsz_kb` - Virtual memory size
- `threads` - Thread count
- `utime` - User CPU time (clock ticks)
- `stime` - System CPU time (clock ticks)
- `cpu_pct` - CPU percentage since last sample
- `active_conns` - Active connections from Prometheus (Go/Rust only)

### Run Benchmark Script

`scripts/run-benchmark.sh` orchestrates the full benchmark process:

```bash
# Usage
./scripts/run-benchmark.sh <daemon> [connections] [hold_time] [ramp_up]

# Examples
./scripts/run-benchmark.sh go 1000 30s
./scripts/run-benchmark.sh rust 5000 60s 20s
```

This script:
1. Validates the daemon is running
2. Starts resource collection in the background
3. Runs the load generator
4. Stops resource collection
5. Displays a quick summary

### Analysis Script

`scripts/analyze.py` processes results and generates comparison reports:

```bash
# Analyze a single benchmark run
python3 scripts/analyze.py results/2024-01-15-120000

# Output includes:
# - Connection statistics (started, completed, failed, success %)
# - Latency metrics (handoff p50/p99, TTFB p50/p99)
# - Memory usage (baseline, peak, delta, per-connection)
# - CPU usage (average %, peak %)
# - Peak connections (from Prometheus)
```

### Using Make Targets

```bash
# Build load generator and daemons
make build

# Run benchmark for a single daemon (preferred method)
# Requires daemon to be already running
make run DAEMON=go CONNECTIONS=1000 HOLD=30s

# Or use bench-* targets to start daemon and run benchmark
make bench-go CONNECTIONS=1000 HOLD=30s

# Analyze results
make analyze DIR=results/2024-01-15-120000
```

### Sample Analysis Output

```
================================================================================
BENCHMARK COMPARISON
================================================================================

### Connection Statistics
Daemon           Started  Completed     Failed  Success %
----------------------------------------------------
go                  1000       1000          0      100.0%
rust                1000       1000          0      100.0%

### Latency (milliseconds)
Daemon        Handoff p50  Handoff p99   TTFB p50   TTFB p99
--------------------------------------------------------
go                   0.02         0.04       0.10       0.20
rust                 0.02         0.03       0.21       0.43

### Memory (RSS in MB)
Daemon        Baseline       Peak      Delta  Per-Conn KB
--------------------------------------------------------
go                12.0       22.0       10.0         10.0
rust              18.0       33.0       15.0         15.0

### CPU Usage
Daemon          Avg %      Peak %    Threads
------------------------------------------
go                6.1        10.8       1005
rust              4.3         8.2          4

================================================================================
```
