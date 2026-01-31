#!/usr/bin/env python3
"""
Async streaming daemon for mod_socket_handoff using asyncio.

Receives client socket fd from Apache via SCM_RIGHTS and handles multiple
connections concurrently using coroutines. Unlike the synchronous test_daemon.py,
this can handle thousands of concurrent connections in a single process.

Usage:
    sudo python3 streaming_daemon_async.py [-s /var/run/streaming-daemon.sock] [-w 50]

The daemon will:
1. Listen on a Unix socket
2. Receive client socket fds via SCM_RIGHTS (in thread pool due to blocking recvmsg)
3. Spawn a coroutine per connection for concurrent handling
4. Stream SSE responses to clients

Performance characteristics:
- Single process, single thread event loop
- One coroutine per connection (~1-5 KB memory each)
- Expected: 10,000+ concurrent connections
- Limited by: Python GIL for CPU work, fd limits
"""

import argparse
import asyncio
import json
import os
import resource
import signal
import socket
import struct
import sys
import threading
import time
from concurrent.futures import ThreadPoolExecutor
from typing import Tuple

# Try to use uvloop for better performance (2-4x faster than default asyncio)
try:
    import uvloop
    UVLOOP_AVAILABLE = True
except ImportError:
    UVLOOP_AVAILABLE = False

# Default configuration
DEFAULT_SOCKET_PATH = "/var/run/streaming-daemon-py.sock"
DEFAULT_SOCKET_MODE = 0o660


class Stats:
    """Track connection statistics."""

    def __init__(self):
        self.connections_started = 0
        self.connections_completed = 0
        self.connections_failed = 0
        self.bytes_sent = 0
        self.active_connections = 0
        self.peak_streams = 0

    def stream_start(self):
        """Record stream start."""
        self.connections_started += 1
        self.active_connections += 1
        if self.active_connections > self.peak_streams:
            self.peak_streams = self.active_connections

    def stream_end(self, success: bool, bytes_sent: int):
        """Record stream end."""
        self.active_connections -= 1
        if success:
            self.connections_completed += 1
        else:
            self.connections_failed += 1
        self.bytes_sent += bytes_sent

    def print_summary(self):
        """Print benchmark summary."""
        print("\n=== Benchmark Summary ===")
        print(f"Peak concurrent streams: {self.peak_streams}")
        print(f"Total started: {self.connections_started}")
        print(f"Total completed: {self.connections_completed}")
        print(f"Total failed: {self.connections_failed}")
        print(f"Total bytes sent: {self.bytes_sent}")
        print("=========================")


stats = Stats()
benchmark_mode = False

# Thread-local storage for pre-allocated buffers to avoid allocation per connection
_thread_local = threading.local()

# Pre-compute control message size once
_CMSG_SIZE = socket.CMSG_SPACE(struct.calcsize("i"))


def _get_thread_buffer() -> bytearray:
    """Get or create a thread-local buffer for recvmsg."""
    buf = getattr(_thread_local, 'recv_buf', None)
    if buf is None:
        buf = bytearray(65536)
        _thread_local.recv_buf = buf
    return buf


def recv_fd_blocking(conn: socket.socket) -> Tuple[int, bytes]:
    """
    Receive a file descriptor from a Unix socket using SCM_RIGHTS.

    This is blocking and should be called via run_in_executor().
    Uses thread-local buffer to avoid allocation per connection.

    Returns (fd, data) tuple.
    """
    # Use thread-local buffer to avoid allocation per connection
    data_buf = _get_thread_buffer()

    # Receive with ancillary data
    msg, ancdata, flags, addr = conn.recvmsg(len(data_buf), _CMSG_SIZE)

    # Extract fd from ancillary data
    fd = -1
    for cmsg_level, cmsg_type, cmsg_data in ancdata:
        if cmsg_level == socket.SOL_SOCKET and cmsg_type == socket.SCM_RIGHTS:
            fd = struct.unpack("i", cmsg_data[: struct.calcsize("i")])[0]
            break

    if fd < 0:
        raise RuntimeError("No file descriptor received")

    return fd, msg


