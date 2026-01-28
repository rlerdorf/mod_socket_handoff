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
#include "util_filter.h"

#include <sys/socket.h>
#include <sys/un.h>
#include <unistd.h>
#include <errno.h>
#include <string.h>

#define HANDOFF_HEADER "X-Socket-Handoff"
#define HANDOFF_DATA_HEADER "X-Handoff-Data"
#define HANDOFF_FILTER_NAME "SOCKET_HANDOFF"

module AP_MODULE_DECLARE_DATA socket_handoff_module;

/* Per-server configuration */
typedef struct {
    int enabled;
    int enabled_set;
    const char *allowed_socket_prefix;
} socket_handoff_config;

/* Create per-server config */
static void *create_server_config(apr_pool_t *p, server_rec *s)
{
    socket_handoff_config *conf = apr_pcalloc(p, sizeof(socket_handoff_config));
    conf->enabled = 1;
    conf->enabled_set = 0;
    conf->allowed_socket_prefix = "/var/run/";
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

    return conf;
}

/*
 * Send file descriptor over Unix socket using SCM_RIGHTS
 *
 * This is the core mechanism for passing the client connection
 * to the external daemon. The fd is sent as ancillary data.
 */
static int send_fd_with_data(int unix_sock, int fd_to_send,
                             const char *data, size_t data_len)
{
    struct msghdr msg;
    struct iovec iov;
    union {
        struct cmsghdr cm;
        char control[CMSG_SPACE(sizeof(int))];
    } control_un;
    struct cmsghdr *cmsg;
    char dummy = '\0';

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

    return sendmsg(unix_sock, &msg, 0);
}

/*
 * Connect to Unix domain socket
 */
static int connect_to_socket(const char *socket_path, request_rec *r)
{
    int sock;
    struct sockaddr_un addr;

    sock = socket(AF_UNIX, SOCK_STREAM, 0);
    if (sock < 0) {
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
        return -1;
    }

    strncpy(addr.sun_path, socket_path, sizeof(addr.sun_path) - 1);

    if (connect(sock, (struct sockaddr *)&addr, sizeof(addr)) < 0) {
        ap_log_rerror(APLOG_MARK, APLOG_ERR, 0, r,
            "mod_socket_handoff: connect() to %s failed: %s",
            socket_path, strerror(errno));
        close(sock);
        return -1;
    }

    return sock;
}

/*
 * Create a dummy socket and swap it for the real client socket.
 *
 * This is the key trick borrowed from mod_proxy_fdpass:
 * - Apache's cleanup code will close whatever socket is in conn_config
 * - By swapping in a dummy, the real client socket stays open
 * - The external daemon now owns the real socket
 */
static apr_status_t swap_to_dummy_socket(conn_rec *c, apr_pool_t *p,
                                         request_rec *r)
{
    apr_socket_t *dummy_socket;
    apr_status_t rv;

    /*
     * Create an unconnected TCP socket.
     * It doesn't matter that it's not connected - we just need
     * something for Apache to close instead of the real socket.
     */
    rv = apr_socket_create(&dummy_socket, APR_INET, SOCK_STREAM,
                           APR_PROTO_TCP, p);
    if (rv != APR_SUCCESS) {
        ap_log_rerror(APLOG_MARK, APLOG_WARNING, rv, r,
            "mod_socket_handoff: Could not create dummy socket");
        return rv;
    }

    /*
     * Replace the connection's socket with our dummy.
     * The core module stores the socket in conn_config.
     */
    ap_set_core_module_config(c->conn_config, dummy_socket);

    return APR_SUCCESS;
}

/*
 * Validate that the socket path is under the allowed prefix.
 * This prevents PHP from redirecting to arbitrary Unix sockets.
 */
