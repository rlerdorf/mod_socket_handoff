# Swow Streaming Daemon

High-performance PHP streaming daemon using the Swow extension with libcurl for HTTP/2 multiplexing.

## Features

- **Swow coroutines** for concurrent connection handling
- **curl_multi HTTP/2 multiplexing** via TLS for efficient backend connections
- **Consistent p99 latency** (~2ms) from 100 to 10,000+ connections
- **Low CPU usage** (27% at 10k connections vs 84% for AMP)
- **OpenAI-compatible API** backend support

## Requirements

- PHP 8.1+
- [Swow extension](https://github.com/swow/swow)
- curl extension with HTTP/2 support (libcurl built with nghttp2)

### Installing Swow

```bash
git clone https://github.com/swow/swow.git
cd swow/ext
phpize
./configure
make -j$(nproc)
sudo make install
```

Add to php.ini (or load dynamically):
```ini
extension=swow.so
```

**Note:** Swow and Swoole extensions conflict. Only load one at a time.

## Usage

```bash
# Basic usage with mock backend
sudo php -d extension=swow.so streaming_daemon.php \
    -s /var/run/streaming-daemon.sock \
    -m 0666

# With OpenAI backend (HTTP/2 via HTTPS)
OPENAI_API_KEY=sk-xxx \
OPENAI_HTTP2_ENABLED=true \
sudo php -d extension=swow.so streaming_daemon.php \
    -s /var/run/streaming-daemon.sock \
    --backend openai \
    --openai-base https://api.openai.com/v1
```

## Options

```
-s, --socket PATH        Unix socket path (required)
-m, --mode MODE          Socket permissions in octal (default: 0660)
-d, --delay MS           Delay between SSE messages for mock backend (default: 50)
--backend TYPE           Backend provider: mock or openai (default: mock)
--openai-base URL        OpenAI API base URL
--benchmark              Enable benchmark mode (quieter output, summary on exit)
--max-connections NUM    Maximum concurrent connections (default: 50000)
```

## Environment Variables

| Variable | Description |
|----------|-------------|
| `OPENAI_API_KEY` | API key for OpenAI backend |
| `OPENAI_HTTP2_ENABLED` | Set to "true" for HTTP/2 multiplexing (requires https://) |
| `OPENAI_INSECURE_SSL` | Set to "true" to skip TLS certificate verification (testing only) |

## Architecture

The daemon uses a dedicated reader coroutine that integrates libcurl with Swow:

1. **CurlMultiHandle** class wraps `curl_multi_*` functions
2. Reader coroutine calls `curl_multi_select()` and `curl_multi_exec()` in a loop
3. HTTP/2 multiplexing enabled via `CURLPIPE_MULTIPLEX` and `CURLOPT_PIPEWAIT`
4. SSE responses parsed through `CURLOPT_WRITEFUNCTION` callbacks
5. Stream completion signaled via per-stream Swow channels

This architecture provides excellent scaling because:
- curl handles HTTP/2 connection management internally via libnghttp2
- `CURLOPT_WRITEFUNCTION` callbacks deliver data directly without channel dispatch overhead
- The polling loop in the reader coroutine yields to other coroutines efficiently

## Performance

At 10,000 concurrent connections (HTTP/2 with TLS):

| Metric | Value |
|--------|-------|
| TTFB p50 | 0.47ms |
| TTFB p99 | 2ms |
| Peak RSS | 431 MB |
| CPU | 27% |
| Per-connection memory | ~37 KB |

## HTTP/2 Multiplexing

For HTTP/2 multiplexing to work correctly with Swow's curl integration:

1. **Use HTTPS URLs** - Swow's curl hooks require TLS for HTTP/2 connection reuse
2. Set `OPENAI_HTTP2_ENABLED=true`
3. The daemon automatically configures curl handles with:
   - `CURLOPT_HTTP_VERSION` = `CURL_HTTP_VERSION_2TLS`
   - `CURLOPT_PIPEWAIT` = true
   - `CURLMOPT_PIPELINING` = `CURLPIPE_MULTIPLEX`

With HTTP/2, ~100 streams share each TCP connection, reducing connection overhead from 100k to ~1k connections for 100k concurrent streams.

## License

Apache 2.0
