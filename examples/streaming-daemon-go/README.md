# streaming-daemon-go

Production-ready Go daemon for receiving Apache socket handoffs and streaming responses.

## Overview

This daemon receives client connections from Apache via `mod_socket_handoff` using Unix domain socket fd passing (`SCM_RIGHTS`). Once the connection is handed off, the daemon streams responses directly to the client while the Apache worker is freed immediately.

```
Client ──TCP──> Apache ──Unix Socket──> streaming-daemon-go
                  │         (fd pass)           │
                  │                             │
            Worker freed              Streams SSE to client
```

## Features

- **Goroutine-per-connection** - Lightweight concurrency for high throughput
- **SCM_RIGHTS fd receiving** - Portable buffer sizing with `syscall.CmsgSpace`
- **Connection limiting** - Configurable max concurrent connections (default: 1000)
- **Graceful shutdown** - Drains active connections on SIGTERM/SIGINT
- **Per-write timeouts** - Prevents hung connections from blocking resources
- **Prometheus metrics** - Built-in metrics endpoint for monitoring
- **Panic recovery** - Isolated panics don't crash the daemon

## Requirements

- Go 1.18+
- Linux (for SCM_RIGHTS support)
- Apache with `mod_socket_handoff` enabled

## Building

```bash
cd examples/streaming-daemon-go

# Build
go build -o streaming-daemon .

# Build optimized
go build -ldflags="-s -w" -o streaming-daemon .
```

## Usage

```bash
# Run with defaults
sudo ./streaming-daemon

# Custom metrics port
sudo ./streaming-daemon -metrics-addr=:9091

# Disable metrics
sudo ./streaming-daemon -metrics-addr=""

# Show help
./streaming-daemon -h
```

### Command-Line Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-metrics-addr` | `127.0.0.1:9090` | Address for Prometheus metrics server (empty to disable) |
| `-socket-mode` | `0660` | Permission mode for Unix socket |

### Signals

- **SIGTERM/SIGINT** - Initiates graceful shutdown, stops accepting new connections, waits for active streams to complete (up to 2 minutes)
- **SIGHUP** - Logs current active connection count

## Configuration

Configuration is via constants in the source code. Edit and rebuild to customize:

```go
const (
    DaemonSocket       = "/var/run/streaming-daemon.sock"
    HandoffTimeout     = 5 * time.Second   // Max time to receive fd from Apache
    WriteTimeout       = 30 * time.Second  // Max time for a single write
    ShutdownTimeout    = 2 * time.Minute   // Graceful shutdown timeout
    MaxConnections     = 1000              // Max concurrent connections
    MaxHandoffDataSize = 65536             // 64KB buffer for handoff JSON
)
```

## Apache Configuration

Ensure `mod_socket_handoff` is enabled and configured:

```apache
SocketHandoffEnabled On
SocketHandoffAllowedPrefix /var/run/
SocketHandoffConnectTimeoutMs 500
```

## PHP Integration

Example PHP endpoint that hands off to the daemon:

```php
<?php
// Authenticate the request
session_start();
if (!isset($_SESSION['user_id'])) {
    http_response_code(401);
    exit('Unauthorized');
}

// Prepare handoff data
$data = json_encode([
    'prompt' => $_POST['prompt'] ?? 'Hello',
    'user_id' => $_SESSION['user_id'],
    'model' => 'gpt-4o',
]);

// Set handoff headers
header('X-Socket-Handoff: /var/run/streaming-daemon.sock');
header('X-Handoff-Data: ' . $data);

// Exit - mod_socket_handoff takes over
exit;
```

## Prometheus Metrics

When metrics are enabled (default), a Prometheus endpoint is available:

```bash
curl http://localhost:9090/metrics
```

### Available Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `daemon_active_connections` | Gauge | - | Number of currently active client connections |
| `daemon_connections_total` | Counter | - | Total number of connections accepted |
| `daemon_connections_rejected_total` | Counter | - | Total connections rejected due to capacity limits |
| `daemon_handoffs_total` | Counter | - | Total number of successful fd handoffs received |
| `daemon_handoff_errors_total` | Counter | `reason` | Total handoff receive errors by type |
| `daemon_stream_errors_total` | Counter | `reason` | Total stream errors by type |
| `daemon_bytes_sent_total` | Counter | - | Total bytes sent to clients |
| `daemon_handoff_duration_seconds` | Histogram | - | Time spent receiving fd handoff from Apache |
| `daemon_stream_duration_seconds` | Histogram | - | Total time spent streaming response to client |

