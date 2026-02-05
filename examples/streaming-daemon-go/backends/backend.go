// Package backends provides a plugin architecture for streaming daemon backends.
// Backends implement the Backend interface and register themselves via init().
package backends

import (
	"context"
	"net"
	"sort"
	"sync"

	"examples/config"
)

// Message represents a single message in the conversation.
type Message struct {
	Role    string `json:"role"`    // "user", "assistant", or "system"
	Content string `json:"content"` // The message content
}

// HandoffData is the JSON structure passed from PHP via X-Handoff-Data header.
// Customize this struct to match your application's needs.
type HandoffData struct {
	UserID      int64     `json:"user_id"`
	Prompt      string    `json:"prompt"`                 // Single prompt (legacy, used if Messages is empty)
	Messages    []Message `json:"messages,omitempty"`     // Full conversation history
	Model       string    `json:"model,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Timestamp   int64     `json:"timestamp,omitempty"`
	TestPattern string    `json:"test_pattern,omitempty"` // For testing: passed to backend as X-Test-Pattern header
}

// Backend defines the interface for streaming backends.
// Each backend is responsible for streaming content to the client connection.
//
// Thread safety:
//   - Init() is called once at startup before any Stream() calls.
//   - Stream() may be called concurrently from multiple goroutines.
//   - Backends must ensure Stream() is safe for concurrent use.
type Backend interface {
	// Name returns the unique identifier for this backend (used with -backend flag).
	Name() string

	// Description returns a human-readable description for help text.
	Description() string

	// Init initializes the backend (called once at startup before any Stream calls).
	// Use this for setting up HTTP clients, loading configuration, etc.
	// The config parameter contains backend-specific settings from the config file.
	// Not safe for concurrent use - must complete before Stream() is called.
	Init(cfg *config.BackendConfig) error

	// Stream sends the response to the client connection.
	// Returns bytes written and any error.
	// The connection already has SSE headers sent; backends should send SSE data events.
	// Must be safe for concurrent use from multiple goroutines.
	Stream(ctx context.Context, conn net.Conn, handoff HandoffData) (int64, error)
}

// registry holds all registered backends.
var (
	registry   = make(map[string]Backend)
	registryMu sync.RWMutex
)

// Register adds a backend to the registry.
// Call this from init() in each backend file.
func Register(b Backend) {
	registryMu.Lock()
	defer registryMu.Unlock()
	name := b.Name()
	if _, exists := registry[name]; exists {
		panic("backend already registered: " + name)
	}
	registry[name] = b
}

// Get retrieves a backend by name. Returns nil if not found.
func Get(name string) Backend {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return registry[name]
}

// List returns a sorted list of all registered backend names.
func List() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// All returns all registered backends as a map.
func All() map[string]Backend {
	registryMu.RLock()
	defer registryMu.RUnlock()
	result := make(map[string]Backend, len(registry))
	for name, b := range registry {
		result[name] = b
	}
	return result
}
