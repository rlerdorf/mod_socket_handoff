//! Test pattern handling for validation testing.
//!
//! Supports the X-Test-Pattern header to select test-specific response content.
//! Available patterns:
//! - `unicode` - Emoji, CJK, 4-byte UTF-8
//! - `short` - 1-2 byte chunks
//! - `long` - 4KB chunks
//! - `abort:N` - Abort after N chunks
//! - `finish:reason` - Custom finish_reason

use bytes::Bytes;

use crate::test_data::{long_chunk_content, unicode_chunks, SHORT_CHUNKS};

/// Parsed test pattern from X-Test-Pattern header.
#[derive(Debug, Clone, PartialEq)]
pub enum TestPattern {
    /// Default benchmark response (no test pattern)
    Default,
    /// Unicode content: emoji, CJK, 4-byte UTF-8
    Unicode,
    /// Short chunks (1-2 bytes each)
    Short,
    /// Long chunks (~4KB each)
    Long,
    /// Abort after N chunks (for testing partial response handling)
    Abort(usize),
    /// Custom finish_reason
    Finish(String),
}

impl TestPattern {
    /// Parse X-Test-Pattern header value into a TestPattern.
    pub fn parse(header: Option<&str>) -> Self {
        let header = match header {
            Some(h) => h.trim(),
            None => return TestPattern::Default,
        };

        if header.is_empty() {
            return TestPattern::Default;
        }

        match header {
            "unicode" => TestPattern::Unicode,
            "short" => TestPattern::Short,
            "long" => TestPattern::Long,
            s if s.starts_with("abort:") => {
                let n = s[6..].parse().unwrap_or(5);
                TestPattern::Abort(n)
            }
            s if s.starts_with("finish:") => {
                let reason = s[7..].to_string();
                TestPattern::Finish(if reason.is_empty() {
                    "stop".to_string()
                } else {
                    reason
                })
            }
            _ => TestPattern::Default,
        }
    }
}

/// Pre-computed test response chunks.
pub struct TestResponseChunks {
    /// Content chunks (SSE data lines)
    chunks: Vec<Bytes>,
    /// Final chunk with finish_reason
    final_chunk: Bytes,
    /// [DONE] marker
    done_chunk: Bytes,
    /// Optional abort after N chunks
    abort_after: Option<usize>,
}

impl TestResponseChunks {
    /// Generate response chunks for the given test pattern.
    pub fn new(pattern: &TestPattern) -> Self {
        match pattern {
            TestPattern::Default => Self::default_chunks(18),
            TestPattern::Unicode => Self::unicode_chunks(),
            TestPattern::Short => Self::short_chunks(),
            TestPattern::Long => Self::long_chunks(3), // 3 x 4KB chunks
            TestPattern::Abort(n) => {
                let mut chunks = Self::default_chunks((*n).max(1) + 5);
                chunks.abort_after = Some(*n);
                chunks
            }
            TestPattern::Finish(reason) => Self::finish_reason_chunks(reason),
        }
    }

    /// Default benchmark-style response.
    fn default_chunks(count: usize) -> Self {
        const WORDS: &[&str] = &[
            "Hello",
            " world",
            "!",
            " This",
            " is",
            " a",
            " test",
            " response",
            " for",
            " validation",
            ".",
        ];

        let chunks: Vec<Bytes> = (0..count)
            .map(|i| {
                let word = WORDS[i % WORDS.len()];
                Self::make_chunk(word)
            })
            .collect();

        Self {
            chunks,
            final_chunk: Self::make_final_chunk("stop"),
            done_chunk: Bytes::from("data: [DONE]\n\n"),
            abort_after: None,
        }
    }

    /// Unicode content chunks.
    fn unicode_chunks() -> Self {
        let chunks: Vec<Bytes> = unicode_chunks().map(Self::make_chunk).collect();

        Self {
            chunks,
            final_chunk: Self::make_final_chunk("stop"),
            done_chunk: Bytes::from("data: [DONE]\n\n"),
            abort_after: None,
        }
    }

    /// Short (1-2 byte) chunks.
    fn short_chunks() -> Self {
        let chunks: Vec<Bytes> = SHORT_CHUNKS.iter().map(|s| Self::make_chunk(s)).collect();

        Self {
            chunks,
            final_chunk: Self::make_final_chunk("stop"),
            done_chunk: Bytes::from("data: [DONE]\n\n"),
            abort_after: None,
        }
    }

    /// Long (~4KB) chunks.
    fn long_chunks(count: usize) -> Self {
        let content = long_chunk_content();
        let chunks: Vec<Bytes> = (0..count).map(|_| Self::make_chunk(&content)).collect();

        Self {
            chunks,
            final_chunk: Self::make_final_chunk("stop"),
            done_chunk: Bytes::from("data: [DONE]\n\n"),
            abort_after: None,
        }
    }

    /// Custom finish_reason.
    fn finish_reason_chunks(reason: &str) -> Self {
        let chunks: Vec<Bytes> = [
            "Test",
            " response",
            " with",
            " custom",
            " finish_reason",
            ".",
        ]
        .iter()
        .map(|s| Self::make_chunk(s))
        .collect();

        Self {
            chunks,
            final_chunk: Self::make_final_chunk(reason),
            done_chunk: Bytes::from("data: [DONE]\n\n"),
            abort_after: None,
        }
    }

