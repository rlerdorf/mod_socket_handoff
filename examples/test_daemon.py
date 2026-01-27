#!/usr/bin/env python3
"""
Simple test daemon for mod_proxy_fdpass_filter.

Receives client socket fd from Apache, sends a simple SSE response.
For testing only - use Go version for production.

Usage:
    sudo python3 test_daemon.py

The daemon will:
1. Listen on /var/run/streaming-daemon.sock
2. Receive client socket fd via SCM_RIGHTS
3. Send HTTP response with SSE stream
4. Close the connection
"""

import os
import socket
import struct
import json
import time
import array

SOCKET_PATH = "/var/run/streaming-daemon.sock"


def recv_fd(conn):
    """
    Receive a file descriptor from a Unix socket using SCM_RIGHTS.
    Returns (fd, data) tuple.
    """
    # Buffer for data (handoff JSON)
    data_buf = bytearray(4096)

    # Buffer for ancillary data (the fd)
    ancillary_buf = bytearray(socket.CMSG_SPACE(struct.calcsize('i')))

    # Receive with ancillary data
    msg, ancdata, flags, addr = conn.recvmsg(
        len(data_buf),
        socket.CMSG_SPACE(struct.calcsize('i'))
    )

    # Extract fd from ancillary data
    fd = -1
    for cmsg_level, cmsg_type, cmsg_data in ancdata:
        if cmsg_level == socket.SOL_SOCKET and cmsg_type == socket.SCM_RIGHTS:
            fd = struct.unpack('i', cmsg_data[:struct.calcsize('i')])[0]
            break

    if fd < 0:
        raise RuntimeError("No file descriptor received")

    return fd, msg.decode('utf-8', errors='replace').strip('\x00')


def handle_client(client_fd, handoff_data):
    """
    Handle the client connection.
    Sends HTTP response headers and SSE data.
    """
    # Parse handoff data
    try:
        data = json.loads(handoff_data) if handoff_data else {}
    except json.JSONDecodeError:
        data = {"raw": handoff_data}

    print(f"  Handoff data: {data}")

    # Create a file object from the fd for easier I/O
    client_file = os.fdopen(client_fd, 'wb', buffering=0)

    try:
        # Send HTTP response headers
        response = (
            "HTTP/1.1 200 OK\r\n"
            "Content-Type: text/event-stream\r\n"
            "Cache-Control: no-cache\r\n"
            "Connection: close\r\n"
            "\r\n"
        )
        client_file.write(response.encode('utf-8'))

        # Send SSE events
        messages = [
            "Hello from streaming daemon!",
            f"User ID: {data.get('user_id', 'unknown')}",
            f"Prompt: {data.get('prompt', 'none')[:50]}",
            "Streaming response...",
            "This is a test of the fd passing mechanism.",
            "The Apache worker has been freed.",
            "This response is coming directly from the daemon.",
            "[DONE]"
        ]

        for i, msg in enumerate(messages):
            event_data = json.dumps({"content": msg, "index": i})
            sse_msg = f"data: {event_data}\n\n"
            client_file.write(sse_msg.encode('utf-8'))
            print(f"  Sent: {msg[:40]}...")
            time.sleep(0.5)  # Simulate streaming delay

    except BrokenPipeError:
        print("  Client disconnected")
    except Exception as e:
        print(f"  Error: {e}")
    finally:
        client_file.close()


def main():
    # Remove stale socket
    try:
        os.unlink(SOCKET_PATH)
    except FileNotFoundError:
        pass

    # Create Unix socket
    server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    server.bind(SOCKET_PATH)
    server.listen(5)

    # Set permissions so Apache can connect
    os.chmod(SOCKET_PATH, 0o666)

    print(f"Test daemon listening on {SOCKET_PATH}")
    print("Press Ctrl+C to stop\n")

    try:
        while True:
            conn, addr = server.accept()
            print(f"Connection from Apache")

            try:
                # Receive client fd and handoff data
                client_fd, handoff_data = recv_fd(conn)
                print(f"  Received client fd: {client_fd}")

                # Handle the client
                handle_client(client_fd, handoff_data)
                print(f"  Done\n")

            except Exception as e:
                print(f"  Error: {e}\n")
            finally:
                conn.close()

    except KeyboardInterrupt:
        print("\nShutting down...")
    finally:
        server.close()
        try:
            os.unlink(SOCKET_PATH)
        except:
            pass


if __name__ == "__main__":
    main()
