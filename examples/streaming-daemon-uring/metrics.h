/*
 * metrics.h - Prometheus metrics endpoint
 *
 * Provides a simple HTTP server for Prometheus scraping.
 * Runs in a separate thread to not block the io_uring event loop.
 */
#ifndef METRICS_H
#define METRICS_H

#include "daemon.h"

/* Start metrics server on specified port */
int metrics_start(daemon_ctx_t *ctx, int port);

/* Stop metrics server */
void metrics_stop(void);

/* Check if metrics server is running */
int metrics_running(void);

#endif /* METRICS_H */
