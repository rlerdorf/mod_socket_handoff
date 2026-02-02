//! OpenAI streaming backend.
//!
//! Uses the OpenAI Chat Completions API with streaming.
//! Supports HTTP/2 multiplexing for high-concurrency scenarios.

use async_trait::async_trait;
use futures::StreamExt;
use reqwest::Client;
use reqwest_eventsource::{Event, EventSource};
use serde::{Deserialize, Serialize};
use std::pin::Pin;
use std::time::Duration;

use super::traits::{
    ChunkMetadata, ChunkStreamTrait, StreamChunk, StreamRequest, StreamingBackend,
};
use crate::config::Http2Config;
use crate::error::BackendError;

/// OpenAI streaming backend.
pub struct OpenAIBackend {
    client: Client,
    api_key: String,
    api_base: String,
    default_model: String,
}

impl OpenAIBackend {
    /// Create a new OpenAI backend.
    ///
    /// When HTTP/2 is enabled (default), connections use HTTP/2 multiplexing over TCP,
    /// allowing ~100 concurrent streams per connection. This dramatically reduces
    /// connection overhead for high-concurrency scenarios.
    ///
    /// When HTTP/2 is disabled, falls back to HTTP/1.1 mode where `api_socket` can be
    /// used to route connections through a Unix socket for connection pooling.
    pub fn new(
        api_key: String,
        api_base: String,
        default_model: String,
        timeout: Duration,
        pool_max_idle_per_host: usize,
        api_socket: Option<String>,
        http2_config: &Http2Config,
    ) -> Result<Self, BackendError> {
        let mut builder = Client::builder()
            .timeout(timeout)
            .pool_max_idle_per_host(pool_max_idle_per_host);

        // Determine if this is an HTTP (not HTTPS) endpoint
        let is_plaintext = api_base.starts_with("http://");

        // Configure HTTP version
        if http2_config.enabled {
            if is_plaintext {
                // For http:// endpoints, use prior knowledge to skip upgrade negotiation.
                // This enables h2c (HTTP/2 over cleartext) directly.
                builder = builder.http2_prior_knowledge();
                tracing::info!("HTTP/2 with prior knowledge (h2c) enabled for plaintext endpoint");
            } else {
                // For https:// endpoints, use ALPN negotiation during TLS handshake.
                // Cannot use prior_knowledge with TLS - let TLS negotiate h2.
                tracing::info!("HTTP/2 via ALPN negotiation enabled for TLS endpoint");
            }

            // Flow control tuning for high concurrency
            builder = builder
                .http2_initial_stream_window_size(http2_config.initial_stream_window_kb * 1024)
                .http2_initial_connection_window_size(
                    http2_config.initial_connection_window_kb * 1024,
                );

            if http2_config.adaptive_window {
                builder = builder.http2_adaptive_window(true);
            }

            // Keep-alive for connection health
            if http2_config.keep_alive_interval_secs > 0 {
                builder = builder
                    .http2_keep_alive_interval(Duration::from_secs(
                        http2_config.keep_alive_interval_secs,
                    ))
                    .http2_keep_alive_timeout(Duration::from_secs(
                        http2_config.keep_alive_timeout_secs,
                    ))
                    .http2_keep_alive_while_idle(true);
            }

            tracing::info!(
                stream_window_kb = http2_config.initial_stream_window_kb,
                conn_window_kb = http2_config.initial_connection_window_kb,
                adaptive = http2_config.adaptive_window,
                keep_alive_secs = http2_config.keep_alive_interval_secs,
                "HTTP/2 flow control configured"
            );
        } else {
            // HTTP/1.1 fallback mode
            builder = builder.http1_only();
            tracing::info!("HTTP/1.1 mode (HTTP/2 disabled)");

            // Unix socket only makes sense for HTTP/1.1 connection pooling.
            // With HTTP/2, multiplexing happens at the protocol layer over TCP,
            // so we use TCP connections instead.
            #[cfg(unix)]
            if let Some(socket_path) = api_socket {
                tracing::info!(socket = %socket_path, "Using Unix socket for API connections");
                builder = builder.unix_socket(socket_path);
            }
        }

        let client = builder
            .build()
            .map_err(|e| BackendError::Connection(e.to_string()))?;

        Ok(Self {
            client,
            api_key,
            api_base,
            default_model,
        })
    }
}

