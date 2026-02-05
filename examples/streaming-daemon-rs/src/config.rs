//! Configuration loading from TOML files and environment variables.

use serde::Deserialize;
use std::path::Path;
use std::time::Duration;

use crate::error::DaemonError;

/// Main configuration structure.
#[derive(Debug, Clone, Default, Deserialize)]
#[serde(default)]
pub struct Config {
    pub server: ServerConfig,
    pub backend: BackendConfig,
    pub metrics: MetricsConfig,
    pub logging: LoggingConfig,
}

/// Server configuration.
#[derive(Debug, Clone, Deserialize)]
#[serde(default)]
pub struct ServerConfig {
    /// Unix socket path for receiving handoffs from Apache.
    pub socket_path: String,

    /// Socket file permissions (octal).
    #[serde(deserialize_with = "deserialize_mode")]
    pub socket_mode: u32,

    /// Maximum concurrent connections.
    pub max_connections: usize,

    /// Timeout for receiving handoff from Apache (seconds).
    pub handoff_timeout_secs: u64,

    /// Timeout for individual writes to client (seconds).
    pub write_timeout_secs: u64,

    /// Graceful shutdown timeout (seconds).
    pub shutdown_timeout_secs: u64,

    /// Buffer size for receiving handoff data.
    pub handoff_buffer_size: usize,

    /// Timeout for backend stream creation (seconds).
    /// Prevents indefinite blocking when the upstream API hangs.
    pub backend_timeout_secs: u64,
}

impl Default for ServerConfig {
    fn default() -> Self {
        Self {
            socket_path: "/var/run/streaming-daemon-rs.sock".to_string(),
            socket_mode: 0o660,
            max_connections: 100_000,
            handoff_timeout_secs: 5,
            write_timeout_secs: 30,
            shutdown_timeout_secs: 120,
            handoff_buffer_size: 65536,
            backend_timeout_secs: 30,
        }
    }
}

impl ServerConfig {
    pub fn handoff_timeout(&self) -> Duration {
        Duration::from_secs(self.handoff_timeout_secs)
    }

    pub fn write_timeout(&self) -> Duration {
        Duration::from_secs(self.write_timeout_secs)
    }

    pub fn shutdown_timeout(&self) -> Duration {
        Duration::from_secs(self.shutdown_timeout_secs)
    }

    pub fn backend_timeout(&self) -> Duration {
        Duration::from_secs(self.backend_timeout_secs)
    }
}

/// Backend configuration for LLM providers.
#[derive(Debug, Clone, Deserialize)]
#[serde(default)]
pub struct BackendConfig {
    /// Backend provider: "mock" or "openai".
    pub provider: String,

    /// Default model to use.
    pub default_model: String,

    /// API timeout (seconds).
    pub timeout_secs: u64,

    /// OpenAI-specific settings.
    pub openai: OpenAIConfig,
}

impl Default for BackendConfig {
    fn default() -> Self {
        Self {
            provider: "mock".to_string(),
            default_model: "gpt-4o".to_string(),
            timeout_secs: 120,
            openai: OpenAIConfig::default(),
        }
    }
}

impl BackendConfig {
    pub fn timeout(&self) -> Duration {
        Duration::from_secs(self.timeout_secs)
    }
}

/// OpenAI-specific configuration.
#[derive(Debug, Clone, Deserialize)]
#[serde(default)]
pub struct OpenAIConfig {
    /// API key (can also be set via OPENAI_API_KEY env var).
    pub api_key: Option<String>,

    /// API base URL.
    pub api_base: String,

    /// Unix socket path for API connections (eliminates ephemeral port limits).
    /// When set, all HTTP connections to the API use this Unix socket.
    /// The api_base URL is still used for the HTTP path, but the connection
    /// goes through the Unix socket.
    /// Note: Only used when HTTP/2 is disabled (HTTP/1.1 mode).
    /// With HTTP/2, multiplexing happens at the protocol layer over TCP.
    pub api_socket: Option<String>,

    /// Maximum idle connections per host in pool.
    pub pool_max_idle_per_host: usize,

    /// Skip TLS certificate verification (for testing with self-signed certs).
    /// WARNING: Only use in testing environments with trusted self-signed certificates.
    /// Can be set via OPENAI_INSECURE_SSL=true environment variable.
    pub insecure_ssl: bool,

    /// HTTP/2 configuration for upstream API connections.
    pub http2: Http2Config,
}

impl Default for OpenAIConfig {
    fn default() -> Self {
        Self {
            api_key: None,
            api_base: "https://api.openai.com/v1".to_string(),
            api_socket: None,
            pool_max_idle_per_host: 100,
            insecure_ssl: false,
            http2: Http2Config::default(),
        }
    }
}

/// HTTP/2 configuration for upstream API connections.
///
/// HTTP/2 multiplexing allows ~100 concurrent streams to share a single TCP
/// connection, dramatically reducing connection overhead for high-concurrency
/// scenarios (100k streams can share ~1000 connections instead of 100k).
#[derive(Debug, Clone, Deserialize)]
#[serde(default)]
pub struct Http2Config {
    /// Enable HTTP/2 for upstream connections (default: true).
    /// Set to false to force HTTP/1.1 for compatibility or debugging.
    /// Can be overridden with OPENAI_HTTP2_ENABLED=false environment variable.
    pub enabled: bool,

