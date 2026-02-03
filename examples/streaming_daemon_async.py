#!/usr/bin/env python3
"""
Async streaming daemon for mod_socket_handoff using asyncio.

Receives client socket fd from Apache via SCM_RIGHTS and handles multiple
connections concurrently using coroutines. Unlike the synchronous test_daemon.py,
this can handle thousands of concurrent connections in a single process.

Usage:
    sudo python3 streaming_daemon_async.py [-s /var/run/streaming-daemon.sock] [-w 50]
    sudo python3 streaming_daemon_async.py --backend openai --openai-socket /tmp/api.sock

Environment variables (for OpenAI backend):
    OPENAI_API_KEY        - Required. API key for authentication
    OPENAI_API_BASE       - Optional. Base URL (default: https://api.openai.com/v1)
    OPENAI_API_SOCKET     - Optional. Unix socket path for API connections
    OPENAI_HTTP2_ENABLED  - Optional. Enable HTTP/2 multiplexing (default: true)
    OPENAI_INSECURE_SSL   - Optional. Skip SSL verification for self-signed certs (default: false)

The daemon will:
1. Listen on a Unix socket
2. Receive client socket fds via SCM_RIGHTS (in thread pool due to blocking recvmsg)
3. Spawn a coroutine per connection for concurrent handling
4. Stream SSE responses to clients (from OpenAI API or mock messages)

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

# OpenAI backend configuration (set from args/env)
openai_backend = False
openai_api_base = None
openai_api_key = None
openai_api_socket = None
openai_http2_enabled = True  # Default on, like Go/Rust daemons
openai_insecure_ssl = False  # Skip SSL verification (for self-signed certs)

# HTTP/2 support (optional, loaded on demand)
PYCURL_HTTP2_AVAILABLE = False
AsyncCurlMulti = None


def is_http2_enabled() -> bool:
    """Check if HTTP/2 is enabled via environment variable."""
    val = os.environ.get("OPENAI_HTTP2_ENABLED", "true").lower()
    return val not in ("false", "0", "no", "off")


def is_insecure_ssl() -> bool:
    """Check if SSL verification should be skipped (for self-signed certs)."""
    val = os.environ.get("OPENAI_INSECURE_SSL", "false").lower()
    return val in ("true", "1", "yes", "on")


def check_pycurl_http2(quiet: bool = False) -> bool:
    """
    Check if PycURL is available with HTTP/2 support.

    Args:
        quiet: If True, suppress warning messages (for benchmark mode).

    Returns True if pycurl is installed and libcurl has HTTP/2 support.
    """
    global PYCURL_HTTP2_AVAILABLE, AsyncCurlMulti
    try:
        from pycurl_http2 import AsyncCurlMulti as _AsyncCurlMulti, check_http2_support
        if check_http2_support():
            AsyncCurlMulti = _AsyncCurlMulti
            PYCURL_HTTP2_AVAILABLE = True
            return True
        else:
            if not quiet:
                print("Warning: pycurl available but libcurl lacks HTTP/2 support", file=sys.stderr)
            return False
    except ImportError as e:
        if not quiet:
            print(f"Note: HTTP/2 not available ({e})", file=sys.stderr)
        return False


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


class SharedTicker:
    """Shared ticker to avoid per-connection sleep timers under heavy load."""

    def __init__(self, interval: float):
        self.interval = interval
        self.current_tick = 0
        self._shutdown = False
        self._cond = asyncio.Condition()

    async def run(self) -> None:
        if self.interval <= 0:
            return
        try:
            while True:
                await asyncio.sleep(self.interval)
                async with self._cond:
                    self.current_tick += 1
                    self._cond.notify_all()
        except asyncio.CancelledError:
            async with self._cond:
                self._shutdown = True
                self._cond.notify_all()

    async def wait_for_next(self, last_tick: int) -> int:
        async with self._cond:
            if self.interval <= 0:
                return last_tick + 1
            await self._cond.wait_for(
                lambda: self._shutdown or self.current_tick > last_tick
            )
            if self._shutdown:
                raise asyncio.CancelledError
            return self.current_tick

    async def get_tick(self) -> int:
        async with self._cond:
            return self.current_tick


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

    # Extract fd from ancillary data FIRST, before checking truncation flags.
    # After recvmsg succeeds, any SCM_RIGHTS fds have been transferred to our
    # process and must be closed to avoid leaks.
    fd = -1
    for cmsg_level, cmsg_type, cmsg_data in ancdata:
        if cmsg_level == socket.SOL_SOCKET and cmsg_type == socket.SCM_RIGHTS:
            fd = struct.unpack("i", cmsg_data[: struct.calcsize("i")])[0]
            break

    # Check for truncation - if data exceeded buffer, JSON would be corrupt.
    # Close the fd first to prevent leaks.
    if flags & socket.MSG_TRUNC:
        if fd >= 0:
            os.close(fd)
        raise RuntimeError(f"Handoff data truncated (exceeded {len(data_buf)} byte buffer)")
    if flags & socket.MSG_CTRUNC:
        if fd >= 0:
            os.close(fd)
        raise RuntimeError("Control message truncated; fd may be corrupted")

    if fd < 0:
        raise RuntimeError("No file descriptor received")

    return fd, msg


async def stream_from_openai(
    client_writer: asyncio.StreamWriter,
    handoff_data: dict,
    conn_id: int,
) -> Tuple[int, bool]:
    """
    Stream response from OpenAI API to the client.

    Connects to OpenAI API (via Unix socket if configured), sends a streaming
    chat completion request, and forwards SSE chunks to the client.

    Uses asyncio StreamWriter for non-blocking client writes.

    NOTE: This has issues under high load (100+ concurrent connections) due to
    difficulties wrapping an SCM_RIGHTS-received fd in asyncio streams.
    Works for small-scale testing but not production benchmarks.
    Use Go/Rust/PHP daemons for production workloads.

    Returns (bytes_sent, success) tuple.
    """
    bytes_sent = 0
    success = False

    try:
        # Build the request
        prompt = handoff_data.get("prompt", "Hello")
        model = handoff_data.get("model", "gpt-4o-mini")
        max_tokens = handoff_data.get("max_tokens", 1024)

        request_body = json.dumps({
            "model": model,
            "messages": [{"role": "user", "content": prompt}],
            "max_tokens": max_tokens,
            "stream": True
        })

        # Parse API base URL
        from urllib.parse import urlparse
        parsed = urlparse(openai_api_base)
        host = parsed.netloc or "localhost"
        path = parsed.path or ""
        if not path.endswith("/"):
            path += "/"
        path += "chat/completions"

        # Connect to API (Unix socket or TCP)
        if openai_api_socket:
            reader, api_writer = await asyncio.open_unix_connection(openai_api_socket)
        else:
            port = parsed.port or (443 if parsed.scheme == "https" else 80)
            hostname = parsed.hostname or "localhost"
            if parsed.scheme == "https":
                import ssl
                ssl_context = ssl.create_default_context()
                reader, api_writer = await asyncio.open_connection(
                    hostname, port, ssl=ssl_context
                )
            else:
                reader, api_writer = await asyncio.open_connection(hostname, port)

        # Send HTTP request
        request = (
            f"POST {path} HTTP/1.1\r\n"
            f"Host: {host}\r\n"
            f"Content-Type: application/json\r\n"
            f"Authorization: Bearer {openai_api_key}\r\n"
            f"Content-Length: {len(request_body)}\r\n"
            f"Connection: close\r\n"
            f"\r\n"
            f"{request_body}"
        )
        api_writer.write(request.encode())
        await api_writer.drain()

        # Read HTTP response status line
        status_line = await reader.readline()
        if not status_line:
            raise RuntimeError("No response from API")

        # Skip response headers
        while True:
            header_line = await reader.readline()
            if header_line in (b"\r\n", b"\n", b""):
                break

        # Send SSE headers to client (non-blocking)
        headers = (
            "HTTP/1.1 200 OK\r\n"
            "Content-Type: text/event-stream\r\n"
            "Cache-Control: no-cache\r\n"
            "Connection: close\r\n"
            "X-Accel-Buffering: no\r\n"
            "\r\n"
        )
        header_bytes = headers.encode("utf-8")
        client_writer.write(header_bytes)
        await client_writer.drain()
        bytes_sent += len(header_bytes)

        # Read and forward SSE chunks
        while True:
            line = await reader.readline()
            if not line:
                break

            line_str = line.decode("utf-8", errors="replace").strip()
            if not line_str:
                continue

            if not line_str.startswith("data: "):
                continue

            json_str = line_str[6:]  # Skip "data: "
            if json_str == "[DONE]":
                break

            # Parse JSON and extract content
            try:
                chunk = json.loads(json_str)
                content = chunk.get("choices", [{}])[0].get("delta", {}).get("content", "")
                if content:
                    # Forward as SSE to client (non-blocking)
                    sse_data = json.dumps({"content": content})
                    sse_msg = f"data: {sse_data}\n\n"
                    msg_bytes = sse_msg.encode("utf-8")
                    client_writer.write(msg_bytes)
                    await client_writer.drain()
                    bytes_sent += len(msg_bytes)
            except json.JSONDecodeError:
                continue

        # Send [DONE] marker
        done_data = json.dumps({"content": "[DONE]"})
        done_msg = f"data: {done_data}\n\n"
        done_bytes = done_msg.encode("utf-8")
        client_writer.write(done_bytes)
        await client_writer.drain()
        bytes_sent += len(done_bytes)

        api_writer.close()
        await api_writer.wait_closed()
        success = True

    except Exception as e:
        if not benchmark_mode:
            print(f"[{conn_id}] OpenAI stream error: {e}", file=sys.stderr)

    return bytes_sent, success


async def async_writer_task(
    client_fd: int,
    context: "StreamContext",
    conn_id: int,
) -> Tuple[int, bool]:
    """
    Async task that writes queued chunks to the client.

    Consumes chunks from the StreamContext queue and writes them to the
    client socket using asyncio's non-blocking I/O. This decouples client
    writes from the curl callback, enabling HTTP/2 multiplexing.

    Args:
        client_fd: File descriptor for the client socket
        context: StreamContext with chunk queue
        conn_id: Connection ID for logging

    Returns:
        (bytes_sent, success) tuple
    """
    bytes_sent = 0
    success = False
    transport = None
    protocol = None

    try:
        # Create socket from fd and make it non-blocking.
        # IMPORTANT: We use detach() to transfer fd ownership to a new socket
        # that will be owned by the transport. This prevents double-close:
        # - Without detach: client_sock owns fd, transport.close() closes fd,
        #   then client_sock.__del__ tries to close fd again = double-close bug
        # - With detach: client_sock releases fd, new socket owns it, transport
        #   takes ownership of new socket, single clean close path
        temp_sock = socket.socket(fileno=client_fd)
        temp_sock.setblocking(False)
        fd = temp_sock.detach()  # Transfer ownership - temp_sock no longer owns fd

        # Create new socket for asyncio transport
        client_sock = socket.socket(fileno=fd)
        client_sock.setblocking(False)

        # Get event loop
        loop = asyncio.get_event_loop()

        # Create a protocol for the socket (needed for asyncio transport)
        # Use a simple protocol that just tracks connection state
        class ClientProtocol(asyncio.Protocol):
            def __init__(self):
                self.connected = True

            def connection_lost(self, exc):
                self.connected = False

        # Create transport and protocol from socket
        # Transport now owns client_sock and will close it properly
        transport, protocol = await loop.create_connection(
            ClientProtocol,
            sock=client_sock,
        )

        # Send HTTP response headers first
        headers = (
            "HTTP/1.1 200 OK\r\n"
            "Content-Type: text/event-stream\r\n"
            "Cache-Control: no-cache\r\n"
            "Connection: close\r\n"
            "X-Accel-Buffering: no\r\n"
            "\r\n"
        )
        header_bytes = headers.encode("utf-8")
        transport.write(header_bytes)
        bytes_sent += len(header_bytes)

        # Wait for headers to drain before continuing
        # Check write buffer - if too large, wait for it to drain
        write_buffer_limit = 65536  # 64KB
        while transport.get_write_buffer_size() > write_buffer_limit:
            await asyncio.sleep(0.001)
            if not protocol.connected:
                context.abort("client disconnected")
                return bytes_sent, False

        # Consume chunks from queue and write to client.
        # Use done_event with timeout for reliable completion detection,
        # since queue.put_nowait(None) can fail silently if queue is full.
        while True:
            # Check if done_event is set (stream complete or aborted)
            if context.done_event.is_set():
                # Drain any remaining queued chunks before exiting
                while True:
                    try:
                        chunk = context.queue.get_nowait()
                        if chunk is None:
                            break
                        # Process remaining chunk (same logic as below)
                        if chunk == b"[DONE]":
                            sse_msg = b"data: [DONE]\n\n"
                            transport.write(sse_msg)
                            bytes_sent += len(sse_msg)
                        else:
                            try:
                                chunk_data = json.loads(chunk.decode("utf-8"))
                                content = chunk_data.get("choices", [{}])[0].get("delta", {}).get("content", "")
                                if content:
                                    sse_data = json.dumps({"content": content})
                                    sse_msg = f"data: {sse_data}\n\n".encode("utf-8")
                                    transport.write(sse_msg)
                                    bytes_sent += len(sse_msg)
                            except (json.JSONDecodeError, UnicodeDecodeError):
                                pass
                    except asyncio.QueueEmpty:
                        break
                # Stream complete - check if it was successful or aborted
                success = not context.aborted
                break

            try:
                # Wait for next chunk with short timeout, then check done_event
                chunk = await asyncio.wait_for(context.queue.get(), timeout=1.0)
            except asyncio.TimeoutError:
                # Short timeout - loop back to check done_event
                continue

            if chunk is None:
                # Sentinel - stream complete
                success = True
                break

            if not protocol.connected:
                context.abort("client disconnected")
                break

            # Process the chunk - it's SSE data from the API
            if chunk == b"[DONE]":
                # Forward [DONE] marker
                sse_msg = b"data: [DONE]\n\n"
                transport.write(sse_msg)
                bytes_sent += len(sse_msg)
            else:
                # Parse JSON and extract content
                try:
                    chunk_data = json.loads(chunk.decode("utf-8"))
                    content = chunk_data.get("choices", [{}])[0].get("delta", {}).get("content", "")
                    if content:
                        sse_data = json.dumps({"content": content})
                        sse_msg = f"data: {sse_data}\n\n".encode("utf-8")
                        transport.write(sse_msg)
                        bytes_sent += len(sse_msg)
                except (json.JSONDecodeError, UnicodeDecodeError):
                    pass

            # Apply backpressure if write buffer is getting too large
            while transport.get_write_buffer_size() > write_buffer_limit:
                await asyncio.sleep(0.001)
                if not protocol.connected:
                    context.abort("client disconnected during backpressure")
                    break

    except Exception as e:
        if not benchmark_mode:
            print(f"[{conn_id}] Writer task error: {e}", file=sys.stderr)
        context.abort(str(e))

    finally:
        # Drain any remaining queue items to free memory and prevent leaks.
        # This is critical: if we exit early due to error/timeout, orphaned
        # queue items hold memory indefinitely.
        while True:
            try:
                context.queue.get_nowait()
            except asyncio.QueueEmpty:
                break

        if transport is not None:
            try:
                transport.close()
            except Exception:
                pass

    return bytes_sent, success


async def stream_from_openai_http2(
    client_fd: int,
    handoff_data: dict,
    conn_id: int,
    curl_multi: "AsyncCurlMulti",
) -> Tuple[int, bool]:
    """
    Stream response from OpenAI API using HTTP/2 multiplexing via PycURL.

    Uses queue-based decoupling to enable true HTTP/2 multiplexing:
    1. Curl callback queues chunks (returns immediately, non-blocking)
    2. Separate writer task consumes queue and writes to client asynchronously
    3. Bounded queue provides backpressure for slow clients

    This fixes the serialization bottleneck where blocking client writes
    in the curl callback prevented all HTTP/2 streams from making progress.

    Args:
        client_fd: File descriptor for the client socket
        handoff_data: Parsed JSON from X-Handoff-Data header
        conn_id: Connection ID for logging
        curl_multi: The shared AsyncCurlMulti instance

    Returns:
        (bytes_sent, success) tuple
    """
    import pycurl
    from pycurl_http2 import StreamContext, StreamingRequestQueued, configure_http2

    # Create stream context with bounded queue for backpressure
    loop = asyncio.get_event_loop()
    context = StreamContext(loop, maxsize=100)

    # Start writer task first - it will handle client writes asynchronously
    # while curl populates the queue
    writer_task = asyncio.create_task(
        async_writer_task(client_fd, context, conn_id)
    )

    try:
        # Build the request
        prompt = handoff_data.get("prompt", "Hello")
        model = handoff_data.get("model", "gpt-4o-mini")
        max_tokens = handoff_data.get("max_tokens", 1024)
        test_pattern = handoff_data.get("test_pattern", "")

        request_body = json.dumps({
            "model": model,
            "messages": [{"role": "user", "content": prompt}],
            "max_tokens": max_tokens,
            "stream": True
        })

        # Determine URL
        url = openai_api_base.rstrip("/") + "/chat/completions"

        # Create curl handle with queued request
        curl = pycurl.Curl()
        request = StreamingRequestQueued(curl, context)

        # Configure request
        curl.setopt(pycurl.URL, url)
        curl.setopt(pycurl.POST, 1)
        curl.setopt(pycurl.POSTFIELDS, request_body)
        curl.setopt(pycurl.WRITEFUNCTION, request.write_callback)
        curl.setopt(pycurl.HEADERFUNCTION, request.header_callback)

        # Headers
        http_headers = [
            "Content-Type: application/json",
            f"Authorization: Bearer {openai_api_key}",
        ]
        if test_pattern:
            http_headers.append(f"X-Test-Pattern: {test_pattern}")
        curl.setopt(pycurl.HTTPHEADER, http_headers)

        # Configure HTTP/2
        configure_http2(curl, url, insecure=openai_insecure_ssl)

        # Perform curl request asynchronously
        # This populates the queue via write_callback (non-blocking)
        await curl_multi.perform(curl, request)

        # Signal that curl transfer is complete
        context.signal_done()

        # Check HTTP status
        http_code = context.http_code
        if http_code != 200 and not benchmark_mode:
            print(f"[{conn_id}] HTTP/2 API returned {http_code}", file=sys.stderr)

        # Clean up curl handle
        curl.close()

    except Exception as e:
        if not benchmark_mode:
            print(f"[{conn_id}] HTTP/2 curl error: {e}", file=sys.stderr)
        context.abort(str(e))

    # Wait for writer task to complete and get its results
    try:
        bytes_sent, success = await writer_task
    except Exception as e:
        if not benchmark_mode:
            print(f"[{conn_id}] Writer task failed: {e}", file=sys.stderr)
        bytes_sent, success = 0, False

    # Success requires both curl success (HTTP 200) and writer success
    if context.http_code != 200:
        success = False

    return bytes_sent, success


async def stream_response(
    client_fd: int,
    handoff_data: dict,
    conn_id: int,
    ticker: SharedTicker,
    write_delay: float = 0.05,
    curl_multi: "AsyncCurlMulti" = None,
) -> None:
    """
    Stream an SSE response to the client.

    Uses asyncio for non-blocking writes and delays.
    If OpenAI backend is enabled, streams from the API instead of mock messages.
    """
    stats.stream_start()
    bytes_sent = 0
    success = False
    writer_file = None
    client_writer = None

    try:
        # Use OpenAI backend if configured
        if openai_backend:
            # Use HTTP/2 multiplexing if available and enabled
            if curl_multi is not None and openai_http2_enabled:
                bytes_sent, success = await stream_from_openai_http2(
                    client_fd, handoff_data, conn_id, curl_multi
                )
                # HTTP/2 function takes ownership of fd and closes it
            else:
                # HTTP/1.1 fallback using asyncio streams
                # Create asyncio StreamWriter from fd for non-blocking I/O
                client_sock = socket.socket(fileno=client_fd)
                client_sock.setblocking(False)
                # Open asyncio stream connection from the socket
                _, client_writer = await asyncio.open_connection(sock=client_sock)

                bytes_sent, success = await stream_from_openai(
                    client_writer, handoff_data, conn_id
                )

                # Close the writer (which closes the socket)
                client_writer.close()
                await client_writer.wait_closed()
        else:
            # Mock backend with hardcoded messages
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
            last_tick = await ticker.get_tick()
            for index, line in enumerate(lines):
                data = json.dumps({"content": line, "index": index})
                sse_msg = f"data: {data}\n\n"
                msg_bytes = sse_msg.encode("utf-8")

                try:
                    writer_file.write(msg_bytes)
                    bytes_sent += len(msg_bytes)
                except (BrokenPipeError, OSError):
                    break

                # Shared ticker to reduce per-connection timers under heavy load
                if write_delay > 0:
                    last_tick = await ticker.wait_for_next(last_tick)

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
    ticker: SharedTicker,
    write_delay: float,
    curl_multi: "AsyncCurlMulti" = None,
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
    except Exception as e:
        if not benchmark_mode:
            print(f"[{conn_id}] Handoff error: {e}", file=sys.stderr)
        # Note: Don't call stats.stream_end() here - stream_start() is only called
        # inside stream_response(), so calling stream_end() here would unbalance counters
        return
    finally:
        # Close Apache connection immediately after receiving fd.
        # The Apache socket is only needed for the brief SCM_RIGHTS handoff.
        # Keeping it open during streaming (10-30+ seconds) wastes file descriptors
        # and can cause fd exhaustion under high concurrency.
        apache_conn.close()

    # Parse handoff data
    try:
        handoff_data = json.loads(data.decode("utf-8", errors="replace").strip("\x00")) if data else {}
    except json.JSONDecodeError:
        handoff_data = {"raw": data.decode("utf-8", errors="replace")}

    # Stream response as a coroutine (non-blocking)
    await stream_response(client_fd, handoff_data, conn_id, ticker, write_delay, curl_multi)


async def accept_loop(
    server: socket.socket,
    executor: ThreadPoolExecutor,
    ticker: SharedTicker,
    write_delay: float,
    curl_multi: "AsyncCurlMulti" = None,
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
            while True:
                conn_id += 1
                asyncio.create_task(
                    handle_handoff(apache_conn, conn_id, executor, ticker, write_delay, curl_multi)
                )
                try:
                    apache_conn, _ = server.accept()
                except BlockingIOError:
                    break

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

    # Set our executor as the default for any future run_in_executor(None, ...) calls.
    # Current run_in_executor calls pass executor explicitly, so this is mainly for
    # consistency and future-proofing.
    loop.set_default_executor(executor)
    shutdown_event = asyncio.Event()

    def signal_handler():
        print("\nShutting down...")
        shutdown_event.set()

    for sig in (signal.SIGINT, signal.SIGTERM):
        loop.add_signal_handler(sig, signal_handler)

    # Start shared ticker for all streams (reduces per-connection timers)
    ticker = SharedTicker(args.delay / 1000.0)
    ticker_task = asyncio.create_task(ticker.run())

    # Initialize HTTP/2 curl multi handle if enabled and available
    curl_multi = None
    if openai_backend and openai_http2_enabled and PYCURL_HTTP2_AVAILABLE:
        curl_multi = AsyncCurlMulti(loop)
        if not benchmark_mode:
            print("HTTP/2 multiplexing enabled via PycURL")

    # Start accept loop
    accept_task = asyncio.create_task(
        accept_loop(server, executor, ticker, args.delay / 1000.0, curl_multi)
    )

    # Wait for shutdown signal
    await shutdown_event.wait()

    # Cancel accept task
    accept_task.cancel()
    ticker_task.cancel()

    try:
        await accept_task
    except asyncio.CancelledError:
        pass  # Expected: accept loop was cancelled for shutdown
    try:
        await ticker_task
    except asyncio.CancelledError:
        pass  # Expected: ticker task was cancelled for shutdown

    # Cleanup
    if curl_multi is not None:
        curl_multi.close()
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
    parser.add_argument(
        "--backend",
        choices=["mock", "openai"],
        default="mock",
        help="Backend to use: 'mock' for hardcoded messages, 'openai' for LLM API (default: mock)",
    )
    parser.add_argument(
        "--openai-base",
        default=os.environ.get("OPENAI_API_BASE", "https://api.openai.com/v1"),
        help="OpenAI API base URL (default: $OPENAI_API_BASE or https://api.openai.com/v1)",
    )
    parser.add_argument(
        "--openai-socket",
        default=os.environ.get("OPENAI_API_SOCKET"),
        help="Unix socket path for OpenAI API (default: $OPENAI_API_SOCKET)",
    )
    parser.add_argument(
        "--openai-key",
        default=os.environ.get("OPENAI_API_KEY"),
        help="OpenAI API key (default: $OPENAI_API_KEY)",
    )

    args = parser.parse_args()

    global benchmark_mode, openai_backend, openai_api_base, openai_api_key, openai_api_socket, openai_http2_enabled, openai_insecure_ssl
    benchmark_mode = args.benchmark

    # Configure OpenAI backend
    if args.backend == "openai":
        openai_backend = True
        openai_api_base = args.openai_base
        openai_api_key = args.openai_key
        openai_api_socket = args.openai_socket
        openai_insecure_ssl = is_insecure_ssl()

        if not openai_api_key:
            print("Error: --openai-key or OPENAI_API_KEY required for openai backend", file=sys.stderr)
            sys.exit(1)

        # Check HTTP/2 configuration
        openai_http2_enabled = is_http2_enabled()
        if openai_http2_enabled:
            # HTTP/2 requires TCP connection (not Unix socket) for multiplexing
            if openai_api_socket:
                if not benchmark_mode:
                    print("Note: HTTP/2 disabled when using Unix socket", file=sys.stderr)
                openai_http2_enabled = False
            else:
                # Check if PycURL with HTTP/2 is available - use return value!
                if not check_pycurl_http2(quiet=benchmark_mode):
                    openai_http2_enabled = False

        if openai_api_socket:
            print(f"OpenAI backend: {openai_api_base} (via unix:{openai_api_socket})")
        else:
            http2_status = "HTTP/2" if (openai_http2_enabled and PYCURL_HTTP2_AVAILABLE) else "HTTP/1.1"
            print(f"OpenAI backend: {openai_api_base} ({http2_status})")

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
