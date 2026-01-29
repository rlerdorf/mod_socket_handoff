# streaming-daemon-rs

Production-ready Rust daemon for receiving Apache socket handoffs and streaming LLM responses.

## Overview

This daemon receives client connections from Apache via `mod_socket_handoff` using Unix domain socket fd passing (`SCM_RIGHTS`). Once the connection is handed off, the daemon streams responses directly to the client while the Apache worker is freed immediately.

```
Client ──TCP──> Apache ──Unix Socket──> streaming-daemon-rs
                  │         (fd pass)           │
                  │                             │
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

### Future Enhancements

- **io_uring support** - The Cargo.toml includes a commented `uring` feature for `tokio-uring` integration. When enabled, this would use Linux's io_uring (kernel 5.1+) for even higher throughput through reduced syscall overhead. Currently experimental and not enabled by default.

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

All configuration can be set via environment variables:

```bash
# Socket path
export DAEMON_SOCKET_PATH=/var/run/streaming-daemon-rs.sock

# Backend selection
export DAEMON_BACKEND_PROVIDER=openai
export OPENAI_API_KEY=sk-...

# Connection limits
export DAEMON_MAX_CONNECTIONS=50000

# Logging
export DAEMON_LOG_LEVEL=debug
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
  -h, --help               Print help
  -V, --version            Print version
```

## Configuration

See `config/daemon.toml` for all options:

```toml
[server]
socket_path = "/var/run/streaming-daemon-rs.sock"
socket_mode = "0o666"
max_connections = 100000
handoff_timeout_secs = 5
write_timeout_secs = 30
shutdown_timeout_secs = 120

[backend]
provider = "mock"  # or "openai"
default_model = "gpt-4o"
timeout_secs = 120

[backend.openai]
api_base = "https://api.openai.com/v1"
pool_max_idle_per_host = 100

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
SocketHandoffAllowedPrefix /var/run/
SocketHandoffConnectTimeoutMs 500
```

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

// 3. Set handoff headers
header('X-Socket-Handoff: /var/run/streaming-daemon-rs.sock');
header('X-Handoff-Data: ' . $data);

// 4. Exit - mod_socket_handoff takes over
exit;
```

## Backends

### Mock Backend

Simulates streaming responses for testing. Streams a word at a time with configurable delays.

```bash
./streaming-daemon-rs --backend mock
```

### OpenAI Backend

Streams responses from OpenAI's chat completions API.

```bash
export OPENAI_API_KEY=sk-...
./streaming-daemon-rs --backend openai
```

The handoff data can specify:
- `prompt` - The user message
- `model` - Model to use (default: gpt-4o)
- `system` - System prompt
- `max_tokens` - Maximum response tokens
- `temperature` - Sampling temperature

## Signals

- **SIGTERM/SIGINT** - Initiates graceful shutdown, stops accepting new connections, waits for active streams to complete
- **SIGHUP** - Logs current connection count (status report)

## Metrics

When metrics are enabled, a Prometheus endpoint is available:

```bash
curl http://localhost:9090/metrics
```

Available metrics:
- `daemon_connections_active` - Current active connections
- `daemon_connections_total` - Total connections handled
- `daemon_handoffs_received` - Successful fd handoffs
- `daemon_handoffs_failed` - Failed handoffs
- `daemon_stream_bytes_sent` - Total bytes streamed
- `daemon_backend_requests` - Backend API requests
- `daemon_backend_errors` - Backend errors

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
- **File descriptors**: Ensure `ulimit -n` is high enough. The systemd service sets `LimitNOFILE=200000`.
- **HTTP connection pooling**: The OpenAI backend maintains a connection pool to reduce latency.
- **Write timeouts**: Per-write timeouts prevent slow clients from blocking resources indefinitely.

## Troubleshooting

### Permission Denied on Socket

```bash
# Check socket permissions
ls -la /var/run/streaming-daemon-rs.sock

# Ensure www-data can access
sudo chown www-data:www-data /var/run/streaming-daemon-rs.sock
sudo chmod 666 /var/run/streaming-daemon-rs.sock
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
