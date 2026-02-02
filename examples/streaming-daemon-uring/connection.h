/*
 * connection.h - Connection state machine
 */
#ifndef CONNECTION_H
#define CONNECTION_H

#include "daemon.h"

/* Initialize connection pool */
int conn_pool_init(daemon_ctx_t *ctx);

/* Cleanup connection pool */
void conn_pool_cleanup(daemon_ctx_t *ctx);

/* Allocate a connection from the pool */
connection_t *conn_alloc(daemon_ctx_t *ctx);

/* Return connection to the pool */
void conn_free(daemon_ctx_t *ctx, connection_t *conn);

/* Reset connection state for reuse */
void conn_reset(connection_t *conn);

/* Get connection by ID */
connection_t *conn_get(daemon_ctx_t *ctx, uint32_t id);

/* Grow handoff buffer to next tier size. Returns 0 on success, -1 if at max. */
int conn_grow_handoff_buf(connection_t *conn);

/* Set connection state */
void conn_set_state(connection_t *conn, conn_state_t state);

/* Close all file descriptors for a connection */
void conn_close_fds(connection_t *conn);

/* Get elapsed time since connection start in milliseconds */
uint64_t conn_elapsed_ms(connection_t *conn);

#endif /* CONNECTION_H */
