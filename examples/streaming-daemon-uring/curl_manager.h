/*
 * curl_manager.h - Async curl_multi integration with io_uring
 *
 * Manages non-blocking HTTP requests using libcurl's multi interface.
 * Socket events and timeouts are handled via io_uring, enabling
 * single-threaded handling of 50k+ concurrent API requests.
 */
#ifndef CURL_MANAGER_H
#define CURL_MANAGER_H

#include "daemon.h"
#include <curl/curl.h>
#include <stdbool.h>

/* Maximum concurrent curl requests */
#define CURL_MAX_CONCURRENT 50000

/* Forward declaration */
typedef struct curl_manager curl_manager_t;

/*
 * Socket tracking for io_uring poll integration.
 * Maps curl socket fds to their current poll state.
 */
typedef struct {
    int sockfd;
    int poll_mask;          /* Current poll mask (POLLIN|POLLOUT) */
    bool poll_active;       /* io_uring poll submitted */
} curl_socket_info_t;

/* Forward declaration */
struct curl_manager;

/*
 * Per-request context linking curl easy handle to connection.
 */
typedef struct {
    connection_t *conn;
    struct curl_manager *mgr;  /* For immediate io_uring writes in callback */
    CURL *easy;
    struct curl_slist *headers;
    char *post_data;        /* Owned POST body */
    bool headers_sent;      /* HTTP headers sent to client */
    int chunk_count;
} curl_request_t;

/*
 * Curl manager context.
 */
struct curl_manager {
    daemon_ctx_t *ctx;
    CURLM *multi;

    /* Timer state */
    uint32_t timer_id;
    bool timer_active;

    /* Socket tracking (sparse array indexed by fd) */
    curl_socket_info_t *sockets;
    int sockets_capacity;

    /* Active request count for stats */
    int active_requests;
};

/*
 * Initialize the curl manager.
 * Must be called after ring_init().
 */
int curl_manager_init(curl_manager_t *mgr, daemon_ctx_t *ctx);

/*
 * Cleanup the curl manager.
 */
void curl_manager_cleanup(curl_manager_t *mgr);

/*
 * Start an async LLM API request for a connection.
 *
 * The connection moves to CURL_PENDING state.
 * When the request completes, the connection moves to STREAMING or CLOSING.
 *
 * Returns:
 *   0 on success (request started)
 *  -1 on failure (connection should be closed)
 */
int curl_manager_start_request(curl_manager_t *mgr, connection_t *conn);

/*
 * Handle socket event from io_uring poll completion.
 *
 * Called when OP_CURL_POLL completes. Notifies curl of socket activity.
 *
 * sockfd: the socket that had activity
 * events: poll events (POLLIN, POLLOUT, POLLERR, etc.)
 */
void curl_manager_socket_action(curl_manager_t *mgr, int sockfd, int events);

/*
 * Handle timeout from io_uring.
 *
 * Called when OP_CURL_TIMEOUT fires.
 */
void curl_manager_timeout_action(curl_manager_t *mgr);

/*
 * Check for completed transfers and update connection states.
 *
 * Should be called after socket_action or timeout_action.
 */
void curl_manager_check_completions(curl_manager_t *mgr);

/*
 * Cancel a pending request for a connection.
 *
 * Used when a connection needs to be closed while request is in progress.
 */
void curl_manager_cancel_request(curl_manager_t *mgr, connection_t *conn);

/*
 * Get the number of active requests.
 */
static inline int curl_manager_active_count(const curl_manager_t *mgr) {
    return mgr->active_requests;
}

#endif /* CURL_MANAGER_H */
