mod config;
mod handlers;
mod patterns;
mod responses;
mod test_data;

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

use rustls::ServerConfig;
use tokio_rustls::TlsAcceptor;

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
    keep_alive_timeout: Option<Duration>,
}

impl Http2Settings {
    fn from_config(config: &Config) -> Self {
        // Only configure keep-alive settings when enabled (h2_keepalive_secs > 0)
        let (keep_alive_interval, keep_alive_timeout) = if config.h2_keepalive_secs > 0 {
            let interval = Duration::from_secs(config.h2_keepalive_secs);
            // Use a longer timeout than interval (2x) to allow for network delays
            // and processing time, avoiding premature connection closure.
            let timeout = Duration::from_secs(config.h2_keepalive_secs.saturating_mul(2));
            (Some(interval), Some(timeout))
        } else {
            (None, None)
        };

        Self {
            max_concurrent_streams: config.h2_max_streams,
            initial_stream_window_size: config.h2_stream_window_kb * 1024,
            initial_connection_window_size: config.h2_connection_window_kb * 1024,
            max_send_buffer_size: (config.h2_send_buffer_kb as usize) * 1024,
            keep_alive_interval,
            keep_alive_timeout,
        }
    }

    fn configure_builder(&self, builder: &mut http2::Builder<TokioExecutor>) {
        builder.max_concurrent_streams(self.max_concurrent_streams);
        builder.initial_stream_window_size(self.initial_stream_window_size);
        builder.initial_connection_window_size(self.initial_connection_window_size);
        builder.adaptive_window(true);
        // Rely on the HTTP/2 max frame size RFC 7540 default (16 KiB).
        // Larger frames can increase memory pressure and cause interoperability
        // issues with some clients and intermediaries, so we intentionally do
        // not expose this as a user-tunable CLI option.
        builder.max_send_buf_size(self.max_send_buffer_size);

        // Keep-alive requires a timer for scheduling pings
        if let (Some(interval), Some(timeout)) = (self.keep_alive_interval, self.keep_alive_timeout)
        {
            builder.timer(TokioTimer::new());
            builder.keep_alive_interval(Some(interval));
            builder.keep_alive_timeout(timeout);
        }
    }
}

/// Create TLS configuration for HTTPS server.
/// If cert/key files are provided, loads them; otherwise generates a self-signed cert.
fn create_tls_config(config: &Config) -> Arc<ServerConfig> {
    use rustls::pki_types::{CertificateDer, PrivateKeyDer};

    let (certs, key) =
        if let (Some(cert_path), Some(key_path)) = (&config.tls_cert, &config.tls_key) {
            // Load from files
            let cert_pem = std::fs::read(cert_path).expect("Failed to read TLS cert file");
            let key_pem = std::fs::read(key_path).expect("Failed to read TLS key file");

            let certs: Vec<CertificateDer<'static>> = rustls_pemfile::certs(&mut &cert_pem[..])
                .map(|r| r.expect("Failed to parse certificate"))
                .collect();

            let key = rustls_pemfile::private_key(&mut &key_pem[..])
                .expect("Failed to parse private key")
                .expect("No private key found");

            (certs, key)
        } else {
            // Generate self-signed certificate at startup
            info!("Generating self-signed TLS certificate...");
            let subject_alt_names = vec!["localhost".to_string(), "127.0.0.1".to_string()];

            let cert = rcgen::generate_simple_self_signed(subject_alt_names)
                .expect("Failed to generate self-signed certificate");

            let cert_der = CertificateDer::from(cert.cert.der().to_vec());
            let key_der = PrivateKeyDer::try_from(cert.key_pair.serialize_der())
                .expect("Failed to serialize private key");

            (vec![cert_der], key_der)
        };

    // Configure TLS with performance-optimized settings:
    // - TLS 1.3 only (fastest handshake, 1-RTT)
    // - AES-128-GCM preferred (hardware accelerated via AES-NI)
    // - ALPN with h2 for HTTP/2 negotiation
    let mut server_config = ServerConfig::builder()
        .with_no_client_auth()
        .with_single_cert(certs, key)
        .expect("Failed to create TLS config");

    // Enable HTTP/2 via ALPN
    server_config.alpn_protocols = vec![b"h2".to_vec()];

    Arc::new(server_config)
}

fn main() {
    // Install the ring crypto provider for rustls (required when multiple providers available)
    rustls::crypto::ring::default_provider()
        .install_default()
        .expect("Failed to install rustls crypto provider");

    let config = Config::parse();

    // Initialize tracing
    // Suppress h2 crate warnings about malformed PING frames (common with Swoole HTTP/2 client)
    // Suppress rustls warnings about IP address in SNI (common with PHP clients connecting to 127.0.0.1)
    let filter = if config.quiet {
        EnvFilter::new("warn,h2=error,rustls=error")
    } else {
        EnvFilter::try_from_default_env()
            .unwrap_or_else(|_| EnvFilter::new("info,h2=error,rustls=error"))
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

    // Optional TLS configuration
    let tls_acceptor = if config.tls {
        let tls_config = create_tls_config(&config);
        info!("TLS enabled (HTTPS with HTTP/2 ALPN)");
        Some(TlsAcceptor::from(tls_config))
    } else {
        None
    };

    let protocol = if config.tls { "https" } else { "http" };
    info!(
        "Mock LLM API server listening on {}://{} ({} workers)",
        protocol,
        config.listen,
        config.worker_threads()
    );

    // Spawn TCP accept loop
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
            let tls_acceptor = tls_acceptor.clone();

            tokio::spawn(async move {
                // HTTP/2 settings optimized for high-concurrency streaming
                let mut builder = http2::Builder::new(TokioExecutor::new());
                h2_settings.configure_builder(&mut builder);

                if let Some(acceptor) = tls_acceptor {
                    // HTTPS: TLS handshake then HTTP/2
                    match acceptor.accept(stream).await {
                        Ok(tls_stream) => {
                            let io = TokioIo::new(tls_stream);
                            let service = TowerToHyperService::new(app);
                            if let Err(e) = builder.serve_connection(io, service).await {
                                let err_str = format!("{}", e);
                                if !err_str.contains("connection closed") {
                                    tracing::debug!("H2 TLS connection error: {}", e);
                                }
                            }
                        }
                        Err(e) => {
                            tracing::debug!("TLS handshake error: {}", e);
                        }
                    }
                } else {
                    // HTTP: h2c prior knowledge (no TLS)
                    let io = TokioIo::new(stream);
                    let service = TowerToHyperService::new(app);
                    if let Err(e) = builder.serve_connection(io, service).await {
                        let err_str = format!("{}", e);
                        if !err_str.contains("connection closed") {
                            tracing::debug!("H2 connection error: {}", e);
                        }
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
