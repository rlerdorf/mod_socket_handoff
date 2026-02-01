/*
 * metrics.c - Prometheus metrics endpoint
 *
 * Simple HTTP server exposing metrics in Prometheus text format.
 * Runs in a dedicated thread to avoid blocking io_uring.
 */
#include "metrics.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <pthread.h>
#include <poll.h>
#include <stdatomic.h>
#include <sys/socket.h>
#include <netinet/in.h>
#include <arpa/inet.h>
#include <errno.h>

static pthread_t metrics_thread;
static int metrics_fd = -1;
static atomic_bool metrics_shutdown = false;
static daemon_ctx_t *metrics_ctx = NULL;

/* Format metrics in Prometheus text format */
static int format_metrics(char *buf, size_t bufsize, daemon_ctx_t *ctx) {
    int active = atomic_load(&ctx->active_connections);
    int peak = atomic_load(&ctx->peak_connections);
    uint64_t started = atomic_load(&ctx->total_started);
    uint64_t completed = atomic_load(&ctx->total_completed);
    uint64_t failed = atomic_load(&ctx->total_failed);
    uint64_t bytes = atomic_load(&ctx->total_bytes);

    return snprintf(buf, bufsize,
        "# HELP streaming_daemon_connections_active Current active connections\n"
        "# TYPE streaming_daemon_connections_active gauge\n"
        "streaming_daemon_connections_active %d\n"
        "\n"
        "# HELP streaming_daemon_connections_peak Peak concurrent connections\n"
        "# TYPE streaming_daemon_connections_peak gauge\n"
        "streaming_daemon_connections_peak %d\n"
        "\n"
        "# HELP streaming_daemon_connections_max Maximum allowed connections\n"
        "# TYPE streaming_daemon_connections_max gauge\n"
        "streaming_daemon_connections_max %d\n"
        "\n"
        "# HELP streaming_daemon_requests_total Total requests by status\n"
        "# TYPE streaming_daemon_requests_total counter\n"
        "streaming_daemon_requests_total{status=\"started\"} %lu\n"
        "streaming_daemon_requests_total{status=\"completed\"} %lu\n"
        "streaming_daemon_requests_total{status=\"failed\"} %lu\n"
        "\n"
        "# HELP streaming_daemon_bytes_total Total bytes sent\n"
        "# TYPE streaming_daemon_bytes_total counter\n"
        "streaming_daemon_bytes_total %lu\n"
        "\n"
        "# HELP streaming_daemon_info Daemon information\n"
        "# TYPE streaming_daemon_info gauge\n"
        "streaming_daemon_info{version=\"%s\",sqpoll=\"%s\",thread_pool=\"%s\"} 1\n",
        active, peak, ctx->max_connections,
        (unsigned long)started,
        (unsigned long)completed,
        (unsigned long)failed,
        (unsigned long)bytes,
        DAEMON_VERSION,
        ctx->use_sqpoll ? "enabled" : "disabled",
        ctx->use_thread_pool ? "enabled" : "disabled"
    );
}

/* Handle a single HTTP request */
static void handle_request(int client_fd, daemon_ctx_t *ctx) {
    char request[1024];
    ssize_t n = recv(client_fd, request, sizeof(request) - 1, 0);
    if (n <= 0) {
        close(client_fd);
        return;
    }
    request[n] = '\0';

    /* Check if it's a GET /metrics request */
    if (strncmp(request, "GET /metrics", 12) != 0 &&
        strncmp(request, "GET / ", 6) != 0) {
        const char *not_found =
            "HTTP/1.1 404 Not Found\r\n"
            "Content-Length: 9\r\n"
            "Connection: close\r\n"
            "\r\n"
            "Not Found";
        send(client_fd, not_found, strlen(not_found), 0);
        close(client_fd);
        return;
    }

    /* Format metrics */
    char body[4096];
    int body_len = format_metrics(body, sizeof(body), ctx);

    /* Send response */
    char response[8192];
    int resp_len = snprintf(response, sizeof(response),
        "HTTP/1.1 200 OK\r\n"
        "Content-Type: text/plain; version=0.0.4; charset=utf-8\r\n"
        "Content-Length: %d\r\n"
        "Connection: close\r\n"
        "\r\n"
        "%s",
        body_len, body);

    send(client_fd, response, resp_len, 0);
    close(client_fd);
}

/* Metrics server thread */
static void *metrics_thread_func(void *arg) {
    daemon_ctx_t *ctx = (daemon_ctx_t *)arg;

    while (!atomic_load(&metrics_shutdown)) {
        struct sockaddr_in client_addr;
        socklen_t addr_len = sizeof(client_addr);

        /* Use poll with timeout to check for shutdown */
        struct pollfd pfd = {.fd = metrics_fd, .events = POLLIN};
        int ret = poll(&pfd, 1, 1000);

        if (ret <= 0) {
            continue;
        }

        int client_fd = accept(metrics_fd, (struct sockaddr *)&client_addr, &addr_len);
        if (client_fd < 0) {
            if (errno != EINTR && errno != EAGAIN) {
                perror("metrics accept");
            }
            continue;
        }

        handle_request(client_fd, ctx);
    }

    return NULL;
}

int metrics_start(daemon_ctx_t *ctx, int port) {
    /* Create socket */
    metrics_fd = socket(AF_INET, SOCK_STREAM, 0);
    if (metrics_fd < 0) {
        perror("metrics socket");
        return -1;
    }

    /* Allow address reuse */
    int opt = 1;
    setsockopt(metrics_fd, SOL_SOCKET, SO_REUSEADDR, &opt, sizeof(opt));

    /* Bind to port */
    struct sockaddr_in addr = {
        .sin_family = AF_INET,
        .sin_port = htons(port),
        .sin_addr.s_addr = INADDR_ANY
    };

    if (bind(metrics_fd, (struct sockaddr *)&addr, sizeof(addr)) < 0) {
        perror("metrics bind");
        close(metrics_fd);
        metrics_fd = -1;
        return -1;
    }

    if (listen(metrics_fd, 16) < 0) {
        perror("metrics listen");
        close(metrics_fd);
        metrics_fd = -1;
        return -1;
    }

    metrics_ctx = ctx;
    atomic_store(&metrics_shutdown, false);

    /* Start thread */
    if (pthread_create(&metrics_thread, NULL, metrics_thread_func, ctx) != 0) {
        perror("metrics pthread_create");
        close(metrics_fd);
        metrics_fd = -1;
        return -1;
    }

    fprintf(stderr, "Metrics endpoint: http://0.0.0.0:%d/metrics\n", port);
    return 0;
}

void metrics_stop(void) {
    if (metrics_fd < 0) {
        return;
    }

    atomic_store(&metrics_shutdown, true);
    pthread_join(metrics_thread, NULL);

    close(metrics_fd);
    metrics_fd = -1;
}

int metrics_running(void) {
    return metrics_fd >= 0;
}
