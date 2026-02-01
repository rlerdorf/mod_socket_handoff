/*
 * streaming.h - SSE streaming to client
 */
#ifndef STREAMING_H
#define STREAMING_H

#include "daemon.h"

/* Start streaming to client - sends HTTP headers */
int stream_start(connection_t *conn);

/* Get next SSE message to send
 * Returns pointer to static buffer, or NULL when done
 * Sets *len to message length
 */
const char *stream_next_message(connection_t *conn, size_t *len);

/* Format SSE event with JSON content
 * Returns bytes written to buf, or -1 on error
 */
int stream_format_sse(char *buf, size_t buflen, const char *content);

/* Get the demo response messages */
const char **stream_get_messages(int *count);

/* HTTP headers for SSE response */
#define SSE_HEADERS \
    "HTTP/1.1 200 OK\r\n" \
    "Content-Type: text/event-stream\r\n" \
    "Cache-Control: no-cache\r\n" \
    "Connection: close\r\n" \
    "X-Accel-Buffering: no\r\n" \
    "\r\n"

#define SSE_DONE "data: [DONE]\n\n"

#endif /* STREAMING_H */
