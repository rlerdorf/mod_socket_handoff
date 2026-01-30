/*
 * mod_socket_handoff.c
 *
 * Apache module that provides X-Accel-Redirect style socket handoff.
 * Allows PHP to authenticate a request and then hand off the client
 * connection to an external daemon for streaming responses.
 *
 * This module borrows code and concepts from:
 * - mod_proxy_fdpass: Dummy socket swap trick to prevent Apache from
 *   closing the real client socket
 * - mod_xsendfile: Output filter pattern for intercepting response headers
 *
 * Usage:
 *   PHP sets header: X-Socket-Handoff: /var/run/streaming.sock
 *   Optional data:   X-Handoff-Data: {"user_id":123,"prompt":"..."}
 *
 * The module:
 *   1. Intercepts these headers in an output filter
 *   2. Connects to the specified Unix socket
 *   3. Passes client fd + data via SCM_RIGHTS
 *   4. Creates dummy socket so Apache doesn't close real connection
 *   5. Returns immediately - Apache worker is freed
 *
 * The external daemon:
 *   1. Receives the client socket fd via recvmsg()
 *   2. Sends HTTP response headers
 *   3. Streams response body (e.g., SSE from LLM)
 *   4. Closes the connection when done
 *
 * Configuration:
 *   SocketHandoffEnabled On|Off
 *   SocketHandoffAllowedPrefix /var/run/
 *
 * Build:
 *   apxs -c mod_socket_handoff.c
 *   sudo apxs -i -a mod_socket_handoff.la
 *
 * License: Apache 2.0
 */

#include "httpd.h"
#include "http_config.h"
#include "http_protocol.h"
#include "http_request.h"
#include "http_connection.h"
#include "http_log.h"
#include "http_core.h"
#include "ap_mpm.h"
#include "apr_strings.h"
#include "apr_buckets.h"
#include "apr_network_io.h"
#include "apr_file_info.h"
#include "apr_time.h"
#include "util_filter.h"

#include <sys/socket.h>
#include <sys/un.h>
#include <unistd.h>
#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <string.h>
#include <stdlib.h>
#include <limits.h>
#include <time.h>

#define HANDOFF_HEADER "X-Socket-Handoff"
#define HANDOFF_DATA_HEADER "X-Handoff-Data"
#define HANDOFF_FILTER_NAME "SOCKET_HANDOFF"
module AP_MODULE_DECLARE_DATA socket_handoff_module;

/* Per-server configuration */
typedef struct {
    int enabled;
    int enabled_set;
    const char *allowed_socket_prefix;      /* Original config value */
    const char *resolved_prefix;            /* Cached resolved prefix with trailing slash */
    const char *resolved_prefix_truename;   /* Cached TRUENAME version (symlinks resolved) */
    int connect_timeout_ms;
    int connect_timeout_ms_set;
    int send_timeout_ms;
    int send_timeout_ms_set;
    int max_retries;
    int max_retries_set;
} socket_handoff_config;

/* Retry configuration defaults */
#define DEFAULT_MAX_RETRIES 3
#define RETRY_BASE_DELAY_MS 10  /* Base delay: 10ms, 20ms, 40ms */

/* Create per-server config */
static void *create_server_config(apr_pool_t *p, server_rec *s)
{
    socket_handoff_config *conf = apr_pcalloc(p, sizeof(socket_handoff_config));
    conf->enabled = 1;
    conf->enabled_set = 0;
    conf->allowed_socket_prefix = "/var/run/";
    conf->connect_timeout_ms = 500;
    conf->connect_timeout_ms_set = 0;
    conf->send_timeout_ms = 500;
    conf->send_timeout_ms_set = 0;
    conf->max_retries = DEFAULT_MAX_RETRIES;
    conf->max_retries_set = 0;
    return conf;
}

/* Merge per-server configs */
static void *merge_server_config(apr_pool_t *p, void *base_conf, void *new_conf)
{
    socket_handoff_config *base = (socket_handoff_config *)base_conf;
    socket_handoff_config *add = (socket_handoff_config *)new_conf;
    socket_handoff_config *conf = apr_pcalloc(p, sizeof(socket_handoff_config));

    conf->enabled = add->enabled_set ? add->enabled : base->enabled;
    conf->enabled_set = add->enabled_set || base->enabled_set;
    conf->allowed_socket_prefix = add->allowed_socket_prefix
        ? add->allowed_socket_prefix
        : base->allowed_socket_prefix;
    /* Cached prefixes are computed in post_config, not inherited */
    conf->resolved_prefix = NULL;
    conf->resolved_prefix_truename = NULL;
    conf->connect_timeout_ms = add->connect_timeout_ms_set
        ? add->connect_timeout_ms
        : base->connect_timeout_ms;
    conf->connect_timeout_ms_set = add->connect_timeout_ms_set
        || base->connect_timeout_ms_set;
    conf->send_timeout_ms = add->send_timeout_ms_set
        ? add->send_timeout_ms
        : base->send_timeout_ms;
    conf->send_timeout_ms_set = add->send_timeout_ms_set
        || base->send_timeout_ms_set;
    conf->max_retries = add->max_retries_set
        ? add->max_retries
        : base->max_retries;
    conf->max_retries_set = add->max_retries_set
        || base->max_retries_set;

    return conf;
}