async def stream_response(
    client_fd: int, handoff_data: dict, conn_id: int, write_delay: float = 0.05
) -> None:
    """
    Stream an SSE response to the client.

    Uses asyncio for non-blocking writes and delays.
    """
    stats.stream_start()
    bytes_sent = 0
    success = False
    writer_file = None

    try:
        # Wrap fd in file object for writing (takes ownership of fd)
        writer_file = os.fdopen(client_fd, "wb", buffering=0)

        # Send HTTP response headers
        headers = (
            "HTTP/1.1 200 OK\r\n"
            "Content-Type: text/event-stream\r\n"
            "Cache-Control: no-cache\r\n"
            "Connection: close\r\n"
            "X-Accel-Buffering: no\r\n"
            "\r\n"
        )

        header_bytes = headers.encode("utf-8")
        # Direct write - with buffering=0, this goes straight to kernel buffer
        # and completes immediately for local/fast clients
        writer_file.write(header_bytes)
        bytes_sent += len(header_bytes)

        # Prepare response content
        user_id = handoff_data.get("user_id", 0)
        prompt = handoff_data.get("prompt", "unknown")
        prompt_display = prompt[:100] + "..." if len(prompt) > 100 else prompt

        lines = [
            f"Connection {conn_id}: Hello from async Python daemon!",
            f"User ID: {user_id}",
            f"Prompt: {prompt_display[:50]}",
            "This daemon uses asyncio for concurrent connection handling.",
            "Each connection runs as a lightweight coroutine.",
            "Expected capacity: 10,000+ concurrent connections.",
            "Memory per connection: ~1-5 KB.",
            "The Apache worker was freed immediately after handoff.",
            "Python's asyncio provides cooperative multitasking.",
            "The event loop schedules coroutines efficiently.",
            "Non-blocking I/O allows high concurrency.",
            "This is similar to Node.js or Go's concurrency model.",
            "This is message 13 of 18.",
            "This is message 14 of 18.",
            "This is message 15 of 18.",
            "This is message 16 of 18.",
            "This is message 17 of 18.",
            "[DONE]",
        ]

        # Stream each line with simulated delay
        for index, line in enumerate(lines):
            data = json.dumps({"content": line, "index": index})
            sse_msg = f"data: {data}\n\n"
            msg_bytes = sse_msg.encode("utf-8")

            try:
                # Direct write - with buffering=0, this goes straight to kernel buffer
                writer_file.write(msg_bytes)
                bytes_sent += len(msg_bytes)
            except (BrokenPipeError, OSError):
                break

            # Non-blocking delay using asyncio
            await asyncio.sleep(write_delay)

        success = True

    except Exception as e:
        if not benchmark_mode:
            print(f"[{conn_id}] Error: {e}", file=sys.stderr)

    finally:
        # Close the file object (also closes the underlying fd)
        if writer_file is not None:
            try:
                writer_file.close()
            except OSError:
                pass  # Ignore errors during close; OS will reclaim resources
        stats.stream_end(success, bytes_sent)


async def handle_handoff(
    apache_conn: socket.socket,
    conn_id: int,
    executor: ThreadPoolExecutor,
    write_delay: float,
) -> None:
    """
    Handle a single handoff from Apache.

    Receives fd in thread pool (blocking), then streams response async.
    """
    loop = asyncio.get_event_loop()

    try:
        # Set socket to blocking mode for recvmsg (asyncio sets it non-blocking)
        apache_conn.setblocking(True)

        # Receive fd in thread pool (recvmsg is blocking)
        client_fd, data = await loop.run_in_executor(
            executor, recv_fd_blocking, apache_conn
        )

        # Parse handoff data
        try:
            handoff_data = json.loads(data.decode("utf-8", errors="replace").strip("\x00")) if data else {}
        except json.JSONDecodeError:
            handoff_data = {"raw": data.decode("utf-8", errors="replace")}

        # Stream response as a coroutine (non-blocking)
        await stream_response(client_fd, handoff_data, conn_id, write_delay)

    except Exception as e:
        if not benchmark_mode:
            print(f"[{conn_id}] Handoff error: {e}", file=sys.stderr)
        # Note: Don't call stats.stream_end() here - stream_start() is only called
        # inside stream_response(), so calling stream_end() here would unbalance counters

    finally:
        apache_conn.close()


async def accept_loop(
    server: socket.socket, executor: ThreadPoolExecutor, write_delay: float
) -> None:
    """
    Main accept loop using asyncio.

    Accepts connections and spawns coroutines for each.
    """
    loop = asyncio.get_event_loop()
    conn_id = 0
    last_error_time = 0.0
    error_count = 0

    print(f"Async daemon ready, accepting connections...")

    while True:
        try:
            # Non-blocking accept using asyncio
            apache_conn, _ = await loop.sock_accept(server)
            conn_id += 1

            # Spawn coroutine for this connection (fire and forget)
            asyncio.create_task(
                handle_handoff(apache_conn, conn_id, executor, write_delay)
            )

        except asyncio.CancelledError:
            print("Accept loop cancelled")
            break
        except Exception as e:
            # Rate-limit error messages to avoid filling disk
            error_count += 1
            now = time.time()
            if now - last_error_time >= 1.0:  # Print at most once per second
                if error_count > 1:
                    print(f"Accept error: {e} (x{error_count} in last second)", file=sys.stderr)
                else:
                    print(f"Accept error: {e}", file=sys.stderr)
                last_error_time = now
                error_count = 0


