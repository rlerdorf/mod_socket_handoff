#!/usr/bin/env php
<?php
/**
 * Swow streaming daemon for mod_socket_handoff with HTTP/2 multiplexing.
 *
 * Uses Swow coroutines for concurrent connection handling and
 * curl_multi with CURLPIPE_MULTIPLEX for true HTTP/2 multiplexing.
 *
 * Performance characteristics:
 * - Single process, coroutine-driven
 * - One coroutine per connection (lightweight)
 * - Expected capacity: 10,000+ concurrent connections
 * - True HTTP/2 multiplexing: ~100 streams per TCP connection
 * - 99% reduction in backend connections vs HTTP/1.1
 *
 * Usage:
 *   sudo php streaming_daemon.php [-s /var/run/streaming-daemon-swow.sock]
 *
 * Requirements:
 *   - PHP 8.1+ with Swow extension
 *   - sockets extension
 *   - curl extension with HTTP/2 support (nghttp2)
 */

declare(strict_types=1);

use Swow\Coroutine;
use Swow\Signal;
use Swow\SignalException;

// Disable memory limit
ini_set('memory_limit', '-1');

// Constants
const DEFAULT_SOCKET_PATH = '/var/run/streaming-daemon-swow.sock';
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
    "This daemon uses Swow coroutines with HTTP/2.",
    "Each connection runs as a lightweight coroutine.",
    "Expected capacity: 10,000+ concurrent connections.",
    "HTTP/2 multiplexing via curl_multi.",
    "The Apache worker was freed immediately after handoff.",
    "Coroutines provide cooperative multitasking.",
    "Swow schedules coroutines efficiently.",
    "Non-blocking I/O allows high concurrency.",
    "curl_multi provides HTTP/2 pipelining.",
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
Swow Streaming Daemon for mod_socket_handoff (HTTP/2 support)

Usage: php streaming_daemon.php [options]

Options:
  -s, --socket PATH      Unix socket path (default: /var/run/streaming-daemon-swow.sock)
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
if (!extension_loaded('swow')) {
    fwrite(STDERR, "Error: Swow extension required\n");
    fwrite(STDERR, "Install with: pecl install swow\n");
    exit(1);
}
if (!extension_loaded('sockets')) {
    fwrite(STDERR, "Error: sockets extension required\n");
    exit(1);
}
if (!extension_loaded('curl')) {
    fwrite(STDERR, "Error: curl extension required\n");
    exit(1);
}