static int validate_socket_path(const char *socket_path,
                                const char *allowed_prefix,
                                request_rec *r)
{
    char *resolved_path = NULL;
    char *resolved_prefix = NULL;
    const char *prefix_with_slash = NULL;
    apr_status_t rv;

    if (!allowed_prefix || allowed_prefix[0] == '\0') {
        /* No prefix configured - allow all (not recommended) */
        return 1;
    }

    /* Canonicalize paths without requiring the socket to exist */
    rv = apr_filepath_merge(&resolved_path, NULL, socket_path,
                            APR_FILEPATH_NOTRELATIVE, r->pool);
    if (rv != APR_SUCCESS || !resolved_path) {
        ap_log_rerror(APLOG_MARK, APLOG_ERR, rv, r,
            "mod_socket_handoff: Invalid socket path %s", socket_path);
        return 0;
    }

    rv = apr_filepath_merge(&resolved_prefix, NULL, allowed_prefix,
                            APR_FILEPATH_NOTRELATIVE, r->pool);
    if (rv != APR_SUCCESS || !resolved_prefix) {
        ap_log_rerror(APLOG_MARK, APLOG_ERR, rv, r,
            "mod_socket_handoff: Invalid allowed prefix %s", allowed_prefix);
        return 0;
    }

    if (resolved_prefix[strlen(resolved_prefix) - 1] == '/') {
        prefix_with_slash = resolved_prefix;
    } else {
        prefix_with_slash = apr_pstrcat(r->pool, resolved_prefix, "/", NULL);
    }

    if (strncmp(resolved_path, prefix_with_slash,
                strlen(prefix_with_slash)) != 0) {
        ap_log_rerror(APLOG_MARK, APLOG_ERR, 0, r,
            "mod_socket_handoff: Path %s resolves outside allowed prefix",
            socket_path);
        return 0;
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
    int daemon_sock;
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
    if (!validate_socket_path(socket_path, conf->allowed_socket_prefix, r)) {
        ap_remove_output_filter(f);
        return HTTP_FORBIDDEN;
    }

    /* Get optional data header */
    handoff_data = apr_table_get(r->headers_out, HANDOFF_DATA_HEADER);
    if (!handoff_data) {
        handoff_data = apr_table_get(r->err_headers_out, HANDOFF_DATA_HEADER);
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
        return HTTP_INTERNAL_SERVER_ERROR;
    }

    /* Extract the OS-level file descriptor */
    if (apr_os_sock_get(&client_fd, client_socket) != APR_SUCCESS) {
        ap_log_rerror(APLOG_MARK, APLOG_ERR, 0, r,
            "mod_socket_handoff: Could not get client fd");
        return HTTP_INTERNAL_SERVER_ERROR;
    }

    /* Connect to the streaming daemon */
    daemon_sock = connect_to_socket(socket_path, r);
    if (daemon_sock < 0) {
        return HTTP_SERVICE_UNAVAILABLE;
    }

    /* Pass the client fd to the daemon via SCM_RIGHTS */
    if (send_fd_with_data(daemon_sock, client_fd,
                          handoff_data,
                          handoff_data ? strlen(handoff_data) : 0) < 0) {
        ap_log_rerror(APLOG_MARK, APLOG_ERR, 0, r,
            "mod_socket_handoff: Failed to send fd: %s", strerror(errno));
        close(daemon_sock);
        return HTTP_INTERNAL_SERVER_ERROR;
    }

    /* Done with daemon socket */
    close(daemon_sock);

    /*
     * Swap in a dummy socket so Apache doesn't close the real one.
     * The daemon now owns the client connection.
     * This trick is borrowed from mod_proxy_fdpass.
     */
    if (swap_to_dummy_socket(c, r->pool, r) != APR_SUCCESS) {
        ap_log_rerror(APLOG_MARK, APLOG_WARNING, 0, r,
            "mod_socket_handoff: Dummy socket swap failed - "
            "connection may be closed prematurely");
    }

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

    ap_remove_output_filter(f);

    return APR_SUCCESS;
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
    {NULL}
};

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
