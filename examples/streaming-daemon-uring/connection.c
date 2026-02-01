/*
 * connection.c - Connection state machine and pool management
 */
#include "connection.h"
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <time.h>

int conn_pool_init(daemon_ctx_t *ctx) {
    size_t size = sizeof(connection_t) * ctx->max_connections;
    ctx->connections = calloc(ctx->max_connections, sizeof(connection_t));
    if (!ctx->connections) {
        return -1;
    }

    /* Initialize free list */
    ctx->free_list = NULL;
    for (int i = ctx->max_connections - 1; i >= 0; i--) {
        ctx->connections[i].id = i;
        ctx->connections[i].state = CONN_STATE_UNUSED;
        ctx->connections[i].unix_fd = -1;
        ctx->connections[i].client_fd = -1;
        ctx->connections[i].next_free = ctx->free_list;
        ctx->free_list = &ctx->connections[i];
    }

    atomic_store(&ctx->active_connections, 0);
    (void)size; /* Silence unused warning */
    return 0;
}

void conn_pool_cleanup(daemon_ctx_t *ctx) {
    if (ctx->connections) {
        /* Close any open connections and free buffers */
        for (int i = 0; i < ctx->max_connections; i++) {
            conn_close_fds(&ctx->connections[i]);
            if (ctx->connections[i].handoff_buf) {
                free(ctx->connections[i].handoff_buf);
            }
            if (ctx->connections[i].write_buf) {
                free(ctx->connections[i].write_buf);
            }
        }
        free(ctx->connections);
        ctx->connections = NULL;
    }
    ctx->free_list = NULL;
}

connection_t *conn_alloc(daemon_ctx_t *ctx) {
    if (!ctx->free_list) {
        return NULL;
    }

    connection_t *conn = ctx->free_list;
    ctx->free_list = conn->next_free;
    conn->next_free = NULL;

    /* Allocate handoff buffer on-demand */
    if (!conn->handoff_buf) {
        conn->handoff_buf = malloc(MAX_HANDOFF_DATA_SIZE);
        if (!conn->handoff_buf) {
            /* Put connection back on free list */
            conn->next_free = ctx->free_list;
            ctx->free_list = conn;
            return NULL;
        }
    }

    int active = atomic_fetch_add(&ctx->active_connections, 1) + 1;

    /* Track peak */
    int peak = atomic_load(&ctx->peak_connections);
    while (active > peak) {
        if (atomic_compare_exchange_weak(&ctx->peak_connections, &peak, active)) {
            break;
        }
    }

    return conn;
}

void conn_free(daemon_ctx_t *ctx, connection_t *conn) {
    conn_close_fds(conn);
    conn_reset(conn);

    conn->next_free = ctx->free_list;
    ctx->free_list = conn;

    atomic_fetch_sub(&ctx->active_connections, 1);
}

void conn_reset(connection_t *conn) {
    conn->state = CONN_STATE_UNUSED;
    conn->unix_fd = -1;
    conn->client_fd = -1;
    conn->handoff_len = 0;
    memset(&conn->data, 0, sizeof(conn->data));
    conn->stream_index = 0;
    conn->bytes_sent = 0;
    if (conn->write_buf) {
        free(conn->write_buf);
        conn->write_buf = NULL;
    }
    conn->write_len = 0;
    conn->write_offset = 0;
}

connection_t *conn_get(daemon_ctx_t *ctx, uint32_t id) {
    if (id >= (uint32_t)ctx->max_connections) {
        return NULL;
    }
    return &ctx->connections[id];
}

void conn_set_state(connection_t *conn, conn_state_t state) {
    conn->state = state;
}

void conn_close_fds(connection_t *conn) {
    if (conn->unix_fd >= 0) {
        close(conn->unix_fd);
        conn->unix_fd = -1;
    }
    if (conn->client_fd >= 0) {
        close(conn->client_fd);
        conn->client_fd = -1;
    }
}

uint64_t conn_elapsed_ms(connection_t *conn) {
    struct timespec now;
    clock_gettime(CLOCK_MONOTONIC, &now);

    uint64_t start_ms = (uint64_t)conn->start_time.tv_sec * 1000 +
                        conn->start_time.tv_nsec / 1000000;
    uint64_t now_ms = (uint64_t)now.tv_sec * 1000 + now.tv_nsec / 1000000;

    return now_ms - start_ms;
}
