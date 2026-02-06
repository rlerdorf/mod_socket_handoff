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
	"log/slog"
	"net"
	"net/http"
	_ "net/http/pprof" // Register pprof handlers for profiling
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
	maxStreamDurationFlag = flag.Int("max-stream-duration-ms", 0,
		"Maximum per-stream duration in ms (overrides config file, 0 = use config)")
	memLimitFlag = flag.String("memlimit", "",
		"Soft memory limit, e.g. 512MiB, 1GiB (overrides config file)")
	gcPercentFlag = flag.Int("gc-percent", -1,
		"GOGC value; -1 = use Go default, 0 = disable GC (overrides config file)")
)

// activeBackend holds the initialized backend for streaming
var activeBackend backends.Backend

// maxStreamDuration is the maximum duration for a single stream.
// Set once in main(), read-only after. Zero means no timeout.
var maxStreamDuration time.Duration

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

// initLogging configures the default slog logger based on LoggingConfig.
func initLogging(cfg config.LoggingConfig) {
	var level slog.Level
	switch strings.ToLower(cfg.Level) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{
		Level:     level,
		AddSource: true,
	}

	var handler slog.Handler
	if strings.ToLower(cfg.Format) == "json" {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		handler = slog.NewTextHandler(os.Stderr, opts)
	}

	slog.SetDefault(slog.New(handler))
}

