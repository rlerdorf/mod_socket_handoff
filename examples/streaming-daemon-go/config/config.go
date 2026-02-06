// Package config provides YAML configuration file support for the streaming daemon.
package config

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
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
	SocketPath          string `yaml:"socket_path"`
	SocketMode          uint32 `yaml:"socket_mode"`
	MaxConnections      int    `yaml:"max_connections"`
	MaxStreamDurationMs int    `yaml:"max_stream_duration_ms"`
	PprofAddr           string `yaml:"pprof_addr"`
	MemLimit            string `yaml:"memlimit"`   // Soft memory limit, e.g. "768MiB", "1GiB"
	GCPercent           int    `yaml:"gc_percent"` // GOGC value; 0 = not set (use -gc-percent flag for GOGC=0)
}

// BackendConfig contains backend selection and configuration.
type BackendConfig struct {
	Provider     string `yaml:"provider"`
	DefaultModel string `yaml:"default_model"`

	// Backend-specific configuration sections
	OpenAI    OpenAIConfig    `yaml:"openai"`
	LangGraph LangGraphConfig `yaml:"langgraph"`
	Mock      MockConfig      `yaml:"mock"`
	Typing    TypingConfig    `yaml:"typing"`
}

// OpenAIConfig contains OpenAI-compatible API settings.
type OpenAIConfig struct {
	APIKey       string `yaml:"api_key"`
	APIBase      string `yaml:"api_base"`
	APISocket    string `yaml:"api_socket"`
	HTTP2Enabled *bool  `yaml:"http2_enabled"` // Pointer to distinguish unset from false
	InsecureSSL  bool   `yaml:"insecure_ssl"`
}

// LangGraphConfig contains LangGraph Platform API settings.
type LangGraphConfig struct {
	APIKey       string `yaml:"api_key"`
	APIBase      string `yaml:"api_base"`
	APISocket    string `yaml:"api_socket"`
	AssistantID  string `yaml:"assistant_id"`
	StreamMode   string `yaml:"stream_mode"`
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

// LoggingConfig contains logging settings for slog configuration.
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
	if c.Server.MaxStreamDurationMs < 0 {
		return fmt.Errorf("server.max_stream_duration_ms must be non-negative")
	}

	// Validate socket paths for security.
	// Requiring absolute paths prevents path traversal; filepath.Clean()
	// (used by callers) normalizes any ".." components.
	if c.Server.SocketPath != "" && !filepath.IsAbs(c.Server.SocketPath) {
		return fmt.Errorf("server.socket_path must be absolute path")
	}
	if c.Backend.OpenAI.APISocket != "" && !filepath.IsAbs(c.Backend.OpenAI.APISocket) {
		return fmt.Errorf("backend.openai.api_socket must be absolute path")
	}
	if c.Backend.LangGraph.APISocket != "" && !filepath.IsAbs(c.Backend.LangGraph.APISocket) {
		return fmt.Errorf("backend.langgraph.api_socket must be absolute path")
	}

	// Validate metrics config
	if c.Metrics.Enabled && c.Metrics.ListenAddr == "" {
		return fmt.Errorf("metrics.listen_addr required when metrics.enabled is true")
	}

	// Note: Backend provider validation is done in main.go against registered backends
	// to avoid circular imports between config and backends packages.

	// Validate memory limit format early so config-file typos are caught at load time
	if c.Server.MemLimit != "" {
		if ParseMemLimit(c.Server.MemLimit) <= 0 {
			return fmt.Errorf("server.memlimit: invalid value %q (expected positive integer with optional suffix B, KiB, MiB, GiB, TiB)", c.Server.MemLimit)
		}
	}

	// Validate GC percent: negative values have special meaning in the Go runtime
	// (e.g. -1 disables the GOGC knob entirely), so reject them in config to avoid
	// surprising behavior. Use the -gc-percent flag for advanced runtime tuning.
	if c.Server.GCPercent < 0 {
		return fmt.Errorf("server.gc_percent must be non-negative")
	}

	// Validate mock backend
	if c.Backend.Mock.MessageDelayMs < 0 {
		return fmt.Errorf("backend.mock.message_delay_ms must be non-negative")
	}

	return nil
}

// ParseMemLimit parses a memory limit string like "768MiB" or "1GiB" into bytes.
// Supported suffixes: B, KiB, MiB, GiB, TiB (case-insensitive).
// The numeric part must be a positive integer (no zero, negative, or fractional values).
// Returns -1 on parse error or overflow.
func ParseMemLimit(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return -1
	}

	// Find where the numeric part ends
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return -1
	}

	numStr := s[:i]
	suffix := strings.TrimSpace(s[i:])

	// Parse the number as uint64 (catches overflow and negative values)
	num, err := strconv.ParseUint(numStr, 10, 64)
	if err != nil || num == 0 {
		return -1
	}

	// Parse the suffix
	var multiplier uint64
	switch strings.ToLower(suffix) {
	case "", "b":
		multiplier = 1
	case "kib", "k", "kb":
		multiplier = 1024
	case "mib", "m", "mb":
		multiplier = 1024 * 1024
	case "gib", "g", "gb":
		multiplier = 1024 * 1024 * 1024
	case "tib", "t", "tb":
		multiplier = 1024 * 1024 * 1024 * 1024
	default:
		return -1
	}

	// Check for overflow before multiplying.
	// If multiplier > MaxInt64/num, the product would exceed MaxInt64.
	if multiplier > math.MaxInt64/num {
		return -1
	}

	return int64(num * multiplier)
}

// Default returns a Config with sensible defaults.
func Default() *Config {
	http2 := true
	return &Config{
		Server: ServerConfig{
			SocketPath:          "/var/run/streaming-daemon.sock",
			SocketMode:          0660,
			MaxConnections:      50000,
			MaxStreamDurationMs: 300000,
		},
		Backend: BackendConfig{
			Provider:     "mock",
			DefaultModel: "gpt-4o-mini",
			OpenAI: OpenAIConfig{
				APIBase:      "https://api.openai.com/v1",
				HTTP2Enabled: &http2,
			},
			LangGraph: LangGraphConfig{
				APIBase:      "https://api.langchain.com/v1",
				AssistantID:  "agent",
				StreamMode:   "messages-tuple",
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
