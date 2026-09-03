package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Each *bool field must be its own allocation: yaml.v3 decodes into the
// existing pointee in place, so a shared pointer would make one backend's
// http2_enabled setting overwrite the other's.
func TestDefaultHTTP2PointersIndependent(t *testing.T) {
	cfg := Default()
	if cfg.Backend.OpenAI.HTTP2Enabled == cfg.Backend.LangGraph.HTTP2Enabled {
		t.Fatal("OpenAI and LangGraph HTTP2Enabled share one pointer")
	}

	tmpFile := filepath.Join(t.TempDir(), "config.yaml")
	content := "backend:\n  openai:\n    http2_enabled: false\n"
	if err := os.WriteFile(tmpFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(tmpFile)
	if err != nil {
		t.Fatal(err)
	}
	if *loaded.Backend.OpenAI.HTTP2Enabled {
		t.Error("openai.http2_enabled = true, want false")
	}
	if !*loaded.Backend.LangGraph.HTTP2Enabled {
		t.Error("langgraph.http2_enabled flipped to false by the openai setting")
	}
}

// A file that is valid at startup (defaults fill in listen_addr and
// max_connections) must also be accepted by the SIGHUP reload path.
func TestLoadRawAcceptsPartialConfig(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "config.yaml")
	content := "metrics:\n  enabled: true\nlogging:\n  level: debug\n"
	if err := os.WriteFile(tmpFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(tmpFile); err != nil {
		t.Fatalf("Load: %v", err)
	}
	raw, err := LoadRaw(tmpFile)
	if err != nil {
		t.Fatalf("LoadRaw rejected a config that Load accepted: %v", err)
	}
	if raw.Logging.Level != "debug" {
		t.Errorf("Logging.Level = %q, want debug", raw.Logging.Level)
	}

	// Reloadable fields are still validated.
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("logging:\n  level: loud\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRaw(bad); err == nil || !strings.Contains(err.Error(), "logging.level") {
		t.Errorf("LoadRaw error = %v, want logging.level validation error", err)
	}
}

func TestValidateServerLimits(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"max_connections zero rejects all connections", func(c *Config) { c.Server.MaxConnections = 0 }, "max_connections"},
		{"socket_mode decimal 660 is a typo", func(c *Config) { c.Server.SocketMode = 660 }, "socket_mode"},
		{"socket_mode octal 0660 ok", func(c *Config) { c.Server.SocketMode = 0660 }, ""},
		{"negative data_dir_max_age_ms", func(c *Config) { c.Server.DataDirMaxAgeMs = -1 }, "data_dir_max_age_ms"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			tt.mutate(cfg)
			err := cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}
