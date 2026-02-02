/*
 * curl_manager.c - Async curl_multi integration with io_uring
 *
 * Uses libcurl's multi interface with socket and timer callbacks
 * to integrate with the io_uring event loop. This enables handling
 * 50k+ concurrent API requests in a single thread.
 */
#include "curl_manager.h"
#include "daemon.h"
#include "ring.h"
#include "write_buffer.h"
#include "streaming.h"
#include "connection.h"
#include <stdlib.h>
#include <string.h>
#include <stdio.h>
#include <poll.h>

/* API configuration (from environment) */
static const char *api_key = NULL;
static const char *api_base = NULL;
static const char *api_socket = NULL;
static const char *default_model = NULL;

/* Forward declarations */
static int socket_callback(CURL *easy, curl_socket_t sockfd, int what,
                           void *userp, void *socketp);
static int timer_callback(CURLM *multi, long timeout_ms, void *userp);
static size_t write_callback(char *ptr, size_t size, size_t nmemb, void *userdata);
static void try_immediate_write(curl_request_t *req);

/*
 * Ensure socket tracking array is large enough.
 */
static int ensure_socket_capacity(curl_manager_t *mgr, int fd) {
    if (fd < mgr->sockets_capacity) {
        return 0;
    }

    int new_cap = fd + 256;  /* Grow with some headroom */
    curl_socket_info_t *new_sockets = realloc(mgr->sockets,
                                               new_cap * sizeof(curl_socket_info_t));
    if (!new_sockets) {
        return -1;
    }

    /* Zero new entries */
    memset(new_sockets + mgr->sockets_capacity, 0,
           (new_cap - mgr->sockets_capacity) * sizeof(curl_socket_info_t));

    mgr->sockets = new_sockets;
    mgr->sockets_capacity = new_cap;
    return 0;
}

int curl_manager_init(curl_manager_t *mgr, daemon_ctx_t *ctx) {
    memset(mgr, 0, sizeof(*mgr));
    mgr->ctx = ctx;

    /* Get API configuration from environment */
    api_key = getenv("OPENAI_API_KEY");
    if (!api_key || strlen(api_key) == 0) {
        fprintf(stderr, "Warning: OPENAI_API_KEY not set\n");
    }

    api_base = getenv("OPENAI_API_BASE");
    if (!api_base) {
        api_base = "https://api.openai.com/v1";
    }

    api_socket = getenv("OPENAI_API_SOCKET");

    default_model = getenv("OPENAI_MODEL");
    if (!default_model) {
        default_model = "gpt-4o-mini";
    }

    /* Initialize curl */
    if (curl_global_init(CURL_GLOBAL_DEFAULT) != 0) {
        fprintf(stderr, "Failed to initialize libcurl\n");
        return -1;
    }

    /* Create multi handle */
    mgr->multi = curl_multi_init();
    if (!mgr->multi) {
        fprintf(stderr, "Failed to create curl multi handle\n");
        curl_global_cleanup();
        return -1;
    }

    /* Set socket callback for io_uring integration */
    curl_multi_setopt(mgr->multi, CURLMOPT_SOCKETFUNCTION, socket_callback);
    curl_multi_setopt(mgr->multi, CURLMOPT_SOCKETDATA, mgr);

    /* Set timer callback for timeout handling */
    curl_multi_setopt(mgr->multi, CURLMOPT_TIMERFUNCTION, timer_callback);
    curl_multi_setopt(mgr->multi, CURLMOPT_TIMERDATA, mgr);

    /* Enable pipelining/multiplexing for HTTP/2 */
    curl_multi_setopt(mgr->multi, CURLMOPT_PIPELINING, CURLPIPE_MULTIPLEX);

    /* Allow many concurrent connections for high-scale operation.
     * With HTTP/1.1 (no multiplexing), each request needs its own connection.
     * Default limits are too low for 50k+ concurrent requests.
     */
    curl_multi_setopt(mgr->multi, CURLMOPT_MAX_HOST_CONNECTIONS, 0);  /* Unlimited */
    curl_multi_setopt(mgr->multi, CURLMOPT_MAX_TOTAL_CONNECTIONS, 0); /* Unlimited */

    /* Allocate initial socket tracking array */
    mgr->sockets_capacity = 256;
    mgr->sockets = calloc(mgr->sockets_capacity, sizeof(curl_socket_info_t));
    if (!mgr->sockets) {
        curl_multi_cleanup(mgr->multi);
        curl_global_cleanup();
        return -1;
    }

    if (api_socket) {
        fprintf(stderr, "Curl manager initialized (base: %s, socket: %s, model: %s)\n",
                api_base, api_socket, default_model);
    } else {
        fprintf(stderr, "Curl manager initialized (base: %s, model: %s)\n",
                api_base, default_model);
    }

    return 0;
}

