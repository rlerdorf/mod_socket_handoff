# mod_socket_handoff - Developer Guide

This document helps coding agents understand the Apache module for socket handoff.

## Overview

This Apache module allows PHP (or any handler) to authenticate a request and run any business logic needed, then hand off the client connection to an external daemon for streaming responses. The Apache worker is freed immediately, allowing efficient handling of long-running streams like LLM responses.

## Key Concept: SCM_RIGHTS File Descriptor Passing

The core mechanism is Unix domain socket fd passing via `SCM_RIGHTS`:
- Apache sends the client's TCP socket fd to a daemon over a Unix socket
- The daemon receives the fd and can read/write directly to the client
- Apache swaps in a dummy socket so it doesn't close the real connection
- The daemon now owns the connection and streams the response

## Architecture

```
Client <--TCP--> Apache Worker <--Unix Socket--> Streaming Daemon
                     |                                   |
                     | (hands off fd)                    | (owns connection)
                     v                                   v
               Worker freed                    Streams SSE to client
```

## Files

### Core Module

- **mod_socket_handoff.c** - The Apache module (~970 lines)
  - Output filter that intercepts `X-Socket-Handoff` header
  - Passes client fd to daemon via SCM_RIGHTS
  - Uses dummy socket trick to prevent Apache closing the real socket
  - Caches resolved prefix at config time for performance
  - Uses SOCK_NONBLOCK on Linux 2.6.27+ to reduce syscalls

### Configuration

- **apache/socket_handoff.load** - Apache module loader
- **apache/socket_handoff.conf** - Default configuration

### Example Daemons

- **examples/streaming_daemon.go** - Production-ready Go daemon with goroutines
- **examples/streaming_daemon.php** - PHP daemon using `socket_cmsg_space()`
- **examples/test_daemon.py** - Simple Python daemon for testing
- **examples/fdrecv.c** - Minimal C daemon that execs any handler

## Key Functions in mod_socket_handoff.c

### `socket_handoff_output_filter()` (line 613)
The main output filter. Runs after PHP generates response:
1. Checks for `X-Socket-Handoff` header
2. Validates socket path against allowed prefix
3. Gets client fd from connection
4. Connects to daemon with retry and sends fd via SCM_RIGHTS
5. Swaps to dummy socket
6. Marks connection as aborted

### `send_fd_with_data()` (line 179)
Sends file descriptor over Unix socket using `sendmsg()` with `SCM_RIGHTS`.
Uses poll() to enforce send timeout; handles EAGAIN/EWOULDBLOCK as timeout errors:
```c
cmsg->cmsg_level = SOL_SOCKET;
cmsg->cmsg_type = SCM_RIGHTS;
cmsg->cmsg_len = CMSG_LEN(sizeof(int));
memcpy(CMSG_DATA(cmsg), &fd_to_send, sizeof(int));
```

### `connect_to_socket()` (line 266)
Connects to Unix socket using non-blocking connect with poll() timeout.
Returns a non-blocking socket ready for sending.

### `connect_with_retry()` (line 429)
Wraps connect_to_socket() with retry logic for transient errors (ENOENT, ECONNREFUSED).
Uses exponential backoff: 10ms, 20ms, 40ms, etc.

### `create_dummy_socket()` (line 482)
The trick from mod_proxy_fdpass - creates a dummy socket and swaps it into the connection config so Apache closes the dummy instead of the real client socket.

### `validate_socket_path()` (line 510)
Security check using `realpath()` to prevent path traversal attacks.

## Configuration Directives

```apache
SocketHandoffEnabled On|Off              # Enable/disable (default: On)
SocketHandoffAllowedPrefix /var/run/     # Security prefix for socket paths
SocketHandoffConnectTimeoutMs 500        # Daemon connect timeout (default: 500ms)
SocketHandoffSendTimeoutMs 500           # Daemon send timeout (default: 500ms)
SocketHandoffMaxRetries 3                # Retries for transient errors (default: 3)
```

### Timeout Directives

- **SocketHandoffConnectTimeoutMs** - Timeout for establishing connection to daemon socket.
  Uses non-blocking connect with poll(). Range: 1-60000ms.

- **SocketHandoffSendTimeoutMs** - Timeout for sending the fd to daemon via sendmsg().
  Uses poll() to wait for socket buffer space before sending on the non-blocking socket.
  Critical for production to prevent worker starvation. Range: 1-60000ms.

### Retry Directive

