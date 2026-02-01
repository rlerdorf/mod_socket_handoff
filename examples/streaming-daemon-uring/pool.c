/*
 * pool.c - Thread pool for LLM API calls
 */
#include "pool.h"
#include "connection.h"
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <sys/eventfd.h>
#include <errno.h>

/* Forward declarations */
static void *worker_thread(void *arg);
static pool_work_t *pool_dequeue(pool_ctx_t *pool);
static void pool_add_completion(pool_ctx_t *pool, connection_t *conn);

/* External: LLM backend processing (implemented in backend_*.c) */
extern int backend_process(connection_t *conn);

int pool_init(pool_ctx_t *pool, daemon_ctx_t *ctx, int num_threads) {
    memset(pool, 0, sizeof(*pool));
    pool->ctx = ctx;
    pool->num_threads = num_threads;

    /* Create eventfd for signaling completions to io_uring loop */
    pool->eventfd = eventfd(0, EFD_NONBLOCK | EFD_CLOEXEC);
    if (pool->eventfd < 0) {
        return -1;
    }

    /* Initialize mutexes and condition variable */
    if (pthread_mutex_init(&pool->queue_mutex, NULL) != 0) {
        close(pool->eventfd);
        return -1;
    }
    if (pthread_cond_init(&pool->queue_cond, NULL) != 0) {
        pthread_mutex_destroy(&pool->queue_mutex);
        close(pool->eventfd);
        return -1;
    }
    if (pthread_mutex_init(&pool->completion_mutex, NULL) != 0) {
        pthread_cond_destroy(&pool->queue_cond);
        pthread_mutex_destroy(&pool->queue_mutex);
        close(pool->eventfd);
        return -1;
    }

    /* Allocate completion ring buffer */
    pool->completion_size = ctx->max_connections;
    pool->completions = calloc(pool->completion_size, sizeof(connection_t *));
    if (!pool->completions) {
        pthread_mutex_destroy(&pool->completion_mutex);
        pthread_cond_destroy(&pool->queue_cond);
        pthread_mutex_destroy(&pool->queue_mutex);
        close(pool->eventfd);
        return -1;
    }

    /* Create worker threads */
    pool->threads = calloc(num_threads, sizeof(pthread_t));
    if (!pool->threads) {
        free(pool->completions);
        pthread_mutex_destroy(&pool->completion_mutex);
        pthread_cond_destroy(&pool->queue_cond);
        pthread_mutex_destroy(&pool->queue_mutex);
        close(pool->eventfd);
        return -1;
    }

    for (int i = 0; i < num_threads; i++) {
        if (pthread_create(&pool->threads[i], NULL, worker_thread, pool) != 0) {
            /* Signal existing threads to exit */
            pool->shutdown = 1;
            pthread_cond_broadcast(&pool->queue_cond);
            for (int j = 0; j < i; j++) {
                pthread_join(pool->threads[j], NULL);
            }
            free(pool->threads);
            free(pool->completions);
            pthread_mutex_destroy(&pool->completion_mutex);
            pthread_cond_destroy(&pool->queue_cond);
            pthread_mutex_destroy(&pool->queue_mutex);
            close(pool->eventfd);
            /* Leave pool in safe state so pool_cleanup can be called */
            pool->threads = NULL;
            pool->completions = NULL;
            pool->eventfd = -1;
            return -1;
        }
    }

    return 0;
}

void pool_cleanup(pool_ctx_t *pool) {
    if (!pool->threads) {
        return;
    }

    /* Signal shutdown */
    pool_shutdown(pool);

    /* Wait for all threads */
    for (int i = 0; i < pool->num_threads; i++) {
        pthread_join(pool->threads[i], NULL);
    }

    /* Free work queue */
    pool_work_t *work = pool->queue_head;
    while (work) {
        pool_work_t *next = work->next;
        free(work);
        work = next;
    }

    /* Cleanup resources */
    free(pool->threads);
    free(pool->completions);
    pthread_mutex_destroy(&pool->completion_mutex);
    pthread_cond_destroy(&pool->queue_cond);
    pthread_mutex_destroy(&pool->queue_mutex);
    if (pool->eventfd >= 0) {
        close(pool->eventfd);
    }
}

