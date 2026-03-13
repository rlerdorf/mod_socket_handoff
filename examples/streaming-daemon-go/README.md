# streaming-daemon-go

Production-ready Go daemon for receiving Apache socket handoffs and streaming responses.

## Overview

This daemon receives client connections from Apache via `mod_socket_handoff` using Unix domain socket fd passing (`SCM_RIGHTS`). Once the connection is handed off, the daemon streams responses from the LLM API directly to the client while the Apache worker is freed immediately.

```
Client ──TCP──> Apache ──Unix Socket──> streaming-daemon-go ──HTTP/2──> LLM API
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

- Go 1.25.7+ (automatically downloaded if missing — see below)
- Linux (for SCM_RIGHTS support)
- Apache with `mod_socket_handoff` enabled

## Building

```bash
cd examples/streaming-daemon-go

# Build (downloads Go toolchain if needed)
make

# Run tests
make test

# Format and vet
make fmt
make vet

# Clean build artifacts
make clean

# Rebuild from scratch (e.g. after code changes). Restart the service so the new binary is used.
make clean && make
# If running as a systemd service:
#   sudo systemctl restart streaming-daemon

# Remove downloaded Go toolchain
make clean-go

# Build with Profile-Guided Optimization (after collecting a profile)
make build-pgo

# Print instructions for collecting a CPU profile
make profile
```

The Makefile uses `ensure-go.sh` to verify that Go >= 1.25.7 is available.
If the system Go is missing or too old, the script automatically downloads
the latest Go from go.dev into a local `.goroot/` directory with SHA256
verification. Set `MIN_GO_VERSION` to override the minimum:

```bash
make MIN_GO_VERSION=1.26.0
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
| - | `OPENAI_INSECURE_SSL` | `backend.openai.insecure_ssl` | Skip TLS verification |

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
| `stream_mode` | Stream modes to use (e.g., `["messages"]`, `["events"]`); defaults to configured `LANGGRAPH_STREAM_MODE` |
| `langgraph_input` | Custom input fields passed to the agent (e.g., `seller_id`, `shop_id`) |
| `attachments` | Map of ref names to file paths (relative to `data_dir`) for multimodal requests — see [Attachments](#attachments-multimodal-requests) |
| `attachment_types` | Optional map of ref names to MIME types (overrides extension-based detection) |
| `images` | Array of `{"base64": "...", "mime_type": "..."}` for inline images already in memory |
| `image_base64` | *(deprecated)* Single inline base64 image — use `images` array instead |
| `image_mime_type` | *(deprecated)* MIME type for `image_base64` — defaults to `image/jpeg` |
| `image_path` | *(deprecated)* Single image file path — use `attachments` instead |
| `image_paths` | *(deprecated)* Array of image file paths — use `attachments` instead |

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
  max_stream_duration_ms: 300000  # Max per-stream duration (0 = no timeout, default: 5 min)
  # pprof_addr: localhost:6060  # Uncomment to enable profiling
  # mem_limit: 768MiB            # Soft memory limit (e.g. 512MiB, 1GiB); empty = no limit
  # gc_percent: 100             # GOGC value; 0 = not set (use -gc-percent=0 flag to disable GC)
  # data_dir: /run/handoff-data  # Directory for attachment files (images, text, etc.)

backend:
  provider: openai
  default_model: gpt-4o-mini

  openai:
    # api_key: sk-...  # Better to use OPENAI_API_KEY env var
    api_base: https://api.openai.com/v1
    # api_socket: /var/run/openai-proxy.sock  # Unix socket for high concurrency
    http2_enabled: true
    insecure_ssl: false

  langgraph:
    # api_key: lgk_...  # Better to use LANGGRAPH_API_KEY env var
    api_base: https://api.langchain.com/v1
    # api_socket: /var/run/langgraph-proxy.sock  # Unix socket for high concurrency
    assistant_id: agent
    stream_mode: messages-tuple
    http2_enabled: true
    insecure_ssl: false

  mock:
    message_delay_ms: 50

metrics:
  enabled: true
  listen_addr: 127.0.0.1:9090

logging:
  level: info    # debug, info, warn, error
  format: text   # text or json
```

#### Logging

The daemon uses Go's `log/slog` structured logging. The `logging` section controls the output:

- **`level`** - Minimum log level: `debug`, `info` (default), `warn`, `error`
- **`format`** - Output format: `text` (default) or `json`

JSON format is useful for log aggregation systems (ELK, Loki, Datadog):

```yaml
logging:
  level: info
  format: json
```

Source file and line information is included in all log output via `AddSource`.

#### Memory Tuning

The `mem_limit` and `gc_percent` settings configure Go's garbage collector for production workloads:

- **`mem_limit`** - Sets a soft memory limit via `debug.SetMemoryLimit()`. The GC becomes more aggressive as memory approaches this limit. Accepts human-readable sizes: `B`, `KiB`, `MiB`, `GiB`, `TiB`. Example: `768MiB`, `1GiB`.
- **`gc_percent`** - Sets `GOGC` via `debug.SetGCPercent()`. Controls how much the heap can grow before triggering GC. Default is 100 (GC when heap doubles). Set higher (e.g., 200) when using `mem_limit` to reduce GC frequency and let the memory limit drive collection instead. Note: `gc_percent: 0` in the config file is treated as "not set" (YAML zero value). To disable GC entirely (`GOGC=0`), use the `-gc-percent=0` flag.

```yaml
server:
  mem_limit: 768MiB
  gc_percent: 200
```

These can also be set via flags (`-memlimit`, `-gc-percent`) or environment variables (`GOMEMLIMIT`, `GOGC`).

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
| `-max-stream-duration-ms` | `300000` | Max per-stream duration in ms (0 = no limit) |
| `-memlimit` | - | Soft memory limit, e.g. `512MiB`, `1GiB` (equivalent to `GOMEMLIMIT`) |
| `-gc-percent` | - | GC target percentage; -1 = Go default, 0 = disable GC (equivalent to `GOGC`) |
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
│   ├── openai_test.go       # OpenAI backend tests
│   ├── typing.go            # Typewriter effect backend
│   ├── transport.go         # Shared HTTP transport builder
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

### Attachments (Multimodal Requests)

The daemon supports multimodal requests by staging files in the `data_dir` directory
(default: `/run/handoff-data`). PHP writes files to this directory, then references
them by name in the handoff JSON. The daemon reads the files, detects their types,
and deletes them after use.

Attachments use a placeholder syntax: `{ref_name}` in the prompt is replaced with
the file content at that position. Text files (`.txt`, `.md`, `.csv`, `.json`, `.xml`,
`.html`, `.yaml`, `.yml`, `.log`) are inlined as text. Image files (`.png`, `.jpg`,
`.jpeg`, `.gif`, `.webp`) are inserted as `image_url` content parts for vision models.
PDF files (`.pdf`) are supported when using a provider that accepts them — see
[PDF Attachments](#pdf-attachments) below.

```php
<?php
// 1. Stage files in the data directory
//    tempnam() guarantees unique filenames at the filesystem level.
//    getmypid() prefix is redundant under prefork MPM (one request per process)
//    but makes collisions impossible even if tempnam() races.
//    rename() adds the extension the daemon needs for MIME type detection.
$data_dir = '/run/handoff-data';
$prefix = getmypid() . '_';

$tmp = tempnam($data_dir, $prefix);
$photo = "$tmp.jpg";
rename($tmp, $photo);
move_uploaded_file($_FILES['photo']['tmp_name'], $photo);

$tmp = tempnam($data_dir, $prefix);
$notes = "$tmp.txt";
rename($tmp, $notes);
file_put_contents($notes, $_POST['notes']);

// 2. Reference files by name in the prompt using {ref_name} placeholders
$data = json_encode([
    'prompt' => 'Describe this photo: {photo}. Here are my notes: {notes}',
    'user_id' => $_SESSION['user_id'],
    'attachments' => [
        'photo' => basename($photo),   // relative to data_dir
        'notes' => basename($notes),
    ],
]);

header('X-Socket-Handoff: /var/run/streaming-daemon.sock');
header('X-Handoff-Data: ' . $data);
exit;
```

The daemon resolves the placeholders and builds the appropriate content format for
the LLM API. The text attachment is inlined at its placeholder position, and the
image becomes an `image_url` content part:

```json
{
  "assistant_id": "agent",
  "input": {
    "messages": [{
      "type": "human",
      "content": [
        {"type": "text", "text": "Describe this photo: "},
        {"type": "image_url", "image_url": {"url": "data:image/jpeg;base64,..."}},
        {"type": "text", "text": ". Here are my notes: my notes content here"}
      ]
    }]
  }
}
```

If all attachments are text (no images), the content stays a simple string — no
content array is generated. Unresolved `{ref_name}` placeholders (not in the
attachments map) are left as literal text, preventing prompt injection from
triggering arbitrary file reads.

#### Image-only example (no placeholder)

Placeholders are optional. If an attachment isn't referenced by `{ref_name}` in the
prompt, it is appended to the end of the message — the same way traditional image
uploads work:

```php
<?php
$data_dir = '/run/handoff-data';
$tmp = tempnam($data_dir, getmypid() . '_');
$photo = "$tmp.jpg";
rename($tmp, $photo);
move_uploaded_file($_FILES['photo']['tmp_name'], $photo);

$data = json_encode([
    'prompt' => 'What is in this image?',
    'user_id' => $_SESSION['user_id'],
    'attachments' => [
        'photo' => basename($photo),
    ],
]);

header('X-Socket-Handoff: /var/run/streaming-daemon.sock');
header('X-Handoff-Data: ' . $data);
exit;
```

The image is appended after the text part:

```json
{
  "input": {
    "messages": [{
      "type": "human",
      "content": [
        {"type": "text", "text": "What is in this image?"},
        {"type": "image_url", "image_url": {"url": "data:image/jpeg;base64,..."}}
      ]
    }]
  }
}
```

#### Text-only example

```php
<?php
$data_dir = '/run/handoff-data';
$tmp = tempnam($data_dir, getmypid() . '_');
$report = "$tmp.csv";
rename($tmp, $report);
file_put_contents($report, $csv_data);

$data = json_encode([
    'prompt' => 'Summarize this sales data: {report}',
    'user_id' => $_SESSION['user_id'],
    'attachments' => [
        'report' => basename($report),
    ],
]);

header('X-Socket-Handoff: /var/run/streaming-daemon.sock');
header('X-Handoff-Data: ' . $data);
exit;
```

This produces a simple string content (no array needed):

```json
{
  "input": {
    "messages": [{
      "type": "human",
      "content": "Summarize this sales data: col1,col2\n1,2\n3,4\n..."
    }]
  }
}
```

#### Explicit MIME types

When PHP already knows the MIME type (e.g., after image compression or format
conversion), use `attachment_types` to skip extension-based detection. This is
useful when the file extension doesn't match the content, or when using generic
extensions like `.dat` or `.bin`:

```php
<?php
$data_dir = '/run/handoff-data';
$tmp = tempnam($data_dir, getmypid() . '_');
// No rename needed — extension doesn't matter when attachment_types is set
file_put_contents($tmp, ImageCompressor::compressAndEncode($input->image, raw: true));
$mime_type = ImageCompressor::getOutputMimeType($input->image->mimetype);

$data = json_encode([
    'prompt' => 'What is in this image?',
    'user_id' => $_SESSION['user_id'],
    'attachments' => [
        'photo' => basename($tmp),
    ],
    'attachment_types' => [
        'photo' => $mime_type,  // e.g. "image/jpeg" — overrides extension detection
    ],
]);

header('X-Socket-Handoff: /var/run/streaming-daemon.sock');
header('X-Handoff-Data: ' . $data);
exit;
```

`attachment_types` is optional and only needed for entries where the extension is
missing or wrong. Entries not in `attachment_types` still detect from the extension
as usual. Any MIME type starting with `text/` (plus `application/json`,
`application/xml`, `application/yaml`) is treated as text and inlined; everything
else is base64-encoded as a binary attachment.

#### PDF Attachments

The `content_format` setting on a LangGraph profile controls how non-image
binary attachments (like PDFs) are serialized in the content array. The default
format uses OpenAI-compatible `image_url` parts for images, which LangChain
translates to each provider's native format (Vertex AI, Anthropic, etc.):

| Format | Images | PDFs | Unsupported types |
|--------|--------|------|-------------------|
| `openai` (default) | `image_url` | Placeholder preserved as literal `{ref}` | Warning logged |
| `anthropic` | `image_url` | `document` block with base64 source | Emitted natively |

To enable PDF support with an Anthropic-backed agent, set `content_format` on
the profile:

```yaml
backend:
  langgraph:
    profiles:
      claude-agent:
        content_format: anthropic  # Anthropic document blocks for PDFs
        assistant_id: claude-agent
```

PHP example:

```php
<?php
$data_dir = '/run/handoff-data';
$tmp = tempnam($data_dir, getmypid() . '_');
$report = "$tmp.pdf";
rename($tmp, $report);
move_uploaded_file($_FILES['report']['tmp_name'], $report);

$data = json_encode([
    'prompt' => 'Summarize this document: {report}',
    'user_id' => $_SESSION['user_id'],
    'attachments' => [
        'report' => basename($report),
    ],
]);

header('X-Socket-Handoff: /var/run/streaming-daemon.sock');
header('X-Handoff-Data: ' . $data);
exit;
```

With the default `openai` format, the `{report}` placeholder is preserved as
literal text (OpenAI does not support PDF content parts) and a warning is
logged.

With `content_format: anthropic`, the PDF produces a native document block:

```json
{"type": "document", "source": {"type": "base64", "media_type": "application/pdf", "data": "JVBERi0..."}}
```

Unreferenced images (no `{ref_name}` placeholder in the prompt) are placed
before the text part, matching the LangChain `HumanMessage` convention.

#### Inline base64 images

If the image is already in memory as base64 (e.g., from `ImageCompressor`), there
is no need to write it to disk. Use the `images` array to pass it directly through
the handoff JSON:

```php
<?php
$data = json_encode([
    'prompt' => 'What is in this image?',
    'user_id' => $_SESSION['user_id'],
    'images' => [[
        'base64' => ImageCompressor::compressAndEncode($input->image),
        'mime_type' => ImageCompressor::getOutputMimeType($input->image->mimetype),
    ]],
]);

header('X-Socket-Handoff: /var/run/streaming-daemon.sock');
header('X-Handoff-Data: ' . $data);
exit;
```

This avoids the disk round-trip entirely. The image is sent inline in the handoff
JSON and appended to the last message as an `image_url` content part. The
`mime_type` field defaults to `image/jpeg` if omitted. Use this path for small to
medium images; for large files that might exceed the 128 KB handoff buffer, stage
them on disk via `attachments` instead.

#### Limits and security

- **Path containment** — File paths are relative to `data_dir`. Path traversal
  (`../`) is blocked; the daemon returns HTTP 400.
- **Manifest-only resolution** — Only ref names declared in the `attachments` map
  are resolved. `{anything_else}` in the prompt is treated as literal text.
- **Text file size limit** — Text attachments are capped at 1 MB per file.
- **Unknown extensions** — Files with unrecognized extensions are rejected with
  an error (not silently defaulted).
- **Cleanup** — The daemon deletes each file after reading it (best-effort).

## How It Works

This section traces a single request through the system, from client to LLM API and back, with the relevant filenames and function names.

### Prerequisites

Socket handoff requires that Apache receives **plain HTTP/1.1** connections. TLS must be terminated upstream (e.g., by HAProxy, nginx, or a CDN) before reaching Apache. There are two reasons for this:

1. **TLS sockets cannot be passed** — The crypto state (session keys, sequence numbers) is internal to OpenSSL and cannot be extracted for transfer to another process.
2. **HTTP/2 multiplexed streams cannot be split** — A single HTTP/2 connection carries multiple streams that share flow control and header compression state, so individual streams cannot be isolated into separate socket fds.

### Request Flow (OpenAI Backend)

```
Client ──TLS──> Terminator ──HTTP/1.1──> Apache ──Unix Socket──> Daemon ──HTTP/2──> OpenAI
                                           │         (fd pass)       │  <---SSE---- API
                                           │                         │
                                     Worker freed          Streams SSE to client
```

1. **Apache receives HTTP/1.1 request** — The upstream TLS terminator has already decrypted the connection and forwards plain HTTP/1.1 to Apache.

2. **PHP authenticates and sets handoff headers** — The PHP handler validates the user session, prepares a JSON payload with the prompt and user context, and sets `X-Socket-Handoff` (daemon socket path) and `X-Handoff-Data` (JSON payload) response headers.

3. **mod_socket_handoff intercepts the headers** — The Apache output filter connects to the daemon's Unix socket, sends the client's TCP socket fd via `SCM_RIGHTS` along with the handoff data, then swaps in a dummy socket so Apache doesn't close the real client connection.

4. **Daemon accept loop** — `main()` in `streaming_daemon.go` runs an accept loop on the Unix socket. Each connection is dispatched to a goroutine after acquiring the connection semaphore (bounded to `max_connections`).

5. **Panic recovery** — `safeHandleConnection()` in `streaming_daemon.go` wraps the handler with `defer recover()` so that a panic in one connection doesn't crash the daemon.

6. **Receive fd handoff** — `handleConnection()` in `streaming_daemon.go` calls `receiveFd()`, which uses `ReadMsgUnix()` to receive the client socket fd via `SCM_RIGHTS`. It validates that neither `MSG_TRUNC` nor `MSG_CTRUNC` flags are set (detecting truncated data or control messages), parses the control messages, and extracts the file descriptor. Buffer pools (`handoffBufPool`, `oobBufPool`) reduce allocation pressure.

7. **Close Apache socket, create client connection** — `handleConnection()` closes the Apache Unix socket immediately after receiving the fd to avoid holding it open during long streams (10-30+ seconds). It validates the fd is `SOCK_STREAM`, converts it to a `net.Conn` via `net.FileConn()`, enables TCP keepalive, and sets up a deferred panic handler that can send HTTP 500 to the client.

8. **Parse handoff data** — The handoff JSON is unmarshalled into `HandoffData` (defined in `backends/backend.go`), which contains the prompt (or full message history), model selection, user ID, and backend-specific fields.

9. **Write SSE headers and start streaming** — `streamToClientWithBytes()` in `streaming_daemon.go` writes pre-allocated HTTP response headers (`HTTP/1.1 200 OK`, `Content-Type: text/event-stream`, etc.) directly to the client socket, then calls `activeBackend.Stream()`.

10. **Build and send API request** — `OpenAI.Stream()` in `backends/openai.go` builds the JSON request body manually using append operations (avoiding `encoding/json` reflection overhead), sets the `Authorization` header, and POSTs to the OpenAI `/chat/completions` endpoint with `"stream": true`. The HTTP client uses HTTP/2 multiplexing by default (configured via shared `NewHTTPClient()` in `backends/transport.go`).

11. **Parse upstream SSE stream** — The API response is parsed line-by-line with `bufio.Scanner`. Lines with the `data: ` prefix are extracted; `extractContentFast()` uses byte-level pattern matching on `"content":"` to extract the content string without full JSON parsing. The `unescapeJSON()` function handles JSON escape sequences including UTF-16 surrogate pairs.

12. **Forward chunks to client** — Each extracted content string is re-wrapped as an SSE event by `SendSSE()` in `backends/sse.go` (format: `data: {"content":"..."}\n\n`) and written directly to the client socket. Write deadlines are refreshed every 10 chunks to balance between timeout protection and syscall overhead.

13. **Stream completion** — When the API sends `[DONE]` or the scanner reaches EOF, a `data: [DONE]\n\n` completion marker is sent to the client. Backend duration and TTFB metrics are recorded. The client connection is closed, the goroutine exits, and the connection semaphore slot is released.

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

## Profile-Guided Optimization (PGO)

PGO uses a CPU profile collected from production to optimize the build. This typically improves throughput by 2-7%.

```bash
# 1. Run the daemon with pprof enabled
./streaming-daemon -pprof-addr localhost:6060

# 2. Collect a 30-second CPU profile under realistic load
curl -o default.pgo 'http://localhost:6060/debug/pprof/profile?seconds=30'

# 3. Rebuild with the profile
make build-pgo
```

The `default.pgo` file is gitignored. Run `make profile` for a quick reminder of these steps.

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
# Set a soft memory limit to keep GC aggressive
./streaming-daemon -memlimit 768MiB

# Enable pprof for profiling
./streaming-daemon -pprof-addr localhost:6060

# Analyze heap
go tool pprof http://localhost:6060/debug/pprof/heap
```

See the [Memory Tuning](#memory-tuning) section for `mem_limit` and `gc_percent` configuration.

## License

Apache 2.0 - Same as mod_socket_handoff.

## See Also

- [mod_socket_handoff](../../) - The Apache module
- [streaming-daemon-rs](../streaming-daemon-rs/) - Rust implementation
- [streaming_daemon.php](../streaming_daemon.php) - PHP implementation