// Check for HTTP/2 support in curl
if ($http2Enabled) {
    $curlVersion = curl_version();
    if (!($curlVersion['features'] & CURL_VERSION_HTTP2)) {
        fwrite(STDERR, "Error: curl HTTP/2 support not available\n");
        fwrite(STDERR, "Rebuild curl with nghttp2 support\n");
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

/**
 * Per-stream context for curl_multi write callbacks.
 *
 * Each stream gets its own context to track state and buffer data.
 * The write callback accesses this to stream SSE data to the client.
 * The completionChannel is used to signal when the transfer is done.
 */
class StreamContext
{
    public \Socket $clientSocket;
    public int $bytesSent = 0;
    public string $sseBuffer = '';
    public bool $streamEnded = false;
    public bool $success = false;
    public bool $aborted = false;
    public \Swow\Channel $completionChannel;

    public function __construct(\Socket $clientSocket)
    {
        $this->clientSocket = $clientSocket;
        // Capacity 1: completion callback will push once
        $this->completionChannel = new \Swow\Channel(1);
    }
}

/**
 * Wrapper around a single curl_multi handle for HTTP/2 multiplexing.
 *
 * Each CurlMultiManager manages up to maxStreams concurrent HTTP/2 streams
 * over a single TCP connection. A dedicated reader coroutine polls the
 * curl_multi and dispatches completions via channels.
 */
class CurlMultiManager
{
    private \CurlMultiHandle $multi;
    private int $activeStreams = 0;
    private int $maxStreams;
    private bool $readerRunning = false;

    /** @var array<int, callable> handleId -> completion callback */
    private array $completionCallbacks = [];

    /** @var array<int, \CurlHandle> handleId -> CurlHandle for removal */
    private array $activeHandles = [];

    public function __construct(int $maxStreams = 100)
    {
        $this->maxStreams = $maxStreams;
        $this->multi = curl_multi_init();

        // Enable HTTP/2 multiplexing
        curl_multi_setopt($this->multi, CURLMOPT_PIPELINING, CURLPIPE_MULTIPLEX);
    }

    /**
     * Start the dedicated reader coroutine that polls curl_multi.
     *
     * This coroutine runs continuously while there are active handles,
     * polling for activity and dispatching completions. Only ONE reader
     * runs per CurlMultiManager, avoiding the overhead of N coroutines
     * all polling independently.
     */
    public function startReader(): void
    {
        if ($this->readerRunning) {
            return;
        }
        $this->readerRunning = true;

        \Swow\Coroutine::run(function () {
            while ($this->readerRunning) {
                if (empty($this->activeHandles)) {
                    // No active handles, sleep longer and check again
                    usleep(10000); // 10ms
                    continue;
                }

                // Wait for activity (100ms timeout to stay responsive)
                curl_multi_select($this->multi, 0.1);

                // Drive transfers
                do {
                    $status = curl_multi_exec($this->multi, $stillRunning);
                } while ($status === CURLM_CALL_MULTI_PERFORM);

                // Check for completed transfers
                while (($info = curl_multi_info_read($this->multi)) !== false) {
                    $this->handleCompletion($info);
                }

                // Brief yield to other coroutines (5ms)
                usleep(5000);
            }
        });
    }

    /**
     * Stop the reader coroutine.
     */
    public function stopReader(): void
    {
        $this->readerRunning = false;
    }

    /**
     * Handle a completed transfer.
     */
    private function handleCompletion(array $info): void
    {
        $handle = $info['handle'];
        $handleId = spl_object_id($handle);
        $success = ($info['result'] === CURLE_OK);

        if (isset($this->completionCallbacks[$handleId])) {
            $httpCode = curl_getinfo($handle, CURLINFO_HTTP_CODE);
            $callback = $this->completionCallbacks[$handleId];
            $callback($success, $httpCode);
        }

        // Clean up
        $this->removeHandleInternal($handle, $handleId);
    }

    /**
     * Check if this handle can accept more streams.
     */
    public function hasCapacity(): bool
    {
        return $this->activeStreams < $this->maxStreams;
    }

    /**
     * Add a curl easy handle to this multi handle.
     *
     * @param \CurlHandle $easy The configured curl easy handle
     * @param callable $onComplete Callback(bool $success, int $httpCode) when transfer completes
     * @return bool True if added successfully
     */
    public function addHandle(\CurlHandle $easy, callable $onComplete): bool
    {
        if (!$this->hasCapacity()) {
            return false;
        }

        $handleId = spl_object_id($easy);
        $this->activeHandles[$handleId] = $easy;
        $this->completionCallbacks[$handleId] = $onComplete;
        $this->activeStreams++;

        curl_multi_add_handle($this->multi, $easy);

        return true;
    }

    /**
     * Remove a curl easy handle (e.g., on abort).
     */
    public function removeHandle(\CurlHandle $easy): void
    {
        $handleId = spl_object_id($easy);
        $this->removeHandleInternal($easy, $handleId);
    }

    /**
     * Internal cleanup of a handle.
     */
    private function removeHandleInternal(\CurlHandle $easy, int $handleId): void
    {
        if (isset($this->activeHandles[$handleId])) {
            curl_multi_remove_handle($this->multi, $easy);
            unset($this->activeHandles[$handleId]);
            unset($this->completionCallbacks[$handleId]);
            if ($this->activeStreams > 0) {
                $this->activeStreams--;
            }
        }
    }

    /**
     * Check if a specific handle is still active.
     */
    public function isHandleActive(\CurlHandle $easy): bool
    {
        $handleId = spl_object_id($easy);
        return isset($this->activeHandles[$handleId]);
    }

    /**
     * Get active stream count.
     */
    public function getActiveStreams(): int
    {
        return $this->activeStreams;
    }

    /**
     * Close this multi handle and cleanup.
     */
    public function close(): void
    {
        // Stop reader first
        $this->stopReader();

        // Remove all handles
        foreach ($this->activeHandles as $handleId => $handle) {
            curl_multi_remove_handle($this->multi, $handle);
        }
        $this->activeHandles = [];
        $this->completionCallbacks = [];
        $this->activeStreams = 0;

        curl_multi_close($this->multi);
    }
}

/**
 * Pool of CurlMultiManager instances for HTTP/2 multiplexing.
 *
 * Manages multiple curl_multi handles, each supporting up to maxStreamsPerHandle
 * concurrent HTTP/2 streams. This allows scaling to very high concurrency
 * (e.g., 1000 handles × 100 streams = 100,000 concurrent streams).
 */
class CurlMultiPool
{
    /** @var CurlMultiManager[] */
    private array $handles = [];

    private string $host;
    private int $port;
    private bool $ssl;
    private int $poolSize;
    private int $maxStreamsPerHandle;
    private bool $acquiring = false;

    // Cached URL and headers for creating curl handles
    private string $baseUrl;
    private string $basePath;

    public function __construct(
        string $host,
        int $port,
        bool $ssl,
        int $poolSize,
        int $maxStreamsPerHandle = 100,
        string $basePath = ''
    ) {
        $this->host = $host;
        $this->port = $port;
        $this->ssl = $ssl;
        $this->poolSize = $poolSize;
        $this->maxStreamsPerHandle = $maxStreamsPerHandle;
        $this->basePath = rtrim($basePath, '/');

        // Build base URL (including path prefix like /v1)
        $scheme = $ssl ? 'https' : 'http';
        $this->baseUrl = "{$scheme}://{$host}:{$port}" . $this->basePath;
    }

    /**
     * Acquire a CurlMultiManager with available capacity.
     *
     * @return array{0: CurlMultiManager|null, 1: int} [handle, index] or [null, -1] if exhausted
     */
    public function acquire(): array
    {
        // Simple spinlock for thread safety
        while ($this->acquiring) {
            usleep(1000);
        }
        $this->acquiring = true;

        try {
            // Find existing handle with capacity
            foreach ($this->handles as $i => $handle) {
                if ($handle->hasCapacity()) {
                    return [$handle, $i];
                }
            }

            // Create new if pool not full
            if (count($this->handles) < $this->poolSize) {
                $handle = new CurlMultiManager($this->maxStreamsPerHandle);
                $handle->startReader();  // Start dedicated reader coroutine
                $this->handles[] = $handle;
                return [$handle, count($this->handles) - 1];
            }

            return [null, -1];
        } finally {
            $this->acquiring = false;
        }
    }

    /**
     * Get the base URL for requests.
     */
    public function getBaseUrl(): string
    {
        return $this->baseUrl;
    }

    /**
     * Check if using SSL.
     */
    public function isSsl(): bool
    {
        return $this->ssl;
    }

    /**
     * Close all handles in the pool.
     */
    public function close(): void
    {
        foreach ($this->handles as $handle) {
            $handle->close();
        }
        $this->handles = [];
    }

    /**
     * Get pool statistics.
     */
    public function getStats(): array
    {
        $activeStreams = 0;
        $handleCount = count($this->handles);
        foreach ($this->handles as $handle) {
            $activeStreams += $handle->getActiveStreams();
        }
        return [
            'pool_size' => $this->poolSize,
            'max_streams_per_handle' => $this->maxStreamsPerHandle,
            'handles' => $handleCount,
            'active_streams' => $activeStreams,
        ];
    }
}

/** @var CurlMultiPool|null */
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
            usleep(1000);
            continue;
        }
        $written += $result;
    }
    return $written;
}

