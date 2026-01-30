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
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
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

	// DefaultMetricsAddr is the default address for the Prometheus metrics HTTP server.
	DefaultMetricsAddr = "127.0.0.1:9090"

	// DefaultSocketMode is the default permission mode for the Unix socket.
	// 0660 restricts access to owner and group only. Apache (www-data) must be
	// in the same group as the daemon, or run the daemon as www-data.
	DefaultSocketMode = 0660
)

// Command-line flags
var (
	metricsAddr = flag.String("metrics-addr", DefaultMetricsAddr,
		"Address for Prometheus metrics server (empty to disable)")
	socketMode = flag.Uint("socket-mode", DefaultSocketMode,
		"Permission mode for Unix socket (e.g., 0660)")
)

// Prometheus metrics
var (
	metricActiveConnections = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "daemon_active_connections",
		Help: "Number of currently active client connections",
	})

	metricConnectionsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "daemon_connections_total",
		Help: "Total number of connections accepted",
	})

	metricConnectionsRejected = promauto.NewCounter(prometheus.CounterOpts{
		Name: "daemon_connections_rejected_total",
		Help: "Total number of connections rejected due to capacity limits",
	})

	metricHandoffsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "daemon_handoffs_total",
		Help: "Total number of successful fd handoffs received",
	})

	metricHandoffErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "daemon_handoff_errors_total",
		Help: "Total number of handoff receive errors",
	}, []string{"reason"})

	metricStreamErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "daemon_stream_errors_total",
		Help: "Total number of stream errors",
	}, []string{"reason"})

	metricBytesSent = promauto.NewCounter(prometheus.CounterOpts{
		Name: "daemon_bytes_sent_total",
		Help: "Total bytes sent to clients",
	})

	metricHandoffDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "daemon_handoff_duration_seconds",
		Help:    "Time spent receiving fd handoff from Apache",
		Buckets: prometheus.ExponentialBuckets(0.0001, 2, 15), // 0.1ms to ~1.6s
	})

	metricStreamDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "daemon_stream_duration_seconds",
		Help:    "Total time spent streaming response to client",
		Buckets: prometheus.ExponentialBuckets(0.01, 2, 15), // 10ms to ~163s
	})
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

