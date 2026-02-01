/*
 * config.c - Configuration file parsing
 *
 * Simple TOML-like parser supporting:
 * - key = "string"
 * - key = 123
 * - key = true/false
 * - # comments
 * - [section] headers (ignored, flat namespace)
 */
#include "config.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <ctype.h>
#include <errno.h>

/* Trim whitespace from both ends */
static char *trim(char *s) {
    while (isspace((unsigned char)*s)) s++;
    if (*s == '\0') return s;

    char *end = s + strlen(s) - 1;
    while (end > s && isspace((unsigned char)*end)) end--;
    end[1] = '\0';
    return s;
}

/* Parse a single config line */
static int parse_line(daemon_ctx_t *ctx, const char *key, const char *value) {
    /* String values (quoted) */
    if (value[0] == '"') {
        size_t len = strlen(value);
        if (len < 2 || value[len-1] != '"') {
            return -1; /* Unterminated string */
        }
        /* Extract string without quotes */
        char *str = strndup(value + 1, len - 2);
        if (!str) return -1;

        if (strcmp(key, "socket") == 0 || strcmp(key, "socket_path") == 0) {
            free(ctx->socket_path);
            ctx->socket_path = str;
        } else {
            free(str);
        }
        return 0;
    }

    /* Boolean values */
    if (strcmp(value, "true") == 0) {
        if (strcmp(key, "benchmark") == 0) ctx->benchmark_mode = true;
        else if (strcmp(key, "thread_pool") == 0) ctx->use_thread_pool = true;
        else if (strcmp(key, "sqpoll") == 0) ctx->use_sqpoll = true;
        return 0;
    }
    if (strcmp(value, "false") == 0) {
        if (strcmp(key, "benchmark") == 0) ctx->benchmark_mode = false;
        else if (strcmp(key, "thread_pool") == 0) ctx->use_thread_pool = false;
        else if (strcmp(key, "sqpoll") == 0) ctx->use_sqpoll = false;
        return 0;
    }

    /* Integer values */
    char *endptr;
    long num = strtol(value, &endptr, 0);
    if (*endptr == '\0') {
        if (strcmp(key, "max_connections") == 0) {
            if (num < 1 || num > 1000000) {
                fprintf(stderr, "max_connections out of range (1-1000000): %ld\n", num);
                return -1;
            }
            ctx->max_connections = (int)num;
        } else if (strcmp(key, "socket_mode") == 0) {
            if (num < 0 || num > 0777) {
                fprintf(stderr, "socket_mode out of range (0-0777): %ld\n", num);
                return -1;
            }
            ctx->socket_mode = (int)num;
        } else if (strcmp(key, "delay") == 0 || strcmp(key, "message_delay_ms") == 0) {
            if (num < 0 || num > 60000) {
                fprintf(stderr, "message_delay_ms out of range (0-60000): %ld\n", num);
                return -1;
            }
            ctx->message_delay_ms = (int)num;
        } else if (strcmp(key, "pool_size") == 0) {
            if (num < 1 || num > MAX_POOL_SIZE) {
                fprintf(stderr, "pool_size out of range (1-%d): %ld\n", MAX_POOL_SIZE, num);
                return -1;
            }
            ctx->pool_size = (int)num;
        } else if (strcmp(key, "metrics_port") == 0) {
            if (num < 0 || num > 65535) {
                fprintf(stderr, "metrics_port out of range (0-65535): %ld\n", num);
                return -1;
            }
            ctx->metrics_port = (int)num;
        } else if (strcmp(key, "handoff_timeout_ms") == 0) {
            if (num < 1 || num > 60000) {
                fprintf(stderr, "handoff_timeout_ms out of range (1-60000): %ld\n", num);
                return -1;
            }
            ctx->handoff_timeout_ms = (int)num;
        } else if (strcmp(key, "write_timeout_ms") == 0) {
            if (num < 1 || num > 300000) {
                fprintf(stderr, "write_timeout_ms out of range (1-300000): %ld\n", num);
                return -1;
            }
            ctx->write_timeout_ms = (int)num;
        } else if (strcmp(key, "shutdown_timeout_s") == 0) {
            if (num < 1 || num > 3600) {
                fprintf(stderr, "shutdown_timeout_s out of range (1-3600): %ld\n", num);
                return -1;
            }
            ctx->shutdown_timeout_s = (int)num;
        }
        return 0;
    }

    /* Unknown key or invalid value - ignore */
    return 0;
}

int config_load(daemon_ctx_t *ctx, const char *path) {
    FILE *f = fopen(path, "r");
    if (!f) {
        if (errno == ENOENT) {
            return 0; /* File doesn't exist - not an error */
        }
        fprintf(stderr, "Failed to open config file %s: %s\n", path, strerror(errno));
        return -1;
    }

    char line[1024];
    int line_num = 0;

    while (fgets(line, sizeof(line), f)) {
        line_num++;
        char *s = trim(line);

        /* Skip empty lines and comments */
        if (*s == '\0' || *s == '#') continue;

        /* Skip section headers */
        if (*s == '[') continue;

        /* Find = separator */
        char *eq = strchr(s, '=');
        if (!eq) {
            fprintf(stderr, "%s:%d: missing '=' in config line\n", path, line_num);
            continue;
        }

        *eq = '\0';
        char *key = trim(s);
        char *value = trim(eq + 1);

        if (parse_line(ctx, key, value) < 0) {
            fprintf(stderr, "%s:%d: invalid value for '%s'\n", path, line_num, key);
        }
    }

    fclose(f);
    fprintf(stderr, "Loaded config from %s\n", path);
    return 0;
}

int config_load_default(daemon_ctx_t *ctx) {
    static const char *paths[] = {
        "./streaming-daemon.toml",
        "/etc/streaming-daemon/config.toml",
        "/etc/streaming-daemon.toml",
        NULL
    };

    for (int i = 0; paths[i]; i++) {
        FILE *f = fopen(paths[i], "r");
        if (f) {
            fclose(f);
            return config_load(ctx, paths[i]);
        }
    }

    return 0; /* No config file found - use defaults */
}
