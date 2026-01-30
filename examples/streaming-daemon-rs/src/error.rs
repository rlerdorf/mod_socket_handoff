//! Error types for the streaming daemon.

use std::io;
use thiserror::Error;

/// Result type alias for daemon operations.
pub type Result<T> = std::result::Result<T, DaemonError>;

/// Main error type for the daemon.
#[derive(Error, Debug)]
pub enum DaemonError {
    #[error("Configuration error: {0}")]
    Config(String),

    #[error("IO error: {0}")]
    Io(#[from] io::Error),

    #[error("Socket error: {0}")]
    Socket(String),

    #[error("Handoff error: {0}")]
    Handoff(#[from] HandoffError),

    #[error("Backend error: {0}")]
    Backend(#[from] BackendError),

    #[error("Shutdown in progress")]
    Shutdown,

    #[error("Connection limit reached")]
    ConnectionLimit,

    #[error("Timeout: {0}")]
    Timeout(String),
}

/// Errors during fd handoff from Apache.
#[derive(Error, Debug)]
pub enum HandoffError {
    #[error("Failed to receive fd: {0}")]
    ReceiveFailed(String),

    #[error("No file descriptor received")]
    NoFileDescriptor,

    #[error("Invalid socket type: expected SOCK_STREAM")]
    InvalidSocketType,

    #[error("Control message truncated")]
    ControlMessageTruncated,

    #[error("Data truncated (exceeded buffer size)")]
    DataTruncated,

    #[error("Handoff timeout")]
    Timeout,

    #[error("System error: {0}")]
    System(#[from] nix::Error),
}

/// Errors from streaming backends (OpenAI, etc.).
#[derive(Error, Debug)]
pub enum BackendError {
    #[error("HTTP error: {0}")]
    Http(String),

    #[error("API error: {status} - {message}")]
    Api { status: u16, message: String },

    #[error("Stream error: {0}")]
    Stream(String),

    #[error("Parse error: {0}")]
    Parse(String),

    #[error("Configuration error: {0}")]
    Config(String),

    #[error("Timeout")]
    Timeout,

    #[error("Rate limited")]
    RateLimited,

    #[error("Connection failed: {0}")]
    Connection(String),
}

/// Errors during streaming to the client.
#[derive(Error, Debug)]
pub enum StreamError {
    #[error("Write error: {0}")]
    Write(#[from] io::Error),

    #[error("Client disconnected")]
    ClientDisconnected,

    #[error("Write timeout")]
    Timeout,

    #[error("Serialization error: {0}")]
    Serialization(String),
}
