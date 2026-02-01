use std::convert::Infallible;
use std::sync::Arc;
use std::time::Duration;

use axum::body::Body;
use axum::extract::State;
use axum::http::{header, StatusCode};
use axum::response::{IntoResponse, Response};
use bytes::Bytes;
use futures::stream::{self, StreamExt};

use crate::responses::{health_response, models_response, ResponseChunks};

/// Shared application state
pub struct AppState {
    pub chunks: ResponseChunks,
    pub chunk_delay: Duration,
}

/// POST /v1/chat/completions
///
/// Streams pre-computed SSE chunks with configurable delays.
/// Request body is completely ignored for maximum performance.
pub async fn chat_completions(State(state): State<Arc<AppState>>) -> impl IntoResponse {
    let delay = state.chunk_delay;

    // Build the stream: content chunks + final chunk + done marker
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
        .unwrap()
}

/// GET /v1/models
///
/// Returns a minimal models list response.
pub async fn list_models() -> impl IntoResponse {
    Response::builder()
        .status(StatusCode::OK)
        .header(header::CONTENT_TYPE, "application/json")
        .body(Body::from(models_response()))
        .unwrap()
}

/// GET /health
///
/// Simple health check endpoint.
pub async fn health() -> impl IntoResponse {
    Response::builder()
        .status(StatusCode::OK)
        .header(header::CONTENT_TYPE, "application/json")
        .body(Body::from(health_response()))
        .unwrap()
}
