package backends

import (
	"testing"
)

func TestUnescapeJSONSurrogatePair(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "simple ASCII no escapes",
			input: "hello",
			want:  "hello",
		},
		{
			name:  "newline escape",
			input: `hello\nworld`,
			want:  "hello\nworld",
		},
		{
			name:  "BMP codepoint",
			input: `\u0041`,
			want:  "A",
		},
		{
			name:  "valid surrogate pair grinning face",
			input: `\uD83D\uDE00`,
			want:  "\U0001F600",
		},
		{
			name:  "orphan high surrogate",
			input: `\uD83D`,
			want:  "\uFFFD",
		},
		{
			name:  "orphan low surrogate",
			input: `\uDE00`,
			want:  "\uFFFD",
		},
		{
			name:  "surrogate pair embedded in text",
			input: `hello \uD83D\uDE00 world`,
			want:  "hello \U0001F600 world",
		},
		{
			name:  "multiple consecutive pairs",
			input: `\uD83D\uDE00\uD83D\uDE01`,
			want:  "\U0001F600\U0001F601",
		},
		{
			name:  "high surrogate followed by non-surrogate escape",
			input: `\uD83D\u0041`,
			want:  "\uFFFDA",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := unescapeJSON([]byte(tt.input))
			if got != tt.want {
				t.Errorf("unescapeJSON(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
