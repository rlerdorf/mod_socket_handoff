//! Async SSE writer with timeout support.
//!
//! Writes directly to TcpStream without buffering for lowest latency SSE streaming.

use bytes::Bytes;
use std::io;
use std::time::Duration;
use tokio::io::AsyncWriteExt;
use tokio::net::TcpStream;

use super::sse::{format_error_response, format_sse_chunk, format_sse_done, format_sse_error, SSE_HEADERS};
use crate::metrics;

/// Unbuffered SSE writer with per-write timeout.
/// Writes directly to the TCP stream for lowest latency.
pub struct SseWriter {
    stream: TcpStream,
    write_timeout: Duration,
    bytes_written: u64,
}

impl SseWriter {
    /// Create a new SSE writer.
    pub fn new(stream: TcpStream, write_timeout: Duration) -> Self {
        Self {
            stream,
            write_timeout,
            bytes_written: 0,
        }
    }

    /// Send HTTP headers for SSE.
    pub async fn send_headers(&mut self) -> io::Result<()> {
        self.write_with_timeout(SSE_HEADERS.as_bytes()).await
    }

    /// Send a content chunk.
    pub async fn send_chunk(&mut self, content: &str) -> io::Result<()> {
        let data = format_sse_chunk(content);
        self.write_with_timeout(&data).await
    }

    /// Send the done marker.
    pub async fn send_done(&mut self) -> io::Result<()> {
        let data = format_sse_done();
        self.write_with_timeout(&data).await
    }

    /// Send an error.
    pub async fn send_error(&mut self, error: &str) -> io::Result<()> {
        let data = format_sse_error(error);
        self.write_with_timeout(&data).await
    }

    /// Send an HTTP error response (non-200 status).
    /// Used when the backend is unavailable before headers are sent.
    pub async fn send_error_response(&mut self, status: u16, reason: &str) {
        let response = format_error_response(status, reason);
        if let Err(e) = self.write_with_timeout(response.as_bytes()).await {
            tracing::debug!(status, error = %e, "Failed to send error response to client");
        }
    }

    /// Write raw bytes with timeout.
    pub async fn write_bytes(&mut self, data: &Bytes) -> io::Result<()> {
        self.write_with_timeout(data).await
    }

    /// Shutdown the writer.
    pub async fn shutdown(&mut self) -> io::Result<()> {
        // Shutdown write side with timeout to avoid hanging on slow/broken clients
        tokio::time::timeout(self.write_timeout, self.stream.shutdown())
            .await
            .map_err(|_| io::Error::new(io::ErrorKind::TimedOut, "Shutdown timeout"))?
    }

    /// Get total bytes written.
    pub fn bytes_written(&self) -> u64 {
        self.bytes_written
    }

    async fn write_with_timeout(&mut self, data: &[u8]) -> io::Result<()> {
        tokio::time::timeout(self.write_timeout, self.stream.write_all(data))
            .await
            .map_err(|_| io::Error::new(io::ErrorKind::TimedOut, "Write timeout"))??;

        self.bytes_written += data.len() as u64;
        metrics::record_bytes_sent(data.len() as u64);
        Ok(())
    }
}
