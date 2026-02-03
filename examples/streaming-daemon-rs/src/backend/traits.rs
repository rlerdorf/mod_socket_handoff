//! Backend trait definitions.

use async_trait::async_trait;
use std::pin::Pin;

use crate::error::BackendError;
use crate::server::HandoffData;

/// A streaming backend that can generate responses.
#[async_trait]
pub trait StreamingBackend: Send + Sync {
    /// Get the backend name for metrics/logging.
    fn name(&self) -> &'static str;

    /// Stream a response for the given request.
    async fn stream(&self, request: StreamRequest) -> Result<ChunkStream, BackendError>;

    /// Health check for the backend.
    async fn health_check(&self) -> Result<(), BackendError>;
}

/// Request to stream a response.
#[derive(Debug, Clone)]
pub struct StreamRequest {
    /// The prompt to respond to.
    pub prompt: String,
    /// Model to use (optional, uses backend default).
    pub model: Option<String>,
    /// Maximum tokens to generate.
    pub max_tokens: Option<u32>,
    /// Temperature for generation.
    pub temperature: Option<f32>,
    /// System prompt.
    pub system: Option<String>,
    /// User ID for tracking.
    pub user_id: Option<i64>,
    /// Request ID for correlation.
    pub request_id: String,
    /// Test pattern for validation testing (passed to backend as X-Test-Pattern header).
    pub test_pattern: Option<String>,
}

impl StreamRequest {
    /// Create a stream request from handoff data.
    /// Converts Box<str> fields to String for API use.
    pub fn from_handoff(data: &HandoffData, conn_id: u64) -> Self {
        Self {
            prompt: data
                .prompt
                .as_ref()
                .map(|s| s.to_string())
                .unwrap_or_default(),
            model: data.model.as_ref().map(|s| s.to_string()),
            max_tokens: data.max_tokens,
            temperature: data.temperature,
            system: data.system.as_ref().map(|s| s.to_string()),
            user_id: data.user_id,
            request_id: data
                .request_id
                .as_ref()
                .map(|s| s.to_string())
                .unwrap_or_else(|| format!("conn-{}", conn_id)),
            test_pattern: data.test_pattern.as_ref().map(|s| s.to_string()),
        }
    }
}

/// A chunk of streamed content.
#[derive(Debug, Clone)]
pub struct StreamChunk {
    /// The content of this chunk.
    pub content: String,
    /// Whether this is the final chunk.
    pub done: bool,
    /// Optional metadata.
    pub metadata: Option<ChunkMetadata>,
}

impl StreamChunk {
    /// Create a content chunk.
    pub fn content(content: impl Into<String>) -> Self {
        Self {
            content: content.into(),
            done: false,
            metadata: None,
        }
    }

    /// Create a done marker chunk.
    pub fn done() -> Self {
        Self {
            content: String::new(),
            done: true,
            metadata: None,
        }
    }
}

/// Optional metadata for a chunk.
#[derive(Debug, Clone, Default)]
pub struct ChunkMetadata {
    /// Token count for this chunk.
    pub tokens: Option<u32>,
    /// Finish reason if stream is done.
    pub finish_reason: Option<String>,
}

/// Stream of chunks from a backend.
pub type ChunkStream = Box<dyn ChunkStreamTrait>;

/// Trait for chunk streams.
pub trait ChunkStreamTrait: Send {
    /// Get the next chunk.
    #[allow(clippy::type_complexity)]
    fn next(
        &mut self,
    ) -> Pin<
        Box<
            dyn std::future::Future<Output = Option<Result<StreamChunk, BackendError>>> + Send + '_,
        >,
    >;
}

/// Simple vector-based chunk stream for testing.
pub struct VecChunkStream {
    chunks: std::vec::IntoIter<StreamChunk>,
    delay: Option<std::time::Duration>,
}

impl VecChunkStream {
    pub fn new(chunks: Vec<StreamChunk>, delay: Option<std::time::Duration>) -> Self {
        Self {
            chunks: chunks.into_iter(),
            delay,
        }
    }
}

impl ChunkStreamTrait for VecChunkStream {
    fn next(
        &mut self,
    ) -> Pin<
        Box<
            dyn std::future::Future<Output = Option<Result<StreamChunk, BackendError>>> + Send + '_,
        >,
    > {
        let delay = self.delay;
        let chunk = self.chunks.next();

        Box::pin(async move {
            if let Some(d) = delay {
                tokio::time::sleep(d).await;
            }
            chunk.map(Ok)
        })
    }
}
