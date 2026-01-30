//! OpenAI streaming backend.
//!
//! Uses the OpenAI Chat Completions API with streaming.

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
    pub fn new(
        api_key: String,
        api_base: String,
        default_model: String,
        timeout: Duration,
        pool_max_idle_per_host: usize,
    ) -> Result<Self, BackendError> {
        let client = Client::builder()
            .timeout(timeout)
            .pool_max_idle_per_host(pool_max_idle_per_host)
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