/**
 * Stream response from OpenAI API using HTTP/2 via curl.
 */
function streamFromOpenAIHttp2(
    \Socket $clientSocket,
    array $handoffData,
    string $openaiBase,
    string $openaiKey,
    bool $benchmarkMode
): array {
    $bytesSent = 0;
    $buffer = '';
    $success = false;
    $streamEnded = false;

    $prompt = $handoffData['prompt'] ?? 'Hello';
    $model = $handoffData['model'] ?? 'gpt-4o-mini';

    $body = json_encode([
        'model' => $model,
        'messages' => [['role' => 'user', 'content' => $prompt]],
        'stream' => true,
    ]);

    $url = $openaiBase . '/chat/completions';

    // Create curl handle
    $ch = curl_init();
    curl_setopt($ch, CURLOPT_URL, $url);
    curl_setopt($ch, CURLOPT_POST, true);
    curl_setopt($ch, CURLOPT_POSTFIELDS, $body);
    curl_setopt($ch, CURLOPT_RETURNTRANSFER, false);
    curl_setopt($ch, CURLOPT_TIMEOUT, 60);

    // HTTP/2 prior knowledge for http://, ALPN for https://
    $parsedUrl = parse_url($url);
    if (($parsedUrl['scheme'] ?? 'http') === 'https') {
        curl_setopt($ch, CURLOPT_HTTP_VERSION, CURL_HTTP_VERSION_2TLS);
    } else {
        curl_setopt($ch, CURLOPT_HTTP_VERSION, CURL_HTTP_VERSION_2_PRIOR_KNOWLEDGE);
    }

    // Headers
    $headers = [
        'Content-Type: application/json',
        'Authorization: Bearer ' . $openaiKey,
    ];
    if (isset($handoffData['test_pattern']) && $handoffData['test_pattern'] !== '') {
        $headers[] = 'X-Test-Pattern: ' . $handoffData['test_pattern'];
    }
    curl_setopt($ch, CURLOPT_HTTPHEADER, $headers);

    // Write callback to stream data to client
    curl_setopt($ch, CURLOPT_WRITEFUNCTION, function ($ch, $data) use ($clientSocket, &$bytesSent, &$buffer, &$success, &$streamEnded) {
        $buffer .= $data;

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
                $success = true;
                $streamEnded = true;
                return strlen($data);
            }

            $chunk = json_decode($sseData, true);
            $content = $chunk['choices'][0]['delta']['content'] ?? null;
            if ($content !== null && $content !== '') {
                $sseMsg = "data: " . json_encode(['content' => $content]) . "\n\n";
                $written = socketWriteAll($clientSocket, $sseMsg);
                if ($written === false) {
                    // Client disconnected
                    return 0; // Abort curl transfer
                }
                $bytesSent += $written;
            }
        }

        return strlen($data);
    });

    // Execute the request
    $result = curl_exec($ch);
    $httpCode = curl_getinfo($ch, CURLINFO_HTTP_CODE);
    curl_close($ch);

    // If stream didn't end with [DONE], send it now (for abort patterns)
    if (!$streamEnded && $httpCode === 200) {
        $doneMsg = "data: [DONE]\n\n";
        $written = socketWriteAll($clientSocket, $doneMsg);
        if ($written !== false) {
            $bytesSent += $written;
        }
        $success = true;
    }

    return [$success || $httpCode === 200, $bytesSent];
}

