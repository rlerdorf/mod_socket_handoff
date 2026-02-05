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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof" // Register pprof handlers for profiling
	"os"
	"os/signal"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"examples/backends"
	"examples/config"
)

const (
	// Socket path - must match SocketHandoffAllowedPrefix in Apache config
	DaemonSocket = "/var/run/streaming-daemon.sock"

	// Timeouts for robustness
	HandoffTimeout  = 5 * time.Second  // Max time to receive fd from Apache
	WriteTimeout    = 30 * time.Second // Max time for a single write to client
	ShutdownTimeout = 2 * time.Minute  // Graceful shutdown timeout; long enough for LLM streams to complete

	// DefaultMaxConnections is the default maximum concurrent connections.
	// Can be overridden with -max-connections flag for benchmarking.
	DefaultMaxConnections = 50000

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
	configFile = flag.String("config", "",
		"Path to YAML config file (flags override config file values)")
	socketPath = flag.String("socket", "",
		"Unix socket path for daemon (overrides config file)")
	metricsAddr = flag.String("metrics-addr", "",
		"Address for Prometheus metrics server (empty to disable, overrides config file)")
	socketMode = flag.Uint("socket-mode", 0,
		"Permission mode for Unix socket (e.g., 0660, overrides config file)")
	maxConnections = flag.Int("max-connections", 0,
		"Maximum concurrent connections (overrides config file)")
	pprofAddr = flag.String("pprof-addr", "",
		"Address for pprof server (empty to disable, e.g., localhost:6060)")
	benchmarkMode = flag.Bool("benchmark", false,
		"Enable benchmark mode: skip Prometheus updates, print summary on shutdown")
	backendType = flag.String("backend", "",
		"Backend type: langgraph, mock, openai, typing (overrides config file)")
)

// activeBackend holds the initialized backend for streaming
var activeBackend backends.Backend

// Benchmark stats (only updated in benchmark mode)
// Uses atomic operations instead of mutex for better performance under high concurrency.
type BenchmarkStats struct {
	peakStreams    int64
	activeStreams  int64
	totalStarted   int64
	totalCompleted int64
	totalFailed    int64
	totalBytes     int64
}

var benchStats BenchmarkStats

func (b *BenchmarkStats) streamStart() {
	atomic.AddInt64(&b.totalStarted, 1)
	current := atomic.AddInt64(&b.activeStreams, 1)
	// Update peak using CAS loop
	for {
		peak := atomic.LoadInt64(&b.peakStreams)
		if current <= peak {
			break
		}
		if atomic.CompareAndSwapInt64(&b.peakStreams, peak, current) {
			break
		}
	}
}

func (b *BenchmarkStats) streamEnd(success bool, bytesSent int64) {
	atomic.AddInt64(&b.activeStreams, -1)
	if success {
		atomic.AddInt64(&b.totalCompleted, 1)
	} else {
		atomic.AddInt64(&b.totalFailed, 1)
	}
	atomic.AddInt64(&b.totalBytes, bytesSent)
}

func (b *BenchmarkStats) print() {
	fmt.Println("\n=== Benchmark Summary ===")
	fmt.Printf("Peak concurrent streams: %d\n", atomic.LoadInt64(&b.peakStreams))
	fmt.Printf("Total started: %d\n", atomic.LoadInt64(&b.totalStarted))
	fmt.Printf("Total completed: %d\n", atomic.LoadInt64(&b.totalCompleted))
	fmt.Printf("Total failed: %d\n", atomic.LoadInt64(&b.totalFailed))
	fmt.Printf("Total bytes sent: %d\n", atomic.LoadInt64(&b.totalBytes))
	fmt.Println("=========================")
}


// Prometheus metrics
var (
	metricActiveConnections = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "daemon_active_connections",
		Help: "Number of currently active Apache handoff connections",
	})

	metricActiveStreams = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "daemon_active_streams",
		Help: "Number of currently active client streaming connections",
	})

	metricPeakStreams = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "daemon_peak_streams",
		Help: "Peak number of concurrent streaming connections seen",
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


