# Mock LLM API Server

High-performance mock OpenAI-compatible streaming API server for benchmarking streaming daemons.

## Purpose

This server simulates an OpenAI Chat Completions streaming endpoint. It's designed to be fast enough that it never becomes a bottleneck during daemon benchmarks.

Key characteristics:
- **Zero allocation per request** - All SSE chunks are pre-computed at startup
- **Request body ignored** - No JSON parsing overhead
- **Configurable timing** - Simulate realistic LLM response delays

## Usage

```bash
# Build
cargo build --release

# Run with defaults (18 chunks, 50ms delays)
./target/release/mock-llm-api

# Simulate slower LLM (matches Go daemon benchmark config)
./target/release/mock-llm-api --chunk-delay-ms 5625

# Fast mode for throughput testing
./target/release/mock-llm-api --chunk-delay-ms 0

# Custom configuration
./target/release/mock-llm-api \
    --listen 0.0.0.0:8080 \
    --chunk-count 50 \
    --chunk-delay-ms 100 \
    --workers 4
```

## CLI Options

```
Options:
  -l, --listen <ADDR>        Listen address [default: 127.0.0.1:8080]
  -c, --chunk-count <N>      Number of chunks to stream [default: 18]
  -d, --chunk-delay-ms <N>   Delay between chunks in ms [default: 50]
  -w, --workers <N>          Tokio worker threads, 0 = num_cpus [default: 0]
      --quiet                Minimal logging
  -h, --help                 Print help
```

## Endpoints

| Endpoint | Method | Purpose |
|----------|--------|---------|
| `/v1/chat/completions` | POST | OpenAI-compatible streaming endpoint |
| `/v1/models` | GET | Model list (health check) |
| `/health` | GET | Simple health check |

## Response Format

Matches OpenAI's streaming format:

```
HTTP/1.1 200 OK
Content-Type: text/event-stream
Cache-Control: no-cache
X-Accel-Buffering: no

data: {"id":"chatcmpl-mock","object":"chat.completion.chunk",...,"delta":{"content":"Hello"}}

data: {"id":"chatcmpl-mock","object":"chat.completion.chunk",...,"delta":{"content":" world"}}

... (more chunks with delays) ...

data: {"id":"chatcmpl-mock","object":"chat.completion.chunk",...,"finish_reason":"stop"}

data: [DONE]
```

## Integration with Benchmarks

### With Rust Streaming Daemon

```bash
# Terminal 1: Start mock API
./mock-llm-api --chunk-delay-ms 50

# Terminal 2: Configure and start Rust daemon
export DAEMON_BACKEND_PROVIDER=openai
export OPENAI_API_BASE=http://localhost:8080
./streaming-daemon-rust
```

### With Benchmark Suite

```bash
# Start mock API in background
./mock-llm-api --chunk-delay-ms 50 &

# Run benchmarks with daemons pointing to mock API
make benchmark-rust DAEMON_BACKEND=openai OPENAI_API_BASE=http://localhost:8080
```

## Performance

The server is designed for maximum throughput:

- Pre-computed byte buffers eliminate per-request allocations
- No request body parsing - just streams the response
- Multi-threaded Tokio runtime for concurrent connections
- Minimal logging in production (`--quiet`)

Typical throughput on modern hardware: 50,000+ requests/second with 0ms delays.

## Testing

```bash
# Test streaming endpoint
curl -N -X POST http://localhost:8080/v1/chat/completions

# Test health check
curl http://localhost:8080/health

# Test models endpoint
curl http://localhost:8080/v1/models
```
