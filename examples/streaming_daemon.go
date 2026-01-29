/*
 * Streaming Daemon Example for mod_socket_handoff
 *
 * This daemon receives client connections from Apache via SCM_RIGHTS
 * and streams responses directly to clients. Use this as a template
 * for integrating with LLM APIs or other streaming services.
 *
 * Build:
 *   go build -o streaming-daemon streaming_daemon.go
 *
 * Run:
 *   sudo ./streaming-daemon
 *
 * Or install as systemd service - see streaming-daemon.service
 */

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	// Socket path - must match SocketHandoffAllowedPrefix in Apache config
	DaemonSocket = "/var/run/streaming-daemon.sock"

	// Timeouts for robustness
	HandoffTimeout  = 5 * time.Second   // Max time to receive fd from Apache
	WriteTimeout    = 30 * time.Second  // Max time for a single write to client
	ShutdownTimeout = 2 * time.Minute   // Graceful shutdown timeout; long enough for LLM streams to complete
	MaxConnections  = 1000              // Max concurrent connections

	// MaxHandoffDataSize is the buffer size for receiving handoff JSON from Apache
	// via the Unix socket. The data originates from the X-Handoff-Data HTTP header.
	// This should be large enough to hold LLM prompts. Increase if needed.
	// Note: Apache's LimitRequestFieldSize (default 8190) limits individual header size.
	MaxHandoffDataSize = 65536 // 64KB
)

// HandoffData is the JSON structure passed from PHP via X-Handoff-Data header.
// Customize this struct to match your application's needs.
type HandoffData struct {
	UserID    int64  `json:"user_id"`
	Prompt    string `json:"prompt"`
	Model     string `json:"model,omitempty"`
	MaxTokens int    `json:"max_tokens,omitempty"`
	Timestamp int64  `json:"timestamp,omitempty"`
}

// Connection tracking for graceful shutdown
var (
	activeConns   int64
	connSemaphore chan struct{}
	connWg        sync.WaitGroup
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	// Initialize connection limiter
	connSemaphore = make(chan struct{}, MaxConnections)

	// Handle shutdown gracefully
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	go func() {
		for sig := range sigChan {
			switch sig {
			case syscall.SIGHUP:
				log.Printf("Received SIGHUP, active connections: %d", atomic.LoadInt64(&activeConns))
			default:
				log.Printf("Received %v, shutting down...", sig)
				signal.Stop(sigChan)
				cancel()
				return
			}
		}
	}()

	// Remove stale socket
	os.Remove(DaemonSocket)

	// Create Unix socket listener
	listener, err := net.Listen("unix", DaemonSocket)
	if err != nil {
		log.Fatalf("Failed to listen on %s: %v", DaemonSocket, err)
	}
	// Ensure listener is closed on exit; also closed explicitly during graceful shutdown
	defer listener.Close()

	// Set permissions so Apache (www-data) can connect.
	// SECURITY: 0666 allows any local user to connect. For production,
	// consider 0660 with proper group ownership, or use directory permissions.
	if err := os.Chmod(DaemonSocket, 0666); err != nil {
		log.Printf("Warning: Could not chmod socket: %v", err)
	}

	log.Printf("Streaming daemon listening on %s (max %d connections)", DaemonSocket, MaxConnections)

	// Accept connections
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				select {
				case <-ctx.Done():
					return
				default:
					log.Printf("Accept error: %v", err)
					time.Sleep(100 * time.Millisecond) // Back off on errors
					continue
				}
			}

			// Check if context is cancelled before processing connection
			select {
			case <-ctx.Done():
				if err := conn.Close(); err != nil {
					log.Printf("Error closing connection during shutdown: %v", err)
				}
				return
			default:
			}

			// Acquire semaphore (limit concurrent connections)
			select {
			case connSemaphore <- struct{}{}:
				connWg.Add(1)
				atomic.AddInt64(&activeConns, 1)
				go func(c net.Conn) {
					defer func() {
						<-connSemaphore
						connWg.Done()
						atomic.AddInt64(&activeConns, -1)
					}()
					// Check context again inside goroutine to handle race condition
					select {
					case <-ctx.Done():
						if err := c.Close(); err != nil {
							log.Printf("Error closing connection during shutdown: %v", err)
						}
						return
					default:
					}
					safeHandleConnection(ctx, c)
				}(conn)
			default:
				log.Printf("Connection limit reached (%d), rejecting", MaxConnections)
				if err := conn.Close(); err != nil {
					log.Printf("Error closing rejected connection: %v", err)
				}
				// Back off slightly to avoid tight accept-reject loops under overload,
				// but exit promptly if context is cancelled.
				select {
				case <-ctx.Done():
					return
				case <-time.After(10 * time.Millisecond):
				}
			}
		}
	}()

	<-ctx.Done()

	// Listener will be closed by defer; wait for active connections to finish
	log.Printf("Waiting for %d active connections to finish...", atomic.LoadInt64(&activeConns))
	done := make(chan struct{})
	go func() {
		connWg.Wait()
		close(done)
	}()

	select {
	case <-done:
		log.Println("All connections closed gracefully")
	case <-time.After(ShutdownTimeout):
		log.Printf("Timeout waiting for connections, %d still active", atomic.LoadInt64(&activeConns))
	}

	// Cleanup
	os.Remove(DaemonSocket)
	log.Println("Daemon stopped")
}

