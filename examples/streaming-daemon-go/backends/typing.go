package backends

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"examples/config"
)

// fortuneTimeout is the maximum time to wait for fortune command.
// Keep short to avoid resource exhaustion under high concurrency.
const fortuneTimeout = 500 * time.Millisecond

// fortuneRateLimit limits fortune command invocations to avoid fork bombing.
// Under high load, fallback responses are used instead.
var (
	fortuneLastCall time.Time
	fortuneMu       sync.Mutex
	fortuneMinGap   = 10 * time.Millisecond // Max ~100 fortune calls/second
)

// fallbackResponses are used when fortune is unavailable or rate-limited.
var fallbackResponses = []string{
	"the best way to predict the future is to invent it.",
	"simplicity is the ultimate sophistication.",
	"a journey of a thousand miles begins with a single step.",
	"the only way to do great work is to love what you do.",
	"in the middle of difficulty lies opportunity.",
	"imagination is more important than knowledge.",
	"the mind is everything. What you think you become.",
	"it does not matter how slowly you go as long as you do not stop.",
}

// sseCharBufPool reuses buffers for SSE character messages.
var sseCharBufPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 0, 64)
		return &buf
	},
}

// Typing is a backend that streams characters one at a time with a typewriter effect.
// Uses fortune for dynamic content when the prompt is not recognized.
type Typing struct{}

func init() {
	Register(&Typing{})
}

func (t *Typing) Name() string {
	return "typing"
}

func (t *Typing) Description() string {
	return "Typewriter effect with character-by-character streaming (uses {char,index} format)"
}

func (t *Typing) Init(cfg *config.BackendConfig) error {
	slog.Info("typing backend initialized")
	return nil
}

// Stream sends characters one at a time with realistic typing delays.
func (t *Typing) Stream(ctx context.Context, conn net.Conn, handoff HandoffData) (int64, error) {
	var totalBytes int64
	backendStart := time.Now()
	var ttfbRecorded bool

	// Record backend request
	RecordBackendRequest("typing")

	// Set initial write timeout
	if err := conn.SetWriteDeadline(time.Now().Add(WriteTimeout)); err != nil {
		return 0, fmt.Errorf("could not set write deadline: %w", err)
	}

	writer := bufio.NewWriter(conn)

	var response string
	if handoff.Prompt == "Tell me about streaming" {
		// Known prompt - give the streaming explanation
		response = `Hello! I received your request.

Let me tell you about streaming:

The Apache worker that handled your initial request has already been freed. This response is being streamed directly from a lightweight Go daemon that received the client socket via SCM_RIGHTS file descriptor passing.

This architecture allows:
1. Heavy PHP processes to handle authentication
2. Lightweight daemons to handle long-running streams
3. Better resource utilization under load

The connection handoff is transparent to the client - you see a single HTTP request with a streaming response.

This is the end of my response. Have a great day!`
	} else {
		// Check context before spawning fortune process
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		default:
		}

		var fortuneStr string

		// Rate-limit fortune calls to avoid fork bombing under high load
		fortuneMu.Lock()
		canCallFortune := time.Since(fortuneLastCall) >= fortuneMinGap
		if canCallFortune {
			fortuneLastCall = time.Now()
		}
		fortuneMu.Unlock()

		if canCallFortune {
			// Try fortune command with timeout
			fortuneCtx, cancel := context.WithTimeout(ctx, fortuneTimeout)
			fortune, err := exec.CommandContext(fortuneCtx, "/usr/games/fortune", "literature").Output()
			cancel()
			if err == nil {
				fortuneStr = string(fortune)
				// Remove attribution (lines starting with -- or tabs followed by --)
				lines := strings.Split(fortuneStr, "\n")
				var cleanLines []string
				for _, line := range lines {
					trimmed := strings.TrimSpace(line)
					if strings.HasPrefix(trimmed, "--") {
						break // Stop at attribution
					}
					cleanLines = append(cleanLines, line)
				}
				fortuneStr = strings.Join(cleanLines, " ")
			}
		}

		// Use fallback if fortune failed or was rate-limited
		if fortuneStr == "" {
			// Pick a fallback based on current time for variety
			idx := int(time.Now().UnixNano()) % len(fallbackResponses)
			fortuneStr = fallbackResponses[idx]
		}

		// Clean up whitespace: collapse multiple spaces, trim
		fortuneStr = strings.Join(strings.Fields(fortuneStr), " ")
		fortuneStr = strings.TrimSpace(fortuneStr)

		// Lowercase first letter unless it should stay uppercase
		fortuneStr = lowercaseFirstIfAppropriate(fortuneStr)
		response = fmt.Sprintf("I don't know anything about that, but %s", fortuneStr)
	}

	// Stream character by character like typing
	// Use a reusable timer to avoid leaking timers from time.After
	delayTimer := time.NewTimer(0)
	if !delayTimer.Stop() {
		<-delayTimer.C
	}
	defer delayTimer.Stop()

	for i, char := range response {
		select {
		case <-ctx.Done():
			return totalBytes, ctx.Err()
		default:
		}

		// Send each character as a separate SSE event
		// Uses typing daemon format: {"char": "x", "index": 0}
		// Build JSON manually to avoid allocations from json.Marshal
		bufPtr := sseCharBufPool.Get().(*[]byte)
		buf := (*bufPtr)[:0]
		buf = append(buf, `data: {"char":"`...)
		buf = appendJSONEscapedRune(buf, char)
		buf = append(buf, `","index":`...)
		buf = strconv.AppendInt(buf, int64(i), 10)
		buf = append(buf, "}\n\n"...)

		n, err := writer.Write(buf)
		*bufPtr = buf
		sseCharBufPool.Put(bufPtr)
		totalBytes += int64(n)
		RecordChunkSent()

		// Record TTFB on first chunk
		if !ttfbRecorded {
			RecordBackendTTFB("typing", time.Since(backendStart).Seconds())
			ttfbRecorded = true
		}

		if err != nil {
			RecordBackendError("typing")
			return totalBytes, fmt.Errorf("write failed: %w", err)
		}
		if err := writer.Flush(); err != nil {
			return totalBytes, fmt.Errorf("flush failed: %w", err)
		}

		// Reset deadline after successful write for per-write idle timeout
		if err := conn.SetWriteDeadline(time.Now().Add(WriteTimeout)); err != nil {
			return totalBytes, fmt.Errorf("set write deadline failed: %w", err)
		}

		// Typing speed - vary delay for realistic effect
		var delay time.Duration
		switch char {
		case '\n':
			delay = 100 * time.Millisecond // Pause at newlines
		case '.', '!', '?':
			delay = 150 * time.Millisecond // Pause at sentence ends
		case ',', ':':
			delay = 80 * time.Millisecond // Brief pause at punctuation
		case ' ':
			delay = 30 * time.Millisecond // Quick between words
		default:
			delay = 25 * time.Millisecond // Fast typing
		}

		delayTimer.Reset(delay)
		select {
		case <-delayTimer.C:
		case <-ctx.Done():
			return totalBytes, ctx.Err()
		}
	}

	// Check context before sending completion marker for responsive shutdown
	select {
	case <-ctx.Done():
		return totalBytes, ctx.Err()
	default:
	}

	// Send done marker
	n, err := fmt.Fprintf(writer, "data: [DONE]\n\n")
	totalBytes += int64(n)
	if err != nil {
		return totalBytes, fmt.Errorf("write failed: %w", err)
	}
	if err := writer.Flush(); err != nil {
		return totalBytes, fmt.Errorf("flush failed: %w", err)
	}

	// Record backend duration
	RecordBackendDuration("typing", time.Since(backendStart).Seconds())

	return totalBytes, nil
}

