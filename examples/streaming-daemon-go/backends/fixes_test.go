package backends

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// "[ ]" (whitespace inside empty brackets) must be treated as empty; the old
// code emitted a trailing comma and produced invalid JSON.
func TestPrependAttachmentsToContentArrayWhitespace(t *testing.T) {
	images := []ImageData{{Base64: "QUJD", MimeType: "image/png"}}
	for _, existing := range []string{"[]", "[ ]", "[\n]", "[ {\"type\":\"text\",\"text\":\"hi\"} ]"} {
		out := prependAttachmentsToContentArray([]byte(existing), nil, images, "openai")
		if !json.Valid(out) {
			t.Errorf("existing %q -> invalid JSON: %s", existing, out)
		}
	}
}

// Inline base64 comes straight from the handoff JSON and must be escaped like
// any other untrusted string when spliced into the upstream request.
func TestInlineBase64IsEscaped(t *testing.T) {
	hostile := `AA"}}],"assistant_id":"other`
	images := []ImageData{{Base64: hostile, MimeType: "image/png"}}

	body := buildLangGraphRequestBody(HandoffData{Prompt: "hi", ResolvedImages: images}, "agent", "messages")
	if !json.Valid(body) {
		t.Fatalf("v1 body invalid JSON: %s", body)
	}
	var parsed struct {
		AssistantID string `json:"assistant_id"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.AssistantID != "agent" {
		t.Errorf("assistant_id = %q (err %v), want agent: injection not neutralised", parsed.AssistantID, err)
	}

	out := prependAttachmentsToContentArray([]byte(`[{"type":"text","text":"x"}]`), nil, images, "openai")
	if !json.Valid(out) {
		t.Errorf("v2 content invalid JSON: %s", out)
	}
	withAtt := appendContentWithAttachments(nil, "see {img}", map[string]ResolvedAttachment{
		"img": {MimeType: "image/png", Base64: hostile},
	}, nil, "anthropic")
	if !json.Valid(withAtt) {
		t.Errorf("attachment content invalid JSON: %s", withAtt)
	}
}

// The write deadline is re-armed on every SSE write, so a deadline that
// expired while waiting on upstream must not fail the next write.
func TestWriteSSEReArmsDeadline(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	defer server.Close()
	go io.Copy(io.Discard, client)

	if err := server.SetWriteDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := SendSSE(server, "hello"); err != nil {
		t.Fatalf("SendSSE with stale deadline: %v", err)
	}
	if _, err := SendSSEDone(server); err != nil {
		t.Fatalf("SendSSEDone with stale deadline: %v", err)
	}
}

// OpenAI-compatible servers may send "data:" without a space, and single SSE
// lines can exceed 64 KB (tool-call argument deltas, coalescing proxies).
func TestOpenAIStreamDataPrefixAndLongLines(t *testing.T) {
	longContent := strings.Repeat("y", 100*1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data:{\"choices\":[{\"delta\":{\"content\":\"nospace\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\""+longContent+"\"}}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	origBase, origClient := openaiAPIBase, httpClient
	openaiAPIBase, httpClient = srv.URL, srv.Client()
	defer func() { openaiAPIBase, httpClient = origBase, origClient }()

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	var received strings.Builder
	done := make(chan struct{})
	go func() {
		io.Copy(&received, clientConn)
		close(done)
	}()

	o := &OpenAI{}
	_, err := o.Stream(context.Background(), serverConn, HandoffData{Prompt: "hi"})
	serverConn.Close()
	<-done
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	got := received.String()
	if !strings.Contains(got, `{"content":"nospace"}`) {
		t.Errorf("event without space after data: was dropped:\n%s", got[:min(len(got), 200)])
	}
	if !strings.Contains(got, longContent) {
		t.Error("100 KB SSE line was not forwarded")
	}
	if !strings.HasSuffix(got, "data: [DONE]\n\n") {
		t.Error("missing [DONE] marker")
	}
}
