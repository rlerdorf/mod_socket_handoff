package backends

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"examples/config"
)

// LangGraph backend configuration flags (can override config file)
var (
	langgraphBaseFlag = flag.String("langgraph-base", "", "LangGraph API base URL (overrides config file)")
	langgraphSocketFlag = flag.String("langgraph-socket", "", "Unix socket for LangGraph API (overrides config file)")
	langgraphAssistantFlag = flag.String("langgraph-assistant", "", "LangGraph assistant ID (overrides config file)")
)

// langgraphProfile holds resolved configuration for a LangGraph backend target.
// Profiles are built once during Init() and are read-only after that.
type langgraphProfile struct {
	apiBase       string
	apiKey        string
	assistantID   string
	streamMode    string
	contentFormat string // "openai" (default) or "anthropic"
	insecure      bool
	httpClient    *http.Client
}

// LangGraph configuration (set during Init).
//
// Thread safety: Init() must be called exactly once at process startup,
// on a single goroutine, before any Stream() calls. The default profile
// and profiles map are then read-only during normal operation.
// Per-request clones are ephemeral and goroutine-local.
var (
	langgraphDefault  *langgraphProfile
	langgraphProfiles map[string]*langgraphProfile
)

// LangGraph is a backend that streams responses from the LangGraph Platform API.
type LangGraph struct{}

func init() {
	Register(&LangGraph{})
}

func (l *LangGraph) Name() string {
	return "langgraph"
}

func (l *LangGraph) Description() string {
	return "LangGraph Platform API (stateful agents with threads)"
}

