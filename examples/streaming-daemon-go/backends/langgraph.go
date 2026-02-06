package backends

import (
	"bufio"
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

// LangGraph configuration (set during Init).
//
// Thread safety: Init() must be called exactly once at process startup,
// on a single goroutine, before any Stream() calls. These variables are
// then read-only during normal operation.
var (
	langgraphAPIBase      string
	langgraphAPIKey       string
	langgraphAssistantID  string
	langgraphStreamMode   string
	langgraphInsecure     bool
	langgraphHTTPClient   *http.Client
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
func (l *LangGraph) Init(cfg *config.BackendConfig) error {
	// Default values
	langgraphAPIBase = "https://api.langchain.com/v1"
	langgraphAssistantID = "agent"
	langgraphStreamMode = "messages-tuple"
	useHTTP2 := true
	var socketPath string

	// Apply config file values
	if cfg != nil {
		if cfg.LangGraph.APIBase != "" {
			langgraphAPIBase = cfg.LangGraph.APIBase
		}
		if cfg.LangGraph.APIKey != "" {
			langgraphAPIKey = cfg.LangGraph.APIKey
		}
		if cfg.LangGraph.APISocket != "" {
			socketPath = cfg.LangGraph.APISocket
		}
		if cfg.LangGraph.AssistantID != "" {
			langgraphAssistantID = cfg.LangGraph.AssistantID
		}
		if cfg.LangGraph.StreamMode != "" {
			langgraphStreamMode = cfg.LangGraph.StreamMode
		}
		if cfg.LangGraph.HTTP2Enabled != nil {
			useHTTP2 = *cfg.LangGraph.HTTP2Enabled
		}
		langgraphInsecure = cfg.LangGraph.InsecureSSL
	}

	// Environment variables override config file
	if envVal := os.Getenv("LANGGRAPH_API_BASE"); envVal != "" {
		langgraphAPIBase = envVal
	}
	if envVal := os.Getenv("LANGGRAPH_API_KEY"); envVal != "" {
		langgraphAPIKey = envVal
	}
	if envVal := os.Getenv("LANGGRAPH_API_SOCKET"); envVal != "" {
		socketPath = envVal
	}
	if envVal := os.Getenv("LANGGRAPH_ASSISTANT_ID"); envVal != "" {
		langgraphAssistantID = envVal
	}
	if envVal := os.Getenv("LANGGRAPH_STREAM_MODE"); envVal != "" {
		langgraphStreamMode = envVal
	}
	if envVal := os.Getenv("LANGGRAPH_HTTP2_ENABLED"); envVal != "" {
		isFalse := strings.EqualFold(envVal, "false") ||
			strings.EqualFold(envVal, "0") ||
			strings.EqualFold(envVal, "no") ||
			strings.EqualFold(envVal, "off")
		useHTTP2 = !isFalse
	}
	if envVal := os.Getenv("LANGGRAPH_INSECURE_SSL"); envVal != "" {
		langgraphInsecure = strings.EqualFold(envVal, "true") || strings.EqualFold(envVal, "1")
	}

	// Flags override everything (only if explicitly set)
	if *langgraphBaseFlag != "" {
		langgraphAPIBase = *langgraphBaseFlag
	}
	if *langgraphSocketFlag != "" {
		socketPath = *langgraphSocketFlag
	}
	if *langgraphAssistantFlag != "" {
		langgraphAssistantID = *langgraphAssistantFlag
	}

	// Validate API key
	if langgraphAPIKey == "" {
		slog.Warn("LANGGRAPH_API_KEY not set, API calls will fail")
	}

	if langgraphInsecure {
		slog.Warn("TLS certificate verification disabled for LangGraph")
	}

	// Create HTTP client with shared transport builder
	langgraphHTTPClient = NewHTTPClient(TransportConfig{
		BaseURL:     langgraphAPIBase,
		SocketPath:  socketPath,
		UseHTTP2:    useHTTP2,
		InsecureSSL: langgraphInsecure,
		Label:       "LangGraph",
	})

	return nil
}

// langgraphBufPool reuses buffers for building HTTP request bodies.
var langgraphBufPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 0, 1024)
		return &buf
	},
}

