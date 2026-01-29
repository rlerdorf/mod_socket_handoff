//! Configuration loading from TOML files and environment variables.

use serde::Deserialize;
use std::path::Path;
use std::time::Duration;

use crate::error::DaemonError;

/// Main configuration structure.
#[derive(Debug, Clone, Deserialize)]
#[serde(default)]
pub struct Config {
    pub server: ServerConfig,
    pub backend: BackendConfig,
    pub metrics: MetricsConfig,
    pub logging: LoggingConfig,
}

impl Default for Config {
    fn default() -> Self {
        Self {
            server: ServerConfig::default(),
            backend: BackendConfig::default(),
            metrics: MetricsConfig::default(),
            logging: LoggingConfig::default(),
        }
    }
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
}

impl Default for ServerConfig {
    fn default() -> Self {
        Self {
            socket_path: "/var/run/streaming-daemon-rs.sock".to_string(),
            socket_mode: 0o666,
            max_connections: 100_000,
            handoff_timeout_secs: 5,
            write_timeout_secs: 30,
            shutdown_timeout_secs: 120,
            handoff_buffer_size: 65536,
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

    /// Maximum idle connections per host in pool.
    pub pool_max_idle_per_host: usize,
}

impl Default for OpenAIConfig {
    fn default() -> Self {
        Self {
            api_key: None,
            api_base: "https://api.openai.com/v1".to_string(),
            pool_max_idle_per_host: 100,
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
        let content = std::fs::read_to_string(path.as_ref()).map_err(|e| {
            DaemonError::Config(format!(
                "Failed to read config file {}: {}",
                path.as_ref().display(),
                e
            ))
        })?;

        let config: Config = toml::from_str(&content)
            .map_err(|e| DaemonError::Config(format!("Failed to parse config: {}", e)))?;

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
        if let Ok(v) = std::env::var("DAEMON_HANDOFF_TIMEOUT") {
            if let Ok(n) = v.parse() {
                self.server.handoff_timeout_secs = n;
            }
        }
        if let Ok(v) = std::env::var("DAEMON_WRITE_TIMEOUT") {
            if let Ok(n) = v.parse() {
                self.server.write_timeout_secs = n;
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
}