int pool_submit(pool_ctx_t *pool, connection_t *conn) {
    pool_work_t *work = malloc(sizeof(pool_work_t));
    if (!work) {
        return -1;
    }
    work->conn = conn;
    work->next = NULL;

    pthread_mutex_lock(&pool->queue_mutex);
    if (pool->queue_tail) {
        pool->queue_tail->next = work;
    } else {
        pool->queue_head = work;
    }
    pool->queue_tail = work;
    pthread_cond_signal(&pool->queue_cond);
    pthread_mutex_unlock(&pool->queue_mutex);

    return 0;
}

int pool_get_eventfd(pool_ctx_t *pool) {
    return pool->eventfd;
}

connection_t *pool_get_completion(pool_ctx_t *pool) {
    connection_t *conn = NULL;

    pthread_mutex_lock(&pool->completion_mutex);
    if (pool->completion_head != pool->completion_tail) {
        conn = pool->completions[pool->completion_head];
        pool->completion_head = (pool->completion_head + 1) % pool->completion_size;
    }
    pthread_mutex_unlock(&pool->completion_mutex);

    return conn;
}

void pool_shutdown(pool_ctx_t *pool) {
    pthread_mutex_lock(&pool->queue_mutex);
    pool->shutdown = 1;
    pthread_cond_broadcast(&pool->queue_cond);
    pthread_mutex_unlock(&pool->queue_mutex);
}

/* Internal: Dequeue work item (caller holds mutex) */
static pool_work_t *pool_dequeue(pool_ctx_t *pool) {
    pool_work_t *work = pool->queue_head;
    if (work) {
        pool->queue_head = work->next;
        if (!pool->queue_head) {
            pool->queue_tail = NULL;
        }
    }
    return work;
}

/* Internal: Add completed connection and signal eventfd */
static void pool_add_completion(pool_ctx_t *pool, connection_t *conn) {
    pthread_mutex_lock(&pool->completion_mutex);

    int next_tail = (pool->completion_tail + 1) % pool->completion_size;

    /* If ring is full, wait until there is space instead of dropping */
    while (next_tail == pool->completion_head) {
        pthread_mutex_unlock(&pool->completion_mutex);
        /* Briefly sleep to avoid busy-waiting while waiting for the main loop
         * to consume completions and free space in the ring. */
        usleep(1000); /* 1 ms */
        pthread_mutex_lock(&pool->completion_mutex);
        next_tail = (pool->completion_tail + 1) % pool->completion_size;
    }

    pool->completions[pool->completion_tail] = conn;
    pool->completion_tail = next_tail;

    pthread_mutex_unlock(&pool->completion_mutex);

    /* Signal eventfd to wake up io_uring loop */
    uint64_t val = 1;
    ssize_t ret;
    do {
        ret = write(pool->eventfd, &val, sizeof(val));
    } while (ret < 0 && errno == EINTR);
}

/* Worker thread function */
static void *worker_thread(void *arg) {
    pool_ctx_t *pool = (pool_ctx_t *)arg;

    while (1) {
        pthread_mutex_lock(&pool->queue_mutex);

        /* Wait for work or shutdown */
        while (!pool->queue_head && !pool->shutdown) {
            pthread_cond_wait(&pool->queue_cond, &pool->queue_mutex);
        }

        if (pool->shutdown && !pool->queue_head) {
            pthread_mutex_unlock(&pool->queue_mutex);
            break;
        }

        pool_work_t *work = pool_dequeue(pool);
        pthread_mutex_unlock(&pool->queue_mutex);

        if (work) {
            connection_t *conn = work->conn;
            free(work);

            /* Process the LLM API call (blocking) */
            int result = backend_process(conn);

            /* Update state based on result */
            if (result == 0) {
                conn_set_state(conn, CONN_STATE_STREAMING);
            } else {
                conn_set_state(conn, CONN_STATE_CLOSING);
            }

            /* Add to completion queue and signal main loop */
            pool_add_completion(pool, conn);
        }
    }

    return NULL;
}
