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

/// Format an HTTP error response with the given status code and reason.
/// The reason is sanitized to prevent HTTP response splitting.
pub fn format_error_response(status: u16, reason: &str) -> String {
    let status_text = match status {
        502 => "Bad Gateway",
        503 => "Service Unavailable",
        504 => "Gateway Timeout",
        _ => "Internal Server Error",
    };
    // Sanitize reason to prevent HTTP response splitting
    let safe_reason: String = reason.chars().filter(|c| *c != '\r' && *c != '\n').collect();
    let body = format!("{}: {}\n", status_text, safe_reason);
    format!(
        "HTTP/1.1 {} {}\r\nContent-Type: text/plain\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{}",
        status,
        status_text,
        body.len(),
        body
    )
}

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

    #[test]
    fn test_format_error_response_502() {
        let resp = format_error_response(502, "Backend unavailable");
        assert!(resp.starts_with("HTTP/1.1 502 Bad Gateway\r\n"));
        assert!(resp.contains("Content-Type: text/plain\r\n"));
        assert!(resp.contains("Connection: close\r\n"));
        assert!(resp.contains("Bad Gateway: Backend unavailable"));
        // Verify Content-Length matches actual body
        let body = "Bad Gateway: Backend unavailable\n";
        assert!(resp.contains(&format!("Content-Length: {}\r\n", body.len())));
    }

    #[test]
    fn test_format_error_response_504() {
        let resp = format_error_response(504, "Backend timeout");
        assert!(resp.starts_with("HTTP/1.1 504 Gateway Timeout\r\n"));
        assert!(resp.contains("Gateway Timeout: Backend timeout"));
    }

    #[test]
    fn test_format_error_response_unknown_status() {
        let resp = format_error_response(500, "Something broke");
        assert!(resp.starts_with("HTTP/1.1 500 Internal Server Error\r\n"));
    }
}