/*
 * Remaining time until deadline in milliseconds (0 if expired).
 */
static int remaining_timeout_ms(apr_time_t deadline)
{
    apr_time_t now = apr_time_now();
    apr_time_t diff;

    if (now >= deadline) {
        return 0;
    }
    diff = deadline - now; /* microseconds */
    if (diff / 1000 > INT_MAX) {
        return INT_MAX;
    }
    return (int)(diff / 1000);
}

/*
 * Compute an overall deadline for connect + send, including max backoff.
 */
static apr_time_t compute_handoff_deadline(const socket_handoff_config *conf)
{
    apr_int64_t backoff_ms = 0;
    apr_int64_t total_ms;
    int safe_retries;

    if (conf->max_retries > 0) {
        /* Cap shift to avoid overflow (30 gives ~10 billion ms, plenty) */
        safe_retries = conf->max_retries > 30 ? 30 : conf->max_retries;
        backoff_ms = (apr_int64_t)RETRY_BASE_DELAY_MS
            * ((1LL << safe_retries) - 1);
    }

    total_ms = (apr_int64_t)conf->connect_timeout_ms
        + (apr_int64_t)conf->send_timeout_ms
        + backoff_ms;

    if (total_ms < 1) {
        total_ms = 1;
    }
    if (total_ms > APR_INT32_MAX) {
        total_ms = APR_INT32_MAX;
    }

    return apr_time_now() + apr_time_from_msec((apr_int32_t)total_ms);
}

/*
 * Wait for socket to be ready for writing with timeout.
 * Returns 1 if ready, 0 on timeout, -1 on error.
 */
static int wait_for_write(int sock, int timeout_ms)
{
    struct pollfd pfd;
    int ret;

    pfd.fd = sock;
    pfd.events = POLLOUT;
    pfd.revents = 0;

    do {
        ret = poll(&pfd, 1, timeout_ms);
    } while (ret < 0 && errno == EINTR);

    if (ret == 0) {
        errno = ETIMEDOUT;
        return 0;
    }
    if (ret < 0) {
        return -1;
    }
    if (pfd.revents & (POLLERR | POLLHUP | POLLNVAL)) {
        errno = EPIPE;
        return -1;
    }

    return 1;
}

/*
 * Send file descriptor over Unix socket using SCM_RIGHTS
 *
 * This is the core mechanism for passing the client connection
 * to the external daemon. The fd is sent as ancillary data.
 *
 * Uses poll() to enforce send timeout on non-blocking socket.
 */
static int send_fd_with_data(int unix_sock, int fd_to_send,
                             const char *data, size_t data_len,
                             int timeout_ms,
                             apr_time_t deadline)
{
    struct msghdr msg;
    struct iovec iov;
    union {
        struct cmsghdr cm;
        char control[CMSG_SPACE(sizeof(int))];
    } control_un;
    struct cmsghdr *cmsg;
    char dummy = '\0';
    ssize_t sent;
    size_t remaining;
    const char *p;
    ssize_t n;

    memset(&msg, 0, sizeof(msg));
    memset(&control_un, 0, sizeof(control_un));

    /* Must send at least 1 byte of real data with SCM_RIGHTS */
    if (data && data_len > 0) {
        iov.iov_base = (void *)data;
        iov.iov_len = data_len;
    } else {
        iov.iov_base = &dummy;
        iov.iov_len = 1;
    }
    msg.msg_iov = &iov;
    msg.msg_iovlen = 1;

    /* Set up control message containing the fd */
    msg.msg_control = control_un.control;
    msg.msg_controllen = sizeof(control_un.control);

    cmsg = CMSG_FIRSTHDR(&msg);
    cmsg->cmsg_level = SOL_SOCKET;
    cmsg->cmsg_type = SCM_RIGHTS;
    cmsg->cmsg_len = CMSG_LEN(sizeof(int));
    memcpy(CMSG_DATA(cmsg), &fd_to_send, sizeof(int));

    /* Wait for socket to be ready for writing */
    {
        int remaining_ms = remaining_timeout_ms(deadline);
        if (remaining_ms <= 0) {
            errno = ETIMEDOUT;
            return -1;
        }
        if (wait_for_write(unix_sock,
                           remaining_ms < timeout_ms ? remaining_ms : timeout_ms) <= 0) {
            return -1;
        }
    }

    sent = sendmsg(unix_sock, &msg, 0);
    if (sent < 0) {
        if (errno == EAGAIN || errno == EWOULDBLOCK) {
            /* Socket not ready despite poll - treat as transient, but fail */
            errno = ETIMEDOUT;
        }
        return -1;
    }

    if (data && data_len > 0 && (size_t)sent < data_len) {
        remaining = data_len - (size_t)sent;
        p = data + sent;
        while (remaining > 0) {
            /* Wait for socket to be ready before each send */
            int remaining_ms = remaining_timeout_ms(deadline);
            if (remaining_ms <= 0) {
                errno = ETIMEDOUT;
                return -1;
            }
            if (wait_for_write(unix_sock,
                               remaining_ms < timeout_ms ? remaining_ms : timeout_ms) <= 0) {
                return -1;
            }

            n = send(unix_sock, p, remaining, 0);
            if (n < 0) {
                if (errno == EINTR) {
                    continue;
                }
                if (errno == EAGAIN || errno == EWOULDBLOCK) {
                    errno = ETIMEDOUT;
                }
                return -1;
            }
            if (n == 0) {
                /* No progress possible: treat as error to avoid infinite loop */
                errno = EPIPE;
                return -1;
            }
            remaining -= (size_t)n;
            p += n;
        }
    }

    return 0;
}

