/*
 * daemon.c - Main daemon entry point and event loop
 *
 * High-performance C streaming daemon using Linux io_uring.
 * Receives client sockets from Apache via SCM_RIGHTS and streams SSE responses.
 */
#define _GNU_SOURCE
#include "daemon.h"
#include "ring.h"
#include "server.h"
#include "handoff.h"
#include "connection.h"
#include "streaming.h"
#include "pool.h"
#include "backend.h"
#include "metrics.h"
#include "config.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <signal.h>
#include <errno.h>
#include <getopt.h>
#include <sys/resource.h>
#include <time.h>

/* Global daemon context for signal handler */
daemon_ctx_t *g_ctx = NULL;

/* Thread pool context */
static pool_ctx_t g_pool;

/* Per-connection msghdr storage for io_uring recvmsg */
typedef struct {
    struct msghdr msg;
    struct iovec iov;
    char ctrl_buf[CONTROL_MSG_SIZE];
} recv_ctx_t;

static recv_ctx_t *recv_contexts = NULL;

static void signal_handler(int sig) {
    (void)sig;
    if (g_ctx) {
        atomic_store(&g_ctx->shutdown_requested, true);
    }
}

static void setup_signals(void) {
    struct sigaction sa = {0};
    sa.sa_handler = signal_handler;
    sigemptyset(&sa.sa_mask);

    sigaction(SIGINT, &sa, NULL);
    sigaction(SIGTERM, &sa, NULL);

    /* Ignore SIGPIPE - we handle write errors via io_uring */
    signal(SIGPIPE, SIG_IGN);
}

static void increase_fd_limit(int max_connections) {
    struct rlimit rlim;
    if (getrlimit(RLIMIT_NOFILE, &rlim) < 0) {
        perror("getrlimit");
        return;
    }

    /* Each connection needs ~3 fds */
    rlim_t desired = (rlim_t)(max_connections * 3 + 1000);
    if (rlim.rlim_cur >= desired) {
        return;
    }

    rlim.rlim_cur = desired;
    if (rlim.rlim_max < desired) {
        rlim.rlim_max = desired;
    }

    if (setrlimit(RLIMIT_NOFILE, &rlim) < 0) {
        fprintf(stderr, "Warning: could not increase fd limit to %lu: %s\n",
                (unsigned long)desired, strerror(errno));
    } else {
        fprintf(stderr, "Increased fd limit to %lu\n", (unsigned long)desired);
    }
}

static void print_usage(const char *prog) {
    fprintf(stderr, "Usage: %s [options]\n\n", prog);
    fprintf(stderr, "Options:\n");
    fprintf(stderr, "  --socket PATH       Unix socket path (default: %s)\n",
            DEFAULT_SOCKET_PATH);
    fprintf(stderr, "  --socket-mode MODE  Socket permissions in octal (default: 0660)\n");
    fprintf(stderr, "  --max-connections N Maximum concurrent connections (default: %d)\n",
            DEFAULT_MAX_CONNECTIONS);
    fprintf(stderr, "  --delay MS          Delay between SSE messages (default: %d)\n",
            DEFAULT_MESSAGE_DELAY_MS);
    fprintf(stderr, "  --pool-size N       Thread pool size (default: %d)\n",
            DEFAULT_POOL_SIZE);
    fprintf(stderr, "  --thread-pool       Enable thread pool for LLM calls\n");
    fprintf(stderr, "  --sqpoll            Enable SQPOLL (kernel polling, requires root)\n");
    fprintf(stderr, "  --metrics PORT      Enable Prometheus metrics on PORT\n");
    fprintf(stderr, "  --config FILE       Load configuration from TOML file\n");
    fprintf(stderr, "  --benchmark         Enable benchmark mode\n");
    fprintf(stderr, "  --help              Show this help\n");
    fprintf(stderr, "  --version           Show version\n");
}

static void print_version(void) {
    fprintf(stderr, "streaming-daemon-uring %s\n", DAEMON_VERSION);
}

