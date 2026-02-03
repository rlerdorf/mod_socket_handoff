# Swoole Streaming Daemon with HTTP/2 Support

A high-performance streaming daemon for `mod_socket_handoff` using Swoole coroutines
with native HTTP/2 client support for backend API requests.

## Features

- **HTTP/2 multiplexing**: Multiple streams share single TCP connection to backend
- **Coroutine-based**: Lightweight concurrency (~8KB per coroutine)
- **h2c support**: HTTP/2 cleartext with prior knowledge for http:// endpoints
- **ALPN negotiation**: HTTP/2 over TLS for https:// endpoints
- **High capacity**: 10,000+ concurrent connections

## Requirements

- PHP 8.1+
- Swoole extension with HTTP/2 support
- sockets extension

### Building Swoole with HTTP/2

```bash
# Install dependencies
sudo apt-get install libnghttp2-dev

# Install Swoole via pecl
pecl install swoole

# Or build from source with HTTP/2 support
git clone https://github.com/swoole/swoole-src.git
cd swoole-src
phpize
./configure \
    --enable-swoole \
    --enable-sockets \
    --enable-openssl \
    --with-openssl-dir=/usr \
    --with-nghttp2-dir=/usr
make -j$(nproc)
sudo make install
```

Add to php.ini:
```ini
extension=swoole.so
```

Verify HTTP/2 support:
```bash
php -r "echo 'HTTP/2: ' . (class_exists('Swoole\Coroutine\Http2\Client') ? 'Yes' : 'No') . PHP_EOL;"
```

## Usage

```bash
# Mock mode (test messages)
sudo php streaming_daemon.php -s /var/run/streaming-daemon-swoole.sock

# OpenAI backend with HTTP/2
sudo OPENAI_API_KEY=your-key php streaming_daemon.php \
    --backend openai \
    --openai-base http://api.example.com/v1

# HTTP/1.1 mode (disable HTTP/2)
sudo OPENAI_HTTP2_ENABLED=false php streaming_daemon.php \
    --backend openai \
    --openai-base http://api.example.com/v1

# Benchmark mode (quieter output)
sudo php streaming_daemon.php --benchmark
```

## Command Line Options

| Option | Description |
|--------|-------------|
| `-s, --socket PATH` | Unix socket path (default: /var/run/streaming-daemon-swoole.sock) |
| `-m, --mode MODE` | Socket permissions in octal (default: 0660) |
| `-d, --delay MS` | Delay between SSE messages in mock mode (default: 50) |
| `--backend TYPE` | Backend type: 'mock' or 'openai' |
| `--openai-base URL` | OpenAI API base URL |
| `--max-connections N` | Max concurrent connections (default: 50000) |
| `-b, --benchmark` | Enable benchmark mode |
| `-h, --help` | Show help |

## Environment Variables

| Variable | Description |
|----------|-------------|
| `OPENAI_HTTP2_ENABLED` | Set to 'false' to disable HTTP/2 |
| `OPENAI_API_BASE` | Default API base URL |
| `OPENAI_API_KEY` | API key for OpenAI backend |
| `MAX_CONNECTIONS` | Max concurrent connections |

## HTTP/2 Mode Details

### Protocol Selection

| URL Scheme | HTTP/2 Enabled | Mode |
|------------|----------------|------|
| `http://` | true | HTTP/2 h2c (prior knowledge) |
| `https://` | true | HTTP/2 with ALPN |
| any | false | HTTP/1.1 |

### HTTP/2 Multiplexing Benefits

With HTTP/1.1, each concurrent API request requires a separate TCP connection.
At 100,000 concurrent streams, this means 100,000 TCP connections to the backend.

With HTTP/2 multiplexing, approximately 100 streams share each TCP connection.
This reduces backend connections from 100,000 to ~1,000, significantly reducing:
- Ephemeral port exhaustion
- TCP connection overhead
- Backend server load

## Benchmarking

```bash
cd /path/to/mod_socket_handoff/benchmarks

# Test Swoole with HTTP/2
sudo ./run_capacity_benchmark.sh swoole-http2

# Compare with other implementations
sudo ./run_capacity_benchmark.sh swoole swoole-http2 go-http2 rust-http2
```

## Architecture

```
                    ┌─────────────────────────────────────────┐
                    │         Swoole Daemon                    │
                    │                                          │
Client ──TCP──> Apache ──Unix Socket──> Coroutine ──HTTP/2──> Backend API
                    │                     │                    │
                    │                     └── Multiple streams │
                    │                         share single     │
                    │                         TCP connection   │
                    └─────────────────────────────────────────┘
```

## Comparison with AMPHP Daemon

| Feature | AMPHP Daemon | Swoole Daemon |
|---------|--------------|---------------|
| Concurrency model | Fibers | Coroutines |
| HTTP client | HTTP/1.1 only | HTTP/1.1 + HTTP/2 |
| Multiplexing | No | Yes (HTTP/2) |
| Memory per connection | ~1-5KB | ~8KB |
| Dependencies | Composer packages | Extension only |

The AMPHP daemon remains the default for HTTP/1.1 workloads. Use the Swoole daemon
when HTTP/2 multiplexing is needed for high-concurrency backend connections.