/*
 * Connect to Unix domain socket with timeout
 *
 * Returns connected non-blocking socket, or -1 on error.
 * The returned_errno parameter is set to the error code for retry decisions.
 */
static int connect_to_socket(const char *socket_path,
                             int connect_timeout_ms,
                             request_rec *r,
                             int *returned_errno)
{
    int sock;
    struct sockaddr_un addr;
    int flags;
    struct pollfd pfd;
    int poll_ret;
    int so_error;
    socklen_t so_error_len;
    int sock_nonblock = 0;

    *returned_errno = 0;

    /*
     * Use SOCK_NONBLOCK if available (Linux 2.6.27+).
     * This eliminates one fcntl() call per connection.
     */
#ifdef SOCK_NONBLOCK
    sock = socket(AF_UNIX, SOCK_STREAM | SOCK_NONBLOCK, 0);
    if (sock >= 0) {
        sock_nonblock = 1;
    } else if (errno == EINVAL) {
        /* SOCK_NONBLOCK not supported, fall back */
        sock = socket(AF_UNIX, SOCK_STREAM, 0);
    }
#else
    sock = socket(AF_UNIX, SOCK_STREAM, 0);
#endif

    if (sock < 0) {
        *returned_errno = errno;
        ap_log_rerror(APLOG_MARK, APLOG_ERR, 0, r,
            "mod_socket_handoff: socket() failed: %s", strerror(errno));
        return -1;
    }

    memset(&addr, 0, sizeof(addr));
    addr.sun_family = AF_UNIX;

    if (strlen(socket_path) >= sizeof(addr.sun_path)) {
        ap_log_rerror(APLOG_MARK, APLOG_ERR, 0, r,
            "mod_socket_handoff: Socket path too long: %s", socket_path);
        close(sock);
        *returned_errno = ENAMETOOLONG;
        return -1;
    }

    strncpy(addr.sun_path, socket_path, sizeof(addr.sun_path) - 1);

    /* Set non-blocking for connect with timeout (if not already set via SOCK_NONBLOCK) */
    if (!sock_nonblock) {
        flags = fcntl(sock, F_GETFL, 0);
        if (flags < 0) {
            *returned_errno = errno;
            ap_log_rerror(APLOG_MARK, APLOG_ERR, 0, r,
                "mod_socket_handoff: fcntl(F_GETFL) failed: %s", strerror(errno));
            close(sock);
            return -1;
        }
        if (fcntl(sock, F_SETFL, flags | O_NONBLOCK) < 0) {
            *returned_errno = errno;
            ap_log_rerror(APLOG_MARK, APLOG_ERR, 0, r,
                "mod_socket_handoff: fcntl(F_SETFL) failed: %s", strerror(errno));
            close(sock);
            return -1;
        }
    }

    if (connect(sock, (struct sockaddr *)&addr, sizeof(addr)) < 0) {
        if (errno != EINPROGRESS) {
            *returned_errno = errno;
            ap_log_rerror(APLOG_MARK, APLOG_ERR, 0, r,
                "mod_socket_handoff: connect() to %s failed: %s",
                socket_path, strerror(errno));
            close(sock);
            return -1;
        }

        /* Wait for connection with timeout */
        pfd.fd = sock;
        pfd.events = POLLOUT;
        pfd.revents = 0;
        do {
            poll_ret = poll(&pfd, 1, connect_timeout_ms);
        } while (poll_ret < 0 && errno == EINTR);

        if (poll_ret == 0) {
            ap_log_rerror(APLOG_MARK, APLOG_ERR, 0, r,
                "mod_socket_handoff: connect() to %s timed out after %d ms",
                socket_path, connect_timeout_ms);
            close(sock);
            *returned_errno = ETIMEDOUT;
            return -1;
        }
        if (poll_ret < 0) {
            *returned_errno = errno;
            ap_log_rerror(APLOG_MARK, APLOG_ERR, 0, r,
                "mod_socket_handoff: poll() for connect() to %s failed: %s",
                socket_path, strerror(errno));
            close(sock);
            return -1;
        }

        /* Check for poll error conditions */
        if (pfd.revents & (POLLERR | POLLHUP | POLLNVAL)) {
            ap_log_rerror(APLOG_MARK, APLOG_ERR, 0, r,
                "mod_socket_handoff: poll() on connect() to %s indicated error",
                socket_path);
            close(sock);
            *returned_errno = ECONNREFUSED;
            return -1;
        }

        /* Check socket error status */
        so_error = 0;
        so_error_len = sizeof(so_error);
        if (getsockopt(sock, SOL_SOCKET, SO_ERROR,
                       &so_error, &so_error_len) < 0) {
            *returned_errno = errno;
            ap_log_rerror(APLOG_MARK, APLOG_ERR, 0, r,
                "mod_socket_handoff: getsockopt(SO_ERROR) failed: %s",
                strerror(errno));
            close(sock);
            return -1;
        }
        if (so_error != 0) {
            *returned_errno = so_error;
            ap_log_rerror(APLOG_MARK, APLOG_ERR, 0, r,
                "mod_socket_handoff: connect() to %s failed: %s",
                socket_path, strerror(so_error));
            close(sock);
            return -1;
        }
    }

    /* Socket stays non-blocking; send timeout is enforced via poll() in send_fd_with_data() */

    return sock;
}

