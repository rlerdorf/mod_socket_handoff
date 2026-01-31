//! Backend providers for streaming responses.

mod mock;
mod openai;
mod traits;

pub use mock::MockBackend;
pub use openai::OpenAIBackend;
pub use traits::{ChunkStream, ChunkStreamTrait, StreamChunk, StreamRequest, StreamingBackend};

use std::sync::Arc;

use crate::config::BackendConfig;
use crate::error::BackendError;

/// Create a backend from configuration.
pub fn create_backend(config: &BackendConfig) -> Result<Arc<dyn StreamingBackend>, BackendError> {
    match config.provider.as_str() {
        "mock" => {
            // MockBackend::new() reads DAEMON_TOKEN_DELAY_MS env var
            Ok(Arc::new(MockBackend::new()))
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
