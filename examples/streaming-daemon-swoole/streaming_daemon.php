#!/usr/bin/env php
<?php
/**
 * Swoole streaming daemon for mod_socket_handoff with HTTP/2 support.
 *
 * Uses Swoole coroutines for concurrent connection handling and
 * native HTTP/2 client for backend API requests.
 *
 * Performance characteristics:
 * - Single process, coroutine-driven
 * - One coroutine per connection (lightweight, ~8KB each)
 * - Expected capacity: 10,000+ concurrent connections
 * - Native HTTP/2 multiplexing for backend requests
 *
 * Usage:
 *   sudo php streaming_daemon.php [-s /var/run/streaming-daemon-swoole.sock]
 *
 * Requirements:
 *   - PHP 8.1+ with Swoole extension (--with-nghttp2-dir for HTTP/2)
 *   - sockets extension
 */

declare(strict_types=1);

use Swoole\Coroutine;
use Swoole\Coroutine\Http2\Client as Http2Client;
use Swoole\Coroutine\Http\Client as HttpClient;
use Swoole\Http2\Request as Http2Request;

// Disable memory limit
ini_set('memory_limit', '-1');

// Constants
const DEFAULT_SOCKET_PATH = '/var/run/streaming-daemon-swoole.sock';
const DEFAULT_SOCKET_MODE = 0660;
const MAX_HANDOFF_DATA_SIZE = 65536;
const DEFAULT_MAX_CONNECTIONS = 50000;
const DEFAULT_SSE_DELAY_MS = 50;

// MSG_TRUNC and MSG_CTRUNC constants
if (!defined('MSG_TRUNC')) {
    define('MSG_TRUNC', 0x20);
}
if (!defined('MSG_CTRUNC')) {
    define('MSG_CTRUNC', 0x08);
}

// SSE headers for client response
const SSE_HEADERS = "HTTP/1.1 200 OK\r\n" .
    "Content-Type: text/event-stream\r\n" .
    "Cache-Control: no-cache\r\n" .
    "Connection: close\r\n" .
    "X-Accel-Buffering: no\r\n" .
    "\r\n";

// Static messages for mock mode
const STATIC_MESSAGES = [
    "This daemon uses Swoole coroutines with HTTP/2.",
    "Each connection runs as a lightweight coroutine.",
    "Expected capacity: 10,000+ concurrent connections.",
    "Memory per coroutine: ~8KB each.",
    "The Apache worker was freed immediately after handoff.",
    "Coroutines provide cooperative multitasking.",
    "Swoole schedules coroutines efficiently.",
    "Non-blocking I/O allows high concurrency.",
    "Swoole provides native HTTP/2 client.",
    "This is message 13 of 18.",
    "This is message 14 of 18.",
    "This is message 15 of 18.",
    "This is message 16 of 18.",
    "This is message 17 of 18.",
    "[DONE]",
];

// Parse command line arguments
$options = getopt('s:m:d:hb', [
    'socket:', 'mode:', 'delay:', 'help', 'benchmark',
    'backend:', 'openai-base:', 'max-connections:'
]);

if (isset($options['h']) || isset($options['help'])) {
    echo <<<HELP
Swoole Streaming Daemon for mod_socket_handoff (HTTP/2 support)

Usage: php streaming_daemon.php [options]

Options:
  -s, --socket PATH      Unix socket path (default: /var/run/streaming-daemon-swoole.sock)
  -m, --mode MODE        Socket permissions in octal (default: 0660)
  -d, --delay MS         Delay between SSE messages in ms for mock mode (default: 50)
  --backend TYPE         Backend type: 'mock' (demo messages) or 'openai' (HTTP streaming API)
  --openai-base URL      OpenAI API base URL (or OPENAI_API_BASE env)
                         Uses HTTP/2 h2c for http://, HTTP/2 ALPN for https://
  --max-connections N    Max concurrent connections (default: 50000)
  -b, --benchmark        Enable benchmark mode: quieter output
  -h, --help             Show this help

Environment:
  OPENAI_HTTP2_ENABLED   Set to 'false' to disable HTTP/2 and use HTTP/1.1
  OPENAI_API_BASE        Default API base URL
  OPENAI_API_KEY         API key for OpenAI backend

HTTP/2 Mode:
  - For http:// URLs: Uses HTTP/2 with prior knowledge (h2c)
  - For https:// URLs: Uses HTTP/2 with ALPN negotiation
  - HTTP/2 is TCP only (no Unix socket support for HTTP/2)
  - Set OPENAI_HTTP2_ENABLED=false to use HTTP/1.1 instead

HELP;
    exit(0);
}

