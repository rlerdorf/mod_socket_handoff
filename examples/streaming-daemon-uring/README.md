# streaming-daemon-uring

A high-performance C streaming daemon using Linux io_uring for async I/O.
Receives client TCP sockets from Apache via SCM_RIGHTS and streams SSE responses.

## Features

- **io_uring** for true async I/O with minimal syscall overhead
- **Multishot accept** (Linux 5.19+) for efficient connection handling
- **Single-threaded** event loop avoiding synchronization complexity
- **Thread pool** for blocking LLM API calls (optional)
- **OpenAI backend** with libcurl for real LLM streaming (optional)
- **Dynamic memory allocation** - 64KB handoff buffers allocated on-demand
- **Connection pooling** with pre-allocated structures
- **Graceful shutdown** with connection draining
- **Benchmark mode** for performance testing

## Requirements

- Linux 5.1+ (5.19+ recommended for multishot accept)
- liburing development headers (`liburing-dev` on Debian/Ubuntu)
- GCC or Clang
- libcurl (optional, for OpenAI backend): `libcurl4-openssl-dev`

## Building

```bash
# Install dependencies (Debian/Ubuntu)
sudo apt-get install liburing-dev

# Build with mock backend (default - for testing/benchmarks)
make

# Build with OpenAI backend (requires libcurl)
sudo apt-get install libcurl4-openssl-dev
make BACKEND=openai

# Build with debug symbols
make debug

# Build with AddressSanitizer
make sanitize
```

## Usage

```bash
# Run with defaults (socket at /var/run/streaming-daemon.sock)
sudo ./streaming-daemon-uring

# Custom socket path
./streaming-daemon-uring --socket /tmp/daemon.sock

# High concurrency benchmark (mock backend)
./streaming-daemon-uring --socket /tmp/daemon.sock --max-connections 50000 --benchmark --delay 0

# Enable thread pool for LLM calls
./streaming-daemon-uring --thread-pool --pool-size 16

# OpenAI backend (requires BACKEND=openai build)
export OPENAI_API_KEY="sk-..."
./streaming-daemon-uring --thread-pool
```

## Command Line Options

| Option | Default | Description |
|--------|---------|-------------|
| `--socket PATH` | /var/run/streaming-daemon.sock | Unix socket path |
| `--socket-mode MODE` | 0660 | Socket permissions (octal) |
| `--max-connections N` | 10000 | Maximum concurrent connections |
| `--delay MS` | 50 | Delay between SSE messages (mock backend) |
| `--thread-pool` | off | Enable thread pool for LLM API calls |
| `--pool-size N` | 8 | Number of worker threads (1-64) |
| `--sqpoll` | off | Enable SQPOLL kernel polling (requires root) |
| `--metrics PORT` | off | Enable Prometheus metrics on PORT |
| `--config FILE` | | Load configuration from TOML file |
| `--benchmark` | off | Enable benchmark mode (stats on exit) |
| `--help` | | Show help |
| `--version` | | Show version |

## Configuration File

The daemon can be configured via a TOML file. See `streaming-daemon.toml.example`.

Default config file locations (searched in order):
1. `./streaming-daemon.toml`
2. `/etc/streaming-daemon/config.toml`
3. `/etc/streaming-daemon.toml`

CLI arguments override config file settings.

## Prometheus Metrics

When `--metrics PORT` is specified, the daemon exposes metrics at `http://localhost:PORT/metrics`:

```
streaming_daemon_connections_active    - Current active connections
streaming_daemon_connections_peak      - Peak concurrent connections
streaming_daemon_connections_max       - Maximum allowed connections
streaming_daemon_requests_total        - Total requests by status
streaming_daemon_bytes_total           - Total bytes sent
streaming_daemon_info                  - Daemon version and config
```

## Environment Variables (OpenAI Backend)

| Variable | Default | Description |
|----------|---------|-------------|
| `OPENAI_API_KEY` | (required) | API key for authentication |
| `OPENAI_API_BASE` | https://api.openai.com/v1 | Base URL for API |
| `OPENAI_MODEL` | gpt-4o-mini | Default model to use |
| `MOCK_LATENCY_MS` | 0 | Simulated latency (mock backend only) |

## Architecture

### Without Thread Pool (Mock Backend)