/*
 * Check if an error is transient and worth retrying.
 * ENOENT: daemon socket file doesn't exist yet (daemon starting)
 * ECONNREFUSED: daemon not listening (daemon restarting)
 */
static int is_transient_error(int err)
{
    return (err == ENOENT || err == ECONNREFUSED);
}

/*
 * Connect to daemon with retry for transient errors.
 * Uses exponential backoff: 10ms, 20ms, 40ms, etc.
 */
static int connect_with_retry(const char *socket_path,
                              int connect_timeout_ms,
                              int max_retries,
                              request_rec *r,
                              apr_time_t deadline)
{
    int sock;
    int returned_errno = 0;
    int attempt;
    int delay_ms;
    struct timespec ts;

    for (attempt = 0; attempt <= max_retries; attempt++) {
        int remaining_ms = remaining_timeout_ms(deadline);
        int attempt_timeout;
        if (remaining_ms <= 0) {
            errno = ETIMEDOUT;
            break;
        }
        attempt_timeout = remaining_ms < connect_timeout_ms ? remaining_ms : connect_timeout_ms;
        sock = connect_to_socket(socket_path, attempt_timeout,
                                 r, &returned_errno);
        if (sock >= 0) {
            if (attempt > 0) {
                ap_log_rerror(APLOG_MARK, APLOG_INFO, 0, r,
                    "mod_socket_handoff: connect() to %s succeeded on attempt %d",
                    socket_path, attempt + 1);
            }
            return sock;
        }

        /* Don't retry on final attempt or non-transient errors */
        if (attempt >= max_retries || !is_transient_error(returned_errno)) {
            break;
        }

        /* Exponential backoff: 10ms, 20ms, 40ms, ... */
        {
            int safe_attempt = attempt > 20 ? 20 : attempt;
            delay_ms = RETRY_BASE_DELAY_MS * (1 << safe_attempt);
        }
        {
            int backoff_remaining = remaining_timeout_ms(deadline);
            if (backoff_remaining <= 0) {
                errno = ETIMEDOUT;
                break;
            }
            if (delay_ms > backoff_remaining) {
                delay_ms = backoff_remaining;
            }
        }

        ap_log_rerror(APLOG_MARK, APLOG_DEBUG, 0, r,
            "mod_socket_handoff: connect() to %s failed (%s), "
            "retrying in %d ms (attempt %d/%d)",
            socket_path, strerror(returned_errno),
            delay_ms, attempt + 1, max_retries);

        ts.tv_sec = delay_ms / 1000;
        ts.tv_nsec = (delay_ms % 1000) * 1000000L;
        nanosleep(&ts, NULL);
    }

    return -1;
}

