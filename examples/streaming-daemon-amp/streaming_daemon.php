#!/usr/bin/env php
<?php
/**
 * Async streaming daemon for mod_socket_handoff using AMPHP.
 *
 * Uses AMPHP's event loop (Revolt) with PHP 8.1+ fibers for concurrent
 * connection handling. Similar to Python asyncio or Node.js event loop.
 *
 * Performance characteristics:
 * - Single process, event-driven
 * - One fiber per connection (lightweight, ~1-5 KB each)
 * - Expected: 10,000+ concurrent connections
 * - Non-blocking I/O for all operations
 *
 * Usage:
 *   sudo php streaming_daemon.php [-s /var/run/streaming-daemon.sock]
 *
 * Requirements:
 *   - PHP 8.1+ with sockets extension
 *   - amphp/amp, amphp/socket, amphp/byte-stream (via composer)
 */

declare(strict_types=1);

// Disable memory limit to match Go/Rust/Python behavior
ini_set('memory_limit', '-1');

// Note: fiber.stack_size is set in php.ini (64k) - cannot be changed at runtime
// zend.max_allowed_stack_size is also set in php.ini (-1 to disable limit)

require __DIR__ . '/vendor/autoload.php';

use Amp\DeferredCancellation;
use Amp\Socket\ConnectContext;
use Amp\Http\Client\HttpClientBuilder;
use Amp\Http\Client\HttpClient;
use Amp\Http\Client\Request;
use Amp\Http\Client\Connection\DefaultConnectionFactory;
use Amp\Http\Client\Connection\ConnectionLimitingPool;
use Amp\Socket\ClientTlsContext;
use Revolt\EventLoop;
use function Amp\async;
use function Amp\delay;
use function Amp\Socket\connect;

const DEFAULT_SOCKET_PATH = '/var/run/streaming-daemon-amp.sock';
const DEFAULT_SOCKET_MODE = 0660;
const DEFAULT_SSE_DELAY_MS = 50;
const MAX_HANDOFF_DATA_SIZE = 65536;  // 64KB - matches Go daemon
const DEFAULT_MAX_CONNECTIONS = 50000; // Limit concurrent connections to prevent OOM

// MSG_TRUNC and MSG_CTRUNC may not be defined on all platforms
if (!defined('MSG_TRUNC')) {
    define('MSG_TRUNC', 0x20);
}
if (!defined('MSG_CTRUNC')) {
    define('MSG_CTRUNC', 0x08);
}

// Pre-defined SSE headers to avoid per-connection string building
const SSE_HEADERS = "HTTP/1.1 200 OK\r\n" .
    "Content-Type: text/event-stream\r\n" .
    "Cache-Control: no-cache\r\n" .
    "Connection: close\r\n" .
    "X-Accel-Buffering: no\r\n" .
    "\r\n";

// Static message lines (indices 3-17)
const STATIC_MESSAGES = [
    "This daemon uses AMPHP with PHP 8.1+ fibers.",
    "Each connection runs as a lightweight fiber.",
    "Expected capacity: 10,000+ concurrent connections.",
    "Memory per connection: optimized for high concurrency.",
    "The Apache worker was freed immediately after handoff.",
    "Fibers provide cooperative multitasking in PHP.",
    "The event loop schedules fibers efficiently.",
    "Non-blocking I/O allows high concurrency.",
    "AMPHP provides async primitives for PHP.",
    "This is message 13 of 18.",
    "This is message 14 of 18.",
    "This is message 15 of 18.",
    "This is message 16 of 18.",
    "This is message 17 of 18.",
    "[DONE]",
];

// Parse command line arguments
$options = getopt('s:m:d:hb', ['socket:', 'mode:', 'delay:', 'help', 'benchmark', 'backend:', 'openai-base:', 'openai-socket:', 'max-connections:']);

