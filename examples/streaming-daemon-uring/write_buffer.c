/*
 * write_buffer.c - Async write buffering for io_uring
 *
 * Uses a per-connection linear buffer that resets when drained.
 * This avoids malloc on the hot path - SSE chunks are copied into
 * the pre-allocated buffer with simple pointer arithmetic.
 *
 * When the buffer drains (tail catches up to head), both pointers
 * reset to 0, avoiding wrap-around complexity.
 *
 * If data won't fit at the end, we compact (memmove remaining data
 * to start) before appending. This is rare since writes complete
 * quickly and the buffer drains frequently.
 */
#include "write_buffer.h"
#include <stdlib.h>
#include <string.h>

int write_buffer_init(write_buffer_t *wb) {
    return write_buffer_init_sized(wb, WRITE_BUFFER_DEFAULT_SIZE);
}

int write_buffer_init_sized(write_buffer_t *wb, size_t capacity) {
    wb->buffer = malloc(capacity);
    if (!wb->buffer) {
        return -1;
    }
    wb->capacity = capacity;
    wb->head = 0;
    wb->tail = 0;
    wb->bytes_written = 0;
    wb->write_pending = false;
    return 0;
}

void write_buffer_cleanup(write_buffer_t *wb) {
    free(wb->buffer);
    wb->buffer = NULL;
    wb->capacity = 0;
    wb->head = 0;
    wb->tail = 0;
}

void write_buffer_reset(write_buffer_t *wb) {
    wb->head = 0;
    wb->tail = 0;
    wb->write_pending = false;
    /* Keep bytes_written for stats */
}

int write_buffer_append(write_buffer_t *wb, const void *data, size_t len) {
    /* Check backpressure limit */
    size_t pending = wb->head - wb->tail;
    if (pending + len > WRITE_BUFFER_MAX_SIZE) {
        return -2;  /* Buffer full - backpressure */
    }

    /* Check if data fits at current position */
    size_t space_at_end = wb->capacity - wb->head;

    if (len > space_at_end) {
        /* Not enough space at end - need to compact or grow */

        if (wb->write_pending) {
            /* Can't compact while write is in flight (pointer in use).
             * Try to grow the buffer instead.
             */
            size_t new_cap = wb->capacity * 2;
            if (new_cap > WRITE_BUFFER_MAX_SIZE) {
                new_cap = WRITE_BUFFER_MAX_SIZE;
            }
            if (new_cap < wb->head + len) {
                new_cap = wb->head + len + WRITE_BUFFER_DEFAULT_SIZE;
            }
            if (new_cap > WRITE_BUFFER_MAX_SIZE) {
                return -2;  /* Would exceed max size */
            }

            char *new_buf = realloc(wb->buffer, new_cap);
            if (!new_buf) {
                return -1;  /* Allocation failure */
            }
            wb->buffer = new_buf;
            wb->capacity = new_cap;
        } else {
            /* No write pending - compact by moving data to start */
            if (pending > 0) {
                memmove(wb->buffer, wb->buffer + wb->tail, pending);
            }
            wb->head = pending;
            wb->tail = 0;

            /* After compacting, check if we have enough space.
             * If not, grow the buffer.
             */
            if (pending + len > wb->capacity) {
                size_t new_cap = wb->capacity * 2;
                if (new_cap < pending + len) {
                    new_cap = pending + len;
                }
                if (new_cap > WRITE_BUFFER_MAX_SIZE) {
                    return -2;  /* Would exceed max size */
                }

                char *new_buf = realloc(wb->buffer, new_cap);
                if (!new_buf) {
                    return -1;  /* Allocation failure */
                }
                wb->buffer = new_buf;
                wb->capacity = new_cap;
            }
        }
    }

    /* Now we have space - append data */
    memcpy(wb->buffer + wb->head, data, len);
    wb->head += len;

    return 0;
}

size_t write_buffer_consume(write_buffer_t *wb, size_t bytes) {
    size_t pending = wb->head - wb->tail;
    if (bytes > pending) {
        bytes = pending;
    }

    wb->tail += bytes;
    wb->bytes_written += bytes;

    /* Reset to start when buffer drains */
    if (wb->tail == wb->head) {
        wb->head = 0;
        wb->tail = 0;
    }

    return bytes;
}

const char *write_buffer_get_next(write_buffer_t *wb, size_t *len) {
    size_t pending = wb->head - wb->tail;
    if (pending == 0) {
        *len = 0;
        return NULL;
    }

    *len = pending;
    return wb->buffer + wb->tail;
}