// handoffBufPool reuses large buffers for receiving handoff data to reduce
// memory pressure and GC overhead under high connection throughput.
var handoffBufPool = sync.Pool{
	New: func() any {
		buf := make([]byte, MaxHandoffDataSize)
		return &buf
	},
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	flag.Parse()

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

	// Handle existing socket file
	if info, err := os.Lstat(DaemonSocket); err == nil {
		// Path exists - verify it's a socket before doing anything
		if info.Mode().Type() != os.ModeSocket {
			log.Fatalf("Path %s exists but is not a socket", DaemonSocket)
		}

		// Probe to see if another daemon is listening
		probeConn, probeErr := net.DialTimeout("unix", DaemonSocket, time.Second)
		if probeErr == nil {
			// Connection succeeded - another daemon is running
			probeConn.Close()
			log.Fatalf("Socket %s is already in use by another process", DaemonSocket)
		}

		// Check if the error indicates a stale socket or benign race condition
		// ECONNREFUSED = socket exists but no listener (stale)
		// ENOENT = socket removed between Lstat and Dial (race, safe to proceed)
		if isConnectionRefused(probeErr) {
			// Socket is stale - safe to remove
			// Ignore NotFound errors (race with another process removing it)
			if err := os.Remove(DaemonSocket); err != nil && !os.IsNotExist(err) {
				log.Fatalf("Failed to remove stale socket %s: %v", DaemonSocket, err)
			}
			log.Printf("Removed stale socket file %s", DaemonSocket)
		} else if isNotExist(probeErr) {
			// Socket was removed between Lstat and Dial - safe to proceed
			log.Printf("Socket file %s was removed (race), proceeding", DaemonSocket)
		} else {
			// Other error (permission denied, etc.) - can't determine if socket is in use
			log.Fatalf("Cannot determine if socket %s is in use: %v", DaemonSocket, probeErr)
		}
	}

	// Create Unix socket listener
	listener, err := net.Listen("unix", DaemonSocket)
	if err != nil {
		log.Fatalf("Failed to listen on %s: %v", DaemonSocket, err)
	}
	// Ensure listener is closed on exit (backup for panics); also closed explicitly
	// after ctx.Done() during graceful shutdown to unblock the accept loop.
	defer listener.Close()

	// Set socket permissions. Default is 0660 (owner + group only).
	// Apache (www-data) must be in the daemon's group to connect.
	// Use -socket-mode=0666 only for testing, never in production.
	if err := os.Chmod(DaemonSocket, os.FileMode(*socketMode)); err != nil {
		log.Printf("Warning: Could not chmod socket: %v", err)
	}

	log.Printf("Streaming daemon listening on %s (max %d connections)", DaemonSocket, MaxConnections)

	// Start Prometheus metrics server
	if *metricsAddr != "" {
		go func() {
			mux := http.NewServeMux()
			mux.Handle("/metrics", promhttp.Handler())
			server := &http.Server{
				Addr:              *metricsAddr,
				Handler:           mux,
				ReadHeaderTimeout: 10 * time.Second,
			}
			log.Printf("Metrics server listening on %s", *metricsAddr)
			if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("Metrics server error: %v", err)
			}
		}()
	}

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
				metricConnectionsTotal.Inc()
				metricActiveConnections.Inc()
				go func(c net.Conn) {
					defer func() {
						<-connSemaphore
						connWg.Done()
						atomic.AddInt64(&activeConns, -1)
						metricActiveConnections.Dec()
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
				metricConnectionsRejected.Inc()
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

	// Close listener to unblock accept loop immediately. The defer above is kept
	// as a safety net for panics. Calling Close() twice is safe (second returns error).
	listener.Close()

	// Wait for active connections to finish
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

// safeHandleConnection wraps handleConnection with panic recovery.
// This outer recovery catches panics that occur before we have a client connection
// (e.g., during fd receive). Panics after client connection is established are
// handled inside handleConnection where we can send an error response.
func safeHandleConnection(ctx context.Context, conn net.Conn) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Panic in connection handler (pre-client): %v\n%s", r, debug.Stack())
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
	handoffStart := time.Now()
	clientFd, data, err := receiveFd(conn)
	handoffDuration := time.Since(handoffStart).Seconds()
	if err != nil {
		metricHandoffErrors.WithLabelValues(classifyError(err)).Inc()
		log.Printf("Failed to receive fd: %v", err)
		return
	}
	metricHandoffDuration.Observe(handoffDuration)
	metricHandoffsTotal.Inc()

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

	// Validate that the received fd is actually a stream socket.
	// This defends against malicious or buggy senders passing non-socket fds.
	sockType, sockErr := syscall.GetsockoptInt(clientFd, syscall.SOL_SOCKET, syscall.SO_TYPE)
	if sockErr != nil {
		log.Printf("fd %d is not a valid socket: %v", clientFd, sockErr)
		return
	}
	if sockType != syscall.SOCK_STREAM {
		log.Printf("fd %d has unexpected socket type %d (expected SOCK_STREAM)", clientFd, sockType)
		return
	}

	// Panic recovery with error response to client. This runs after we have
	// the client connection, so we can send a 500 error before closing.
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Panic in connection handler: %v\n%s", r, debug.Stack())
			// Attempt to send error response to client. This may fail if
			// headers were already sent, but we try anyway for better UX.
			errorResponse := "HTTP/1.1 500 Internal Server Error\r\n" +
				"Content-Type: text/plain\r\n" +
				"Connection: close\r\n" +
				"\r\n" +
				"Internal server error\n"
			if _, err := clientFile.Write([]byte(errorResponse)); err != nil {
				log.Printf("Failed to send error response to client: %v", err)
			}
		}
	}()

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
	streamStart := time.Now()
	if err := streamToClient(ctx, clientFile, handoff); err != nil {
		metricStreamErrors.WithLabelValues(classifyError(err)).Inc()
		log.Printf("Stream error: %v", err)
	}
	metricStreamDuration.Observe(time.Since(streamStart).Seconds())

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

	// Get buffer from pool to reduce allocations under high throughput
	bufPtr := handoffBufPool.Get().(*[]byte)
	buf := *bufPtr

	// Buffer for control message (SCM_RIGHTS)
	// Use CmsgSpace for portable size calculation across architectures.
	// The argument is the payload size (one int32 fd = 4 bytes).
	oob := make([]byte, syscall.CmsgSpace(4))

	n, oobn, recvflags, _, err := syscall.Recvmsg(int(file.Fd()), buf, oob, 0)
	if err != nil {
		handoffBufPool.Put(bufPtr)
		return -1, nil, fmt.Errorf("recvmsg failed: %w", err)
	}

	// Parse control message to extract fd (do this early so we can close on error).
	// After Recvmsg succeeds, any SCM_RIGHTS fds are already in our fd table.
	msgs, parseErr := syscall.ParseSocketControlMessage(oob[:oobn])

	// Helper to close any received file descriptors to prevent leaks on error paths
	closeReceivedFDs := func() {
		if parseErr != nil {
			return // Can't parse, nothing to close
		}
		for i := range msgs {
			fds, err := syscall.ParseUnixRights(&msgs[i])
			if err != nil {
				log.Printf("Warning: failed to parse Unix rights for cleanup: %v", err)
				continue
			}
			for _, fd := range fds {
				syscall.Close(fd)
			}
		}
	}

	// Check MSG_TRUNC flag to detect if data was truncated
	if recvflags&syscall.MSG_TRUNC != 0 {
		closeReceivedFDs()
		handoffBufPool.Put(bufPtr)
		return -1, nil, fmt.Errorf("handoff data truncated (exceeded %d byte buffer); increase MaxHandoffDataSize", MaxHandoffDataSize)
	}

	// Check MSG_CTRUNC flag to detect if control message (containing fd) was truncated
	if recvflags&syscall.MSG_CTRUNC != 0 {
		closeReceivedFDs()
		handoffBufPool.Put(bufPtr)
		return -1, nil, fmt.Errorf("control message truncated; fd may be corrupted")
	}

	if parseErr != nil {
		handoffBufPool.Put(bufPtr)
		return -1, nil, fmt.Errorf("parse control message failed: %w", parseErr)
	}

	if len(msgs) == 0 {
		handoffBufPool.Put(bufPtr)
		return -1, nil, fmt.Errorf("no control messages received")
	}

	fds, err := syscall.ParseUnixRights(&msgs[0])
	if err != nil {
		handoffBufPool.Put(bufPtr)
		return -1, nil, fmt.Errorf("parse unix rights failed: %w", err)
	}

	if len(fds) == 0 {
		handoffBufPool.Put(bufPtr)
		return -1, nil, fmt.Errorf("no file descriptors received")
	}

	// Close any extra fds if multiple were received (we only use the first one)
	if len(fds) > 1 {
		log.Printf("Warning: received %d fds, expected 1; closing extras", len(fds))
		for _, extraFd := range fds[1:] {
			syscall.Close(extraFd)
		}
	}

	// Copy data before returning buffer to pool
	data := make([]byte, n)
	copy(data, buf[:n])
	handoffBufPool.Put(bufPtr)

	return fds[0], data, nil
}

