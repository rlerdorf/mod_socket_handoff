//! Mock backend for testing and demos.
//!
//! Simulates LLM streaming by returning a canned response word-by-word.

use async_trait::async_trait;
use std::time::Duration;

use super::traits::{StreamChunk, StreamRequest, StreamingBackend, VecChunkStream};
use crate::error::BackendError;

/// Mock backend that simulates LLM streaming.
pub struct MockBackend {
    /// Delay between tokens.
    token_delay: Duration,
}

impl MockBackend {
    /// Create a new mock backend.
    ///
    /// Reads DAEMON_TOKEN_DELAY_MS environment variable for token delay (default: 50ms).
    /// For ~100-second streams with 18 messages, use DAEMON_TOKEN_DELAY_MS=5625.
    pub fn new() -> Self {
        let delay_ms = std::env::var("DAEMON_TOKEN_DELAY_MS")
            .ok()
            .and_then(|v| v.parse::<u64>().ok())
            .unwrap_or(50);
        Self {
            token_delay: Duration::from_millis(delay_ms),
        }
    }

    /// Create with custom token delay.
    pub fn with_delay(token_delay: Duration) -> Self {
        Self { token_delay }
    }
}

impl Default for MockBackend {
    fn default() -> Self {
        Self::new()
    }
}

#[async_trait]
impl StreamingBackend for MockBackend {
    fn name(&self) -> &'static str {
        "mock"
    }

    async fn stream(&self, request: StreamRequest) -> Result<super::ChunkStream, BackendError> {
        // Static messages - no allocation needed
        static STATIC_MESSAGES: &[&str] = &[
            "This daemon uses Tokio async tasks for concurrent handling.",
            "Each connection runs as a lightweight async task.",
            "Expected capacity: 100,000+ concurrent connections.",
            "Memory per connection: ~10-15 KB.",
            "The Apache worker was freed immediately after handoff.",
            "Replace this mock backend with your LLM API integration.",
            "Tokio provides async I/O with work-stealing scheduler.",
            "Rust's zero-cost abstractions make async efficient.",
            "Non-blocking I/O is built into Tokio's runtime.",
            "This is similar to Go goroutines but with static guarantees.",
            "This is message 13 of 18.",
            "This is message 14 of 18.",
            "This is message 15 of 18.",
            "This is message 16 of 18.",
            "This is message 17 of 18.",
            "[DONE-CONTENT]",
        ];

        // Only allocate for dynamic messages (3 strings per connection)
        let prompt_preview = if request.prompt.len() > 50 {
            format!("{}...", &request.prompt[..50])
        } else {
            request.prompt.clone()
        };

        let user_id = request
            .user_id
            .map(|id| id.to_string())
            .unwrap_or_else(|| "unknown".into());

        // Pre-allocate with exact capacity: 2 dynamic + 16 static + 1 done = 19
        let mut chunks = Vec::with_capacity(19);

        // Dynamic messages (allocate)
        chunks.push(StreamChunk::content(format!(
            "Hello from Rust daemon! Prompt: {}",
            prompt_preview
        )));
        chunks.push(StreamChunk::content(format!("User ID: {}", user_id)));

        // Static messages (no allocation for the &str, only StreamChunk wrapper)
        for msg in STATIC_MESSAGES {
            chunks.push(StreamChunk::content(*msg));
        }

        // Done marker
        chunks.push(StreamChunk::done());

        Ok(Box::new(VecChunkStream::new(
            chunks,
            Some(self.token_delay),
        )))
    }

    async fn health_check(&self) -> Result<(), BackendError> {
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn test_mock_stream() {
        let backend = MockBackend::with_delay(Duration::from_millis(1));

        let request = StreamRequest {
            prompt: "test".to_string(),
            model: None,
            max_tokens: None,
            temperature: None,
            system: None,
            user_id: None,
            request_id: "test-1".to_string(),
        };

        let mut stream = backend.stream(request).await.unwrap();

        let mut chunks = Vec::new();
        while let Some(result) = stream.next().await {
            let chunk = result.unwrap();
            chunks.push(chunk.clone());
            if chunk.done {
                break;
            }
        }

        assert!(!chunks.is_empty());
        assert!(chunks.last().unwrap().done);
    }
}
