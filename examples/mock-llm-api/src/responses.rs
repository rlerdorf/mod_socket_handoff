use bytes::Bytes;

/// Pre-computed SSE response chunks for zero-allocation streaming.
/// Each chunk is a complete SSE data line with trailing newlines.
pub struct ResponseChunks {
    chunks: Vec<Bytes>,
    final_chunk: Bytes,
    done_chunk: Bytes,
}

impl ResponseChunks {
    /// Generate pre-computed chunks at startup.
    /// Words are cycled if chunk_count exceeds word list length.
    ///
    /// # Panics
    ///
    /// Panics if `chunk_count` is zero. Use a value of at least 1 to
    /// produce a valid streaming response.
    ///
    /// # Note
    ///
    /// The JSON escaping only handles backslashes and quotes. The built-in
    /// word list contains only simple ASCII text. If extending with custom
    /// content containing control characters (newlines, tabs), use proper
    /// JSON serialization.
    pub fn new(chunk_count: usize) -> Self {
        assert!(
            chunk_count >= 1,
            "ResponseChunks::new requires chunk_count >= 1 to produce a valid streaming response"
        );

        const WORDS: &[&str] = &[
            "Hello",
            " world",
            "!",
            " This",
            " is",
            " a",
            " mock",
            " LLM",
            " response",
            " for",
            " benchmarking",
            " streaming",
            " performance",
            ".",
            " The",
            " actual",
            " content",
            " doesn't",
            " matter",
            " -",
            " only",
            " the",
            " timing",
            " and",
            " format",
            ".",
        ];

        let chunks: Vec<Bytes> = (0..chunk_count)
            .map(|i| {
                let word = WORDS[i % WORDS.len()];
                // Escape the word for JSON (handle quotes and backslashes)
                let escaped = word.replace('\\', "\\\\").replace('"', "\\\"");
                let json = format!(
                    r#"data: {{"id":"chatcmpl-mock","object":"chat.completion.chunk","created":0,"model":"mock-model","choices":[{{"index":0,"delta":{{"content":"{}"}},"finish_reason":null}}]}}"#,
                    escaped
                );
                Bytes::from(format!("{}\n\n", json))
            })
            .collect();

        // Final chunk with finish_reason: "stop"
        let final_chunk = Bytes::from(
            r#"data: {"id":"chatcmpl-mock","object":"chat.completion.chunk","created":0,"model":"mock-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

"#,
        );

        // [DONE] marker
        let done_chunk = Bytes::from("data: [DONE]\n\n");

        Self {
            chunks,
            final_chunk,
            done_chunk,
        }
    }

    /// Iterator over content chunks (not including final/done)
    pub fn content_chunks(&self) -> impl Iterator<Item = Bytes> + '_ {
        self.chunks.iter().cloned()
    }

    /// The finish_reason: "stop" chunk
    pub fn final_chunk(&self) -> Bytes {
        self.final_chunk.clone()
    }

    /// The [DONE] marker
    pub fn done_chunk(&self) -> Bytes {
        self.done_chunk.clone()
    }

    /// Total number of content chunks
    pub fn len(&self) -> usize {
        self.chunks.len()
    }

    /// Whether there are no content chunks
    #[allow(dead_code)] // Required by Rust convention when implementing len()
    pub fn is_empty(&self) -> bool {
        self.chunks.is_empty()
    }
}

/// Pre-computed JSON response for /v1/models endpoint
pub fn models_response() -> Bytes {
    Bytes::from_static(
        br#"{"object":"list","data":[{"id":"mock-model","object":"model","created":0,"owned_by":"mock-llm-api"}]}"#,
    )
}

/// Pre-computed response for /health endpoint
pub fn health_response() -> Bytes {
    Bytes::from_static(br#"{"status":"ok"}"#)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_response_chunks_new() {
        let chunks = ResponseChunks::new(5);
        assert_eq!(chunks.len(), 5);
        assert!(!chunks.is_empty());
    }

    #[test]
    fn test_response_chunks_content_format() {
        let chunks = ResponseChunks::new(1);
        let content: Vec<Bytes> = chunks.content_chunks().collect();
        assert_eq!(content.len(), 1);

        let chunk_str = String::from_utf8_lossy(&content[0]);
        // Verify SSE format
        assert!(chunk_str.starts_with("data: "));
        assert!(chunk_str.ends_with("\n\n"));
        // Verify JSON structure
        assert!(chunk_str.contains("\"id\":\"chatcmpl-mock\""));
        assert!(chunk_str.contains("\"object\":\"chat.completion.chunk\""));
        assert!(chunk_str.contains("\"delta\":{\"content\":"));
        assert!(chunk_str.contains("\"finish_reason\":null"));
    }

    #[test]
    fn test_response_chunks_final_chunk_format() {
        let chunks = ResponseChunks::new(1);
        let final_chunk = chunks.final_chunk();
        let chunk_str = String::from_utf8_lossy(&final_chunk);

        assert!(chunk_str.starts_with("data: "));
        assert!(chunk_str.contains("\"finish_reason\":\"stop\""));
        assert!(chunk_str.contains("\"delta\":{}"));
    }

    #[test]
    fn test_response_chunks_done_marker() {
        let chunks = ResponseChunks::new(1);
        let done = chunks.done_chunk();
        assert_eq!(&done[..], b"data: [DONE]\n\n");
    }

    #[test]
    fn test_response_chunks_cycling() {
        // Test that words cycle when chunk_count exceeds word list
        let chunks = ResponseChunks::new(100);
        assert_eq!(chunks.len(), 100);

        // All chunks should be valid SSE format
        for chunk in chunks.content_chunks() {
            let chunk_str = String::from_utf8_lossy(&chunk);
            assert!(chunk_str.starts_with("data: "));
            assert!(chunk_str.ends_with("\n\n"));
        }
    }

    #[test]
    #[should_panic(expected = "chunk_count >= 1")]
    fn test_response_chunks_zero_panics() {
        ResponseChunks::new(0);
    }

    #[test]
    fn test_models_response_valid_json() {
        let response = models_response();
        let json_str = String::from_utf8_lossy(&response);
        assert!(json_str.contains("\"object\":\"list\""));
        assert!(json_str.contains("\"id\":\"mock-model\""));
    }

    #[test]
    fn test_health_response_valid_json() {
        let response = health_response();
        assert_eq!(&response[..], br#"{"status":"ok"}"#);
    }
}