void curl_manager_cleanup(curl_manager_t *mgr) {
    if (mgr->multi) {
        curl_multi_cleanup(mgr->multi);
    }
    free(mgr->sockets);
    curl_global_cleanup();
}

/*
 * Socket callback - called by curl when it needs to change socket poll state.
 *
 * what:
 *   CURL_POLL_NONE   - unregister
 *   CURL_POLL_IN     - wait for read
 *   CURL_POLL_OUT    - wait for write
 *   CURL_POLL_INOUT  - wait for both
 *   CURL_POLL_REMOVE - remove socket
 */
static int socket_callback(CURL *easy, curl_socket_t sockfd, int what,
                           void *userp, void *socketp) {
    curl_manager_t *mgr = (curl_manager_t *)userp;
    (void)easy;
    (void)socketp;

    if (ensure_socket_capacity(mgr, sockfd) < 0) {
        fprintf(stderr, "Failed to track socket %d\n", sockfd);
        return 0;
    }

    curl_socket_info_t *info = &mgr->sockets[sockfd];

    /* Determine new poll mask */
    int new_mask = 0;
    if (what == CURL_POLL_IN || what == CURL_POLL_INOUT) {
        new_mask |= POLLIN;
    }
    if (what == CURL_POLL_OUT || what == CURL_POLL_INOUT) {
        new_mask |= POLLOUT;
    }

    /* Cancel existing poll if mask changed or being removed */
    if (info->poll_active && (new_mask != info->poll_mask || what == CURL_POLL_REMOVE)) {
        ring_submit_poll_cancel(mgr->ctx, sockfd);
        info->poll_active = false;
    }

    /* Submit new poll if needed */
    if (new_mask != 0 && what != CURL_POLL_REMOVE) {
        if (ring_submit_poll_add(mgr->ctx, sockfd, new_mask) == 0) {
            info->sockfd = sockfd;
            info->poll_mask = new_mask;
            info->poll_active = true;
        }
        ring_submit(mgr->ctx);
    }

    if (what == CURL_POLL_REMOVE) {
        info->sockfd = 0;
        info->poll_mask = 0;
        info->poll_active = false;
    }

    return 0;
}

/*
 * Timer callback - called by curl when the timeout value changes.
 *
 * timeout_ms:
 *   -1 - delete the timer
 *    0 - call curl_multi_socket_action immediately
 *   >0 - set timer for this many milliseconds
 */
static int timer_callback(CURLM *multi, long timeout_ms, void *userp) {
    curl_manager_t *mgr = (curl_manager_t *)userp;
    (void)multi;

    /* Cancel existing timer */
    if (mgr->timer_active) {
        ring_cancel_curl_timeout(mgr->ctx, mgr->timer_id);
        mgr->timer_active = false;
    }

    if (timeout_ms == -1) {
        /* No timer needed */
        return 0;
    }

    if (timeout_ms == 0) {
        /* Need immediate action - call socket_action right away.
         * This is critical for HTTP/2 to work properly.
         *
         * DO NOT call socket_action directly here due to reentrancy.
         * Instead, submit a very short timeout that will fire immediately
         * on the next io_uring wait.
         */
        timeout_ms = 1;

        /* Also, we need to ensure curl gets its data processed.
         * Submit a zero-timeout that fires immediately.
         */
    }

    /* Submit new timeout */
    mgr->timer_id++;
    if (ring_submit_curl_timeout(mgr->ctx, (int)timeout_ms, mgr->timer_id) == 0) {
        mgr->timer_active = true;
    }
    ring_submit(mgr->ctx);

    return 0;
}

/*
 * Parse SSE line and extract content for forwarding to client.
 */