// safeHandleConnection wraps handleConnection with panic recovery
func safeHandleConnection(ctx context.Context, conn net.Conn) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Panic in connection handler: %v\n%s", r, debug.Stack())
		}
	}()
	handleConnection(ctx, conn)
}

func handleConnection(ctx context.Context, conn net.Conn) {
	defer func() {
		if err := conn.Close(); err != nil {
			log.Printf("Error closing connection: %v", err)
		}
	}()

	// Receive file descriptor and handoff data from Apache
	clientFd, data, err := receiveFd(conn)
	if err != nil {
		log.Printf("Failed to receive fd: %v", err)
		return
	}

	// Wrap fd in os.File immediately to ensure cleanup on any error path.
	// os.NewFile takes ownership only on success; if it returns nil (invalid fd),
	// we must close the fd ourselves with syscall.Close().
	clientFile := os.NewFile(uintptr(clientFd), "client")
	if clientFile == nil {
		log.Printf("Failed to create file from fd %d", clientFd)
		syscall.Close(clientFd)
		return
	}
	defer clientFile.Close()

	log.Printf("Received handoff: fd=%d, data_len=%d", clientFd, len(data))

	// Parse handoff data
	var handoff HandoffData
	if len(data) > 0 {
		if err := json.Unmarshal(data, &handoff); err != nil {
			log.Printf("Failed to parse handoff data: %v", err)
		}
	}

	log.Printf("  user_id=%d, prompt=%q", handoff.UserID, truncate(handoff.Prompt, 50))

	// Stream response to client
	if err := streamToClient(ctx, clientFile, handoff); err != nil {
		log.Printf("Stream error: %v", err)
	}

	log.Printf("  Connection closed")
}

// receiveFd receives a file descriptor and data from the Unix socket connection.
// Apache sends the client socket fd via SCM_RIGHTS along with the handoff data.
func receiveFd(conn net.Conn) (int, []byte, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return -1, nil, fmt.Errorf("not a Unix connection")
	}

	file, err := unixConn.File()
	if err != nil {
		return -1, nil, fmt.Errorf("could not get file: %w", err)
	}
	defer file.Close()

	// Set receive timeout on the raw fd using SO_RCVTIMEO (Unix-specific).
	// We use SO_RCVTIMEO instead of conn.SetReadDeadline because we call
	// syscall.Recvmsg directly on the fd, bypassing Go's net.Conn deadline logic.
	// Use NsecToTimeval for portability across Unix platforms (Timeval field types vary).
	tv := syscall.NsecToTimeval(HandoffTimeout.Nanoseconds())
	if err := syscall.SetsockoptTimeval(int(file.Fd()), syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv); err != nil {
		return -1, nil, fmt.Errorf("could not set receive timeout: %w", err)
	}

	// Buffer for handoff data (sized to hold large LLM prompts)
	buf := make([]byte, MaxHandoffDataSize)

	// Buffer for control message (SCM_RIGHTS)
	// Size: 16 bytes for Cmsghdr on 64-bit Linux + 4 bytes for the fd
	oob := make([]byte, 24)

	n, oobn, recvflags, _, err := syscall.Recvmsg(int(file.Fd()), buf, oob, 0)
	if err != nil {
		return -1, nil, fmt.Errorf("recvmsg failed: %w", err)
	}

	// Check MSG_TRUNC flag to detect if data was truncated
	if recvflags&syscall.MSG_TRUNC != 0 {
		return -1, nil, fmt.Errorf("handoff data truncated (exceeded %d byte buffer); increase MaxHandoffDataSize", MaxHandoffDataSize)
	}

	// Parse control message to extract fd
	msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return -1, nil, fmt.Errorf("parse control message failed: %w", err)
	}

	if len(msgs) == 0 {
		return -1, nil, fmt.Errorf("no control messages received")
	}

	fds, err := syscall.ParseUnixRights(&msgs[0])
	if err != nil {
		return -1, nil, fmt.Errorf("parse unix rights failed: %w", err)
	}

	if len(fds) == 0 {
		return -1, nil, fmt.Errorf("no file descriptors received")
	}

	return fds[0], buf[:n], nil
}

