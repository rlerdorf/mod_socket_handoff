# streaming-daemon-rs

Production-ready Rust daemon for receiving Apache socket handoffs and streaming LLM responses.

## Overview

This daemon receives client connections from Apache via `mod_socket_handoff` using Unix domain socket fd passing (`SCM_RIGHTS`). Once the connection is handed off, the daemon streams responses directly to the client while the Apache worker is freed immediately.

```
Client ──TCP──> Apache ──Unix Socket──> streaming-daemon-rs ──────HTTP/2──────> LLM
                  │         (fd pass)           │           <---SSE response--- API
                  │                             │
                  │                             v
            Worker freed              Streams SSE to client
```

## Features

- **Async I/O** - Built on Tokio for high-performance async operations
- **SCM_RIGHTS fd receiving** - Uses `nix` crate for robust fd passing
- **Pluggable backends** - Mock (for testing) and OpenAI streaming
- **SSE streaming** - Server-Sent Events format for real-time responses
- **Prometheus metrics** - Built-in metrics endpoint at `/metrics`
- **Graceful shutdown** - Drains connections on SIGTERM/SIGINT
- **Connection limiting** - Configurable max connections (default: 100,000)
- **Per-write timeouts** - Prevents hung connections from blocking resources
- **HTTP/2 multiplexing** - Upstream API connections share TCP connections via HTTP/2
- **Benchmark mode** - Skip Prometheus overhead for performance testing

## Requirements

- Rust 1.80+ (uses edition 2021 features)
- Linux (for SCM_RIGHTS support)
- Apache with `mod_socket_handoff` enabled

## Building

```bash
cd examples/streaming-daemon-rs

# Debug build
cargo build

# Release build (optimized, stripped)
cargo build --release
```

The release binary will be at `target/release/streaming-daemon-rs`.

## Usage

### Quick Start (Mock Backend)

```bash
# Start daemon with mock backend for testing
./target/release/streaming-daemon-rs --backend mock

# Or with explicit socket path
./target/release/streaming-daemon-rs --socket /var/run/streaming-daemon-rs.sock --backend mock
```

### With Configuration File

```bash
./target/release/streaming-daemon-rs config/daemon.toml
```

### Environment Variables

The following settings can be overridden via environment variables:

```bash
# Server settings
export DAEMON_SOCKET_PATH=/var/run/streaming-daemon-rs.sock
export DAEMON_MAX_CONNECTIONS=50000
export DAEMON_HANDOFF_TIMEOUT_SECS=5
export DAEMON_WRITE_TIMEOUT_SECS=30
export DAEMON_SHUTDOWN_TIMEOUT_SECS=120
export DAEMON_SOCKET_MODE=0o660          # Octal format supported
export DAEMON_HANDOFF_BUFFER_SIZE=65536
export DAEMON_BACKEND_TIMEOUT_SECS=30   # Backend stream creation timeout

# Backend selection
export DAEMON_BACKEND_PROVIDER=openai    # or "mock"
export DAEMON_DEFAULT_MODEL=gpt-4o
export DAEMON_TOKEN_DELAY_MS=50         # Mock backend: delay between tokens (ms)

# OpenAI settings
export OPENAI_API_KEY=sk-...
export OPENAI_API_BASE=https://api.openai.com/v1
export OPENAI_API_SOCKET=/var/run/proxy.sock  # Unix socket (HTTP/1.1 mode only)
export OPENAI_HTTP2_ENABLED=true        # false or 0 to disable
export OPENAI_INSECURE_SSL=false        # true or 1 for self-signed certs

# Metrics
export DAEMON_METRICS_ENABLED=true
export DAEMON_METRICS_ADDR=127.0.0.1:9090

# Logging
export DAEMON_LOG_LEVEL=debug
export DAEMON_LOG_FORMAT=pretty          # or "json"
export RUST_LOG=streaming_daemon_rs=debug
```

### Command Line Options