static int process_sse_line(curl_request_t *req, const char *line) {
    /* SSE format: data: {"choices":[{"delta":{"content":"..."}}]} */
    if (strncmp(line, "data: ", 6) != 0) {
        return 0;  /* Not a data line */
    }

    const char *json = line + 6;

    /* Check for [DONE] marker */
    if (strncmp(json, "[DONE]", 6) == 0) {
        return 0;  /* Will send our own [DONE] after curl completes */
    }

    /* Find "content":" in JSON */
    const char *content_key = strstr(json, "\"content\":\"");
    if (!content_key) {
        return 0;  /* No content field */
    }

    content_key += 11;  /* Skip "content":" */

    /* Extract content until closing quote, handling escapes */
    char content[4096];
    size_t i = 0;
    while (*content_key && *content_key != '"' && i < sizeof(content) - 1) {
        if (*content_key == '\\' && *(content_key + 1)) {
            content_key++;
            switch (*content_key) {
            case 'n': content[i++] = '\n'; break;
            case 'r': content[i++] = '\r'; break;
            case 't': content[i++] = '\t'; break;
            case '"': content[i++] = '"'; break;
            case '\\': content[i++] = '\\'; break;
            default: content[i++] = *content_key; break;
            }
        } else {
            content[i++] = *content_key;
        }
        content_key++;
    }
    content[i] = '\0';

    if (i == 0) {
        return 0;  /* Empty content */
    }

    /* Format as SSE and queue to write buffer */
    char sse_buf[8192];
    int len = stream_format_sse(sse_buf, sizeof(sse_buf), content);
    if (len < 0) {
        return -1;
    }

    /* Get or create write buffer */
    connection_t *conn = req->conn;
    if (!conn->async_write_buf) {
        conn->async_write_buf = malloc(sizeof(write_buffer_t));
        if (!conn->async_write_buf) {
            return -1;
        }
        if (write_buffer_init((write_buffer_t *)conn->async_write_buf) < 0) {
            free(conn->async_write_buf);
            conn->async_write_buf = NULL;
            return -1;
        }
    }

    write_buffer_t *wb = (write_buffer_t *)conn->async_write_buf;
    if (write_buffer_append(wb, sse_buf, len) < 0) {
        return -1;  /* Buffer full */
    }

    /* Mark connection as having pending data for efficient flush */
    daemon_mark_connection_dirty(req->mgr->ctx, conn);

    /* Try to send immediately for lowest latency */
    try_immediate_write(req);

    req->chunk_count++;
    return 0;
}

/*
 * Immediately submit io_uring write if buffer has data and no write pending.
 * Called from write_callback to minimize latency - data flows directly to
 * client without waiting for flush_streaming_connections scan.
 */
static void try_immediate_write(curl_request_t *req) {
    connection_t *conn = req->conn;
    curl_manager_t *mgr = req->mgr;
    daemon_ctx_t *ctx = mgr->ctx;

    write_buffer_t *wb = (write_buffer_t *)conn->async_write_buf;
    if (!wb || !write_buffer_has_data(wb) || write_buffer_is_busy(wb)) {
        return;
    }

    size_t len;
    const char *data = write_buffer_get_next(wb, &len);
    if (!data || len == 0) {
        return;
    }

    /* Submit write + linked timeout to io_uring */
    if (ring_submit_write(ctx, conn, data, len) < 0) {
        return;  /* Will be retried by flush_streaming_connections */
    }
    if (ring_submit_linked_timeout(ctx, conn, ctx->write_timeout_ms) < 0) {
        return;
    }

    write_buffer_set_pending(wb, true);

    /* Submit to kernel immediately for lowest latency */
    ring_submit(ctx);
}

/*
 * Curl write callback - receives data from API response.
 */
