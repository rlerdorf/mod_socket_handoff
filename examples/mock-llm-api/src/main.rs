mod config;
mod handlers;
mod responses;

use std::sync::Arc;
use std::time::Duration;

use axum::routing::{get, post};
use axum::Router;
use clap::Parser;
use hyper::server::conn::http2;
use hyper_util::rt::{TokioExecutor, TokioIo};
use hyper_util::server::conn::auto::Builder as AutoBuilder;
use hyper_util::service::TowerToHyperService;
use tokio::net::TcpListener;
#[cfg(unix)]
use tokio::net::UnixListener;
use tracing::info;
use tracing_subscriber::EnvFilter;

use config::Config;
use handlers::AppState;
use responses::ResponseChunks;

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

    info!(
        "Pre-computed {} SSE chunks, {}ms delay between chunks",
        chunks.len(),
        config.chunk_delay_ms
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
            let (stream, _) = tcp_listener
                .accept()
                .await
                .expect("Failed to accept TCP connection");
            let app = tcp_app.clone();

            tokio::spawn(async move {
                let io = TokioIo::new(stream);
                let service = TowerToHyperService::new(app);

                // Use http2::Builder for h2c prior knowledge support
                // The TokioExecutor is critical for spawning sub-tasks to handle
                // concurrent streams on the same connection.
                // Limit to 100 concurrent streams per connection (HTTP/2 best practice).
                // Curl will open multiple connections for higher concurrency.
                if let Err(e) = http2::Builder::new(TokioExecutor::new())
                    .max_concurrent_streams(100)
                    .serve_connection(io, service)
                    .await
                {
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

        // Serve on Unix socket - use auto builder for HTTP/1.1 and HTTP/2 support
        loop {
            let (stream, _) = unix_listener
                .accept()
                .await
                .expect("Failed to accept Unix connection");
            let app = app.clone();

            tokio::spawn(async move {
                let io = TokioIo::new(stream);
                let service = TowerToHyperService::new(app);

                // AutoBuilder detects HTTP/1.1 vs HTTP/2 automatically
                if let Err(e) = AutoBuilder::new(TokioExecutor::new())
                    .serve_connection(io, service)
                    .await
                {
                    tracing::error!("Connection error: {}", e);
                }
            });
        }
    }

    // If no Unix socket, just wait on TCP task
    tcp_task.await.expect("TCP listener task panicked");
}
