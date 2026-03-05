package backends

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestBuildLangGraphRequestBody(t *testing.T) {
	tests := []struct {
		name             string
		handoff          HandoffData
		assistantID      string
		defaultStreamMode string
		wantContains     []string
	}{
		{
			name: "single prompt",
			handoff: HandoffData{
				Prompt: "Hello, how are you?",
			},
			assistantID:      "test-agent",
			defaultStreamMode: "messages-tuple",
			wantContains: []string{
				`"assistant_id":"test-agent"`,
				`"type":"human"`,
				`"content":"Hello, how are you?"`,
				`"stream_mode":[`,
				`"on_completion":"delete"`,
				`"on_disconnect":"cancel"`,
			},
		},
		{
			name: "messages array",
			handoff: HandoffData{
				Messages: []Message{
					{Role: "user", Content: "What is 2+2?"},
					{Role: "assistant", Content: "4"},
					{Role: "user", Content: "And 3+3?"},
				},
			},
			assistantID:      "math-agent",
			defaultStreamMode: "messages-tuple",
			wantContains: []string{
				`"assistant_id":"math-agent"`,
				`"type":"human","content":"What is 2+2?"`,
				`"type":"ai","content":"4"`,
				`"type":"human","content":"And 3+3?"`,
			},
		},
		{
			name: "with langgraph input",
			handoff: HandoffData{
				Prompt: "Test",
				LangGraphInput: map[string]any{
					"seller_id": "seller123",
					"shop_id":   int64(456),
				},
			},
			assistantID:      "shop-agent",
			defaultStreamMode: "messages-tuple",
			wantContains: []string{
				`"seller_id":"seller123"`,
				`"shop_id":456`,
			},
		},
		{
			name:             "empty prompt uses default",
			handoff:          HandoffData{},
			assistantID:      "agent",
			defaultStreamMode: "messages-tuple",
			wantContains: []string{
				`"content":"Hello"`,
			},
		},
		{
			name: "system role preserved",
			handoff: HandoffData{
				Messages: []Message{
					{Role: "system", Content: "You are helpful."},
					{Role: "user", Content: "Hi"},
				},
			},
			assistantID:      "agent",
			defaultStreamMode: "messages-tuple",
			wantContains: []string{
				`"type":"system","content":"You are helpful."`,
				`"type":"human","content":"Hi"`,
			},
		},
		{
			name: "multimodal content with image (single prompt)",
			handoff: HandoffData{
				Prompt:        "What's in this image?",
				ImageBase64:   "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==",
				ImageMimeType: "image/png",
			},
			assistantID:      "vision-agent",
			defaultStreamMode: "messages-tuple",
			wantContains: []string{
				`"assistant_id":"vision-agent"`,
				`"type":"human"`,
				`"content":[`,
				`{"type":"text","text":"What's in this image?"}`,
				`{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="}}`,
			},
		},
		{
			name: "multimodal content with image (messages array)",
			handoff: HandoffData{
				Messages: []Message{
					{Role: "user", Content: "Hello"},
					{Role: "assistant", Content: "Hi there!"},
					{Role: "user", Content: "Describe this image"},
				},
				ImageBase64:   "SGVsbG8gV29ybGQ=",
				ImageMimeType: "image/jpeg",
			},
			assistantID:      "agent",
			defaultStreamMode: "messages-tuple",
			wantContains: []string{
				`"type":"human","content":"Hello"`,
				`"type":"ai","content":"Hi there!"`,
				`"type":"human","content":[`,
				`{"type":"text","text":"Describe this image"}`,
				`{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,SGVsbG8gV29ybGQ="}}`,
			},
		},
		{
			name: "multimodal content defaults to jpeg mime type",
			handoff: HandoffData{
				Prompt:      "Analyze",
				ImageBase64: "dGVzdA==",
			},
			assistantID:      "agent",
			defaultStreamMode: "messages-tuple",
			wantContains: []string{
				`"content":[`,
				`{"type":"text","text":"Analyze"}`,
				`{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,dGVzdA=="}}`,
			},
		},
		{
			name: "custom stream_mode single",
			handoff: HandoffData{
				Prompt:     "test",
				StreamMode: []string{"events"},
			},
			assistantID:      "agent",
			defaultStreamMode: "messages-tuple",
			wantContains: []string{
				`"stream_mode":["events"]`,
			},
		},
		{
			name: "custom stream_mode multiple",
			handoff: HandoffData{
				Prompt:     "test",
				StreamMode: []string{"messages", "updates"},
			},
			assistantID:      "agent",
			defaultStreamMode: "messages-tuple",
			wantContains: []string{
				`"stream_mode":["messages","updates"]`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildLangGraphRequestBody(tt.handoff, tt.assistantID, tt.defaultStreamMode)
			gotStr := string(got)

			for _, want := range tt.wantContains {
				if !strings.Contains(gotStr, want) {
					t.Errorf("buildLangGraphRequestBody() missing %q in %s", want, gotStr)
				}
			}
		})
	}
}

