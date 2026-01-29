//! SSE streaming and response writing.

mod sse;
mod writer;

pub use sse::{format_sse_chunk, format_sse_done, format_sse_error};
pub use writer::SseWriter;

// Re-export for convenience
pub use crate::backend::StreamRequest;