/**
 * Create the write callback for streaming SSE data to client.
 *
 * This is separated out so it can be used by both the single-request
 * and multiplexed curl implementations.
 */
function createWriteCallback(StreamContext $ctx): callable
{
    return function ($ch, $data) use ($ctx) {
        if ($ctx->aborted) {
            return 0; // Abort transfer
        }

        $ctx->sseBuffer .= $data;

        // Process SSE lines
        while (($pos = strpos($ctx->sseBuffer, "\n")) !== false) {
            $line = rtrim(substr($ctx->sseBuffer, 0, $pos), "\r");
            $ctx->sseBuffer = substr($ctx->sseBuffer, $pos + 1);

            if ($line === '' || !str_starts_with($line, 'data: ')) {
                continue;
            }
            $sseData = substr($line, 6);

            if ($sseData === '[DONE]') {
                $doneMsg = "data: [DONE]\n\n";
                $written = socketWriteAll($ctx->clientSocket, $doneMsg);
                if ($written !== false) {
                    $ctx->bytesSent += $written;
                }
                $ctx->success = true;
                $ctx->streamEnded = true;
                return strlen($data);
            }

            $chunk = json_decode($sseData, true);
            $content = $chunk['choices'][0]['delta']['content'] ?? null;
            if ($content !== null && $content !== '') {
                $sseMsg = "data: " . json_encode(['content' => $content]) . "\n\n";
                $written = socketWriteAll($ctx->clientSocket, $sseMsg);
                if ($written === false) {
                    // Client disconnected
                    $ctx->aborted = true;
                    return 0; // Abort curl transfer
                }
                $ctx->bytesSent += $written;
            }
        }

        return strlen($data);
    };
}