    /// Initial stream-level flow control window size in KB (default: 64).
    /// Larger values allow more buffering per stream before flow control kicks in.
    pub initial_stream_window_kb: u32,

    /// Initial connection-level flow control window size in KB (default: 1024).
    /// Should be >= stream_window * expected_concurrent_streams.
    /// For 100 concurrent streams with 64KB windows, 1024KB is the minimum.
    pub initial_connection_window_kb: u32,

    /// Enable adaptive flow control (default: true).
    /// Allows dynamic adjustment of window sizes based on actual throughput,
    /// which helps with bursty LLM streaming patterns.
    pub adaptive_window: bool,

    /// Keep-alive ping interval in seconds (default: 20, 0 = disabled).
    /// Sends HTTP/2 PING frames to keep connections alive and detect stale ones.
    pub keep_alive_interval_secs: u64,

    /// Keep-alive timeout in seconds (default: 40).
    /// How long to wait for a PING response before considering the connection dead.
    pub keep_alive_timeout_secs: u64,
}

impl Default for Http2Config {
    fn default() -> Self {
        Self {
            enabled: true, // HTTP/2 is default
            // Large window sizes for high-concurrency SSE streaming
            // 1MB per stream, 10MB per connection
            initial_stream_window_kb: 1024,
            initial_connection_window_kb: 10240,
            adaptive_window: true,
            keep_alive_interval_secs: 10,
            keep_alive_timeout_secs: 20,
        }
    }
}

/// Metrics/Prometheus configuration.
#[derive(Debug, Clone, Deserialize)]
#[serde(default)]
pub struct MetricsConfig {
    /// Enable Prometheus metrics endpoint.
    pub enabled: bool,

    /// Listen address for metrics server.
    pub listen_addr: String,
}

impl Default for MetricsConfig {
    fn default() -> Self {
        Self {
            enabled: true,
            listen_addr: "127.0.0.1:9090".to_string(),
        }
    }
}

/// Logging configuration.
#[derive(Debug, Clone, Deserialize)]
#[serde(default)]
pub struct LoggingConfig {
    /// Log level filter (e.g., "info", "debug", "warn").
    pub level: String,

    /// Output format: "pretty" or "json".
    pub format: String,
}

impl Default for LoggingConfig {
    fn default() -> Self {
        Self {
            level: "info".to_string(),
            format: "pretty".to_string(),
        }
    }
}

impl Config {
    /// Load configuration from a TOML file.
    pub fn from_file<P: AsRef<Path>>(path: P) -> Result<Self, DaemonError> {
        let content = std::fs::read_to_string(path.as_ref()).map_err(|e| DaemonError::Config(format!("Failed to read config file {}: {}", path.as_ref().display(), e)))?;

        let config: Config = toml::from_str(&content).map_err(|e| DaemonError::Config(format!("Failed to parse config: {}", e)))?;

        Ok(config)
    }

    /// Load configuration from file, then apply environment variable overrides.
    pub fn load<P: AsRef<Path>>(path: Option<P>) -> Result<Self, DaemonError> {
        let mut config = match path {
            Some(p) => Self::from_file(p)?,
            None => Self::default(),
        };

        // Apply environment variable overrides
        config.apply_env_overrides();

        Ok(config)
    }