static size_t write_callback(char *ptr, size_t size, size_t nmemb, void *userdata) {
    curl_request_t *req = (curl_request_t *)userdata;
    size_t bytes = size * nmemb;
    connection_t *conn = req->conn;

    /* Send HTTP headers on first data */
    if (!req->headers_sent) {
        if (!conn->async_write_buf) {
            conn->async_write_buf = malloc(sizeof(write_buffer_t));
            if (!conn->async_write_buf) {
                return 0;  /* Abort transfer */
            }
            if (write_buffer_init((write_buffer_t *)conn->async_write_buf) < 0) {
                free(conn->async_write_buf);
                conn->async_write_buf = NULL;
                return 0;  /* Abort transfer */
            }
        }

        write_buffer_t *wb = (write_buffer_t *)conn->async_write_buf;
        if (write_buffer_append(wb, SSE_HEADERS, strlen(SSE_HEADERS)) < 0) {
            return 0;
        }
        req->headers_sent = true;

        /* Mark connection as having pending data for efficient flush */
        daemon_mark_connection_dirty(req->mgr->ctx, conn);

        /* Transition to CURL_ACTIVE state */
        conn_set_state(conn, CONN_STATE_CURL_ACTIVE);

        /* Immediately start sending headers to client */
        try_immediate_write(req);
    }

    /* Accumulate data in line buffer for SSE parsing */
    if (!conn->sse_line_buf) {
        conn->sse_line_cap = 4096;
        conn->sse_line_buf = malloc(conn->sse_line_cap);
        if (!conn->sse_line_buf) {
            return 0;
        }
        conn->sse_line_len = 0;
    }

    /* Expand buffer if needed */
    if (conn->sse_line_len + bytes + 1 > conn->sse_line_cap) {
        size_t new_cap = conn->sse_line_cap * 2;
        if (new_cap < conn->sse_line_len + bytes + 1) {
            new_cap = conn->sse_line_len + bytes + 1;
        }
        char *new_buf = realloc(conn->sse_line_buf, new_cap);
        if (!new_buf) {
            return 0;
        }
        conn->sse_line_buf = new_buf;
        conn->sse_line_cap = new_cap;
    }

    /* Append new data */
    memcpy(conn->sse_line_buf + conn->sse_line_len, ptr, bytes);
    conn->sse_line_len += bytes;
    conn->sse_line_buf[conn->sse_line_len] = '\0';

    /* Process complete lines */
    char *line_start = conn->sse_line_buf;
    char *newline;
    while ((newline = strchr(line_start, '\n')) != NULL) {
        *newline = '\0';
        if (newline > line_start && *(newline - 1) == '\r') {
            *(newline - 1) = '\0';
        }

        if (process_sse_line(req, line_start) < 0) {
            return 0;  /* Error processing line */
        }

        line_start = newline + 1;
    }

    /* Move remaining partial line to start */
    if (line_start != conn->sse_line_buf) {
        size_t remaining = conn->sse_line_len - (line_start - conn->sse_line_buf);
        if (remaining > 0) {
            memmove(conn->sse_line_buf, line_start, remaining);
        }
        conn->sse_line_len = remaining;
        conn->sse_line_buf[conn->sse_line_len] = '\0';
    }

    /* Immediately submit io_uring write to minimize TTFB.
     * This eliminates the latency of waiting for flush_streaming_connections.
     */
    try_immediate_write(req);

    return bytes;
}

/*
 * Build JSON request body for OpenAI API.
 */
static char *build_request_body(connection_t *conn, const char *model, int max_tokens) {
    const char *prompt = conn->data.prompt ? conn->data.prompt : "Hello";

    /* Escape prompt for JSON */
    size_t prompt_len = strlen(prompt);
    char *escaped = malloc(prompt_len * 6 + 1);  /* Worst case: all \uXXXX */
    if (!escaped) {
        return NULL;
    }

    char *out = escaped;
    const char *p = prompt;
    while (*p) {
        switch (*p) {
        case '"':  *out++ = '\\'; *out++ = '"'; break;
        case '\\': *out++ = '\\'; *out++ = '\\'; break;
        case '\n': *out++ = '\\'; *out++ = 'n'; break;
        case '\r': *out++ = '\\'; *out++ = 'r'; break;
        case '\t': *out++ = '\\'; *out++ = 't'; break;
        default:
            if ((unsigned char)*p < 32) {
                out += sprintf(out, "\\u%04x", (unsigned char)*p);
            } else {
                *out++ = *p;
            }
            break;
        }
        p++;
    }
    *out = '\0';

    /* Build JSON body */
    size_t body_size = strlen(escaped) + 256;
    char *body = malloc(body_size);
    if (!body) {
        free(escaped);
        return NULL;
    }

    snprintf(body, body_size,
        "{"
        "\"model\":\"%s\","
        "\"messages\":[{\"role\":\"user\",\"content\":\"%s\"}],"
        "\"max_tokens\":%d,"
        "\"stream\":true"
        "}",
        model, escaped, max_tokens);

    free(escaped);
    return body;
}