// Stream sends a request to the LangGraph API and streams the response to the client.
func (l *LangGraph) Stream(ctx context.Context, conn net.Conn, handoff HandoffData) (int64, error) {
	var totalBytes int64
	backendStart := time.Now()
	var ttfbRecorded bool

	// Determine assistant ID (handoff override > config)
	assistantID := langgraphAssistantID
	if handoff.AssistantID != "" {
		assistantID = handoff.AssistantID
	}

	// Build request body (shared with tests via buildLangGraphRequestBody)
	buf := buildLangGraphRequestBody(handoff, assistantID)

	// Determine endpoint: stateful (with thread_id) or stateless
	var reqURL string
	if handoff.ThreadID != "" {
		// URL-escape ThreadID to prevent path traversal attacks
		reqURL = fmt.Sprintf("%s/threads/%s/runs/stream", langgraphAPIBase, url.PathEscape(handoff.ThreadID))
	} else {
		reqURL = langgraphAPIBase + "/runs/stream"
	}

	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx, "POST", reqURL, bytes.NewReader(buf))
	if err != nil {
		return 0, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", langgraphAPIKey)

	// Add test pattern header if present (for validation testing)
	if handoff.TestPattern != "" {
		req.Header.Set("X-Test-Pattern", handoff.TestPattern)
	}

	// Record backend request attempt
	RecordBackendRequest("langgraph")

	// Make request
	resp, err := langgraphHTTPClient.Do(req)

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

	// Set write deadline once for entire stream
	if err := conn.SetWriteDeadline(time.Now().Add(WriteTimeout)); err != nil {
		return 0, fmt.Errorf("set write deadline: %w", err)
	}

	// Parse LangGraph SSE stream and forward to client
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, scannerInitialBufferSize), scannerMaxBufferSize)

	writeCount := 0
	for {
		eventType, data, err := parseLangGraphEvent(scanner)
		if err != nil {
			if err == io.EOF {
				break
			}
			return totalBytes, fmt.Errorf("parse event: %w", err)
		}

		// Handle different event types
		switch eventType {
		case "end":
			// Stream complete
			goto done
		case "messages":
			// Extract content from messages-tuple format
			content := extractContentFromMessages(data)
			if content != "" {
				// Record TTFB on first chunk
				if !ttfbRecorded {
					RecordBackendTTFB("langgraph", time.Since(backendStart).Seconds())
					ttfbRecorded = true
				}

				n, err := SendSSE(conn, content)
				totalBytes += int64(n)
				RecordChunkSent()
				if err != nil {
					return totalBytes, err
				}

				// Refresh write deadline every 10 writes
				writeCount++
				if writeCount%10 == 0 {
					if err := conn.SetWriteDeadline(time.Now().Add(WriteTimeout)); err != nil {
						return totalBytes, fmt.Errorf("set write deadline: %w", err)
					}
				}
			}
		case "metadata", "values", "updates":
			// These event types don't contain streamable content
			continue
		default:
			// Log unknown event types for forward compatibility debugging
			if eventType != "" {
				slog.Warn("LangGraph: unknown event type, skipping", "event_type", eventType)
			}
		}
	}

done:
	// Send completion marker
	doneMsg := []byte("data: [DONE]\n\n")
	n, err := conn.Write(doneMsg)
	totalBytes += int64(n)
	if err != nil {
		return totalBytes, fmt.Errorf("write done: %w", err)
	}

	// Record backend duration
	RecordBackendDuration("langgraph", time.Since(backendStart).Seconds())

	return totalBytes, nil
}

