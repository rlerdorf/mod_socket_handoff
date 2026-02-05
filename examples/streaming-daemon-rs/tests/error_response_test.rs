//! Integration tests for backend error handling.
//!
//! Verifies that the daemon sends proper HTTP error responses (502/504)
//! when the backend is unavailable or times out, instead of sending
//! HTTP 200 followed by a connection drop.

use std::io::{IoSlice, Read};
use std::net::TcpListener;
use std::os::unix::io::AsRawFd;
use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use nix::sys::socket::{sendmsg, ControlMessage, MsgFlags};

use streaming_daemon_rs::backend::{ChunkStream, StreamChunk, StreamRequest, StreamingBackend};
use streaming_daemon_rs::config::ServerConfig;
use streaming_daemon_rs::error::BackendError;
use streaming_daemon_rs::metrics::init_metrics;
use streaming_daemon_rs::server::ConnectionHandler;
use streaming_daemon_rs::shutdown::ShutdownCoordinator;

/// Backend that always returns an error on stream().
struct FailingBackend;

#[async_trait]
impl StreamingBackend for FailingBackend {
    fn name(&self) -> &'static str {
        "failing"
    }

    async fn stream(&self, _request: StreamRequest) -> Result<ChunkStream, BackendError> {
        Err(BackendError::Connection("connection refused".to_string()))
    }

    async fn health_check(&self) -> Result<(), BackendError> {
        Err(BackendError::Connection("unhealthy".to_string()))
    }
}

/// Backend that hangs forever on stream(), simulating an unresponsive API.
struct HangingBackend;

#[async_trait]
impl StreamingBackend for HangingBackend {
    fn name(&self) -> &'static str {
        "hanging"
    }

    async fn stream(&self, _request: StreamRequest) -> Result<ChunkStream, BackendError> {
        // Sleep forever to simulate an unresponsive backend
        tokio::time::sleep(Duration::from_secs(3600)).await;
        unreachable!()
    }

    async fn health_check(&self) -> Result<(), BackendError> {
        Ok(())
    }
}

/// Backend that succeeds, returning a single chunk then done.
struct SuccessBackend;

/// Simple chunk stream for testing.
struct TestChunkStream {
    chunks: std::vec::IntoIter<StreamChunk>,
}

impl streaming_daemon_rs::backend::ChunkStreamTrait for TestChunkStream {
    fn next(&mut self) -> std::pin::Pin<Box<dyn std::future::Future<Output = Option<Result<StreamChunk, BackendError>>> + Send + '_>> {
        let chunk = self.chunks.next();
        Box::pin(async move { chunk.map(Ok) })
    }
}

#[async_trait]
impl StreamingBackend for SuccessBackend {
    fn name(&self) -> &'static str {
        "success"
    }

    async fn stream(&self, _request: StreamRequest) -> Result<ChunkStream, BackendError> {
        let chunks = vec![StreamChunk::content("hello"), StreamChunk::done()];
        Ok(Box::new(TestChunkStream { chunks: chunks.into_iter() }))
    }

    async fn health_check(&self) -> Result<(), BackendError> {
        Ok(())
    }
}

/// Helper: create a real TCP socket pair (via loopback listener+connect),
/// send the server-side fd via SCM_RIGHTS handoff, run the handler,
/// and read back the HTTP response from the client side.
async fn run_handler_and_capture(backend: Arc<dyn StreamingBackend>, config: ServerConfig) -> String {
    // Create a real TCP connection (needed because daemon calls set_nodelay)
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let addr = listener.local_addr().unwrap();
    let client_tcp = std::net::TcpStream::connect(addr).unwrap();
    let (server_tcp, _) = listener.accept().unwrap();

    // Create the Apache <-> daemon Unix socket pair for handoff
    let (apache_side, daemon_side) = std::os::unix::net::UnixStream::pair().unwrap();

    // Send the server-side TCP fd from "Apache" to the daemon
    let data = br#"{"prompt": "test"}"#;
    let iov = [IoSlice::new(data)];
    let fds = [server_tcp.as_raw_fd()];
    let cmsg = [ControlMessage::ScmRights(&fds)];
    sendmsg::<nix::sys::socket::SockaddrStorage>(apache_side.as_raw_fd(), &iov, &cmsg, MsgFlags::empty(), None).unwrap();
    // Close Apache's copies - daemon now owns the fd
    drop(server_tcp);
    drop(apache_side);

    // Convert daemon_side to tokio UnixStream
    daemon_side.set_nonblocking(true).unwrap();
    let tokio_stream = tokio::net::UnixStream::from_std(daemon_side).unwrap();

    let shutdown = ShutdownCoordinator::new();
    let guard = shutdown.register_connection();
    let handler = ConnectionHandler::new(config, backend);

    handler.handle(tokio_stream, guard).await;

    // Read the response from client side
    client_tcp.set_nonblocking(false).unwrap();
    client_tcp.set_read_timeout(Some(Duration::from_secs(2))).unwrap();
    let mut buf = Vec::new();
    let mut tmp = [0u8; 4096];
    loop {
        match (&client_tcp).read(&mut tmp) {
            Ok(0) => break,
            Ok(n) => buf.extend_from_slice(&tmp[..n]),
            Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => break,
            Err(e) if e.kind() == std::io::ErrorKind::TimedOut => break,
            Err(e) => panic!("read error: {}", e),
        }
    }

    String::from_utf8_lossy(&buf).to_string()
}

fn test_config() -> ServerConfig {
    ServerConfig {
        handoff_timeout_secs: 5,
        write_timeout_secs: 5,
        backend_timeout_secs: 2,
        ..ServerConfig::default()
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn test_backend_error_returns_502() {
    init_metrics();

    let backend: Arc<dyn StreamingBackend> = Arc::new(FailingBackend);
    let response = run_handler_and_capture(backend, test_config()).await;

    assert!(response.starts_with("HTTP/1.1 502 Bad Gateway"), "Expected 502 response, got: {}", response);
    assert!(response.contains("Backend unavailable"));
}

#[tokio::test(flavor = "multi_thread")]
async fn test_backend_timeout_returns_504() {
    init_metrics();

    let mut config = test_config();
    // Very short timeout to trigger the 504 quickly
    config.backend_timeout_secs = 1;

    let backend: Arc<dyn StreamingBackend> = Arc::new(HangingBackend);
    let response = run_handler_and_capture(backend, config).await;

    assert!(response.starts_with("HTTP/1.1 504 Gateway Timeout"), "Expected 504 response, got: {}", response);
    assert!(response.contains("Backend timeout"));
}

#[tokio::test(flavor = "multi_thread")]
async fn test_successful_backend_returns_200() {
    init_metrics();

    let backend: Arc<dyn StreamingBackend> = Arc::new(SuccessBackend);
    let response = run_handler_and_capture(backend, test_config()).await;

    assert!(response.starts_with("HTTP/1.1 200 OK"), "Expected 200 response, got: {}", response);
    assert!(response.contains("text/event-stream"));
}
