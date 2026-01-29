//! Connection handler for processing handoffs and streaming responses.

use std::os::unix::io::AsRawFd;
use std::panic::AssertUnwindSafe;
use std::sync::Arc;
use std::time::Instant;

use futures::FutureExt;
use tokio::net::UnixStream;

use crate::backend::StreamingBackend;
use crate::config::ServerConfig;
use crate::error::HandoffError;
use crate::metrics::{self, Timer};
use crate::server::handoff::{receive_handoff, HandoffResult};
use crate::shutdown::ConnectionGuard;
use crate::streaming::{SseWriter, StreamRequest};

/// Connection handler that processes a single handoff.
pub struct ConnectionHandler {
    config: ServerConfig,
    backend: Arc<dyn StreamingBackend>,
}

impl ConnectionHandler {
    /// Create a new connection handler.
    pub fn new(config: ServerConfig, backend: Arc<dyn StreamingBackend>) -> Self {
        Self { config, backend }
    }

    /// Handle a connection from Apache.
    ///
    /// This is the main entry point for processing a handoff:
    /// 1. Receive the fd and data via SCM_RIGHTS
    /// 2. Stream response to the client
    /// 3. Close the client connection
    pub async fn handle(
        &self,
        stream: UnixStream,
        guard: ConnectionGuard,
    ) {
        let conn_id = guard.id();
        let span = tracing::info_span!("connection", id = conn_id);
        let _enter = span.enter();

        // Wrap in panic catcher
        let result = AssertUnwindSafe(self.handle_inner(stream, &guard))
            .catch_unwind()
            .await;

        match result {
            Ok(Ok(())) => {
                tracing::debug!(id = conn_id, "Connection completed successfully");
            }
            Ok(Err(e)) => {
                tracing::error!(id = conn_id, error = %e, "Connection error");
            }
            Err(panic) => {
                let panic_msg = if let Some(s) = panic.downcast_ref::<&str>() {
                    s.to_string()
                } else if let Some(s) = panic.downcast_ref::<String>() {
                    s.clone()
                } else {
                    "Unknown panic".to_string()
                };
                tracing::error!(id = conn_id, panic = %panic_msg, "Connection handler panicked");
            }
        }
        // Note: active connections gauge is updated atomically in ConnectionGuard::drop
    }

    async fn handle_inner(
        &self,
        stream: UnixStream,
        guard: &ConnectionGuard,
    ) -> Result<(), ConnectionError> {
        // Receive handoff from Apache
        let handoff_timer = Timer::new();
        let handoff = receive_handoff(
            &stream,
            self.config.handoff_timeout(),
            self.config.handoff_buffer_size,
        )
        .await
        .map_err(|e| {
            metrics::record_handoff_error();
            ConnectionError::Handoff(e)
        })?;

        metrics::record_handoff_success(handoff_timer.elapsed());

        let client_fd = handoff.client_fd.as_raw_fd();
        tracing::info!(
            client_fd = client_fd,
            data_len = handoff.raw_data_len,
            user_id = ?handoff.data.user_id,
            prompt_len = handoff.data.prompt.as_ref().map(|s| s.len()),
            "Received handoff"
        );

        // Stream response to client
        self.stream_to_client(handoff, guard).await
    }

    async fn stream_to_client(
        &self,
        handoff: HandoffResult,
        guard: &ConnectionGuard,
    ) -> Result<(), ConnectionError> {
        let stream_timer = Instant::now();

        // Destructure handoff to avoid partial move issues
        let HandoffResult {
            client_fd,
            data,
            raw_data_len: _,
        } = handoff;

        // Convert OwnedFd to std::net::TcpStream using safe ownership transfer (Rust 1.80+)
        let std_stream = std::net::TcpStream::from(client_fd);

        // Set non-blocking for tokio
        std_stream.set_nonblocking(true).map_err(|e| {
            ConnectionError::Stream(format!("Failed to set non-blocking: {}", e))
        })?;

        let tcp_stream = tokio::net::TcpStream::from_std(std_stream).map_err(|e| {
            ConnectionError::Stream(format!("Failed to create tokio stream: {}", e))
        })?;

        // Create SSE writer
        let mut writer = SseWriter::new(tcp_stream, self.config.write_timeout());

        // Send HTTP headers
        writer.send_headers().await.map_err(|e| {
            ConnectionError::Stream(format!("Failed to send headers: {}", e))
        })?;

        // Create stream request
        let request = StreamRequest::from_handoff(&data, guard.id());

        // Get stream from backend
        let mut shutdown_rx = guard.subscribe();

        // Stream chunks to client
        let backend_timer = Timer::new();
        let mut ttfb_recorded = false;

        let mut chunk_stream = self.backend.stream(request).await.map_err(|e| {
            metrics::record_backend_error(self.backend.name());
            ConnectionError::Backend(e.to_string())
        })?;

        metrics::record_backend_request(self.backend.name());

        // Stream chunks - track whether we completed normally
        let mut stream_completed_normally = false;

        loop {
            tokio::select! {
                biased;

                // Check for shutdown
                _ = shutdown_rx.changed() => {
                    tracing::info!("Shutdown signaled, closing stream");
                    break;
                }

                // Get next chunk
                chunk = chunk_stream.next() => {
                    match chunk {
                        Some(Ok(chunk)) => {
                            // Record TTFB on first chunk
                            if !ttfb_recorded {
                                metrics::record_backend_ttfb(
                                    self.backend.name(),
                                    backend_timer.elapsed()
                                );
                                ttfb_recorded = true;
                            }

                            // Write content first (some backends include content in done chunk)
                            if !chunk.content.is_empty() {
                                if let Err(e) = writer.send_chunk(&chunk.content).await {
                                    metrics::record_stream_error();
                                    tracing::warn!(error = %e, "Client write error");
                                    break;
                                }
                                metrics::record_chunk_sent();
                            }

                            // Then check for done marker
                            if chunk.done {
                                stream_completed_normally = true;
                                break;
                            }
                        }
                        Some(Err(e)) => {
                            metrics::record_backend_error(self.backend.name());
                            tracing::error!(error = %e, "Backend stream error");
                            // Try to send error to client
                            let _ = writer.send_error(&e.to_string()).await;
                            break;
                        }
                        None => {
                            // Stream ended naturally
                            stream_completed_normally = true;
                            break;
                        }
                    }
                }
            }
        }

        // Only send done marker on normal completion (not on error or shutdown)
        if stream_completed_normally {
            let _ = writer.send_done().await;
        }
        let _ = writer.shutdown().await;

        metrics::record_backend_duration(self.backend.name(), backend_timer.elapsed());
        metrics::record_stream_duration(stream_timer.elapsed());

        tracing::info!(
            duration_ms = stream_timer.elapsed().as_millis(),
            "Stream completed"
        );

        Ok(())
    }
}

/// Errors during connection handling.
#[derive(Debug, thiserror::Error)]
pub enum ConnectionError {
    #[error("Handoff error: {0}")]
    Handoff(#[from] HandoffError),

    #[error("Stream error: {0}")]
    Stream(String),

    #[error("Backend error: {0}")]
    Backend(String),
}