int curl_manager_start_request(curl_manager_t *mgr, connection_t *conn) {
    if (!api_key) {
        fprintf(stderr, "No API key configured\n");
        return -1;
    }

    /* Create request context */
    curl_request_t *req = malloc(sizeof(curl_request_t));
    if (!req) {
        return -1;
    }
    memset(req, 0, sizeof(*req));
    req->conn = conn;
    req->mgr = mgr;

    /* Create easy handle */
    req->easy = curl_easy_init();
    if (!req->easy) {
        free(req);
        return -1;
    }

    /* Build URL - if api_base ends with /v1 or /v1/, use that; otherwise append /v1 */
    char url[512];
    const char *v1_suffix = "/v1";
    size_t base_len = strlen(api_base);

    /* Check if api_base already ends with /v1 */
    if (base_len >= 3 && strcmp(api_base + base_len - 3, "/v1") == 0) {
        snprintf(url, sizeof(url), "%s/chat/completions", api_base);
    } else if (base_len >= 4 && strcmp(api_base + base_len - 4, "/v1/") == 0) {
        snprintf(url, sizeof(url), "%schat/completions", api_base);
    } else {
        snprintf(url, sizeof(url), "%s%s/chat/completions", api_base, v1_suffix);
    }

    /* Get model and max_tokens */
    const char *model = conn->data.model ? conn->data.model : default_model;
    int max_tokens = conn->data.max_tokens > 0 ? conn->data.max_tokens : 1024;

    /* Build request body */
    req->post_data = build_request_body(conn, model, max_tokens);
    if (!req->post_data) {
        curl_easy_cleanup(req->easy);
        free(req);
        return -1;
    }

    /* Set up headers */
    req->headers = curl_slist_append(NULL, "Content-Type: application/json");
    char auth[256];
    snprintf(auth, sizeof(auth), "Authorization: Bearer %s", api_key);
    req->headers = curl_slist_append(req->headers, auth);

    /* Configure curl */
    curl_easy_setopt(req->easy, CURLOPT_URL, url);
    curl_easy_setopt(req->easy, CURLOPT_HTTPHEADER, req->headers);
    curl_easy_setopt(req->easy, CURLOPT_POSTFIELDS, req->post_data);
    curl_easy_setopt(req->easy, CURLOPT_WRITEFUNCTION, write_callback);
    curl_easy_setopt(req->easy, CURLOPT_WRITEDATA, req);
    curl_easy_setopt(req->easy, CURLOPT_PRIVATE, req);
    curl_easy_setopt(req->easy, CURLOPT_TIMEOUT, 300L);
    curl_easy_setopt(req->easy, CURLOPT_CONNECTTIMEOUT, 10L);

    /* Use Unix socket if configured */
    if (api_socket) {
        curl_easy_setopt(req->easy, CURLOPT_UNIX_SOCKET_PATH, api_socket);
    }

    /* Enable HTTP/2 multiplexing for efficient connection reuse.
     * For HTTPS: use CURL_HTTP_VERSION_2TLS (negotiate via ALPN)
     * For HTTP: use CURL_HTTP_VERSION_2_PRIOR_KNOWLEDGE (h2c)
     * PIPEWAIT makes curl wait for a connection to multiplex on.
     */
    if (strncmp(url, "https://", 8) == 0) {
        /* HTTPS - negotiate HTTP/2 via ALPN */
        curl_easy_setopt(req->easy, CURLOPT_HTTP_VERSION, CURL_HTTP_VERSION_2TLS);
    } else {
        /* HTTP - use h2c prior knowledge */
        curl_easy_setopt(req->easy, CURLOPT_HTTP_VERSION, CURL_HTTP_VERSION_2_PRIOR_KNOWLEDGE);
    }
    curl_easy_setopt(req->easy, CURLOPT_PIPEWAIT, 1L);

    /* Store easy handle in connection for cancellation */
    conn->curl_easy = req->easy;

    /* Update state */
    conn_set_state(conn, CONN_STATE_CURL_PENDING);

    /* Add to multi handle */
    CURLMcode mc = curl_multi_add_handle(mgr->multi, req->easy);
    if (mc != CURLM_OK) {
        fprintf(stderr, "curl_multi_add_handle failed: %s\n", curl_multi_strerror(mc));
        curl_slist_free_all(req->headers);
        curl_easy_cleanup(req->easy);
        free(req->post_data);
        free(req);
        conn->curl_easy = NULL;
        return -1;
    }

    mgr->active_requests++;

    /* Kick curl_multi to start processing the new handle immediately.
     * This triggers the socket and timer callbacks to set up io_uring events.
     */
    int running_handles;
    curl_multi_socket_action(mgr->multi, CURL_SOCKET_TIMEOUT, 0, &running_handles);

    return 0;
}

