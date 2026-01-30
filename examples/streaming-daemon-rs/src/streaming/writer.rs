//! Async SSE writer with timeout support.

use bytes::Bytes;
use std::io;
use std::time::Duration;
use tokio::io::{AsyncWriteExt, BufWriter};
use tokio::net::TcpStream;

use super::sse::{format_sse_chunk, format_sse_done, format_sse_error, SSE_HEADERS};
use crate::metrics;

/// Buffered SSE writer with per-write timeout.
pub struct SseWriter {
    writer: BufWriter<TcpStream>,
    write_timeout: Duration,
    bytes_written: u64,
}

impl SseWriter {
    /// Create a new SSE writer.
    pub fn new(stream: TcpStream, write_timeout: Duration) -> Self {
        Self {
            writer: BufWriter::with_capacity(8192, stream),
            write_timeout,
            bytes_written: 0,
        }
    }

    /// Send HTTP headers for SSE.
    pub async fn send_headers(&mut self) -> io::Result<()> {
        self.write_with_timeout(SSE_HEADERS.as_bytes()).await?;
        self.flush_with_timeout().await
    }

    /// Send a content chunk.
    pub async fn send_chunk(&mut self, content: &str) -> io::Result<()> {
        let data = format_sse_chunk(content);
        self.write_with_timeout(&data).await?;
        self.flush_with_timeout().await
    }

    /// Send the done marker.
    pub async fn send_done(&mut self) -> io::Result<()> {
        let data = format_sse_done();
        self.write_with_timeout(&data).await?;
        self.flush_with_timeout().await
    }

    /// Send an error.
    pub async fn send_error(&mut self, error: &str) -> io::Result<()> {
        let data = format_sse_error(error);
        self.write_with_timeout(&data).await?;
        self.flush_with_timeout().await
    }

    /// Write raw bytes with timeout.
    pub async fn write_bytes(&mut self, data: &Bytes) -> io::Result<()> {
        self.write_with_timeout(data).await?;
        self.flush_with_timeout().await
    }

    /// Shutdown the writer.
    pub async fn shutdown(&mut self) -> io::Result<()> {
        // Final flush - log errors but don't fail shutdown
        if let Err(e) = self.flush_with_timeout().await {
            tracing::warn!(error = %e, "Final flush failed during shutdown");
        }
        // Shutdown write side with timeout to avoid hanging on slow/broken clients
        tokio::time::timeout(self.write_timeout, self.writer.shutdown())
            .await
            .map_err(|_| io::Error::new(io::ErrorKind::TimedOut, "Shutdown timeout"))?
    }

    /// Get total bytes written.
    pub fn bytes_written(&self) -> u64 {
        self.bytes_written
    }

    async fn write_with_timeout(&mut self, data: &[u8]) -> io::Result<()> {
        tokio::time::timeout(self.write_timeout, self.writer.write_all(data))
            .await
            .map_err(|_| io::Error::new(io::ErrorKind::TimedOut, "Write timeout"))??;

        self.bytes_written += data.len() as u64;
        metrics::record_bytes_sent(data.len() as u64);
        Ok(())
    }

    async fn flush_with_timeout(&mut self) -> io::Result<()> {
        tokio::time::timeout(self.write_timeout, self.writer.flush())
            .await
            .map_err(|_| io::Error::new(io::ErrorKind::TimedOut, "Flush timeout"))?
    }
}
