/*
 * server.h - Unix socket listener
 */
#ifndef SERVER_H
#define SERVER_H

#include "daemon.h"

/* Create and bind Unix socket listener */
int server_create_listener(daemon_ctx_t *ctx);

/* Close listener and remove socket file */
void server_close_listener(daemon_ctx_t *ctx);

/* Check if another daemon is using the socket */
int server_check_stale_socket(const char *path);

#endif /* SERVER_H */
