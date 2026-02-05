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

- **Backend plugin architecture** - Extensible backends (langgraph, mock, openai, typing) with Caddy-style `init()` registration
- **YAML configuration** - Optional config file with flag overrides
- **Goroutine-per-connection** - Lightweight concurrency for high throughput
- **SCM_RIGHTS fd receiving** - Portable buffer sizing with `syscall.CmsgSpace`
- **Connection limiting** - Configurable max concurrent connections (default: 50,000)
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

## Quick Start

```bash
# Run with defaults (mock backend)
sudo ./streaming-daemon

# Run with OpenAI backend
OPENAI_API_KEY=sk-... sudo ./streaming-daemon -backend openai

# Run with LangGraph backend
LANGGRAPH_API_KEY=lgk_... sudo ./streaming-daemon -backend langgraph

# Run with config file
sudo ./streaming-daemon -config config/local.yaml

# Show help
./streaming-daemon -help
```

## Backends

The daemon supports multiple streaming backends via a plugin architecture. Backends are registered at startup and selected via the `-backend` flag or config file.

### Available Backends

| Backend | Description | Use Case |
|---------|-------------|----------|
| `langgraph` | LangGraph Platform API | Stateful agents with conversation threads |
| `mock` | Fixed demo messages with configurable delay | Testing, benchmarking |
| `openai` | OpenAI-compatible streaming API | GPT-4, Groq, Ollama, any OpenAI-compatible API |
| `typing` | Character-by-character typewriter effect | Demos, fortune integration |

### Mock Backend

Streams 18 fixed messages with configurable delay. Useful for testing and benchmarking without external dependencies.

```bash
# Default 50ms delay
./streaming-daemon -backend mock

# Slower for visual testing
./streaming-daemon -backend mock -message-delay 200ms

# 30-second streams for capacity testing
./streaming-daemon -backend mock -message-delay 1667ms
```

### OpenAI Backend

Streams responses from any OpenAI-compatible API (OpenAI, Groq, Ollama, etc.).

```bash
# OpenAI
OPENAI_API_KEY=sk-... ./streaming-daemon -backend openai

# Groq
OPENAI_API_KEY=gsk-... ./streaming-daemon -backend openai \
    -openai-base https://api.groq.com/openai/v1

# Local Ollama
./streaming-daemon -backend openai \
    -openai-base http://localhost:11434/v1
```

Configuration options:

| Flag | Env Var | Config Key | Description |
|------|---------|------------|-------------|
| `-openai-base` | `OPENAI_API_BASE` | `backend.openai.api_base` | API base URL |
| - | `OPENAI_API_KEY` | `backend.openai.api_key` | API key |
| `-openai-socket` | `OPENAI_API_SOCKET` | `backend.openai.api_socket` | Unix socket for API |
| `-http2-enabled` | `OPENAI_HTTP2_ENABLED` | `backend.openai.http2_enabled` | Enable HTTP/2 |

### LangGraph Backend

Streams responses from the LangGraph Platform API. Supports both stateless runs and stateful conversation threads.

```bash
# Basic usage
LANGGRAPH_API_KEY=lgk_... ./streaming-daemon -backend langgraph

# Custom assistant
LANGGRAPH_API_KEY=lgk_... ./streaming-daemon -backend langgraph \
    -langgraph-assistant my-agent

# Self-hosted LangGraph
LANGGRAPH_API_KEY=lgk_... ./streaming-daemon -backend langgraph \
    -langgraph-base http://localhost:8000
```

Configuration options:

| Flag | Env Var | Config Key | Description |
|------|---------|------------|-------------|
| `-langgraph-base` | `LANGGRAPH_API_BASE` | `backend.langgraph.api_base` | API base URL |
| - | `LANGGRAPH_API_KEY` | `backend.langgraph.api_key` | API key |
| `-langgraph-socket` | `LANGGRAPH_API_SOCKET` | `backend.langgraph.api_socket` | Unix socket for API |
| `-langgraph-assistant` | `LANGGRAPH_ASSISTANT_ID` | `backend.langgraph.assistant_id` | Default assistant ID |
| - | `LANGGRAPH_STREAM_MODE` | `backend.langgraph.stream_mode` | Stream mode (default: messages-tuple) |
| - | `LANGGRAPH_HTTP2_ENABLED` | `backend.langgraph.http2_enabled` | Enable HTTP/2 |
| - | `LANGGRAPH_INSECURE_SSL` | `backend.langgraph.insecure_ssl` | Skip TLS verification |

