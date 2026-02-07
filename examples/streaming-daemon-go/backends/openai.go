package backends

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"examples/config"
)

// OpenAI backend configuration flags (can override config file)
var (
	openaiBaseFlag = flag.String("openai-base", "",
		"OpenAI API base URL (overrides config file)")
	openaiSocketFlag = flag.String("openai-socket", "",
		"Unix socket for OpenAI API (overrides config file)")
	http2EnabledFlag = flag.Bool("http2-enabled", true,
		"Enable HTTP/2 for upstream API (overrides config file)")
)

// SSE scanner buffer sizes for parsing upstream API responses.
const (
	scannerInitialBufferSize = 4096  // Initial buffer for bufio.Scanner
	scannerMaxBufferSize     = 65536 // Maximum buffer for bufio.Scanner (64KB)
)

// Package-level byte slices to avoid allocation in hot paths
var (
	contentFieldPattern = []byte(`"content":"`)
	dataPrefix          = []byte("data: ")
	doneMarker          = []byte("[DONE]")
)

// OpenAI configuration (set during Init).
//
// Thread safety: Init() must be called exactly once at process startup,
// on a single goroutine, before any Stream() calls. These variables are
// then read-only during normal operation. Concurrent or repeated calls
// to Init() are not supported and may cause data races.
var (
	openaiAPIBase      string
	openaiAPIKey       string
	openaiInsecure     bool
	openaiDefaultModel string
	httpClient         *http.Client
)

// OpenAI is a backend that streams responses from an OpenAI-compatible API.
type OpenAI struct{}

func init() {
	Register(&OpenAI{})
}

func (o *OpenAI) Name() string {
	return "openai"
}

func (o *OpenAI) Description() string {
	return "OpenAI-compatible streaming API (GPT-4, etc.)"
}

// Init sets up the HTTP client for OpenAI API calls.
// Configuration priority: flag > env var > config file > default
func (o *OpenAI) Init(cfg *config.BackendConfig) error {
	// Default values
	openaiAPIBase = "https://api.openai.com/v1"
	openaiDefaultModel = "gpt-4o-mini"
	useHTTP2 := true
	var socketPath string

	// Apply config file values
	if cfg != nil {
		if cfg.DefaultModel != "" {
			openaiDefaultModel = cfg.DefaultModel
		}
		if cfg.OpenAI.APIBase != "" {
			openaiAPIBase = cfg.OpenAI.APIBase
		}
		if cfg.OpenAI.APIKey != "" {
			openaiAPIKey = cfg.OpenAI.APIKey
		}
		if cfg.OpenAI.APISocket != "" {
			socketPath = cfg.OpenAI.APISocket
		}
		if cfg.OpenAI.HTTP2Enabled != nil {
			useHTTP2 = *cfg.OpenAI.HTTP2Enabled
		}
		openaiInsecure = cfg.OpenAI.InsecureSSL
	}

	// Environment variables override config file
	if envVal := os.Getenv("OPENAI_API_BASE"); envVal != "" {
		openaiAPIBase = envVal
	}
	if envVal := os.Getenv("OPENAI_API_KEY"); envVal != "" {
		openaiAPIKey = envVal
	}
	if envVal := os.Getenv("OPENAI_API_SOCKET"); envVal != "" {
		socketPath = envVal
	}
	if envVal := os.Getenv("OPENAI_HTTP2_ENABLED"); envVal != "" {
		isFalse := strings.EqualFold(envVal, "false") ||
			strings.EqualFold(envVal, "0") ||
			strings.EqualFold(envVal, "no") ||
			strings.EqualFold(envVal, "off")
		useHTTP2 = !isFalse
	}
	if envVal := os.Getenv("OPENAI_INSECURE_SSL"); envVal != "" {
		openaiInsecure = strings.EqualFold(envVal, "true") ||
			strings.EqualFold(envVal, "1")
	}

	// Flags override everything (only if explicitly set)
	if *openaiBaseFlag != "" {
		openaiAPIBase = *openaiBaseFlag
	}
	if *openaiSocketFlag != "" {
		socketPath = *openaiSocketFlag
	}
	// Note: http2EnabledFlag defaults to true, so we can't detect if it was explicitly set
	// Use the flag value directly (it will match default unless user changed it)
	if !*http2EnabledFlag {
		useHTTP2 = false
	}

	// Validate API key
	if openaiAPIKey == "" {
		slog.Warn("OPENAI_API_KEY not set, API calls will fail")
	}

	if openaiInsecure {
		slog.Warn("TLS certificate verification disabled")
	}

	// Create HTTP client with shared transport builder
	httpClient = NewHTTPClient(TransportConfig{
		BaseURL:     openaiAPIBase,
		SocketPath:  socketPath,
		UseHTTP2:    useHTTP2,
		InsecureSSL: openaiInsecure,
		Label:       "OpenAI",
	})

	return nil
}

