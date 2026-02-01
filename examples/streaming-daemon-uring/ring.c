/*
 * ring.c - io_uring wrapper
 */
#include "ring.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <errno.h>
#include <unistd.h>

int ring_init(daemon_ctx_t *ctx, unsigned entries) {
    struct io_uring_params params = {0};

    /* Request features for better performance */
    /* IORING_SETUP_COOP_TASKRUN reduces kernel-to-user notifications */
    /* IORING_SETUP_SINGLE_ISSUER optimizes for single-threaded use */
    params.flags = IORING_SETUP_COOP_TASKRUN | IORING_SETUP_SINGLE_ISSUER;

    /* SQPOLL: kernel thread polls SQ, eliminating submit syscalls */
    if (ctx->use_sqpoll) {
        params.flags |= IORING_SETUP_SQPOLL;
        params.sq_thread_idle = 2000; /* 2 second idle before sleeping */
        fprintf(stderr, "SQPOLL enabled (kernel polling thread)\n");
    }

    int ret = io_uring_queue_init_params(entries, &ctx->ring, &params);
    if (ret < 0) {
        if (ctx->use_sqpoll && ret == -EPERM) {
            fprintf(stderr, "SQPOLL requires CAP_SYS_NICE or root\n");
        }
        /* Try without optional flags */
        memset(&params, 0, sizeof(params));
        ret = io_uring_queue_init(entries, &ctx->ring, 0);
        if (ret < 0) {
            fprintf(stderr, "io_uring_queue_init failed: %s\n", strerror(-ret));
            return -1;
        }
        if (ctx->use_sqpoll) {
            fprintf(stderr, "Warning: SQPOLL not available, using standard mode\n");
            ctx->use_sqpoll = false;
        }
    }

    ctx->ring_initialized = true;
    return 0;
}

void ring_cleanup(daemon_ctx_t *ctx) {
    if (ctx->ring_initialized) {
        io_uring_queue_exit(&ctx->ring);
        ctx->ring_initialized = false;
    }
}

/* Helper to get SQE, flushing if necessary */
static struct io_uring_sqe *ring_get_sqe_flush(daemon_ctx_t *ctx) {
    struct io_uring_sqe *sqe = io_uring_get_sqe(&ctx->ring);
    if (!sqe) {
        /* Flush pending submissions and try again */
        io_uring_submit(&ctx->ring);
        sqe = io_uring_get_sqe(&ctx->ring);
    }
    return sqe;
}

int ring_submit_accept(daemon_ctx_t *ctx) {
    struct io_uring_sqe *sqe = io_uring_get_sqe(&ctx->ring);
    if (!sqe) {
        return -EAGAIN;
    }

    /* Use multishot accept for better performance (Linux 5.19+) */
    io_uring_prep_multishot_accept(sqe, ctx->listen_fd, NULL, NULL, 0);
    io_uring_sqe_set_data64(sqe, ring_pack_data(OP_ACCEPT, 0));

    return 0;
}

int ring_submit_recvmsg(daemon_ctx_t *ctx, connection_t *conn,
                        struct msghdr *msg) {
    struct io_uring_sqe *sqe = ring_get_sqe_flush(ctx);
    if (!sqe) {
        return -EAGAIN;
    }

    io_uring_prep_recvmsg(sqe, conn->unix_fd, msg, MSG_CMSG_CLOEXEC);
    io_uring_sqe_set_data64(sqe, ring_pack_data(OP_RECV_FD, conn->id));

    return 0;
}

int ring_submit_write(daemon_ctx_t *ctx, connection_t *conn,
                      const void *buf, size_t len) {
    struct io_uring_sqe *sqe = ring_get_sqe_flush(ctx);
    if (!sqe) {
        return -EAGAIN;
    }

    io_uring_prep_write(sqe, conn->client_fd, buf, len, 0);
    io_uring_sqe_set_data64(sqe, ring_pack_data(OP_WRITE, conn->id));

    /* Link with timeout for next SQE */
    sqe->flags |= IOSQE_IO_LINK;

    return 0;
}

int ring_submit_linked_timeout(daemon_ctx_t *ctx, connection_t *conn,
                               unsigned int timeout_ms) {
    /* Note: Don't flush here - linked timeout must follow its linked operation */
    struct io_uring_sqe *sqe = io_uring_get_sqe(&ctx->ring);
    if (!sqe) {
        return -EAGAIN;
    }

    /* Timeout spec - per-connection to avoid race conditions */
    struct __kernel_timespec *ts = &conn->timeout_spec;
    ts->tv_sec = timeout_ms / 1000;
    ts->tv_nsec = (timeout_ms % 1000) * 1000000L;

    io_uring_prep_link_timeout(sqe, ts, 0);
    io_uring_sqe_set_data64(sqe, ring_pack_data(OP_TIMEOUT, conn->id));

    return 0;
}

int ring_submit_cancel(daemon_ctx_t *ctx, connection_t *conn) {
    struct io_uring_sqe *sqe = io_uring_get_sqe(&ctx->ring);
    if (!sqe) {
        return -EAGAIN;
    }

    /* Cancel all operations for this fd */
    io_uring_prep_cancel_fd(sqe, conn->client_fd, 0);
    io_uring_sqe_set_data64(sqe, ring_pack_data(OP_CANCEL, conn->id));

    return 0;
}

/* Static buffer for eventfd read - only one eventfd read active at a time */
static uint64_t eventfd_buf;

int ring_submit_eventfd_read(daemon_ctx_t *ctx, int eventfd) {
    struct io_uring_sqe *sqe = ring_get_sqe_flush(ctx);
    if (!sqe) {
        return -EAGAIN;
    }

    io_uring_prep_read(sqe, eventfd, &eventfd_buf, sizeof(eventfd_buf), 0);
    io_uring_sqe_set_data64(sqe, ring_pack_data(OP_EVENTFD, 0));

    return 0;
}

int ring_wait_cqe(daemon_ctx_t *ctx, struct io_uring_cqe **cqe,
                  unsigned int timeout_ms) {
    struct __kernel_timespec ts;
    ts.tv_sec = timeout_ms / 1000;
    ts.tv_nsec = (timeout_ms % 1000) * 1000000L;

    return io_uring_wait_cqe_timeout(&ctx->ring, cqe, &ts);
}

void ring_cqe_seen(daemon_ctx_t *ctx, struct io_uring_cqe *cqe) {
    io_uring_cqe_seen(&ctx->ring, cqe);
}

int ring_submit(daemon_ctx_t *ctx) {
    return io_uring_submit(&ctx->ring);
}