// Init sets up the HTTP client for LangGraph API calls.
// Configuration priority: flag > env var > config file > default
//
// Named profiles inherit from the fully-resolved default, then overlay
// their own fields. Profile map is write-once here, read-only after.
func (l *LangGraph) Init(cfg *config.BackendConfig) error {
	// Build the default profile: hardcoded defaults → config → env → flags
	apiBase := "https://api.langchain.com/v1"
	var apiKey string
	assistantID := "agent"
	streamMode := "messages-tuple"
	useHTTP2 := true
	insecure := false
	var socketPath string

	// Apply config file values
	if cfg != nil {
		if cfg.LangGraph.APIBase != "" {
			apiBase = cfg.LangGraph.APIBase
		}
		if cfg.LangGraph.APIKey != "" {
			apiKey = cfg.LangGraph.APIKey
		}
		if cfg.LangGraph.APISocket != "" {
			socketPath = cfg.LangGraph.APISocket
		}
		if cfg.LangGraph.AssistantID != "" {
			assistantID = cfg.LangGraph.AssistantID
		}
		if cfg.LangGraph.StreamMode != "" {
			streamMode = cfg.LangGraph.StreamMode
		}
		if cfg.LangGraph.HTTP2Enabled != nil {
			useHTTP2 = *cfg.LangGraph.HTTP2Enabled
		}
		insecure = cfg.LangGraph.InsecureSSL
	}

	// Environment variables override config file
	if envVal := os.Getenv("LANGGRAPH_API_BASE"); envVal != "" {
		apiBase = envVal
	}
	if envVal := os.Getenv("LANGGRAPH_API_KEY"); envVal != "" {
		apiKey = envVal
	}
	if envVal := os.Getenv("LANGGRAPH_API_SOCKET"); envVal != "" {
		socketPath = envVal
	}
	if envVal := os.Getenv("LANGGRAPH_ASSISTANT_ID"); envVal != "" {
		assistantID = envVal
	}
	if envVal := os.Getenv("LANGGRAPH_STREAM_MODE"); envVal != "" {
		streamMode = envVal
	}
	if envVal := os.Getenv("LANGGRAPH_HTTP2_ENABLED"); envVal != "" {
		isFalse := strings.EqualFold(envVal, "false") ||
			strings.EqualFold(envVal, "0") ||
			strings.EqualFold(envVal, "no") ||
			strings.EqualFold(envVal, "off")
		useHTTP2 = !isFalse
	}
	if envVal := os.Getenv("LANGGRAPH_INSECURE_SSL"); envVal != "" {
		insecure = strings.EqualFold(envVal, "true") || strings.EqualFold(envVal, "1")
	}

	// Flags override everything (only if explicitly set)
	if *langgraphBaseFlag != "" {
		apiBase = *langgraphBaseFlag
	}
	if *langgraphSocketFlag != "" {
		socketPath = *langgraphSocketFlag
	}
	if *langgraphAssistantFlag != "" {
		assistantID = *langgraphAssistantFlag
	}

	// Validate API key — warn if this is the default backend, debug otherwise
	if apiKey == "" {
		if cfg != nil && cfg.Provider == "langgraph" {
			slog.Warn("LANGGRAPH_API_KEY not set, API calls will fail")
		} else {
			slog.Debug("LANGGRAPH_API_KEY not set, API calls will fail")
		}
	}

	if insecure {
		slog.Warn("TLS certificate verification disabled for LangGraph")
	}

	// Build default profile
	langgraphDefault = &langgraphProfile{
		apiBase:       apiBase,
		apiKey:        apiKey,
		assistantID:   assistantID,
		streamMode:    streamMode,
		contentFormat: "openai",
		insecure:      insecure,
		httpClient: NewHTTPClient(TransportConfig{
			BaseURL:     apiBase,
			SocketPath:  socketPath,
			UseHTTP2:    useHTTP2,
			InsecureSSL: insecure,
			Label:       "LangGraph",
		}),
	}

	// Build named profiles (inherit from resolved default, overlay profile-specific fields)
	langgraphProfiles = make(map[string]*langgraphProfile)
	if cfg != nil {
		for name, pc := range cfg.LangGraph.Profiles {
			p := &langgraphProfile{
				apiBase:       langgraphDefault.apiBase,
				apiKey:        langgraphDefault.apiKey,
				assistantID:   langgraphDefault.assistantID,
				streamMode:    langgraphDefault.streamMode,
				contentFormat: langgraphDefault.contentFormat,
				insecure:      langgraphDefault.insecure,
			}
			if pc.APIBase != "" {
				p.apiBase = pc.APIBase
			}
			if pc.APIKey != "" {
				p.apiKey = pc.APIKey
			}
			if pc.AssistantID != "" {
				p.assistantID = pc.AssistantID
			}
			if pc.StreamMode != "" {
				p.streamMode = pc.StreamMode
			}
			if pc.ContentFormat != "" {
				p.contentFormat = pc.ContentFormat
			}
			if pc.InsecureSSL != nil {
				p.insecure = *pc.InsecureSSL
			}

			// Build HTTP client: use profile's own socket/http2 if set, else inherit defaults
			profUseHTTP2 := useHTTP2
			if pc.HTTP2Enabled != nil {
				profUseHTTP2 = *pc.HTTP2Enabled
			}
			profSocket := socketPath
			if pc.APISocket != "" {
				profSocket = pc.APISocket
			}

			p.httpClient = NewHTTPClient(TransportConfig{
				BaseURL:     p.apiBase,
				SocketPath:  profSocket,
				UseHTTP2:    profUseHTTP2,
				InsecureSSL: p.insecure,
				Label:       "LangGraph/" + name,
			})

			langgraphProfiles[name] = p
			slog.Info("LangGraph profile configured", "name", name, "api_base", p.apiBase, "assistant_id", p.assistantID)
		}
	}

	return nil
}

