//! OpenAI streaming backend.
//!
//! Uses the OpenAI Chat Completions API with streaming.
//! Supports HTTP/2 multiplexing for high-concurrency scenarios.

use async_trait::async_trait;
use futures::StreamExt;
use reqwest::Client;
use reqwest_eventsource::{Event, EventSource, RequestBuilderExt};
use serde::{Deserialize, Serialize};

use super::traits::{
    ChunkMetadata, ChunkStreamTrait, StreamChunk, StreamRequest, StreamingBackend,
};
use crate::error::BackendError;

/// OpenAI streaming backend.
pub struct OpenAIBackend {
    client: Client,
    api_key: String,
    api_base: String,
    default_model: String,
}

impl OpenAIBackend {
    /// Create a new OpenAI backend with a pre-configured HTTP client.
    ///
    /// The client should be created using `build_streaming_client()` which
    /// configures proper timeouts and HTTP/2 settings for SSE streaming.
    pub fn new(client: Client, api_key: String, api_base: String, default_model: String) -> Self {
        Self {
            client,
            api_key,
            api_base,
            default_model,
        }
    }
}

#[async_trait]
impl StreamingBackend for OpenAIBackend {
    fn name(&self) -> &'static str {
        "openai"
    }

    async fn stream(&self, request: StreamRequest) -> Result<super::ChunkStream, BackendError> {
        let model = request.model.unwrap_or_else(|| self.default_model.clone());

        let messages = if !request.messages.is_empty() {
            // Use full conversation history from request
            request
                .messages
                .into_iter()
                .map(|m| ChatMessage {
                    role: m.role,
                    content: m.content,
                })
                .collect()
        } else {
            // Legacy: build messages from prompt and system
            let mut msgs = Vec::new();

            // Add system message if provided
            if let Some(system) = &request.system {
                msgs.push(ChatMessage {
                    role: "system".to_string(),
                    content: system.clone(),
                });
            }

            // Add user message
            msgs.push(ChatMessage {
                role: "user".to_string(),
                content: request.prompt,
            });

            msgs
        };

        let body = ChatRequest {
            model,
            messages,
            max_tokens: request.max_tokens,
            temperature: request.temperature,
            stream: true,
        };

        let url = format!("{}/chat/completions", self.api_base);

        let mut req = self
            .client
            .post(&url)
            .header("Authorization", format!("Bearer {}", self.api_key))
            .header("Content-Type", "application/json");

        // Add test pattern header if present (for validation testing)
        if let Some(test_pattern) = &request.test_pattern {
            req = req.header("X-Test-Pattern", test_pattern);
        }

        let req = req.json(&body);

        // Create EventSource from the request builder
        let es = req.eventsource().map_err(|e| {
            // Log the full error chain for debugging connection issues
            use std::error::Error;
            let mut chain = format!("{}", e);
            let mut source = e.source();
            while let Some(s) = source {
                chain.push_str(&format!(" -> {}", s));
                source = s.source();
            }
            tracing::error!(error = %chain, "EventSource creation failed");
            BackendError::Connection(chain)
        })?;

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

/// OpenAI streaming chunk stream using reqwest-eventsource.
struct OpenAIChunkStream {
    es: EventSource,
}

impl ChunkStreamTrait for OpenAIChunkStream {
    fn next(
        &mut self,
    ) -> std::pin::Pin<
        Box<
            dyn std::future::Future<Output = Option<Result<StreamChunk, BackendError>>> + Send + '_,
        >,
    > {
        Box::pin(async move {
            loop {
                match self.es.next().await {
                    Some(Ok(Event::Open)) => {
                        // Connection opened, continue to next event
                        continue;
                    }
                    Some(Ok(Event::Message(msg))) => {
                        // Check for [DONE] marker
                        if msg.data == "[DONE]" {
                            return Some(Ok(StreamChunk::done()));
                        }

                        // Skip empty data
                        if msg.data.is_empty() {
                            continue;
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
                                // Empty chunk, continue
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
                        // Log the full error chain for debugging
                        use std::error::Error;
                        let mut chain = format!("{}", e);
                        let mut source = e.source();
                        while let Some(s) = source {
                            chain.push_str(&format!(" -> {}", s));
                            source = s.source();
                        }
                        tracing::error!(error = %chain, "SSE stream error");
                        return Some(Err(BackendError::Stream(chain)));
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
