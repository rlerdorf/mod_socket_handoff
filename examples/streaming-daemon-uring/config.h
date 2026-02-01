/*
 * config.h - Configuration file parsing
 *
 * Supports a simple TOML-like configuration format.
 */
#ifndef CONFIG_H
#define CONFIG_H

#include "daemon.h"

/*
 * Load configuration from a TOML file.
 * CLI arguments override file settings.
 *
 * Returns 0 on success, -1 on error.
 * If file doesn't exist, returns 0 (not an error).
 */
int config_load(daemon_ctx_t *ctx, const char *path);

/*
 * Default config file locations (searched in order):
 * 1. ./streaming-daemon.toml
 * 2. /etc/streaming-daemon/config.toml
 * 3. /etc/streaming-daemon.toml
 */
int config_load_default(daemon_ctx_t *ctx);

#endif /* CONFIG_H */