static int parse_args(int argc, char **argv, daemon_ctx_t *ctx) {
    static struct option long_options[] = {
        {"socket",          required_argument, NULL, 's'},
        {"socket-mode",     required_argument, NULL, 'm'},
        {"max-connections", required_argument, NULL, 'c'},
        {"delay",           required_argument, NULL, 'd'},
        {"pool-size",       required_argument, NULL, 'p'},
        {"thread-pool",     no_argument,       NULL, 't'},
        {"sqpoll",          no_argument,       NULL, 'q'},
        {"metrics",         required_argument, NULL, 'M'},
        {"config",          required_argument, NULL, 'C'},
        {"benchmark",       no_argument,       NULL, 'b'},
        {"help",            no_argument,       NULL, 'h'},
        {"version",         no_argument,       NULL, 'v'},
        {NULL, 0, NULL, 0}
    };

    /* First pass: find --config option and load it before processing other args.
     * This ensures CLI args always override config file settings.
     * If no --config is specified, load from default locations. */
    int saved_optind = optind;
    int opt;
    int found_config = 0;
    while ((opt = getopt_long(argc, argv, "s:m:c:d:p:tqM:C:bhv", long_options, NULL)) != -1) {
        if (opt == 'C') {
            if (config_load(ctx, optarg) < 0) {
                return -1;
            }
            found_config = 1;
            break; /* Found and loaded config, stop first pass */
        }
    }
    if (!found_config) {
        config_load_default(ctx);
    }
    optind = saved_optind; /* Reset for second pass */

    /* Second pass: process all CLI args (overriding config file) */
    while ((opt = getopt_long(argc, argv, "s:m:c:d:p:tqM:C:bhv", long_options, NULL)) != -1) {
        switch (opt) {
        case 's':
            free(ctx->socket_path);
            ctx->socket_path = strdup(optarg);
            break;
        case 'm':
            ctx->socket_mode = (int)strtol(optarg, NULL, 8);
            break;
        case 'c':
            ctx->max_connections = atoi(optarg);
            if (ctx->max_connections < 1) {
                fprintf(stderr, "Invalid max-connections: %s\n", optarg);
                return -1;
            }
            break;
        case 'd':
            ctx->message_delay_ms = atoi(optarg);
            break;
        case 'p':
            ctx->pool_size = atoi(optarg);
            if (ctx->pool_size < 1 || ctx->pool_size > MAX_POOL_SIZE) {
                fprintf(stderr, "Invalid pool-size: %s (1-%d)\n", optarg, MAX_POOL_SIZE);
                return -1;
            }
            break;
        case 't':
            ctx->use_thread_pool = true;
            break;
        case 'q':
            ctx->use_sqpoll = true;
            break;
        case 'M':
            ctx->metrics_port = atoi(optarg);
            if (ctx->metrics_port < 0 || ctx->metrics_port > 65535) {
                fprintf(stderr, "Invalid metrics port: %s\n", optarg);
                return -1;
            }
            break;
        case 'C':
            /* Already loaded in first pass, skip */
            break;
        case 'b':
            ctx->benchmark_mode = true;
            break;
        case 'h':
            print_usage(argv[0]);
            exit(0);
        case 'v':
            print_version();
            exit(0);
        default:
            print_usage(argv[0]);
            return -1;
        }
    }

    return 0;
}