$socketPath = $options['s'] ?? $options['socket'] ?? DEFAULT_SOCKET_PATH;
$socketMode = octdec($options['m'] ?? $options['mode'] ?? '0660');
$sseDelayMs = (int)($options['d'] ?? $options['delay'] ?? DEFAULT_SSE_DELAY_MS);
$benchmarkMode = isset($options['b']) || isset($options['benchmark']);
$backendType = $options['backend'] ?? 'mock';
$openaiBase = $options['openai-base'] ?? getenv('OPENAI_API_BASE') ?: 'http://127.0.0.1:8080';
$openaiKey = getenv('OPENAI_API_KEY') ?: 'benchmark-test';
$maxConnections = (int)($options['max-connections'] ?? getenv('MAX_CONNECTIONS') ?: DEFAULT_MAX_CONNECTIONS);

// HTTP/2 configuration
$http2Enabled = true;
$envVal = getenv('OPENAI_HTTP2_ENABLED');
if ($envVal !== false) {
    $http2Enabled = !in_array(strtolower($envVal), ['false', '0', 'no', 'off'], true);
}

// Check extensions
if (!extension_loaded('swoole') && !extension_loaded('openswoole')) {
    fwrite(STDERR, "Error: Swoole or OpenSwoole extension required\n");
    fwrite(STDERR, "Install with: pecl install swoole\n");
    fwrite(STDERR, "Build flags for HTTP/2: --with-nghttp2-dir=/usr\n");
    exit(1);
}
if (!extension_loaded('sockets')) {
    fwrite(STDERR, "Error: sockets extension required\n");
    exit(1);
}

// Check for HTTP/2 support if needed
if ($http2Enabled && $backendType === 'openai') {
    if (!class_exists('Swoole\Coroutine\Http2\Client')) {
        fwrite(STDERR, "Error: Swoole HTTP/2 support not available\n");
        fwrite(STDERR, "Rebuild Swoole with: --with-nghttp2-dir=/usr\n");
        exit(1);
    }
}

// Increase file descriptor limit
if (function_exists('posix_setrlimit')) {
    $desiredLimit = 500000;
    if (!posix_setrlimit(POSIX_RLIMIT_NOFILE, $desiredLimit, $desiredLimit)) {
        $desiredLimit = 100000;
        if (!posix_setrlimit(POSIX_RLIMIT_NOFILE, $desiredLimit, $desiredLimit)) {
            echo "Warning: could not increase fd limit\n";
        }
    }
}

/**
 * Stats tracking
 */
class Stats
{
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

use Swoole\Coroutine\Channel;

/**
 * Multiplexed HTTP/2 Connection with dedicated reader coroutine.
 *
 * Enables true HTTP/2 multiplexing by having a single reader coroutine
 * that dispatches responses to waiting streams via channels.
 */
class MultiplexedHttp2Conn
{
    private Http2Client $client;
    private int $activeStreams = 0;
    private int $maxStreams;
    private bool $running = false;
    private int $readerCid = 0;

    /** @var array<int, Channel> Map of streamId -> response channel */
    private array $streamChannels = [];

    /** @var array<int, array> Buffer for early responses before channel is registered */
    private array $pendingResponses = [];

    public function __construct(Http2Client $client, int $maxStreams = 100)
    {
        $this->client = $client;
        $this->maxStreams = $maxStreams;
    }

    /**
     * Start the reader coroutine that dispatches responses.
     */
    public function startReader(): void
    {
        if ($this->running) {
            return;
        }
        $this->running = true;

        $this->readerCid = Coroutine::create(function () {
            while ($this->running && $this->client->connected) {
                // Read next response (any stream)
                // Use short timeout (10ms) to minimize head-of-line blocking
                // and ensure responsive data dispatch to waiting streams
                $response = $this->client->recv(0.01);

                if ($response === false) {
                    // Timeout or error - check if still running
                    if (!$this->client->connected) {
                        break;
                    }
                    continue;
                }

                $streamId = $response->streamId;
                if (isset($this->streamChannels[$streamId])) {
                    // Dispatch to waiting coroutine
                    $this->streamChannels[$streamId]->push($response);
                } else {
                    // Buffer early response - channel not registered yet (race condition)
                    if (!isset($this->pendingResponses[$streamId])) {
                        $this->pendingResponses[$streamId] = [];
                    }
                    $this->pendingResponses[$streamId][] = $response;
                }
            }

            // Signal end to all waiting streams
            foreach ($this->streamChannels as $ch) {
                $ch->push(false);
            }
            $this->running = false;
        });
    }