LangGraph-specific handoff data fields:

| Field | Description |
|-------|-------------|
| `thread_id` | Thread ID for stateful conversations (uses `/threads/{id}/runs/stream`) |
| `assistant_id` | Override the default assistant ID |
| `langgraph_input` | Custom input fields passed to the agent (e.g., `seller_id`, `shop_id`) |

### Typing Backend

Streams characters one at a time with realistic typing delays. Uses `/usr/games/fortune` for dynamic content when the prompt isn't recognized.

```bash
./streaming-daemon -backend typing
```

### Adding New Backends

To add a new backend (e.g., Anthropic):

1. Create `backends/anthropic.go`
2. Implement the `Backend` interface:
   ```go
   type Backend interface {
       Name() string
       Description() string
       Init(cfg *config.BackendConfig) error
       Stream(ctx context.Context, conn net.Conn, handoff HandoffData) (int64, error)
   }
   ```
3. Register in `init()`:
   ```go
   func init() {
       Register(&Anthropic{})
   }
   ```
4. Add config struct to `config/config.go` if needed

No changes to the main daemon required.

## Configuration

The daemon can be configured via:
1. **YAML config file** - Recommended for production
2. **Command-line flags** - Override config file values
3. **Environment variables** - For secrets (API keys)

Priority: flags > env vars > config file > defaults

### Config File

Copy `config/example.yaml` to `config/local.yaml` and customize:

```yaml
server:
  socket_path: /var/run/streaming-daemon.sock
  socket_mode: 0660
  max_connections: 50000
  # pprof_addr: localhost:6060  # Uncomment to enable profiling

backend:
  provider: openai
  default_model: gpt-4o-mini

  openai:
    # api_key: sk-...  # Better to use OPENAI_API_KEY env var
    api_base: https://api.openai.com/v1
    http2_enabled: true

  langgraph:
    # api_key: lgk_...  # Better to use LANGGRAPH_API_KEY env var
    api_base: https://api.langchain.com/v1
    assistant_id: agent
    stream_mode: messages-tuple
    http2_enabled: true

  mock:
    message_delay_ms: 50

metrics:
  enabled: true
  listen_addr: 127.0.0.1:9090

logging:
  level: info
  format: text
```

Run with config file:

```bash
./streaming-daemon -config config/local.yaml
```

Override specific values:

```bash
./streaming-daemon -config config/local.yaml -backend mock -socket /tmp/test.sock
```

### Command-Line Flags

Flags override config file values when explicitly set. The "Config Default" column shows the default value used when neither flag nor config file specifies a value.

| Flag | Config Default | Description |
|------|----------------|-------------|
| `-config` | - | Path to YAML config file |
| `-socket` | `/var/run/streaming-daemon.sock` | Unix socket path |
| `-socket-mode` | `0660` | Socket permission mode |
| `-max-connections` | `50000` | Max concurrent connections |
| `-backend` | `mock` | Backend: langgraph, mock, openai, typing |
| `-metrics-addr` | `127.0.0.1:9090` | Metrics server address (empty to disable) |
| `-pprof-addr` | - | pprof server address (empty to disable) |
| `-benchmark` | `false` | Enable benchmark mode |
| `-message-delay` | `50ms` | Delay between mock messages |
| `-openai-base` | - | OpenAI API base URL |
| `-openai-socket` | - | Unix socket for OpenAI API |
| `-http2-enabled` | `true` | Enable HTTP/2 for upstream |
| `-langgraph-base` | - | LangGraph API base URL |
| `-langgraph-socket` | - | Unix socket for LangGraph API |
| `-langgraph-assistant` | - | LangGraph assistant ID |

### Signals

- **SIGTERM/SIGINT** - Graceful shutdown, waits for active streams (up to 2 minutes)
- **SIGHUP** - Logs current active connection count

## File Structure