void daemon_init(daemon_ctx_t *ctx) {
    memset(ctx, 0, sizeof(*ctx));
    ctx->socket_path = strdup(DEFAULT_SOCKET_PATH);
    ctx->socket_mode = DEFAULT_SOCKET_MODE;
    ctx->max_connections = DEFAULT_MAX_CONNECTIONS;
    ctx->handoff_timeout_ms = DEFAULT_HANDOFF_TIMEOUT_MS;
    ctx->write_timeout_ms = DEFAULT_WRITE_TIMEOUT_MS;
    ctx->shutdown_timeout_s = DEFAULT_SHUTDOWN_TIMEOUT_S;
    ctx->message_delay_ms = DEFAULT_MESSAGE_DELAY_MS;
    ctx->pool_size = DEFAULT_POOL_SIZE;
    ctx->use_thread_pool = false;  /* Disabled by default for Phase 1 compat */
    ctx->listen_fd = -1;
    atomic_store(&ctx->shutdown_requested, false);
    atomic_store(&ctx->active_connections, 0);
    atomic_store(&ctx->peak_connections, 0);
    atomic_store(&ctx->total_started, 0);
    atomic_store(&ctx->total_completed, 0);
    atomic_store(&ctx->total_failed, 0);
    atomic_store(&ctx->total_bytes, 0);
}

void daemon_cleanup(daemon_ctx_t *ctx) {
    metrics_stop();
    if (ctx->use_thread_pool && ctx->pool) {
        pool_cleanup(ctx->pool);
    }
    backend_cleanup();
    ring_cleanup(ctx);
    conn_pool_cleanup(ctx);
    server_close_listener(ctx);
    free(ctx->socket_path);
    free(recv_contexts);
    recv_contexts = NULL;
}

void daemon_request_shutdown(daemon_ctx_t *ctx) {
    atomic_store(&ctx->shutdown_requested, true);
}

/* Handle accept completion */
static void handle_accept(daemon_ctx_t *ctx, struct io_uring_cqe *cqe) {
    int fd = cqe->res;

    /* Check for multishot exhaustion */
    if (!(cqe->flags & IORING_CQE_F_MORE)) {
        /* Resubmit accept */
        if (!atomic_load(&ctx->shutdown_requested)) {
            int rc = ring_submit_accept(ctx);
            if (rc == -EAGAIN) {
                /* SQ ring temporarily full: retry a few times */
                for (int i = 0; i < 3 && rc == -EAGAIN; i++) {
                    rc = ring_submit_accept(ctx);
                }
            }
            if (rc < 0 && rc != -EAGAIN && rc != -ECANCELED) {
                fprintf(stderr, "Failed to resubmit accept: %s\n", strerror(-rc));
            }
        }
    }

    if (fd < 0) {
        if (fd != -EAGAIN && fd != -ECANCELED) {
            fprintf(stderr, "Accept error: %s\n", strerror(-fd));
        }
        return;
    }

    /* Allocate connection */
    connection_t *conn = conn_alloc(ctx);
    if (!conn) {
        fprintf(stderr, "Connection limit reached, rejecting\n");
        close(fd);
        return;
    }

    conn->unix_fd = fd;
    atomic_store(&conn->state, CONN_STATE_RECEIVING_FD);
    clock_gettime(CLOCK_MONOTONIC, &conn->start_time);

    if (ctx->benchmark_mode) {
        atomic_fetch_add(&ctx->total_started, 1);
    }

    /* Prepare recvmsg for SCM_RIGHTS */
    recv_ctx_t *rctx = &recv_contexts[conn->id];
    handoff_prepare_msghdr(&rctx->msg, &rctx->iov,
                           conn->handoff_buf, MAX_HANDOFF_DATA_SIZE,
                           rctx->ctrl_buf, sizeof(rctx->ctrl_buf));

    /* Submit recvmsg */
    if (ring_submit_recvmsg(ctx, conn, &rctx->msg) < 0) {
        fprintf(stderr, "Failed to submit recvmsg\n");
        conn_free(ctx, conn);
        return;
    }
}

/* Start streaming for a connection (send headers) */
static void start_streaming(daemon_ctx_t *ctx, connection_t *conn) {
    /* Send HTTP headers first */
    static const char headers[] = SSE_HEADERS;
    conn->write_len = sizeof(headers) - 1;
    conn->write_offset = 0;
    if (ring_submit_write(ctx, conn, headers, sizeof(headers) - 1) < 0) {
        fprintf(stderr, "Failed to submit header write\n");
        if (ctx->benchmark_mode) {
            atomic_fetch_add(&ctx->total_failed, 1);
        }
        conn_free(ctx, conn);
        return;
    }

    /* Link timeout */
    if (ring_submit_linked_timeout(ctx, conn, ctx->write_timeout_ms) < 0) {
        fprintf(stderr, "Failed to submit linked timeout\n");
        if (ctx->benchmark_mode) {
            atomic_fetch_add(&ctx->total_failed, 1);
        }
        conn_free(ctx, conn);
        return;
    }
}