// appendJSONEscapedRune appends a JSON-escaped rune to the buffer.
func appendJSONEscapedRune(buf []byte, r rune) []byte {
	switch r {
	case '"':
		return append(buf, `\"`...)
	case '\\':
		return append(buf, `\\`...)
	case '\n':
		return append(buf, `\n`...)
	case '\r':
		return append(buf, `\r`...)
	case '\t':
		return append(buf, `\t`...)
	default:
		if r < 0x20 {
			// Control characters: use \uXXXX format
			buf = append(buf, `\u00`...)
			buf = append(buf, "0123456789abcdef"[r>>4])
			buf = append(buf, "0123456789abcdef"[r&0xf])
			return buf
		}
		// Normal character - append UTF-8 bytes
		var tmp [4]byte
		n := utf8.EncodeRune(tmp[:], r)
		return append(buf, tmp[:n]...)
	}
}

// lowercaseFirstIfAppropriate lowercases the first letter of a string
// unless it's a word that should remain capitalized mid-sentence
func lowercaseFirstIfAppropriate(s string) string {
	if len(s) == 0 {
		return s
	}

	// Find the first word
	firstWord := strings.Split(s, " ")[0]
	firstWord = strings.TrimFunc(firstWord, func(r rune) bool {
		return !unicode.IsLetter(r)
	})

	// Words that should stay uppercase mid-sentence
	keepUppercase := map[string]bool{
		"I": true, "I'm": true, "I've": true, "I'll": true, "I'd": true,
		"God": true, "Jesus": true, "Allah": true, "Buddha": true,
		"Monday": true, "Tuesday": true, "Wednesday": true, "Thursday": true,
		"Friday": true, "Saturday": true, "Sunday": true,
		"January": true, "February": true, "March": true, "April": true,
		"May": true, "June": true, "July": true, "August": true,
		"September": true, "October": true, "November": true, "December": true,
		"America": true, "American": true, "English": true, "French": true,
		"German": true, "Chinese": true, "Japanese": true, "Russian": true,
		"Linux": true, "Unix": true, "Windows": true, "Mac": true, "Apple": true,
		"Google": true, "Microsoft": true, "Facebook": true, "Amazon": true,
		"NASA": true, "FBI": true, "CIA": true, "USA": true, "UK": true,
	}

	// Check if first word should stay uppercase
	if keepUppercase[firstWord] {
		return s
	}

	// Check if it looks like an acronym (multiple uppercase letters)
	upperCount := 0
	for _, r := range firstWord {
		if unicode.IsUpper(r) {
			upperCount++
		}
	}
	if upperCount > 1 {
		return s // Probably an acronym
	}

	// Lowercase the first letter
	runes := []rune(s)
	runes[0] = unicode.ToLower(runes[0])
	return string(runes)
}
