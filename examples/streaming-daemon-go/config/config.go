// Package config provides YAML configuration file support for the streaming daemon.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration structure.
type Config struct {
	Server  ServerConfig  `yaml:"server"`
	Backend BackendConfig `yaml:"backend"`
	Metrics MetricsConfig `yaml:"metrics"`
	Logging LoggingConfig `yaml:"logging"`
}

// ServerConfig contains server-level settings.
type ServerConfig struct {
	SocketPath     string `yaml:"socket_path"`
	SocketMode     uint32 `yaml:"socket_mode"`
	MaxConnections int    `yaml:"max_connections"`
	PprofAddr      string `yaml:"pprof_addr"`
}

// BackendConfig contains backend selection and configuration.
type BackendConfig struct {
	Provider     string `yaml:"provider"`
	DefaultModel string `yaml:"default_model"`

	// Backend-specific configuration sections
	OpenAI OpenAIConfig `yaml:"openai"`
	Mock   MockConfig   `yaml:"mock"`
	Typing TypingConfig `yaml:"typing"`
}

// OpenAIConfig contains OpenAI-compatible API settings.
type OpenAIConfig struct {
	APIKey       string `yaml:"api_key"`
	APIBase      string `yaml:"api_base"`
	APISocket    string `yaml:"api_socket"`
	HTTP2Enabled *bool  `yaml:"http2_enabled"` // Pointer to distinguish unset from false
	InsecureSSL  bool   `yaml:"insecure_ssl"`
}

// MockConfig contains mock backend settings.
type MockConfig struct {
	MessageDelayMs int `yaml:"message_delay_ms"`
}

// TypingConfig contains typing backend settings.
type TypingConfig struct {
	// Future: typing speed settings, fortune category, etc.
}

// MetricsConfig contains Prometheus metrics settings.
type MetricsConfig struct {
	Enabled    bool   `yaml:"enabled"`
	ListenAddr string `yaml:"listen_addr"`
}

// LoggingConfig contains logging settings.
// Note: Currently unused but reserved for future structured logging support.
type LoggingConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// Load reads and parses a YAML config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config file: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &cfg, nil
}

// Validate checks the configuration for errors.
func (c *Config) Validate() error {
	// Validate server config
	if c.Server.MaxConnections < 0 {
		return fmt.Errorf("server.max_connections must be non-negative")
	}

	// Validate socket path for security (prevent path traversal)
	if c.Server.SocketPath != "" {
		// Must be absolute path
		if !filepath.IsAbs(c.Server.SocketPath) {
			return fmt.Errorf("server.socket_path must be absolute path")
		}
		// Check for path traversal attempts
		cleaned := filepath.Clean(c.Server.SocketPath)
		if strings.Contains(cleaned, "..") {
			return fmt.Errorf("server.socket_path contains invalid path components")
		}
	}

	// Validate metrics config
	if c.Metrics.Enabled && c.Metrics.ListenAddr == "" {
		return fmt.Errorf("metrics.listen_addr required when metrics.enabled is true")
	}

	// Note: Backend provider validation is done in main.go against registered backends
	// to avoid circular imports between config and backends packages.

	// Validate mock backend
	if c.Backend.Mock.MessageDelayMs < 0 {
		return fmt.Errorf("backend.mock.message_delay_ms must be non-negative")
	}

	return nil
}

// Default returns a Config with sensible defaults.
func Default() *Config {
	http2 := true
	return &Config{
		Server: ServerConfig{
			SocketPath:     "/var/run/streaming-daemon.sock",
			SocketMode:     0660,
			MaxConnections: 50000,
		},
		Backend: BackendConfig{
			Provider:     "mock",
			DefaultModel: "gpt-4o-mini",
			OpenAI: OpenAIConfig{
				APIBase:      "https://api.openai.com/v1",
				HTTP2Enabled: &http2,
			},
			Mock: MockConfig{
				MessageDelayMs: 50,
			},
		},
		Metrics: MetricsConfig{
			Enabled:    true,
			ListenAddr: "127.0.0.1:9090",
		},
		Logging: LoggingConfig{
			Level:  "info",
			Format: "text",
		},
	}
}
