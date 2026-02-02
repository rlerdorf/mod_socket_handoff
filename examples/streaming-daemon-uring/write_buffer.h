/*
 * write_buffer.h - Async write buffering for io_uring
 *
 * Uses a per-connection linear buffer that resets when drained.
 * Avoids malloc on the hot path - only allocates once per connection.
 */
#ifndef WRITE_BUFFER_H
#define WRITE_BUFFER_H

#include <stddef.h>
#include <stdbool.h>
#include <stdint.h>

/* Default buffer capacity - handles typical SSE responses */
#define WRITE_BUFFER_DEFAULT_SIZE (8 * 1024)    /* 8KB */

/* Maximum buffer size for backpressure */
#define WRITE_BUFFER_MAX_SIZE (1024 * 1024)     /* 1MB */

/* Write buffer using a linear buffer with reset-on-drain */
typedef struct {
    char *buffer;           /* Pre-allocated buffer */
    size_t capacity;        /* Buffer capacity */
    size_t head;            /* Write position (end of data) */
    size_t tail;            /* Read position (start of data) */
    size_t bytes_written;   /* Cumulative bytes written (for stats) */
    bool write_pending;     /* io_uring write in flight */
} write_buffer_t;

/*
 * Initialize a write buffer.
 * Allocates the initial buffer. Returns 0 on success, -1 on failure.
 */
int write_buffer_init(write_buffer_t *wb);

/*
 * Initialize with a specific capacity.
 */
int write_buffer_init_sized(write_buffer_t *wb, size_t capacity);

/*
 * Cleanup a write buffer, freeing the buffer.
 */
void write_buffer_cleanup(write_buffer_t *wb);

/*
 * Reset buffer for reuse (keeps allocation, resets pointers).
 */
void write_buffer_reset(write_buffer_t *wb);

/*
 * Append data to the write buffer.
 *
 * The data is copied into the pre-allocated buffer.
 * May compact or grow the buffer if needed.
 *
 * Returns:
 *   0 on success
 *  -1 on allocation failure (grow failed)
 *  -2 if buffer is full (backpressure - connection should be dropped)
 */
int write_buffer_append(write_buffer_t *wb, const void *data, size_t len);

/*
 * Mark bytes as written (called after io_uring write completion).
 *
 * Advances tail pointer. When buffer drains, resets to start.
 *
 * Returns: number of bytes consumed (for stats tracking)
 */
size_t write_buffer_consume(write_buffer_t *wb, size_t bytes_written);

/*
 * Get the next chunk to write via io_uring.
 *
 * Returns pointer directly into the buffer and sets *len to length.
 * Returns NULL if no data is pending.
 *
 * Note: The returned pointer is valid until the buffer is compacted or freed.
 */
const char *write_buffer_get_next(write_buffer_t *wb, size_t *len);

/*
 * Check if there's data waiting to be written.
 */
static inline bool write_buffer_has_data(const write_buffer_t *wb) {
    return wb->head > wb->tail;
}

/*
 * Check if a write operation is in flight.
 */
static inline bool write_buffer_is_busy(const write_buffer_t *wb) {
    return wb->write_pending;
}

/*
 * Mark write operation as in-flight.
 */
static inline void write_buffer_set_pending(write_buffer_t *wb, bool pending) {
    wb->write_pending = pending;
}

/*
 * Get total bytes currently queued.
 */
static inline size_t write_buffer_size(const write_buffer_t *wb) {
    return wb->head - wb->tail;
}

/*
 * Check if buffer is over the limit (backpressure condition).
 */
static inline bool write_buffer_is_full(const write_buffer_t *wb) {
    return (wb->head - wb->tail) >= WRITE_BUFFER_MAX_SIZE;
}

#endif /* WRITE_BUFFER_H */