// streamToClient sends an SSE response to the client.
// Replace the demo response with your LLM API integration.
func streamToClient(ctx context.Context, clientFile *os.File, handoff HandoffData) error {
	// Create net.Conn from file to enable write deadlines.
	// We must write to this conn (not clientFile) for the deadline to apply.
	// Note: net.FileConn dups the fd, so conn has its own fd that must be closed.
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
	var headerBytes int
	for _, h := range headers {
		n, err := fmt.Fprintf(writer, "%s\r\n", h)
		if err != nil {
			return fmt.Errorf("failed to write header: %w", err)
		}
		headerBytes += n
	}
	n, err := fmt.Fprintf(writer, "\r\n")
	if err != nil {
		return fmt.Errorf("failed to write header terminator: %w", err)
	}
	headerBytes += n
	if err := writer.Flush(); err != nil {
		return fmt.Errorf("failed to send headers: %w", err)
	}
	metricBytesSent.Add(float64(headerBytes))

	// Check for context cancellation before starting to stream
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// Stream the response (sendSSE resets write deadline after each successful write)
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

	// Check context before sending completion marker for responsive shutdown
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
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
	msg := fmt.Sprintf("data: %s\n\n", data)
	n, err := writer.WriteString(msg)
	if err != nil {
		return fmt.Errorf("write failed: %w", err)
	}
	if err := writer.Flush(); err != nil {
		return fmt.Errorf("flush failed: %w", err)
	}
	metricBytesSent.Add(float64(n))
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

// classifyError returns a short label for the error type for metrics.
// Uses errors.Is/errors.As to handle wrapped errors correctly.
func classifyError(err error) string {
	if err == nil {
		return "none"
	}
	// Use errors.Is for context errors (handles wrapped errors)
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	// Check for syscall errors first (more specific)
	// Use errors.As to unwrap and find syscall.Errno
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case syscall.EPIPE, syscall.ECONNRESET:
			return "client_disconnected"
		case syscall.ETIMEDOUT:
			return "timeout"
		}
	}
	// Check for net.Error interface (includes timeouts)
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return "timeout"
		}
		return "network"
	}
	return "other"
}

// isConnectionRefused checks if the error is ECONNREFUSED, indicating
// a socket exists but no process is listening (stale socket).
func isConnectionRefused(err error) bool {
	return hasSyscallErrno(err, syscall.ECONNREFUSED)
}

// isNotExist checks if the error is ENOENT, indicating the socket
// file doesn't exist (removed between check and dial).
func isNotExist(err error) bool {
	return hasSyscallErrno(err, syscall.ENOENT)
}

// hasSyscallErrno checks if the error wraps a specific syscall.Errno.
func hasSyscallErrno(err error, target syscall.Errno) bool {
	if err == nil {
		return false
	}
	// Unwrap to find the underlying syscall error
	// net.OpError -> os.SyscallError -> syscall.Errno
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		var syscallErr *os.SyscallError
		if errors.As(opErr.Err, &syscallErr) {
			if errno, ok := syscallErr.Err.(syscall.Errno); ok {
				return errno == target
			}
		}
		// Also check if Err is directly a syscall.Errno
		if errno, ok := opErr.Err.(syscall.Errno); ok {
			return errno == target
		}
	}
	return false
}