// Connection tracking for graceful shutdown
var (
	activeConns    int64
	activeStreams  int64
	peakStreams    int64
	connSemaphore  chan struct{}
	connWg         sync.WaitGroup
	peakStreamsMux sync.Mutex
)

// handoffBufPool reuses large buffers for receiving handoff data to reduce
// memory pressure and GC overhead under high connection throughput.
var handoffBufPool = sync.Pool{
	New: func() any {
		buf := make([]byte, MaxHandoffDataSize)
		return &buf
	},
}

// oobBufPool reuses buffers for SCM_RIGHTS control messages.
var oobBufPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 256)
		return &buf
	},
}

// Pre-allocated HTTP headers for SSE responses (avoid []byte conversion on every request)
var sseHeadersBytes = []byte("HTTP/1.1 200 OK\r\n" +
	"Content-Type: text/event-stream\r\n" +
	"Cache-Control: no-cache\r\n" +
	"Connection: close\r\n" +
	"X-Accel-Buffering: no\r\n" +
	"\r\n")

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	flag.Parse()

	// Load configuration (defaults -> config file -> flags)
	cfg := config.Default()

	if *configFile != "" {
		fileCfg, err := config.Load(*configFile)
		if err != nil {
			log.Fatalf("Failed to load config file: %v", err)
		}
		cfg = fileCfg
		log.Printf("Loaded config from %s", *configFile)
	}

	// Apply flag overrides (only if explicitly set, i.e., non-zero/non-empty)
	if *socketPath != "" {
		cfg.Server.SocketPath = *socketPath
	}
	if *socketMode != 0 {
		cfg.Server.SocketMode = uint32(*socketMode)
	}
	if *maxConnections != 0 {
		cfg.Server.MaxConnections = *maxConnections
	}
	if *pprofAddr != "" {
		cfg.Server.PprofAddr = *pprofAddr
	}
	// Metrics flag: empty string disables, non-empty enables and sets address
	if *metricsAddr != "" {
		cfg.Metrics.ListenAddr = *metricsAddr
		cfg.Metrics.Enabled = true
	}
	if *backendType != "" {
		cfg.Backend.Provider = *backendType
	}

	// Initialize the selected backend
	activeBackend = backends.Get(cfg.Backend.Provider)
	if activeBackend == nil {
		available := backends.List()
		log.Fatalf("Unknown backend: %s. Available backends: %v", cfg.Backend.Provider, available)
	}
	if err := activeBackend.Init(&cfg.Backend); err != nil {
		log.Fatalf("Failed to initialize backend %s: %v", cfg.Backend.Provider, err)
	}
	log.Printf("Using backend: %s (%s)", activeBackend.Name(), activeBackend.Description())

	// Set benchmark mode for backends package (skips Prometheus updates)
	if *benchmarkMode {
		backends.SetBenchmarkMode(true)
	}

	// Increase file descriptor limit to handle many concurrent connections.
	// Each active stream needs 1 fd for the client socket. Upstream HTTP
	// connections are pooled (100-500 fds shared across all streams).
	// Add headroom for the upstream pool, listening socket, and system overhead.
	var rLimit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rLimit); err != nil {
		log.Printf("Warning: could not get fd limit: %v", err)
	} else {
		desiredLimit := uint64(cfg.Server.MaxConnections + 2000)
		if rLimit.Cur < desiredLimit {
			rLimit.Cur = desiredLimit
			if rLimit.Max < desiredLimit {
				rLimit.Max = desiredLimit
			}
			if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rLimit); err != nil {
				log.Printf("Warning: could not set fd limit to %d: %v (current: %d)",
					desiredLimit, err, rLimit.Cur)
			} else {
				log.Printf("Increased fd limit to %d", desiredLimit)
			}
		}
	}

	// Initialize connection limiter
	connSemaphore = make(chan struct{}, cfg.Server.MaxConnections)

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
	if info, err := os.Lstat(cfg.Server.SocketPath); err == nil {
		// Path exists - verify it's a socket before doing anything
		if info.Mode().Type() != os.ModeSocket {
			log.Fatalf("Path %s exists but is not a socket", cfg.Server.SocketPath)
		}

		// Probe to see if another daemon is listening
		probeConn, probeErr := net.DialTimeout("unix", cfg.Server.SocketPath, time.Second)
		if probeErr == nil {
			// Connection succeeded - another daemon is running
			probeConn.Close()
			log.Fatalf("Socket %s is already in use by another process", cfg.Server.SocketPath)
		}

		// Check if the error indicates a stale socket or benign race condition
		// ECONNREFUSED = socket exists but no listener (stale)
		// ENOENT = socket removed between Lstat and Dial (race, safe to proceed)
		if isConnectionRefused(probeErr) {
			// Socket is stale - safe to remove
			// Ignore NotFound errors (race with another process removing it)
			if err := os.Remove(cfg.Server.SocketPath); err != nil && !os.IsNotExist(err) {
				log.Fatalf("Failed to remove stale socket %s: %v", cfg.Server.SocketPath, err)
			}
			log.Printf("Removed stale socket file %s", cfg.Server.SocketPath)
		} else if isNotExist(probeErr) {
			// Socket was removed between Lstat and Dial - safe to proceed
			log.Printf("Socket file %s was removed (race), proceeding", cfg.Server.SocketPath)
		} else {
			// Other error (permission denied, etc.) - can't determine if socket is in use
			log.Fatalf("Cannot determine if socket %s is in use: %v", cfg.Server.SocketPath, probeErr)
		}
	}

	// Create Unix socket listener
	listener, err := net.Listen("unix", cfg.Server.SocketPath)
	if err != nil {
		log.Fatalf("Failed to listen on %s: %v", cfg.Server.SocketPath, err)
	}
	// Ensure listener is closed on exit (backup for panics); also closed explicitly
	// after ctx.Done() during graceful shutdown to unblock the accept loop.
	defer listener.Close()

	// Set socket permissions. Default is 0660 (owner + group only).
	// Apache (www-data) must be in the daemon's group to connect.
	// Use -socket-mode=0666 only for testing, never in production.
	if err := os.Chmod(cfg.Server.SocketPath, os.FileMode(cfg.Server.SocketMode)); err != nil {
		log.Printf("Warning: Could not chmod socket: %v", err)
	}

	log.Printf("Streaming daemon listening on %s (max %d connections)", cfg.Server.SocketPath, cfg.Server.MaxConnections)

	// Start Prometheus metrics server
	if cfg.Metrics.Enabled && cfg.Metrics.ListenAddr != "" {
		go func() {
			mux := http.NewServeMux()
			mux.Handle("/metrics", promhttp.Handler())
			server := &http.Server{
				Addr:              cfg.Metrics.ListenAddr,
				Handler:           mux,
				ReadHeaderTimeout: 10 * time.Second,
			}
			log.Printf("Metrics server listening on %s", cfg.Metrics.ListenAddr)
			if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("Metrics server error: %v", err)
			}
		}()
	}

	// Start pprof server for profiling (heap, goroutine, GC analysis)
	if cfg.Server.PprofAddr != "" {
		go func() {
			log.Printf("pprof server listening on %s", cfg.Server.PprofAddr)
			// Uses default mux which has pprof handlers registered via import
			if err := http.ListenAndServe(cfg.Server.PprofAddr, nil); err != nil && err != http.ErrServerClosed {
				log.Printf("pprof server error: %v", err)
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
				if !*benchmarkMode {
					metricConnectionsTotal.Inc()
					metricActiveConnections.Inc()
				}
				go func(c net.Conn) {
					defer func() {
						<-connSemaphore
						connWg.Done()
						atomic.AddInt64(&activeConns, -1)
						if !*benchmarkMode {
							metricActiveConnections.Dec()
						}
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
				if !*benchmarkMode {
					metricConnectionsRejected.Inc()
				}
				log.Printf("Connection limit reached (%d), rejecting", cfg.Server.MaxConnections)
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

	// Print benchmark summary if in benchmark mode
	if *benchmarkMode {
		benchStats.print()
	}

	// Cleanup
	os.Remove(cfg.Server.SocketPath)
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
	// Receive file descriptor and handoff data from Apache.
	// Timing includes receiveFd() and conn.Close() to measure full handoff latency.
	handoffStart := time.Now()
	clientFd, data, err := receiveFd(conn)

	// Close Apache connection immediately after receiving fd.
	// The Apache socket is only needed for the brief SCM_RIGHTS handoff.
	// Keeping it open during streaming (10-30+ seconds) wastes file descriptors
	// and can cause fd exhaustion under high concurrency.
	if closeErr := conn.Close(); closeErr != nil {
		log.Printf("Error closing Apache connection: %v", closeErr)
	}

	if err != nil {
		if !*benchmarkMode {
			metricHandoffErrors.WithLabelValues(classifyError(err)).Inc()
		}
		log.Printf("Failed to receive fd: %v", err)
		return
	}
	handoffDuration := time.Since(handoffStart).Seconds()
	if !*benchmarkMode {
		metricHandoffDuration.Observe(handoffDuration)
		metricHandoffsTotal.Inc()
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

	// Parse handoff data
	// Trim NUL bytes - mod_socket_handoff sends a dummy \0 byte when
	// X-Handoff-Data is omitted.
	var handoff backends.HandoffData
	trimmedData := bytes.Trim(data, "\x00")
	if len(trimmedData) > 0 {
		if err := json.Unmarshal(trimmedData, &handoff); err != nil {
			log.Printf("Failed to parse handoff data: %v", err)
		}
	}

	// Stream response to client
	if *benchmarkMode {
		benchStats.streamStart()
	} else {
		current := atomic.AddInt64(&activeStreams, 1)
		metricActiveStreams.Set(float64(current))
		peakStreamsMux.Lock()
		if current > peakStreams {
			peakStreams = current
			metricPeakStreams.Set(float64(current))
		}
		peakStreamsMux.Unlock()
	}

	streamStart := time.Now()
	bytesSent, err := streamToClientWithBytes(ctx, clientFile, handoff)
	success := err == nil
	if err != nil {
		if !*benchmarkMode {
			metricStreamErrors.WithLabelValues(classifyError(err)).Inc()
		}
		log.Printf("Stream error: %v", err)
	}

	if *benchmarkMode {
		benchStats.streamEnd(success, bytesSent)
	} else {
		current := atomic.AddInt64(&activeStreams, -1)
		metricActiveStreams.Set(float64(current))
		metricStreamDuration.Observe(time.Since(streamStart).Seconds())
	}
}

// receiveFd receives a file descriptor and data from the Unix socket connection.
// Apache sends the client socket fd via SCM_RIGHTS along with the handoff data.
func receiveFd(conn net.Conn) (int, []byte, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return -1, nil, fmt.Errorf("not a Unix connection")
	}

	// Set read deadline for timeout
	if err := unixConn.SetReadDeadline(time.Now().Add(HandoffTimeout)); err != nil {
		return -1, nil, fmt.Errorf("could not set read deadline: %w", err)
	}

	// Get buffers from pools to reduce allocations under high throughput
	bufPtr := handoffBufPool.Get().(*[]byte)
	buf := *bufPtr

	oobPtr := oobBufPool.Get().(*[]byte)
	oob := *oobPtr

	// Use ReadMsgUnix instead of File() + syscall.Recvmsg to avoid
	// the issues with File() disconnecting the Go runtime's poller.
	n, oobn, recvflags, _, err := unixConn.ReadMsgUnix(buf, oob)
	if err != nil {
		handoffBufPool.Put(bufPtr)
		oobBufPool.Put(oobPtr)
		return -1, nil, fmt.Errorf("ReadMsgUnix failed: %w", err)
	}

	// Debug: log if control message was truncated
	if recvflags&syscall.MSG_CTRUNC != 0 {
		log.Printf("DEBUG: MSG_CTRUNC set, oobn=%d, n=%d, recvflags=0x%x", oobn, n, recvflags)
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
		oobBufPool.Put(oobPtr)
		return -1, nil, fmt.Errorf("handoff data truncated (exceeded %d byte buffer); increase MaxHandoffDataSize", MaxHandoffDataSize)
	}

	// Check MSG_CTRUNC flag to detect if control message (containing fd) was truncated
	if recvflags&syscall.MSG_CTRUNC != 0 {
		closeReceivedFDs()
		handoffBufPool.Put(bufPtr)
		oobBufPool.Put(oobPtr)
		return -1, nil, fmt.Errorf("control message truncated; fd may be corrupted")
	}

	if parseErr != nil {
		handoffBufPool.Put(bufPtr)
		oobBufPool.Put(oobPtr)
		return -1, nil, fmt.Errorf("parse control message failed: %w", parseErr)
	}

	if len(msgs) == 0 {
		handoffBufPool.Put(bufPtr)
		oobBufPool.Put(oobPtr)
		return -1, nil, fmt.Errorf("no control messages received")
	}

	// Collect all fds from all control messages to avoid leaking any.
	// Normally there's only one message with one fd, but we handle the
	// general case to be safe.
	var allFds []int
	for i := range msgs {
		fds, err := syscall.ParseUnixRights(&msgs[i])
		if err != nil {
			// Not an SCM_RIGHTS message or malformed; log for diagnostics and skip
			log.Printf("Warning: failed to parse SCM_RIGHTS control message: %v", err)
			continue
		}
		allFds = append(allFds, fds...)
	}

	if len(allFds) == 0 {
		handoffBufPool.Put(bufPtr)
		oobBufPool.Put(oobPtr)
		return -1, nil, fmt.Errorf("no file descriptors received")
	}

	// Close any extra fds if multiple were received (we only use the first one)
	if len(allFds) > 1 {
		log.Printf("Warning: received %d fds, expected 1; closing extras", len(allFds))
		for _, extraFd := range allFds[1:] {
			syscall.Close(extraFd)
		}
	}

	// Copy data before returning buffers to pool
	data := make([]byte, n)
	copy(data, buf[:n])
	handoffBufPool.Put(bufPtr)
	oobBufPool.Put(oobPtr)

	return allFds[0], data, nil
}

// streamToClientWithBytes sends an SSE response and returns bytes sent.
func streamToClientWithBytes(ctx context.Context, clientFile *os.File, handoff backends.HandoffData) (int64, error) {
	var totalBytes int64

	// Create net.Conn from file to enable write deadlines.
	// We must write to this conn (not clientFile) for the deadline to apply.
	// Note: net.FileConn dups the fd, so conn has its own fd that must be closed.
	conn, err := net.FileConn(clientFile)
	if err != nil {
		return 0, fmt.Errorf("failed to create conn from file: %w", err)
	}
	defer conn.Close()

	// Set initial write timeout - fail fast if we can't set deadline
	if err := conn.SetWriteDeadline(time.Now().Add(WriteTimeout)); err != nil {
		return 0, fmt.Errorf("could not set write deadline: %w", err)
	}

	// Write directly to conn without buffering for lowest latency SSE streaming.
	// Each write goes straight to the kernel, minimizing TTFB.

	// Send HTTP headers for SSE (use pre-allocated bytes to avoid conversion)
	headerBytes, err := conn.Write(sseHeadersBytes)
	if err != nil {
		return totalBytes, fmt.Errorf("failed to write headers: %w", err)
	}
	totalBytes += int64(headerBytes)
	if !*benchmarkMode {
		metricBytesSent.Add(float64(headerBytes))
	}

	// Check for context cancellation before starting to stream
	select {
	case <-ctx.Done():
		return totalBytes, ctx.Err()
	default:
	}

	// Stream the response using the active backend
	bodyBytes, err := activeBackend.Stream(ctx, conn, handoff)
	totalBytes += bodyBytes
	if !*benchmarkMode && bodyBytes > 0 {
		metricBytesSent.Add(float64(bodyBytes))
	}
	return totalBytes, err
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