- **SocketHandoffMaxRetries** - Number of retries for transient connection errors.
  Retries on ENOENT (socket doesn't exist) and ECONNREFUSED (daemon not listening).
  Uses exponential backoff: 10ms, 20ms, 40ms, etc. Set to 0 to disable retries.
  Range: 0-10.

## Headers

- **X-Socket-Handoff** (required) - Unix socket path for the daemon
- **X-Handoff-Data** (optional) - JSON data to pass to daemon (user_id, prompt, etc.)

## PHP Usage Pattern

```php
<?php
// 1. Authenticate and prepare
$user = authenticate();
$data = json_encode(['user_id' => $user->id, 'prompt' => $_POST['prompt']]);

// 2. Set handoff headers
header('X-Socket-Handoff: /var/run/streaming-daemon.sock');
header('X-Handoff-Data: ' . $data);

// 3. Exit - module takes over
exit;
```

## Daemon Requirements

A receiving daemon must:

1. **Listen on Unix socket** - Path must match what PHP sends
2. **Receive fd via recvmsg()** - Use SCM_RIGHTS to extract the fd
3. **Parse handoff data** - JSON sent along with the fd
4. **Send HTTP response** - Full response including headers (HTTP/1.1 200 OK...)
5. **Stream content** - SSE, chunked, or regular response
6. **Close fd when done** - Daemon owns the connection

### Go Daemon Pattern (examples/streaming_daemon.go)

```go
// Receive fd
msgs, _ := syscall.ParseSocketControlMessage(oob[:oobn])
fds, _ := syscall.ParseUnixRights(&msgs[0])
clientFd := fds[0]

// IMPORTANT: os.NewFile takes ownership - use file.Close(), not syscall.Close()
file := os.NewFile(uintptr(clientFd), "client")
defer file.Close()

// Send HTTP response
writer := bufio.NewWriter(file)
fmt.Fprintf(writer, "HTTP/1.1 200 OK\r\n")
fmt.Fprintf(writer, "Content-Type: text/event-stream\r\n\r\n")
```

### PHP Daemon Pattern (examples/streaming_daemon.php)

```php
// Key: Use socket_cmsg_space() for proper buffer size
$message = [
    'name' => [],
    'buffer_size' => 4096,
    'controllen' => socket_cmsg_space(SOL_SOCKET, SCM_RIGHTS, 1),
];
socket_recvmsg($socket, $message, 0);
$client_fd = $message['control'][0]['data'][0];
```

## Build & Install

```bash
make                    # Build with apxs
sudo make install       # Install to Apache modules
make enable             # Enable on Debian/Ubuntu
sudo systemctl reload apache2
```

## Testing

1. Start a daemon:
   ```bash
   cd examples
   go build -o streaming-daemon streaming_daemon.go
   sudo ./streaming-daemon
   ```

2. Create test PHP endpoint:
   ```php
   header('X-Socket-Handoff: /var/run/streaming-daemon.sock');
   header('X-Handoff-Data: {"prompt":"test"}');
   exit;
   ```

3. Test with curl:
   ```bash
   curl http://localhost/your-endpoint
   ```

## Common Issues

### "bad file descriptor" mid-stream (Go)
**Cause**: `os.NewFile()` takes ownership of fd. If the file object is garbage collected, the fd is closed.
**Fix**: Keep file in scope with `defer file.Close()`, don't use `syscall.Close()`.

### Socket permission denied
**Cause**: Apache (www-data) can't connect to daemon socket.
**Fix**: Set proper ownership and permissions on the socket. For example:
- `chown www-data:www-data /var/run/streaming-daemon.sock` (if daemon runs as www-data)
- Or add Apache user to daemon's group and use `chmod 660`
- Avoid `chmod 666` as it allows any local user to connect, bypassing authentication

### Module not loading after reboot
**Fix**: Run `sudo a2enmod socket_handoff` and reload Apache.

### Path traversal blocked
**Cause**: Socket path doesn't start with allowed prefix.
**Fix**: Ensure path starts with `SocketHandoffAllowedPrefix` (default: /var/run/).

### 503 errors during daemon restart
**Cause**: Daemon socket briefly unavailable during restart.
**Fix**: `SocketHandoffMaxRetries 3` (default) handles this with exponential backoff.
Increase if daemon restarts take longer than ~70ms (10+20+40ms).

### Send timeout errors (ETIMEDOUT)
**Cause**: Daemon socket buffer full, sendmsg() timed out.
**Fix**: Increase `SocketHandoffSendTimeoutMs` or investigate why daemon isn't
reading from its socket fast enough. May indicate daemon is overloaded.

## Performance Optimizations

The module includes several optimizations for high-traffic deployments:

1. **Prefix caching** - The allowed socket prefix is resolved once at config time
   via `socket_handoff_post_config()`. This eliminates 2-3 `apr_filepath_merge()`
   calls per request.

2. **SOCK_NONBLOCK** - On Linux 2.6.27+, the daemon socket is created with
   `SOCK_NONBLOCK` to eliminate one `fcntl()` syscall per connection.

3. **Lower default timeout** - Default connect timeout is 500ms (was 2000ms).
   For localhost Unix sockets, 500ms is generous. Lower timeouts prevent worker
   starvation when the daemon is slow.

4. **poll()-based send timeout** - Before sending, poll() is used to wait for
   socket buffer space with the configured timeout. This prevents sendmsg() from
   blocking indefinitely if the daemon's socket buffer is full. Critical for
   production - without it, a slow daemon can block Apache workers indefinitely.

5. **Retry with exponential backoff** - Transient errors (ENOENT, ECONNREFUSED)
   are retried with exponential backoff (10ms, 20ms, 40ms). This handles daemon
   restarts gracefully without failing all in-flight requests.

6. **Non-blocking throughout** - The socket stays non-blocking after connect.
   Combined with the poll()-based connect and send timeouts, this bounds how long
   any operation can block the worker to the configured timeout.

## Security Considerations

1. **Socket prefix validation** - Only sockets under allowed prefix can be used
2. **Path traversal prevention** - `realpath()` check blocks `../` attacks
3. **Headers removed** - X-Socket-Handoff headers are stripped before response
4. **Main requests only** - Subrequests are not handled

## Credits

- **mod_proxy_fdpass** - Dummy socket swap trick
- **mod_xsendfile** - Output filter pattern for header interception

## License

Apache 2.0
