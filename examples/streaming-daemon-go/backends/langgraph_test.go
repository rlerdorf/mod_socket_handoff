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
				Prompt: "What's in this image?",
				ResolvedImages: []ImageData{
					{Base64: "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==", MimeType: "image/png"},
				},
			},
			assistantID:      "vision-agent",
			defaultStreamMode: "messages-tuple",
			wantContains: []string{
				`"assistant_id":"vision-agent"`,
				`"type":"human"`,
				`"content":[`,
				`{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="}}`,
				`{"type":"text","text":"What's in this image?"}`,
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
				ResolvedImages: []ImageData{
					{Base64: "SGVsbG8gV29ybGQ=", MimeType: "image/jpeg"},
				},
			},
			assistantID:      "agent",
			defaultStreamMode: "messages-tuple",
			wantContains: []string{
				`"type":"human","content":"Hello"`,
				`"type":"ai","content":"Hi there!"`,
				`"type":"human","content":[`,
				`{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,SGVsbG8gV29ybGQ="}}`,
				`{"type":"text","text":"Describe this image"}`,
			},
		},
		{
			name: "multimodal content defaults to jpeg mime type",
			handoff: HandoffData{
				Prompt: "Analyze",
				ResolvedImages: []ImageData{
					{Base64: "dGVzdA=="},
				},
			},
			assistantID:      "agent",
			defaultStreamMode: "messages-tuple",
			wantContains: []string{
				`"content":[`,
				`{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,dGVzdA=="}}`,
				`{"type":"text","text":"Analyze"}`,
			},
		},
		{
			name: "multiple images attached to last message",
			handoff: HandoffData{
				Prompt: "Compare these images",
				ResolvedImages: []ImageData{
					{Base64: "aW1hZ2Ux", MimeType: "image/png"},
					{Base64: "aW1hZ2Uy", MimeType: "image/jpeg"},
				},
			},
			assistantID:      "agent",
			defaultStreamMode: "messages-tuple",
			wantContains: []string{
				`"content":[`,
				`{"type":"image_url","image_url":{"url":"data:image/png;base64,aW1hZ2Ux"}}`,
				`{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,aW1hZ2Uy"}}`,
				`{"type":"text","text":"Compare these images"}`,
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

func TestParsePlaceholders(t *testing.T) {
	textAtt := ResolvedAttachment{MimeType: "text/plain", IsText: true, Text: "file content"}
	imgAtt := ResolvedAttachment{MimeType: "image/png", IsText: false, Base64: "iVBOR"}
	attachments := map[string]ResolvedAttachment{
		"notes": textAtt,
		"photo": imgAtt,
	}

	t.Run("single text placeholder resolved", func(t *testing.T) {
		parts := parsePlaceholders("Read {notes} carefully", attachments)
		if len(parts) != 3 {
			t.Fatalf("expected 3 parts, got %d: %+v", len(parts), parts)
		}
		if parts[0].text != "Read " {
			t.Errorf("part[0] = %q, want %q", parts[0].text, "Read ")
		}
		if parts[1].text != "file content" {
			t.Errorf("part[1] = %q, want %q", parts[1].text, "file content")
		}
		if parts[2].text != " carefully" {
			t.Errorf("part[2] = %q, want %q", parts[2].text, " carefully")
		}
	})

	t.Run("single image placeholder resolved", func(t *testing.T) {
		parts := parsePlaceholders("Describe {photo}", attachments)
		if len(parts) != 2 {
			t.Fatalf("expected 2 parts, got %d: %+v", len(parts), parts)
		}
		if parts[0].text != "Describe " {
			t.Errorf("part[0] = %q, want %q", parts[0].text, "Describe ")
		}
		if parts[1].attachment == nil {
			t.Fatal("part[1] should be an attachment")
		}
		if parts[1].attachment.MimeType != "image/png" {
			t.Errorf("attachment mime = %q, want image/png", parts[1].attachment.MimeType)
		}
	})

	t.Run("multiple placeholders", func(t *testing.T) {
		parts := parsePlaceholders("See {photo} and read {notes}", attachments)
		// "See " + {photo} + " and read " + {notes}
		if len(parts) != 4 {
			t.Fatalf("expected 4 parts, got %d: %+v", len(parts), parts)
		}
		if parts[1].attachment == nil {
			t.Error("part[1] should be image attachment")
		}
		if parts[3].text != "file content" {
			t.Errorf("part[3] = %q, want %q", parts[3].text, "file content")
		}
	})

	t.Run("unresolved placeholder left as literal", func(t *testing.T) {
		parts := parsePlaceholders("Hello {unknown} world", attachments)
		if len(parts) != 2 {
			t.Fatalf("expected 2 parts, got %d: %+v", len(parts), parts)
		}
		if parts[0].text != "Hello {unknown}" {
			t.Errorf("part[0] = %q, want %q", parts[0].text, "Hello {unknown}")
		}
		if parts[1].text != " world" {
			t.Errorf("part[1] = %q, want %q", parts[1].text, " world")
		}
	})

	t.Run("no placeholders returns single text part", func(t *testing.T) {
		parts := parsePlaceholders("Just plain text", attachments)
		if len(parts) != 1 {
			t.Fatalf("expected 1 part, got %d", len(parts))
		}
		if parts[0].text != "Just plain text" {
			t.Errorf("part[0] = %q, want %q", parts[0].text, "Just plain text")
		}
	})

	t.Run("placeholder at start of string", func(t *testing.T) {
		parts := parsePlaceholders("{notes} at start", attachments)
		if len(parts) != 2 {
			t.Fatalf("expected 2 parts, got %d: %+v", len(parts), parts)
		}
		if parts[0].text != "file content" {
			t.Errorf("part[0] = %q, want %q", parts[0].text, "file content")
		}
	})

	t.Run("placeholder at end of string", func(t *testing.T) {
		parts := parsePlaceholders("at end {notes}", attachments)
		if len(parts) != 2 {
			t.Fatalf("expected 2 parts, got %d: %+v", len(parts), parts)
		}
		if parts[1].text != "file content" {
			t.Errorf("part[1] = %q, want %q", parts[1].text, "file content")
		}
	})

	t.Run("adjacent placeholders", func(t *testing.T) {
		parts := parsePlaceholders("{notes}{photo}", attachments)
		if len(parts) != 2 {
			t.Fatalf("expected 2 parts, got %d: %+v", len(parts), parts)
		}
		if parts[0].text != "file content" {
			t.Errorf("part[0] = %q, want %q", parts[0].text, "file content")
		}
		if parts[1].attachment == nil {
			t.Error("part[1] should be image attachment")
		}
	})

	t.Run("lone open brace treated as literal", func(t *testing.T) {
		parts := parsePlaceholders("hello { world", attachments)
		if len(parts) != 1 {
			t.Fatalf("expected 1 part, got %d: %+v", len(parts), parts)
		}
		if parts[0].text != "hello { world" {
			t.Errorf("part[0] = %q, want %q", parts[0].text, "hello { world")
		}
	})

	t.Run("lone close brace treated as literal", func(t *testing.T) {
		parts := parsePlaceholders("hello } world", attachments)
		if len(parts) != 1 {
			t.Fatalf("expected 1 part, got %d: %+v", len(parts), parts)
		}
		if parts[0].text != "hello } world" {
			t.Errorf("part[0] = %q, want %q", parts[0].text, "hello } world")
		}
	})

	t.Run("empty attachments map returns single part", func(t *testing.T) {
		parts := parsePlaceholders("Hello {notes}", nil)
		if len(parts) != 1 {
			t.Fatalf("expected 1 part, got %d", len(parts))
		}
		if parts[0].text != "Hello {notes}" {
			t.Errorf("part[0] = %q, want %q", parts[0].text, "Hello {notes}")
		}
	})
}

func TestAppendContentWithAttachments(t *testing.T) {
	t.Run("text attachment inlined produces simple string", func(t *testing.T) {
		attachments := map[string]ResolvedAttachment{
			"notes": {MimeType: "text/plain", IsText: true, Text: "my notes"},
		}
		buf := appendContentWithAttachments(nil, "Read {notes} here", attachments, nil, "openai")
		got := string(buf)
		want := `"Read my notes here"`
		if got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	})

	t.Run("image attachment produces content array", func(t *testing.T) {
		attachments := map[string]ResolvedAttachment{
			"photo": {MimeType: "image/png", IsText: false, Base64: "iVBOR"},
		}
		buf := appendContentWithAttachments(nil, "Describe {photo}", attachments, nil, "openai")
		got := string(buf)
		if !strings.Contains(got, `[`) {
			t.Error("expected content array for image attachment")
		}
		if !strings.Contains(got, `{"type":"text","text":"Describe "}`) {
			t.Errorf("missing text part in %s", got)
		}
		if !strings.Contains(got, `{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBOR"}}`) {
			t.Errorf("missing image part in %s", got)
		}
	})

	t.Run("mixed text before image then text after", func(t *testing.T) {
		attachments := map[string]ResolvedAttachment{
			"photo": {MimeType: "image/jpeg", IsText: false, Base64: "abc123"},
			"notes": {MimeType: "text/plain", IsText: true, Text: "some notes"},
		}
		buf := appendContentWithAttachments(nil, "See {photo} with {notes} attached", attachments, nil, "openai")
		got := string(buf)
		if !strings.Contains(got, `"type":"image_url"`) {
			t.Errorf("missing image_url in %s", got)
		}
		if !strings.Contains(got, `"See "`) {
			t.Errorf("missing leading text in %s", got)
		}
		if !strings.Contains(got, `some notes`) {
			t.Errorf("missing inlined text attachment in %s", got)
		}
	})

	t.Run("legacy images and attachments both present", func(t *testing.T) {
		attachments := map[string]ResolvedAttachment{
			"photo": {MimeType: "image/png", IsText: false, Base64: "newimg"},
		}
		legacyImages := []ImageData{
			{Base64: "oldimg", MimeType: "image/jpeg"},
		}
		buf := appendContentWithAttachments(nil, "See {photo}", attachments, legacyImages, "openai")
		got := string(buf)
		// Should have both images
		if !strings.Contains(got, "newimg") {
			t.Errorf("missing attachment image in %s", got)
		}
		if !strings.Contains(got, "oldimg") {
			t.Errorf("missing legacy image in %s", got)
		}
	})

	t.Run("no attachments or images produces simple string", func(t *testing.T) {
		buf := appendContentWithAttachments(nil, "Hello world", nil, nil, "openai")
		got := string(buf)
		if got != `"Hello world"` {
			t.Errorf("got %s, want %q", got, `"Hello world"`)
		}
	})

	t.Run("unreferenced image precedes text", func(t *testing.T) {
		attachments := map[string]ResolvedAttachment{
			"photo": {MimeType: "image/png", IsText: false, Base64: "iVBOR"},
		}
		buf := appendContentWithAttachments(nil, "What is in this image?", attachments, nil, "openai")
		got := string(buf)
		if !strings.Contains(got, `[`) {
			t.Error("expected content array for image attachment")
		}
		if !strings.Contains(got, `{"type":"text","text":"What is in this image?"}`) {
			t.Errorf("missing text part in %s", got)
		}
		if !strings.Contains(got, `{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBOR"}}`) {
			t.Errorf("missing image part in %s", got)
		}
		// Image should come before text (LangChain convention)
		imgIdx := strings.Index(got, `"type":"image_url"`)
		txtIdx := strings.Index(got, `"type":"text"`)
		if imgIdx > txtIdx {
			t.Errorf("unreferenced image should precede text, got %s", got)
		}
	})

	t.Run("text attachment without placeholder appended to text", func(t *testing.T) {
		attachments := map[string]ResolvedAttachment{
			"notes": {MimeType: "text/plain", IsText: true, Text: " [attached notes]"},
		}
		buf := appendContentWithAttachments(nil, "Summarize this", attachments, nil, "openai")
		got := string(buf)
		want := `"Summarize this [attached notes]"`
		if got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	})

	t.Run("mix of referenced and unreferenced attachments", func(t *testing.T) {
		attachments := map[string]ResolvedAttachment{
			"notes":  {MimeType: "text/plain", IsText: true, Text: "inline notes"},
			"photo1": {MimeType: "image/png", IsText: false, Base64: "img1"},
			"photo2": {MimeType: "image/jpeg", IsText: false, Base64: "img2"},
		}
		// Only {notes} is referenced; photo1 and photo2 should be appended
		buf := appendContentWithAttachments(nil, "Read {notes} carefully", attachments, nil, "openai")
		got := string(buf)
		if !strings.Contains(got, `"type":"image_url"`) {
			t.Errorf("unreferenced images should be appended, got %s", got)
		}
		if !strings.Contains(got, "img1") || !strings.Contains(got, "img2") {
			t.Errorf("both unreferenced images should be present in %s", got)
		}
		if !strings.Contains(got, "inline notes") {
			t.Errorf("referenced text should be inlined in %s", got)
		}
	})

	t.Run("non-image binary placeholder preserved as literal", func(t *testing.T) {
		attachments := map[string]ResolvedAttachment{
			"doc": {MimeType: "application/octet-stream", IsText: false, Base64: "AAAA"},
			"img": {MimeType: "image/png", IsText: false, Base64: "iVBO"},
		}
		buf := appendContentWithAttachments(nil, "See {doc} and {img}", attachments, nil, "openai")
		got := string(buf)
		// The octet-stream attachment should keep its placeholder as literal text
		if strings.Contains(got, "AAAA") {
			t.Errorf("non-image binary data should not be emitted, got %s", got)
		}
		if !strings.Contains(got, "{doc}") {
			t.Errorf("non-image binary placeholder should be preserved as literal, got %s", got)
		}
		if !strings.Contains(got, "iVBO") {
			t.Errorf("image attachment should be present, got %s", got)
		}
	})

	t.Run("unresolved placeholder preserved in text", func(t *testing.T) {
		attachments := map[string]ResolvedAttachment{
			"notes": {MimeType: "text/plain", IsText: true, Text: "content"},
		}
		buf := appendContentWithAttachments(nil, "{notes} and {unknown}", attachments, nil, "openai")
		got := string(buf)
		// No images, so should be simple string
		want := `"content and {unknown}"`
		if got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	})

	t.Run("pdf with anthropic format emits document block", func(t *testing.T) {
		attachments := map[string]ResolvedAttachment{
			"report": {MimeType: "application/pdf", IsText: false, Base64: "JVBERi0xLjQ="},
		}
		buf := appendContentWithAttachments(nil, "Summarize {report}", attachments, nil, "anthropic")
		got := string(buf)
		if !strings.Contains(got, `[`) {
			t.Error("expected content array for document attachment")
		}
		if !strings.Contains(got, `{"type":"text","text":"Summarize "}`) {
			t.Errorf("missing text part in %s", got)
		}
		if !strings.Contains(got, `{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0xLjQ="}}`) {
			t.Errorf("missing document part in %s", got)
		}
	})

	t.Run("pdf with openai format preserved as literal placeholder", func(t *testing.T) {
		attachments := map[string]ResolvedAttachment{
			"report": {MimeType: "application/pdf", IsText: false, Base64: "JVBERi0xLjQ="},
		}
		buf := appendContentWithAttachments(nil, "Summarize {report}", attachments, nil, "openai")
		got := string(buf)
		// PDF data should NOT appear in output
		if strings.Contains(got, "JVBERi0xLjQ=") {
			t.Errorf("PDF data should not be emitted for openai format, got %s", got)
		}
		// Placeholder should be preserved as literal text
		if !strings.Contains(got, "{report}") {
			t.Errorf("PDF placeholder should be preserved as literal, got %s", got)
		}
	})

	t.Run("unreferenced non-image binary skipped in openai format", func(t *testing.T) {
		attachments := map[string]ResolvedAttachment{
			"report": {MimeType: "application/pdf", IsText: false, Base64: "JVBERi0xLjQ="},
		}
		buf := appendContentWithAttachments(nil, "What does this say?", attachments, nil, "openai")
		got := string(buf)
		// PDF data should NOT appear
		if strings.Contains(got, "JVBERi0xLjQ=") {
			t.Errorf("unreferenced non-image binary should be skipped in openai format, got %s", got)
		}
		// Should NOT inject empty braces {} into text
		if strings.Contains(got, "{}") {
			t.Errorf("should not inject empty braces into text, got %s", got)
		}
		// Text should still be present
		if !strings.Contains(got, "What does this say?") {
			t.Errorf("text should be present, got %s", got)
		}
	})

	t.Run("pdf unreferenced with anthropic format emits document", func(t *testing.T) {
		attachments := map[string]ResolvedAttachment{
			"report": {MimeType: "application/pdf", IsText: false, Base64: "JVBERi0xLjQ="},
		}
		buf := appendContentWithAttachments(nil, "What does this say?", attachments, nil, "anthropic")
		got := string(buf)
		if !strings.Contains(got, `"type":"document"`) {
			t.Errorf("unreferenced PDF should emit document block for anthropic, got %s", got)
		}
		if !strings.Contains(got, "JVBERi0xLjQ=") {
			t.Errorf("PDF data should be present in document block, got %s", got)
		}
	})

	t.Run("mixed image and pdf with anthropic format", func(t *testing.T) {
		attachments := map[string]ResolvedAttachment{
			"photo":  {MimeType: "image/png", IsText: false, Base64: "iVBOR"},
			"report": {MimeType: "application/pdf", IsText: false, Base64: "JVBERi0="},
		}
		buf := appendContentWithAttachments(nil, "See {photo} and read {report}", attachments, nil, "anthropic")
		got := string(buf)
		if !strings.Contains(got, `"type":"image_url"`) {
			t.Errorf("image should use image_url format, got %s", got)
		}
		if !strings.Contains(got, `"type":"document"`) {
			t.Errorf("PDF should use document format for anthropic, got %s", got)
		}
	})
}

func TestBuildLangGraphRequestBodyWithAttachments(t *testing.T) {
	t.Run("text attachment inlined in single prompt", func(t *testing.T) {
		handoff := HandoffData{
			Prompt: "Read {notes} carefully",
			ResolvedAttachments: map[string]ResolvedAttachment{
				"notes": {MimeType: "text/plain", IsText: true, Text: "meeting notes"},
			},
		}
		got := string(buildLangGraphRequestBody(handoff, "agent", "messages-tuple"))
		if !strings.Contains(got, "meeting notes") {
			t.Errorf("expected inlined text in %s", got)
		}
		if strings.Contains(got, "{notes}") {
			t.Errorf("placeholder should be resolved in %s", got)
		}
	})

	t.Run("image attachment in messages array on last message only", func(t *testing.T) {
		handoff := HandoffData{
			Messages: []Message{
				{Role: "user", Content: "First {photo}"},
				{Role: "assistant", Content: "I see"},
				{Role: "user", Content: "Now describe {photo}"},
			},
			ResolvedAttachments: map[string]ResolvedAttachment{
				"photo": {MimeType: "image/png", IsText: false, Base64: "iVBOR"},
			},
		}
		got := string(buildLangGraphRequestBody(handoff, "agent", "messages-tuple"))
		// First message should NOT have resolved placeholder (not last)
		if !strings.Contains(got, `"content":"First {photo}"`) {
			t.Errorf("first message placeholder should be literal in %s", got)
		}
		// Last message should have image
		if !strings.Contains(got, `"type":"image_url"`) {
			t.Errorf("last message should contain image_url in %s", got)
		}
	})

	t.Run("attachments with legacy images combined", func(t *testing.T) {
		handoff := HandoffData{
			Prompt: "See {photo}",
			ResolvedAttachments: map[string]ResolvedAttachment{
				"photo": {MimeType: "image/png", IsText: false, Base64: "new"},
			},
			ResolvedImages: []ImageData{
				{Base64: "legacy", MimeType: "image/jpeg"},
			},
		}
		got := string(buildLangGraphRequestBody(handoff, "agent", "messages-tuple"))
		if !strings.Contains(got, "new") {
			t.Errorf("missing attachment image in %s", got)
		}
		if !strings.Contains(got, "legacy") {
			t.Errorf("missing legacy image in %s", got)
		}
	})
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