/**
 * Stream response from OpenAI API using HTTP/2 via curl_multi with true multiplexing.
 *
 * Uses the CurlMultiPool to share HTTP/2 connections across multiple streams.
 * This achieves ~100 streams per TCP connection (OpenAI's limit), reducing
 * backend connection overhead by 99%.
 *
 * HTTP/2 multiplexing works by:
 * - Multiple easy handles share a single curl_multi handle
 * - curl_multi maintains HTTP/2 connection pool internally
 * - CURLOPT_PIPEWAIT ensures new requests wait to reuse existing connections
 * - CURLPIPE_MULTIPLEX enables HTTP/2 stream multiplexing
 */
function streamFromOpenAIHttp2Multi(
    \Socket $clientSocket,
    array $handoffData,
    CurlMultiPool $pool,
    string $openaiKey,
    bool $benchmarkMode
): array {
    $ctx = new StreamContext($clientSocket);

    $prompt = $handoffData['prompt'] ?? 'Hello';
    $model = $handoffData['model'] ?? 'gpt-4o-mini';

    $body = json_encode([
        'model' => $model,
        'messages' => [['role' => 'user', 'content' => $prompt]],
        'stream' => true,
    ]);

    $url = $pool->getBaseUrl() . '/chat/completions';

    // Acquire from shared pool for HTTP/2 connection reuse
    [$multiHandle, $poolIndex] = $pool->acquire();
    if ($multiHandle === null) {
        if (!$benchmarkMode) {
            echo "HTTP/2 pool exhausted\n";
        }
        return [false, 0];
    }

    // Create curl easy handle
    $ch = curl_init();
    curl_setopt($ch, CURLOPT_URL, $url);
    curl_setopt($ch, CURLOPT_POST, true);
    curl_setopt($ch, CURLOPT_POSTFIELDS, $body);
    curl_setopt($ch, CURLOPT_RETURNTRANSFER, false);
    curl_setopt($ch, CURLOPT_TIMEOUT, 120);

    // Memory optimization: Reduce receive buffer from default 16KB to 4KB
    // SSE chunks are small, so we don't need large buffers
    curl_setopt($ch, CURLOPT_BUFFERSIZE, 4096);

    // HTTP/2 with multiplexing
    if ($pool->isSsl()) {
        curl_setopt($ch, CURLOPT_HTTP_VERSION, CURL_HTTP_VERSION_2TLS);
        // For self-signed certificates in testing/benchmarking
        // In production, use proper certificate verification
        curl_setopt($ch, CURLOPT_SSL_VERIFYPEER, false);
        curl_setopt($ch, CURLOPT_SSL_VERIFYHOST, false);
        // Enable SSL session ID caching for memory efficiency
        curl_setopt($ch, CURLOPT_SSL_SESSIONID_CACHE, true);
    } else {
        curl_setopt($ch, CURLOPT_HTTP_VERSION, CURL_HTTP_VERSION_2_PRIOR_KNOWLEDGE);
    }

    // Enable PIPEWAIT for efficient multiplexing - wait to reuse existing connection
    curl_setopt($ch, CURLOPT_PIPEWAIT, true);

    // Keep the connection alive for reuse
    curl_setopt($ch, CURLOPT_TCP_KEEPALIVE, true);
    curl_setopt($ch, CURLOPT_FORBID_REUSE, false);

    // Headers
    $headers = [
        'Content-Type: application/json',
        'Authorization: Bearer ' . $openaiKey,
    ];
    if (isset($handoffData['test_pattern']) && $handoffData['test_pattern'] !== '') {
        $headers[] = 'X-Test-Pattern: ' . $handoffData['test_pattern'];
    }
    curl_setopt($ch, CURLOPT_HTTPHEADER, $headers);

    // Write callback streams data to client via the context
    curl_setopt($ch, CURLOPT_WRITEFUNCTION, createWriteCallback($ctx));

    // Completion callback - called when transfer finishes
    // Signals the waiting coroutine via the completion channel
    $onComplete = function (bool $success, int $httpCode) use ($ctx) {
        // If stream didn't end with [DONE], send it now
        if (!$ctx->streamEnded && !$ctx->aborted && $httpCode === 200) {
            $doneMsg = "data: [DONE]\n\n";
            $written = @socketWriteAll($ctx->clientSocket, $doneMsg);
            if ($written !== false) {
                $ctx->bytesSent += $written;
            }
            $ctx->success = true;
        } elseif ($success && $httpCode === 200) {
            $ctx->success = true;
        }

        // Signal completion to waiting coroutine
        try {
            $ctx->completionChannel->push(true, 0);  // Non-blocking push
        } catch (\Throwable $e) {
            // Channel may be closed if coroutine timed out
        }
    };

    // Add to multi handle - this starts the transfer
    // The dedicated reader coroutine will poll and dispatch completions
    if (!$multiHandle->addHandle($ch, $onComplete)) {
        curl_close($ch);
        return [false, 0];
    }

    // Wait for completion via channel (blocks this coroutine, not others)
    // The dedicated reader coroutine drives the curl_multi and signals when done
    try {
        $ctx->completionChannel->pop(120000);  // 120 second timeout in milliseconds
    } catch (\Swow\ChannelException $e) {
        // Timeout or channel error - abort the transfer
        $ctx->aborted = true;
        $multiHandle->removeHandle($ch);
    }

    // Clean up easy handle only - multi handle stays alive for connection reuse
    curl_close($ch);
    return [$ctx->success, $ctx->bytesSent];
}