```
streaming-daemon-rs [OPTIONS] [CONFIG]

Arguments:
  [CONFIG]  Path to configuration file (TOML)

Options:
  -s, --socket <SOCKET>    Override socket path
  -b, --backend <BACKEND>  Override backend provider (mock, openai)
  -d, --debug              Enable debug logging
      --benchmark          Skip Prometheus updates, print summary on shutdown
  -h, --help               Print help
  -V, --version            Print version
```

## Configuration

See `config/daemon.toml` for all options:

```toml
[server]
socket_path = "/var/run/streaming-daemon-rs.sock"
socket_mode = "0o660"  # Restrict access to owner/group
max_connections = 100000
handoff_timeout_secs = 5
write_timeout_secs = 30
shutdown_timeout_secs = 120
handoff_buffer_size = 65536
backend_timeout_secs = 30  # Timeout for backend stream creation

[backend]
provider = "mock"  # or "openai"
default_model = "gpt-4o"
timeout_secs = 120

[backend.openai]
# api_key = "sk-..."          # Or set OPENAI_API_KEY env var
api_base = "https://api.openai.com/v1"
# api_socket = "/var/run/proxy.sock"  # Unix socket (HTTP/1.1 only)
pool_max_idle_per_host = 100
insecure_ssl = false           # Skip TLS verification (testing only)

[backend.openai.http2]
enabled = true                 # HTTP/2 multiplexing (default: true)
initial_stream_window_kb = 1024
initial_connection_window_kb = 10240
adaptive_window = true
keep_alive_interval_secs = 10  # HTTP/2 PING interval (0 = disabled)
keep_alive_timeout_secs = 20

[metrics]
enabled = true
listen_addr = "127.0.0.1:9090"

[logging]
level = "info"
format = "pretty"  # or "json"
```

## Apache Configuration

Ensure `mod_socket_handoff` is enabled and configured:

```apache
# /etc/apache2/mods-enabled/socket_handoff.conf
SocketHandoffEnabled On
SocketHandoffAllowedPrefix /run/
SocketHandoffConnectTimeoutMs 100
```

Note: The systemd service uses `RuntimeDirectory` which creates the socket under
`/run/`. While `/var/run` is typically a symlink to `/run` on modern Linux systems,
using `/run/` explicitly is more consistent with systemd conventions.

## PHP Integration

Example PHP endpoint that hands off to the daemon:

```php
<?php
// stream.php - Handoff endpoint

// 1. Authenticate the request
session_start();
if (!isset($_SESSION['user_id'])) {
    http_response_code(401);
    exit('Unauthorized');
}

// 2. Prepare handoff data
$data = json_encode([
    'prompt' => $_GET['prompt'] ?? 'Hello',
    'user_id' => $_SESSION['user_id'],
    'model' => 'gpt-4o',
    'request_id' => uniqid('req_', true),
]);

// 3. Set handoff headers (path must match systemd service socket path)
header('X-Socket-Handoff: /run/streaming-daemon-rs/daemon.sock');
header('X-Handoff-Data: ' . $data);

// 4. Exit - mod_socket_handoff takes over
exit;
```

## Backends

### Mock Backend

Simulates streaming responses for testing. Streams a canned response word-by-word with configurable delay between tokens (default: 50ms).

```bash
./streaming-daemon-rs --backend mock

# Custom token delay (ms) for benchmarking
DAEMON_TOKEN_DELAY_MS=10 ./streaming-daemon-rs --backend mock
```

### OpenAI Backend

Streams responses from OpenAI's chat completions API.

```bash
export OPENAI_API_KEY=sk-...
./streaming-daemon-rs --backend openai
```

The handoff data JSON can specify:
- `prompt` - The user message (used if `messages` is absent)
- `messages` - Full conversation history (`[{"role": "user", "content": "..."}]`)
- `model` - Model to use (default: gpt-4o)
- `system` - System prompt
- `max_tokens` - Maximum response tokens
- `temperature` - Sampling temperature
- `user_id` - User ID for tracking
- `request_id` - Request ID for log correlation

## Signals

- **SIGTERM/SIGINT** - Initiates graceful shutdown, stops accepting new connections, waits for active streams to complete
- **SIGHUP** - Logs current connection count (status report)

## Benchmark Mode