// requestBufPool reuses buffers for building HTTP request bodies.
var requestBufPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 0, 512)
		return &buf
	},
}

// Stream sends a request to the OpenAI API and streams the response to the client.
func (o *OpenAI) Stream(ctx context.Context, conn net.Conn, handoff HandoffData) (int64, error) {
	var totalBytes int64
	backendStart := time.Now()
	var ttfbRecorded bool

	model := handoff.Model
	if model == "" {
		model = openaiDefaultModel
	}

	// Get buffer from pool and build request JSON manually
	bufPtr := requestBufPool.Get().(*[]byte)
	buf := (*bufPtr)[:0]
	buf = append(buf, `{"model":"`...)
	buf = appendJSONEscaped(buf, model)
	buf = append(buf, `","messages":[`...)

	// Use messages array if provided, otherwise fall back to single prompt
	if len(handoff.Messages) > 0 {
		for i, msg := range handoff.Messages {
			if i > 0 {
				buf = append(buf, ',')
			}
			buf = append(buf, `{"role":"`...)
			buf = appendJSONEscaped(buf, msg.Role)
			buf = append(buf, `","content":"`...)
			buf = appendJSONEscaped(buf, msg.Content)
			buf = append(buf, `"}`...)
		}
	} else {
		// Legacy: single prompt mode
		prompt := handoff.Prompt
		if prompt == "" {
			prompt = "Hello"
		}
		buf = append(buf, `{"role":"user","content":"`...)
		buf = appendJSONEscaped(buf, prompt)
		buf = append(buf, `"}`...)
	}

	buf = append(buf, `],"stream":true}`...)

	// Create HTTP request
	url := openaiAPIBase + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(buf))
	if err != nil {
		*bufPtr = buf
		requestBufPool.Put(bufPtr)
		return 0, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+openaiAPIKey)

	// Add test pattern header if present (for validation testing)
	if handoff.TestPattern != "" {
		req.Header.Set("X-Test-Pattern", handoff.TestPattern)
	}

	// Record backend request attempt (before the call, so all attempts are counted)
	RecordBackendRequest("openai")

	// Make request
	resp, err := httpClient.Do(req)

	// Return buffer to pool after request is sent
	*bufPtr = buf
	requestBufPool.Put(bufPtr)

	if err != nil {
		RecordBackendError("openai")
		return 0, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		RecordBackendError("openai")
		// Limit error body size to prevent memory exhaustion from large error payloads
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return 0, fmt.Errorf("API error %d: %s", resp.StatusCode, string(bodyBytes))
	}

	// Set write deadline once for entire stream (refreshed periodically below)
	if err := conn.SetWriteDeadline(time.Now().Add(WriteTimeout)); err != nil {
		return 0, fmt.Errorf("set write deadline: %w", err)
	}

	// Parse SSE stream and forward to client
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, scannerInitialBufferSize), scannerMaxBufferSize)

	writeCount := 0
	for scanner.Scan() {
		line := scanner.Bytes()

		// Skip empty lines
		if len(line) == 0 {
			continue
		}

		// Check for data: prefix using bytes comparison (no allocation)
		if !bytes.HasPrefix(line, dataPrefix) {
			continue
		}

		data := line[6:] // Skip "data: " prefix

		// Check for [DONE] marker using bytes comparison (no allocation)
		if bytes.Equal(data, doneMarker) {
			break
		}

		// Fast-path content extraction without full JSON parsing
		content := extractContentFast(data)
		if content != "" {
			// Record TTFB on first chunk
			if !ttfbRecorded {
				RecordBackendTTFB("openai", time.Since(backendStart).Seconds())
				ttfbRecorded = true
			}

			n, err := SendSSE(conn, content)
			totalBytes += int64(n)
			RecordChunkSent()
			if err != nil {
				return totalBytes, err
			}

			// Refresh write deadline every 10 writes instead of every write
			writeCount++
			if writeCount%10 == 0 {
				if err := conn.SetWriteDeadline(time.Now().Add(WriteTimeout)); err != nil {
					return totalBytes, fmt.Errorf("set write deadline: %w", err)
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return totalBytes, fmt.Errorf("read stream: %w", err)
	}

	// Send completion marker
	n, err := conn.Write(doneMsg)
	totalBytes += int64(n)
	if err != nil {
		return totalBytes, fmt.Errorf("write done: %w", err)
	}

	// Record backend duration
	RecordBackendDuration("openai", time.Since(backendStart).Seconds())

	return totalBytes, nil
}

// extractContentFast extracts the "content" field from an OpenAI SSE chunk
// without full JSON parsing. Returns empty string if not found.
func extractContentFast(data []byte) string {
	// Look for "content":" pattern
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

	if end < start {
		return ""
	}

	// Unescape the content if it contains escapes
	content := data[start:end]
	if bytes.IndexByte(content, '\\') >= 0 {
		return unescapeJSON(content)
	}
	return string(content)
}

// unescapeJSON unescapes a JSON string value, including surrogate pairs.
func unescapeJSON(b []byte) string {
	if bytes.IndexByte(b, '\\') < 0 {
		return string(b)
	}

	result := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if b[i] == '\\' && i+1 < len(b) {
			i++
			switch b[i] {
			case 'n':
				result = append(result, '\n')
			case 'r':
				result = append(result, '\r')
			case 't':
				result = append(result, '\t')
			case 'b':
				result = append(result, '\b')
			case 'f':
				result = append(result, '\f')
			case '"', '\\', '/':
				result = append(result, b[i])
			case 'u':
				if i+4 > len(b) {
					break
				}
				hexStr := string(b[i+1 : i+5])
				codepoint, parseErr := strconv.ParseUint(hexStr, 16, 32)
				if parseErr != nil {
					break
				}
				if codepoint >= 0xD800 && codepoint <= 0xDBFF {
					// High surrogate — look for low surrogate \uXXXX
					if i+10 < len(b) && b[i+5] == '\\' && b[i+6] == 'u' {
						loHex := string(b[i+7 : i+11])
						if lo, loErr := strconv.ParseUint(loHex, 16, 32); loErr == nil && lo >= 0xDC00 && lo <= 0xDFFF {
							combined := 0x10000 + (codepoint-0xD800)*0x400 + (lo - 0xDC00)
							var ubuf [4]byte
							n := utf8.EncodeRune(ubuf[:], rune(combined))
							result = append(result, ubuf[:n]...)
							i += 10
							break
						}
					}
					// Orphan high surrogate — emit replacement character
					var ubuf [4]byte
					n := utf8.EncodeRune(ubuf[:], utf8.RuneError)
					result = append(result, ubuf[:n]...)
					i += 4
				} else if codepoint >= 0xDC00 && codepoint <= 0xDFFF {
					// Orphan low surrogate — emit replacement character
					var ubuf [4]byte
					n := utf8.EncodeRune(ubuf[:], utf8.RuneError)
					result = append(result, ubuf[:n]...)
					i += 4
				} else {
					var ubuf [4]byte
					n := utf8.EncodeRune(ubuf[:], rune(codepoint))
					result = append(result, ubuf[:n]...)
					i += 4
				}
			default:
				result = append(result, b[i])
			}
		} else {
			result = append(result, b[i])
		}
	}
	return string(result)
}