/* Handle recvmsg completion (fd received) */
static void handle_recv_fd(daemon_ctx_t *ctx, connection_t *conn,
                           struct io_uring_cqe *cqe) {
    if (cqe->res < 0) {
        if (cqe->res != -ECANCELED) {
            fprintf(stderr, "recvmsg error: %s\n", strerror(-cqe->res));
        }
        if (ctx->benchmark_mode) {
            atomic_fetch_add(&ctx->total_failed, 1);
        }
        conn_free(ctx, conn);
        return;
    }

    conn->handoff_len = cqe->res;

    /* Extract fd from control message */
    recv_ctx_t *rctx = &recv_contexts[conn->id];
    int client_fd = handoff_extract_fd(&rctx->msg);
    if (client_fd < 0) {
        fprintf(stderr, "No fd in control message\n");
        if (ctx->benchmark_mode) {
            atomic_fetch_add(&ctx->total_failed, 1);
        }
        conn_free(ctx, conn);
        return;
    }

    conn->client_fd = client_fd;

    /* Close Unix fd - we don't need it anymore */
    close(conn->unix_fd);
    conn->unix_fd = -1;

    /* Validate socket */
    if (handoff_validate_socket(client_fd) < 0) {
        fprintf(stderr, "Invalid socket type\n");
        if (ctx->benchmark_mode) {
            atomic_fetch_add(&ctx->total_failed, 1);
        }
        conn_free(ctx, conn);
        return;
    }

    /* Parse handoff data */
    atomic_store(&conn->state, CONN_STATE_PARSING);
    handoff_parse_data(conn->handoff_buf, conn->handoff_len, &conn->data);

    if (ctx->use_thread_pool) {
        /* Submit to thread pool for LLM processing */
        atomic_store(&conn->state, CONN_STATE_LLM_QUEUED);
        if (pool_submit(ctx->pool, conn) < 0) {
            fprintf(stderr, "Failed to submit to thread pool\n");
            if (ctx->benchmark_mode) {
                atomic_fetch_add(&ctx->total_failed, 1);
            }
            conn_free(ctx, conn);
        }
    } else {
        /* Direct streaming (Phase 1 mode) */
        atomic_store(&conn->state, CONN_STATE_STREAMING);
        stream_start(conn);
        start_streaming(ctx, conn);
    }
}

/* Handle eventfd signal from thread pool */
static void handle_eventfd(daemon_ctx_t *ctx) {
    /* Read and discard eventfd counter */
    uint64_t val;
    ssize_t ret = read(pool_get_eventfd(ctx->pool), &val, sizeof(val));
    (void)ret;

    /* Process all completed connections */
    connection_t *conn;
    while ((conn = pool_get_completion(ctx->pool)) != NULL) {
        conn_state_t state = atomic_load(&conn->state);
        if (state == CONN_STATE_STREAMING) {
            start_streaming(ctx, conn);
        } else if (state == CONN_STATE_CLOSING) {
            /* Backend failed */
            if (ctx->benchmark_mode) {
                atomic_fetch_add(&ctx->total_failed, 1);
            }
            conn_free(ctx, conn);
        }
    }
}

