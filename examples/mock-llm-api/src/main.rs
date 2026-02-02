mod config;
mod handlers;
mod responses;

use std::sync::Arc;
use std::time::Duration;

use axum::routing::{get, post};
use axum::Router;
use clap::Parser;
use hyper::server::conn::http2;
use hyper_util::rt::{TokioExecutor, TokioIo, TokioTimer};
use hyper_util::service::TowerToHyperService;
use tokio::net::TcpListener;
#[cfg(unix)]
use tokio::net::UnixListener;
use tracing::info;
use tracing_subscriber::EnvFilter;

use config::Config;
use handlers::AppState;
use responses::ResponseChunks;

/// HTTP/2 settings for high-concurrency streaming
#[derive(Clone, Copy)]
struct Http2Settings {
    max_concurrent_streams: u32,
    initial_stream_window_size: u32,
    initial_connection_window_size: u32,
    max_send_buffer_size: usize,
    keep_alive_interval: Option<Duration>,
    keep_alive_timeout: Duration,
}

impl Http2Settings {
    fn from_config(config: &Config) -> Self {
        Self {
            max_concurrent_streams: config.h2_max_streams,
            initial_stream_window_size: config.h2_stream_window_kb * 1024,
            initial_connection_window_size: config.h2_connection_window_kb * 1024,
            max_send_buffer_size: (config.h2_send_buffer_kb as usize) * 1024,
            keep_alive_interval: if config.h2_keepalive_secs > 0 {
                Some(Duration::from_secs(config.h2_keepalive_secs))
            } else {
                None
            },
            keep_alive_timeout: Duration::from_secs(config.h2_keepalive_secs.max(10)),
        }
    }

    fn configure_builder(&self, builder: &mut http2::Builder<TokioExecutor>) {
        builder.max_concurrent_streams(self.max_concurrent_streams);
        builder.initial_stream_window_size(self.initial_stream_window_size);
        builder.initial_connection_window_size(self.initial_connection_window_size);
        builder.adaptive_window(true);
        builder.max_frame_size(16 * 1024);
        builder.max_send_buf_size(self.max_send_buffer_size);

        // Keep-alive requires a timer for scheduling pings
        if self.keep_alive_interval.is_some() {
            builder.timer(TokioTimer::new());
            builder.keep_alive_interval(self.keep_alive_interval);
            builder.keep_alive_timeout(self.keep_alive_timeout);
        }
    }
}

fn main() {
    let config = Config::parse();

    // Initialize tracing
    let filter = if config.quiet {
        EnvFilter::new("warn")
    } else {
        EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new("info"))
    };

    tracing_subscriber::fmt()
        .with_env_filter(filter)
        .with_target(false)
        .init();

    // Build the runtime with configured worker threads
    let runtime = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(config.worker_threads())
        .enable_all()
        .build()
        .expect("Failed to build Tokio runtime");

    runtime.block_on(run_server(config));
}

async fn run_server(config: Config) {
    // Pre-compute response chunks at startup
    let chunks = ResponseChunks::new(config.chunk_count);
    let chunk_delay = Duration::from_millis(config.chunk_delay_ms);

    // HTTP/2 settings for high-concurrency streaming
    let h2_settings = Http2Settings::from_config(&config);

    info!(
        "Pre-computed {} SSE chunks, {}ms delay between chunks",
        chunks.len(),
        config.chunk_delay_ms
    );
    info!(
        "HTTP/2: max_streams={}, stream_window={}KB, conn_window={}KB, send_buffer={}KB",
        h2_settings.max_concurrent_streams,
        h2_settings.initial_stream_window_size / 1024,
        h2_settings.initial_connection_window_size / 1024,
        h2_settings.max_send_buffer_size / 1024
    );

    let state = Arc::new(AppState {
        chunks,
        chunk_delay,
    });

    // Build router
    let app = Router::new()
        .route("/v1/chat/completions", post(handlers::chat_completions))
        .route("/v1/models", get(handlers::list_models))
        .route("/health", get(handlers::health))
        .with_state(state);

    // TCP listener (always start for HTTP/2 multiplexing support)
    let tcp_listener = TcpListener::bind(&config.listen)
        .await
        .expect("Failed to bind to TCP address");

    info!(
        "Mock LLM API server listening on {} ({} workers)",
        config.listen,
        config.worker_threads()
    );

    // Spawn TCP accept loop
    // Use pure HTTP/2 builder for h2c prior knowledge (required for curl's CURL_HTTP_VERSION_2_PRIOR_KNOWLEDGE)
    let tcp_app = app.clone();
    let tcp_task = tokio::spawn(async move {
        loop {
            let (stream, _) = match tcp_listener.accept().await {
                Ok(conn) => conn,
                Err(e) => {
                    tracing::error!("Failed to accept TCP connection: {}", e);
                    // Brief backoff on error to avoid busy loop
                    tokio::time::sleep(Duration::from_millis(100)).await;
                    continue;
                }
            };
            let app = tcp_app.clone();

            tokio::spawn(async move {
                let io = TokioIo::new(stream);
                let service = TowerToHyperService::new(app);

                // HTTP/2 settings optimized for high-concurrency streaming
                let mut builder = http2::Builder::new(TokioExecutor::new());
                h2_settings.configure_builder(&mut builder);

                if let Err(e) = builder.serve_connection(io, service).await {
                    // Only log unexpected errors, not normal connection closes
                    let err_str = format!("{}", e);
                    if !err_str.contains("connection closed") {
                        tracing::debug!("H2 connection error: {}", e);
                    }
                }
            });
        }
    });

    // Unix socket listener (if configured)
    #[cfg(unix)]
    if let Some(socket_path) = &config.socket {
        // Remove existing socket file if it exists
        let _ = std::fs::remove_file(socket_path);

        let unix_listener = UnixListener::bind(socket_path).expect("Failed to bind Unix socket");

        // Set permissions to allow other processes to connect
        std::fs::set_permissions(
            socket_path,
            std::os::unix::fs::PermissionsExt::from_mode(0o666),
        )
        .expect("Failed to set socket permissions");

        info!("Also listening on unix:{}", socket_path);

        // Serve on Unix socket with same HTTP/2 settings as TCP
        loop {
            let (stream, _) = match unix_listener.accept().await {
                Ok(conn) => conn,
                Err(e) => {
                    tracing::error!("Failed to accept Unix connection: {}", e);
                    tokio::time::sleep(Duration::from_millis(100)).await;
                    continue;
                }
            };
            let app = app.clone();

            tokio::spawn(async move {
                let io = TokioIo::new(stream);
                let service = TowerToHyperService::new(app);

                // Same HTTP/2 settings as TCP for consistency
                let mut builder = http2::Builder::new(TokioExecutor::new());
                h2_settings.configure_builder(&mut builder);

                if let Err(e) = builder.serve_connection(io, service).await {
                    let err_str = format!("{}", e);
                    if !err_str.contains("connection closed") {
                        tracing::debug!("H2 Unix connection error: {}", e);
                    }
                }
            });
        }
    }

    // If no Unix socket, just wait on TCP task
    tcp_task.await.expect("TCP listener task panicked");
}