/**
 * Stream response from OpenAI API using HTTP/1.1 via curl.
 */
function streamFromOpenAIHttp1(
    \Socket $clientSocket,
    array $handoffData,
    string $openaiBase,
    string $openaiKey,
    bool $benchmarkMode
): array {
    $bytesSent = 0;
    $buffer = '';
    $success = false;
    $streamEnded = false;

    $prompt = $handoffData['prompt'] ?? 'Hello';
    $model = $handoffData['model'] ?? 'gpt-4o-mini';

    $body = json_encode([
        'model' => $model,
        'messages' => [['role' => 'user', 'content' => $prompt]],
        'stream' => true,
    ]);

    $url = $openaiBase . '/chat/completions';

    // Create curl handle
    $ch = curl_init();
    curl_setopt($ch, CURLOPT_URL, $url);
    curl_setopt($ch, CURLOPT_POST, true);
    curl_setopt($ch, CURLOPT_POSTFIELDS, $body);
    curl_setopt($ch, CURLOPT_RETURNTRANSFER, false);
    curl_setopt($ch, CURLOPT_TIMEOUT, 60);
    curl_setopt($ch, CURLOPT_HTTP_VERSION, CURL_HTTP_VERSION_1_1);

    // Headers
    $headers = [
        'Content-Type: application/json',
        'Authorization: Bearer ' . $openaiKey,
    ];
    if (isset($handoffData['test_pattern']) && $handoffData['test_pattern'] !== '') {
        $headers[] = 'X-Test-Pattern: ' . $handoffData['test_pattern'];
    }
    curl_setopt($ch, CURLOPT_HTTPHEADER, $headers);

    // Write callback to stream data to client
    curl_setopt($ch, CURLOPT_WRITEFUNCTION, function ($ch, $data) use ($clientSocket, &$bytesSent, &$buffer, &$success, &$streamEnded) {
        $buffer .= $data;

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
                $success = true;
                $streamEnded = true;
                return strlen($data);
            }

            $chunk = json_decode($sseData, true);
            $content = $chunk['choices'][0]['delta']['content'] ?? null;
            if ($content !== null && $content !== '') {
                $sseMsg = "data: " . json_encode(['content' => $content]) . "\n\n";
                $written = socketWriteAll($clientSocket, $sseMsg);
                if ($written === false) {
                    // Client disconnected
                    return 0; // Abort curl transfer
                }
                $bytesSent += $written;
            }
        }

        return strlen($data);
    });

    // Execute the request
    $result = curl_exec($ch);
    $httpCode = curl_getinfo($ch, CURLINFO_HTTP_CODE);
    curl_close($ch);

    // If stream didn't end with [DONE], send it now
    if (!$streamEnded && $httpCode === 200) {
        $doneMsg = "data: [DONE]\n\n";
        $written = socketWriteAll($clientSocket, $doneMsg);
        if ($written !== false) {
            $bytesSent += $written;
        }
        $success = true;
    }

    return [$success || $httpCode === 200, $bytesSent];
}

