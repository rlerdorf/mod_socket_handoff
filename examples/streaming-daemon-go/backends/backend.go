// Package backends provides a plugin architecture for streaming daemon backends.
// Backends implement the Backend interface and register themselves via init().
package backends

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"net"
	"slices"
	"sync"

	"examples/config"
)

// StringMap is a map[string]string that silently ignores non-string values
// during JSON unmarshalling instead of failing the entire decode.
type StringMap map[string]string

// UnmarshalJSON decodes a JSON object into a string-to-string map, skipping
// any values that are not JSON strings. Non-object inputs (null, array, etc.)
// are silently treated as an empty map to avoid failing the entire handoff decode.
func (m *StringMap) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		// Non-object value (null, array, number, etc.) — treat as empty.
		// Callers only check len/range, so nil avoids an allocation.
		*m = nil
		return nil
	}
	// JSON null or {} — treat as empty without allocating.
	if len(raw) == 0 {
		*m = nil
		return nil
	}
	var result StringMap
	for k, v := range raw {
		// Skip JSON null values without allocating a string.
		if len(v) == 4 && v[0] == 'n' && v[1] == 'u' && v[2] == 'l' && v[3] == 'l' {
			continue
		}
		var s string
		if json.Unmarshal(v, &s) == nil {
			if result == nil {
				result = make(StringMap, len(raw))
			}
			result[k] = s
		}
	}
	*m = result
	return nil
}

// Message represents a single message in the conversation.
type Message struct {
	Role    string `json:"role"`    // "user", "assistant", or "system"
	Content string `json:"content"` // The message content
}

// HandoffData is the JSON structure passed from PHP via X-Handoff-Data header.
// Customize this struct to match your application's needs.
type HandoffData struct {
	// The complete LangGraph run envelope built by the client (assistant_id,
	// input, stream_mode, config, on_disconnect, etc.). When present, the daemon
	// forwards it verbatim after injecting resolved file attachments.
	LGBody json.RawMessage `json:"lg_body,omitempty"`

	// Deprecated: use lg_body
	UserID         int64          `json:"user_id"`
	Prompt         string         `json:"prompt"`                    // Deprecated: use lg_body.input.messages
	Messages       []Message      `json:"messages,omitempty"`        // Deprecated: use lg_body.input.messages
	AssistantID    string         `json:"assistant_id,omitempty"`    // Deprecated: use lg_body.assistant_id
	StreamMode     []string       `json:"stream_mode,omitempty"`     // Deprecated: use lg_body.stream_mode
	LangGraphInput map[string]any `json:"langgraph_input,omitempty"` // Deprecated: use lg_body.input.*
	Images         []ImageData    `json:"images,omitempty"`          // Deprecated: use lg_body content or ImagePaths

	// Deprecated: unused
	Model     string `json:"model,omitempty"`
	MaxTokens int    `json:"max_tokens,omitempty"`
	Timestamp int64  `json:"timestamp,omitempty"`

	// Deprecated: use Attachments
	ImagePath     string `json:"image_path,omitempty"`
	ImageBase64   string `json:"image_base64,omitempty"`
	ImageMimeType string `json:"image_mime_type,omitempty"`

	TestPattern string `json:"test_pattern,omitempty"` // For testing: passed to backend as X-Test-Pattern header

	// LangGraph transport/routing fields
	ThreadID     string `json:"thread_id,omitempty"`         // For stateful runs (uses /threads/{id}/runs/stream)
	Profile      string `json:"profile,omitempty"`           // Named LangGraph profile from config
	LangGraphURL string `json:"langgraph_url,omitempty"`     // Per-request API base URL override
	LangGraphKey string `json:"langgraph_api_key,omitempty"` // Per-request API key override
	LG           string `json:"lg,omitempty"`                // Compact: "profile|url|key" (pipe-delimited)

	// Per-request backend selection (overrides the daemon's default provider)
	Backend string `json:"backend,omitempty"`

	// Image and attachment file paths — daemon reads, base64-encodes, and deletes
	ImagePaths []string `json:"image_paths,omitempty"`

	// Custom HTTP response headers to include in the response to the client.
	// Headers that conflict with SSE framing (Content-Type, Connection, etc.) are silently dropped.
	// Uses StringMap to silently ignore non-string values without failing the entire unmarshal.
	ResponseHeaders StringMap `json:"response_headers,omitempty"`

	// Generalized attachments: ref_name -> relative file path (under data_dir)
	Attachments     map[string]string `json:"attachments,omitempty"`
	AttachmentTypes map[string]string `json:"attachment_types,omitempty"` // ref_name -> MIME type override (skips extension detection)

	// Computed field populated by resolveImages() before Stream()
	ResolvedImages []ImageData `json:"-"`

	// Computed field populated by resolveAttachments() before Stream()
	ResolvedAttachments map[string]ResolvedAttachment `json:"-"`

	// StagedBytes is the running total of on-disk bytes consumed by image and
	// attachment files for this request, used to enforce the per-request cap
	// (config.MaxTotalAttachmentBytes) across resolveImages and resolveAttachments.
	StagedBytes int64 `json:"-"`

	// Content format for non-text/non-image attachments (populated from LangGraph profile)
	ContentFormat string `json:"-"`
}

// ResolvedAttachment holds a resolved attachment ready for inclusion in LLM requests.
type ResolvedAttachment struct {
	MimeType string // e.g. "image/png", "text/plain"
	IsText   bool   // true = inline text content, false = base64 binary
	Base64   string // populated for binary (images)
	Text     string // populated for text files
}

// ImageData represents a single image for multimodal requests.
type ImageData struct {
	Base64   string `json:"base64"`
	MimeType string `json:"mime_type,omitempty"` // defaults to "image/jpeg"
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
// initialized tracks backends that completed Init() successfully.
var (
	registry      = make(map[string]Backend)
	registryMu    sync.RWMutex
	initialized   map[string]Backend
	initializedMu sync.RWMutex
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
	slices.Sort(names)
	return names
}

// All returns all registered backends as a map.
func All() map[string]Backend {
	registryMu.RLock()
	defer registryMu.RUnlock()
	result := make(map[string]Backend, len(registry))
	maps.Copy(result, registry)
	return result
}

// GetInitialized retrieves an initialized backend by name.
// Returns nil if the backend was not successfully initialized.
func GetInitialized(name string) Backend {
	initializedMu.RLock()
	defer initializedMu.RUnlock()
	return initialized[name]
}

// InitAll initializes all registered backends with the given config.
// Returns the names of successfully initialized backends.
// Backends that fail to initialize are logged but not fatal.
// Successfully initialized backends are available via GetInitialized().
func InitAll(cfg *config.BackendConfig) []string {
	// Snapshot the registry under the lock to avoid holding it
	// across potentially slow Init() calls.
	registryMu.RLock()
	snapshot := make(map[string]Backend, len(registry))
	maps.Copy(snapshot, registry)
	registryMu.RUnlock()

	// Initialize in sorted order for deterministic startup logs.
	names := make([]string, 0, len(snapshot))
	for name := range snapshot {
		names = append(names, name)
	}
	slices.Sort(names)

	result := make(map[string]Backend, len(names))
	var initNames []string
	for _, name := range names {
		b := snapshot[name]
		if err := b.Init(cfg); err != nil {
			slog.Warn("backend init failed", "backend", name, "error", err)
			continue
		}
		result[name] = b
		initNames = append(initNames, name)
	}

	initializedMu.Lock()
	initialized = result
	initializedMu.Unlock()

	return initNames
}
