//! Shared HTTP client builder for streaming backends.
//!
//! Provides a configured reqwest Client for SSE streaming with proper
//! HTTP/2 multiplexing settings. All backends should use this to avoid
//! duplicating the complex timeout and flow control configuration.

use reqwest::Client;
use std::time::Duration;

use crate::config::Http2Config;
use crate::error::BackendError;

/// Build an HTTP client configured for SSE streaming.
///
/// When HTTP/2 is enabled, the client is configured for high-concurrency
/// multiplexed streaming with no request timeout (SSE streams are long-lived
/// and requests may queue waiting for available HTTP/2 streams).
///
/// When HTTP/2 is disabled, uses HTTP/1.1 with the specified timeout and
/// optional Unix socket for connection pooling.
///
/// # Arguments
///
/// * `http2_config` - HTTP/2 specific settings (window sizes, keep-alive, etc.)
/// * `timeout` - Request timeout (only used for HTTP/1.1 mode)
/// * `pool_max_idle_per_host` - Connection pool size (only used for HTTP/1.1 mode)
/// * `api_socket` - Optional Unix socket path (only used for HTTP/1.1 mode)
/// * `insecure_ssl` - Skip TLS certificate verification (for testing)
/// * `api_base` - API base URL (used to detect http:// vs https://)
pub fn build_streaming_client(
    http2_config: &Http2Config,
    timeout: Duration,
    pool_max_idle_per_host: usize,
    api_socket: Option<&str>,
    insecure_ssl: bool,
    api_base: &str,
) -> Result<Client, BackendError> {
    let mut builder = Client::builder();

    // For SSE streaming with HTTP/2, disable timeouts to avoid issues with
    // long-running streams and HTTP/2 queue waiting. Reqwest's timeout() and
    // read_timeout() apply to the entire request lifecycle including HTTP/2
    // stream queuing, which causes spurious timeouts under high concurrency.
    // The server's keep-alive and connection limits handle cleanup.
    // For HTTP/1.1, use the overall timeout which covers the entire request.
    if http2_config.enabled {
        // No timeout for HTTP/2 - streams are long-lived and requests may queue
        // Larger pool to allow more HTTP/2 connections for high concurrency
        builder = builder.pool_max_idle_per_host(200).pool_idle_timeout(Duration::from_secs(90));
        tracing::info!(pool_size = 200, "HTTP/2: no request timeout for SSE streaming");
    } else {
        builder = builder.timeout(timeout).pool_max_idle_per_host(pool_max_idle_per_host);
    }

    // Allow insecure TLS connections (for testing with self-signed certificates)
    if insecure_ssl {
        builder = builder.danger_accept_invalid_certs(true);
        tracing::warn!("TLS certificate verification disabled (insecure_ssl=true)");
    }

    // Determine if this is an HTTP (not HTTPS) endpoint
    let is_plaintext = api_base.starts_with("http://");

    // Configure HTTP version
    if http2_config.enabled {
        if is_plaintext {
            // For http:// endpoints, use prior knowledge to skip upgrade negotiation.
            // This enables h2c (HTTP/2 over cleartext) directly.
            builder = builder.http2_prior_knowledge();
            tracing::info!("HTTP/2 with prior knowledge (h2c) enabled for plaintext endpoint");
        } else {
            // For https:// endpoints, use ALPN negotiation during TLS handshake.
            // Cannot use prior_knowledge with TLS - let TLS negotiate h2.
            tracing::info!("HTTP/2 via ALPN negotiation enabled for TLS endpoint");
        }

        // TCP keep-alive to detect dead connections
        builder = builder.tcp_keepalive(Duration::from_secs(60));

        // HTTP/2 keep-alive settings from config
        if http2_config.keep_alive_interval_secs > 0 {
            builder = builder
                .http2_keep_alive_interval(Duration::from_secs(http2_config.keep_alive_interval_secs))
                .http2_keep_alive_timeout(Duration::from_secs(http2_config.keep_alive_timeout_secs))
                .http2_keep_alive_while_idle(true);
        }

        // Flow control window sizes from config
        let stream_window = http2_config.initial_stream_window_kb * 1024;
        let conn_window = http2_config.initial_connection_window_kb * 1024;
        builder = builder.http2_initial_stream_window_size(stream_window).http2_initial_connection_window_size(conn_window);

        if http2_config.adaptive_window {
            builder = builder.http2_adaptive_window(true);
        }

        tracing::info!(
            stream_window_kb = http2_config.initial_stream_window_kb,
            conn_window_kb = http2_config.initial_connection_window_kb,
            tcp_keepalive_secs = 60,
            h2_keepalive_secs = http2_config.keep_alive_interval_secs,
            adaptive_window = http2_config.adaptive_window,
            "HTTP/2 flow control configured"
        );
    } else {
        // HTTP/1.1 fallback mode
        builder = builder.http1_only();
        tracing::info!("HTTP/1.1 mode (HTTP/2 disabled)");

        // Unix socket only makes sense for HTTP/1.1 connection pooling.
        // With HTTP/2, multiplexing happens at the protocol layer over TCP,
        // so we use TCP connections instead.
        #[cfg(unix)]
        if let Some(socket_path) = api_socket {
            tracing::info!(socket = %socket_path, "Using Unix socket for API connections");
            builder = builder.unix_socket(socket_path);
        }

        #[cfg(not(unix))]
        let _ = api_socket; // Suppress unused warning on non-Unix
    }

    builder.build().map_err(|e| BackendError::Connection(e.to_string()))
}
