# Example Streaming Daemons

This directory contains example daemon implementations that receive client connections from Apache via `mod_socket_handoff` and stream SSE responses.

## Production-Ready Daemons

These daemons are designed for high-concurrency production use:

### Go Daemon (`streaming-daemon-go/`)

Goroutine-per-connection model using Go's native concurrency.

```bash
cd streaming-daemon-go
go build -o streaming-daemon .
sudo ./streaming-daemon -socket-mode 0666
```

**Features:**
- Goroutine per connection (~2-8 KB stack each)
- Handles 100,000+ concurrent connections
- Prometheus metrics on port 9090
- Benchmark mode with `--benchmark` flag

**Options:**
```
-max-connections int    Maximum concurrent connections (default 100000)
-socket-mode uint       Unix socket permissions (default 0660)
-message-delay duration Delay between SSE messages (default 50ms)
-metrics-addr string    Prometheus metrics address (default ":9090")
-benchmark              Enable benchmark mode
```

### Rust Daemon (`streaming-daemon-rs/`)

Tokio async runtime with task-per-connection model.

```bash
cd streaming-daemon-rs
cargo build --release
sudo DAEMON_BACKEND_PROVIDER=mock ./target/release/streaming-daemon-rs
```

**Features:**
- Async tasks (~1-4 KB each)
- Most memory-efficient (~3.5 KB per connection)
- Handles 100,000+ concurrent connections
- Pluggable backends (mock, OpenAI)
- Prometheus metrics

**Environment Variables:**
```
DAEMON_SOCKET_PATH        Unix socket path (default: /var/run/streaming-daemon.sock)
DAEMON_SOCKET_MODE        Socket permissions (default: 0660)
DAEMON_BACKEND_PROVIDER   Backend: "mock" or "openai"
DAEMON_TOKEN_DELAY_MS     Delay between messages in ms (default: 50)
DAEMON_METRICS_ENABLED    Enable Prometheus metrics (default: true)
```

### PHP AMPHP Daemon (`streaming-daemon-amp/`)

Event-driven daemon using AMPHP with PHP 8.1+ Fibers.

```bash
cd streaming-daemon-amp
composer install
sudo php streaming_daemon.php -s /var/run/streaming-daemon.sock -m 0666
```

**Performance Enhancement - ev extension:**

For significantly better performance, install and use the `ev` PECL extension:

```bash
sudo pecl install ev
sudo php -dextension=ev.so streaming_daemon.php -s /var/run/streaming-daemon.sock
```

The AMPHP Revolt event loop automatically detects and uses `ev` when available, providing faster event handling than the default stream_select() implementation.

**Features:**
- Fiber per connection (~64 KB stack, configurable)
- Handles 100,000+ concurrent connections
- Best p99 TTFB latency in benchmarks
- Lowest CPU usage

**Options:**
```
-s, --socket PATH    Unix socket path
-m, --mode MODE      Socket permissions in octal (default: 0660)
-d, --delay MS       Delay between SSE messages (default: 50)
-b, --benchmark      Enable benchmark mode
```

### Python asyncio Daemon (`streaming_daemon_async.py`)

Event-driven daemon using Python asyncio with optional uvloop.

```bash
sudo python3 streaming_daemon_async.py -s /var/run/streaming-daemon.sock -m 0666
```

**Performance Enhancement - uvloop:**

For 2-4x better performance, install uvloop:

```bash
pip install uvloop
sudo python3 streaming_daemon_async.py -s /var/run/streaming-daemon.sock
```

The daemon automatically detects and uses uvloop when available, replacing the default asyncio event loop with a libuv-based implementation.

**Features:**
- Coroutine per connection (~1-5 KB each)
- Thread pool for blocking SCM_RIGHTS receive
- Handles 50,000+ concurrent connections (limited by GIL)

**Options:**
```
-s, --socket PATH    Unix socket path
-m, --mode MODE      Socket permissions in octal
-w, --workers NUM    Thread pool size for blocking recvmsg (default: 500)
-d, --delay MS       Delay between SSE messages (default: 50)
-b, --backlog NUM    Listen backlog (default: 8192)
--benchmark          Enable benchmark mode
```

## Simple/Test Daemons

These are simpler implementations for testing and learning:

### Python Test Daemon (`test_daemon.py`)

Minimal synchronous daemon for testing the handoff mechanism.

```bash
sudo python3 test_daemon.py
```

Single-threaded, handles one connection at a time. Good for understanding the SCM_RIGHTS protocol.

### PHP Simple Daemon (`streaming_daemon.php`)

Basic PHP daemon without AMPHP dependencies. Uses blocking I/O.

```bash
sudo php streaming_daemon.php
```

### C fdrecv (`fdrecv.c`)

Minimal C daemon that receives the fd and execs an external handler script.

```bash
make fdrecv
sudo ./fdrecv /path/to/handler.sh
```

Useful for prototyping handlers in any language. See `handler.sh` and `figlet_handler.sh` for examples.

## Benchmark Comparison

At 100,000 concurrent connections (from `benchmarks/`):

| Daemon | Success Rate | TTFB p99 | Peak RSS | CPU |
|--------|-------------|----------|----------|-----|
| Go | 100% | 2.88ms | 883 MB | 21.6% |
| Rust | 100% | 4.04ms | 350 MB | 17.3% |
| PHP (AMP) | 100% | 0.63ms | 1114 MB | 16.6% |
| Python | 78% | 38,795ms | 458 MB | 31.1% |

**Recommendations:**
- **Rust** - Best memory efficiency, good for memory-constrained environments
- **Go** - Good balance of performance and simplicity
- **PHP (AMP)** - Best latency, lowest CPU, good if already using PHP
- **Python** - Suitable for moderate concurrency (<50k connections)

## Systemd Service

A sample systemd unit file is provided in `streaming-daemon.service`. Customize the `ExecStart` line for your chosen daemon.

```bash
sudo cp streaming-daemon.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable streaming-daemon
sudo systemctl start streaming-daemon
```
