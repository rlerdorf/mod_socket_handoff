# Repository Custom Instructions for GitHub Copilot

This repository contains `mod_socket_handoff`, an Apache module for high-performance socket handoff to streaming daemons. The code is designed for **high-concurrency production deployments** handling 50,000+ concurrent connections.

## Code Review Priority: Production Concurrency Safety

When reviewing or suggesting changes, prioritize these concerns in order:

### 1. Resource Lifecycle Management

- **File descriptors must be closed exactly once.** Double-close bugs cause crashes or data corruption in production. Track ownership transfers explicitly (e.g., `socket.detach()` in Python, `os.NewFile()` taking ownership in Go).
- **Memory must be freed in all code paths.** Check error handling paths for resource leaks. Use RAII patterns where available.
- **Socket buffers must be bounded.** Unbounded buffers lead to OOM under load.

### 2. Error Handling for High-Concurrency

- **Never ignore errors silently.** Log or propagate all errors appropriately.
- **Handle partial reads/writes.** Streaming connections may return partial data.
- **Distinguish transient vs permanent errors.** Retry EAGAIN/EWOULDBLOCK; don't retry EPIPE.
- **Timeouts are mandatory.** Every blocking operation needs a timeout to prevent worker starvation.

### 3. Concurrency Safety

- **Avoid shared mutable state** where possible. When necessary, use appropriate synchronization.
- **Check for race conditions** in connection lifecycle (accept, handoff, close).
- **Verify atomic operations** are used correctly for counters and stats.

### 4. Performance Under Load

- **Minimize allocations per request.** Pre-allocate where possible.
- **Avoid blocking operations** in async/event-loop code paths.
- **Watch for O(n) operations** where n is connection count.
- **HTTP/2 multiplexing** should share connections efficiently (100+ streams per connection).

### 5. Security

- **Validate all input from sockets.** Assume malicious clients.
- **Path traversal prevention** for Unix socket paths.
- **TLS certificate validation** must be explicit (insecure mode only for testing).

## Language-Specific Guidance

### C (Apache Module, io_uring Daemon)

- Check all `malloc`/`calloc` return values
- Use `poll()` with timeouts, never blocking `read()`/`write()`
- Validate buffer sizes before `memcpy()`
- Prefer stack allocation for fixed-size buffers

### Rust (Streaming Daemon, Mock API)

- Prefer `?` operator for error propagation
- Use `Arc` for shared state across async tasks
- Avoid `.unwrap()` in production code paths
- Check for potential panics in `expect()` calls

### Go (Streaming Daemon, Load Generator)

- Always check error returns
- Use `defer` for cleanup, but verify order with multiple defers
- Context cancellation for timeouts
- `sync.Pool` for frequently allocated objects

### PHP (AMPHP, Swoole, Swow Daemons)

- Use strict_types declaration
- Handle Fiber/Coroutine cancellation
- Verify socket type conversions (`socket_import_stream()`)
- Check return values of socket functions

### Python (asyncio Daemon)

- Use `socket.detach()` when transferring fd ownership to asyncio
- Handle `asyncio.CancelledError` for graceful shutdown
- Set socket to non-blocking before async operations
- Use `ssl_context` options explicitly for TLS

## Testing Expectations

- **Unit tests** for parsing and utility functions
- **Integration tests** covering the full socket handoff flow
- **Benchmarks** for any changes affecting hot paths
- Test with **50k connections** before claiming production-ready
