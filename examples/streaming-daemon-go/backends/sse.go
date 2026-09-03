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

// Pre-allocated completion marker shared across backends to avoid allocation in hot paths.
var doneMsg = []byte("data: [DONE]\n\n")

// maxPooledBufSize is the largest buffer a backend puts back into a sync.Pool.
// Buffers that grew beyond this (e.g. to hold a multi-megabyte attachment) are
// left for the GC instead of being kept alive for the next request.
const maxPooledBufSize = 64 << 10

// sseBufPool reuses buffers for SSE message construction to reduce allocations.
var sseBufPool = sync.Pool{
	New: func() any {
		// Pre-allocate buffer for typical SSE message: "data: {"content":"..."}\n\n"
		buf := make([]byte, 0, 256)
		return &buf
	},
}

// putPooledBuf returns buf to pool unless it has grown past maxPooledBufSize.
func putPooledBuf(pool *sync.Pool, bufPtr *[]byte, buf []byte) {
	if cap(buf) > maxPooledBufSize {
		return
	}
	*bufPtr = buf
	pool.Put(bufPtr)
}

// WriteSSE writes one already-framed SSE payload to conn, re-arming the write
// deadline first. SetWriteDeadline is not a syscall (it only updates the
// runtime poller's timer), so doing it per write is cheap, and it is the only
// way to make WriteTimeout mean "time blocked on this write" rather than
// "time since some earlier write" - a slow upstream must not expire the
// client's deadline.
func WriteSSE(conn net.Conn, frame []byte) (int, error) {
	if err := conn.SetWriteDeadline(time.Now().Add(WriteTimeout)); err != nil {
		return 0, fmt.Errorf("set write deadline: %w", err)
	}
	n, err := conn.Write(frame)
	if err != nil {
		return n, fmt.Errorf("write failed: %w", err)
	}
	return n, nil
}

// SendSSEDone writes the "data: [DONE]" completion marker.
func SendSSEDone(conn net.Conn) (int, error) {
	return WriteSSE(conn, doneMsg)
}

// SendSSEError sends an SSE error event to the client.
// Format: data: {"error":"<message>"}\n\n
func SendSSEError(conn net.Conn, errMsg string) error {
	bufPtr := sseBufPool.Get().(*[]byte)
	buf := (*bufPtr)[:0]

	buf = append(buf, "data: {\"error\":\""...)
	buf = appendJSONEscaped(buf, errMsg)
	buf = append(buf, "\"}\n\n"...)

	_, err := WriteSSE(conn, buf)
	putPooledBuf(&sseBufPool, bufPtr, buf)
	return err
}

// SendSSE sends a single SSE event with the given content.
// Writes directly to conn without buffering for lowest latency.
// Returns bytes written and any error. The write deadline is re-armed per call.
func SendSSE(conn net.Conn, content string) (int, error) {
	// Get buffer from pool
	bufPtr := sseBufPool.Get().(*[]byte)
	buf := (*bufPtr)[:0]

	// Manually construct JSON to avoid map allocation and reflection.
	// Format: data: {"content":"<escaped-content>"}\n\n
	buf = append(buf, "data: {\"content\":\""...)
	buf = appendJSONEscaped(buf, content)
	buf = append(buf, "\"}\n\n"...)

	n, err := WriteSSE(conn, buf)
	putPooledBuf(&sseBufPool, bufPtr, buf)
	return n, err
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
