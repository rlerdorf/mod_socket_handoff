package backends

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---- mergeAttachmentsIntoLGBody ----

func TestMergeAttachmentsIntoLGBody(t *testing.T) {
	t.Run("no attachments passthrough", func(t *testing.T) {
		body := json.RawMessage(`{"assistant_id":"agent","input":{"messages":[{"type":"human","content":"hello"}]},"stream_mode":["custom"]}`)
		got, err := mergeAttachmentsIntoLGBody(body, nil, nil, "openai")
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, body) {
			t.Errorf("expected passthrough: got %s", got)
		}
	})

	t.Run("string content + resolved image openai", func(t *testing.T) {
		body := json.RawMessage(`{"input":{"messages":[{"type":"human","content":"describe this"}]}}`)
		resolved := map[string]ResolvedAttachment{}
		images := []ImageData{{Base64: "aW1nZGF0YQ==", MimeType: "image/png"}}
		got, err := mergeAttachmentsIntoLGBody(body, resolved, images, "openai")
		if err != nil {
			t.Fatal(err)
		}
		s := string(got)
		if !strings.Contains(s, `"image_url"`) {
			t.Errorf("expected image_url in output: %s", s)
		}
		if !strings.Contains(s, "describe this") {
			t.Errorf("original text lost: %s", s)
		}
	})

	t.Run("string content + text attachment inlined", func(t *testing.T) {
		body := json.RawMessage(`{"input":{"messages":[{"type":"human","content":"context: {doc}"}]}}`)
		resolved := map[string]ResolvedAttachment{
			"doc": {MimeType: "text/plain", IsText: true, Text: "some document text"},
		}
		got, err := mergeAttachmentsIntoLGBody(body, resolved, nil, "openai")
		if err != nil {
			t.Fatal(err)
		}
		s := string(got)
		if !strings.Contains(s, "some document text") {
			t.Errorf("text attachment not inlined: %s", s)
		}
	})

	t.Run("string content + binary attachment openai", func(t *testing.T) {
		body := json.RawMessage(`{"input":{"messages":[{"type":"human","content":"see {img}"}]}}`)
		resolved := map[string]ResolvedAttachment{
			"img": {MimeType: "image/jpeg", IsText: false, Base64: "aW1nZGF0YQ=="},
		}
		got, err := mergeAttachmentsIntoLGBody(body, resolved, nil, "openai")
		if err != nil {
			t.Fatal(err)
		}
		s := string(got)
		if !strings.Contains(s, `"image_url"`) {
			t.Errorf("expected image_url content part: %s", s)
		}
	})

	t.Run("string content + binary attachment anthropic document", func(t *testing.T) {
		body := json.RawMessage(`{"input":{"messages":[{"type":"human","content":"review {pdf}"}]}}`)
		resolved := map[string]ResolvedAttachment{
			"pdf": {MimeType: "application/pdf", IsText: false, Base64: "cGRmZGF0YQ=="},
		}
		got, err := mergeAttachmentsIntoLGBody(body, resolved, nil, "anthropic")
		if err != nil {
			t.Fatal(err)
		}
		s := string(got)
		if !strings.Contains(s, `"document"`) {
			t.Errorf("expected document content part: %s", s)
		}
	})

	t.Run("array content unreferenced image prepended", func(t *testing.T) {
		body := json.RawMessage(`{"input":{"messages":[{"type":"human","content":[{"type":"text","text":"hello"}]}]}}`)
		images := []ImageData{{Base64: "aW1n", MimeType: "image/png"}}
		got, err := mergeAttachmentsIntoLGBody(body, nil, images, "openai")
		if err != nil {
			t.Fatal(err)
		}
		s := string(got)
		if !strings.Contains(s, `"image_url"`) {
			t.Errorf("expected image_url prepended: %s", s)
		}
		// Existing text element must still be present
		if !strings.Contains(s, `"hello"`) {
			t.Errorf("existing text element missing: %s", s)
		}
		// Image must come before text in the array
		imageIdx := strings.Index(s, "image_url")
		textIdx := strings.Index(s, `"hello"`)
		if imageIdx > textIdx {
			t.Errorf("image_url should precede text: %s", s)
		}
	})

	t.Run("earlier messages not modified", func(t *testing.T) {
		body := json.RawMessage(`{"input":{"messages":[{"type":"human","content":"first"},{"type":"human","content":"second"}]}}`)
		images := []ImageData{{Base64: "aW1n", MimeType: "image/jpeg"}}
		got, err := mergeAttachmentsIntoLGBody(body, nil, images, "openai")
		if err != nil {
			t.Fatal(err)
		}
		// Unmarshal and inspect messages directly rather than counting substrings.
		var result struct {
			Input struct {
				Messages []struct {
					Content json.RawMessage `json:"content"`
				} `json:"messages"`
			} `json:"input"`
		}
		if err := json.Unmarshal(got, &result); err != nil {
			t.Fatalf("unmarshal result: %v", err)
		}
		if len(result.Input.Messages) != 2 {
			t.Fatalf("expected 2 messages, got %d", len(result.Input.Messages))
		}
		// First message must still be a plain string.
		if result.Input.Messages[0].Content[0] != '"' {
			t.Errorf("first message content should still be a string, got: %s", result.Input.Messages[0].Content)
		}
		// Last message must be a content array.
		if result.Input.Messages[1].Content[0] != '[' {
			t.Errorf("last message content should be an array, got: %s", result.Input.Messages[1].Content)
		}
	})

	t.Run("empty array content + image produces valid JSON array", func(t *testing.T) {
		body := json.RawMessage(`{"input":{"messages":[{"type":"human","content":[]}]}}`)
		images := []ImageData{{Base64: "aW1n", MimeType: "image/png"}}
		got, err := mergeAttachmentsIntoLGBody(body, nil, images, "openai")
		if err != nil {
			t.Fatal(err)
		}
		// Must be valid JSON (no trailing comma)
		var v any
		if err := json.Unmarshal(got, &v); err != nil {
			t.Errorf("result is not valid JSON: %v\nbody: %s", err, got)
		}
		if !strings.Contains(string(got), `"image_url"`) {
			t.Errorf("expected image_url in output: %s", got)
		}
	})

	t.Run("text attachment in array mode appended as text part", func(t *testing.T) {
		body := json.RawMessage(`{"input":{"messages":[{"type":"human","content":[{"type":"text","text":"hello"}]}]}}`)
		resolved := map[string]ResolvedAttachment{
			"doc": {MimeType: "text/plain", IsText: true, Text: "appended text"},
		}
		got, err := mergeAttachmentsIntoLGBody(body, resolved, nil, "openai")
		if err != nil {
			t.Fatal(err)
		}
		s := string(got)
		if !strings.Contains(s, "appended text") {
			t.Errorf("text attachment content missing from output: %s", s)
		}
		var v any
		if err := json.Unmarshal(got, &v); err != nil {
			t.Errorf("result is not valid JSON: %v", err)
		}
	})

	t.Run("multiple binary attachments in array mode have deterministic order", func(t *testing.T) {
		body := json.RawMessage(`{"input":{"messages":[{"type":"human","content":[{"type":"text","text":"hi"}]}]}}`)
		resolved := map[string]ResolvedAttachment{
			"zzz": {MimeType: "image/png", IsText: false, Base64: "enp6"},
			"aaa": {MimeType: "image/png", IsText: false, Base64: "YWFh"},
			"mmm": {MimeType: "image/png", IsText: false, Base64: "bW1t"},
		}
		first, err := mergeAttachmentsIntoLGBody(body, resolved, nil, "openai")
		if err != nil {
			t.Fatal(err)
		}
		second, err := mergeAttachmentsIntoLGBody(body, resolved, nil, "openai")
		if err != nil {
			t.Fatal(err)
		}
		if string(first) != string(second) {
			t.Errorf("output is non-deterministic:\nfirst:  %s\nsecond: %s", first, second)
		}
		// aaa sorts before mmm before zzz, so base64 values should appear in that order
		s := string(first)
		aaaIdx := strings.Index(s, "YWFh")
		mmmIdx := strings.Index(s, "bW1t")
		zzzIdx := strings.Index(s, "enp6")
		if !(aaaIdx < mmmIdx && mmmIdx < zzzIdx) {
			t.Errorf("attachments not in sorted order: aaa@%d mmm@%d zzz@%d in: %s", aaaIdx, mmmIdx, zzzIdx, s)
		}
	})

	t.Run("uppercase MIME type treated as image not document", func(t *testing.T) {
		body := json.RawMessage(`{"input":{"messages":[{"type":"human","content":"look"}]}}`)
		resolved := map[string]ResolvedAttachment{
			"img": {MimeType: "image/PNG", IsText: false, Base64: "aW1n"},
		}
		got, err := mergeAttachmentsIntoLGBody(body, resolved, nil, "anthropic")
		if err != nil {
			t.Fatal(err)
		}
		s := string(got)
		if strings.Contains(s, `"document"`) {
			t.Errorf("image/PNG should produce image_url not document: %s", s)
		}
		if !strings.Contains(s, `"image_url"`) {
			t.Errorf("expected image_url for image/PNG: %s", s)
		}
	})
}