    /**
     * Stop the reader coroutine.
     */
    public function stopReader(): void
    {
        $this->running = false;
        if ($this->readerCid > 0 && Coroutine::exists($this->readerCid)) {
            Coroutine::cancel($this->readerCid);
        }
    }

    /**
     * Check if connection can accept more streams.
     */
    public function hasCapacity(): bool
    {
        return $this->activeStreams < $this->maxStreams && $this->client->connected;
    }

    /**
     * Get the HTTP/2 client for sending requests.
     */
    public function getClient(): Http2Client
    {
        return $this->client;
    }

    /**
     * Register a stream and get its response channel.
     * Flushes any pending responses that arrived before registration (race condition fix).
     */
    public function registerStream(int $streamId): Channel
    {
        $ch = new Channel(32); // Buffer for streaming responses
        $this->streamChannels[$streamId] = $ch;
        $this->activeStreams++;

        // Flush any pending responses that arrived before registration
        // This handles the race condition where reader receives data before
        // the stream caller has registered its channel
        if (isset($this->pendingResponses[$streamId])) {
            foreach ($this->pendingResponses[$streamId] as $response) {
                $ch->push($response);
            }
            unset($this->pendingResponses[$streamId]);
        }

        return $ch;
    }

    /**
     * Unregister a stream when done.
     */
    public function unregisterStream(int $streamId): void
    {
        if (isset($this->streamChannels[$streamId])) {
            $this->streamChannels[$streamId]->close();
            unset($this->streamChannels[$streamId]);
        }
        if ($this->activeStreams > 0) {
            $this->activeStreams--;
        }
    }

    /**
     * Get active stream count.
     */
    public function getActiveStreams(): int
    {
        return $this->activeStreams;
    }

    /**
     * Check if connected.
     */
    public function isConnected(): bool
    {
        return $this->client->connected;
    }

    /**
     * Close the connection.
     */
    public function close(): void
    {
        $this->stopReader();
        try {
            $this->client->close();
        } catch (Throwable $e) {
            // Ignore
        }
    }
}

/**
 * HTTP/2 Connection Pool with true multiplexing.
 *
 * Each connection has a dedicated reader coroutine that dispatches
 * responses to waiting streams via channels. This enables multiple
 * concurrent streams per connection (up to OpenAI's limit of 100).
 */
class Http2Pool
{
    /** @var MultiplexedHttp2Conn[] */
    private array $connections = [];

    /** @var \Swoole\Atomic Lock for thread-safe acquire (0=unlocked, 1=locked) */
    private \Swoole\Atomic $lock;

    private string $host;
    private int $port;
    private bool $ssl;
    private bool $sslVerify;
    private int $poolSize;
    private int $maxStreamsPerConn;

    public function __construct(
        string $host,
        int $port,
        bool $ssl = false,
        int $poolSize = 1000,
        int $maxStreamsPerConn = 100,
        bool $sslVerify = true
    ) {
        $this->host = $host;
        $this->port = $port;
        $this->ssl = $ssl;
        $this->sslVerify = $sslVerify;
        $this->poolSize = $poolSize;
        $this->maxStreamsPerConn = $maxStreamsPerConn;
        $this->lock = new \Swoole\Atomic(0); // 0 = unlocked
    }

    /**
     * Initialize the connection pool.
     */
    public function init(): void
    {
        // Connections are created on-demand in acquire()
        // Connection creation happens outside the lock to avoid blocking
    }

    /**
     * Acquire a connection with available capacity.
     * Returns [MultiplexedHttp2Conn, index] or [null, -1] if pool exhausted.
     */
    public function acquire(): array
    {
        // Acquire spinlock - cmpset(expected, new) returns true if swap succeeded
        while (!$this->lock->cmpset(0, 1)) {
            // Yield to other coroutines while waiting
            Coroutine::sleep(0.001);
        }

        try {
            // First, find existing connection with capacity
            for ($i = 0; $i < count($this->connections); $i++) {
                if ($this->connections[$i]->hasCapacity()) {
                    return [$this->connections[$i], $i];
                }
            }

            // Create new connection if pool not full
            if (count($this->connections) < $this->poolSize) {
                $client = new Http2Client($this->host, $this->port, $this->ssl);
                $settings = [
                    'timeout' => 60,
                    // Use 'localhost' for SNI to avoid RFC violation when host is IP address
                    'ssl_host_name' => 'localhost',
                ];
                // Allow self-signed certificates when SSL verification is disabled
                if ($this->ssl && !$this->sslVerify) {
                    $settings['ssl_verify_peer'] = false;
                }
                $client->set($settings);

                if (!$client->connect()) {
                    return [null, -1];
                }

                $conn = new MultiplexedHttp2Conn($client, $this->maxStreamsPerConn);
                $conn->startReader();
                $this->connections[] = $conn;
                return [$conn, count($this->connections) - 1];
            }

            return [null, -1];
        } finally {
            // Release lock
            $this->lock->set(0);
        }
    }