func TestMapRoleToLangGraph(t *testing.T) {
	tests := []struct {
		role string
		want string
	}{
		{"user", "human"},
		{"assistant", "ai"},
		{"system", "system"},
		{"custom", "custom"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.role, func(t *testing.T) {
			got := mapRoleToLangGraph(tt.role)
			if got != tt.want {
				t.Errorf("mapRoleToLangGraph(%q) = %q, want %q", tt.role, got, tt.want)
			}
		})
	}
}

func TestAppendJSONValue(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  string
	}{
		{"string", "hello", `"hello"`},
		{"string with quotes", "say \"hi\"", `"say \"hi\""`},
		{"int", 42, "42"},
		{"int64", int64(123456789), "123456789"},
		{"float64", 3.14, "3.14"},
		{"bool true", true, "true"},
		{"bool false", false, "false"},
		{"nil", nil, "null"},
		{"slice", []string{"a", "b"}, `["a","b"]`},
		{"map", map[string]int{"x": 1}, `{"x":1}`},
		{"nested map", map[string]any{"key": map[string]int{"nested": 42}}, `{"key":{"nested":42}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := appendJSONValue(nil, tt.value)
			got := string(buf)
			if got != tt.want {
				t.Errorf("appendJSONValue() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEnsureThreadExists(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		wantErr    bool
	}{
		{"200 OK", http.StatusOK, false},
		{"201 Created", http.StatusCreated, false},
		{"409 Conflict", http.StatusConflict, false},
		{"500 Internal Server Error", http.StatusInternalServerError, true},
		{"404 Not Found", http.StatusNotFound, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
			}))
			defer srv.Close()

			p := &langgraphProfile{
				apiBase:    srv.URL,
				apiKey:     "test-key",
				httpClient: srv.Client(),
			}

			err := ensureThreadExists(context.Background(), p, "test-thread-123")
			if (err != nil) != tt.wantErr {
				t.Errorf("ensureThreadExists() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestLangGraphStreamProxy(t *testing.T) {
	// Simulate a LangGraph SSE upstream that sends known data
	ssePayload := "event: metadata\ndata: {\"run_id\":\"abc\"}\n\nevent: messages\ndata: [{\"content\":\"Hello\"}]\n\nevent: end\ndata: null\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(ssePayload))
	}))
	defer srv.Close()

	// Override the default profile for the test
	origDefault := langgraphDefault
	origProfiles := langgraphProfiles
	langgraphDefault = &langgraphProfile{
		apiBase:     srv.URL,
		apiKey:      "test-key",
		assistantID: "agent",
		streamMode:  "messages-tuple",
		httpClient:  srv.Client(),
	}
	langgraphProfiles = make(map[string]*langgraphProfile)
	defer func() {
		langgraphDefault = origDefault
		langgraphProfiles = origProfiles
	}()

	// Create a pipe to act as the client connection
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	// Read from the client side in a goroutine
	var received bytes.Buffer
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(&received, clientConn)
		done <- err
	}()

	lg := &LangGraph{}
	handoff := HandoffData{Prompt: "test"}
	n, err := lg.Stream(context.Background(), serverConn, handoff)
	serverConn.Close() // signal EOF to reader

	<-done

	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	if n != int64(len(ssePayload)) {
		t.Errorf("Stream() returned %d bytes, want %d", n, len(ssePayload))
	}
	if received.String() != ssePayload {
		t.Errorf("Stream() proxied data mismatch:\ngot:  %q\nwant: %q", received.String(), ssePayload)
	}
}

func TestLangGraphStreamProxyWithThreadID(t *testing.T) {
	ssePayload := "event: messages\ndata: [{\"content\":\"Hi\"}]\n\nevent: end\ndata: null\n\n"

	var requestPaths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestPaths = append(requestPaths, r.URL.Path)
		if r.URL.Path == "/threads" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(ssePayload))
	}))
	defer srv.Close()

	origDefault := langgraphDefault
	origProfiles := langgraphProfiles
	langgraphDefault = &langgraphProfile{
		apiBase:     srv.URL,
		apiKey:      "test-key",
		assistantID: "agent",
		streamMode:  "messages-tuple",
		httpClient:  srv.Client(),
	}
	langgraphProfiles = make(map[string]*langgraphProfile)
	defer func() {
		langgraphDefault = origDefault
		langgraphProfiles = origProfiles
	}()

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	var received bytes.Buffer
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(&received, clientConn)
		done <- err
	}()

	lg := &LangGraph{}
	handoff := HandoffData{Prompt: "test", ThreadID: "thread-abc"}
	_, err := lg.Stream(context.Background(), serverConn, handoff)
	serverConn.Close()
	<-done

	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	// Verify thread creation was called first, then streaming
	if len(requestPaths) < 2 {
		t.Fatalf("expected at least 2 requests, got %d: %v", len(requestPaths), requestPaths)
	}
	if requestPaths[0] != "/threads" {
		t.Errorf("first request path = %q, want /threads", requestPaths[0])
	}
	if requestPaths[1] != "/threads/thread-abc/runs/stream" {
		t.Errorf("second request path = %q, want /threads/thread-abc/runs/stream", requestPaths[1])
	}
	if received.String() != ssePayload {
		t.Errorf("Stream() proxied data mismatch:\ngot:  %q\nwant: %q", received.String(), ssePayload)
	}
}

func TestLangGraphStreamProxyContextCanceled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("event: metadata\ndata: {}\n\n"))
	}))
	defer srv.Close()

	origDefault := langgraphDefault
	origProfiles := langgraphProfiles
	langgraphDefault = &langgraphProfile{
		apiBase:     srv.URL,
		apiKey:      "test-key",
		assistantID: "agent",
		streamMode:  "messages-tuple",
		httpClient:  srv.Client(),
	}
	langgraphProfiles = make(map[string]*langgraphProfile)
	defer func() {
		langgraphDefault = origDefault
		langgraphProfiles = origProfiles
	}()

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	go func() { io.Copy(io.Discard, clientConn) }()

	// Cancel context before calling Stream
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	lg := &LangGraph{}
	_, err := lg.Stream(ctx, serverConn, HandoffData{Prompt: "test"})
	if err == nil {
		t.Error("Stream() error = nil, want non-nil for canceled context")
	}
}

func TestLangGraphStreamChunkCounting(t *testing.T) {
	tests := []struct {
		name       string
		payload    string
		wantChunks int
	}{
		{
			name:       "LF line endings",
			payload:    "event: metadata\ndata: {}\n\nevent: messages\ndata: []\n\nevent: end\ndata: null\n\n",
			wantChunks: 3,
		},
		{
			name:       "CRLF line endings",
			payload:    "event: metadata\r\ndata: {}\r\n\r\nevent: messages\r\ndata: []\r\n\r\nevent: end\r\ndata: null\r\n\r\n",
			wantChunks: 3,
		},
		{
			name:       "single event",
			payload:    "data: hello\n\n",
			wantChunks: 1,
		},
		{
			name:       "no blank lines",
			payload:    "data: hello\n",
			wantChunks: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(tt.payload))
			}))
			defer srv.Close()

			origDefault := langgraphDefault
			origProfiles := langgraphProfiles
			langgraphDefault = &langgraphProfile{
				apiBase:     srv.URL,
				apiKey:      "test-key",
				assistantID: "agent",
				streamMode:  "messages-tuple",
				httpClient:  srv.Client(),
			}
			langgraphProfiles = make(map[string]*langgraphProfile)
			defer func() {
				langgraphDefault = origDefault
				langgraphProfiles = origProfiles
			}()

			clientConn, serverConn := net.Pipe()
			defer clientConn.Close()
			defer serverConn.Close()

			go func() { io.Copy(io.Discard, clientConn) }()

			before := testutil.ToFloat64(metricChunksSent)

			lg := &LangGraph{}
			_, err := lg.Stream(context.Background(), serverConn, HandoffData{Prompt: "test"})
			if err != nil {
				t.Fatalf("Stream() error = %v", err)
			}

			after := testutil.ToFloat64(metricChunksSent)
			got := int(after - before)
			if got != tt.wantChunks {
				t.Errorf("chunks sent = %d, want %d", got, tt.wantChunks)
			}
		})
	}
}

func TestLangGraphStreamProxyMalformedSSE(t *testing.T) {
	// Malformed data; proxy should relay bytes transparently
	malformed := "this is not valid SSE\njust some random text\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(malformed))
	}))
	defer srv.Close()

	origDefault := langgraphDefault
	origProfiles := langgraphProfiles
	langgraphDefault = &langgraphProfile{
		apiBase:     srv.URL,
		apiKey:      "test-key",
		assistantID: "agent",
		streamMode:  "messages-tuple",
		httpClient:  srv.Client(),
	}
	langgraphProfiles = make(map[string]*langgraphProfile)
	defer func() {
		langgraphDefault = origDefault
		langgraphProfiles = origProfiles
	}()

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	var received bytes.Buffer
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(&received, clientConn)
		done <- err
	}()

	lg := &LangGraph{}
	n, err := lg.Stream(context.Background(), serverConn, HandoffData{Prompt: "test"})
	serverConn.Close()
	<-done

	if err != nil {
		t.Fatalf("Stream() error = %v, want nil for malformed but proxied SSE", err)
	}
	if n != int64(len(malformed)) {
		t.Errorf("Stream() bytes = %d, want %d", n, len(malformed))
	}
	if received.String() != malformed {
		t.Errorf("Stream() proxied data mismatch:\ngot:  %q\nwant: %q", received.String(), malformed)
	}
}

func TestParseLG(t *testing.T) {
	tests := []struct {
		input               string
		wantProfile, wantURL, wantKey string
	}{
		{"sales", "sales", "", ""},
		{"sales|https://api.example.com", "sales", "https://api.example.com", ""},
		{"sales|https://api.example.com|lgk_key", "sales", "https://api.example.com", "lgk_key"},
		{"sales||lgk_key", "sales", "", "lgk_key"},
		{"|https://api.example.com|lgk_key", "", "https://api.example.com", "lgk_key"},
		{"|https://api.example.com", "", "https://api.example.com", ""},
		{"||lgk_key", "", "", "lgk_key"},
		{"", "", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			profile, url, key := parseLG(tt.input)
			if profile != tt.wantProfile {
				t.Errorf("parseLG(%q) profile = %q, want %q", tt.input, profile, tt.wantProfile)
			}
			if url != tt.wantURL {
				t.Errorf("parseLG(%q) url = %q, want %q", tt.input, url, tt.wantURL)
			}
			if key != tt.wantKey {
				t.Errorf("parseLG(%q) key = %q, want %q", tt.input, key, tt.wantKey)
			}
		})
	}
}

func TestResolveLangGraphProfile(t *testing.T) {
	// Set up default and a named profile
	origDefault := langgraphDefault
	origProfiles := langgraphProfiles
	langgraphDefault = &langgraphProfile{
		apiBase:     "https://default.example.com",
		apiKey:      "default-key",
		assistantID: "default-agent",
		streamMode:  "messages-tuple",
		httpClient:  http.DefaultClient,
	}
	langgraphProfiles = map[string]*langgraphProfile{
		"sales": {
			apiBase:     "https://sales.example.com",
			apiKey:      "sales-key",
			assistantID: "sales-agent",
			streamMode:  "messages-tuple",
			httpClient:  http.DefaultClient,
		},
	}
	defer func() {
		langgraphDefault = origDefault
		langgraphProfiles = origProfiles
	}()

	tests := []struct {
		name        string
		handoff     HandoffData
		wantBase    string
		wantKey     string
		wantErr     bool
	}{
		// Default fallback (no routing fields)
		{
			name:     "default profile when no routing fields set",
			handoff:  HandoffData{},
			wantBase: "https://default.example.com",
			wantKey:  "default-key",
		},
		// Verbose-only paths
		{
			name:     "verbose profile only",
			handoff:  HandoffData{Profile: "sales"},
			wantBase: "https://sales.example.com",
			wantKey:  "sales-key",
		},
		{
			name:     "verbose url + key override (no profile)",
			handoff:  HandoffData{LangGraphURL: "https://override.example.com", LangGraphKey: "override-key"},
			wantBase: "https://override.example.com",
			wantKey:  "override-key",
		},
		{
			name:     "verbose url override only",
			handoff:  HandoffData{LangGraphURL: "https://override.example.com"},
			wantBase: "https://override.example.com",
			wantKey:  "default-key",
		},
		{
			name:     "verbose key override only",
			handoff:  HandoffData{LangGraphKey: "override-key"},
			wantBase: "https://default.example.com",
			wantKey:  "override-key",
		},
		{
			name:    "verbose unknown profile errors",
			handoff: HandoffData{Profile: "nonexistent"},
			wantErr: true,
		},
		// Compact syntax paths
		{
			name:     "compact profile only",
			handoff:  HandoffData{LG: "sales"},
			wantBase: "https://sales.example.com",
			wantKey:  "sales-key",
		},
		{
			name:     "compact profile + url override",
			handoff:  HandoffData{LG: "sales|https://staging.example.com"},
			wantBase: "https://staging.example.com",
			wantKey:  "sales-key",
		},
		{
			name:     "compact url + key override (no profile)",
			handoff:  HandoffData{LG: "|https://custom.example.com|lgk_custom"},
			wantBase: "https://custom.example.com",
			wantKey:  "lgk_custom",
		},
		{
			name:     "verbose overrides compact",
			handoff:  HandoffData{LG: "sales", LangGraphURL: "https://override.example.com"},
			wantBase: "https://override.example.com",
			wantKey:  "sales-key",
		},
		{
			name:    "compact unknown profile errors",
			handoff: HandoffData{LG: "nonexistent"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := resolveLangGraphProfile(tt.handoff)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if p.apiBase != tt.wantBase {
				t.Errorf("apiBase = %q, want %q", p.apiBase, tt.wantBase)
			}
			if p.apiKey != tt.wantKey {
				t.Errorf("apiKey = %q, want %q", p.apiKey, tt.wantKey)
			}
		})
	}
}