### Error Reason Labels

The `reason` label on error counters can have these values:

| Value | Description |
|-------|-------------|
| `timeout` | Operation timed out |
| `canceled` | Context was canceled (shutdown) |
| `network` | Generic network error |
| `client_disconnected` | Client closed connection (EPIPE, ECONNRESET) |
| `other` | Unclassified error |

### Example Queries

```promql
# Current connection count
daemon_active_connections

# Connection accept rate (per second)
rate(daemon_connections_total[1m])

# Rejection rate due to capacity
rate(daemon_connections_rejected_total[1m])

# Handoff error rate by reason
rate(daemon_handoff_errors_total[1m])

# 99th percentile handoff latency
histogram_quantile(0.99, rate(daemon_handoff_duration_seconds_bucket[5m]))

# Average bytes per stream
rate(daemon_bytes_sent_total[1m]) / rate(daemon_connections_total[1m])
```

## How It Works

1. **Apache receives HTTP request** from client
2. **PHP authenticates** and sets `X-Socket-Handoff` header
3. **mod_socket_handoff** intercepts the header in output filter
4. **Apache connects** to daemon's Unix socket
5. **Apache sends client fd** via `SCM_RIGHTS` with handoff JSON
6. **Apache swaps in dummy socket** and frees worker thread
7. **Daemon receives fd** and parses handoff JSON
8. **Daemon streams SSE** directly to client socket
9. **Daemon closes connection** when stream completes

## Customizing the Response

The demo streams a simple test message. To integrate with an LLM API, modify `streamDemoResponse()`:

```go
func streamDemoResponse(ctx context.Context, conn net.Conn, writer *bufio.Writer, handoff HandoffData) error {
    // Example: OpenAI streaming integration
    stream, err := openai.CreateChatCompletionStream(ctx, openai.ChatCompletionRequest{
        Model: handoff.Model,
        Messages: []openai.ChatCompletionMessage{
            {Role: "user", Content: handoff.Prompt},
        },
        Stream: true,
    })
    if err != nil {
        return err
    }
    defer stream.Close()

    for {
        chunk, err := stream.Recv()
        if err == io.EOF {
            break
        }
        if err != nil {
            return err
        }
        if err := sendSSE(conn, writer, chunk.Choices[0].Delta.Content); err != nil {
            return err
        }
    }

    // Send completion marker
    fmt.Fprintf(writer, "data: [DONE]\n\n")
    return writer.Flush()
}
```

## Systemd Installation

```bash
# Copy binary
sudo cp streaming-daemon /usr/local/bin/

# Create systemd service
sudo tee /etc/systemd/system/streaming-daemon.service << 'EOF'
[Unit]
Description=Streaming Daemon for Apache Socket Handoff
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/streaming-daemon
Restart=always
RestartSec=5
User=www-data
Group=www-data
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
EOF

# Enable and start
sudo systemctl daemon-reload
sudo systemctl enable streaming-daemon
sudo systemctl start streaming-daemon

# Check status
sudo systemctl status streaming-daemon
sudo journalctl -u streaming-daemon -f
```

## Troubleshooting

### Permission Denied on Socket

The daemon uses mode 0660 by default (owner + group access only). Apache must
be able to connect to the socket:

```bash
# Check socket permissions
ls -la /var/run/streaming-daemon.sock

# Option 1: Run daemon as www-data (same user as Apache)
sudo -u www-data ./streaming-daemon

# Option 2: Add www-data to daemon's group
sudo usermod -aG streaming-daemon www-data

# Option 3: For testing only (NOT for production)
./streaming-daemon -socket-mode=0666
```

### Connection Refused

```bash
# Check if daemon is running
pgrep -a streaming-daemon

# Check socket exists
ls -la /var/run/streaming-daemon.sock

# Test socket directly
echo '{"prompt":"test"}' | nc -U /var/run/streaming-daemon.sock
```

### High Rejection Rate

If `daemon_connections_rejected_total` is increasing:

```bash
# Check current connections vs limit
curl -s localhost:9090/metrics | grep daemon_active_connections

# Consider increasing MaxConnections in source and rebuilding
```

## License

Apache 2.0 - Same as mod_socket_handoff.

## See Also

- [mod_socket_handoff](../../) - The Apache module
- [streaming-daemon-rs](../streaming-daemon-rs/) - Rust implementation
- [streaming_daemon.php](../streaming_daemon.php) - PHP implementation