/**
 * Stream response to client
 *
 * @param \Socket $clientSocket The client socket received via SCM_RIGHTS
 */
function streamResponse(\Socket $clientSocket, array $handoffData, int $connId, Stats $stats): void
{
    global $benchmarkMode, $backendType, $openaiBase, $openaiKey, $http2Enabled, $sseDelayMs, $http2Pool;

    $stats->streamStart();
    $bytesSent = 0;
    $success = false;

    try {
        // Send SSE headers
        $written = socketWriteAll($clientSocket, SSE_HEADERS);
        if ($written === false) {
            $error = socket_last_error($clientSocket);
            throw new RuntimeException("Failed to write headers: " . socket_strerror($error));
        }
        $bytesSent += $written;

        if ($backendType === 'openai') {
            if ($http2Enabled && $http2Pool !== null) {
                // Use multiplexed HTTP/2 via curl_multi pool
                [$success, $apiBytes] = streamFromOpenAIHttp2Multi($clientSocket, $handoffData, $http2Pool, $openaiKey, $benchmarkMode);
            } elseif ($http2Enabled) {
                // Fallback to single-request HTTP/2
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
                "Connection $connId: Hello from Swow daemon!",
                "User ID: $userId",
                "Prompt: $promptDisplay",
            ];

            $index = 0;
            $sseDelayUs = $sseDelayMs * 1000;

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
                usleep($sseDelayUs);
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
                usleep($sseDelayUs);
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
echo "Swow Streaming Daemon starting\n";
echo "PHP " . PHP_VERSION . "\n";
echo "Swow " . (defined('Swow\\Extension::VERSION') ? \Swow\Extension::VERSION : 'unknown') . "\n";
echo "Socket: $socketPath\n";
echo "Backend: $backendType\n";

if ($backendType === 'openai') {
    echo "OpenAI base: $openaiBase\n";
    $url = parse_url($openaiBase);
    if ($http2Enabled) {
        if (($url['scheme'] ?? 'http') === 'https') {
            echo "HTTP mode: HTTP/2 ALPN (multiplexed)\n";
        } else {
            echo "HTTP mode: HTTP/2 h2c (prior knowledge, multiplexed)\n";
        }

        // Initialize HTTP/2 connection pool with curl_multi multiplexing
        // Default: 100 streams per handle (OpenAI's limit)
        // For non-OpenAI backends, increase via OPENAI_H2_STREAMS_PER_HANDLE to reduce
        // SSL connections and memory usage (e.g., 500 = 5x fewer SSL connections)
        $host = $url['host'] ?? 'localhost';
        $port = $url['port'] ?? (($url['scheme'] ?? 'http') === 'https' ? 443 : 80);
        $ssl = ($url['scheme'] ?? 'http') === 'https';
        $basePath = $url['path'] ?? '';

        $maxStreamsPerHandle = (int) (getenv('OPENAI_H2_STREAMS_PER_HANDLE') ?: 100);
        $maxStreamsPerHandle = max(10, min(1000, $maxStreamsPerHandle)); // Clamp 10-1000
        $poolSize = (int) ceil($maxConnections / $maxStreamsPerHandle);
        $http2Pool = new CurlMultiPool($host, $port, $ssl, $poolSize, $maxStreamsPerHandle, $basePath);
        echo "HTTP/2 pool: {$poolSize} multi handles x {$maxStreamsPerHandle} streams = " .
             ($poolSize * $maxStreamsPerHandle) . " max concurrent\n";
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

// Create Unix socket
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

// Active connections counter
$activeConnections = 0;
$connId = 0;

// Track active coroutines for graceful shutdown
$activeCoroutines = [];

// Signal handler coroutine
Coroutine::run(static function () use (&$running, $server, $stats, $benchmarkMode, $socketPath, &$activeCoroutines, &$http2Pool) {
    try {
        Signal::wait(SIGINT);
    } catch (SignalException) {
        // Signal received
    }
    $running = false;
    echo "\nReceived signal, shutting down...\n";

    // Kill active coroutines
    $killed = 0;
    foreach ($activeCoroutines as $coro) {
        if ($coro->isAvailable()) {
            $coro->kill();
            $killed++;
        }
    }
    if ($killed > 0 && !$benchmarkMode) {
        echo "Killed $killed active coroutines.\n";
    }

    // Close HTTP/2 connection pool
    if ($http2Pool !== null) {
        $poolStats = $http2Pool->getStats();
        if (!$benchmarkMode) {
            echo "Closing HTTP/2 pool ({$poolStats['handles']} handles, {$poolStats['active_streams']} active streams).\n";
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

    exit(0);
});

// Also handle SIGTERM
Coroutine::run(static function () use (&$running, $server, $stats, $benchmarkMode, $socketPath, &$activeCoroutines, &$http2Pool) {
    try {
        Signal::wait(SIGTERM);
    } catch (SignalException) {
        // Signal received
    }
    $running = false;
    echo "\nReceived SIGTERM, shutting down...\n";

    // Kill active coroutines
    $killed = 0;
    foreach ($activeCoroutines as $coro) {
        if ($coro->isAvailable()) {
            $coro->kill();
            $killed++;
        }
    }
    if ($killed > 0 && !$benchmarkMode) {
        echo "Killed $killed active coroutines.\n";
    }

    // Close HTTP/2 connection pool
    if ($http2Pool !== null) {
        $poolStats = $http2Pool->getStats();
        if (!$benchmarkMode) {
            echo "Closing HTTP/2 pool ({$poolStats['handles']} handles, {$poolStats['active_streams']} active streams).\n";
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

    exit(0);
});

// Main accept loop
while ($running) {
    // Try to accept connections (non-blocking)
    $conn = @socket_accept($server);
    if ($conn === false) {
        // No connection available, sleep briefly and retry
        usleep(1000); // 1ms
        continue;
    }

    if ($activeConnections >= $maxConnections) {
        socket_close($conn);
        continue;
    }

    // Debug: connection accepted (first few only)
    if (!$benchmarkMode && $connId < 5) {
        echo "Accepted connection!\n";
    }

    $connId++;
    $activeConnections++;
    $currentConnId = $connId;

    // Handle in new coroutine
    $coro = Coroutine::run(static function () use ($conn, $currentConnId, $stats, &$activeConnections, &$activeCoroutines) {
        $myCoro = Coroutine::getCurrent();
        try {
            handleConnection($conn, $currentConnId, $stats);
        } finally {
            $activeConnections--;
            unset($activeCoroutines[$myCoro->getId()]);
        }
    });
    $activeCoroutines[$coro->getId()] = $coro;
}

// Cleanup (only if still running - signal handlers do their own cleanup)
if ($running) {
    @socket_close($server);
    if (file_exists($socketPath)) {
        @unlink($socketPath);
    }

    if ($benchmarkMode) {
        $stats->printSummary();
    }
}