// streamToClient sends an SSE response to the client.
// Replace the demo response with your LLM API integration.
func streamToClient(ctx context.Context, clientFile *os.File, handoff HandoffData) error {
	// Create net.Conn from file to enable write deadlines
	// We must write to this conn (not clientFile) for the deadline to apply
	conn, err := net.FileConn(clientFile)
	if err != nil {
		return fmt.Errorf("failed to create conn from file: %w", err)
	}
	defer conn.Close()

	// Set initial write timeout - fail fast if we can't set deadline
	if err := conn.SetWriteDeadline(time.Now().Add(WriteTimeout)); err != nil {
		return fmt.Errorf("could not set write deadline: %w", err)
	}

	writer := bufio.NewWriter(conn)

	// Send HTTP headers for SSE
	headers := []string{
		"HTTP/1.1 200 OK",
		"Content-Type: text/event-stream",
		"Cache-Control: no-cache",
		"Connection: close",
		"X-Accel-Buffering: no", // Disable buffering in nginx/proxies
	}
	for _, h := range headers {
		if _, err := fmt.Fprintf(writer, "%s\r\n", h); err != nil {
			return fmt.Errorf("failed to write header: %w", err)
		}
	}
	if _, err := fmt.Fprintf(writer, "\r\n"); err != nil {
		return fmt.Errorf("failed to write header terminator: %w", err)
	}
	if err := writer.Flush(); err != nil {
		return fmt.Errorf("failed to send headers: %w", err)
	}

	// Check for context cancellation before starting to stream
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// Reset deadline after successful header write
	if err := conn.SetWriteDeadline(time.Now().Add(WriteTimeout)); err != nil {
		return fmt.Errorf("could not reset write deadline: %w", err)
	}

	// Stream the response
	// TODO: Replace this with your LLM API call
	return streamDemoResponse(ctx, conn, writer, handoff)
}

// streamDemoResponse simulates an LLM streaming response.
// Replace this with your actual LLM integration, e.g.:
//
//	stream, _ := openai.CreateChatCompletionStream(ctx, request)
//	for {
//	    chunk, err := stream.Recv()
//	    if err == io.EOF { break }
//	    sendSSE(conn, writer, chunk.Choices[0].Delta.Content)
//	}
func streamDemoResponse(ctx context.Context, conn net.Conn, writer *bufio.Writer, handoff HandoffData) error {
	response := fmt.Sprintf(
		"Hello! I received your prompt: %q\n\n"+
			"This response is being streamed from a daemon that received "+
			"the client socket via SCM_RIGHTS. The Apache worker has been freed.\n\n"+
			"Replace this demo with your LLM API integration.",
		truncate(handoff.Prompt, 100))

	// Stream word by word to simulate LLM token generation
	words := strings.Fields(response)
	for i, word := range words {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Add space before word (except first)
		content := word
		if i > 0 {
			content = " " + word
		}

		// Send SSE event (resets write deadline after each successful write)
		if err := sendSSE(conn, writer, content); err != nil {
			return err
		}

		// Simulate token generation delay
		time.Sleep(50 * time.Millisecond)
	}

	// Send completion marker
	if _, err := fmt.Fprintf(writer, "data: [DONE]\n\n"); err != nil {
		return fmt.Errorf("write failed: %w", err)
	}
	if err := writer.Flush(); err != nil {
		return fmt.Errorf("flush failed: %w", err)
	}
	return nil
}

// sendSSE sends a single SSE event with the given content.
// It resets the write deadline after each successful flush to implement
// a per-write idle timeout (not a total stream timeout).
func sendSSE(conn net.Conn, writer *bufio.Writer, content string) error {
	// json.Marshal on map[string]string cannot fail with current types;
	// keep error check as defensive programming in case payload changes.
	data, err := json.Marshal(map[string]string{"content": content})
	if err != nil {
		return fmt.Errorf("json marshal failed: %w", err)
	}
	if _, err := fmt.Fprintf(writer, "data: %s\n\n", data); err != nil {
		return fmt.Errorf("write failed: %w", err)
	}
	if err := writer.Flush(); err != nil {
		return fmt.Errorf("flush failed: %w", err)
	}
	// Reset deadline after successful write for per-write idle timeout.
	// Treat failure as fatal so we don't continue streaming without timeout protection.
	if err := conn.SetWriteDeadline(time.Now().Add(WriteTimeout)); err != nil {
		return fmt.Errorf("set write deadline failed: %w", err)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
