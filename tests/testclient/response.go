package testclient

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
	"unicode/utf8"
)

// Response represents a parsed SSE response from a streaming daemon.
type Response struct {
	// StatusLine is the HTTP status line (e.g., "HTTP/1.1 200 OK")
	StatusLine string
	// StatusCode extracted from status line
	StatusCode int
	// Headers is the HTTP headers
	Headers map[string]string
	// Chunks is the list of SSE data chunks received
	Chunks []SSEChunk
	// HasDoneMarker indicates if [DONE] was received
	HasDoneMarker bool
	// FinishReason is the finish_reason from the final chunk (if any)
	FinishReason string
	// RawContent is all content deltas concatenated
	RawContent string
	// TTFB is time to first byte
	TTFB time.Duration
	// TotalDuration is total time to receive the response
	TotalDuration time.Duration
	// TotalBytes is total bytes received
	TotalBytes int
	// Error if reading failed
	Error error
}

// SSEChunk represents a single SSE data chunk.
type SSEChunk struct {
	// Raw is the raw SSE line (including "data: " prefix)
	Raw string
	// Data is the JSON data (without "data: " prefix)
	Data string
	// Parsed is the parsed JSON (if valid)
	Parsed *ChatCompletionChunk
	// IsDone is true if this is the [DONE] marker
	IsDone bool
}

// ChatCompletionChunk represents a parsed chat completion chunk.
type ChatCompletionChunk struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
}

// Choice represents a choice in a chat completion chunk.
type Choice struct {
	Index        int    `json:"index"`
	Delta        Delta  `json:"delta"`
	FinishReason string `json:"finish_reason"`
}

// Delta represents the delta content in a choice.
type Delta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

// SimplifiedChunk is the format used by some daemons (e.g., Rust daemon).
// Format: {"content": "..."}
type SimplifiedChunk struct {
	Content string `json:"content,omitempty"`
	Error   string `json:"error,omitempty"`
}

// ReadResponse reads and parses an HTTP response with SSE content from a connection.
func ReadResponse(conn net.Conn) *Response {
	resp := &Response{
		Headers: make(map[string]string),
		Chunks:  make([]SSEChunk, 0),
	}

	reader := bufio.NewReader(conn)
	startTime := time.Now()
	var firstByteTime time.Time
	gotFirstByte := false

	// Read status line
	line, err := reader.ReadString('\n')
	if err != nil {
		resp.Error = fmt.Errorf("read status line: %w", err)
		return resp
	}

	if !gotFirstByte {
		firstByteTime = time.Now()
		gotFirstByte = true
	}

	resp.TotalBytes += len(line)
	resp.StatusLine = strings.TrimSpace(line)
	resp.StatusCode = parseStatusCode(resp.StatusLine)

	// Read headers until blank line
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			resp.Error = fmt.Errorf("read header: %w", err)
			return resp
		}
		resp.TotalBytes += len(line)

		line = strings.TrimSpace(line)
		if line == "" {
			break // End of headers
		}

		// Parse header (normalize key to canonical form for case-insensitive matching)
		if idx := strings.Index(line, ":"); idx > 0 {
			key := strings.TrimSpace(line[:idx])
			value := strings.TrimSpace(line[idx+1:])
			// Use canonical HTTP header casing (e.g., "content-type" -> "Content-Type")
			resp.Headers[canonicalHeaderKey(key)] = value
		}
	}

	// Read SSE body
	var contentBuilder strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err != io.EOF {
				resp.Error = fmt.Errorf("read body: %w", err)
			}
			break
		}
		resp.TotalBytes += len(line)

		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			continue // SSE event separator
		}

		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			chunk := SSEChunk{
				Raw:  line,
				Data: data,
			}

			if data == "[DONE]" {
				chunk.IsDone = true
				resp.HasDoneMarker = true
			} else {
				// Try to parse JSON - first try OpenAI format, then simplified format
				var parsed ChatCompletionChunk
				if err := json.Unmarshal([]byte(data), &parsed); err == nil && len(parsed.Choices) > 0 {
					// OpenAI format: {"choices":[{"delta":{"content":"..."}}]}
					chunk.Parsed = &parsed
					for _, choice := range parsed.Choices {
						if choice.Delta.Content != "" {
							contentBuilder.WriteString(choice.Delta.Content)
						}
						if choice.FinishReason != "" {
							resp.FinishReason = choice.FinishReason
						}
					}
				} else {
					// Try simplified format: {"content":"..."} or {"error":"..."}
					var simplified SimplifiedChunk
					if err := json.Unmarshal([]byte(data), &simplified); err == nil {
						if simplified.Error != "" {
							// Map error chunk to response error
							resp.Error = fmt.Errorf("daemon error: %s", simplified.Error)
						}
						if simplified.Content != "" {
							contentBuilder.WriteString(simplified.Content)
							// Create a synthetic parsed chunk for compatibility
							chunk.Parsed = &ChatCompletionChunk{
								Choices: []Choice{{
									Delta: Delta{Content: simplified.Content},
								}},
							}
						}
					}
				}
			}

			resp.Chunks = append(resp.Chunks, chunk)
		}
	}

	resp.RawContent = contentBuilder.String()
	resp.TotalDuration = time.Since(startTime)
	if gotFirstByte {
		resp.TTFB = firstByteTime.Sub(startTime)
	}

	return resp
}

// parseStatusCode extracts the status code from a status line.
func parseStatusCode(statusLine string) int {
	// Format: HTTP/1.1 200 OK
	parts := strings.SplitN(statusLine, " ", 3)
	if len(parts) < 2 {
		return 0
	}
	var code int
	fmt.Sscanf(parts[1], "%d", &code)
	return code
}

// canonicalHeaderKey converts a header key to canonical HTTP format.
// For example: "content-type" -> "Content-Type", "CONTENT-TYPE" -> "Content-Type"
func canonicalHeaderKey(key string) string {
	// Split by hyphen and capitalize first letter of each word
	parts := strings.Split(strings.ToLower(key), "-")
	for i, part := range parts {
		if len(part) > 0 {
			parts[i] = strings.ToUpper(part[:1]) + part[1:]
		}
	}
	return strings.Join(parts, "-")
}

// ContentChunks returns only the content chunks (not [DONE] marker).
func (r *Response) ContentChunks() []SSEChunk {
	result := make([]SSEChunk, 0, len(r.Chunks))
	for _, chunk := range r.Chunks {
		if !chunk.IsDone {
			result = append(result, chunk)
		}
	}
	return result
}

// IsValidUTF8 returns true if the raw content is valid UTF-8.
func (r *Response) IsValidUTF8() bool {
	return utf8.ValidString(r.RawContent)
}

// ChunkCount returns the number of content chunks (excluding [DONE]).
func (r *Response) ChunkCount() int {
	count := 0
	for _, chunk := range r.Chunks {
		if !chunk.IsDone && chunk.Parsed != nil {
			count++
		}
	}
	return count
}
