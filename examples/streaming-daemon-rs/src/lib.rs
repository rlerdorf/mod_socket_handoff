//! Streaming daemon for mod_socket_handoff
//!
//! This library provides the core functionality for receiving client connections
//! from Apache via SCM_RIGHTS and streaming responses (e.g., from LLM APIs).

pub mod backend;
pub mod config;
pub mod error;
pub mod metrics;
pub mod server;
pub mod shutdown;
pub mod streaming;

pub use config::Config;
pub use error::{DaemonError, Result};