/*
 * Create a dummy socket and swap it for the real client socket.
 *
 * This is the key trick borrowed from mod_proxy_fdpass:
 * - Apache's cleanup code will close whatever socket is in conn_config
 * - By swapping in a dummy, the real client socket stays open
 * - The external daemon now owns the real socket
 */
static apr_status_t create_dummy_socket(apr_socket_t **dummy_socket,
                                        apr_pool_t *p, request_rec *r)
{
    apr_status_t rv;

    /*
     * Create an unconnected TCP socket.
     * It doesn't matter that it's not connected - we just need
     * something for Apache to close instead of the real socket.
     */
    rv = apr_socket_create(dummy_socket, APR_INET, SOCK_STREAM,
                           APR_PROTO_TCP, p);
    if (rv != APR_SUCCESS) {
        ap_log_rerror(APLOG_MARK, APLOG_WARNING, rv, r,
            "mod_socket_handoff: Could not create dummy socket");
        return rv;
    }

    return APR_SUCCESS;
}

/*
 * Validate that the socket path is under the allowed prefix.
 * This prevents PHP from redirecting to arbitrary Unix sockets.
 *
 * Uses cached resolved prefixes from post_config when available,
 * reducing apr_filepath_merge() calls per request.
 */
static int validate_socket_path(const char *socket_path,
                                socket_handoff_config *conf,
                                request_rec *r)
{
    char *resolved_path = NULL;
    char *truename_path = NULL;
    const char *prefix_with_slash = NULL;
    const char *truename_prefix_with_slash = NULL;
    apr_status_t rv;

    if (!conf->allowed_socket_prefix || conf->allowed_socket_prefix[0] == '\0') {
        /* No prefix configured - allow all (not recommended) */
        return 1;
    }

    /* Canonicalize socket path without requiring it to exist */
    rv = apr_filepath_merge(&resolved_path, NULL, socket_path,
                            APR_FILEPATH_NOTRELATIVE, r->pool);
    if (rv != APR_SUCCESS || !resolved_path) {
        ap_log_rerror(APLOG_MARK, APLOG_ERR, rv, r,
            "mod_socket_handoff: Invalid socket path %s", socket_path);
        return 0;
    }

    /* Use cached resolved prefix if available, otherwise compute */
    if (conf->resolved_prefix) {
        prefix_with_slash = conf->resolved_prefix;
    } else {
        char *resolved_prefix = NULL;
        rv = apr_filepath_merge(&resolved_prefix, NULL, conf->allowed_socket_prefix,
                                APR_FILEPATH_NOTRELATIVE, r->pool);
        if (rv != APR_SUCCESS || !resolved_prefix) {
            ap_log_rerror(APLOG_MARK, APLOG_ERR, rv, r,
                "mod_socket_handoff: Invalid allowed prefix %s",
                conf->allowed_socket_prefix);
            return 0;
        }
        if (resolved_prefix[strlen(resolved_prefix) - 1] == '/') {
            prefix_with_slash = resolved_prefix;
        } else {
            prefix_with_slash = apr_pstrcat(r->pool, resolved_prefix, "/", NULL);
        }
    }

    if (strncmp(resolved_path, prefix_with_slash,
                strlen(prefix_with_slash)) != 0) {
        ap_log_rerror(APLOG_MARK, APLOG_ERR, 0, r,
            "mod_socket_handoff: Path %s resolves outside allowed prefix",
            socket_path);
        return 0;
    }

    /*
     * If the socket path exists, resolve symlinks and re-check. This closes
     * symlink-escape attacks while allowing non-existent paths to be validated
     * lexically (e.g., when daemons create the socket later).
     */
    rv = apr_filepath_merge(&truename_path, NULL, socket_path,
                            APR_FILEPATH_TRUENAME | APR_FILEPATH_NOTRELATIVE,
                            r->pool);
    if (rv == APR_SUCCESS && truename_path) {
        /* Use cached truename prefix if available */
        if (conf->resolved_prefix_truename) {
            truename_prefix_with_slash = conf->resolved_prefix_truename;
        } else {
            char *truename_prefix = NULL;
            rv = apr_filepath_merge(&truename_prefix, NULL, conf->allowed_socket_prefix,
                                    APR_FILEPATH_TRUENAME | APR_FILEPATH_NOTRELATIVE,
                                    r->pool);
            if (rv != APR_SUCCESS || !truename_prefix) {
                ap_log_rerror(APLOG_MARK, APLOG_ERR, rv, r,
                    "mod_socket_handoff: Invalid allowed prefix %s",
                    conf->allowed_socket_prefix);
                return 0;
            }
            if (truename_prefix[strlen(truename_prefix) - 1] == '/') {
                truename_prefix_with_slash = truename_prefix;
            } else {
                truename_prefix_with_slash = apr_pstrcat(r->pool, truename_prefix, "/", NULL);
            }
        }

        if (strncmp(truename_path, truename_prefix_with_slash,
                    strlen(truename_prefix_with_slash)) != 0) {
            ap_log_rerror(APLOG_MARK, APLOG_ERR, 0, r,
                "mod_socket_handoff: Path %s resolves outside allowed prefix "
                "after symlink resolution",
                socket_path);
            return 0;
        }
    }

    return 1;
}

