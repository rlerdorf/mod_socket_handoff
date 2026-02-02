/*
 * connection.c - Connection state machine and pool management
 */
#include "connection.h"
#include "write_buffer.h"
#include <stdio.h>
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
        atomic_init(&ctx->connections[i].state, CONN_STATE_UNUSED);
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
            connection_t *conn = &ctx->connections[i];
            conn_close_fds(conn);
            if (conn->handoff_buf) {
                free(conn->handoff_buf);
            }
            if (conn->write_buf) {
                free(conn->write_buf);
            }
            if (conn->async_write_buf) {
                write_buffer_cleanup((write_buffer_t *)conn->async_write_buf);
                free(conn->async_write_buf);
            }
            if (conn->sse_line_buf) {
                free(conn->sse_line_buf);
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

    /* Allocate handoff buffer on-demand with tiered sizing.
     * Start with tier 1 (4KB) - handles 90%+ of requests.
     * Reuse existing buffer if already allocated (from previous use).
     */
    if (!conn->handoff_buf) {
        conn->handoff_buf = malloc(HANDOFF_TIER1_SIZE);
        if (!conn->handoff_buf) {
            /* Put connection back on free list */
            conn->next_free = ctx->free_list;
            ctx->free_list = conn;
            return NULL;
        }
        conn->handoff_cap = HANDOFF_TIER1_SIZE;
    }
    /* If buffer exists from previous connection, reuse it at current size */

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

/*
 * Grow the handoff buffer to accommodate more data.
 * Uses tiered sizes: 4KB -> 16KB -> 64KB
 * Returns 0 on success, -1 if already at max size.
 */
int conn_grow_handoff_buf(connection_t *conn) {
    size_t new_cap;

    if (conn->handoff_cap < HANDOFF_TIER2_SIZE) {
        new_cap = HANDOFF_TIER2_SIZE;
    } else if (conn->handoff_cap < HANDOFF_TIER3_SIZE) {
        new_cap = HANDOFF_TIER3_SIZE;
    } else {
        /* Already at max size */
        return -1;
    }

    char *new_buf = realloc(conn->handoff_buf, new_cap);
    if (!new_buf) {
        return -1;
    }

    conn->handoff_buf = new_buf;
    conn->handoff_cap = new_cap;
    return 0;
}

void conn_free(daemon_ctx_t *ctx, connection_t *conn) {
    conn_close_fds(conn);
    conn_reset(conn);

    conn->next_free = ctx->free_list;
    ctx->free_list = conn;

    atomic_fetch_sub(&ctx->active_connections, 1);
}

void conn_reset(connection_t *conn) {
    atomic_store(&conn->state, CONN_STATE_UNUSED);
    conn->unix_fd = -1;
    conn->client_fd = -1;
    conn->handoff_len = 0;
    memset(&conn->data, 0, sizeof(conn->data));
    conn->stream_index = 0;
    conn->bytes_sent = 0;
    conn->use_llm_response = false;
    conn->llm_response_pos = 0;

    /* Legacy write buffer */
    if (conn->write_buf) {
        free(conn->write_buf);
        conn->write_buf = NULL;
    }
    conn->write_len = 0;
    conn->write_offset = 0;

    /* Async write buffer - reset but keep allocated for reuse */
    if (conn->async_write_buf) {
        write_buffer_cleanup((write_buffer_t *)conn->async_write_buf);
        free(conn->async_write_buf);
        conn->async_write_buf = NULL;
    }

    /* Curl handle (should already be NULL) */
    conn->curl_easy = NULL;

    /* SSE line buffer - reset for reuse */
    if (conn->sse_line_buf) {
        free(conn->sse_line_buf);
        conn->sse_line_buf = NULL;
    }
    conn->sse_line_len = 0;
    conn->sse_line_cap = 0;

    /* Dirty list fields - should already be cleared by daemon_unmark_connection_dirty
     * but reset here for safety in case of direct conn_reset calls */
    conn->dirty_next = NULL;
    conn->on_dirty_list = false;
}

connection_t *conn_get(daemon_ctx_t *ctx, uint32_t id) {
    if (id >= (uint32_t)ctx->max_connections) {
        return NULL;
    }
    return &ctx->connections[id];
}

void conn_set_state(connection_t *conn, conn_state_t state) {
    atomic_store(&conn->state, state);
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