    /// Create an SSE chunk with JSON-escaped content.
    fn make_chunk(content: &str) -> Bytes {
        // JSON escape the content
        let escaped = json_escape(content);
        let json = format!(
            r#"data: {{"id":"chatcmpl-test","object":"chat.completion.chunk","created":0,"model":"test-model","choices":[{{"index":0,"delta":{{"content":"{}"}},"finish_reason":null}}]}}"#,
            escaped
        );
        Bytes::from(format!("{}\n\n", json))
    }

    /// Create the final chunk with finish_reason.
    fn make_final_chunk(reason: &str) -> Bytes {
        let escaped = json_escape(reason);
        let json = format!(
            r#"data: {{"id":"chatcmpl-test","object":"chat.completion.chunk","created":0,"model":"test-model","choices":[{{"index":0,"delta":{{}},"finish_reason":"{}"}}]}}"#,
            escaped
        );
        Bytes::from(format!("{}\n\n", json))
    }

    /// Iterator over content chunks (respects abort_after).
    pub fn content_chunks(&self) -> impl Iterator<Item = Bytes> + '_ {
        let limit = self.abort_after.unwrap_or(self.chunks.len());
        self.chunks.iter().take(limit).cloned()
    }

    /// The finish_reason chunk (None if aborted).
    pub fn final_chunk(&self) -> Option<Bytes> {
        if self.abort_after.is_some() {
            None
        } else {
            Some(self.final_chunk.clone())
        }
    }

    /// The [DONE] marker (None if aborted).
    pub fn done_chunk(&self) -> Option<Bytes> {
        if self.abort_after.is_some() {
            None
        } else {
            Some(self.done_chunk.clone())
        }
    }

    /// Total number of content chunks (before abort).
    #[allow(dead_code)]
    pub fn len(&self) -> usize {
        self.abort_after.unwrap_or(self.chunks.len())
    }

    /// Whether there are no content chunks.
    #[allow(dead_code)]
    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }

    /// Whether this is an abort pattern.
    pub fn is_abort(&self) -> bool {
        self.abort_after.is_some()
    }
}

/// JSON escape a string (handles quotes, backslashes, and control characters).
fn json_escape(s: &str) -> String {
    let mut result = String::with_capacity(s.len());
    for c in s.chars() {
        match c {
            '"' => result.push_str("\\\""),
            '\\' => result.push_str("\\\\"),
            '\n' => result.push_str("\\n"),
            '\r' => result.push_str("\\r"),
            '\t' => result.push_str("\\t"),
            c if c.is_control() => {
                // Unicode escape for other control characters
                result.push_str(&format!("\\u{:04x}", c as u32));
            }
            c => result.push(c),
        }
    }
    result
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_parse_default() {
        assert_eq!(TestPattern::parse(None), TestPattern::Default);
        assert_eq!(TestPattern::parse(Some("")), TestPattern::Default);
        assert_eq!(TestPattern::parse(Some("unknown")), TestPattern::Default);
    }

    #[test]
    fn test_parse_unicode() {
        assert_eq!(TestPattern::parse(Some("unicode")), TestPattern::Unicode);
    }

    #[test]
    fn test_parse_short() {
        assert_eq!(TestPattern::parse(Some("short")), TestPattern::Short);
    }

    #[test]
    fn test_parse_long() {
        assert_eq!(TestPattern::parse(Some("long")), TestPattern::Long);
    }

    #[test]
    fn test_parse_abort() {
        assert_eq!(TestPattern::parse(Some("abort:5")), TestPattern::Abort(5));
        assert_eq!(TestPattern::parse(Some("abort:0")), TestPattern::Abort(0));
        // Invalid number defaults to 5
        assert_eq!(TestPattern::parse(Some("abort:xyz")), TestPattern::Abort(5));
    }

    #[test]
    fn test_parse_finish() {
        assert_eq!(
            TestPattern::parse(Some("finish:length")),
            TestPattern::Finish("length".to_string())
        );
        assert_eq!(
            TestPattern::parse(Some("finish:stop")),
            TestPattern::Finish("stop".to_string())
        );
        // Empty reason defaults to "stop"
        assert_eq!(
            TestPattern::parse(Some("finish:")),
            TestPattern::Finish("stop".to_string())
        );
    }

    #[test]
    fn test_json_escape() {
        assert_eq!(json_escape("hello"), "hello");
        assert_eq!(json_escape("hello\"world"), "hello\\\"world");
        assert_eq!(json_escape("back\\slash"), "back\\\\slash");
        assert_eq!(json_escape("new\nline"), "new\\nline");
    }

    #[test]
    fn test_unicode_chunks_valid() {
        let chunks = TestResponseChunks::new(&TestPattern::Unicode);
        let content: Vec<Bytes> = chunks.content_chunks().collect();
        assert!(!content.is_empty());

        // Verify SSE format
        for chunk in &content {
            let s = String::from_utf8_lossy(chunk);
            assert!(s.starts_with("data: "));
            assert!(s.ends_with("\n\n"));
        }
    }

    #[test]
    fn test_abort_pattern() {
        let chunks = TestResponseChunks::new(&TestPattern::Abort(3));
        assert!(chunks.is_abort());
        assert!(chunks.final_chunk().is_none());
        assert!(chunks.done_chunk().is_none());

        let content: Vec<Bytes> = chunks.content_chunks().collect();
        assert_eq!(content.len(), 3);
    }

    #[test]
    fn test_finish_reason_pattern() {
        let chunks = TestResponseChunks::new(&TestPattern::Finish("length".to_string()));
        let final_chunk = chunks.final_chunk().expect("should have final chunk");
        let s = String::from_utf8_lossy(&final_chunk);
        assert!(s.contains("\"finish_reason\":\"length\""));
    }
}