// ---- streamLGBody integration ----

func setupLGBodyTest(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

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
	t.Cleanup(func() {
		langgraphDefault = origDefault
		langgraphProfiles = origProfiles
	})
}

func runStreamLGBody(t *testing.T, handoff HandoffData) (string, error) {
	t.Helper()
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
	_, err := lg.Stream(context.Background(), serverConn, handoff)
	serverConn.Close()
	<-done
	return received.String(), err
}

func TestStreamLGBodyProxy(t *testing.T) {
	ssePayload := "event: messages\ndata: [{\"content\":\"hi\"}]\n\nevent: end\ndata: null\n\n"

	t.Run("body forwarded verbatim no thread", func(t *testing.T) {
		var capturedPath string
		var capturedBody []byte
		setupLGBodyTest(t, func(w http.ResponseWriter, r *http.Request) {
			capturedPath = r.URL.Path
			capturedBody, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(ssePayload))
		})

		lgBody := json.RawMessage(`{"assistant_id":"agent","input":{"messages":[{"type":"human","content":"hello"}]},"stream_mode":["custom"]}`)
		result, err := runStreamLGBody(t, HandoffData{LGBody: lgBody})
		if err != nil {
			t.Fatalf("Stream() error: %v", err)
		}
		if capturedPath != "/runs/stream" {
			t.Errorf("path = %q, want /runs/stream", capturedPath)
		}
		if !bytes.Equal(capturedBody, lgBody) {
			t.Errorf("body not forwarded verbatim:\ngot:  %s\nwant: %s", capturedBody, lgBody)
		}
		if result != ssePayload {
			t.Errorf("SSE not proxied correctly:\ngot:  %q\nwant: %q", result, ssePayload)
		}
	})

	t.Run("thread_id routes to stateful endpoint and creates thread first", func(t *testing.T) {
		var paths []string
		setupLGBodyTest(t, func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			if r.URL.Path == "/threads" {
				w.WriteHeader(http.StatusOK)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(ssePayload))
		})

		lgBody := json.RawMessage(`{"assistant_id":"agent","input":{"messages":[{"type":"human","content":"hi"}]}}`)
		_, err := runStreamLGBody(t, HandoffData{LGBody: lgBody, ThreadID: "thread-xyz"})
		if err != nil {
			t.Fatalf("Stream() error: %v", err)
		}
		if len(paths) < 2 {
			t.Fatalf("expected at least 2 requests, got %d: %v", len(paths), paths)
		}
		if paths[0] != "/threads" {
			t.Errorf("first request = %q, want /threads", paths[0])
		}
		if paths[1] != "/threads/thread-xyz/runs/stream" {
			t.Errorf("second request = %q, want /threads/thread-xyz/runs/stream", paths[1])
		}
	})

	t.Run("context canceled", func(t *testing.T) {
		setupLGBodyTest(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("event: start\ndata: {}\n\n"))
		})

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		lgBody := json.RawMessage(`{"input":{"messages":[{"type":"human","content":"hi"}]}}`)
		clientConn, serverConn := net.Pipe()
		defer clientConn.Close()
		defer serverConn.Close()

		go io.Copy(io.Discard, clientConn)

		lg := &LangGraph{}
		_, err := lg.Stream(ctx, serverConn, HandoffData{LGBody: lgBody})
		if err == nil {
			t.Error("expected error for canceled context")
		}
	})

	t.Run("SSE chunks proxied correctly", func(t *testing.T) {
		multiChunk := strings.Repeat("event: data\ndata: chunk\n\n", 5)
		setupLGBodyTest(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(multiChunk))
		})

		lgBody := json.RawMessage(`{"input":{"messages":[{"type":"human","content":"hi"}]}}`)
		result, err := runStreamLGBody(t, HandoffData{LGBody: lgBody})
		if err != nil {
			t.Fatalf("Stream() error: %v", err)
		}
		if result != multiChunk {
			t.Errorf("chunks not proxied correctly:\ngot:  %q\nwant: %q", result, multiChunk)
		}
	})

	t.Run("malformed SSE proxied transparently", func(t *testing.T) {
		malformed := "not valid SSE\nstill forwarded\n"
		setupLGBodyTest(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(malformed))
		})

		lgBody := json.RawMessage(`{"input":{"messages":[{"type":"human","content":"hi"}]}}`)
		result, err := runStreamLGBody(t, HandoffData{LGBody: lgBody})
		if err != nil {
			t.Fatalf("Stream() error: %v", err)
		}
		if result != malformed {
			t.Errorf("malformed SSE not passed through: got %q, want %q", result, malformed)
		}
	})
}