When started with `--benchmark`, the daemon skips all Prometheus metric updates to eliminate their overhead during performance testing. Instead, it tracks lightweight atomic counters and prints a summary on shutdown:

```
=== Benchmark Summary ===
Peak concurrent streams: 5000
Total started: 50000
Total completed: 49998
Total failed: 2
Total bytes sent: 12500000
=========================
```

This is useful for measuring raw throughput without Prometheus serialization costs.

## Prometheus Metrics

When metrics are enabled, a Prometheus endpoint is available:

```bash
curl http://localhost:9090/metrics
```

Note: The Rust metrics library only exposes metrics after they've been recorded at least once.
Metrics will appear as requests are processed.

### Available Metrics

#### Connection Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `daemon_active_connections` | Gauge | - | Currently active Apache handoff connections |
| `daemon_connections_total` | Counter | - | Total connections accepted |
| `daemon_connections_rejected_total` | Counter | - | Connections rejected due to capacity limits |

#### Stream Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `daemon_active_streams` | Gauge | - | Currently active streaming responses |
| `daemon_peak_streams` | Gauge | - | Peak concurrent streams since startup |
| `daemon_bytes_sent_total` | Counter | - | Total bytes sent to clients |
| `daemon_chunks_sent_total` | Counter | - | Total SSE chunks sent to clients |
| `daemon_stream_errors_total` | Counter | `reason` | Stream errors by type |
| `daemon_stream_duration_seconds` | Histogram | - | Total stream duration per request |

#### Handoff Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `daemon_handoffs_total` | Counter | - | Successful fd handoffs received |
| `daemon_handoff_errors_total` | Counter | `reason` | Handoff errors by type |
| `daemon_handoff_duration_seconds` | Histogram | - | Time spent receiving fd handoff from Apache |

#### Backend Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `daemon_backend_requests_total` | Counter | `backend` | Total backend API requests made |
| `daemon_backend_errors_total` | Counter | `backend` | Total backend API errors |
| `daemon_backend_duration_seconds` | Histogram | `backend` | Backend request total duration |
| `daemon_backend_ttfb_seconds` | Histogram | `backend` | Time to first byte from backend |

### Error Reason Labels

The `reason` label on error counters can have the following values:

| Reason | Description |
|--------|-------------|
| `client_disconnected` | Client closed connection (EPIPE, ECONNRESET) |
| `timeout` | Operation timed out |
| `canceled` | Operation was canceled |
| `network` | Network-related error |
| `other` | Other/unknown error type |

### Example Queries

```promql
# Current connection count
daemon_active_connections

# Connection accept rate (per second)
rate(daemon_connections_total[1m])

# 99th percentile stream duration
histogram_quantile(0.99, rate(daemon_stream_duration_seconds_bucket[5m]))

# Bytes per second throughput
rate(daemon_bytes_sent_total[1m])

# Chunks per second
rate(daemon_chunks_sent_total[1m])

# Backend request rate by backend type
rate(daemon_backend_requests_total[1m])

# Backend TTFB 95th percentile
histogram_quantile(0.95, rate(daemon_backend_ttfb_seconds_bucket[5m]))

# Client disconnect rate
rate(daemon_stream_errors_total{reason="client_disconnected"}[1m])
```

## Systemd Installation

```bash
# Copy binary
sudo cp target/release/streaming-daemon-rs /usr/local/bin/

# Create config directory
sudo mkdir -p /etc/streaming-daemon-rs
sudo cp config/daemon.toml /etc/streaming-daemon-rs/

# Install systemd service
sudo cp systemd/streaming-daemon-rs.service /etc/systemd/system/
sudo systemctl daemon-reload

# Enable and start
sudo systemctl enable streaming-daemon-rs
sudo systemctl start streaming-daemon-rs

# Check status
sudo systemctl status streaming-daemon-rs
sudo journalctl -u streaming-daemon-rs -f
```

## Project Structure

