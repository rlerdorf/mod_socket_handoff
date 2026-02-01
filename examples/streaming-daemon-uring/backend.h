/*
 * backend.h - LLM backend interface
 *
 * Backends are called from pool worker threads.
 * They make blocking HTTP requests to LLM APIs.
 */
#ifndef BACKEND_H
#define BACKEND_H

#include "daemon.h"

/*
 * Process an LLM API request for the given connection.
 *
 * Called from a pool worker thread. May block for seconds.
 * Should populate conn->write_buf with the first SSE chunk
 * or set up state for streaming.
 *
 * For mock backend: Sets up the demo message sequence.
 * For OpenAI backend: Makes streaming API call, parses first chunk.
 *
 * Returns:
 *   0 on success - connection moves to STREAMING state
 *  -1 on failure - connection moves to CLOSING state
 */
int backend_process(connection_t *conn);

/*
 * Initialize the backend (called once at startup).
 * For libcurl backend, initializes curl_global_init().
 */
int backend_init(void);

/*
 * Cleanup backend resources.
 */
void backend_cleanup(void);

#endif /* BACKEND_H */
