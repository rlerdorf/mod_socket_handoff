mod config;
mod handlers;
mod responses;

use std::sync::Arc;
use std::time::Duration;

use axum::routing::{get, post};
use axum::Router;
use clap::Parser;
use tokio::net::TcpListener;
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

    // Bind and serve
    let listener = TcpListener::bind(&config.listen)
        .await
        .expect("Failed to bind to address");

    info!(
        "Mock LLM API server listening on {} ({} workers)",
        config.listen,
        config.worker_threads()
    );

    axum::serve(listener, app)
        .await
        .expect("Server error");
}
