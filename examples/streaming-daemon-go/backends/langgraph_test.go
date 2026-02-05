package backends

import (
	"bufio"
	"io"
	"strings"
	"testing"
)

func TestParseLangGraphEvent(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		wantEventType string
		wantData      string
		wantErr       error
	}{
		{
			name:          "messages event",
			input:         "event: messages\ndata: [{\"content\":\"Hello\",\"type\":\"AIMessageChunk\"},{\"run_id\":\"123\"}]\n\n",
			wantEventType: "messages",
			wantData:      `[{"content":"Hello","type":"AIMessageChunk"},{"run_id":"123"}]`,
		},
		{
			name:          "metadata event",
			input:         "event: metadata\ndata: {\"run_id\":\"abc-123\"}\n\n",
			wantEventType: "metadata",
			wantData:      `{"run_id":"abc-123"}`,
		},
		{
			name:          "end event",
			input:         "event: end\ndata: null\n\n",
			wantEventType: "end",
			wantData:      "null",
		},
		{
			name:          "values event",
			input:         "event: values\ndata: {\"messages\":[{\"type\":\"ai\",\"content\":\"test\"}]}\n\n",
			wantEventType: "values",
			wantData:      `{"messages":[{"type":"ai","content":"test"}]}`,
		},
		{
			name:          "updates event",
			input:         "event: updates\ndata: {\"node\":\"agent\"}\n\n",
			wantEventType: "updates",
			wantData:      `{"node":"agent"}`,
		},
		{
			name:          "event with leading empty lines",
			input:         "\n\nevent: messages\ndata: {\"content\":\"test\"}\n\n",
			wantEventType: "messages",
			wantData:      `{"content":"test"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scanner := bufio.NewScanner(strings.NewReader(tt.input))
			eventType, data, err := parseLangGraphEvent(scanner)

			if err != tt.wantErr {
				t.Errorf("parseLangGraphEvent() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if eventType != tt.wantEventType {
				t.Errorf("parseLangGraphEvent() eventType = %v, want %v", eventType, tt.wantEventType)
			}
			if string(data) != tt.wantData {
				t.Errorf("parseLangGraphEvent() data = %v, want %v", string(data), tt.wantData)
			}
		})
	}
}

func TestParseLangGraphEventEOF(t *testing.T) {
	scanner := bufio.NewScanner(strings.NewReader(""))
	_, _, err := parseLangGraphEvent(scanner)
	if err != io.EOF {
		t.Errorf("parseLangGraphEvent() on empty input should return io.EOF, got %v", err)
	}
}

func TestParseLangGraphEventEOFAfterEventLine(t *testing.T) {
	// Test case: EOF occurs after reading event type but before data line
	// This is valid - the event type is returned with nil data
	scanner := bufio.NewScanner(strings.NewReader("event: messages\n"))
	eventType, data, err := parseLangGraphEvent(scanner)
	if err != nil {
		t.Errorf("parseLangGraphEvent() unexpected error: %v", err)
	}
	if eventType != "messages" {
		t.Errorf("parseLangGraphEvent() eventType = %q, want %q", eventType, "messages")
	}
	if data != nil {
		t.Errorf("parseLangGraphEvent() data = %v, want nil", data)
	}
}

func TestExtractContentFromMessages(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		want    string
	}{
		{
			name: "simple content",
			data: `[{"content":"Hello world","type":"AIMessageChunk"},{"run_id":"123"}]`,
			want: "Hello world",
		},
		{
			name: "empty content",
			data: `[{"content":"","type":"AIMessageChunk"},{"run_id":"123"}]`,
			want: "",
		},
		{
			name: "content with escaped quotes",
			data: `[{"content":"Say \"hello\"","type":"AIMessageChunk"},{"run_id":"123"}]`,
			want: `Say "hello"`,
		},
		{
			name: "content with newlines",
			data: `[{"content":"Line1\nLine2","type":"AIMessageChunk"},{"run_id":"123"}]`,
			want: "Line1\nLine2",
		},
		{
			name: "content with unicode escape",
			data: `[{"content":"Hello \u0041","type":"AIMessageChunk"},{"run_id":"123"}]`,
			want: "Hello A",
		},
		{
			name: "content with backslash",
			data: `[{"content":"path\\to\\file","type":"AIMessageChunk"},{"run_id":"123"}]`,
			want: `path\to\file`,
		},
		{
			name: "no content field",
			data: `[{"type":"AIMessageChunk"},{"run_id":"123"}]`,
			want: "",
		},
		{
			name: "content with tab",
			data: `[{"content":"col1\tcol2","type":"AIMessageChunk"},{"run_id":"123"}]`,
			want: "col1\tcol2",
		},
		{
			name: "real streaming chunk",
			data: `[{"content":" The","additional_kwargs":{},"response_metadata":{},"type":"AIMessageChunk","name":null,"id":"run-abc123","tool_calls":[],"invalid_tool_calls":[],"tool_call_chunks":[]},{"run_id":"abc123","langgraph_step":1}]`,
			want: " The",
		},
		{
			name: "content with special chars",
			data: `[{"content":"<div class=\"test\">","type":"AIMessageChunk"},{"run_id":"123"}]`,
			want: `<div class="test">`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractContentFromMessages([]byte(tt.data))
			if got != tt.want {
				t.Errorf("extractContentFromMessages() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildLangGraphRequestBody(t *testing.T) {
	tests := []struct {
		name        string
		handoff     HandoffData
		assistantID string
		wantContains []string
	}{
		{
			name: "single prompt",
			handoff: HandoffData{
				Prompt: "Hello, how are you?",
			},
			assistantID: "test-agent",
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
			assistantID: "math-agent",
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
			assistantID: "shop-agent",
			wantContains: []string{
				`"seller_id":"seller123"`,
				`"shop_id":456`,
			},
		},
		{
			name: "empty prompt uses default",
			handoff: HandoffData{},
			assistantID: "agent",
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
			assistantID: "agent",
			wantContains: []string{
				`"type":"system","content":"You are helpful."`,
				`"type":"human","content":"Hi"`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildLangGraphRequestBody(tt.handoff, tt.assistantID)
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
