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

// Reduce fiber stack size for high concurrency (default is ~8MB virtual per fiber)
// 64KB is sufficient for simple streaming - reduces memory pressure significantly
ini_set('fiber.stack_size', '64k');

require __DIR__ . '/vendor/autoload.php';

use Amp\DeferredCancellation;
use Revolt\EventLoop;
use function Amp\async;
use function Amp\delay;

const DEFAULT_SOCKET_PATH = '/var/run/streaming-daemon-amp.sock';
const DEFAULT_SOCKET_MODE = 0660;
const DEFAULT_SSE_DELAY_MS = 50;

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
$options = getopt('s:m:d:hb', ['socket:', 'mode:', 'delay:', 'help', 'benchmark']);

if (isset($options['h']) || isset($options['help'])) {
    echo <<<HELP
AMPHP Async Streaming Daemon for mod_socket_handoff

Usage: php streaming_daemon.php [options]

Options:
  -s, --socket PATH    Unix socket path (default: /var/run/streaming-daemon-amp.sock)
  -m, --mode MODE      Socket permissions in octal (default: 0660)
  -d, --delay MS       Delay between SSE messages in ms (default: 50, use 3333 for 30s streams)
  -b, --benchmark      Enable benchmark mode: quieter output, print summary on shutdown
  -h, --help           Show this help

HELP;
    exit(0);
}

$socketPath = $options['s'] ?? $options['socket'] ?? DEFAULT_SOCKET_PATH;
$socketMode = octdec($options['m'] ?? $options['mode'] ?? '0660');
$sseDelayMs = (int)($options['d'] ?? $options['delay'] ?? DEFAULT_SSE_DELAY_MS);
$sseDelaySeconds = $sseDelayMs / 1000;  // Pre-calculate once
$benchmarkMode = isset($options['b']) || isset($options['benchmark']);

// Check for required extensions
if (!extension_loaded('sockets')) {
    die("Error: sockets extension is required\n");
}

// Increase file descriptor limit to handle many concurrent connections
if (function_exists('posix_setrlimit')) {
    $desiredLimit = 100000;
    if (posix_setrlimit(POSIX_RLIMIT_NOFILE, $desiredLimit, $desiredLimit)) {
        echo "Increased fd limit to $desiredLimit\n";
    } else {
        echo "Warning: could not increase fd limit\n";
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
        'buffer_size' => 4096,  // Handoff data is typically < 1KB
        'controllen' => socket_cmsg_space(SOL_SOCKET, SCM_RIGHTS, 1),
    ];

    $bytes = socket_recvmsg($socket, $message, 0);
    if ($bytes === false) {
        throw new RuntimeException('socket_recvmsg failed: ' . socket_strerror(socket_last_error($socket)));
    }

    // Extract the file descriptor from control message
    $fd = null;
    if (!empty($message['control'])) {
        foreach ($message['control'] as $cmsg) {
            if ($cmsg['level'] === SOL_SOCKET && $cmsg['type'] === SCM_RIGHTS) {
                $fd = $cmsg['data'][0];
                break;
            }
        }
    }

    // Extract the data from iov
    $data = '';
    if (!empty($message['iov']) && !empty($message['iov'][0])) {
        $data = $message['iov'][0];
    }

    return [$fd, $data];
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
        $written = @fwrite($stream, SSE_HEADERS);
        if ($written === false) {
            throw new RuntimeException("Failed to write headers");
        }
        $bytesSent += $written;

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
        global $sseDelaySeconds;

        // Send dynamic messages (3 messages with per-connection data)
        foreach ($dynamicLines as $line) {
            $data = json_encode(['content' => $line, 'index' => $index]);
            $sseMsg = "data: {$data}\n\n";

            $written = @fwrite($stream, $sseMsg);
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

            $written = @fwrite($stream, $sseMsg);
            if ($written === false) {
                break;
            }
            $bytesSent += $written;
            $index++;

            delay($sseDelaySeconds);
        }

        fclose($stream);
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

        if ($clientFd === null) {
            throw new RuntimeException("No file descriptor received");
        }

        // Parse handoff data
        $handoffData = [];
        if (!empty($data)) {
            $handoffData = json_decode($data, true) ?? [];
        }

        // Stream response using async I/O
        streamResponse($clientFd, $handoffData, $connId, $stats);

    } catch (Throwable $e) {
        if (!$benchmarkMode) {
            echo "[$connId] Handoff error: {$e->getMessage()}\n";
        }
        // Note: Don't call streamEnd() here - streamStart() is only called
        // inside streamResponse(), so calling streamEnd() here would unbalance counters
    } finally {
        socket_close($apacheConn);
    }
}

/**
 * Main function
 */
function main(string $socketPath, int $socketMode): void
{
    global $stats, $benchmarkMode;

    echo "AMPHP Async Streaming Daemon starting\n";
    echo "PHP " . PHP_VERSION . "\n";
    echo "Event loop: " . get_class(EventLoop::getDriver()) . "\n";
    echo "Socket: $socketPath\n";
    if ($benchmarkMode) {
        echo "Benchmark mode enabled: quieter output, will print summary on shutdown\n";
    }

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
    EventLoop::onReadable(socket_export_stream($server), function ($id, $serverStream) use (&$connId, $server, $stats, &$running) {
        if (!$running) {
            return;
        }

        // Accept connection (non-blocking)
        $apacheConn = @socket_accept($server);
        if ($apacheConn === false) {
            return; // No connection ready
        }

        $connId++;
        $currentConnId = $connId;

        // Handle connection in a new fiber (async)
        async(function () use ($apacheConn, $currentConnId, $stats) {
            handleConnection($apacheConn, $currentConnId, $stats);
        });
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