// resolveLangGraphProfile returns the effective profile for a request.
// Resolution order (highest wins): per-request overrides → named profile → default.
//
// The compact "lg" field (pipe-delimited "profile|url|key") is parsed first as
// defaults, then the verbose fields (Profile, LangGraphURL, LangGraphKey) override
// if set. This lets callers use either syntax or mix them.
//
// Returns a pointer to either a pre-built profile (zero alloc) or a cloned profile
// with overrides applied (one alloc per overridden request).
func resolveLangGraphProfile(handoff HandoffData) (*langgraphProfile, error) {
	// Parse compact "lg" field as baseline overrides
	var lgProfile, lgURL, lgKey string
	if handoff.LG != "" {
		lgProfile, lgURL, lgKey = parseLG(handoff.LG)
	}

	// Verbose fields override compact syntax
	profile := lgProfile
	if handoff.Profile != "" {
		profile = handoff.Profile
	}
	overrideURL := lgURL
	if handoff.LangGraphURL != "" {
		overrideURL = handoff.LangGraphURL
	}
	overrideKey := lgKey
	if handoff.LangGraphKey != "" {
		overrideKey = handoff.LangGraphKey
	}

	p := langgraphDefault

	// If a named profile is requested, start from that
	if profile != "" {
		named, ok := langgraphProfiles[profile]
		if !ok {
			return nil, fmt.Errorf("unknown LangGraph profile: %q", profile)
		}
		p = named
	}

	// Apply per-request overrides (clone to avoid mutating the shared profile)
	if overrideURL != "" || overrideKey != "" {
		clone := *p // shallow copy
		if overrideURL != "" {
			clone.apiBase = overrideURL
			// Reuse the resolved profile's HTTP client for TCP connections.
			// The client works fine for different base URLs over TCP since it
			// dials based on the request URL. Socket-based transports are
			// profile-specific and require named profiles.
		}
		if overrideKey != "" {
			clone.apiKey = overrideKey
		}
		p = &clone
	}

	return p, nil
}

// parseLG splits a compact "profile|url|key" string into its components.
// Empty segments are returned as empty strings. Missing segments are fine:
//
//	"sales"                          → ("sales", "", "")
//	"sales|https://api.example.com"  → ("sales", "https://api.example.com", "")
//	"sales||lgk_key"                 → ("sales", "", "lgk_key")
//	"|https://api.example.com|lgk_key" → ("", "https://api.example.com", "lgk_key")
func parseLG(lg string) (profile, url, key string) {
	first := strings.IndexByte(lg, '|')
	if first < 0 {
		return lg, "", ""
	}
	profile = lg[:first]
	rest := lg[first+1:]
	second := strings.IndexByte(rest, '|')
	if second < 0 {
		return profile, rest, ""
	}
	return profile, rest[:second], rest[second+1:]
}

// langgraphBufPool reuses buffers for building HTTP request bodies.
var langgraphBufPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 0, 1024)
		return &buf
	},
}

// copyBufPool reuses 32KB read buffers for the raw proxy loop to reduce allocations under load.
var copyBufPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 32*1024)
		return &buf
	},
}

// threadCreateTimeout bounds how long the thread creation request can take.
const threadCreateTimeout = 10 * time.Second

// ensureThreadExists creates the thread via POST /threads with if_exists=do_nothing,
// so the thread exists before we call /threads/{id}/runs/stream. Idempotent; safe to call every time.
func ensureThreadExists(ctx context.Context, p *langgraphProfile, threadID string) error {
	ctx, cancel := context.WithTimeout(ctx, threadCreateTimeout)
	defer cancel()

	body := struct {
		ThreadID string `json:"thread_id"`
		IfExists string `json:"if_exists"`
	}{ThreadID: threadID, IfExists: "do_nothing"}
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal thread create: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", p.apiBase+"/threads", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create thread request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", p.apiKey)
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("thread create request: %w", err)
	}
	defer resp.Body.Close()
	// 200/201 = created or returned existing; 409 = conflict (e.g. already exists with if_exists semantics) — treat as success
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusConflict {
		return nil
	}
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("thread create %d: %s", resp.StatusCode, string(bodyBytes))
}

