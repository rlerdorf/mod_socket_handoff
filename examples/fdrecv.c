/*
 * fdrecv - Minimal SCM_RIGHTS file descriptor receiver
 *
 * Listens on a Unix socket, receives client file descriptors via SCM_RIGHTS,
 * and executes a handler program with the fd as stdin/stdout.
 *
 * Usage:
 *   fdrecv /var/run/streaming-daemon.sock ./handler.sh
 *   fdrecv /var/run/streaming-daemon.sock php handler.php
 *   fdrecv /var/run/streaming-daemon.sock cat response.http
 *
 * The handler receives:
 *   - stdin/stdout connected to the client socket
 *   - HANDOFF_DATA environment variable with the JSON data
 *
 * Build:
 *   cc -o fdrecv fdrecv.c
 *
 * License: Apache 2.0
 */

#define _GNU_SOURCE
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <errno.h>
#include <signal.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/un.h>
#include <sys/wait.h>

#define MAX_DATA_SIZE 65536

static volatile int running = 1;
static const char *socket_path = NULL;

static void handle_signal(int sig) {
    (void)sig;
    running = 0;
}

static void cleanup(void) {
    if (socket_path) {
        unlink(socket_path);
    }
}

/*
 * Receive file descriptor and data via SCM_RIGHTS
 * Returns the received fd, or -1 on error
 */
static int recv_fd(int sock, char *data, size_t *data_len) {
    struct msghdr msg = {0};
    struct iovec iov;
    union {
        struct cmsghdr cm;
        char control[CMSG_SPACE(sizeof(int))];
    } control_un;
    struct cmsghdr *cmsg;

    /* Set up to receive data */
    iov.iov_base = data;
    iov.iov_len = *data_len;
    msg.msg_iov = &iov;
    msg.msg_iovlen = 1;

    /* Set up to receive control message */
    msg.msg_control = control_un.control;
    msg.msg_controllen = sizeof(control_un.control);

    ssize_t n = recvmsg(sock, &msg, 0);
    if (n <= 0) {
        return -1;
    }
    *data_len = (size_t)n;

    /* Extract file descriptor from control message */
    cmsg = CMSG_FIRSTHDR(&msg);
    if (cmsg == NULL ||
        cmsg->cmsg_level != SOL_SOCKET ||
        cmsg->cmsg_type != SCM_RIGHTS) {
        return -1;
    }

    int fd;
    memcpy(&fd, CMSG_DATA(cmsg), sizeof(fd));
    return fd;
}

/*
 * Handle a connection: receive fd, fork, exec handler
 */
static void handle_connection(int conn, char **handler_argv) {
    char data[MAX_DATA_SIZE];
    size_t data_len = sizeof(data) - 1;

    int client_fd = recv_fd(conn, data, &data_len);
    if (client_fd < 0) {
        fprintf(stderr, "fdrecv: failed to receive fd\n");
        return;
    }
    data[data_len] = '\0';

    pid_t pid = fork();
    if (pid < 0) {
        fprintf(stderr, "fdrecv: fork failed: %s\n", strerror(errno));
        close(client_fd);
        return;
    }

    if (pid == 0) {
        /* Child process */

        /* Set up client fd as stdin/stdout */
        dup2(client_fd, STDIN_FILENO);
        dup2(client_fd, STDOUT_FILENO);
        close(client_fd);

        /* Pass handoff data via environment variable */
        setenv("HANDOFF_DATA", data, 1);

        /* Execute handler */
        execvp(handler_argv[0], handler_argv);
        fprintf(stderr, "fdrecv: exec failed: %s\n", strerror(errno));
        _exit(1);
    }

    /* Parent: close our copy of client fd, child owns it now */
    close(client_fd);
}

/*
 * Reap zombie children
 */
static void reap_children(void) {
    while (waitpid(-1, NULL, WNOHANG) > 0) {
        /* continue reaping */
    }
}

int main(int argc, char **argv) {
    if (argc < 3) {
        fprintf(stderr, "Usage: %s <socket-path> <handler> [args...]\n", argv[0]);
        fprintf(stderr, "\nExample:\n");
        fprintf(stderr, "  %s /var/run/daemon.sock ./handler.sh\n", argv[0]);
        fprintf(stderr, "  %s /var/run/daemon.sock php stream.php\n", argv[0]);
        return 1;
    }

    socket_path = argv[1];
    char **handler_argv = &argv[2];

    /* Set up signal handlers */
    signal(SIGINT, handle_signal);
    signal(SIGTERM, handle_signal);
    signal(SIGCHLD, SIG_IGN);  /* Auto-reap children */
    atexit(cleanup);

    /* Remove stale socket */
    unlink(socket_path);

    /* Create Unix socket */
    int server = socket(AF_UNIX, SOCK_STREAM, 0);
    if (server < 0) {
        perror("socket");
        return 1;
    }

    struct sockaddr_un addr = {0};
    addr.sun_family = AF_UNIX;
    strncpy(addr.sun_path, socket_path, sizeof(addr.sun_path) - 1);

    if (bind(server, (struct sockaddr *)&addr, sizeof(addr)) < 0) {
        perror("bind");
        return 1;
    }

    /* Set permissions so Apache can connect */
    chmod(socket_path, 0666);

    if (listen(server, 16) < 0) {
        perror("listen");
        return 1;
    }

    fprintf(stderr, "fdrecv: listening on %s\n", socket_path);
    fprintf(stderr, "fdrecv: handler: %s\n", handler_argv[0]);

    while (running) {
        /* Use select for timeout so we can check running flag */
        fd_set fds;
        FD_ZERO(&fds);
        FD_SET(server, &fds);
        struct timeval tv = {1, 0};  /* 1 second timeout */

        int ret = select(server + 1, &fds, NULL, NULL, &tv);
        if (ret < 0) {
            if (errno == EINTR) continue;
            perror("select");
            break;
        }
        if (ret == 0) {
            reap_children();
            continue;
        }

        int conn = accept(server, NULL, NULL);
        if (conn < 0) {
            if (errno == EINTR) continue;
            perror("accept");
            continue;
        }

        handle_connection(conn, handler_argv);
        close(conn);
        reap_children();
    }

    close(server);
    fprintf(stderr, "fdrecv: shutting down\n");
    return 0;
}
