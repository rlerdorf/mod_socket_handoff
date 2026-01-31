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
use nix::libc;
use tokio::signal::unix::{signal, SignalKind};
use tracing_subscriber::{fmt, prelude::*, EnvFilter};

use streaming_daemon_rs::{
    backend::create_backend,
    config::Config,
    metrics::{init_metrics, print_benchmark_summary, set_benchmark_mode, start_metrics_server},
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

    /// Enable benchmark mode: skip Prometheus updates, print summary on shutdown.
    #[arg(long)]
    benchmark: bool,
}

// Configure tokio runtime with larger blocking thread pool for high concurrency
#[tokio::main(flavor = "multi_thread")]
async fn main() -> anyhow::Result<()> {
    let args = Args::parse();

    // Set benchmark mode if requested
    if args.benchmark {
        set_benchmark_mode(true);
        eprintln!("Benchmark mode enabled: skipping Prometheus updates");
    }

    // Increase file descriptor limit to handle many concurrent connections
    increase_fd_limit();

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

    let drain_result =
        tokio::time::timeout(config.server.shutdown_timeout(), shutdown.wait_for_drain()).await;

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

    // Print benchmark summary if in benchmark mode
    print_benchmark_summary();

    tracing::info!("Daemon stopped");
    Ok(())
}

/// Initialize logging with tracing.
fn init_logging(config: &streaming_daemon_rs::config::LoggingConfig) -> anyhow::Result<()> {
    let filter =
        EnvFilter::try_from_default_env().or_else(|_| EnvFilter::try_new(&config.level))?;

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

/// Increase the file descriptor limit to handle many concurrent connections.
fn increase_fd_limit() {
    use std::mem::MaybeUninit;

    const DESIRED_LIMIT: u64 = 100_000;

    unsafe {
        let mut rlim = MaybeUninit::<libc::rlimit>::uninit();
        if libc::getrlimit(libc::RLIMIT_NOFILE, rlim.as_mut_ptr()) == 0 {
            let mut rlim = rlim.assume_init();
            if (rlim.rlim_cur as u64) < DESIRED_LIMIT {
                rlim.rlim_cur = DESIRED_LIMIT as libc::rlim_t;
                if (rlim.rlim_max as u64) < DESIRED_LIMIT {
                    rlim.rlim_max = DESIRED_LIMIT as libc::rlim_t;
                }
                if libc::setrlimit(libc::RLIMIT_NOFILE, &rlim) == 0 {
                    eprintln!("Increased fd limit to {}", DESIRED_LIMIT);
                } else {
                    eprintln!("Warning: could not increase fd limit");
                }
            }
        }
    }
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
