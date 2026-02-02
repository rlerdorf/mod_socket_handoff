/*
 * ring.h - io_uring wrapper
 */
#ifndef RING_H
#define RING_H

#include "daemon.h"
#include <liburing.h>

/* Initialize io_uring with specified queue depth */
int ring_init(daemon_ctx_t *ctx, unsigned entries);

/* Cleanup io_uring */
void ring_cleanup(daemon_ctx_t *ctx);

/* Submit a multishot accept operation */
int ring_submit_accept(daemon_ctx_t *ctx);

/* Submit a recvmsg operation for SCM_RIGHTS */
int ring_submit_recvmsg(daemon_ctx_t *ctx, connection_t *conn,
                        struct msghdr *msg);

/* Submit a write operation */
int ring_submit_write(daemon_ctx_t *ctx, connection_t *conn,
                      const void *buf, size_t len);

/* Submit a linked timeout for the previous operation */
int ring_submit_linked_timeout(daemon_ctx_t *ctx, connection_t *conn,
                               unsigned int timeout_ms);

/* Submit a cancel operation */
int ring_submit_cancel(daemon_ctx_t *ctx, connection_t *conn);

/* Submit a read on eventfd (for thread pool completion notification) */
int ring_submit_eventfd_read(daemon_ctx_t *ctx, int eventfd);

/*
 * Submit a multishot poll for curl socket events.
 * poll_mask: POLLIN, POLLOUT, or both
 * Returns 0 on success, negative on error.
 */
int ring_submit_poll_add(daemon_ctx_t *ctx, int sockfd, int poll_mask);

/*
 * Cancel an active poll on a socket.
 * Returns 0 on success, negative on error.
 */
int ring_submit_poll_cancel(daemon_ctx_t *ctx, int sockfd);

/*
 * Submit a timeout for curl timer events.
 * timeout_ms: timeout in milliseconds (0 = immediate, -1 = cancel)
 * timer_id: identifier for this timer (used in user_data)
 * Returns 0 on success, negative on error.
 */
int ring_submit_curl_timeout(daemon_ctx_t *ctx, int timeout_ms, uint32_t timer_id);

/*
 * Cancel the curl timer.
 */
int ring_cancel_curl_timeout(daemon_ctx_t *ctx, uint32_t timer_id);

/* Wait for and process completions */
int ring_wait_cqe(daemon_ctx_t *ctx, struct io_uring_cqe **cqe,
                  unsigned int timeout_ms);

/* Mark completion as seen */
void ring_cqe_seen(daemon_ctx_t *ctx, struct io_uring_cqe *cqe);

/* Submit pending SQEs */
int ring_submit(daemon_ctx_t *ctx);

/* Pack user data for io_uring */
static inline uint64_t ring_pack_data(op_type_t op, uint32_t conn_id) {
    return ((uint64_t)op << 32) | conn_id;
}

/* Unpack user data from io_uring */
static inline void ring_unpack_data(uint64_t data, op_type_t *op, uint32_t *conn_id) {
    *op = (op_type_t)(data >> 32);
    *conn_id = (uint32_t)(data & 0xFFFFFFFF);
}

#endif /* RING_H */
