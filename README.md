# mod_socket_handoff

Apache module that provides X-Accel-Redirect style socket handoff.

Allows PHP (or any handler) to authenticate a request and then hand off the
client connection to an external daemon for streaming responses. The Apache
worker is freed immediately after handoff.

## Credits

This module borrows code and concepts from:

- **[mod_proxy_fdpass](https://httpd.apache.org/docs/2.4/mod/mod_proxy_fdpass.html)** -
  The dummy socket swap trick that prevents Apache from closing the real client
  socket when the request completes. This is the key mechanism that allows the
  external daemon to take ownership of the connection.

- **[mod_xsendfile](https://tn123.org/mod_xsendfile/)** -
  The output filter pattern for intercepting response headers after the handler
  (PHP) has run. This allows the handoff decision to be made based on headers
  set by the application.

## Use Case

Streaming LLM responses:
1. Client sends request to Apache
2. PHP authenticates user, prepares request parameters
3. PHP sets `X-Socket-Handoff` header and exits
4. This module passes the client socket to a streaming daemon
5. Apache worker is freed immediately
6. Streaming daemon sends response directly to client

## How It Works

```
Client Request
     |
     v
+------------------------------------------+
|              Apache Worker               |
|  +----------+    +--------------------+  |
|  | mod_php  | -> | socket_handoff     |  |
|  | (auth)   |    | - sees header      |  |
|  +----------+    | - passes client fd |  |
|                  | - dummy socket     |  |
|                  +--------------------+  |
+------------------------------------------+
     |                      |
     | Worker freed         | SCM_RIGHTS
     v                      v
                  +-------------------+
                  | Streaming Daemon  |
                  | - receives fd     |
                  | - sends response  |
                  | - streams data    |
                  +-------------------+
                           |
                           v
                    Client receives
                    SSE stream
```

## Build

```bash
make
sudo make install
```

## Configure

### Debian/Ubuntu

```bash
make enable
sudo systemctl reload apache2
```

### Manual

Add to Apache config:

```apache
LoadModule socket_handoff_module modules/mod_socket_handoff.so

SocketHandoffEnabled On
SocketHandoffAllowedPrefix /var/run/
```

## Configuration Directives

### SocketHandoffEnabled

Enable or disable the filter.

```apache
SocketHandoffEnabled On|Off
```

Default: `On`

### SocketHandoffAllowedPrefix

Security setting: only allow socket paths under this prefix.

```apache
SocketHandoffAllowedPrefix /var/run/
```

Default: `/var/run/`

## PHP Usage

```php
<?php
// Authenticate user
$user = authenticate();

// Prepare handoff data
$data = json_encode([
    'user_id' => $user->getId(),
    'prompt' => $_POST['prompt'],
    'model' => 'gpt-4',
]);

// Tell Apache to hand off to streaming daemon
header('X-Socket-Handoff: /var/run/streaming-daemon.sock');
header('X-Handoff-Data: ' . $data);

// Exit immediately - module takes over
exit;
```

## Streaming Daemon

The daemon must:
1. Listen on a Unix socket
2. Receive client fd via `recvmsg()` with `SCM_RIGHTS`
3. Read the handoff data
4. Send HTTP response to the client fd
5. Close the fd when done

See `examples/` for implementations in:
- Go (`streaming_daemon.go`) - Multi-threaded, production-ready
- PHP (`streaming_daemon.php`) - Uses `socket_cmsg_space()` for SCM_RIGHTS
- Python (`test_daemon.py`) - Simple single-threaded test daemon

## Headers

### X-Socket-Handoff (required)

Path to the Unix socket of the daemon.

```
X-Socket-Handoff: /var/run/streaming-daemon.sock
```

### X-Handoff-Data (optional)

Data to pass to the daemon (typically JSON).

```
X-Handoff-Data: {"user_id":123,"prompt":"Hello"}
```

## Security

- Socket paths are validated against `SocketHandoffAllowedPrefix`
- Path traversal attacks (`../`) are blocked via `realpath()` check
- Headers are removed before any response is sent to client
- Only works for main requests (not subrequests)

## Limitations

1. **SSL/TLS**: The client fd is the raw socket. If Apache terminates SSL,
   the fd is the encrypted connection. The daemon would need the SSL context.
   **Workaround**: Terminate SSL at a load balancer.

2. **Keep-Alive**: After handoff, the connection is owned by the daemon.
   HTTP keep-alive for subsequent requests won't work.

3. **Logging**: Apache won't log the response (it handed off before responding).
   The daemon should log instead.

## License

Apache 2.0