// parseLangGraphEvent parses a single LangGraph SSE event.
// LangGraph format: "event: type\ndata: json\n\n"
// Returns event type, data payload, and any error.
// Returns io.EOF when stream ends.
func parseLangGraphEvent(scanner *bufio.Scanner) (eventType string, data []byte, err error) {
	// Read event line
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return "", nil, err
		}
		return "", nil, io.EOF
	}
	line := scanner.Bytes()

	// Skip empty lines
	for len(line) == 0 {
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return "", nil, err
			}
			return "", nil, io.EOF
		}
		line = scanner.Bytes()
	}

	// Parse event type from "event: type" line
	if bytes.HasPrefix(line, []byte("event:")) {
		eventType = strings.TrimSpace(string(line[6:]))
	} else {
		// Fail fast on protocol violations to catch integration issues early
		return "", nil, fmt.Errorf("unexpected event line format: %q", string(line))
	}

	// Read data line
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return "", nil, err
		}
		return eventType, nil, nil
	}
	line = scanner.Bytes()

	// Parse data from "data: json" line
	if bytes.HasPrefix(line, []byte("data:")) {
		dataStr := bytes.TrimSpace(line[5:])
		data = make([]byte, len(dataStr))
		copy(data, dataStr)
	}

	return eventType, data, nil
}

// extractContentFromMessages extracts content from LangGraph messages-tuple format.
// Format: [{"content": "...", "type": "AIMessageChunk", ...}, {"run_id": "..."}]
// Returns the content string or empty if not found.
func extractContentFromMessages(data []byte) string {
	// Fast path: look for "content":" pattern
	idx := bytes.Index(data, contentFieldPattern)
	if idx < 0 {
		return ""
	}

	// Find the start of the content value
	start := idx + len(contentFieldPattern)
	if start >= len(data) {
		return ""
	}

	// Find the end of the string (unescaped quote)
	end := start
	for end < len(data) {
		if data[end] == '\\' {
			if end+1 >= len(data) {
				break
			}
			next := data[end+1]
			if next == '"' || next == '\\' || next == '/' ||
				next == 'b' || next == 'f' || next == 'n' ||
				next == 'r' || next == 't' || next == 'u' {
				end += 2
				continue
			}
			end++
			continue
		}
		if data[end] == '"' {
			break
		}
		end++
	}

	if end <= start {
		return ""
	}

	// Unescape the content if it contains escapes
	content := data[start:end]
	if bytes.IndexByte(content, '\\') >= 0 {
		return unescapeJSON(content)
	}
	return string(content)
}

// buildLangGraphRequestBody builds the JSON request body for a LangGraph API call.
// Used by both Stream() and tests.
func buildLangGraphRequestBody(handoff HandoffData, assistantID string) []byte {
	bufPtr := langgraphBufPool.Get().(*[]byte)
	buf := (*bufPtr)[:0]

	buf = append(buf, `{"assistant_id":"`...)
	buf = appendJSONEscaped(buf, assistantID)
	buf = append(buf, `","input":{"messages":[`...)

	// Use messages array if provided, otherwise fall back to single prompt
	if len(handoff.Messages) > 0 {
		for i, msg := range handoff.Messages {
			if i > 0 {
				buf = append(buf, ',')
			}
			buf = append(buf, `{"type":"`...)
			buf = appendJSONEscaped(buf, mapRoleToLangGraph(msg.Role))
			buf = append(buf, `","content":"`...)
			buf = appendJSONEscaped(buf, msg.Content)
			buf = append(buf, `"}`...)
		}
	} else {
		prompt := handoff.Prompt
		if prompt == "" {
			prompt = "Hello"
		}
		buf = append(buf, `{"type":"human","content":"`...)
		buf = appendJSONEscaped(buf, prompt)
		buf = append(buf, `"}`...)
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

	buf = append(buf, `},"stream_mode":["`...)
	buf = appendJSONEscaped(buf, langgraphStreamMode)
	buf = append(buf, `"],"stream_subgraphs":false,"on_completion":"delete","on_disconnect":"cancel"}`...)

	// Return buffer to pool (make a copy since we're returning the content for tests)
	result := make([]byte, len(buf))
	copy(result, buf)
	*bufPtr = buf
	langgraphBufPool.Put(bufPtr)

	return result
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
