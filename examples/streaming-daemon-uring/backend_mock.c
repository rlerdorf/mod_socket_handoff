/*
 * backend_mock.c - Mock LLM backend for testing
 *
 * This backend simulates an LLM API call by sleeping briefly
 * and then setting up the connection for streaming demo messages.
 * Used for testing the thread pool infrastructure without libcurl.
 */
#include "backend.h"
#include "streaming.h"
#include <stdlib.h>
#include <unistd.h>
#include <time.h>

/* Simulated API latency in milliseconds (0 for benchmarks) */
static int mock_latency_ms = 0;

int backend_init(void) {
    /* Check environment for custom latency */
    const char *latency = getenv("MOCK_LATENCY_MS");
    if (latency) {
        mock_latency_ms = atoi(latency);
        if (mock_latency_ms < 0) {
            mock_latency_ms = 0;
        }
    }
    return 0;
}

void backend_cleanup(void) {
    /* Nothing to clean up for mock backend */
}

int backend_process(connection_t *conn) {
    /* Simulate LLM API latency */
    if (mock_latency_ms > 0) {
        struct timespec ts = {
            .tv_sec = mock_latency_ms / 1000,
            .tv_nsec = (mock_latency_ms % 1000) * 1000000L
        };
        nanosleep(&ts, NULL);
    }

    /* Initialize streaming state */
    stream_start(conn);

    return 0;
}