    /**
     * Check if any connection has other active streams (for drain logic).
     */
    public function hasOtherActiveStreams(int $index): bool
    {
        if ($index >= 0 && $index < count($this->connections)) {
            return $this->connections[$index]->getActiveStreams() > 1;
        }
        return false;
    }

    /**
     * Release is a no-op - stream unregistration handles cleanup.
     */
    public function release(int $index): void
    {
        // No-op - MultiplexedHttp2Conn handles stream lifecycle
    }

    /**
     * Mark failed - remove unhealthy connection.
     */
    public function markFailed(int $index): void
    {
        if ($index >= 0 && $index < count($this->connections)) {
            $this->connections[$index]->close();
            // Don't remove from array to preserve indices
        }
    }

    /**
     * Close all connections.
     */
    public function close(): void
    {
        foreach ($this->connections as $conn) {
            $conn->close();
        }
        $this->connections = [];
    }

    /**
     * Get pool statistics.
     */
    public function getStats(): array
    {
        $activeStreams = 0;
        $connectedCount = 0;
        foreach ($this->connections as $conn) {
            if ($conn->isConnected()) {
                $connectedCount++;
                $activeStreams += $conn->getActiveStreams();
            }
        }
        return [
            'pool_size' => $this->poolSize,
            'max_streams_per_conn' => $this->maxStreamsPerConn,
            'connected' => $connectedCount,
            'active_streams' => $activeStreams,
        ];
    }
}

/** @var Http2Pool|null */
$http2Pool = null;

/**
 * Receive fd via SCM_RIGHTS
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

    // Extract fd first, before checking truncation
    $fd = null;
    if (!empty($message['control'])) {
        foreach ($message['control'] as $cmsg) {
            if ($cmsg['level'] === SOL_SOCKET && $cmsg['type'] === SCM_RIGHTS) {
                $fd = $cmsg['data'][0];
                break;
            }
        }
    }

    // Check for truncation
    $flags = $message['flags'] ?? 0;
    if (($flags & MSG_TRUNC) || ($flags & MSG_CTRUNC)) {
        if ($fd !== null && function_exists('posix_close')) {
            posix_close($fd);
        }
        throw new RuntimeException('Message truncated');
    }

    $data = '';
    if (!empty($message['iov']) && !empty($message['iov'][0])) {
        $data = $message['iov'][0];
    }

    return [$fd, $data];
}

/**
 * Write all data to a Socket object using socket_write.
 *
 * Note: In Swoole's coroutine environment, socket_export_stream() + fwrite()
 * fails with ENOTCONN, but socket_write() works correctly. This appears to be
 * a Swoole quirk with stream wrappers.
 */
function socketWriteAll(\Socket $socket, string $data): int|false
{
    $total = strlen($data);
    $written = 0;
    while ($written < $total) {
        $result = @socket_write($socket, substr($data, $written));
        if ($result === false) {
            return false;
        }
        if ($result === 0) {
            // Socket buffer full, yield and retry
            Coroutine::sleep(0.001);
            continue;
        }
        $written += $result;
    }
    return $written;
}

/**
 * Stream response from OpenAI API using HTTP/2 via Swoole with true multiplexing.
 *
 * Uses a multiplexed connection pool where each connection has a dedicated
 * reader coroutine that dispatches responses to waiting streams via channels.
 */
function streamFromOpenAIHttp2(
    \Socket $clientSocket,
    array $handoffData,
    string $openaiBase,
    string $openaiKey,
    bool $benchmarkMode
): array {
    global $http2Pool;

    $bytesSent = 0;
    $streamId = 0;
    $conn = null;
    $poolIndex = -1;
    $channel = null;

    $prompt = $handoffData['prompt'] ?? 'Hello';
    $model = $handoffData['model'] ?? 'gpt-4o-mini';

    $body = json_encode([
        'model' => $model,
        'messages' => [['role' => 'user', 'content' => $prompt]],
        'stream' => true,
    ]);

    $url = parse_url($openaiBase . '/chat/completions');
    $path = $url['path'] ?? '/chat/completions';

    // Acquire multiplexed connection from pool
    [$conn, $poolIndex] = $http2Pool->acquire();
    if ($conn === null) {
        if (!$benchmarkMode) {
            echo "HTTP/2 pool exhausted\n";
        }
        return [false, $bytesSent];
    }

    try {
        $client = $conn->getClient();

        // Build request
        $request = new Http2Request();
        $request->method = 'POST';
        $request->path = $path;
        $headers = [
            'content-type' => 'application/json',
            'authorization' => 'Bearer ' . $openaiKey,
        ];
        if (isset($handoffData['test_pattern']) && $handoffData['test_pattern'] !== '') {
            $headers['x-test-pattern'] = $handoffData['test_pattern'];
        }
        $request->headers = $headers;
        $request->data = $body;
        $request->pipeline = true;

        // Send request and get stream ID
        $streamId = $client->send($request);
        if ($streamId === false) {
            if (!$benchmarkMode) {
                echo "HTTP/2 send failed\n";
            }
            $http2Pool->markFailed($poolIndex);
            return [false, $bytesSent];
        }

        // Register stream IMMEDIATELY after send to avoid race with reader
        // The reader coroutine might receive data before we register if there's any delay
        $channel = $conn->registerStream($streamId);

        // Read responses from our channel (dispatched by reader coroutine)
        $buffer = '';
        $lastDataTime = microtime(true);
        // Stream timeout must be longer than backend message delay
        // Configurable via STREAM_IDLE_TIMEOUT env var (default 120s for benchmarks)
        // Test patterns complete quickly so this mainly affects abort detection
        static $streamTimeout = null;
        if ($streamTimeout === null) {
            $streamTimeout = (float)(getenv('STREAM_IDLE_TIMEOUT') ?: 120);
        }

        while (true) {
            // Pop with short timeout (10ms) for responsive data handling
            $response = $channel->pop(0.01);

            if ($response === false) {
                // Timeout - check if stream has been idle too long
                $idleTime = microtime(true) - $lastDataTime;
                if ($idleTime > $streamTimeout) {
                    // Stream appears to have ended (no data for too long)
                    // Send [DONE] and close
                    $doneMsg = "data: [DONE]\n\n";
                    @socketWriteAll($clientSocket, $doneMsg);
                    $conn->unregisterStream($streamId);
                    return [true, $bytesSent];
                }

                // Check if client disconnected
                $testWrite = @socket_write($clientSocket, '');
                if ($testWrite === false) {
                    $conn->unregisterStream($streamId);
                    return [false, $bytesSent];
                }

                continue; // Keep waiting
            }

            $lastDataTime = microtime(true);

            // Check if stream ended
            if ($response->data === null || $response->data === '') {
                break;
            }

            $buffer .= $response->data;

            // Process SSE lines
            while (($pos = strpos($buffer, "\n")) !== false) {
                $line = rtrim(substr($buffer, 0, $pos), "\r");
                $buffer = substr($buffer, $pos + 1);

                if ($line === '' || !str_starts_with($line, 'data: ')) {
                    continue;
                }
                $sseData = substr($line, 6);

                if ($sseData === '[DONE]') {
                    $doneMsg = "data: [DONE]\n\n";
                    $written = socketWriteAll($clientSocket, $doneMsg);
                    if ($written !== false) {
                        $bytesSent += $written;
                    }
                    $conn->unregisterStream($streamId);
                    return [true, $bytesSent];
                }

                $chunk = json_decode($sseData, true);
                $content = $chunk['choices'][0]['delta']['content'] ?? null;
                if ($content !== null && $content !== '') {
                    $sseMsg = "data: " . json_encode(['content' => $content]) . "\n\n";
                    $written = socketWriteAll($clientSocket, $sseMsg);
                    if ($written === false) {
                        // Client disconnected
                        $conn->unregisterStream($streamId);
                        return [false, $bytesSent];
                    }
                    $bytesSent += $written;
                }
            }
        }

        // Send [DONE] if not already sent
        $doneMsg = "data: [DONE]\n\n";
        $written = socketWriteAll($clientSocket, $doneMsg);
        if ($written !== false) {
            $bytesSent += $written;
        }

        $conn->unregisterStream($streamId);
        return [true, $bytesSent];

    } catch (Throwable $e) {
        if (!$benchmarkMode) {
            echo "HTTP/2 error: " . $e->getMessage() . "\n";
        }
        if ($conn !== null && $streamId > 0) {
            $conn->unregisterStream($streamId);
        }
        return [false, $bytesSent];
    }
}

/**
 * Stream response from OpenAI API using HTTP/1.1 via Swoole.
 */
function streamFromOpenAIHttp1(
    \Socket $clientSocket,
    array $handoffData,
    string $openaiBase,
    string $openaiKey,
    bool $benchmarkMode
): array {
    $bytesSent = 0;

    $prompt = $handoffData['prompt'] ?? 'Hello';
    $model = $handoffData['model'] ?? 'gpt-4o-mini';

    $body = json_encode([
        'model' => $model,
        'messages' => [['role' => 'user', 'content' => $prompt]],
        'stream' => true,
    ]);

    $url = parse_url($openaiBase . '/chat/completions');
    $host = $url['host'] ?? 'localhost';
    $port = $url['port'] ?? (($url['scheme'] ?? 'http') === 'https' ? 443 : 80);
    $path = $url['path'] ?? '/chat/completions';
    $ssl = ($url['scheme'] ?? 'http') === 'https';

    // Create HTTP/1.1 client
    $client = new HttpClient($host, $port, $ssl);
    $client->set([
        'timeout' => 60,
    ]);

    $headers = [
        'Content-Type' => 'application/json',
        'Authorization' => 'Bearer ' . $openaiKey,
    ];
    // Add test pattern header if present (for validation testing)
    if (isset($handoffData['test_pattern']) && $handoffData['test_pattern'] !== '') {
        $headers['X-Test-Pattern'] = $handoffData['test_pattern'];
    }
    $client->setHeaders($headers);

    $client->setMethod('POST');
    $client->setData($body);

    // Execute request (blocking in coroutine context, non-blocking overall)
    $client->execute($path);

    if ($client->statusCode !== 200) {
        if (!$benchmarkMode) {
            echo "HTTP/1.1 request failed: status {$client->statusCode}\n";
        }
        $client->close();
        return [false, $bytesSent];
    }

    $responseBody = $client->body;
    $client->close();

    // Parse SSE response
    $lines = explode("\n", $responseBody);
    foreach ($lines as $line) {
        $line = rtrim($line, "\r");
        if ($line === '' || !str_starts_with($line, 'data: ')) {
            continue;
        }
        $sseData = substr($line, 6);

        if ($sseData === '[DONE]') {
            $doneMsg = "data: [DONE]\n\n";
            $written = socketWriteAll($clientSocket, $doneMsg);
            if ($written !== false) {
                $bytesSent += $written;
            }
            return [true, $bytesSent];
        }

        $chunk = json_decode($sseData, true);
        $content = $chunk['choices'][0]['delta']['content'] ?? null;
        if ($content !== null && $content !== '') {
            $sseMsg = "data: " . json_encode(['content' => $content]) . "\n\n";
            $written = socketWriteAll($clientSocket, $sseMsg);
            if ($written === false) {
                return [false, $bytesSent];
            }
            $bytesSent += $written;
        }
    }

    // Send [DONE] if not already sent
    $doneMsg = "data: [DONE]\n\n";
    $written = socketWriteAll($clientSocket, $doneMsg);
    if ($written !== false) {
        $bytesSent += $written;
    }

    return [true, $bytesSent];
}

/**
 * Stream response to client
 *
 * @param \Socket $clientSocket The client socket received via SCM_RIGHTS (PHP 8+ returns Socket object)
 */
function streamResponse(\Socket $clientSocket, array $handoffData, int $connId, Stats $stats): void
{
    global $benchmarkMode, $backendType, $openaiBase, $openaiKey, $http2Enabled, $sseDelayMs;

    $stats->streamStart();
    $bytesSent = 0;
    $success = false;

    try {
        // Send SSE headers using direct socket_write (socket_export_stream doesn't work in Swoole)
        $written = socketWriteAll($clientSocket, SSE_HEADERS);
        if ($written === false) {
            $error = socket_last_error($clientSocket);
            throw new RuntimeException("Failed to write headers: " . socket_strerror($error));
        }
        $bytesSent += $written;

        if ($backendType === 'openai') {
            if ($http2Enabled) {
                [$success, $apiBytes] = streamFromOpenAIHttp2($clientSocket, $handoffData, $openaiBase, $openaiKey, $benchmarkMode);
            } else {
                [$success, $apiBytes] = streamFromOpenAIHttp1($clientSocket, $handoffData, $openaiBase, $openaiKey, $benchmarkMode);
            }
            $bytesSent += $apiBytes;
        } else {
            // Mock backend - send test messages
            $userId = $handoffData['user_id'] ?? 0;
            $prompt = $handoffData['prompt'] ?? 'unknown';
            $promptDisplay = strlen($prompt) > 50 ? substr($prompt, 0, 50) . '...' : $prompt;

            $dynamicLines = [
                "Connection $connId: Hello from Swoole daemon!",
                "User ID: $userId",
                "Prompt: $promptDisplay",
            ];

            $index = 0;
            $sseDelaySeconds = $sseDelayMs / 1000;

            // Send dynamic messages
            foreach ($dynamicLines as $line) {
                $data = json_encode(['content' => $line, 'index' => $index]);
                $sseMsg = "data: {$data}\n\n";
                $written = socketWriteAll($clientSocket, $sseMsg);
                if ($written === false) {
                    break;
                }
                $bytesSent += $written;
                $index++;
                Coroutine::sleep($sseDelaySeconds);
            }

            // Send static messages
            foreach (STATIC_MESSAGES as $line) {
                $data = json_encode(['content' => $line, 'index' => $index]);
                $sseMsg = "data: {$data}\n\n";
                $written = socketWriteAll($clientSocket, $sseMsg);
                if ($written === false) {
                    break;
                }
                $bytesSent += $written;
                $index++;
                Coroutine::sleep($sseDelaySeconds);
            }
            $success = true;
        }

        @socket_close($clientSocket);

    } catch (Throwable $e) {
        if (!$benchmarkMode) {
            echo "[$connId] Error: {$e->getMessage()}\n";
        }
        @socket_close($clientSocket);
    } finally {
        $stats->streamEnd($success, $bytesSent);
    }
}

/**
 * Handle connection from Apache
 */
function handleConnection(\Socket $apacheConn, int $connId, Stats $stats): void
{
    global $benchmarkMode;

    try {
        [$clientFd, $data] = receiveFd($apacheConn);
        socket_close($apacheConn);

        if ($clientFd === null) {
            throw new RuntimeException("No file descriptor received");
        }

        $handoffData = [];
        $trimmedData = trim($data, "\x00");
        if ($trimmedData !== '') {
            $handoffData = json_decode($trimmedData, true) ?? [];
        }

        streamResponse($clientFd, $handoffData, $connId, $stats);

    } catch (Throwable $e) {
        // Always show the first few errors for debugging
        static $errorCount = 0;
        $errorCount++;
        if (!$benchmarkMode || $errorCount <= 5) {
            echo "[$connId] Handoff error: {$e->getMessage()}\n";
        }
    }
}


// Global running flag for graceful shutdown
$running = true;

// Print startup info
echo "Swoole Streaming Daemon starting\n";
echo "PHP " . PHP_VERSION . "\n";
echo "Swoole " . (defined('SWOOLE_VERSION') ? SWOOLE_VERSION : (defined('OPENSWOOLE_VERSION') ? OPENSWOOLE_VERSION : 'unknown')) . "\n";
echo "Socket: $socketPath\n";
echo "Backend: $backendType\n";

if ($backendType === 'openai') {
    echo "OpenAI base: $openaiBase\n";
    $url = parse_url($openaiBase);
    if ($http2Enabled) {
        if (($url['scheme'] ?? 'http') === 'https') {
            echo "HTTP mode: HTTP/2 ALPN\n";
        } else {
            echo "HTTP mode: HTTP/2 h2c (prior knowledge)\n";
        }

        // Initialize HTTP/2 connection pool with true multiplexing
        // Each connection has a reader coroutine dispatching to streams via channels
        $host = $url['host'] ?? 'localhost';
        $port = $url['port'] ?? (($url['scheme'] ?? 'http') === 'https' ? 443 : 80);
        $ssl = ($url['scheme'] ?? 'http') === 'https';

        // OPENAI_INSECURE_SSL=true allows self-signed certificates (for testing)
        $sslVerify = !filter_var(getenv('OPENAI_INSECURE_SSL'), FILTER_VALIDATE_BOOLEAN);

        // OpenAI API limits: 100 concurrent streams per HTTP/2 connection
        $maxStreamsPerConn = 100;
        $poolSize = (int) ceil($maxConnections / $maxStreamsPerConn);
        $http2Pool = new Http2Pool($host, $port, $ssl, $poolSize, $maxStreamsPerConn, $sslVerify);
        $http2Pool->init();
        echo "HTTP/2 pool: {$poolSize} connections x {$maxStreamsPerConn} streams = " . ($poolSize * $maxStreamsPerConn) . " max concurrent\n";
    } else {
        echo "HTTP mode: HTTP/1.1\n";
    }
}

echo "Max connections: $maxConnections\n";
if ($benchmarkMode) {
    echo "Benchmark mode enabled\n";
}

// Remove stale socket
if (file_exists($socketPath)) {
    unlink($socketPath);
}

// Create Unix socket BEFORE enabling Swoole hooks
// This ensures socket_recvmsg() works correctly for SCM_RIGHTS
$server = socket_create(AF_UNIX, SOCK_STREAM, 0);
if ($server === false) {
    fwrite(STDERR, "socket_create failed: " . socket_strerror(socket_last_error()) . "\n");
    exit(1);
}

// Set non-blocking for polling
socket_set_nonblock($server);

if (!socket_bind($server, $socketPath)) {
    fwrite(STDERR, "socket_bind failed: " . socket_strerror(socket_last_error($server)) . "\n");
    exit(1);
}

chmod($socketPath, $socketMode);

if (!socket_listen($server, 65535)) {
    fwrite(STDERR, "socket_listen failed: " . socket_strerror(socket_last_error($server)) . "\n");
    exit(1);
}

echo "Listening on $socketPath\n";
echo "Ready, accepting connections...\n";

// Set hook flags to enable non-socket hooks
// Socket hooks are disabled to preserve native PHP socket behavior for SCM_RIGHTS
Coroutine::set([
    'hook_flags' => SWOOLE_HOOK_ALL ^ SWOOLE_HOOK_SOCKETS,
]);

// Active connections counter
$activeConnections = 0;
$connId = 0;

// Track active coroutine IDs for graceful shutdown
$activeCoroutines = [];

// Timer-based accept loop (runs every 1ms)
$timerId = Swoole\Timer::tick(1, function () use ($server, $stats, &$activeConnections, &$connId, $benchmarkMode, $maxConnections, &$activeCoroutines) {
    global $running;

    if (!$running) {
        return;
    }

    if ($activeConnections >= $maxConnections) {
        return;
    }

    // Try to accept connections (non-blocking)
    while (true) {
        $conn = @socket_accept($server);
        if ($conn === false) {
            break; // No more pending connections
        }

        // Debug: connection accepted (first few only)
        if (!$benchmarkMode && $connId < 5) {
            echo "Accepted connection!\n";
        }

        $connId++;
        $activeConnections++;
        $currentConnId = $connId;

        // Handle in new coroutine
        $cid = Coroutine::create(function () use ($conn, $currentConnId, $stats, &$activeConnections, &$activeCoroutines) {
            $myCid = Coroutine::getCid();
            try {
                handleConnection($conn, $currentConnId, $stats);
            } finally {
                $activeConnections--;
                unset($activeCoroutines[$myCid]);
            }
        });
        if ($cid !== false) {
            $activeCoroutines[$cid] = true;
        }
    }
});

// Graceful shutdown function
$shutdownHandler = function (string $signal) use (&$running, $timerId, $server, $stats, $benchmarkMode, $socketPath, &$activeCoroutines, &$http2Pool) {
    $running = false;
    echo "\nReceived $signal, shutting down...\n";
    Swoole\Timer::clear($timerId);

    // Cancel all active coroutines to prevent deadlock
    $cancelled = 0;
    foreach (array_keys($activeCoroutines) as $cid) {
        if (Coroutine::exists($cid)) {
            Coroutine::cancel($cid);
            $cancelled++;
        }
    }
    if ($cancelled > 0 && !$benchmarkMode) {
        echo "Cancelled $cancelled active coroutines.\n";
    }

    // Close HTTP/2 connection pool
    if ($http2Pool !== null) {
        $poolStats = $http2Pool->getStats();
        if (!$benchmarkMode) {
            echo "Closing HTTP/2 pool ({$poolStats['connected']} connections, {$poolStats['active_streams']} active streams).\n";
        }
        $http2Pool->close();
    }

    echo "Shutdown complete.\n";
    if ($benchmarkMode) {
        $stats->printSummary();
    }

    socket_close($server);
    if (file_exists($socketPath)) {
        @unlink($socketPath);
    }

    Swoole\Event::exit();
};

// Handle signals
Swoole\Process::signal(SIGINT, fn() => $shutdownHandler('SIGINT'));
Swoole\Process::signal(SIGTERM, fn() => $shutdownHandler('SIGTERM'));

// Start the event loop
Swoole\Event::wait();
