/*
 * pool.h - Thread pool for LLM API calls
 *
 * The thread pool handles blocking HTTPS requests to LLM APIs.
 * Worker threads make libcurl requests and signal completion
 * back to the io_uring event loop via eventfd.
 */
#ifndef POOL_H
#define POOL_H

#include "daemon.h"
#include <pthread.h>

/* Work item for thread pool */
typedef struct pool_work {
    connection_t *conn;
    struct pool_work *next;
} pool_work_t;

/* Thread pool context */
typedef struct pool_ctx {
    /* Thread management */
    pthread_t *threads;
    int num_threads;
    volatile int shutdown;

    /* Work queue */
    pool_work_t *queue_head;
    pool_work_t *queue_tail;
    pthread_mutex_t queue_mutex;
    pthread_cond_t queue_cond;

    /* Completion notification */
    int eventfd;                /* For signaling io_uring loop */
    connection_t **completions; /* Ring buffer of completed connections */
    int completion_head;
    int completion_tail;
    int completion_size;
    pthread_mutex_t completion_mutex;

    /* Reference to daemon context */
    daemon_ctx_t *ctx;
} pool_ctx_t;

/* Initialize thread pool */
int pool_init(pool_ctx_t *pool, daemon_ctx_t *ctx, int num_threads);

/* Cleanup thread pool */
void pool_cleanup(pool_ctx_t *pool);

/* Submit work to the pool (connection in LLM_QUEUED state) */
int pool_submit(pool_ctx_t *pool, connection_t *conn);

/* Get eventfd for io_uring to poll */
int pool_get_eventfd(pool_ctx_t *pool);

/* Retrieve completed connections (call after eventfd signals) */
connection_t *pool_get_completion(pool_ctx_t *pool);

/* Request pool shutdown */
void pool_shutdown(pool_ctx_t *pool);

#endif /* POOL_H */
