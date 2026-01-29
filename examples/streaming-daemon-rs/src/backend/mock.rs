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
    pub fn new() -> Self {
        Self {
            token_delay: Duration::from_millis(50),
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
        // Generate response based on prompt
        let prompt_preview = if request.prompt.len() > 100 {
            format!("{}...", &request.prompt[..100])
        } else {
            request.prompt.clone()
        };

        let response = format!(
            "Hello! I received your prompt: \"{}\"\n\n\
             This response is being streamed from a Rust daemon that received \
             the client socket via SCM_RIGHTS. The Apache worker has been freed.\n\n\
             Replace this mock backend with your LLM API integration.",
            prompt_preview
        );

        // Split into word chunks
        let words: Vec<&str> = response.split_whitespace().collect();
        let mut chunks: Vec<StreamChunk> = Vec::with_capacity(words.len() + 1);

        for (i, word) in words.iter().enumerate() {
            let content = if i == 0 {
                word.to_string()
            } else {
                format!(" {}", word)
            };
            chunks.push(StreamChunk::content(content));
        }

        // Add done marker
        chunks.push(StreamChunk::done());

        Ok(Box::new(VecChunkStream::new(chunks, Some(self.token_delay))))
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