/*
 * Output filter - this is where the magic happens.
 *
 * Runs after PHP (or any handler) has generated its response.
 * Checks for X-Socket-Handoff header and hands off if present.
 *
 * This output filter pattern is borrowed from mod_xsendfile.
 */
static apr_status_t socket_handoff_output_filter(ap_filter_t *f,
                                                  apr_bucket_brigade *bb)
{
    request_rec *r = f->r;
    conn_rec *c = r->connection;
    socket_handoff_config *conf;
    const char *socket_path;
    const char *handoff_data;
    apr_os_sock_t client_fd;
    apr_socket_t *client_socket;
    apr_socket_t *dummy_socket = NULL;
    int daemon_sock = -1;
    apr_status_t status = APR_SUCCESS;
    apr_time_t deadline = 0;
    apr_bucket *e;
    apr_bucket *next;

    /* Get module config */
    conf = ap_get_module_config(r->server->module_config,
                                &socket_handoff_module);

    /* Check if enabled and if this is a main request */
    if (!conf->enabled || r->main != NULL) {
        ap_remove_output_filter(f);
        return ap_pass_brigade(f->next, bb);
    }

    /* Look for redirect header in response */
    socket_path = apr_table_get(r->headers_out, HANDOFF_HEADER);
    if (!socket_path) {
        socket_path = apr_table_get(r->err_headers_out, HANDOFF_HEADER);
    }

    if (!socket_path) {
        /* No handoff requested - pass through normally */
        ap_remove_output_filter(f);
        return ap_pass_brigade(f->next, bb);
    }

    ap_log_rerror(APLOG_MARK, APLOG_DEBUG, 0, r,
        "mod_socket_handoff: Handoff requested to %s", socket_path);

    /* Validate socket path for security */
    if (!validate_socket_path(socket_path, conf, r)) {
        status = HTTP_FORBIDDEN;
        goto cleanup;
    }

    /* Get optional data header */
    handoff_data = apr_table_get(r->headers_out, HANDOFF_DATA_HEADER);
    if (!handoff_data) {
        handoff_data = apr_table_get(r->err_headers_out, HANDOFF_DATA_HEADER);
    }

    /* Create a dummy socket upfront; if this fails, don't hand off */
    if (create_dummy_socket(&dummy_socket, r->pool, r) != APR_SUCCESS) {
        status = HTTP_INTERNAL_SERVER_ERROR;
        goto cleanup;
    }

    /* Remove our headers so client never sees them */
    apr_table_unset(r->headers_out, HANDOFF_HEADER);
    apr_table_unset(r->err_headers_out, HANDOFF_HEADER);
    apr_table_unset(r->headers_out, HANDOFF_DATA_HEADER);
    apr_table_unset(r->err_headers_out, HANDOFF_DATA_HEADER);

    /* Discard any response body that PHP might have generated */
    for (e = APR_BRIGADE_FIRST(bb);
         e != APR_BRIGADE_SENTINEL(bb);
         e = next) {
        next = APR_BUCKET_NEXT(e);
        apr_bucket_delete(e);
    }

    /* Get the client socket from the connection */
    client_socket = ap_get_conn_socket(c);
    if (!client_socket) {
        ap_log_rerror(APLOG_MARK, APLOG_ERR, 0, r,
            "mod_socket_handoff: Could not get client socket");
        status = HTTP_INTERNAL_SERVER_ERROR;
        goto cleanup;
    }

    /* Extract the OS-level file descriptor */
    if (apr_os_sock_get(&client_fd, client_socket) != APR_SUCCESS) {
        ap_log_rerror(APLOG_MARK, APLOG_ERR, 0, r,
            "mod_socket_handoff: Could not get client fd");
        status = HTTP_INTERNAL_SERVER_ERROR;
        goto cleanup;
    }

    /* Connect to the streaming daemon with retry for transient errors */
    deadline = compute_handoff_deadline(conf);
    daemon_sock = connect_with_retry(socket_path,
                                     conf->connect_timeout_ms,
                                     conf->max_retries,
                                     r,
                                     deadline);
    if (daemon_sock < 0) {
        status = HTTP_SERVICE_UNAVAILABLE;
        goto cleanup;
    }

    /* Pass the client fd to the daemon via SCM_RIGHTS */
    if (send_fd_with_data(daemon_sock, client_fd,
                          handoff_data,
                          handoff_data ? strlen(handoff_data) : 0,
                          conf->send_timeout_ms,
                          deadline) < 0) {
        ap_log_rerror(APLOG_MARK, APLOG_ERR, 0, r,
            "mod_socket_handoff: Failed to send fd: %s", strerror(errno));
        status = HTTP_INTERNAL_SERVER_ERROR;
        goto cleanup;
    }

    /* Done with daemon socket */
    close(daemon_sock);
    daemon_sock = -1;

    /*
     * Swap in a dummy socket so Apache doesn't close the real one.
     * The daemon now owns the client connection.
     * This trick is borrowed from mod_proxy_fdpass.
     */
    /*
     * Replace the connection's socket with our dummy.
     * The core module stores the socket in conn_config.
     */
    ap_set_core_module_config(c->conn_config, dummy_socket);

    /*
     * Mark connection as aborted.
     * This tells Apache to skip normal response handling.
     */
    c->aborted = 1;

    ap_log_rerror(APLOG_MARK, APLOG_INFO, 0, r,
        "mod_socket_handoff: Successfully handed off to %s "
        "(data_len=%lu)",
        socket_path,
        (unsigned long)(handoff_data ? strlen(handoff_data) : 0));

    status = APR_SUCCESS;

cleanup:
    if (daemon_sock >= 0) {
        close(daemon_sock);
    }
    ap_remove_output_filter(f);

    return status;
}