```
streaming-daemon-rs/
├── Cargo.toml              # Dependencies and build config
├── README.md               # This file
├── config/
│   └── daemon.toml         # Example configuration
├── systemd/
│   └── streaming-daemon-rs.service
└── src/
    ├── main.rs             # Entry point, CLI, signal handling
    ├── lib.rs              # Library root, re-exports
    ├── config.rs           # TOML + env config loading
    ├── error.rs            # Error types (thiserror)
    ├── metrics.rs          # Prometheus metrics
    ├── shutdown.rs         # Graceful shutdown coordination
    ├── server/
    │   ├── mod.rs          # Server module exports
    │   ├── listener.rs     # Unix socket listener
    │   ├── handoff.rs      # SCM_RIGHTS fd receiving
    │   └── connection.rs   # Connection lifecycle handler
    ├── backend/
    │   ├── mod.rs          # Backend module, factory
    │   ├── traits.rs       # StreamingBackend trait
    │   ├── mock.rs         # Mock backend for testing
    │   └── openai.rs       # OpenAI streaming backend
    └── streaming/
        ├── mod.rs          # Streaming module exports
        ├── sse.rs          # SSE formatting
        └── writer.rs       # Async writer with timeouts
```

## How It Works

1. **Apache receives HTTPS/HTTP request** from client
2. **PHP authenticates** and sets `X-Socket-Handoff` header
3. **mod_socket_handoff** intercepts the header in output filter
4. **Apache connects** to daemon's Unix socket
5. **Apache sends client fd** via `SCM_RIGHTS` with handoff data
6. **Apache swaps in dummy socket** and frees worker thread
7. **Daemon receives fd** and parses handoff JSON
8. **Daemon calls backend** (mock or OpenAI)
9. **Daemon streams SSE** directly to client socket
10. **Daemon closes connection** when stream completes

## Performance Considerations

- **Connection limit**: Default 100,000 concurrent connections. Adjust `max_connections` based on available memory (~10KB per connection).
- **File descriptors**: The daemon automatically raises the fd limit to 100,000 on startup. The systemd service sets `LimitNOFILE=200000`. If `max_connections` exceeds the fd limit minus a safety reserve (100 fds), it is automatically capped.
- **HTTP/2 multiplexing**: When enabled (default), upstream API connections use HTTP/2, allowing ~100 concurrent streams per TCP connection. This means 100k streams can share ~1000 connections instead of requiring 100k separate connections, dramatically reducing connection overhead and avoiding ephemeral port exhaustion.
- **HTTP/1.1 Unix socket mode**: When HTTP/2 is disabled, the `api_socket` option routes API traffic through a Unix socket to avoid ephemeral port limits.
- **Unbuffered SSE writes**: The SSE writer writes directly to the TCP socket without buffering, combined with `TCP_NODELAY`, for lowest-latency token delivery.
- **Write timeouts**: Per-write timeouts prevent slow clients from blocking resources indefinitely.
- **Benchmark mode**: Use `--benchmark` to skip Prometheus overhead when measuring raw throughput.

## Troubleshooting

### Permission Denied on Socket

```bash
# Check socket permissions
ls -la /var/run/streaming-daemon-rs.sock

# Ensure www-data (or appropriate service user) can access
sudo chown www-data:www-data /var/run/streaming-daemon-rs.sock
sudo chmod 660 /var/run/streaming-daemon-rs.sock
```

### Connection Refused

```bash
# Check if daemon is running
sudo systemctl status streaming-daemon-rs

# Check logs
sudo journalctl -u streaming-daemon-rs -n 50

# Test socket directly
echo '{"prompt":"test"}' | nc -U /var/run/streaming-daemon-rs.sock
```

### Debug Logging

```bash
# Via environment
RUST_LOG=debug ./streaming-daemon-rs

# Via CLI
./streaming-daemon-rs --debug

# Via config
# logging.level = "debug"
```

## Testing

```bash
# Run unit tests
cargo test

# Run with mock backend for manual testing
cargo run -- --backend mock --debug

# Test from browser
# Visit http://localhost/rust-demo/ (with Apache + PHP frontend)
```

## License

Apache 2.0 - Same as mod_socket_handoff.

## See Also

- [mod_socket_handoff](../../) - The Apache module
- [streaming_daemon.go](../streaming_daemon.go) - Go implementation
- [streaming_daemon.php](../streaming_daemon.php) - PHP implementation