    /// Apply environment variable overrides.
    fn apply_env_overrides(&mut self) {
        // Server overrides
        if let Ok(v) = std::env::var("DAEMON_SOCKET_PATH") {
            self.server.socket_path = v;
        }
        if let Ok(v) = std::env::var("DAEMON_MAX_CONNECTIONS") {
            if let Ok(n) = v.parse() {
                self.server.max_connections = n;
            }
        }
        if let Ok(v) = std::env::var("DAEMON_HANDOFF_TIMEOUT_SECS") {
            if let Ok(n) = v.parse() {
                self.server.handoff_timeout_secs = n;
            }
        }
        if let Ok(v) = std::env::var("DAEMON_WRITE_TIMEOUT_SECS") {
            if let Ok(n) = v.parse() {
                self.server.write_timeout_secs = n;
            }
        }
        if let Ok(v) = std::env::var("DAEMON_SHUTDOWN_TIMEOUT_SECS") {
            if let Ok(n) = v.parse() {
                self.server.shutdown_timeout_secs = n;
            }
        }
        if let Ok(v) = std::env::var("DAEMON_SOCKET_MODE") {
            // Parse as octal if prefixed with 0o or 0, otherwise decimal
            let v = v.trim();
            if let Some(stripped) = v.strip_prefix("0o") {
                if let Ok(n) = u32::from_str_radix(stripped, 8) {
                    self.server.socket_mode = n;
                }
            } else if v.starts_with('0') && v.len() > 1 {
                if let Ok(n) = u32::from_str_radix(v, 8) {
                    self.server.socket_mode = n;
                }
            } else if let Ok(n) = v.parse() {
                self.server.socket_mode = n;
            }
        }
        if let Ok(v) = std::env::var("DAEMON_HANDOFF_BUFFER_SIZE") {
            if let Ok(n) = v.parse() {
                self.server.handoff_buffer_size = n;
            }
        }
        if let Ok(v) = std::env::var("DAEMON_BACKEND_TIMEOUT_SECS") {
            if let Ok(n) = v.parse() {
                self.server.backend_timeout_secs = n;
            }
        }

        // Backend overrides
        if let Ok(v) = std::env::var("DAEMON_BACKEND_PROVIDER") {
            self.backend.provider = v;
        }
        if let Ok(v) = std::env::var("DAEMON_DEFAULT_MODEL") {
            self.backend.default_model = v;
        }

        // OpenAI overrides (standard env var)
        if let Ok(v) = std::env::var("OPENAI_API_KEY") {
            self.backend.openai.api_key = Some(v);
        }
        if let Ok(v) = std::env::var("OPENAI_API_BASE") {
            self.backend.openai.api_base = v;
        }
        if let Ok(v) = std::env::var("OPENAI_API_SOCKET") {
            self.backend.openai.api_socket = Some(v);
        }
        // HTTP/2 overrides
        if let Ok(v) = std::env::var("OPENAI_HTTP2_ENABLED") {
            self.backend.openai.http2.enabled = v != "false" && v != "0";
        }
        // TLS insecure mode (for testing with self-signed certificates)
        if let Ok(v) = std::env::var("OPENAI_INSECURE_SSL") {
            self.backend.openai.insecure_ssl = v.eq_ignore_ascii_case("true") || v == "1";
        }

        // Metrics overrides
        if let Ok(v) = std::env::var("DAEMON_METRICS_ENABLED") {
            self.metrics.enabled = v == "true" || v == "1";
        }
        if let Ok(v) = std::env::var("DAEMON_METRICS_ADDR") {
            self.metrics.listen_addr = v;
        }

        // Logging overrides
        if let Ok(v) = std::env::var("DAEMON_LOG_LEVEL") {
            self.logging.level = v;
        }
        if let Ok(v) = std::env::var("DAEMON_LOG_FORMAT") {
            self.logging.format = v;
        }
    }
}

/// Deserialize socket mode from various formats (octal string, decimal).
fn deserialize_mode<'de, D>(deserializer: D) -> Result<u32, D::Error>
where
    D: serde::Deserializer<'de>,
{
    use serde::de::Error;

    #[derive(Deserialize)]
    #[serde(untagged)]
    enum ModeValue {
        Number(u32),
        String(String),
    }

    match ModeValue::deserialize(deserializer)? {
        ModeValue::Number(n) => Ok(n),
        ModeValue::String(s) => {
            // Parse as octal if it starts with 0o or 0
            let s = s.trim();
            if let Some(stripped) = s.strip_prefix("0o") {
                u32::from_str_radix(stripped, 8).map_err(D::Error::custom)
            } else if s.starts_with('0') && s.len() > 1 {
                u32::from_str_radix(s, 8).map_err(D::Error::custom)
            } else {
                s.parse().map_err(D::Error::custom)
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_default_config() {
        let config = Config::default();
        assert_eq!(config.server.max_connections, 100_000);
        assert_eq!(config.backend.provider, "mock");
    }

    #[test]
    fn test_default_http2_config() {
        let config = Http2Config::default();
        assert!(config.enabled);
        assert_eq!(config.initial_stream_window_kb, 1024);
        assert_eq!(config.initial_connection_window_kb, 10240);
        assert!(config.adaptive_window);
        assert_eq!(config.keep_alive_interval_secs, 10);
        assert_eq!(config.keep_alive_timeout_secs, 20);
    }

    #[test]
    fn test_parse_toml() {
        let toml = r#"
            [server]
            socket_path = "/tmp/test.sock"
            max_connections = 5000

            [backend]
            provider = "openai"
        "#;

        let config: Config = toml::from_str(toml).unwrap();
        assert_eq!(config.server.socket_path, "/tmp/test.sock");
        assert_eq!(config.server.max_connections, 5000);
        assert_eq!(config.backend.provider, "openai");
    }

    #[test]
    fn test_parse_http2_config() {
        let toml = r#"
            [backend]
            provider = "openai"

            [backend.openai.http2]
            enabled = false
            initial_stream_window_kb = 128
            initial_connection_window_kb = 2048
            adaptive_window = false
            keep_alive_interval_secs = 30
            keep_alive_timeout_secs = 60
        "#;

        let config: Config = toml::from_str(toml).unwrap();
        assert!(!config.backend.openai.http2.enabled);
        assert_eq!(config.backend.openai.http2.initial_stream_window_kb, 128);
        assert_eq!(config.backend.openai.http2.initial_connection_window_kb, 2048);
        assert!(!config.backend.openai.http2.adaptive_window);
        assert_eq!(config.backend.openai.http2.keep_alive_interval_secs, 30);
        assert_eq!(config.backend.openai.http2.keep_alive_timeout_secs, 60);
    }
}