void curl_manager_socket_action(curl_manager_t *mgr, int sockfd, int events) {
    int curl_events = 0;
    if (events & POLLIN) curl_events |= CURL_CSELECT_IN;
    if (events & POLLOUT) curl_events |= CURL_CSELECT_OUT;
    if (events & (POLLERR | POLLHUP)) curl_events |= CURL_CSELECT_ERR;

    int running_handles;

    /* Tell curl about the socket event */
    curl_multi_socket_action(mgr->multi, sockfd, curl_events, &running_handles);

    /* Also call with CURL_SOCKET_TIMEOUT to let curl do any pending work.
     * This is essential for HTTP/2 - curl needs this kick to process data.
     */
    curl_multi_socket_action(mgr->multi, CURL_SOCKET_TIMEOUT, 0, &running_handles);
}

void curl_manager_timeout_action(curl_manager_t *mgr) {
    mgr->timer_active = false;

    int running_handles;
    curl_multi_socket_action(mgr->multi, CURL_SOCKET_TIMEOUT, 0, &running_handles);
}

void curl_manager_check_completions(curl_manager_t *mgr) {
    CURLMsg *msg;
    int msgs_left;

    while ((msg = curl_multi_info_read(mgr->multi, &msgs_left)) != NULL) {
        if (msg->msg != CURLMSG_DONE) {
            continue;
        }

        CURL *easy = msg->easy_handle;
        CURLcode result = msg->data.result;

        /* Get request context */
        curl_request_t *req = NULL;
        curl_easy_getinfo(easy, CURLINFO_PRIVATE, &req);
        if (!req) {
            continue;
        }

        connection_t *conn = req->conn;
        long http_code = 0;
        curl_easy_getinfo(easy, CURLINFO_RESPONSE_CODE, &http_code);

        /* Remove from multi handle */
        curl_multi_remove_handle(mgr->multi, easy);
        mgr->active_requests--;

        /* Determine outcome */
        bool success = (result == CURLE_OK && (http_code == 200 || req->headers_sent));

        if (success && req->headers_sent) {
            /* Queue [DONE] marker */
            write_buffer_t *wb = (write_buffer_t *)conn->async_write_buf;
            if (wb) {
                write_buffer_append(wb, SSE_DONE, strlen(SSE_DONE));
                /* Mark connection as having pending data for efficient flush */
                daemon_mark_connection_dirty(mgr->ctx, conn);
            }

            /* Move to CLOSING state - [DONE] is the last data to send */
            conn_set_state(conn, CONN_STATE_CLOSING);
        } else {
            /* Request failed */
            if (result != CURLE_OK) {
                fprintf(stderr, "Curl error: %s\n", curl_easy_strerror(result));
            } else if (http_code != 200) {
                fprintf(stderr, "API error %ld\n", http_code);
            }
            conn_set_state(conn, CONN_STATE_CLOSING);
        }

        /* Clear connection's curl handle reference */
        conn->curl_easy = NULL;

        /* Cleanup request and curl easy handle.
         * The handle has been removed from multi above, so it's safe to clean up.
         */
        curl_slist_free_all(req->headers);
        free(req->post_data);
        free(req);
        curl_easy_cleanup(easy);
    }
}

void curl_manager_cancel_request(curl_manager_t *mgr, connection_t *conn) {
    if (!conn->curl_easy) {
        return;
    }

    CURL *easy = (CURL *)conn->curl_easy;

    /* Get request context for cleanup */
    curl_request_t *req = NULL;
    curl_easy_getinfo(easy, CURLINFO_PRIVATE, &req);

    /* Remove from multi handle */
    curl_multi_remove_handle(mgr->multi, easy);
    mgr->active_requests--;

    /* Cleanup */
    if (req) {
        curl_slist_free_all(req->headers);
        free(req->post_data);
        free(req);
    }
    curl_easy_cleanup(easy);

    conn->curl_easy = NULL;
}
