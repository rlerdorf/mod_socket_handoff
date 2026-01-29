//! Streaming daemon for mod_socket_handoff.
//!
//! Receives client connections from Apache via SCM_RIGHTS and streams
//! LLM responses (or mock responses for testing).
//!
//! # Usage
//!
//! ```bash
//! # With config file
//! streaming-daemon-rs config/daemon.toml
//!
//! # With environment variables
//! DAEMON_SOCKET_PATH=/var/run/test.sock streaming-daemon-rs
//!
//! # Mock backend for testing
//! DAEMON_BACKEND_PROVIDER=mock streaming-daemon-rs
//! ```

use std::path::PathBuf;
use std::sync::Arc;

use clap::Parser;
use tokio::signal::unix::{signal, SignalKind};
use tracing_subscriber::{fmt, prelude::*, EnvFilter};

use streaming_daemon_rs::{
    backend::create_backend,
    config::Config,
    metrics::{init_metrics, start_metrics_server},
    server::{ConnectionHandler, UnixSocketListener},
    shutdown::ShutdownCoordinator,
};

/// Streaming daemon for Apache socket handoffs.
#[derive(Parser, Debug)]
#[command(name = "streaming-daemon-rs")]
#[command(author, version, about, long_about = None)]
struct Args {
    /// Path to configuration file (TOML).
    #[arg(value_name = "CONFIG")]
    config: Option<PathBuf>,

    /// Override socket path.
    #[arg(short, long)]
    socket: Option<String>,

    /// Override backend provider (mock, openai).
    #[arg(short, long)]
    backend: Option<String>,

    /// Enable debug logging.
    #[arg(short, long)]
    debug: bool,
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    let args = Args::parse();

    // Load configuration
    let mut config = Config::load(args.config.as_ref())?;

    // Apply CLI overrides
    if let Some(socket) = args.socket {
        config.server.socket_path = socket;
    }
    if let Some(backend) = args.backend {
        config.backend.provider = backend;
    }
    if args.debug {
        config.logging.level = "debug".to_string();
    }

    // Initialize logging
    init_logging(&config.logging)?;

    tracing::info!(
        socket_path = %config.server.socket_path,
        backend = %config.backend.provider,
        max_connections = config.server.max_connections,
        "Starting streaming daemon"
    );

    // Initialize metrics
    init_metrics();

    // Start metrics server if enabled
    if config.metrics.enabled {
        let addr = config.metrics.listen_addr.parse()?;
        start_metrics_server(addr).await?;
    }

    // Create backend
    let backend = create_backend(&config.backend)?;
    tracing::info!(backend = backend.name(), "Backend initialized");

    // Create shutdown coordinator
    let shutdown = ShutdownCoordinator::new();

    // Create Unix socket listener
    let listener = UnixSocketListener::bind(&config.server, shutdown.clone()).await?;

    tracing::info!(
        socket_path = %listener.socket_path().display(),
        "Daemon listening"
    );

    // Set up signal handlers
    let shutdown_clone = shutdown.clone();
    tokio::spawn(async move {
        handle_signals(shutdown_clone).await;
    });

    // Create connection handler
    let handler = Arc::new(ConnectionHandler::new(config.server.clone(), backend));

    // Accept loop
    loop {
        match listener.accept().await {
            Some(conn) => {
                let handler = handler.clone();
                tokio::spawn(async move {
                    handler.handle(conn.stream, conn.guard).await;
                });
            }
            None => {
                // Shutdown signaled
                tracing::info!("Accept loop terminated");
                break;
            }
        }
    }

    // Wait for connections to drain
    tracing::info!(
        active = shutdown.active_connections(),
        timeout_secs = config.server.shutdown_timeout_secs,
        "Waiting for connections to drain"
    );

    let drain_result = tokio::time::timeout(
        config.server.shutdown_timeout(),
        shutdown.wait_for_drain(),
    )
    .await;

    match drain_result {
        Ok(()) => {
            tracing::info!("All connections drained");
        }
        Err(_) => {
            tracing::warn!(
                active = shutdown.active_connections(),
                "Shutdown timeout reached, forcing exit"
            );
        }
    }

    tracing::info!("Daemon stopped");
    Ok(())
}

/// Initialize logging with tracing.
fn init_logging(config: &streaming_daemon_rs::config::LoggingConfig) -> anyhow::Result<()> {
    let filter = EnvFilter::try_from_default_env()
        .or_else(|_| EnvFilter::try_new(&config.level))?;

    match config.format.as_str() {
        "json" => {
            tracing_subscriber::registry()
                .with(filter)
                .with(fmt::layer().json())
                .init();
        }
        _ => {
            tracing_subscriber::registry()
                .with(filter)
                .with(fmt::layer())
                .init();
        }
    }

    Ok(())
}

/// Handle Unix signals.
async fn handle_signals(shutdown: ShutdownCoordinator) {
    let mut sigint = signal(SignalKind::interrupt()).expect("Failed to register SIGINT");
    let mut sigterm = signal(SignalKind::terminate()).expect("Failed to register SIGTERM");
    let mut sighup = signal(SignalKind::hangup()).expect("Failed to register SIGHUP");

    loop {
        tokio::select! {
            _ = sigint.recv() => {
                tracing::info!("Received SIGINT, initiating shutdown");
                shutdown.shutdown();
                break;
            }
            _ = sigterm.recv() => {
                tracing::info!("Received SIGTERM, initiating shutdown");
                shutdown.shutdown();
                break;
            }
            _ = sighup.recv() => {
                tracing::info!(
                    active_connections = shutdown.active_connections(),
                    "Received SIGHUP, status report"
                );
            }
        }
    }
}