/* Handle write completion and send next message */
static void handle_write(daemon_ctx_t *ctx, connection_t *conn,
                         struct io_uring_cqe *cqe) {
    if (cqe->res < 0) {
        if (cqe->res != -ECANCELED && cqe->res != -ETIMEDOUT) {
            /* EPIPE, ECONNRESET, etc - client disconnected */
            if (cqe->res != -EPIPE && cqe->res != -ECONNRESET) {
                fprintf(stderr, "Write error: %s\n", strerror(-cqe->res));
            }
        }
        if (ctx->benchmark_mode) {
            atomic_fetch_add(&ctx->total_failed, 1);
        }
        conn_free(ctx, conn);
        return;
    }

    conn->bytes_sent += cqe->res;
    conn->write_offset += cqe->res;

    /* Handle short writes - resubmit remaining bytes */
    if (conn->write_len > 0 && conn->write_offset < conn->write_len) {
        /* Short write - submit remaining portion */
        const char *buf;
        if (conn->write_buf) {
            buf = conn->write_buf + conn->write_offset;
        } else {
            /* Static buffer (headers or done marker) - can't easily handle short writes
             * for static buffers, but they're small so this is unlikely */
            fprintf(stderr, "Short write on static buffer, dropping connection\n");
            if (ctx->benchmark_mode) {
                atomic_fetch_add(&ctx->total_failed, 1);
            }
            conn_free(ctx, conn);
            return;
        }

        size_t remaining = conn->write_len - conn->write_offset;
        if (ring_submit_write(ctx, conn, buf, remaining) < 0) {
            if (ctx->benchmark_mode) {
                atomic_fetch_add(&ctx->total_failed, 1);
            }
            conn_free(ctx, conn);
            return;
        }
        if (ring_submit_linked_timeout(ctx, conn, ctx->write_timeout_ms) < 0) {
            if (ctx->benchmark_mode) {
                atomic_fetch_add(&ctx->total_failed, 1);
            }
            conn_free(ctx, conn);
            return;
        }
        return; /* Wait for next completion */
    }

    /* Write complete - reset tracking and free buffer */
    conn->write_len = 0;
    conn->write_offset = 0;
    if (conn->write_buf) {
        free(conn->write_buf);
        conn->write_buf = NULL;
    }

    /* Check if we're done (after sending [DONE] marker) */
    if (atomic_load(&conn->state) == CONN_STATE_CLOSING) {
        /* [DONE] was sent, now complete */
        if (ctx->benchmark_mode) {
            atomic_fetch_add(&ctx->total_completed, 1);
            atomic_fetch_add(&ctx->total_bytes, conn->bytes_sent);
        }
        conn_free(ctx, conn);
        return;
    }

    /* Get next message */
    size_t msg_len;
    const char *msg = stream_next_message(conn, &msg_len);

    if (msg == NULL) {
        /* Stream complete - send [DONE] marker */
        static const char done[] = SSE_DONE;

        /* Mark as closing BEFORE submitting write */
        atomic_store(&conn->state, CONN_STATE_CLOSING);

        conn->write_len = sizeof(done) - 1;
        conn->write_offset = 0;
        if (ring_submit_write(ctx, conn, done, sizeof(done) - 1) < 0) {
            if (ctx->benchmark_mode) {
                atomic_fetch_add(&ctx->total_completed, 1);
                atomic_fetch_add(&ctx->total_bytes, conn->bytes_sent);
            }
            conn_free(ctx, conn);
            return;
        }
        if (ring_submit_linked_timeout(ctx, conn, ctx->write_timeout_ms) < 0) {
            if (ctx->benchmark_mode) {
                atomic_fetch_add(&ctx->total_failed, 1);
            }
            conn_free(ctx, conn);
            return;
        }
        return;
    }

    /* Format SSE message */
    conn->write_buf = malloc(4096);
    if (!conn->write_buf) {
        if (ctx->benchmark_mode) {
            atomic_fetch_add(&ctx->total_failed, 1);
        }
        conn_free(ctx, conn);
        return;
    }

    int len = stream_format_sse(conn->write_buf, 4096, msg);
    if (len < 0) {
        if (ctx->benchmark_mode) {
            atomic_fetch_add(&ctx->total_failed, 1);
        }
        conn_free(ctx, conn);
        return;
    }

    conn->write_len = len;
    conn->write_offset = 0;

    /* Add delay between messages if configured */
    if (ctx->message_delay_ms > 0) {
        struct timespec ts = {
            .tv_sec = ctx->message_delay_ms / 1000,
            .tv_nsec = (ctx->message_delay_ms % 1000) * 1000000L
        };
        nanosleep(&ts, NULL);
    }

    /* Submit write */
    if (ring_submit_write(ctx, conn, conn->write_buf, len) < 0) {
        if (ctx->benchmark_mode) {
            atomic_fetch_add(&ctx->total_failed, 1);
        }
        conn_free(ctx, conn);
        return;
    }
    if (ring_submit_linked_timeout(ctx, conn, ctx->write_timeout_ms) < 0) {
        if (ctx->benchmark_mode) {
            atomic_fetch_add(&ctx->total_failed, 1);
        }
        conn_free(ctx, conn);
        return;
    }
}

