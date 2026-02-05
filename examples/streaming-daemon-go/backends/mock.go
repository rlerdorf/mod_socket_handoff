package backends

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"examples/config"
)

// Mock backend configuration flags (can override config file)
var (
	messageDelayFlag = flag.Duration("message-delay", 0,
		"Delay between SSE messages (for mock backend, e.g., 3333ms for 30s streams)")
)

// Mock is a demo backend that sends fixed SSE messages.
// Useful for testing and benchmarking without external dependencies.
type Mock struct {
	messageDelay time.Duration
}

func init() {
	Register(&Mock{})
}

func (m *Mock) Name() string {
	return "mock"
}

func (m *Mock) Description() string {
	return "Demo backend with fixed messages (for testing/benchmarking)"
}

func (m *Mock) Init(cfg *config.BackendConfig) error {
	// Default
	m.messageDelay = 50 * time.Millisecond

	// Config file value
	if cfg != nil && cfg.Mock.MessageDelayMs > 0 {
		m.messageDelay = time.Duration(cfg.Mock.MessageDelayMs) * time.Millisecond
	}

	// Flag override (if explicitly set)
	if *messageDelayFlag != 0 {
		m.messageDelay = *messageDelayFlag
	}

	log.Printf("Mock backend: message_delay=%v", m.messageDelay)
	return nil
}

// Stream sends demo SSE messages to the client.
func (m *Mock) Stream(ctx context.Context, conn net.Conn, handoff HandoffData) (int64, error) {
	var totalBytes int64

	// Fixed messages for consistent benchmarking (18 messages x delay)
	messages := []string{
		fmt.Sprintf("Hello from Go daemon! Prompt: %s", truncate(handoff.Prompt, 50)),
		fmt.Sprintf("User ID: %d", handoff.UserID),
		"This daemon uses goroutines for concurrent handling.",
		"Each connection runs as a lightweight goroutine.",
		"Expected capacity: 50,000+ concurrent connections.",
		"Memory per connection: ~6-10 KB.",
		"The Apache worker was freed immediately after handoff.",
		"Replace this demo with your LLM API integration.",
		"Goroutines are multiplexed onto OS threads by the Go runtime.",
		"The Go scheduler handles thousands of goroutines efficiently.",
		"Non-blocking I/O is built into Go's net package.",
		"This is similar to Erlang's lightweight processes.",
		"This is message 13 of 18.",
		"This is message 14 of 18.",
		"This is message 15 of 18.",
		"This is message 16 of 18.",
		"This is message 17 of 18.",
		"[DONE-CONTENT]",
	}

	// Set write deadline once upfront, refresh periodically
	if err := conn.SetWriteDeadline(time.Now().Add(WriteTimeout)); err != nil {
		return 0, fmt.Errorf("set write deadline: %w", err)
	}

	for i, msg := range messages {
		select {
		case <-ctx.Done():
			return totalBytes, ctx.Err()
		default:
		}

		// Send SSE event
		n, err := SendSSE(conn, msg)
		totalBytes += int64(n)
		if err != nil {
			return totalBytes, err
		}

		// Refresh write deadline every 5 writes to reduce syscalls
		if (i+1)%5 == 0 {
			if err := conn.SetWriteDeadline(time.Now().Add(WriteTimeout)); err != nil {
				return totalBytes, fmt.Errorf("set write deadline: %w", err)
			}
		}

		// Simulate token generation delay (configurable via config or -message-delay flag).
		// Use select to allow context cancellation to interrupt the delay,
		// enabling faster shutdown with long message delays.
		if i < len(messages)-1 {
			select {
			case <-time.After(m.messageDelay):
				// Normal delay completed
			case <-ctx.Done():
				return totalBytes, ctx.Err()
			}
		}
	}

	// Check context before sending completion marker for responsive shutdown
	select {
	case <-ctx.Done():
		return totalBytes, ctx.Err()
	default:
	}

	// Send completion marker
	doneMsg := []byte("data: [DONE]\n\n")
	n, err := conn.Write(doneMsg)
	totalBytes += int64(n)
	if err != nil {
		return totalBytes, fmt.Errorf("write failed: %w", err)
	}
	return totalBytes, nil
}

// WriteTimeout is the maximum time for a single write to client.
const WriteTimeout = 30 * time.Second

// sseBufPool reuses buffers for SSE message construction to reduce allocations.
var sseBufPool = sync.Pool{
	New: func() any {
		// Pre-allocate buffer for typical SSE message: "data: {"content":"..."}\n\n"
		buf := make([]byte, 0, 256)
		return &buf
	},
}

// SendSSE sends a single SSE event with the given content.
// Writes directly to conn without buffering for lowest latency.
// Returns bytes written and any error.
// Note: Write deadline should be set by caller for better performance.
func SendSSE(conn net.Conn, content string) (int, error) {
	// Get buffer from pool
	bufPtr := sseBufPool.Get().(*[]byte)
	buf := (*bufPtr)[:0]

	// Manually construct JSON to avoid map allocation and reflection.
	// Format: data: {"content":"<escaped-content>"}\n\n
	buf = append(buf, "data: {\"content\":\""...)
	buf = appendJSONEscaped(buf, content)
	buf = append(buf, "\"}\n\n"...)

	n, err := conn.Write(buf)

	// Return buffer to pool
	*bufPtr = buf
	sseBufPool.Put(bufPtr)

	if err != nil {
		return n, fmt.Errorf("write failed: %w", err)
	}
	return n, nil
}

// appendJSONEscaped appends a JSON-escaped string to buf.
// Only escapes characters required by JSON spec: \ " and control chars.
func appendJSONEscaped(buf []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\', '"':
			buf = append(buf, '\\', c)
		case '\n':
			buf = append(buf, '\\', 'n')
		case '\r':
			buf = append(buf, '\\', 'r')
		case '\t':
			buf = append(buf, '\\', 't')
		default:
			if c < 0x20 {
				// Control characters (0x00-0x1F) must be escaped as \uXXXX.
				buf = append(buf, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
			} else {
				buf = append(buf, c)
			}
		}
	}
	return buf
}

const hexDigits = "0123456789abcdef"

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
