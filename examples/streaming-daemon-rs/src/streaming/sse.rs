//! SSE (Server-Sent Events) formatting.

use bytes::Bytes;
use serde_json::json;

/// Format a content chunk as an SSE event.
pub fn format_sse_chunk(content: &str) -> Bytes {
    let data = json!({ "content": content });
    format!("data: {}\n\n", data).into()
}

/// Format the done marker as an SSE event.
pub fn format_sse_done() -> Bytes {
    "data: [DONE]\n\n".into()
}

/// Format an error as an SSE event.
pub fn format_sse_error(error: &str) -> Bytes {
    let data = json!({ "error": error });
    format!("data: {}\n\n", data).into()
}

/// HTTP headers for SSE response.
pub const SSE_HEADERS: &str = "\
HTTP/1.1 200 OK\r\n\
Content-Type: text/event-stream\r\n\
Cache-Control: no-cache\r\n\
Connection: close\r\n\
X-Accel-Buffering: no\r\n\
\r\n";

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_format_sse_chunk() {
        let chunk = format_sse_chunk("hello");
        assert_eq!(chunk.as_ref(), b"data: {\"content\":\"hello\"}\n\n");
    }

    #[test]
    fn test_format_sse_done() {
        let done = format_sse_done();
        assert_eq!(done.as_ref(), b"data: [DONE]\n\n");
    }

    #[test]
    fn test_format_sse_error() {
        let error = format_sse_error("test error");
        assert!(String::from_utf8_lossy(&error).contains("error"));
    }
}