/* Process a completion queue entry */
static void process_cqe(daemon_ctx_t *ctx, struct io_uring_cqe *cqe) {
    op_type_t op;
    uint32_t conn_id;
    ring_unpack_data(cqe->user_data, &op, &conn_id);

    switch (op) {
    case OP_ACCEPT:
        handle_accept(ctx, cqe);
        break;

    case OP_RECV_FD: {
        connection_t *conn = conn_get(ctx, conn_id);
        if (conn && atomic_load(&conn->state) == CONN_STATE_RECEIVING_FD) {
            handle_recv_fd(ctx, conn, cqe);
        }
        break;
    }

    case OP_WRITE: {
        connection_t *conn = conn_get(ctx, conn_id);
        conn_state_t state = conn ? atomic_load(&conn->state) : CONN_STATE_UNUSED;
        if (conn && (state == CONN_STATE_STREAMING ||
                     state == CONN_STATE_CLOSING)) {
            handle_write(ctx, conn, cqe);
        }
        break;
    }

    case OP_TIMEOUT:
        /* Linked timeout - handled by the linked operation */
        break;

    case OP_CANCEL:
        /* Cancel completed */
        break;

    case OP_EVENTFD:
        handle_eventfd(ctx);
        /* Resubmit eventfd read */
        if (ctx->use_thread_pool && !atomic_load(&ctx->shutdown_requested)) {
            ring_submit_eventfd_read(ctx, pool_get_eventfd(ctx->pool));
        }
        break;
    }
}

