package backends

import (
	"strings"
	"testing"
)

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
		{
			name: "multimodal content with image (single prompt)",
			handoff: HandoffData{
				Prompt:        "What's in this image?",
				ImageBase64:   "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==",
				ImageMimeType: "image/png",
			},
			assistantID: "vision-agent",
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
			assistantID: "agent",
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
			assistantID: "agent",
			wantContains: []string{
				`"content":[`,
				`{"type":"text","text":"Analyze"}`,
				`{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,dGVzdA=="}}`,
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
