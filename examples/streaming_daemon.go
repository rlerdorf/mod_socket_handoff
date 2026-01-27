/*
 * Streaming Daemon for mod_socket_handoff
 *
 * Production-ready daemon that receives client connections from Apache
 * and streams LLM responses.
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
	"syscall"
	"time"
)

const (
	// Socket path - must match FdpassFilterAllowedPrefix in Apache config
	DaemonSocket = "/var/run/streaming-daemon.sock"
)

// HandoffData is the JSON structure passed from PHP
type HandoffData struct {
	UserID    int64  `json:"user_id"`
	Prompt    string `json:"prompt"`
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens,omitempty"`
	Timestamp int64  `json:"timestamp,omitempty"`
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	// Handle shutdown gracefully
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Println("Shutting down...")
		cancel()
	}()

	// Remove stale socket
	os.Remove(DaemonSocket)

	// Create Unix socket listener
	listener, err := net.Listen("unix", DaemonSocket)
	if err != nil {
		log.Fatalf("Failed to listen on %s: %v", DaemonSocket, err)
	}
	defer listener.Close()

	// Set permissions so Apache (www-data) can connect
	if err := os.Chmod(DaemonSocket, 0666); err != nil {
		log.Printf("Warning: Could not chmod socket: %v", err)
	}

	log.Printf("Streaming daemon listening on %s", DaemonSocket)

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
					continue
				}
			}

			go handleConnection(ctx, conn)
		}
	}()

	<-ctx.Done()

	// Cleanup
	os.Remove(DaemonSocket)
	log.Println("Daemon stopped")
}

func handleConnection(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	// Receive file descriptor and handoff data
	clientFd, data, err := receiveFd(conn)
	if err != nil {
		log.Printf("Failed to receive fd: %v", err)
		return
	}

	log.Printf("Received handoff: fd=%d, data_len=%d", clientFd, len(data))

	// Parse handoff data
	var handoff HandoffData
	if len(data) > 0 {
		if err := json.Unmarshal(data, &handoff); err != nil {
			log.Printf("Failed to parse handoff data: %v (raw: %s)", err, string(data))
			// Continue with empty handoff
		}
	}

	log.Printf("  user_id=%d, prompt=%q", handoff.UserID, truncate(handoff.Prompt, 50))

	// Stream response to client
	if err := streamToClient(ctx, clientFd, handoff); err != nil {
		log.Printf("Stream error: %v", err)
	}

	// Close the client socket
	syscall.Close(clientFd)
	log.Printf("  Connection closed")
}

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

	// Buffer for handoff data
	buf := make([]byte, 4096)

	// Buffer for control message (SCM_RIGHTS)
	// CmsghdrLen is 16 on 64-bit Linux, plus 4 bytes for one int (the fd)
	oob := make([]byte, 24)

	n, oobn, _, _, err := syscall.Recvmsg(int(file.Fd()), buf, oob, 0)
	if err != nil {
		return -1, nil, fmt.Errorf("recvmsg failed: %w", err)
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

	// Return first fd and the data
	return fds[0], buf[:n], nil
}

func streamToClient(ctx context.Context, clientFd int, handoff HandoffData) error {
	// Create file from fd for easier I/O
	file := os.NewFile(uintptr(clientFd), "client")
	if file == nil {
		return fmt.Errorf("could not create file from fd")
	}
	writer := bufio.NewWriter(file)

	// Send HTTP response headers
	// Note: PHP hasn't sent any response, so we send the full HTTP response
	fmt.Fprintf(writer, "HTTP/1.1 200 OK\r\n")
	fmt.Fprintf(writer, "Content-Type: text/event-stream\r\n")
	fmt.Fprintf(writer, "Cache-Control: no-cache\r\n")
	fmt.Fprintf(writer, "Connection: close\r\n")
	fmt.Fprintf(writer, "X-Accel-Buffering: no\r\n")
	fmt.Fprintf(writer, "\r\n")
	if err := writer.Flush(); err != nil {
		return fmt.Errorf("failed to send headers: %w", err)
	}

	// In production, you would call the LLM API here
	// For this example, we simulate a streaming response
	return simulateStream(ctx, writer, handoff)
}

func simulateStream(ctx context.Context, writer *bufio.Writer, handoff HandoffData) error {
	// Simulate an LLM response - streamed token by token with realistic delays
	response := fmt.Sprintf(`Hello! I received your request.

User ID: %d
Prompt: %q

Let me help you with that. Here's my response:

The Apache worker that handled your initial request has already been freed. This response is being streamed directly from a lightweight Go daemon that received the client socket via SCM_RIGHTS file descriptor passing.

This architecture allows:
1. Heavy PHP processes to handle authentication
2. Lightweight daemons to handle long-running streams
3. Better resource utilization under load

The connection handoff is transparent to the client - you see a single HTTP request with a streaming response.

This is the end of my response. Have a great day!`, handoff.UserID, truncate(handoff.Prompt, 100))

	// Split into lines and stream each line with delays
	lines := splitLines(response)

	for i, line := range lines {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Stream each line as a separate SSE event
		data, _ := json.Marshal(map[string]interface{}{
			"content": line,
			"index":   i,
		})
		fmt.Fprintf(writer, "data: %s\n\n", data)
		if err := writer.Flush(); err != nil {
			return fmt.Errorf("write failed: %w", err)
		}

		// Simulate LLM thinking time - varies by line length
		// Shorter lines = faster, longer lines = slower (like real token generation)
		delay := 100 + (len(line) * 10)
		if delay > 500 {
			delay = 500
		}
		time.Sleep(time.Duration(delay) * time.Millisecond)
	}

	// Send done marker
	fmt.Fprintf(writer, "data: [DONE]\n\n")
	return writer.Flush()
}

func splitLines(s string) []string {
	var lines []string
	current := ""
	for _, r := range s {
		if r == '\n' {
			lines = append(lines, current)
			current = ""
		} else {
			current += string(r)
		}
	}
	if current != "" {
		lines = append(lines, current)
	}
	return lines
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