#[async_trait]
impl StreamingBackend for OpenAIBackend {
    fn name(&self) -> &'static str {
        "openai"
    }

    async fn stream(&self, request: StreamRequest) -> Result<super::ChunkStream, BackendError> {
        let model = request.model.unwrap_or_else(|| self.default_model.clone());

        let mut messages = Vec::new();

        // Add system message if provided
        if let Some(system) = &request.system {
            messages.push(ChatMessage {
                role: "system".to_string(),
                content: system.clone(),
            });
        }

        // Add user message
        messages.push(ChatMessage {
            role: "user".to_string(),
            content: request.prompt,
        });

        let body = ChatRequest {
            model,
            messages,
            max_tokens: request.max_tokens,
            temperature: request.temperature,
            stream: true,
        };

        let url = format!("{}/chat/completions", self.api_base);

        let req = self
            .client
            .post(&url)
            .header("Authorization", format!("Bearer {}", self.api_key))
            .header("Content-Type", "application/json")
            .json(&body);

        let es = EventSource::new(req).map_err(|e| BackendError::Connection(e.to_string()))?;

        Ok(Box::new(OpenAIChunkStream { es }))
    }

    async fn health_check(&self) -> Result<(), BackendError> {
        let url = format!("{}/models", self.api_base);

        let response = self
            .client
            .get(&url)
            .header("Authorization", format!("Bearer {}", self.api_key))
            .send()
            .await
            .map_err(|e| BackendError::Connection(e.to_string()))?;

        if !response.status().is_success() {
            return Err(BackendError::Api {
                status: response.status().as_u16(),
                message: "Health check failed".to_string(),
            });
        }

        Ok(())
    }
}

/// OpenAI streaming chunk stream.
struct OpenAIChunkStream {
    es: EventSource,
}

impl ChunkStreamTrait for OpenAIChunkStream {
    fn next(
        &mut self,
    ) -> Pin<
        Box<
            dyn std::future::Future<Output = Option<Result<StreamChunk, BackendError>>> + Send + '_,
        >,
    > {
        Box::pin(async move {
            loop {
                match self.es.next().await {
                    Some(Ok(Event::Open)) => continue,
                    Some(Ok(Event::Message(msg))) => {
                        // Check for done marker
                        if msg.data == "[DONE]" {
                            return Some(Ok(StreamChunk::done()));
                        }

                        // Parse the chunk
                        match serde_json::from_str::<ChatChunk>(&msg.data) {
                            Ok(chunk) => {
                                if let Some(choice) = chunk.choices.first() {
                                    if let Some(content) = &choice.delta.content {
                                        if !content.is_empty() {
                                            let mut stream_chunk = StreamChunk::content(content);

                                            if let Some(reason) = &choice.finish_reason {
                                                stream_chunk.metadata = Some(ChunkMetadata {
                                                    tokens: None,
                                                    finish_reason: Some(reason.clone()),
                                                });
                                                stream_chunk.done = true;
                                            }

                                            return Some(Ok(stream_chunk));
                                        }
                                    }

                                    // Check for finish reason without content
                                    if choice.finish_reason.is_some() {
                                        return Some(Ok(StreamChunk::done()));
                                    }
                                }
                            }
                            Err(e) => {
                                return Some(Err(BackendError::Parse(format!(
                                    "Failed to parse chunk: {}",
                                    e
                                ))));
                            }
                        }
                    }
                    Some(Err(e)) => {
                        return Some(Err(BackendError::Stream(e.to_string())));
                    }
                    None => return None,
                }
            }
        })
    }
}

#[derive(Debug, Serialize)]
struct ChatRequest {
    model: String,
    messages: Vec<ChatMessage>,
    #[serde(skip_serializing_if = "Option::is_none")]
    max_tokens: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    temperature: Option<f32>,
    stream: bool,
}

#[derive(Debug, Serialize)]
struct ChatMessage {
    role: String,
    content: String,
}

#[derive(Debug, Deserialize)]
struct ChatChunk {
    choices: Vec<ChunkChoice>,
}

#[derive(Debug, Deserialize)]
struct ChunkChoice {
    delta: ChunkDelta,
    finish_reason: Option<String>,
}

#[derive(Debug, Deserialize)]
struct ChunkDelta {
    content: Option<String>,
}
