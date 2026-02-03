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
-max-connections int    Maximum concurrent connections (default 1000)
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

Event-driven daemon using AMPHP with PHP 8.1+ Fibers and HTTP/2 multiplexing via amphp/http-client.

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
- HTTP/2 multiplexing via amphp/http-client with TLS
- Low latency at low concurrency
- Connection pool with configurable limits

**Options:**
```
-s, --socket PATH        Unix socket path
-m, --mode MODE          Socket permissions in octal (default: 0660)
-d, --delay MS           Delay between SSE messages (default: 50)
--backend TYPE           Backend: mock or openai (default: mock)
--openai-base URL        OpenAI API base URL
--openai-socket PATH     Unix socket for OpenAI API
--benchmark              Enable benchmark mode
--max-connections NUM    Max concurrent connections (default: 50000)
```

### PHP Swoole Daemon (`streaming-daemon-swoole/`)

High-performance daemon using Swoole's native coroutine scheduler and HTTP/2 client.

```bash
cd streaming-daemon-swoole
sudo php -d extension=swoole.so streaming_daemon.php -s /var/run/streaming-daemon.sock -m 0666
```

**Features:**
- Native Swoole coroutines with channel-based dispatch
- Native HTTP/2 client (Swoole\Coroutine\Http2\Client)
- HTTP/2 connection pool with 100 streams per connection
- Low memory usage at scale

**Options:**
```
-s, --socket PATH        Unix socket path
-m, --mode MODE          Socket permissions in octal (default: 0660)
-d, --delay MS           Delay between SSE messages (default: 50)
--backend TYPE           Backend: mock or openai (default: mock)
--openai-base URL        OpenAI API base URL (use https:// for HTTP/2)
--benchmark              Enable benchmark mode
--max-connections NUM    Max concurrent connections (default: 50000)
```

**Environment Variables:**
```
OPENAI_API_KEY           API key for OpenAI backend
OPENAI_INSECURE_SSL      Set to "true" to skip TLS verification
OPENAI_HTTP2_ENABLED     Set to "true" for HTTP/2 (default: true for https://)
```

### PHP Swow Daemon (`streaming-daemon-swow/`)

High-performance daemon using Swow's coroutine scheduler and libcurl for HTTP/2.

```bash
cd streaming-daemon-swow
sudo php -d extension=swow.so streaming_daemon.php -s /var/run/streaming-daemon.sock -m 0666
```

**Features:**
- Swow coroutines with libcurl HTTP/2 integration
- curl_multi for efficient HTTP/2 multiplexing via TLS
- Excellent scaling with consistent p99 latency (~2ms at 10k connections)
- Low CPU usage (27% at 10k vs 84% for AMP)

**Options:**
```
-s, --socket PATH        Unix socket path
-m, --mode MODE          Socket permissions in octal (default: 0660)
-d, --delay MS           Delay between SSE messages (default: 50)
--backend TYPE           Backend: mock or openai (default: mock)
--openai-base URL        OpenAI API base URL (use https:// for HTTP/2)
--benchmark              Enable benchmark mode
--max-connections NUM    Max concurrent connections (default: 50000)
```

**Environment Variables:**
```
OPENAI_API_KEY           API key for OpenAI backend
OPENAI_HTTP2_ENABLED     Set to "true" for HTTP/2 multiplexing (requires https://)
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
-b, --backlog NUM    Listen backlog (default: 1024)
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

At 10,000 concurrent connections with HTTP/2 backends (from `benchmarks/`):

| Daemon | TTFB p50 | TTFB p99 | Peak RSS | CPU | Per-conn |
|--------|----------|----------|----------|-----|----------|
| Rust | 0.05ms | 1.4ms | 124 MB | 3% | ~12 KB |
| Go | 0.05ms | 28ms | 272 MB | 32% | ~26 KB |
| Swow (PHP) | 0.47ms | 2ms | 431 MB | 27% | ~37 KB |
| Swoole (PHP) | 0.08ms | 1.3ms | 175 MB | 14% | ~18 KB |
| Python | 18ms | 543ms | 847 MB | 59% | ~84 KB |
| AMP (PHP) | 1.4ms | 2516ms | 1332 MB | 83% | ~140 KB |

**Recommendations:**
- **Rust** - Best overall: lowest memory (~12 KB/conn), lowest CPU, best p99 latency
- **Go** - Good balance of performance and simplicity, slightly higher memory
- **Swoole** - Best PHP option for low latency, native HTTP/2 support
- **Swow** - Excellent PHP scaling with consistent p99 latency via curl_multi
- **Python** - Good for moderate concurrency, uses PycURL for HTTP/2 multiplexing
- **AMP** - Good at low concurrency but doesn't scale well above 1k connections

## Systemd Service

A sample systemd unit file is provided in `streaming-daemon.service`. Customize the `ExecStart` line for your chosen daemon.

```bash
sudo cp streaming-daemon.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable streaming-daemon
sudo systemctl start streaming-daemon
```
