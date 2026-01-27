#!/usr/bin/env php
<?php
/**
 * Streaming Daemon for mod_socket_handoff (PHP version)
 *
 * Receives client connections from Apache via SCM_RIGHTS file descriptor
 * passing and streams responses directly to clients.
 *
 * Requirements:
 *   - PHP 7.4+ with sockets extension
 *   - pcntl extension (for signal handling)
 *
 * Run:
 *   sudo php streaming_daemon.php
 *
 * Note: This is a single-process daemon. For production, consider using
 * pcntl_fork() for each connection or a process manager like supervisord.
 */

declare(strict_types=1);

const SOCKET_PATH = '/var/run/streaming-daemon.sock';

/**
 * Receive a file descriptor and data via SCM_RIGHTS
 *
 * Uses socket_cmsg_space() to properly allocate the control message buffer.
 */
function receive_fd(Socket $socket): array
{
    $message = [
        'name' => [],
        'buffer_size' => 4096,
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
                // The data contains a resource/socket
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
 * Stream a simulated LLM response to the client
 *
 * @param resource|Socket|int $client_fd The client socket (resource, Socket object, or fd int)
 */
function stream_response($client_fd, array $handoff_data): void
{
    // Handle different types of client_fd
    if ($client_fd instanceof Socket) {
        // Convert Socket to stream
        $stream = socket_export_stream($client_fd);
        if ($stream === false) {
            error_log("Failed to export Socket to stream");
            return;
        }
    } elseif (is_resource($client_fd)) {
        // Already a resource/stream
        $stream = $client_fd;
    } elseif (is_int($client_fd)) {
        // File descriptor integer
        $stream = fopen("php://fd/{$client_fd}", 'w');
        if ($stream === false) {
            error_log("Failed to open fd {$client_fd} as stream");
            return;
        }
    } else {
        error_log("Unknown client_fd type: " . gettype($client_fd));
        return;
    }

    // Disable buffering
    stream_set_write_buffer($stream, 0);

    // Send HTTP response headers
    $headers = "HTTP/1.1 200 OK\r\n";
    $headers .= "Content-Type: text/event-stream\r\n";
    $headers .= "Cache-Control: no-cache\r\n";
    $headers .= "Connection: close\r\n";
    $headers .= "X-Accel-Buffering: no\r\n";
    $headers .= "\r\n";
    fwrite($stream, $headers);
    fflush($stream);

    // Prepare the response content
    $user_id = $handoff_data['user_id'] ?? 0;
    $prompt = $handoff_data['prompt'] ?? 'unknown';
    $prompt_display = strlen($prompt) > 100 ? substr($prompt, 0, 100) . '...' : $prompt;

    $lines = [
        "Hello! I received your request.",
        "",
        "User ID: {$user_id}",
        "Prompt: \"{$prompt_display}\"",
        "",
        "Let me help you with that. Here's my response:",
        "",
        "The Apache worker that handled your initial request has already been freed. " .
            "This response is being streamed directly from a lightweight PHP daemon " .
            "that received the client socket via SCM_RIGHTS file descriptor passing.",
        "",
        "This architecture allows:",
        "1. Heavy PHP processes to handle authentication",
        "2. Lightweight daemons to handle long-running streams",
        "3. Better resource utilization under load",
        "",
        "The connection handoff is transparent to the client - " .
            "you see a single HTTP request with a streaming response.",
        "",
        "This is the end of my response. Have a great day!",
    ];

    // Stream each line with simulated LLM delays
    foreach ($lines as $index => $line) {
        $data = json_encode(['content' => $line, 'index' => $index]);
        fwrite($stream, "data: {$data}\n\n");
        fflush($stream);

        // Simulate LLM thinking time - varies by line length
        $delay = 100000 + (strlen($line) * 10000); // microseconds
        if ($delay > 500000) {
            $delay = 500000;
        }
        usleep($delay);
    }

    // Send done marker
    fwrite($stream, "data: [DONE]\n\n");
    fflush($stream);

    fclose($stream);
}

/**
 * Handle a connection from Apache
 */
function handle_connection(Socket $conn): void
{
    try {
        [$client_fd, $data] = receive_fd($conn);

        if ($client_fd === null) {
            error_log("No file descriptor received");
            return;
        }

        $fd_type = is_resource($client_fd) ? 'resource' : (($client_fd instanceof Socket) ? 'Socket' : gettype($client_fd));
        echo "Received handoff: fd_type={$fd_type}, data_len=" . strlen($data) . "\n";

        // Parse handoff data
        $handoff_data = [];
        if (!empty($data)) {
            $handoff_data = json_decode($data, true) ?? [];
        }

        $user_id = $handoff_data['user_id'] ?? 'unknown';
        $prompt = $handoff_data['prompt'] ?? 'unknown';
        $prompt_short = strlen($prompt) > 50 ? substr($prompt, 0, 50) . '...' : $prompt;
        echo "  user_id={$user_id}, prompt=\"{$prompt_short}\"\n";

        // Stream response to client
        stream_response($client_fd, $handoff_data);

        // Close the client socket/resource
        if ($client_fd instanceof Socket) {
            socket_close($client_fd);
        } elseif (is_resource($client_fd)) {
            fclose($client_fd);
        }

        echo "  Connection closed\n";

    } catch (Throwable $e) {
        error_log("Error handling connection: " . $e->getMessage());
    }
}

/**
 * Main daemon loop
 */
function main(): void
{
    // Check for required extensions
    if (!extension_loaded('sockets')) {
        die("Error: sockets extension is required\n");
    }

    // Remove stale socket
    if (file_exists(SOCKET_PATH)) {
        unlink(SOCKET_PATH);
    }

    // Create Unix socket
    $server = socket_create(AF_UNIX, SOCK_STREAM, 0);
    if ($server === false) {
        die("socket_create failed: " . socket_strerror(socket_last_error()) . "\n");
    }

    // Bind to socket path
    if (!socket_bind($server, SOCKET_PATH)) {
        die("socket_bind failed: " . socket_strerror(socket_last_error($server)) . "\n");
    }

    // Set permissions so Apache can connect
    chmod(SOCKET_PATH, 0666);

    // Listen for connections
    if (!socket_listen($server, 5)) {
        die("socket_listen failed: " . socket_strerror(socket_last_error($server)) . "\n");
    }

    echo "Streaming daemon (PHP) listening on " . SOCKET_PATH . "\n";

    // Set up signal handling for graceful shutdown
    if (extension_loaded('pcntl')) {
        pcntl_signal(SIGINT, function () use ($server) {
            echo "\nShutting down...\n";
            socket_close($server);
            if (file_exists(SOCKET_PATH)) {
                unlink(SOCKET_PATH);
            }
            exit(0);
        });
        pcntl_signal(SIGTERM, function () use ($server) {
            echo "\nShutting down...\n";
            socket_close($server);
            if (file_exists(SOCKET_PATH)) {
                unlink(SOCKET_PATH);
            }
            exit(0);
        });
    }

    // Accept connections
    while (true) {
        // Check for signals if pcntl is available
        if (extension_loaded('pcntl')) {
            pcntl_signal_dispatch();
        }

        // Accept with timeout so we can check signals
        $read = [$server];
        $write = null;
        $except = null;
        $changed = socket_select($read, $write, $except, 1);

        if ($changed === false) {
            error_log("socket_select failed: " . socket_strerror(socket_last_error()));
            continue;
        }

        if ($changed === 0) {
            continue; // Timeout, check signals and loop
        }

        $conn = socket_accept($server);
        if ($conn === false) {
            error_log("socket_accept failed: " . socket_strerror(socket_last_error($server)));
            continue;
        }

        handle_connection($conn);
        socket_close($conn);
    }
}

// Run the daemon
main();
