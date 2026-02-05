// Shared SSE utilities for all backends.

package backends

import (
	"fmt"
	"net"
	"sync"
	"time"
)

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