```
streaming-daemon-go/
├── streaming_daemon.go      # Main daemon (connection handling, metrics)
├── streaming_daemon_test.go # Tests
├── go.mod
├── go.sum
├── backends/
│   ├── backend.go           # Backend interface + registry
│   ├── langgraph.go         # LangGraph Platform API backend
│   ├── langgraph_test.go    # LangGraph backend tests
│   ├── mock.go              # Mock demo backend
│   ├── openai.go            # OpenAI streaming backend
│   ├── typing.go            # Typewriter effect backend
│   ├── sse.go               # Shared SSE utilities
│   └── metrics.go           # Backend metrics helpers
└── config/
    ├── config.go            # Config structs and loader
    └── example.yaml         # Example configuration
```

## Apache Configuration

Ensure `mod_socket_handoff` is enabled and configured:

```apache
SocketHandoffEnabled On
SocketHandoffAllowedPrefix /var/run/
SocketHandoffConnectTimeoutMs 100
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
| `canceled` | Context was canceled |
| `network` | Network-related error |
| `other` | Other/unknown error type |

### Example Queries

```promql
# Current connection count
daemon_active_connections

# Connection accept rate (per second)
rate(daemon_connections_total[1m])

# Rejection rate due to capacity
rate(daemon_connections_rejected_total[1m])

# 99th percentile stream duration
histogram_quantile(0.99, rate(daemon_stream_duration_seconds_bucket[5m]))

# Bytes per second throughput
rate(daemon_bytes_sent_total[1m])

# Chunks per second
rate(daemon_chunks_sent_total[1m])

# Backend request rate by backend type
rate(daemon_backend_requests_total[1m])

# Backend error rate
rate(daemon_backend_errors_total[1m])

# Backend TTFB 95th percentile
histogram_quantile(0.95, rate(daemon_backend_ttfb_seconds_bucket[5m]))

# Client disconnect rate
rate(daemon_stream_errors_total{reason="client_disconnected"}[1m])
```

## Benchmark Mode

For high-throughput testing, use benchmark mode to skip Prometheus updates:

```bash
./streaming-daemon -benchmark -backend mock -message-delay 100ms
```

Benchmark mode:
- Skips all Prometheus counter/gauge updates
- Uses atomic counters for minimal overhead
- Prints summary on shutdown (peak streams, total completed, bytes sent)

## Systemd Installation

```bash
# Copy binary
sudo cp streaming-daemon /usr/local/bin/

# Copy config
sudo mkdir -p /etc/streaming-daemon
sudo cp config/example.yaml /etc/streaming-daemon/config.yaml
# Edit config as needed

# Create systemd service
sudo tee /etc/systemd/system/streaming-daemon.service << 'EOF'
[Unit]
Description=Streaming Daemon for Apache Socket Handoff
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/streaming-daemon -config /etc/streaming-daemon/config.yaml
Restart=always
RestartSec=5
User=www-data
Group=www-data
LimitNOFILE=65535
Environment=OPENAI_API_KEY=sk-your-key-here

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

The daemon uses mode 0660 by default (owner + group access only):

```bash
# Check socket permissions
ls -la /var/run/streaming-daemon.sock

# Option 1: Run daemon as www-data (same user as Apache)
sudo -u www-data ./streaming-daemon

# Option 2: Add www-data to daemon's group
sudo usermod -aG streaming-daemon www-data

# Option 3: For testing only (NOT for production)
./streaming-daemon -socket-mode 0666
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

### OpenAI API Errors

```bash
# Check API key is set
echo $OPENAI_API_KEY

# Test with curl
curl https://api.openai.com/v1/models \
    -H "Authorization: Bearer $OPENAI_API_KEY"

# Check daemon logs
journalctl -u streaming-daemon -f
```

### High Memory Usage

If memory grows over time:

```bash
# Enable pprof
./streaming-daemon -pprof-addr localhost:6060

# Analyze heap
go tool pprof http://localhost:6060/debug/pprof/heap
```

## License

Apache 2.0 - Same as mod_socket_handoff.

## See Also

- [mod_socket_handoff](../../) - The Apache module
- [streaming-daemon-rs](../streaming-daemon-rs/) - Rust implementation
- [streaming_daemon.php](../streaming_daemon.php) - PHP implementation
