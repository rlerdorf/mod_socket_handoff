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
    pub fn new(chunk_count: usize) -> Self {
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