if (isset($options['h']) || isset($options['help'])) {
    echo <<<HELP
AMPHP Async Streaming Daemon for mod_socket_handoff

Usage: php streaming_daemon.php [options]

Options:
  -s, --socket PATH    Unix socket path (default: /var/run/streaming-daemon-amp.sock)
  -m, --mode MODE      Socket permissions in octal (default: 0660)
  -d, --delay MS       Delay between SSE messages in ms (default: 50, use 3333 for 30s streams)
  --backend TYPE       Backend type: 'mock' (demo messages) or 'openai' (HTTP streaming API)
  --openai-base URL    OpenAI API base URL (default: OPENAI_API_BASE env or http://localhost:8080)
                       Uses HTTP/2 with HTTPS URLs for stream multiplexing
  --openai-socket PATH Unix socket for OpenAI API (eliminates ephemeral port limits)
                       When set, connects via Unix socket instead of TCP
  --max-connections N  Max concurrent connections (default: 50000). When limit is reached,
                       new connections queue in kernel backlog until capacity is available.
  -b, --benchmark      Enable benchmark mode: quieter output, print summary on shutdown
  -h, --help           Show this help

Environment:
  OPENAI_HTTP2_ENABLED   Set to 'false' to disable HTTP/2 and use HTTP/1.1
  OPENAI_INSECURE_SSL    Set to 'true' to skip SSL certificate verification (for testing)

HELP;
    exit(0);
}

$socketPath = $options['s'] ?? $options['socket'] ?? DEFAULT_SOCKET_PATH;
$socketMode = octdec($options['m'] ?? $options['mode'] ?? '0660');
$sseDelayMs = (int)($options['d'] ?? $options['delay'] ?? DEFAULT_SSE_DELAY_MS);
$sseDelaySeconds = $sseDelayMs / 1000;  // Pre-calculate once
$benchmarkMode = isset($options['b']) || isset($options['benchmark']);
$backendType = $options['backend'] ?? 'mock';
$openaiBase = $options['openai-base'] ?? getenv('OPENAI_API_BASE') ?: 'http://127.0.0.1:8080';
$openaiSocket = $options['openai-socket'] ?? getenv('OPENAI_API_SOCKET') ?: null;
$openaiKey = getenv('OPENAI_API_KEY') ?: 'benchmark-test';
$maxConnections = (int)($options['max-connections'] ?? getenv('MAX_CONNECTIONS') ?: DEFAULT_MAX_CONNECTIONS);

// HTTP/2 configuration
$http2Enabled = true;
$envVal = getenv('OPENAI_HTTP2_ENABLED');
if ($envVal !== false) {
    $http2Enabled = !in_array(strtolower($envVal), ['false', '0', 'no', 'off'], true);
}
$sslInsecure = filter_var(getenv('OPENAI_INSECURE_SSL'), FILTER_VALIDATE_BOOLEAN);

/** @var HttpClient|null */
$httpClient = null;

// Check for required extensions
if (!extension_loaded('sockets')) {
    die("Error: sockets extension is required\n");
}

// Increase file descriptor limit to handle many concurrent connections
// With OpenAI backend, each connection uses ~3 fds (apache + client + api socket)
// So 100k connections need ~300k fds. Set high limit for headroom.
if (function_exists('posix_setrlimit')) {
    $desiredLimit = 500000;
    if (posix_setrlimit(POSIX_RLIMIT_NOFILE, $desiredLimit, $desiredLimit)) {
        echo "Increased fd limit to $desiredLimit\n";
    } else {
        // Try lower limit if 500k fails
        $desiredLimit = 100000;
        if (posix_setrlimit(POSIX_RLIMIT_NOFILE, $desiredLimit, $desiredLimit)) {
            echo "Increased fd limit to $desiredLimit (500k not available)\n";
        } else {
            echo "Warning: could not increase fd limit\n";
        }
    }
}

/**
 * Statistics tracking
 */
class Stats {
    public int $connectionsStarted = 0;
    public int $connectionsCompleted = 0;
    public int $connectionsFailed = 0;
    public int $bytesSent = 0;
    public int $activeConnections = 0;
    public int $peakStreams = 0;

    public function streamStart(): void
    {
        $this->connectionsStarted++;
        $this->activeConnections++;
        if ($this->activeConnections > $this->peakStreams) {
            $this->peakStreams = $this->activeConnections;
        }
    }

    public function streamEnd(bool $success, int $bytes): void
    {
        $this->activeConnections--;
        if ($success) {
            $this->connectionsCompleted++;
        } else {
            $this->connectionsFailed++;
        }
        $this->bytesSent += $bytes;
    }

    public function printSummary(): void
    {
        echo "\n=== Benchmark Summary ===\n";
        echo "Peak concurrent streams: {$this->peakStreams}\n";
        echo "Total started: {$this->connectionsStarted}\n";
        echo "Total completed: {$this->connectionsCompleted}\n";
        echo "Total failed: {$this->connectionsFailed}\n";
        echo "Total bytes sent: {$this->bytesSent}\n";
        echo "=========================\n";
    }
}

$stats = new Stats();

/**
 * Receive a file descriptor and data via SCM_RIGHTS.
 * This is blocking but we run it in a non-blocking context.
 */
function receiveFd(\Socket $socket): array
{
    $message = [
        'name' => [],
        'buffer_size' => MAX_HANDOFF_DATA_SIZE,
        'controllen' => socket_cmsg_space(SOL_SOCKET, SCM_RIGHTS, 1),
    ];

    $bytes = socket_recvmsg($socket, $message, 0);
    if ($bytes === false) {
        throw new RuntimeException('socket_recvmsg failed: ' . socket_strerror(socket_last_error($socket)));
    }

    // Extract the file descriptor from control message FIRST, before checking
    // truncation flags. After recvmsg succeeds, any SCM_RIGHTS fds have been
    // transferred to our process and must be closed to avoid leaks.
    $fd = null;
    if (!empty($message['control'])) {
        foreach ($message['control'] as $cmsg) {
            if ($cmsg['level'] === SOL_SOCKET && $cmsg['type'] === SCM_RIGHTS) {
                $fd = $cmsg['data'][0];
                break;
            }
        }
    }

    // Check for truncation - if data exceeded buffer, JSON would be corrupt.
    // Close the fd first to prevent leaks (SCM_RIGHTS fds are already in our
    // process after recvmsg succeeds).
    $flags = $message['flags'] ?? 0;
    if (($flags & MSG_TRUNC) || ($flags & MSG_CTRUNC)) {
        if ($fd !== null && function_exists('posix_close')) {
            posix_close($fd);
        }
        $reason = ($flags & MSG_TRUNC)
            ? 'Handoff data truncated (exceeded ' . MAX_HANDOFF_DATA_SIZE . ' byte buffer)'
            : 'Control message truncated; fd may be corrupted';
        throw new RuntimeException($reason);
    }

    // Extract the data from iov
    $data = '';
    if (!empty($message['iov']) && !empty($message['iov'][0])) {
        $data = $message['iov'][0];
    }

    return [$fd, $data];
}

/**
 * Write all data to stream, handling partial writes.
 * Returns bytes written or false on error.
 */
function writeAll($stream, string $data): int|false
{
    $total = strlen($data);
    $written = 0;
    while ($written < $total) {
        $result = @fwrite($stream, substr($data, $written));
        if ($result === false || $result === 0) {
            return false;
        }
        $written += $result;
    }
    return $written;
}

/**
 * Stream response from OpenAI-compatible API using HTTP/2 via amphp/http-client.
 * Returns [success, bytesSent].
 *
 * Uses HTTP/2 multiplexing for efficient connection sharing across streams.
 *
 * @param mixed $stream Client stream to write SSE chunks to
 * @param array $handoffData Request data from the handoff
 * @param string $openaiBase API base URL
 * @param string $openaiKey API key
 */
function streamFromOpenAIHttp2(mixed $stream, array $handoffData, string $openaiBase, string $openaiKey): array
{
    global $benchmarkMode, $httpClient;
    $bytesSent = 0;

    $prompt = $handoffData['prompt'] ?? 'Hello';
    $model = $handoffData['model'] ?? 'gpt-4o-mini';

    // Build request body
    $body = json_encode([
        'model' => $model,
        'messages' => [['role' => 'user', 'content' => $prompt]],
        'stream' => true,
    ]);

    $url = $openaiBase . '/chat/completions';

    try {
        // Create HTTP request
        $request = new Request($url, 'POST');
        $request->setBody($body);
        $request->setHeader('Content-Type', 'application/json');
        $request->setHeader('Authorization', 'Bearer ' . $openaiKey);
        $request->setTransferTimeout(120);
        $request->setBodySizeLimit(100 * 1024 * 1024); // 100MB for long streams

        // Add test pattern header if present (for validation testing)
        if (isset($handoffData['test_pattern']) && $handoffData['test_pattern'] !== '') {
            $request->setHeader('X-Test-Pattern', $handoffData['test_pattern']);
        }

        // Send request and get streaming response
        $response = $httpClient->request($request);
        $responseBody = $response->getBody();

        // Read response body in chunks and process SSE
        $buffer = '';
        while (($chunk = $responseBody->read()) !== null) {
            $buffer .= $chunk;

            // Process complete lines from buffer
            while (($pos = strpos($buffer, "\n")) !== false) {
                $line = substr($buffer, 0, $pos);
                $buffer = substr($buffer, $pos + 1);

                // Strip \r if present
                $line = rtrim($line, "\r");

                // Skip empty lines
                if ($line === '') continue;

                // Check for data: prefix (SSE format)
                if (!str_starts_with($line, 'data: ')) continue;

                $data = substr($line, 6);

                // Check for [DONE] marker
                if ($data === '[DONE]') {
                    $doneMsg = "data: [DONE]\n\n";
                    $written = writeAll($stream, $doneMsg);
                    if ($written !== false) {
                        $bytesSent += $written;
                    }
                    return [true, $bytesSent];
                }

                // Parse JSON chunk
                $parsed = json_decode($data, true);
                if (!$parsed) continue;

                // Extract content
                $content = $parsed['choices'][0]['delta']['content'] ?? null;
                if ($content !== null && $content !== '') {
                    // Forward as SSE to client
                    $sseData = json_encode(['content' => $content]);
                    $sseMsg = "data: {$sseData}\n\n";
                    $written = writeAll($stream, $sseMsg);
                    if ($written === false) {
                        return [false, $bytesSent];
                    }
                    $bytesSent += $written;
                }
            }
        }

        // API stream ended without [DONE] marker - send one to client
        $doneMsg = "data: [DONE]\n\n";
        $written = writeAll($stream, $doneMsg);
        if ($written !== false) {
            $bytesSent += $written;
        }

        return [true, $bytesSent];

    } catch (\Exception $e) {
        if (!$benchmarkMode) {
            echo "HTTP/2 streaming error: " . $e->getMessage() . "\n";
        }
        return [false, $bytesSent];
    }
}

/**
 * Stream response from OpenAI-compatible API using HTTP/1.1 async I/O.
 * Returns [success, bytesSent].
 *
 * Uses Amp's async socket to avoid blocking the event loop, enabling
 * concurrent API requests across multiple client connections.
 *
 * @param mixed $stream Client stream to write SSE chunks to
 * @param array $handoffData Request data from the handoff
 * @param string $openaiBase API base URL (used for HTTP path)
 * @param string $openaiKey API key
 * @param string|null $openaiSocket Unix socket path (if set, uses Unix socket instead of TCP)
 */
function streamFromOpenAI(mixed $stream, array $handoffData, string $openaiBase, string $openaiKey, ?string $openaiSocket = null): array
{
    global $benchmarkMode;
    $bytesSent = 0;

    $prompt = $handoffData['prompt'] ?? 'Hello';
    $model = $handoffData['model'] ?? 'gpt-4o-mini';

    // Build request body
    $body = json_encode([
        'model' => $model,
        'messages' => [['role' => 'user', 'content' => $prompt]],
        'stream' => true,
    ]);

    // Parse URL for HTTP request path
    $url = parse_url($openaiBase . '/chat/completions');
    $host = $url['host'] ?? 'localhost';
    $port = $url['port'] ?? 80;
    $path = $url['path'] ?? '/chat/completions';

    try {
        // Connect using Amp's async socket (non-blocking)
        $context = (new ConnectContext())->withConnectTimeout(10);

        if ($openaiSocket !== null) {
            // Use Unix socket for high-concurrency (no ephemeral port limits)
            $apiSocket = connect("unix://{$openaiSocket}", $context);
        } else {
            // Use TCP
            $apiSocket = connect("tcp://{$host}:{$port}", $context);
        }
    } catch (\Exception $e) {
        if (!$benchmarkMode) {
            echo "OpenAI connect failed: " . $e->getMessage() . "\n";
        }
        return [false, $bytesSent];
    }

    try {
        // Build and send HTTP request
        $request = "POST {$path} HTTP/1.1\r\n";
        $request .= "Host: {$host}\r\n";
        $request .= "Content-Type: application/json\r\n";
        $request .= "Authorization: Bearer {$openaiKey}\r\n";
        $request .= "Content-Length: " . strlen($body) . "\r\n";
        $request .= "Connection: close\r\n";
        // Add test pattern header if present (for validation testing)
        if (isset($handoffData['test_pattern']) && $handoffData['test_pattern'] !== '') {
            $request .= "X-Test-Pattern: " . $handoffData['test_pattern'] . "\r\n";
        }
        $request .= "\r\n";
        $request .= $body;

        $apiSocket->write($request);

        // Read response (headers + body) in chunks
        $buffer = '';
        $headersComplete = false;

        while (($chunk = $apiSocket->read()) !== null) {
            $buffer .= $chunk;

            // Process complete lines from buffer
            while (($pos = strpos($buffer, "\n")) !== false) {
                $line = substr($buffer, 0, $pos);
                $buffer = substr($buffer, $pos + 1);

                // Strip \r if present
                $line = rtrim($line, "\r");

                // Skip headers until we see empty line
                if (!$headersComplete) {
                    if ($line === '') {
                        $headersComplete = true;
                    }
                    continue;
                }

                // Skip empty lines in body
                if ($line === '') continue;

                // Check for data: prefix (SSE format)
                if (!str_starts_with($line, 'data: ')) continue;

                $data = substr($line, 6);

                // Check for [DONE] marker
                if ($data === '[DONE]') {
                    // Send raw [DONE] marker (not wrapped in JSON)
                    $doneMsg = "data: [DONE]\n\n";
                    $written = writeAll($stream, $doneMsg);
                    if ($written !== false) {
                        $bytesSent += $written;
                    }
                    $apiSocket->close();
                    return [true, $bytesSent];
                }

                // Parse JSON chunk
                $parsed = json_decode($data, true);
                if (!$parsed) continue;

                // Extract content
                $content = $parsed['choices'][0]['delta']['content'] ?? null;
                if ($content !== null && $content !== '') {
                    // Forward as SSE to client
                    $sseData = json_encode(['content' => $content]);
                    $sseMsg = "data: {$sseData}\n\n";
                    $written = writeAll($stream, $sseMsg);
                    if ($written === false) {
                        $apiSocket->close();
                        return [false, $bytesSent];
                    }
                    $bytesSent += $written;
                }
            }
        }

        // API stream ended without [DONE] marker - send one to client
        $doneMsg = "data: [DONE]\n\n";
        $written = writeAll($stream, $doneMsg);
        if ($written !== false) {
            $bytesSent += $written;
        }

        $apiSocket->close();
        unset($apiSocket, $buffer, $request);
        return [true, $bytesSent];

    } catch (\Exception $e) {
        if (!$benchmarkMode) {
            echo "OpenAI streaming error: " . $e->getMessage() . "\n";
        }
        try { $apiSocket->close(); } catch (\Exception $e) {}
        unset($apiSocket, $buffer, $request);
        return [false, $bytesSent];
    }
}

/**
 * Stream an SSE response to the client using async I/O.
 */
function streamResponse(mixed $clientFd, array $handoffData, int $connId, Stats $stats): void
{
    global $benchmarkMode;
    $stats->streamStart();
    $bytesSent = 0;
    $success = false;

    try {
        // Convert to stream
        if ($clientFd instanceof \Socket) {
            $stream = socket_export_stream($clientFd);
            if ($stream === false) {
                throw new RuntimeException("Failed to export Socket to stream");
            }
        } elseif (is_resource($clientFd)) {
            $stream = $clientFd;
        } elseif (is_int($clientFd)) {
            $stream = fopen("php://fd/{$clientFd}", 'w');
            if ($stream === false) {
                throw new RuntimeException("Failed to open fd as stream");
            }
        } else {
            throw new RuntimeException("Unknown client_fd type: " . gettype($clientFd));
        }

        // Set non-blocking mode
        stream_set_blocking($stream, false);
        stream_set_write_buffer($stream, 0);

        // Send HTTP response headers (use pre-defined constant)
        $written = writeAll($stream, SSE_HEADERS);
        if ($written === false) {
            throw new RuntimeException("Failed to write headers");
        }
        $bytesSent += $written;

        global $backendType, $openaiBase, $openaiKey, $openaiSocket, $sseDelaySeconds, $http2Enabled, $httpClient;

        if ($backendType === 'openai') {
            // Use OpenAI backend
            if ($http2Enabled && $httpClient !== null) {
                // HTTP/2 with amphp/http-client
                [$success, $apiBytes] = streamFromOpenAIHttp2($stream, $handoffData, $openaiBase, $openaiKey);
            } else {
                // HTTP/1.1 with raw socket (via Unix socket if configured, otherwise TCP)
                [$success, $apiBytes] = streamFromOpenAI($stream, $handoffData, $openaiBase, $openaiKey, $openaiSocket);
            }
            $bytesSent += $apiBytes;
            if (!$success) {
                throw new RuntimeException("OpenAI streaming failed");
            }
        } else {
            // Send dynamic messages first (3 messages with per-connection data)
            $userId = $handoffData['user_id'] ?? 0;
            $prompt = $handoffData['prompt'] ?? 'unknown';
            $promptDisplay = strlen($prompt) > 50 ? substr($prompt, 0, 50) . '...' : $prompt;

            $dynamicLines = [
                "Connection $connId: Hello from AMPHP async daemon!",
                "User ID: $userId",
                "Prompt: $promptDisplay",
            ];

            $index = 0;

            // Send dynamic messages (3 messages with per-connection data)
            foreach ($dynamicLines as $line) {
                $data = json_encode(['content' => $line, 'index' => $index]);
                $sseMsg = "data: {$data}\n\n";

                $written = writeAll($stream, $sseMsg);
                if ($written === false) {
                    throw new RuntimeException("Write failed");
                }
                $bytesSent += $written;
                $index++;

                delay($sseDelaySeconds);
            }

            // Send static messages (15 messages from constant array)
            foreach (STATIC_MESSAGES as $line) {
                $data = json_encode(['content' => $line, 'index' => $index]);
                $sseMsg = "data: {$data}\n\n";

                $written = writeAll($stream, $sseMsg);
                if ($written === false) {
                    break;
                }
                $bytesSent += $written;
                $index++;

                delay($sseDelaySeconds);
            }
        }

        fclose($stream);
        unset($stream);
        $success = true;

    } catch (Throwable $e) {
        if (!$benchmarkMode) {
            echo "[$connId] Error: {$e->getMessage()}\n";
        }
        if (isset($stream) && is_resource($stream)) {
            @fclose($stream);
        }
    } finally {
        $stats->streamEnd($success, $bytesSent);
    }
}

/**
 * Handle a connection from Apache.
 */
function handleConnection(\Socket $apacheConn, int $connId, Stats $stats): void
{
    global $benchmarkMode;

    try {
        // Receive fd (blocking, but quick)
        [$clientFd, $data] = receiveFd($apacheConn);

        // Close Apache connection immediately - we have the fd and data now
        // This is critical for high concurrency: don't hold the Apache socket
        // open during the entire streaming response (which can be 10-30+ seconds)
        socket_close($apacheConn);

        if ($clientFd === null) {
            throw new RuntimeException("No file descriptor received");
        }

        // Parse handoff data
        // Trim NUL bytes - mod_socket_handoff sends a dummy \0 byte when
        // X-Handoff-Data is omitted.
        $handoffData = [];
        $trimmedData = trim($data, "\x00");
        if ($trimmedData !== '') {
            $handoffData = json_decode($trimmedData, true) ?? [];
        }

        // Stream response using async I/O
        streamResponse($clientFd, $handoffData, $connId, $stats);

    } catch (Throwable $e) {
        if (!$benchmarkMode) {
            echo "[$connId] Handoff error: {$e->getMessage()}\n";
        }
        // Note: Don't call streamEnd() here - streamStart() is only called
        // inside streamResponse(), so calling streamEnd() here would unbalance counters
    }
}

/**
 * Main function
 */
function main(string $socketPath, int $socketMode): void
{
    global $stats, $benchmarkMode, $maxConnections;

    global $backendType, $openaiBase, $openaiSocket, $http2Enabled, $sslInsecure, $httpClient;

    echo "AMPHP Async Streaming Daemon starting\n";
    echo "PHP " . PHP_VERSION . "\n";
    echo "Event loop: " . get_class(EventLoop::getDriver()) . "\n";
    echo "Socket: $socketPath\n";
    echo "Backend: $backendType\n";
    if ($backendType === 'openai') {
        echo "OpenAI base: $openaiBase\n";
        $url = parse_url($openaiBase);
        $isHttps = ($url['scheme'] ?? 'http') === 'https';

        if ($http2Enabled && $isHttps) {
            echo "HTTP mode: HTTP/2 with HTTPS\n";

            // Create HTTP client with TLS configuration
            $tlsContext = new ClientTlsContext('');
            if ($sslInsecure) {
                $tlsContext = $tlsContext->withoutPeerVerification();
                echo "SSL verification: disabled (insecure mode)\n";
            }

            $connectContext = (new ConnectContext())->withTlsContext($tlsContext);
            $connectionFactory = new DefaultConnectionFactory(null, $connectContext);
            // HTTP/2 multiplexing: ~100 streams per connection (server limit)
            // Scale connections based on maxConnections: 100k streams / 100 = 1000 connections
            $h2ConnectionLimit = max(10, (int) ceil($maxConnections / 100));
            echo "HTTP/2 connection limit: $h2ConnectionLimit (for $maxConnections max streams)\n";
            $pool = ConnectionLimitingPool::byAuthority($h2ConnectionLimit, $connectionFactory);

            $httpClient = (new HttpClientBuilder())
                ->usingPool($pool)
                ->build();
        } elseif ($http2Enabled) {
            echo "HTTP mode: HTTP/2 requires HTTPS, falling back to HTTP/1.1\n";
            $http2Enabled = false;
        } else {
            echo "HTTP mode: HTTP/1.1\n";
        }

        if ($openaiSocket) {
            echo "OpenAI socket: $openaiSocket (Unix socket, no port limits)\n";
        }
    }
    echo "Max connections: $maxConnections\n";
    if ($benchmarkMode) {
        echo "Benchmark mode enabled: quieter output, will print summary on shutdown\n";
    }

    // Connection counter for limiting concurrent connections
    // This prevents OOM by capping concurrent connections - excess connections
    // queue in the kernel backlog (up to somaxconn) until capacity is available
    $activeConnections = 0;

    // Remove stale socket
    if (file_exists($socketPath)) {
        unlink($socketPath);
    }

    // Create Unix socket
    $server = socket_create(AF_UNIX, SOCK_STREAM, 0);
    if ($server === false) {
        die("socket_create failed: " . socket_strerror(socket_last_error()) . "\n");
    }

    // Set non-blocking for async accept
    socket_set_nonblock($server);

    // Bind to socket path
    if (!socket_bind($server, $socketPath)) {
        die("socket_bind failed: " . socket_strerror(socket_last_error($server)) . "\n");
    }

    // Set permissions
    chmod($socketPath, $socketMode);

    // Listen for connections
    // Use a high backlog for high-concurrency benchmarks (system caps at somaxconn)
    if (!socket_listen($server, 8192)) {
        die("socket_listen failed: " . socket_strerror(socket_last_error($server)) . "\n");
    }

    echo "Listening on $socketPath (backlog: 8192)\n";

    $connId = 0;
    $running = true;

    // Handle signals
    EventLoop::onSignal(SIGINT, function () use (&$running, $server, $socketPath) {
        echo "\nShutting down...\n";
        $running = false;
        socket_close($server);
        if (file_exists($socketPath)) {
            unlink($socketPath);
        }
        EventLoop::getDriver()->stop();
    });

    EventLoop::onSignal(SIGTERM, function () use (&$running, $server, $socketPath) {
        echo "\nShutting down...\n";
        $running = false;
        socket_close($server);
        if (file_exists($socketPath)) {
            unlink($socketPath);
        }
        EventLoop::getDriver()->stop();
    });

    // Accept loop using event loop
    // Use greedy accept pattern: drain ALL pending connections per readable event
    // This prevents backlog buildup under high connection rates
    $acceptPaused = false;
    $acceptCallbackId = null;  // Will be set after registration
    $acceptCallbackId = EventLoop::onReadable(socket_export_stream($server), function ($id, $serverStream) use (&$connId, $server, $stats, &$running, &$activeConnections, $maxConnections, &$acceptPaused, &$acceptCallbackId) {
        if (!$running) {
            return;
        }

        // Greedy accept: drain all pending connections before returning to event loop
        while (true) {
            // Check connection limit
            if ($activeConnections >= $maxConnections) {
                // At capacity - PAUSE accepting to avoid busy-wait loop
                // Will be resumed when a connection completes
                $acceptPaused = true;
                EventLoop::disable($acceptCallbackId);
                return;
            }

            $apacheConn = @socket_accept($server);
            if ($apacheConn === false) {
                return; // EAGAIN/EWOULDBLOCK - no more pending connections
            }

            $connId++;
            $currentConnId = $connId;
            $activeConnections++;

            // Handle connection in a new fiber (async)
            // Capture callback ID by value to break potential reference cycles
            $cbId = $acceptCallbackId;
            async(function () use ($apacheConn, $currentConnId, $stats, &$activeConnections, &$acceptPaused, $cbId) {
                try {
                    handleConnection($apacheConn, $currentConnId, $stats);
                } finally {
                    $activeConnections--;
                    if ($acceptPaused) {
                        $acceptPaused = false;
                        EventLoop::enable($cbId);
                    }
                    // Break cyclic references in AMPHP connect() - prevents memory explosion
                    gc_collect_cycles();
                }
            });
        }
    });

    echo "Ready, accepting connections...\n";

    // Run the event loop
    EventLoop::run();

    // Print summary
    if ($benchmarkMode) {
        $stats->printSummary();
    } else {
        echo sprintf(
            "Final stats: completed=%d failed=%d bytes=%d\n",
            $stats->connectionsCompleted,
            $stats->connectionsFailed,
            $stats->bytesSent
        );
    }
}

// Run the daemon
main($socketPath, $socketMode);