/*
 * Insert filter hook - adds our filter to every request
 */
static void socket_handoff_insert_filter(request_rec *r)
{
    socket_handoff_config *conf;

    conf = ap_get_module_config(r->server->module_config,
                                &socket_handoff_module);

    if (conf->enabled) {
        ap_add_output_filter(HANDOFF_FILTER_NAME, NULL, r, r->connection);
    }
}

/* Configuration directive handlers */
static const char *set_enabled(cmd_parms *cmd, void *dummy, int flag)
{
    socket_handoff_config *conf = ap_get_module_config(
        cmd->server->module_config, &socket_handoff_module);
    conf->enabled = flag;
    conf->enabled_set = 1;
    return NULL;
}

static const char *set_allowed_prefix(cmd_parms *cmd, void *dummy,
                                      const char *prefix)
{
    socket_handoff_config *conf = ap_get_module_config(
        cmd->server->module_config, &socket_handoff_module);
    conf->allowed_socket_prefix = apr_pstrdup(cmd->pool, prefix);
    return NULL;
}

/* Maximum connect timeout: 60 seconds */
#define MAX_CONNECT_TIMEOUT_MS 60000

static const char *set_connect_timeout(cmd_parms *cmd, void *dummy,
                                       const char *arg)
{
    socket_handoff_config *conf = ap_get_module_config(
        cmd->server->module_config, &socket_handoff_module);
    char *endptr;
    long timeout;

    errno = 0;
    timeout = strtol(arg, &endptr, 10);

    /* Check for conversion errors */
    if (endptr == arg || *endptr != '\0') {
        return "SocketHandoffConnectTimeoutMs must be a valid integer";
    }
    if (errno == ERANGE || timeout < 1 || timeout > MAX_CONNECT_TIMEOUT_MS) {
        return apr_psprintf(cmd->pool,
            "SocketHandoffConnectTimeoutMs must be between 1 and %d",
            MAX_CONNECT_TIMEOUT_MS);
    }

    conf->connect_timeout_ms = (int)timeout;
    conf->connect_timeout_ms_set = 1;
    return NULL;
}

static const char *set_send_timeout(cmd_parms *cmd, void *dummy,
                                    const char *arg)
{
    socket_handoff_config *conf = ap_get_module_config(
        cmd->server->module_config, &socket_handoff_module);
    char *endptr;
    long timeout;

    errno = 0;
    timeout = strtol(arg, &endptr, 10);

    /* Check for conversion errors */
    if (endptr == arg || *endptr != '\0') {
        return "SocketHandoffSendTimeoutMs must be a valid integer";
    }
    if (errno == ERANGE || timeout < 1 || timeout > MAX_CONNECT_TIMEOUT_MS) {
        return apr_psprintf(cmd->pool,
            "SocketHandoffSendTimeoutMs must be between 1 and %d",
            MAX_CONNECT_TIMEOUT_MS);
    }

    conf->send_timeout_ms = (int)timeout;
    conf->send_timeout_ms_set = 1;
    return NULL;
}

/* Maximum retries: 10 */
#define MAX_RETRIES_LIMIT 10

