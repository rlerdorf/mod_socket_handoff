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
	"strings"
	"syscall"
	"time"
)

const (
	// Socket path - must match SocketHandoffAllowedPrefix in Apache config
	DaemonSocket = "/var/run/streaming-daemon.sock"
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

	// Receive file descriptor and handoff data from Apache
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
			log.Printf("Failed to parse handoff data: %v", err)
		}
	}

	log.Printf("  user_id=%d, prompt=%q", handoff.UserID, truncate(handoff.Prompt, 50))

	// Stream response to client
	if err := streamToClient(ctx, clientFd, handoff); err != nil {
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

	// Buffer for handoff data
	buf := make([]byte, 4096)

	// Buffer for control message (SCM_RIGHTS)
	// Size: 16 bytes for Cmsghdr on 64-bit Linux + 4 bytes for the fd
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

	return fds[0], buf[:n], nil
}

// streamToClient sends an SSE response to the client.
// Replace the demo response with your LLM API integration.
func streamToClient(ctx context.Context, clientFd int, handoff HandoffData) error {
	// Wrap the fd in an os.File for easier I/O.
	// IMPORTANT: os.NewFile takes ownership of the fd. Use file.Close() to close it,
	// NOT syscall.Close(). The defer ensures the fd stays valid during streaming.
	file := os.NewFile(uintptr(clientFd), "client")
	if file == nil {
		return fmt.Errorf("could not create file from fd")
	}
	defer file.Close()

	writer := bufio.NewWriter(file)

	// Send HTTP headers for SSE
	headers := []string{
		"HTTP/1.1 200 OK",
		"Content-Type: text/event-stream",
		"Cache-Control: no-cache",
		"Connection: close",
		"X-Accel-Buffering: no", // Disable buffering in nginx/proxies
	}
	for _, h := range headers {
		fmt.Fprintf(writer, "%s\r\n", h)
	}
	fmt.Fprintf(writer, "\r\n")
	if err := writer.Flush(); err != nil {
		return fmt.Errorf("failed to send headers: %w", err)
	}

	// Stream the response
	// TODO: Replace this with your LLM API call
	return streamDemoResponse(ctx, writer, handoff)
}

// streamDemoResponse simulates an LLM streaming response.
// Replace this with your actual LLM integration, e.g.:
//
//	stream, _ := openai.CreateChatCompletionStream(ctx, request)
//	for {
//	    chunk, err := stream.Recv()
//	    if err == io.EOF { break }
//	    sendSSE(writer, chunk.Choices[0].Delta.Content)
//	}
func streamDemoResponse(ctx context.Context, writer *bufio.Writer, handoff HandoffData) error {
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

		// Send SSE event
		if err := sendSSE(writer, content); err != nil {
			return err
		}

		// Simulate token generation delay
		time.Sleep(50 * time.Millisecond)
	}

	// Send completion marker
	fmt.Fprintf(writer, "data: [DONE]\n\n")
	return writer.Flush()
}

// sendSSE sends a single SSE event with the given content.
func sendSSE(writer *bufio.Writer, content string) error {
	data, _ := json.Marshal(map[string]string{"content": content})
	fmt.Fprintf(writer, "data: %s\n\n", data)
	return writer.Flush()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