// Stream sends a request to the LangGraph API and streams the response to the client.
func (l *LangGraph) Stream(ctx context.Context, conn net.Conn, handoff HandoffData) (int64, error) {
	var totalBytes int64
	backendStart := time.Now()
	var ttfbRecorded bool

	// Resolve the effective profile for this request
	p, err := resolveLangGraphProfile(handoff)
	if err != nil {
		return 0, err
	}

	// Determine assistant ID (handoff override > profile)
	assistantID := p.assistantID
	if handoff.AssistantID != "" {
		assistantID = handoff.AssistantID
	}

	// Set content format from profile for attachment serialization
	handoff.ContentFormat = p.contentFormat

	// For stateful runs, ensure the thread exists before streaming (same as PHP: create then stream).
	if handoff.ThreadID != "" {
		if err := ensureThreadExists(ctx, p, handoff.ThreadID); err != nil {
			return 0, fmt.Errorf("ensure thread: %w", err)
		}
	}

	// Build request body (shared with tests via buildLangGraphRequestBody)
	buf := buildLangGraphRequestBody(handoff, assistantID, p.streamMode)

	// Determine endpoint: stateful (with thread_id) or stateless
	var reqURL string
	if handoff.ThreadID != "" {
		// URL-escape ThreadID to prevent path traversal attacks
		reqURL = fmt.Sprintf("%s/threads/%s/runs/stream", p.apiBase, url.PathEscape(handoff.ThreadID))
	} else {
		reqURL = p.apiBase + "/runs/stream"
	}

	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx, "POST", reqURL, bytes.NewReader(buf))
	if err != nil {
		return 0, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", p.apiKey)

	// Add test pattern header if present (for validation testing)
	if handoff.TestPattern != "" {
		req.Header.Set("X-Test-Pattern", handoff.TestPattern)
	}

	// Record backend request attempt
	RecordBackendRequest("langgraph")

	// Make request
	resp, err := p.httpClient.Do(req)

	if err != nil {
		RecordBackendError("langgraph")
		return 0, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		RecordBackendError("langgraph")
		// Limit error body size to prevent memory exhaustion from large error payloads
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return 0, fmt.Errorf("API error %d: %s", resp.StatusCode, string(bodyBytes))
	}

	// Set write deadline for first chunk
	if err := conn.SetWriteDeadline(time.Now().Add(WriteTimeout)); err != nil {
		return 0, fmt.Errorf("set write deadline: %w", err)
	}

	// Proxy raw SSE stream from LangGraph to client (no parsing; forwards heartbeats, all events, etc.)
	copyBufPtr := copyBufPool.Get().(*[]byte)
	copyBuf := *copyBufPtr
	defer func() {
		*copyBufPtr = copyBuf
		copyBufPool.Put(copyBufPtr)
	}()
	var nr int
	var newlines int // consecutive \n count (ignoring \r) for SSE boundary detection
	for {
		select {
		case <-ctx.Done():
			return totalBytes, ctx.Err()
		default:
		}
		nr, err = resp.Body.Read(copyBuf)
		if nr > 0 {
			if !ttfbRecorded {
				RecordBackendTTFB("langgraph", time.Since(backendStart).Seconds())
				ttfbRecorded = true
			}
			chunk := copyBuf[:nr]
			// Count SSE event boundaries for metrics.
			// A blank line (\n\n or \r\n\r\n) separates SSE events.
			for _, b := range chunk {
				switch b {
				case '\n':
					newlines++
					if newlines >= 2 {
						RecordChunkSent()
						newlines = 0
					}
				case '\r':
					// ignore \r — treat \r\n same as \n
				default:
					newlines = 0
				}
			}
			// Write full chunk, handling short writes
			written := 0
			for written < len(chunk) {
				nw, errw := conn.Write(chunk[written:])
				totalBytes += int64(nw)
				written += nw
				if errw != nil {
					RecordBackendError("langgraph")
					RecordBackendDuration("langgraph", time.Since(backendStart).Seconds())
					return totalBytes, errw
				}
			}
			if err := conn.SetWriteDeadline(time.Now().Add(WriteTimeout)); err != nil {
				RecordBackendError("langgraph")
				RecordBackendDuration("langgraph", time.Since(backendStart).Seconds())
				return totalBytes, fmt.Errorf("set write deadline: %w", err)
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			RecordBackendError("langgraph")
			RecordBackendDuration("langgraph", time.Since(backendStart).Seconds())
			return totalBytes, err
		}
	}

	// Record backend duration
	RecordBackendDuration("langgraph", time.Since(backendStart).Seconds())

	return totalBytes, nil
}

// buildLangGraphRequestBody builds the JSON request body for a LangGraph API call.
// Used by both Stream() and tests. The defaultStreamMode parameter specifies the
// fallback stream mode when handoff.StreamMode is empty.
func buildLangGraphRequestBody(handoff HandoffData, assistantID string, defaultStreamMode string) []byte {
	bufPtr := langgraphBufPool.Get().(*[]byte)
	buf := (*bufPtr)[:0]

	buf = append(buf, `{"assistant_id":"`...)
	buf = appendJSONEscaped(buf, assistantID)
	buf = append(buf, `","input":{"messages":[`...)

	// Use messages array if provided, otherwise fall back to single prompt
	hasAttachments := len(handoff.ResolvedAttachments) > 0
	contentFormat := handoff.ContentFormat
	if contentFormat == "" {
		contentFormat = "openai"
	}
	if len(handoff.Messages) > 0 {
		for i, msg := range handoff.Messages {
			if i > 0 {
				buf = append(buf, ',')
			}
			buf = append(buf, `{"type":"`...)
			buf = appendJSONEscaped(buf, mapRoleToLangGraph(msg.Role))
			buf = append(buf, `","content":`...)

			// Apply attachments and images to the last message only
			isLastMessage := i == len(handoff.Messages)-1
			if isLastMessage && hasAttachments {
				buf = appendContentWithAttachments(buf, msg.Content, handoff.ResolvedAttachments, handoff.ResolvedImages, contentFormat)
			} else if isLastMessage && len(handoff.ResolvedImages) > 0 {
				buf = appendMultimodalContent(buf, msg.Content, handoff.ResolvedImages, contentFormat)
			} else {
				buf = appendMultimodalContent(buf, msg.Content, nil, contentFormat)
			}
			buf = append(buf, `}`...)
		}
	} else {
		prompt := handoff.Prompt
		if prompt == "" {
			prompt = "Hello"
		}
		buf = append(buf, `{"type":"human","content":`...)
		if hasAttachments {
			buf = appendContentWithAttachments(buf, prompt, handoff.ResolvedAttachments, handoff.ResolvedImages, contentFormat)
		} else {
			buf = appendMultimodalContent(buf, prompt, handoff.ResolvedImages, contentFormat)
		}
		buf = append(buf, `}`...)
	}

	buf = append(buf, `]`...)

	// Add custom LangGraph input fields if provided (sorted for deterministic output)
	if n := len(handoff.LangGraphInput); n > 0 {
		// Use stack-allocated array for common case (<=8 keys) to avoid heap allocation
		var stackKeys [8]string
		var keys []string
		if n <= 8 {
			keys = stackKeys[:0]
		} else {
			keys = make([]string, 0, n)
		}
		for key := range handoff.LangGraphInput {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			buf = append(buf, ',')
			buf = append(buf, '"')
			buf = appendJSONEscaped(buf, key)
			buf = append(buf, `":`...)
			buf = appendJSONValue(buf, handoff.LangGraphInput[key])
		}
	}

	// Close input object
	buf = append(buf, `}`...)

	// Add stream_mode: use handoff value if provided, otherwise use the profile's default
	if len(handoff.StreamMode) > 0 {
		buf = append(buf, `,"stream_mode":[`...)
		for i, mode := range handoff.StreamMode {
			if i > 0 {
				buf = append(buf, ',')
			}
			buf = append(buf, '"')
			buf = appendJSONEscaped(buf, mode)
			buf = append(buf, '"')
		}
		buf = append(buf, `]`...)
	} else {
		// Fall back to configured default stream mode
		buf = append(buf, `,"stream_mode":["`...)
		buf = appendJSONEscaped(buf, defaultStreamMode)
		buf = append(buf, `"]`...)
	}

	buf = append(buf, `,"stream_subgraphs":false,"on_completion":"delete","on_disconnect":"cancel"}`...)

	// Return buffer to pool (make a copy since we're returning the content for tests)
	result := make([]byte, len(buf))
	copy(result, buf)
	*bufPtr = buf
	langgraphBufPool.Put(bufPtr)

	return result
}

// contentPart represents a parsed segment of prompt text with placeholder resolution.
type contentPart struct {
	text       string              // literal text or inlined text-attachment content
	attachment *ResolvedAttachment // non-nil for image/binary attachments
	refName    string              // original ref name (for debugging)
}

// parsePlaceholders performs a linear scan of text, resolving {ref_name} placeholders
// against the attachments map. Unresolved placeholders are left as literal text.
func parsePlaceholders(text string, attachments map[string]ResolvedAttachment) []contentPart {
	if len(attachments) == 0 {
		return []contentPart{{text: text}}
	}

	var parts []contentPart
	i := 0
	for i < len(text) {
		open := strings.IndexByte(text[i:], '{')
		if open < 0 {
			// No more braces, append rest as text
			parts = append(parts, contentPart{text: text[i:]})
			break
		}
		open += i // absolute index

		close := strings.IndexByte(text[open+1:], '}')
		if close < 0 {
			// Lone '{' with no matching '}', treat rest as literal
			parts = append(parts, contentPart{text: text[i:]})
			break
		}
		close += open + 1 // absolute index

		refName := text[open+1 : close]
		att, found := attachments[refName]

		if !found {
			// Not a known ref — emit everything up to and including the closing brace as literal text
			parts = append(parts, contentPart{text: text[i : close+1]})
			i = close + 1
			continue
		}

		// Emit text before the placeholder
		if open > i {
			parts = append(parts, contentPart{text: text[i:open]})
		}

		if att.IsText {
			// Inline text content at the placeholder position
			parts = append(parts, contentPart{text: att.Text, refName: refName})
		} else {
			// Binary attachment (image) — emit as attachment part
			attCopy := att
			parts = append(parts, contentPart{attachment: &attCopy, refName: refName})
		}

		i = close + 1
	}

	// Handle empty text
	if len(parts) == 0 {
		parts = append(parts, contentPart{text: ""})
	}

	return parts
}

// appendContentWithAttachments builds the content field for a message, resolving
// attachment placeholders. If no attachments or images are present, emits a simple
// JSON string. If only text attachments are present, inlines them into the text.
// If any image attachment or legacy image is present, builds a content array.
// Unreferenced binary attachments and legacy images are emitted BEFORE the prompt
// text, matching the LangChain HumanMessage convention. Referenced attachments
// stay at their placeholder position. Unreferenced text is concatenated to any
// trailing text part.
func appendContentWithAttachments(buf []byte, text string, attachments map[string]ResolvedAttachment, legacyImages []ImageData, contentFormat string) []byte {
	parts := parsePlaceholders(text, attachments)

	// Collect which ref names were used by placeholders
	referenced := make(map[string]bool, len(parts))
	for _, p := range parts {
		if p.refName != "" {
			referenced[p.refName] = true
		}
	}

	// Collect unreferenced attachments (sorted for deterministic output)
	var unreferencedBinaries []ResolvedAttachment
	var unreferencedText []ResolvedAttachment
	if len(referenced) < len(attachments) {
		// Collect unreferenced attachment names and sort for determinism
		var names []string
		for name := range attachments {
			if !referenced[name] {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		for _, name := range names {
			att := attachments[name]
			if att.IsText {
				unreferencedText = append(unreferencedText, att)
			} else {
				unreferencedBinaries = append(unreferencedBinaries, att)
			}
		}
	}

	// willEmitBinary checks whether a binary attachment will actually produce a
	// content part for the given content format (e.g., PDFs are skipped in openai).
	willEmitBinary := func(mimeType string) bool {
		isImage := strings.HasPrefix(strings.ToLower(mimeType), "image/")
		if isImage {
			return true
		}
		return contentFormat == "anthropic"
	}

	// Check if we have any binary parts (images, documents) that require a content array
	hasBinaryPart := len(legacyImages) > 0
	if !hasBinaryPart {
		for _, att := range unreferencedBinaries {
			if willEmitBinary(att.MimeType) {
				hasBinaryPart = true
				break
			}
		}
	}
	if !hasBinaryPart {
		for _, p := range parts {
			if p.attachment != nil && willEmitBinary(p.attachment.MimeType) {
				hasBinaryPart = true
				break
			}
		}
	}

	if !hasBinaryPart {
		// All parts are text — concatenate and emit as simple JSON string
		buf = append(buf, '"')
		for _, p := range parts {
			if p.attachment != nil && !p.attachment.IsText {
				// Non-emittable binary (e.g., PDF in openai format) — preserve placeholder
				buf = appendJSONEscaped(buf, "{"+p.refName+"}")
			} else {
				buf = appendJSONEscaped(buf, p.text)
			}
		}
		// Append unreferenced text attachments
		for _, att := range unreferencedText {
			buf = appendJSONEscaped(buf, att.Text)
		}
		buf = append(buf, '"')
		return buf
	}

	// Build content array with text and image_url parts
	buf = append(buf, '[')
	first := true

	// Merge consecutive text parts into one text element
	var textAccum strings.Builder
	flushText := func() {
		if textAccum.Len() > 0 {
			if !first {
				buf = append(buf, ',')
			}
			first = false
			buf = append(buf, `{"type":"text","text":"`...)
			buf = appendJSONEscaped(buf, textAccum.String())
			buf = append(buf, `"}`...)
			textAccum.Reset()
		}
	}

	appendImageURL := func(mimeType, base64Data string) {
		flushText()
		if !first {
			buf = append(buf, ',')
		}
		first = false
		buf = append(buf, `{"type":"image_url","image_url":{"url":"data:`...)
		buf = appendJSONEscaped(buf, mimeType)
		buf = append(buf, `;base64,`...)
		buf = append(buf, base64Data...)
		buf = append(buf, `"}}`...)
	}

	appendDocument := func(mimeType, base64Data string) {
		flushText()
		if !first {
			buf = append(buf, ',')
		}
		first = false
		buf = append(buf, `{"type":"document","source":{"type":"base64","media_type":"`...)
		buf = appendJSONEscaped(buf, mimeType)
		buf = append(buf, `","data":"`...)
		buf = append(buf, base64Data...)
		buf = append(buf, `"}}`...)
	}

	appendBinary := func(mimeType, base64Data, refName string) {
		isImage := strings.HasPrefix(strings.ToLower(mimeType), "image/")
		switch contentFormat {
		case "anthropic":
			if isImage {
				appendImageURL(mimeType, base64Data)
			} else {
				appendDocument(mimeType, base64Data)
			}
		default: // "openai"
			if isImage {
				appendImageURL(mimeType, base64Data)
			} else if refName != "" {
				slog.Warn("non-image binary attachment not supported by content format",
					"format", contentFormat, "mime", mimeType, "ref", refName)
				textAccum.WriteString("{" + refName + "}")
			} else {
				slog.Warn("skipping unreferenced non-image binary attachment",
					"format", contentFormat, "mime", mimeType)
			}
		}
	}

	// Emit unreferenced binary attachments and legacy images BEFORE the prompt
	// text, matching the LangChain HumanMessage convention where images precede text.
	for _, att := range unreferencedBinaries {
		appendBinary(att.MimeType, att.Base64, "")
	}
	for _, img := range legacyImages {
		mimeType := img.MimeType
		if mimeType == "" {
			mimeType = "image/jpeg"
		}
		appendBinary(mimeType, img.Base64, "")
	}

	// Emit placeholder-positioned parts (text and referenced attachments)
	for _, p := range parts {
		if p.attachment != nil {
			appendBinary(p.attachment.MimeType, p.attachment.Base64, p.refName)
		} else {
			textAccum.WriteString(p.text)
		}
	}

	// Append unreferenced text attachments to the text accumulator
	for _, att := range unreferencedText {
		textAccum.WriteString(att.Text)
	}

	// Flush remaining text
	flushText()

	buf = append(buf, ']')
	return buf
}

// appendMultimodalContent appends a multimodal content array with text and image parts.
// Images are placed before the text part, matching the LangChain HumanMessage convention.
// If images is empty, appends the text as a simple JSON string (no array wrapper).
func appendMultimodalContent(buf []byte, text string, images []ImageData, _ string) []byte {
	if len(images) == 0 {
		buf = append(buf, '"')
		buf = appendJSONEscaped(buf, text)
		buf = append(buf, '"')
		return buf
	}

	// Start content array
	buf = append(buf, '[')

	// Add image parts first, matching the LangChain convention where images
	// precede the text prompt. Base64 is inherently JSON-safe ([A-Za-z0-9+/=]),
	// but mimeType is user-controlled so we escape it to prevent JSON injection.
	for i, img := range images {
		if i > 0 {
			buf = append(buf, ',')
		}
		mimeType := img.MimeType
		if mimeType == "" {
			mimeType = "image/jpeg"
		}
		buf = append(buf, `{"type":"image_url","image_url":{"url":"data:`...)
		buf = appendJSONEscaped(buf, mimeType)
		buf = append(buf, `;base64,`...)
		buf = append(buf, img.Base64...)
		buf = append(buf, `"}}`...)
	}

	// Add text part after images
	buf = append(buf, `,{"type":"text","text":"`...)
	buf = appendJSONEscaped(buf, text)
	buf = append(buf, `"}`...)

	// Close content array
	buf = append(buf, ']')

	return buf
}

// mapRoleToLangGraph maps OpenAI-style roles to LangGraph message types.
func mapRoleToLangGraph(role string) string {
	switch role {
	case "user":
		return "human"
	case "assistant":
		return "ai"
	case "system":
		return "system"
	default:
		return role
	}
}

// appendJSONValue appends a JSON-encoded value to buf.
// Handles strings, numbers, booleans, nil, and complex types (maps, slices, structs).
// Uses strconv.Append* functions for primitives to avoid allocations.
// Falls back to json.Marshal for complex types.
func appendJSONValue(buf []byte, v any) []byte {
	switch val := v.(type) {
	case string:
		buf = append(buf, '"')
		buf = appendJSONEscaped(buf, val)
		buf = append(buf, '"')
	case int:
		buf = strconv.AppendInt(buf, int64(val), 10)
	case int8:
		buf = strconv.AppendInt(buf, int64(val), 10)
	case int16:
		buf = strconv.AppendInt(buf, int64(val), 10)
	case int32:
		buf = strconv.AppendInt(buf, int64(val), 10)
	case int64:
		buf = strconv.AppendInt(buf, val, 10)
	case uint:
		buf = strconv.AppendUint(buf, uint64(val), 10)
	case uint8:
		buf = strconv.AppendUint(buf, uint64(val), 10)
	case uint16:
		buf = strconv.AppendUint(buf, uint64(val), 10)
	case uint32:
		buf = strconv.AppendUint(buf, uint64(val), 10)
	case uint64:
		buf = strconv.AppendUint(buf, val, 10)
	case float32:
		buf = strconv.AppendFloat(buf, float64(val), 'g', -1, 32)
	case float64:
		buf = strconv.AppendFloat(buf, val, 'g', -1, 64)
	case bool:
		if val {
			buf = append(buf, "true"...)
		} else {
			buf = append(buf, "false"...)
		}
	case nil:
		buf = append(buf, "null"...)
	default:
		// For complex types (maps, slices, structs), use json.Marshal
		jsonBytes, err := json.Marshal(val)
		if err != nil {
			slog.Warn("failed to marshal LangGraphInput value", "error", err)
			buf = append(buf, "null"...)
		} else {
			buf = append(buf, jsonBytes...)
		}
	}
	return buf
}