int daemon_run(daemon_ctx_t *ctx) {
    /* Submit initial accept */
    if (ring_submit_accept(ctx) < 0) {
        fprintf(stderr, "Failed to submit initial accept\n");
        return -1;
    }

    /* Submit eventfd read if thread pool is enabled */
    if (ctx->use_thread_pool) {
        if (ring_submit_eventfd_read(ctx, pool_get_eventfd(ctx->pool)) < 0) {
            fprintf(stderr, "Failed to submit eventfd read\n");
            return -1;
        }
    }

    ring_submit(ctx);

    fprintf(stderr, "Streaming daemon listening on %s (max %d connections)\n",
            ctx->socket_path, ctx->max_connections);

    /* Main event loop */
    while (!atomic_load(&ctx->shutdown_requested)) {
        struct io_uring_cqe *cqe;
        int ret = ring_wait_cqe(ctx, &cqe, 1000);

        if (ret == -ETIME) {
            continue;
        }

        if (ret < 0) {
            if (ret == -EINTR) {
                continue;
            }
            fprintf(stderr, "io_uring_wait_cqe error: %s\n", strerror(-ret));
            break;
        }

        /* Process completions */
        unsigned head;
        unsigned count = 0;
        io_uring_for_each_cqe(&ctx->ring, head, cqe) {
            process_cqe(ctx, cqe);
            count++;
        }
        io_uring_cq_advance(&ctx->ring, count);

        /* Submit any pending operations */
        ring_submit(ctx);
    }

    /* Graceful shutdown */
    fprintf(stderr, "Shutting down, waiting for %d active connections...\n",
            atomic_load(&ctx->active_connections));

    /* Wait for active connections with timeout */
    time_t shutdown_start = time(NULL);
    while (atomic_load(&ctx->active_connections) > 0) {
        if (time(NULL) - shutdown_start > ctx->shutdown_timeout_s) {
            fprintf(stderr, "Shutdown timeout, %d connections still active\n",
                    atomic_load(&ctx->active_connections));
            break;
        }

        struct io_uring_cqe *cqe;
        int ret = ring_wait_cqe(ctx, &cqe, 100);
        if (ret == 0) {
            unsigned head;
            unsigned count = 0;
            io_uring_for_each_cqe(&ctx->ring, head, cqe) {
                process_cqe(ctx, cqe);
                count++;
            }
            io_uring_cq_advance(&ctx->ring, count);
            ring_submit(ctx);
        }
    }

    /* Print benchmark summary */
    if (ctx->benchmark_mode) {
        fprintf(stderr, "\n=== Benchmark Summary ===\n");
        fprintf(stderr, "Peak concurrent: %d\n", atomic_load(&ctx->peak_connections));
        fprintf(stderr, "Total started: %lu\n",
                (unsigned long)atomic_load(&ctx->total_started));
        fprintf(stderr, "Total completed: %lu\n",
                (unsigned long)atomic_load(&ctx->total_completed));
        fprintf(stderr, "Total failed: %lu\n",
                (unsigned long)atomic_load(&ctx->total_failed));
        fprintf(stderr, "Total bytes: %lu\n",
                (unsigned long)atomic_load(&ctx->total_bytes));
        fprintf(stderr, "=========================\n");
    }

    fprintf(stderr, "Daemon stopped\n");
    return 0;
}

int main(int argc, char **argv) {
    daemon_ctx_t ctx;
    daemon_init(&ctx);
    g_ctx = &ctx;

    /* Parse args (loads config file first, then CLI args override) */
    if (parse_args(argc, argv, &ctx) < 0) {
        return 1;
    }

    setup_signals();
    increase_fd_limit(ctx.max_connections);

    /* Allocate recv contexts */
    recv_contexts = calloc(ctx.max_connections, sizeof(recv_ctx_t));
    if (!recv_contexts) {
        fprintf(stderr, "Failed to allocate recv contexts\n");
        return 1;
    }

    /* Initialize connection pool */
    if (conn_pool_init(&ctx) < 0) {
        fprintf(stderr, "Failed to initialize connection pool\n");
        return 1;
    }

    /* Initialize io_uring */
    if (ring_init(&ctx, DEFAULT_RING_SIZE) < 0) {
        fprintf(stderr, "Failed to initialize io_uring\n");
        daemon_cleanup(&ctx);
        return 1;
    }

    /* Initialize backend */
    if (backend_init() < 0) {
        fprintf(stderr, "Failed to initialize backend\n");
        daemon_cleanup(&ctx);
        return 1;
    }

    /* Initialize thread pool if enabled */
    if (ctx.use_thread_pool) {
        ctx.pool = &g_pool;
        if (pool_init(&g_pool, &ctx, ctx.pool_size) < 0) {
            fprintf(stderr, "Failed to initialize thread pool\n");
            daemon_cleanup(&ctx);
            return 1;
        }
        fprintf(stderr, "Thread pool: %d workers\n", ctx.pool_size);
    }

    /* Create listener */
    if (server_create_listener(&ctx) < 0) {
        fprintf(stderr, "Failed to create listener\n");
        daemon_cleanup(&ctx);
        return 1;
    }

    /* Start metrics server if configured */
    if (ctx.metrics_port > 0) {
        if (metrics_start(&ctx, ctx.metrics_port) < 0) {
            fprintf(stderr, "Warning: Failed to start metrics server\n");
            /* Non-fatal, continue without metrics */
        }
    }

    /* Run daemon */
    int ret = daemon_run(&ctx);

    daemon_cleanup(&ctx);
    return ret != 0 ? 1 : 0;
}