async def main_async(args: argparse.Namespace) -> None:
    """Async main function."""
    socket_path = args.socket

    # Remove stale socket
    try:
        os.unlink(socket_path)
    except FileNotFoundError:
        pass  # Socket doesn't exist yet, nothing to remove

    # Create Unix socket
    server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    server.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    server.bind(socket_path)
    server.listen(args.backlog)
    server.setblocking(False)  # Required for asyncio

    # Set permissions
    os.chmod(socket_path, args.mode)

    print(f"Async streaming daemon (Python) listening on {socket_path}")
    print(f"Backlog: {args.backlog}, Workers: {args.workers}, Delay: {args.delay}ms")

    # Thread pool for blocking I/O (recvmsg and file writes)
    executor = ThreadPoolExecutor(max_workers=args.workers)

    # Handle shutdown signals
    loop = asyncio.get_event_loop()

    # Set our executor as the default so all run_in_executor(None, ...) calls use it
    # This is critical: without this, writes use the tiny default executor (~8 workers)
    loop.set_default_executor(executor)
    shutdown_event = asyncio.Event()

    def signal_handler():
        print("\nShutting down...")
        shutdown_event.set()

    for sig in (signal.SIGINT, signal.SIGTERM):
        loop.add_signal_handler(sig, signal_handler)

    # Start accept loop
    accept_task = asyncio.create_task(
        accept_loop(server, executor, args.delay / 1000.0)
    )

    # Wait for shutdown signal
    await shutdown_event.wait()

    # Cancel accept task
    accept_task.cancel()

    try:
        await accept_task
    except asyncio.CancelledError:
        pass  # Expected: accept loop was cancelled for shutdown

    # Cleanup
    executor.shutdown(wait=False)
    server.close()
    try:
        os.unlink(socket_path)
    except OSError:
        pass  # Socket file may already be removed

    # Print summary
    if benchmark_mode:
        stats.print_summary()
    else:
        print(f"Final stats: completed={stats.connections_completed} failed={stats.connections_failed}")


def main():
    parser = argparse.ArgumentParser(
        description="Async streaming daemon using asyncio"
    )
    parser.add_argument(
        "-s",
        "--socket",
        default=DEFAULT_SOCKET_PATH,
        help=f"Unix socket path (default: {DEFAULT_SOCKET_PATH})",
    )
    parser.add_argument(
        "-m",
        "--mode",
        type=lambda x: int(x, 8),
        default=DEFAULT_SOCKET_MODE,
        help=f"Socket permissions in octal (default: {oct(DEFAULT_SOCKET_MODE)})",
    )
    parser.add_argument(
        "-b",
        "--backlog",
        type=int,
        default=1024,
        help="Listen backlog (default: 1024)",
    )
    parser.add_argument(
        "-w",
        "--workers",
        type=int,
        default=500,
        help="Thread pool workers for blocking recvmsg (default: 500)",
    )
    parser.add_argument(
        "-d",
        "--delay",
        type=int,
        default=50,
        help="Delay between SSE messages in ms (default: 50)",
    )
    parser.add_argument(
        "--benchmark",
        action="store_true",
        help="Enable benchmark mode: quieter output, print summary on shutdown",
    )

    args = parser.parse_args()

    global benchmark_mode
    benchmark_mode = args.benchmark

    # Increase file descriptor limit to handle many concurrent connections
    try:
        soft, hard = resource.getrlimit(resource.RLIMIT_NOFILE)
        desired = max(100000, soft)
        if soft < desired:
            resource.setrlimit(resource.RLIMIT_NOFILE, (desired, max(hard, desired)))
            print(f"Increased fd limit from {soft} to {desired}")
    except (ValueError, resource.error) as e:
        print(f"Warning: could not increase fd limit: {e}")

    print(f"Python {sys.version}")
    if benchmark_mode:
        print("Benchmark mode enabled: quieter output, will print summary on shutdown")

    # Use uvloop if available for better performance
    if UVLOOP_AVAILABLE:
        print("Using uvloop for improved performance")
        uvloop.run(main_async(args))
    else:
        print("uvloop not available, using default asyncio event loop")
        asyncio.run(main_async(args))


if __name__ == "__main__":
    main()