static const char *set_max_retries(cmd_parms *cmd, void *dummy,
                                   const char *arg)
{
    socket_handoff_config *conf = ap_get_module_config(
        cmd->server->module_config, &socket_handoff_module);
    char *endptr;
    long retries;

    errno = 0;
    retries = strtol(arg, &endptr, 10);

    /* Check for conversion errors */
    if (endptr == arg || *endptr != '\0') {
        return "SocketHandoffMaxRetries must be a valid integer";
    }
    if (errno == ERANGE || retries < 0 || retries > MAX_RETRIES_LIMIT) {
        return apr_psprintf(cmd->pool,
            "SocketHandoffMaxRetries must be between 0 and %d",
            MAX_RETRIES_LIMIT);
    }

    conf->max_retries = (int)retries;
    conf->max_retries_set = 1;
    return NULL;
}

/* Configuration directives */
static const command_rec socket_handoff_cmds[] = {
    AP_INIT_FLAG(
        "SocketHandoffEnabled",
        set_enabled,
        NULL,
        RSRC_CONF,
        "Enable or disable socket handoff (default: On)"
    ),
    AP_INIT_TAKE1(
        "SocketHandoffAllowedPrefix",
        set_allowed_prefix,
        NULL,
        RSRC_CONF,
        "Required prefix for socket paths (default: /var/run/)"
    ),
    AP_INIT_TAKE1(
        "SocketHandoffConnectTimeoutMs",
        set_connect_timeout,
        NULL,
        RSRC_CONF,
        "Timeout for connecting to handoff daemon in milliseconds (default: 500)"
    ),
    AP_INIT_TAKE1(
        "SocketHandoffSendTimeoutMs",
        set_send_timeout,
        NULL,
        RSRC_CONF,
        "Timeout in ms for sending fd to daemon; uses poll() on non-blocking socket (default: 500)"
    ),
    AP_INIT_TAKE1(
        "SocketHandoffMaxRetries",
        set_max_retries,
        NULL,
        RSRC_CONF,
        "Max retries for transient connect errors like ENOENT/ECONNREFUSED (default: 3)"
    ),
    {NULL}
};

/*
 * Post-config hook - resolve allowed prefix once at startup.
 * This eliminates multiple apr_filepath_merge() calls per request.
 */
static int socket_handoff_post_config(apr_pool_t *pconf, apr_pool_t *plog,
                                       apr_pool_t *ptemp, server_rec *s)
{
    server_rec *vhost;
    socket_handoff_config *conf;
    apr_status_t rv;
    char *resolved = NULL;
    size_t len;

    for (vhost = s; vhost; vhost = vhost->next) {
        conf = ap_get_module_config(vhost->module_config, &socket_handoff_module);

        if (!conf || !conf->allowed_socket_prefix) {
            continue;
        }

        /* Resolve prefix without TRUENAME (for lexical prefix matching) */
        rv = apr_filepath_merge(&resolved, NULL, conf->allowed_socket_prefix,
                                APR_FILEPATH_NOTRELATIVE, pconf);
        if (rv == APR_SUCCESS && resolved) {
            len = strlen(resolved);
            if (len > 0 && resolved[len - 1] != '/') {
                conf->resolved_prefix = apr_pstrcat(pconf, resolved, "/", NULL);
            } else {
                conf->resolved_prefix = resolved;
            }
        }

        /* Also resolve with TRUENAME if the prefix exists (for symlink checks) */
        resolved = NULL;
        rv = apr_filepath_merge(&resolved, NULL, conf->allowed_socket_prefix,
                                APR_FILEPATH_TRUENAME | APR_FILEPATH_NOTRELATIVE,
                                pconf);
        if (rv == APR_SUCCESS && resolved) {
            len = strlen(resolved);
            if (len > 0 && resolved[len - 1] != '/') {
                conf->resolved_prefix_truename = apr_pstrcat(pconf, resolved, "/", NULL);
            } else {
                conf->resolved_prefix_truename = resolved;
            }
        }
        /* If TRUENAME fails (prefix doesn't exist), resolved_prefix_truename stays NULL */
    }

    return OK;
}

/* Register hooks */
static void register_hooks(apr_pool_t *p)
{
    ap_register_output_filter(
        HANDOFF_FILTER_NAME,
        socket_handoff_output_filter,
        NULL,
        AP_FTYPE_CONTENT_SET
    );

    /* Insert filter late so it runs after other filters */
    ap_hook_insert_filter(socket_handoff_insert_filter, NULL, NULL, APR_HOOK_LAST);

    /* Post-config hook to cache resolved prefix */
    ap_hook_post_config(socket_handoff_post_config, NULL, NULL, APR_HOOK_MIDDLE);
}

/* Module declaration */
module AP_MODULE_DECLARE_DATA socket_handoff_module = {
    STANDARD20_MODULE_STUFF,
    NULL,                       /* create per-dir config */
    NULL,                       /* merge per-dir config */
    create_server_config,       /* create per-server config */
    merge_server_config,        /* merge per-server config */
    socket_handoff_cmds,        /* command table */
    register_hooks              /* register hooks */
};
