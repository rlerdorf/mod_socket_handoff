use std::convert::Infallible;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use std::time::Duration;

use axum::body::Body;
use axum::extract::State;
use axum::http::{header, HeaderMap, StatusCode};
use axum::response::{IntoResponse, Response};
use axum::Json;
use bytes::Bytes;
use futures::stream::{self, StreamExt};
use serde_json::json;

use crate::patterns::{TestPattern, TestResponseChunks};
use crate::responses::{health_response, models_response, ResponseChunks};

/// Global request counter for benchmark validation
static REQUEST_COUNT: AtomicU64 = AtomicU64::new(0);

/// Shared application state
pub struct AppState {
    pub chunks: ResponseChunks,
    pub chunk_delay: Duration,
}

/// POST /v1/chat/completions
///
/// Streams pre-computed SSE chunks with configurable delays.
/// Request body is completely ignored for maximum performance.
///
/// Supports `X-Test-Pattern` header for validation testing:
/// - `unicode` - Emoji, CJK, 4-byte UTF-8 content
/// - `short` - 1-2 byte chunks
/// - `long` - 4KB chunks
/// - `abort:N` - Abort after N chunks (no finish_reason or [DONE])
/// - `finish:reason` - Custom finish_reason (e.g., `finish:length`)
pub async fn chat_completions(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
) -> impl IntoResponse {
    REQUEST_COUNT.fetch_add(1, Ordering::Relaxed);
    let delay = state.chunk_delay;

    // Check for X-Test-Pattern header
    let pattern_header = headers.get("X-Test-Pattern").and_then(|v| v.to_str().ok());
    let pattern = TestPattern::parse(pattern_header);

    // If test pattern is specified, use test response chunks
    if pattern != TestPattern::Default {
        return test_pattern_response(pattern, delay);
    }

    // Default: use pre-computed benchmark chunks
    // Note: collect() is required here to create owned data that outlives the borrow of `state`
    let chunks: Vec<Bytes> = state.chunks.content_chunks().collect();
    let final_chunk = state.chunks.final_chunk();
    let done_chunk = state.chunks.done_chunk();

    // Add delays between chunks (first chunk sent immediately, then delays between subsequent)
    let delayed_stream = stream::iter(chunks)
        .enumerate()
        .then(move |(index, chunk)| async move {
            if index > 0 && delay > Duration::ZERO {
                tokio::time::sleep(delay).await;
            }
            Ok::<Bytes, Infallible>(chunk)
        })
        .chain(stream::once(async move {
            if delay > Duration::ZERO {
                tokio::time::sleep(delay).await;
            }
            Ok::<Bytes, Infallible>(final_chunk)
        }))
        .chain(stream::once(async move {
            if delay > Duration::ZERO {
                tokio::time::sleep(delay).await;
            }
            Ok::<Bytes, Infallible>(done_chunk)
        }));

    Response::builder()
        .status(StatusCode::OK)
        .header(header::CONTENT_TYPE, "text/event-stream")
        .header(header::CACHE_CONTROL, "no-cache")
        .header("X-Accel-Buffering", "no")
        .body(Body::from_stream(delayed_stream))
        .expect("failed to build SSE response")
}

/// Generate response for test pattern.
fn test_pattern_response(pattern: TestPattern, delay: Duration) -> Response<Body> {
    let test_chunks = TestResponseChunks::new(&pattern);
    let is_abort = test_chunks.is_abort();

    let chunks: Vec<Bytes> = test_chunks.content_chunks().collect();
    let final_chunk = test_chunks.final_chunk();
    let done_chunk = test_chunks.done_chunk();

    // Build the stream based on whether this is an abort pattern
    let content_stream = stream::iter(chunks)
        .enumerate()
        .then(move |(index, chunk)| async move {
            if index > 0 && delay > Duration::ZERO {
                tokio::time::sleep(delay).await;
            }
            Ok::<Bytes, Infallible>(chunk)
        });

    if is_abort {
        // Abort pattern: just content chunks, no final or done
        Response::builder()
            .status(StatusCode::OK)
            .header(header::CONTENT_TYPE, "text/event-stream")
            .header(header::CACHE_CONTROL, "no-cache")
            .header("X-Accel-Buffering", "no")
            .header("X-Test-Pattern-Applied", pattern_name(&pattern))
            .body(Body::from_stream(content_stream))
            .expect("failed to build SSE response")
    } else {
        // Normal pattern: content + final + done
        let delayed_stream = content_stream
            .chain(stream::once(async move {
                if delay > Duration::ZERO {
                    tokio::time::sleep(delay).await;
                }
                Ok::<Bytes, Infallible>(final_chunk.expect("non-abort should have final"))
            }))
            .chain(stream::once(async move {
                if delay > Duration::ZERO {
                    tokio::time::sleep(delay).await;
                }
                Ok::<Bytes, Infallible>(done_chunk.expect("non-abort should have done"))
            }));

        Response::builder()
            .status(StatusCode::OK)
            .header(header::CONTENT_TYPE, "text/event-stream")
            .header(header::CACHE_CONTROL, "no-cache")
            .header("X-Accel-Buffering", "no")
            .header("X-Test-Pattern-Applied", pattern_name(&pattern))
            .body(Body::from_stream(delayed_stream))
            .expect("failed to build SSE response")
    }
}

/// Get pattern name for response header.
fn pattern_name(pattern: &TestPattern) -> &'static str {
    match pattern {
        TestPattern::Default => "default",
        TestPattern::Unicode => "unicode",
        TestPattern::Short => "short",
        TestPattern::Long => "long",
        TestPattern::Abort(_) => "abort",
        TestPattern::Finish(_) => "finish",
    }
}

/// GET /v1/models
///
/// Returns a minimal models list response.
pub async fn list_models() -> impl IntoResponse {
    Response::builder()
        .status(StatusCode::OK)
        .header(header::CONTENT_TYPE, "application/json")
        .body(Body::from(models_response()))
        .expect("failed to build models response")
}

/// GET /health
///
/// Simple health check endpoint.
pub async fn health() -> impl IntoResponse {
    Response::builder()
        .status(StatusCode::OK)
        .header(header::CONTENT_TYPE, "application/json")
        .body(Body::from(health_response()))
        .expect("failed to build health response")
}

/// GET /stats
///
/// Returns request statistics for benchmark validation.
/// Allows comparing request counts between daemon implementations
/// to verify equivalent HTTP/2 multiplexing behavior.
pub async fn stats() -> impl IntoResponse {
    Json(json!({
        "total_requests": REQUEST_COUNT.load(Ordering::Relaxed)
    }))
}

/// POST /stats/reset
///
/// Resets request statistics. Useful between benchmark runs.
pub async fn stats_reset() -> impl IntoResponse {
    REQUEST_COUNT.store(0, Ordering::Relaxed);
    Json(json!({
        "reset": true,
        "total_requests": 0
    }))
}
