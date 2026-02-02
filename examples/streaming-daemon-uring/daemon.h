/*
 * daemon.h - Main daemon types and constants
 */
#ifndef DAEMON_H
#define DAEMON_H

#define _GNU_SOURCE
#include <liburing.h>
#include <stdint.h>
#include <stddef.h>
#include <stdatomic.h>
#include <stdbool.h>
#include <time.h>

/* Version */
#define DAEMON_VERSION "0.4.0"

/* Default configuration */
#define DEFAULT_SOCKET_PATH      "/var/run/streaming-daemon.sock"
#define DEFAULT_SOCKET_MODE      0660
#define DEFAULT_MAX_CONNECTIONS  10000
#define DEFAULT_HANDOFF_TIMEOUT_MS  5000
#define DEFAULT_WRITE_TIMEOUT_MS    30000
#define DEFAULT_SHUTDOWN_TIMEOUT_S  120
#define DEFAULT_MESSAGE_DELAY_MS    50
#define DEFAULT_RING_SIZE           8192  /* Increased for high concurrency */

/* Buffer sizes */
/* Tiered handoff buffer sizes to reduce memory usage.
 * Start small (4KB), grow on demand up to 64KB.
 * 90%+ of requests fit in 4KB, saving significant memory at scale.
 */
#define HANDOFF_TIER1_SIZE       4096    /* Initial size - covers 90%+ of requests */
#define HANDOFF_TIER2_SIZE       16384   /* Medium prompts */
#define HANDOFF_TIER3_SIZE       65536   /* Large prompts (max) */
#define MAX_HANDOFF_DATA_SIZE    HANDOFF_TIER3_SIZE
#define CONTROL_MSG_SIZE         256

/* Thread pool configuration */
#define DEFAULT_POOL_SIZE        8
#define MAX_POOL_SIZE            64

/* Connection states */
typedef enum {
    CONN_STATE_UNUSED = 0,
    CONN_STATE_ACCEPTING,
    CONN_STATE_RECEIVING_FD,
    CONN_STATE_PARSING,
    CONN_STATE_LLM_QUEUED,      /* Waiting in thread pool queue (legacy) */
    CONN_STATE_LLM_PENDING,     /* Being processed by worker thread (legacy) */
    CONN_STATE_CURL_PENDING,    /* Curl request submitted (async mode) */
    CONN_STATE_CURL_ACTIVE,     /* Receiving curl response (async mode) */
    CONN_STATE_STREAMING,       /* Sending to client via io_uring */
    CONN_STATE_STREAMED,        /* Real-time streaming done, just close */
    CONN_STATE_CLOSING
} conn_state_t;

/* io_uring operation types - stored in user_data */
typedef enum {
    OP_ACCEPT = 1,
    OP_RECV_FD,
    OP_WRITE,
    OP_TIMEOUT,
    OP_CANCEL,
    OP_EVENTFD,         /* Thread pool completion signal */
    OP_CURL_POLL,       /* Multishot poll for curl sockets */
    OP_CURL_TIMEOUT,    /* Curl timer expiry */
    OP_POLL_CANCEL      /* Cancel active poll */
} op_type_t;

/* User data for io_uring completions */
typedef struct {
    op_type_t op;
    uint32_t conn_id;
} uring_data_t;

/* Parsed handoff data from PHP */
typedef struct {
    int64_t user_id;
    char *prompt;       /* Points into handoff_buf */
    char *model;        /* Points into handoff_buf */
    int max_tokens;
} handoff_data_t;

/* Per-connection state */
typedef struct connection {
    uint32_t id;
    _Atomic conn_state_t state;  /* Atomic for thread-safe access from pool workers */

    /* File descriptors */
    int unix_fd;        /* Connection from Apache (for receiving fd) */
    int client_fd;      /* Client TCP socket received via SCM_RIGHTS */

    /* Handoff data - tiered allocation to save memory */
    char *handoff_buf;  /* Allocated on-demand, starts at TIER1, grows as needed */
    size_t handoff_len; /* Actual data received */
    size_t handoff_cap; /* Current buffer capacity */
    handoff_data_t data;

    /* Streaming state */
    int stream_index;   /* Current SSE message index */
    size_t bytes_sent;

    /* LLM response streaming */
    bool use_llm_response;     /* True if handoff_buf contains LLM response to stream */
    size_t llm_response_pos;   /* Current position in LLM response for chunking */

    /* Timing */
    struct timespec start_time;
    struct timespec last_write;

    /* Write buffer (for current SSE message - legacy single-buffer mode) */
    char *write_buf;
    size_t write_len;
    size_t write_offset;

    /* Async write buffer (for curl_multi mode) */
    struct write_buffer *async_write_buf;

    /* Curl easy handle for async mode (NULL if not in use) */
    void *curl_easy;

    /* Line buffer for SSE parsing (curl_multi mode) */
    char *sse_line_buf;
    size_t sse_line_len;
    size_t sse_line_cap;

    /* Timeout spec for linked timeouts (per-connection to avoid races) */
    struct __kernel_timespec timeout_spec;

    /* Linked list for free pool */
    struct connection *next_free;

    /* Dirty list for connections with pending writes (intrusive linked list) */
    struct connection *dirty_next;
    bool on_dirty_list;
} connection_t;

/* Forward declarations */
struct pool_ctx;
struct curl_manager;
struct write_buffer;

/* Daemon context */
typedef struct {
    /* io_uring */
    struct io_uring ring;
    bool ring_initialized;

    /* Listening socket */
    int listen_fd;
    char *socket_path;

    /* Connection pool */
    connection_t *connections;
    connection_t *free_list;
    int max_connections;
    atomic_int active_connections;

    /* Dirty list: connections with pending write data (for efficient flush) */
    connection_t *dirty_head;
    connection_t *dirty_tail;

    /* Thread pool for LLM API calls (legacy blocking mode) */
    struct pool_ctx *pool;
    int pool_size;

    /* Curl multi manager (async mode) */
    struct curl_manager *curl_mgr;
    bool use_curl_multi;        /* Use async curl_multi instead of thread pool */

    /* Configuration */
    int handoff_timeout_ms;
    int write_timeout_ms;
    int shutdown_timeout_s;
    int message_delay_ms;
    int socket_mode;
    bool benchmark_mode;
    bool use_thread_pool;       /* Enable thread pool for LLM calls */
    bool use_sqpoll;            /* Enable SQPOLL (kernel-side polling) */
    int metrics_port;           /* Prometheus metrics port (0 = disabled) */

    /* Shutdown */
    atomic_bool shutdown_requested;

    /* Stats (for benchmark mode) */
    atomic_uint_fast64_t total_started;
    atomic_uint_fast64_t total_completed;
    atomic_uint_fast64_t total_failed;
    atomic_uint_fast64_t total_bytes;
    atomic_int peak_connections;
} daemon_ctx_t;

/* Global daemon context */
extern daemon_ctx_t *g_ctx;

/* Function prototypes - daemon.c */
void daemon_init(daemon_ctx_t *ctx);
void daemon_cleanup(daemon_ctx_t *ctx);
int daemon_run(daemon_ctx_t *ctx);
void daemon_request_shutdown(daemon_ctx_t *ctx);

/* Dirty list management - for efficient flush of pending writes */
void daemon_mark_connection_dirty(daemon_ctx_t *ctx, connection_t *conn);
void daemon_unmark_connection_dirty(daemon_ctx_t *ctx, connection_t *conn);

#endif /* DAEMON_H */
