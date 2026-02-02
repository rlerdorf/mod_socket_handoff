/*
 * server.c - Unix socket listener
 */
#include "server.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <errno.h>
#include <fcntl.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/un.h>

int server_check_stale_socket(const char *path) {
    struct stat st;
    if (stat(path, &st) != 0) {
        if (errno == ENOENT) {
            return 0; /* Socket doesn't exist - OK */
        }
        return -1; /* Other error */
    }

    if (!S_ISSOCK(st.st_mode)) {
        fprintf(stderr, "Path %s exists but is not a socket\n", path);
        return -1;
    }

    /* Try to connect - if it succeeds, another daemon is running */
    int probe = socket(AF_UNIX, SOCK_STREAM, 0);
    if (probe < 0) {
        return -1;
    }

    struct sockaddr_un addr = {0};
    addr.sun_family = AF_UNIX;
    strncpy(addr.sun_path, path, sizeof(addr.sun_path) - 1);

    /* Use non-blocking connect with short timeout */
    int flags = fcntl(probe, F_GETFL, 0);
    fcntl(probe, F_SETFL, flags | O_NONBLOCK);

    int ret = connect(probe, (struct sockaddr *)&addr, sizeof(addr));
    if (ret == 0) {
        /* Connected - another daemon is running */
        close(probe);
        fprintf(stderr, "Socket %s is already in use by another process\n", path);
        return -1;
    }

    close(probe);

    if (errno == ECONNREFUSED) {
        /* Socket exists but no one is listening - stale socket */
        fprintf(stderr, "Removing stale socket %s\n", path);
        unlink(path);
        return 0;
    }

    if (errno == EINPROGRESS) {
        /* Connect in progress - another daemon might be running */
        fprintf(stderr, "Socket %s appears to be in use\n", path);
        return -1;
    }

    /* Other error - try to remove anyway */
    unlink(path);
    return 0;
}

int server_create_listener(daemon_ctx_t *ctx) {
    /* Check for stale socket */
    if (server_check_stale_socket(ctx->socket_path) < 0) {
        return -1;
    }

    /* Create socket */
    int fd = socket(AF_UNIX, SOCK_STREAM | SOCK_NONBLOCK | SOCK_CLOEXEC, 0);
    if (fd < 0) {
        perror("socket");
        return -1;
    }

    /* Bind to path */
    struct sockaddr_un addr = {0};
    addr.sun_family = AF_UNIX;
    strncpy(addr.sun_path, ctx->socket_path, sizeof(addr.sun_path) - 1);

    if (bind(fd, (struct sockaddr *)&addr, sizeof(addr)) < 0) {
        perror("bind");
        close(fd);
        return -1;
    }

    /* Set permissions */
    if (chmod(ctx->socket_path, ctx->socket_mode) < 0) {
        perror("chmod");
        /* Non-fatal - continue anyway */
    }

    /* Listen with large backlog for high concurrency.
     * Use 65535 to match net.core.somaxconn limit set by benchmark script.
     * The kernel will cap this to somaxconn if needed.
     */
    if (listen(fd, 65535) < 0) {
        perror("listen");
        close(fd);
        unlink(ctx->socket_path);
        return -1;
    }

    ctx->listen_fd = fd;
    return 0;
}

void server_close_listener(daemon_ctx_t *ctx) {
    if (ctx->listen_fd >= 0) {
        close(ctx->listen_fd);
        ctx->listen_fd = -1;
    }
    if (ctx->socket_path) {
        unlink(ctx->socket_path);
    }
}
