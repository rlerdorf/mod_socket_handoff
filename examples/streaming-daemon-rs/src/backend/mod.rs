//! Backend providers for streaming responses.

mod mock;
mod openai;
mod traits;

pub use mock::MockBackend;
pub use openai::OpenAIBackend;
pub use traits::{ChunkStream, ChunkStreamTrait, StreamChunk, StreamRequest, StreamingBackend};

use std::sync::Arc;
use std::time::Duration;

use crate::config::BackendConfig;
use crate::error::BackendError;

/// Create a backend from configuration.
pub fn create_backend(config: &BackendConfig) -> Result<Arc<dyn StreamingBackend>, BackendError> {
    match config.provider.as_str() {
        "mock" => {
            // Check for custom token delay via environment variable
            // DAEMON_TOKEN_DELAY_MS (default: 50ms, use 3333 for 30s streams with 9 messages)
            let delay_ms = std::env::var("DAEMON_TOKEN_DELAY_MS")
                .ok()
                .and_then(|v| v.parse::<u64>().ok())
                .unwrap_or(50);
            Ok(Arc::new(MockBackend::with_delay(Duration::from_millis(delay_ms))))
        }
        "openai" => {
            let api_key = config
                .openai
                .api_key
                .clone()
                .or_else(|| std::env::var("OPENAI_API_KEY").ok())
                .ok_or_else(|| {
                    BackendError::Config(
                        "OpenAI API key not configured. Set OPENAI_API_KEY or config.backend.openai.api_key".to_string()
                    )
                })?;

            Ok(Arc::new(OpenAIBackend::new(
                api_key,
                config.openai.api_base.clone(),
                config.default_model.clone(),
                config.timeout(),
                config.openai.pool_max_idle_per_host,
            )?))
        }
        other => Err(BackendError::Config(format!(
            "Unknown backend provider: {}. Available: mock, openai",
            other
        ))),
    }
}