func main() {
	flag.Parse()

	// Load configuration (defaults -> config file -> flags)
	cfg := config.Default()

	if *configFile != "" {
		fileCfg, err := config.Load(*configFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to load config file: %v\n", err)
			os.Exit(1)
		}
		cfg = fileCfg
	}

	// Initialize structured logging based on config (before any log output)
	initLogging(cfg.Logging)

	if *configFile != "" {
		slog.Info("loaded config", "path", *configFile)
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

	// Resolve per-stream timeout: flag overrides config
	if *maxStreamDurationFlag > 0 {
		maxStreamDuration = time.Duration(*maxStreamDurationFlag) * time.Millisecond
	} else if cfg.Server.MaxStreamDurationMs > 0 {
		maxStreamDuration = time.Duration(cfg.Server.MaxStreamDurationMs) * time.Millisecond
	}
	if maxStreamDuration > 0 {
		slog.Info("max stream duration configured", "duration", maxStreamDuration)
	}

	// Configure memory limit (flag > config)
	memLimit := cfg.Server.MemLimit
	if *memLimitFlag != "" {
		memLimit = *memLimitFlag
	}
	if memLimit != "" {
		limit := config.ParseMemLimit(memLimit)
		if limit <= 0 {
			slog.Error("invalid memlimit value", "value", memLimit)
			os.Exit(1)
		}
		debug.SetMemoryLimit(limit)
		slog.Info("memory limit set", "limit", memLimit, "bytes", limit)
	}

	// Configure GC percent (flag > config).
	// GOGC=0 is valid (disables GC, useful with memlimit), so we track whether
	// the value was explicitly set rather than testing > 0. The config struct's
	// zero value (0) is ambiguous, so gc_percent=0 can only be set via the flag.
	gcPercent := cfg.Server.GCPercent
	gcPercentSet := cfg.Server.GCPercent != 0
	if *gcPercentFlag >= 0 {
		gcPercent = *gcPercentFlag
		gcPercentSet = true
	}
	if gcPercentSet {
		old := debug.SetGCPercent(gcPercent)
		slog.Info("GC percent set", "value", gcPercent, "previous", old)
	}

	// Initialize the selected backend
	activeBackend = backends.Get(cfg.Backend.Provider)
	if activeBackend == nil {
		available := backends.List()
		slog.Error("unknown backend", "provider", cfg.Backend.Provider, "available", available)
		os.Exit(1)
	}
	if err := activeBackend.Init(&cfg.Backend); err != nil {
		slog.Error("failed to initialize backend", "provider", cfg.Backend.Provider, "error", err)
		os.Exit(1)
	}
	slog.Info("using backend", "name", activeBackend.Name(), "description", activeBackend.Description())

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
		slog.Warn("could not get fd limit", "error", err)
	} else {
		desiredLimit := uint64(cfg.Server.MaxConnections + 2000)
		if rLimit.Cur < desiredLimit {
			rLimit.Cur = desiredLimit
			if rLimit.Max < desiredLimit {
				rLimit.Max = desiredLimit
			}
			if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rLimit); err != nil {
				slog.Warn("could not set fd limit", "desired", desiredLimit, "error", err, "current", rLimit.Cur)
			} else {
				slog.Info("increased fd limit", "limit", desiredLimit)
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
				slog.Info("received SIGHUP", "active_connections", atomic.LoadInt64(&activeConns))
			default:
				slog.Info("received signal, shutting down", "signal", sig)
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
			slog.Error("path exists but is not a socket", "path", cfg.Server.SocketPath)
			os.Exit(1)
		}

		// Probe to see if another daemon is listening
		probeConn, probeErr := net.DialTimeout("unix", cfg.Server.SocketPath, time.Second)
		if probeErr == nil {
			// Connection succeeded - another daemon is running
			probeConn.Close()
			slog.Error("socket already in use by another process", "path", cfg.Server.SocketPath)
			os.Exit(1)
		}

		// Check if the error indicates a stale socket or benign race condition
		// ECONNREFUSED = socket exists but no listener (stale)
		// ENOENT = socket removed between Lstat and Dial (race, safe to proceed)
		if isConnectionRefused(probeErr) {
			// Socket is stale - safe to remove
			// Ignore NotFound errors (race with another process removing it)
			if err := os.Remove(cfg.Server.SocketPath); err != nil && !os.IsNotExist(err) {
				slog.Error("failed to remove stale socket", "path", cfg.Server.SocketPath, "error", err)
				os.Exit(1)
			}
			slog.Info("removed stale socket file", "path", cfg.Server.SocketPath)
		} else if isNotExist(probeErr) {
			// Socket was removed between Lstat and Dial - safe to proceed
			slog.Info("socket file was removed (race), proceeding", "path", cfg.Server.SocketPath)
		} else {
			// Other error (permission denied, etc.) - can't determine if socket is in use
			slog.Error("cannot determine if socket is in use", "path", cfg.Server.SocketPath, "error", probeErr)
			os.Exit(1)
		}
	}

	// Create Unix socket listener
	listener, err := net.Listen("unix", cfg.Server.SocketPath)
	if err != nil {
		slog.Error("failed to listen", "path", cfg.Server.SocketPath, "error", err)
		os.Exit(1)
	}
	// Ensure listener is closed on exit (backup for panics); also closed explicitly
	// after ctx.Done() during graceful shutdown to unblock the accept loop.
	defer listener.Close()

	// Set socket permissions. Default is 0660 (owner + group only).
	// Apache (www-data) must be in the daemon's group to connect.
	// Use -socket-mode=0666 only for testing, never in production.
	if err := os.Chmod(cfg.Server.SocketPath, os.FileMode(cfg.Server.SocketMode)); err != nil {
		slog.Warn("could not chmod socket", "error", err)
	}

	slog.Info("streaming daemon listening", "socket", cfg.Server.SocketPath, "max_connections", cfg.Server.MaxConnections)

	// Start Prometheus metrics server (keep reference for graceful shutdown)
	var metricsServer *http.Server
	if cfg.Metrics.Enabled && cfg.Metrics.ListenAddr != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		mux.HandleFunc("/healthz", healthHandler)
		metricsServer = &http.Server{
			Addr:              cfg.Metrics.ListenAddr,
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			slog.Info("metrics server listening", "addr", cfg.Metrics.ListenAddr)
			if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("metrics server error", "error", err)
			}
		}()
	}

	// Start pprof server for profiling (heap, goroutine, GC analysis)
	var pprofServer *http.Server
	if cfg.Server.PprofAddr != "" {
		pprofServer = &http.Server{
			Addr:              cfg.Server.PprofAddr,
			Handler:           nil, // Uses default mux which has pprof handlers registered via import
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			slog.Info("pprof server listening", "addr", cfg.Server.PprofAddr)
			if err := pprofServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("pprof server error", "error", err)
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
					slog.Error("accept error", "error", err)
					time.Sleep(100 * time.Millisecond) // Back off on errors
					continue
				}
			}

			// Check if context is cancelled before processing connection
			select {
			case <-ctx.Done():
				if err := conn.Close(); err != nil {
					slog.Error("error closing connection during shutdown", "error", err)
				}
				return
			default:
			}

			// Acquire semaphore (limit concurrent connections)
			select {
			case connSemaphore <- struct{}{}:
				conn := conn // local copy for goroutine closure
				atomic.AddInt64(&activeConns, 1)
				if !*benchmarkMode {
					metricConnectionsTotal.Inc()
					metricActiveConnections.Inc()
				}
				connWg.Go(func() {
					defer func() {
						<-connSemaphore
						atomic.AddInt64(&activeConns, -1)
						if !*benchmarkMode {
							metricActiveConnections.Dec()
						}
					}()
					// Check context again inside goroutine to handle race condition
					select {
					case <-ctx.Done():
						if err := conn.Close(); err != nil {
							slog.Error("error closing connection during shutdown", "error", err)
						}
						return
					default:
					}
					safeHandleConnection(ctx, conn)
				})
			default:
				if !*benchmarkMode {
					metricConnectionsRejected.Inc()
				}
				slog.Warn("connection limit reached, rejecting", "max", cfg.Server.MaxConnections)
				if err := conn.Close(); err != nil {
					slog.Error("error closing rejected connection", "error", err)
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

	// Shut down HTTP servers gracefully
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if metricsServer != nil {
		if err := metricsServer.Shutdown(shutdownCtx); err != nil {
			slog.Error("metrics server shutdown error", "error", err)
		}
	}
	if pprofServer != nil {
		if err := pprofServer.Shutdown(shutdownCtx); err != nil {
			slog.Error("pprof server shutdown error", "error", err)
		}
	}

	// Wait for active connections to finish
	slog.Info("waiting for active connections to finish", "count", atomic.LoadInt64(&activeConns))
	done := make(chan struct{})
	go func() {
		connWg.Wait()
		close(done)
	}()

	select {
	case <-done:
		slog.Info("all connections closed gracefully")
	case <-time.After(ShutdownTimeout):
		slog.Warn("timeout waiting for connections", "still_active", atomic.LoadInt64(&activeConns))
	}

	// Print benchmark summary if in benchmark mode
	if *benchmarkMode {
		benchStats.print()
	}

	// Cleanup
	os.Remove(cfg.Server.SocketPath)
	slog.Info("daemon stopped")
}

// safeHandleConnection wraps handleConnection with panic recovery.
// This outer recovery catches panics that occur before we have a client connection
// (e.g., during fd receive). Panics after client connection is established are
// handled inside handleConnection where we can send an error response.
func safeHandleConnection(ctx context.Context, conn net.Conn) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic in connection handler (pre-client)", "panic", r, "stack", string(debug.Stack()))
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
		slog.Error("error closing Apache connection", "error", closeErr)
	}

	if err != nil {
		if !*benchmarkMode {
			metricHandoffErrors.WithLabelValues(classifyError(err)).Inc()
		}
		slog.Error("failed to receive fd", "error", err)
		return
	}
	handoffDuration := time.Since(handoffStart).Seconds()
	if !*benchmarkMode {
		metricHandoffDuration.Observe(handoffDuration)
		metricHandoffsTotal.Inc()
	}

	// Wrap fd in os.File to use net.FileConn below.
	// os.NewFile takes ownership only on success; if it returns nil (invalid fd),
	// we must close the fd ourselves with syscall.Close().
	clientFile := os.NewFile(uintptr(clientFd), "client")
	if clientFile == nil {
		slog.Error("failed to create file from fd", "fd", clientFd)
		syscall.Close(clientFd)
		return
	}

	// Validate that the received fd is actually a stream socket.
	// This defends against malicious or buggy senders passing non-socket fds.
	sockType, sockErr := syscall.GetsockoptInt(clientFd, syscall.SOL_SOCKET, syscall.SO_TYPE)
	if sockErr != nil {
		slog.Error("fd is not a valid socket", "fd", clientFd, "error", sockErr)
		clientFile.Close()
		return
	}
	if sockType != syscall.SOCK_STREAM {
		slog.Error("fd has unexpected socket type", "fd", clientFd, "type", sockType, "expected", syscall.SOCK_STREAM)
		clientFile.Close()
		return
	}

	// Create net.Conn from file (dups the fd). Close clientFile immediately
	// after — the dup is independent, so only one fd is held during streaming.
	clientConn, err := net.FileConn(clientFile)
	clientFile.Close() // original fd no longer needed
	if err != nil {
		slog.Error("failed to create conn from fd", "fd", clientFd, "error", err)
		return
	}
	defer clientConn.Close()

	// Enable TCP keepalive to detect dead connections during long streams
	if tcpConn, ok := clientConn.(*net.TCPConn); ok {
		if err := tcpConn.SetKeepAlive(true); err != nil {
			slog.Warn("could not enable TCP keepalive", "error", err)
		}
		if err := tcpConn.SetKeepAlivePeriod(15 * time.Second); err != nil {
			slog.Warn("could not set TCP keepalive period", "error", err)
		}
	}

	// Panic recovery with error response to client. This runs after we have
	// the client connection, so we can send a 500 error before closing.
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic in connection handler", "panic", r, "stack", string(debug.Stack()))
			// Attempt to send error response to client. This may fail if
			// headers were already sent, but we try anyway for better UX.
			errorResponse := "HTTP/1.1 500 Internal Server Error\r\n" +
				"Content-Type: text/plain\r\n" +
				"Connection: close\r\n" +
				"\r\n" +
				"Internal server error\n"
			if _, err := clientConn.Write([]byte(errorResponse)); err != nil {
				slog.Error("failed to send error response to client", "error", err)
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
			slog.Error("failed to parse handoff data", "error", err)
		}
	}

	// Per-stream context timeout to prevent runaway/hung streams
	var streamCtx context.Context
	var streamCancel context.CancelFunc
	if maxStreamDuration > 0 {
		streamCtx, streamCancel = context.WithTimeout(ctx, maxStreamDuration)
	} else {
		streamCtx, streamCancel = context.WithCancel(ctx)
	}
	defer streamCancel()

	// Stream response to client
	if *benchmarkMode {
		benchStats.streamStart()
	} else {
		current := atomic.AddInt64(&activeStreams, 1)
		metricActiveStreams.Set(float64(current))
		// Update peak using CAS loop (lock-free)
		for {
			peak := atomic.LoadInt64(&peakStreams)
			if current <= peak {
				break
			}
			if atomic.CompareAndSwapInt64(&peakStreams, peak, current) {
				metricPeakStreams.Set(float64(current))
				break
			}
		}
	}

	streamStart := time.Now()
	bytesSent, err := streamToClientWithBytes(streamCtx, clientConn, handoff)
	success := err == nil
	if err != nil {
		if !*benchmarkMode {
			metricStreamErrors.WithLabelValues(classifyError(err)).Inc()
		}
		slog.Error("stream error", "error", err)
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
		slog.Debug("MSG_CTRUNC set", "oobn", oobn, "n", n, "recvflags", recvflags)
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
				slog.Warn("failed to parse Unix rights for cleanup", "error", err)
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
			slog.Warn("failed to parse SCM_RIGHTS control message", "error", err)
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
		slog.Warn("received multiple fds, expected 1; closing extras", "count", len(allFds))
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
func streamToClientWithBytes(ctx context.Context, conn net.Conn, handoff backends.HandoffData) (int64, error) {
	var totalBytes int64

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

// healthHandler returns a JSON health check response with active stream/connection counts.
func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	conns := atomic.LoadInt64(&activeConns)
	streams := atomic.LoadInt64(&activeStreams)
	fmt.Fprintf(w, `{"status":"ok","active_streams":%d,"active_connections":%d}`, streams, conns)
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