```
                              +------------------+
                              |   io_uring SQ    |
                              +--------+---------+
                                       |
    +---------+    +---------+    +----v----+    +---------+
    | Accept  |--->| Recv FD |--->| Stream  |--->| Close   |
    +---------+    +---------+    +---------+    +---------+
         ^              |              |              |
         |              v              v              v
         |         +----+----+    +----+----+    +----+----+
         |         | Parse   |    | Write   |    | Free    |
         |         | JSON    |    | SSE     |    | conn    |
         |         +---------+    +---------+    +---------+
         |                                            |
         +--------------------------------------------+
                    Connection Pool
```

### With Thread Pool (LLM Backend)

```
                              +------------------+
                              |   io_uring SQ    |
                              +--------+---------+
                                       |
    +---------+    +---------+    +----v----+    +---------+    +---------+
    | Accept  |--->| Recv FD |--->| Parse   |--->| LLM API |--->| Stream  |
    +---------+    +---------+    +---------+    +---------+    +---------+
         ^                             |              |              |
         |                             v              v              v
         |                        +----+----+   +-----+-----+   +----+----+
         |                        | Queue   |   | Worker    |   | Write   |
         |                        | Work    |   | Thread    |   | SSE     |
         |                        +---------+   +-----------+   +---------+
         |                                            |              |
         +--------------------------------------------+--------------+
                    Connection Pool            eventfd signal
```

### Connection States

1. **ACCEPTING** - Waiting for connection from Apache
2. **RECEIVING_FD** - Receiving client socket via SCM_RIGHTS
3. **PARSING** - Parsing JSON handoff data
4. **LLM_QUEUED** - Waiting in thread pool queue (thread pool mode)
5. **LLM_PENDING** - Being processed by worker thread (thread pool mode)
6. **STREAMING** - Sending SSE messages to client
7. **CLOSING** - Sending [DONE] marker

### io_uring Operations

- **Multishot Accept** - Single SQE generates multiple CQEs for new connections
- **recvmsg** - Receives fd and handoff data in one operation
- **write** - Sends SSE chunks with linked timeouts
- **link_timeout** - Cancels stuck operations

## Integration with mod_socket_handoff

1. Configure Apache with mod_socket_handoff
2. Start this daemon:
   ```bash
   sudo ./streaming-daemon-uring
   ```
3. Create a PHP endpoint that sets handoff headers:
   ```php
   <?php
   header('X-Socket-Handoff: /var/run/streaming-daemon.sock');
   header('X-Handoff-Data: ' . json_encode([
       'user_id' => 123,
       'prompt' => 'Hello world'
   ]));
   exit;
   ```

## Performance

Expected performance characteristics:

| Metric | Value |
|--------|-------|
| Baseline memory | ~3 MB |
| Memory per active connection | ~68 KB (64KB handoff + 4KB write buffer) |
| Max connections | 100,000+ |
| Syscalls per message | ~1 (batched) |
| Event loop | Single-threaded |
| LLM workers | Configurable (default 8 threads) |

Memory breakdown per connection:
- 64 KB handoff buffer (allocated on-demand, supports large prompts)
- 4 KB write buffer (allocated on-demand)
- 256 bytes control message buffer
- ~200 bytes connection struct

Memory is allocated dynamically - idle connections use minimal memory.
For 10K concurrent active connections: ~700 MB RSS.

## Kernel Compatibility

| Kernel | Support Level |
|--------|---------------|
| 5.1+ | Basic io_uring |
| 5.19+ | Multishot accept (recommended) |
| 6.0+ | Best stability |

Check your kernel support:
```bash
make check-kernel
```

## Future Enhancements

- Fixed buffer rings for zero-copy writes
- Rate limiting per user
- Connection draining with configurable timeout
- Health check endpoint
- TLS termination support

## Files

| File | Purpose |
|------|---------|
| daemon.c | Entry point, event loop, signal handling |
| ring.c/h | io_uring wrapper functions |
| server.c/h | Unix socket listener |
| handoff.c/h | SCM_RIGHTS fd receiving |
| connection.c/h | Connection state machine and pool |
| streaming.c/h | SSE message formatting |
| pool.c/h | Thread pool for LLM API calls |
| metrics.c/h | Prometheus metrics endpoint |
| config.c/h | TOML configuration file parser |
| backend.h | Backend interface |
| backend_mock.c | Mock backend for testing/benchmarks |
| backend_openai.c | OpenAI API backend with libcurl |
| json.c/h | JSON parsing helpers |
| jsmn.h | Vendored JSON tokenizer |
| streaming-daemon.toml.example | Example configuration file |

## License

Apache 2.0
