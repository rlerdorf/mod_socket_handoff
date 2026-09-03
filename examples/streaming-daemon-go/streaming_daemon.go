/*
 * Streaming Daemon Example for mod_socket_handoff
 *
 * This daemon receives client connections from Apache via SCM_RIGHTS
 * and streams responses directly to clients. Use this as a template
 * for integrating with LLM APIs or other streaming services.
 *
 * Build:
 *   make                      (or: go build -o streaming-daemon .)
 *
 * Run:
 *   ./streaming-daemon -config config/local.yaml
 *
 * See README.md for configuration, signals and systemd notes.
 */

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	_ "net/http/pprof" // Register pprof handlers for profiling
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"examples/backends"
	"examples/config"
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

// dataDir is the allowed directory for attachment file reads (images, text, etc.).
// Set once in main(), read-only after.
var dataDir string

// currentLogging tracks the last applied logging config so partial
// config reloads can preserve unspecified fields. Protected by
// currentLoggingMu since reloadConfig runs on the SIGHUP goroutine.
var (
	currentLogging   config.LoggingConfig
	currentLoggingMu sync.Mutex
)

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
	activeConns   int64
	activeStreams int64
	peakStreams   int64
	connSemaphore chan struct{}
	connWg        sync.WaitGroup
)

// handoffBufPool reuses large buffers for receiving handoff data to reduce
// memory pressure and GC overhead under high connection throughput.
var handoffBufPool = sync.Pool{
	New: func() any {
		buf := make([]byte, config.MaxHandoffDataSize)
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

// sseHeadersPrefix is sseHeadersBytes without the trailing blank line,
// derived from the single source of truth to avoid drift.
var sseHeadersPrefix = sseHeadersBytes[:len(sseHeadersBytes)-2]

// blockedResponseHeaders that conflict with SSE framing and must not be
// overridden by PHP userspace.
var blockedResponseHeaders = [...]string{
	"content-type",
	"cache-control",
	"connection",
	"x-accel-buffering",
	"transfer-encoding",
	"content-length",
	"content-encoding",
}

// isBlockedHeader reports whether name matches a blocked SSE header
// using case-insensitive comparison without allocating.
func isBlockedHeader(name string) bool {
	for _, blocked := range blockedResponseHeaders {
		if strings.EqualFold(name, blocked) {
			return true
		}
	}
	return false
}

// isValidHeaderName reports whether name is a valid HTTP token per RFC 7230.
// Rejects empty names, non-ASCII bytes, control characters, whitespace, and
// delimiter characters that could cause header smuggling.
func isValidHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		// Valid token chars are ASCII: !#$%&'*+-.0-9A-Z^_`a-z|~
		// Reject control chars (0-31, 127), non-ASCII (>= 128), space,
		// and delimiters: ():;<=>?@[\]{},"/
		if c <= ' ' || c >= 0x7f ||
			c == '(' || c == ')' || c == ',' || c == '/' ||
			c == ':' || c == ';' || c == '<' || c == '=' ||
			c == '>' || c == '?' || c == '@' || c == '[' ||
			c == '\\' || c == ']' || c == '{' || c == '}' ||
			c == '"' {
			return false
		}
	}
	return true
}

// isValidHeaderValue reports whether value is a valid HTTP field-value
// per RFC 7230. Rejects control characters (0x00-0x1F, 0x7F) except
// horizontal tab (0x09).
func isValidHeaderValue(value string) bool {
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c < 0x20 && c != '\t') || c == 0x7f {
			return false
		}
	}
	return true
}

// writeAll writes buf to conn. net.Conn.Write already loops until the whole
// buffer is written or an error occurs, so this only adapts the return type.
func writeAll(conn net.Conn, buf []byte) (int64, error) {
	n, err := conn.Write(buf)
	return int64(n), err
}

// writeErrorResponse writes a complete non-streaming HTTP error response
// (status line, minimal headers, plain-text body) with a write deadline.
// extraHeaders are complete "Name: value" lines appended before the body.
func writeErrorResponse(conn net.Conn, status, body string, extraHeaders ...string) (int64, error) {
	if err := conn.SetWriteDeadline(time.Now().Add(backends.WriteTimeout)); err != nil {
		return 0, fmt.Errorf("could not set write deadline: %w", err)
	}
	buf := make([]byte, 0, 160+len(body))
	buf = append(buf, "HTTP/1.1 "...)
	buf = append(buf, status...)
	buf = append(buf, "\r\nContent-Type: text/plain\r\nContent-Length: "...)
	buf = strconv.AppendInt(buf, int64(len(body)), 10)
	buf = append(buf, "\r\nConnection: close\r\n"...)
	for _, h := range extraHeaders {
		buf = append(buf, h...)
		buf = append(buf, "\r\n"...)
	}
	buf = append(buf, "\r\n"...)
	buf = append(buf, body...)
	return writeAll(conn, buf)
}

// lazyHeaderConn defers the HTTP status line and SSE headers until the backend
// writes its first byte. Backends contact upstream (connect, authenticate,
// possibly create a thread) before they have anything to send; if that fails,
// nothing has reached the client yet and the daemon can still answer with a
// real 502/504 instead of an empty "200 text/event-stream" that just closes.
// The headers ride in the same writev() as the first chunk on TCP connections.
type lazyHeaderConn struct {
	net.Conn
	handoff     backends.HandoffData
	headersSent bool
	headerBytes int64
}

func (c *lazyHeaderConn) Write(p []byte) (int, error) {
	if c.headersSent {
		return c.Conn.Write(p)
	}
	c.headersSent = true
	hdr := buildSSEHeaders(c.handoff)
	bufs := net.Buffers{hdr, p}
	n, err := bufs.WriteTo(c.Conn)
	if n >= int64(len(hdr)) {
		c.headerBytes = int64(len(hdr))
		n -= int64(len(hdr))
	} else {
		c.headerBytes = n
		n = 0
	}
	if err != nil {
		return int(n), fmt.Errorf("failed to write headers: %w", err)
	}
	return int(n), nil
}

// writeSSEHeaders writes the HTTP response status line and headers to conn.
func writeSSEHeaders(conn net.Conn, handoff backends.HandoffData) (int64, error) {
	return writeAll(conn, buildSSEHeaders(handoff))
}

// buildSSEHeaders returns the HTTP response status line and headers for an SSE
// response. When handoff.ResponseHeaders is non-empty, custom headers are
// appended after the standard SSE headers. Headers that conflict with SSE
// framing or have invalid names/values are silently dropped. The returned
// slice must not be modified when it aliases the shared sseHeadersBytes.
func buildSSEHeaders(handoff backends.HandoffData) []byte {
	if len(handoff.ResponseHeaders) == 0 {
		return sseHeadersBytes
	}

	// Collect valid custom headers first to avoid allocating a buffer
	// when all headers end up being dropped (blocked/invalid).
	type hdr struct{ name, value string }
	var valid []hdr
	for name, value := range handoff.ResponseHeaders {
		if !isValidHeaderName(name) || isBlockedHeader(name) || !isValidHeaderValue(value) {
			continue
		}
		valid = append(valid, hdr{name, value})
	}

	// If no custom headers survived validation, use the fast path.
	if len(valid) == 0 {
		return sseHeadersBytes
	}

	// Build response with custom headers.
	// Estimate: prefix + ~64 bytes per custom header + terminator.
	buf := make([]byte, 0, len(sseHeadersPrefix)+64*len(valid)+2)
	buf = append(buf, sseHeadersPrefix...)
	for _, h := range valid {
		buf = append(buf, h.name...)
		buf = append(buf, ':', ' ')
		buf = append(buf, h.value...)
		buf = append(buf, '\r', '\n')
	}
	buf = append(buf, '\r', '\n') // End of headers.

	return buf
}

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

// reloadConfig re-reads the config file and applies safe runtime changes.
// Called on SIGHUP. Only reloads settings that can be changed without restart:
// logging level/format, memory limit, and GC percent. Other settings (such as
// socket path, max connections, backend provider, etc.) still require a daemon
// restart to take effect.
func reloadConfig() {
	if *configFile == "" {
		slog.Warn("SIGHUP: no config file specified (-config), nothing to reload")
		return
	}

	slog.Info("reloading configuration", "path", *configFile)

	newCfg, err := config.LoadRaw(*configFile)
	if err != nil {
		slog.Error("config reload failed, keeping current config", "error", err)
		return
	}

	// Reload logging, merging with current config so partial reloads
	// (e.g. only level specified) don't reset unspecified fields to defaults.
	if newCfg.Logging.Level != "" || newCfg.Logging.Format != "" {
		currentLoggingMu.Lock()
		merged := currentLogging
		if newCfg.Logging.Level != "" {
			merged.Level = newCfg.Logging.Level
		}
		if newCfg.Logging.Format != "" {
			merged.Format = newCfg.Logging.Format
		}
		initLogging(merged)
		currentLogging = merged
		currentLoggingMu.Unlock()
		slog.Info("logging config reloaded", "level", merged.Level, "format", merged.Format)
	}

	// Reload memory limit (flag overrides config)
	memLimit := newCfg.Server.MemLimit
	memLimitSource := "config"
	if *memLimitFlag != "" {
		memLimit = *memLimitFlag
		memLimitSource = "flag"
	}
	if memLimit != "" {
		limit := config.ParseMemLimit(memLimit)
		if limit > 0 {
			debug.SetMemoryLimit(limit)
			slog.Info("memory limit reloaded", "limit", memLimit, "bytes", limit, "source", memLimitSource)
		} else {
			slog.Warn("invalid memlimit value, skipping", "value", memLimit, "source", memLimitSource)
		}
	}

	// Reload GC percent (flag overrides config).
	// Note: gc_percent=0 (disable GC) cannot be set via config file because
	// the zero value is indistinguishable from "not set". Use the -gc-percent
	// flag for GOGC=0. See config.go ServerConfig.GCPercent comment.
	gcPercent := newCfg.Server.GCPercent
	gcPercentSet := newCfg.Server.GCPercent != 0
	if *gcPercentFlag >= 0 {
		gcPercent = *gcPercentFlag
		gcPercentSet = true
	}
	if gcPercentSet {
		old := debug.SetGCPercent(gcPercent)
		slog.Info("GC percent reloaded", "value", gcPercent, "previous", old)
	}

	slog.Info("configuration reloaded successfully")
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
	currentLoggingMu.Lock()
	currentLogging = cfg.Logging
	currentLoggingMu.Unlock()

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

	// Set data directory (default from config, which defaults to /run/handoff-data).
	// Uses /run/ instead of /tmp to avoid systemd PrivateTmp namespace issues
	// where Apache and the daemon see different /tmp directories.
	dataDir = cfg.Server.DataDir
	if dataDir != "" {
		if err := os.MkdirAll(dataDir, 0750); err != nil {
			slog.Warn("could not create data directory", "path", dataDir, "error", err)
		} else {
			slog.Info("data directory ready", "path", dataDir)
		}
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

	// Initialize all backends for per-request routing, then set the default
	initialized := backends.InitAll(&cfg.Backend)
	slog.Info("initialized backends", "backends", initialized)
	activeBackend = backends.GetInitialized(cfg.Backend.Provider)
	if activeBackend == nil {
		slog.Error("default backend not available", "provider", cfg.Backend.Provider, "initialized", initialized)
		os.Exit(1)
	}
	slog.Info("default backend", "name", activeBackend.Name(), "description", activeBackend.Description())

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
			newLimit := rLimit
			newLimit.Cur = desiredLimit
			if newLimit.Max < desiredLimit {
				newLimit.Max = desiredLimit
			}
			err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &newLimit)
			if err != nil && newLimit.Max != rLimit.Max {
				// Raising the hard limit needs CAP_SYS_RESOURCE. Fall back to the
				// most the existing hard limit allows rather than giving up entirely.
				newLimit.Max = rLimit.Max
				newLimit.Cur = rLimit.Max
				err = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &newLimit)
			}
			if err != nil {
				slog.Warn("could not raise fd limit", "desired", desiredLimit, "error", err, "current", rLimit.Cur, "hard", rLimit.Max)
			} else if newLimit.Cur < desiredLimit {
				slog.Warn("fd limit raised to hard limit only; raise the hard limit (LimitNOFILE / ulimit -Hn) for full capacity",
					"limit", newLimit.Cur, "desired", desiredLimit)
			} else {
				slog.Info("increased fd limit", "limit", newLimit.Cur)
			}
		}
	}

	// Initialize connection limiter
	connSemaphore = make(chan struct{}, cfg.Server.MaxConnections)

	// Handle shutdown gracefully.
	// Go 1.26: signal.NotifyContext sets a CancelCauseFunc with the signal,
	// so context.Cause(ctx) returns the signal after cancellation.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// In-flight streams run under drainCtx, not the signal context: a SIGTERM
	// must stop accepting new work, not abort every response mid-stream.
	// drainCtx is cancelled only if streams are still running when
	// ShutdownTimeout expires.
	drainCtx, drainCancel := context.WithCancel(context.Background())
	defer drainCancel()

	// SIGHUP triggers config reload on a separate channel
	sighupChan := make(chan os.Signal, 1)
	signal.Notify(sighupChan, syscall.SIGHUP)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-sighupChan:
				reloadConfig()
			}
		}
	}()
	defer signal.Stop(sighupChan)

	// Handle existing socket file
	if info, err := os.Lstat(cfg.Server.SocketPath); err == nil {
		// Path exists - verify it's a socket before doing anything
		if info.Mode().Type() != os.ModeSocket {
			slog.Error("path exists but is not a socket", "path", cfg.Server.SocketPath)
			os.Exit(1)
		}

		// Probe to see if another daemon is listening
		probeConn, probeErr := net.DialTimeout("unixpacket", cfg.Server.SocketPath, time.Second)
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

	// Create Unix socket listener (SOCK_SEQPACKET for atomic message delivery).
	// The socket inode is created with the process umask applied, and the chmod
	// below only runs afterwards; tighten the umask for the bind so the socket
	// is never briefly more permissive than socket_mode.
	oldUmask := syscall.Umask(0777 &^ int(cfg.Server.SocketMode))
	listener, err := net.Listen("unixpacket", cfg.Server.SocketPath)
	syscall.Umask(oldUmask)
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

	// Sweep stale staged attachment files. Requests normally delete their files
	// as they consume them; this catches leftovers from requests that failed
	// earlier (malformed JSON, rejected at capacity, crashed mid-resolve).
	if dataDir != "" && cfg.Server.DataDirMaxAgeMs > 0 {
		go runDataDirSweeper(ctx, dataDir, time.Duration(cfg.Server.DataDirMaxAgeMs)*time.Millisecond)
	}

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

	// Accept connections. acceptDone is closed when the loop exits, ensuring
	// no new connWg.Go calls race with connWg.Wait during shutdown.
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
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
				connWg.Go(func() { rejectHandoff(conn, "shutting down") })
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
						rejectHandoff(conn, "shutting down")
						return
					default:
					}
					safeHandleConnection(drainCtx, conn)
				})
			default:
				if !*benchmarkMode {
					metricConnectionsRejected.Inc()
				}
				slog.Warn("connection limit reached, rejecting", "max", cfg.Server.MaxConnections)
				// Tell the client rather than dropping it; bounded by HandoffTimeout.
				connWg.Go(func() { rejectHandoff(conn, "connection limit reached") })
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
	// Restore default signal handling so a second SIGINT/SIGTERM during the
	// drain terminates the process immediately instead of being swallowed.
	stop()
	slog.Info("shutting down", "cause", context.Cause(ctx))

	// Close listener to unblock accept loop immediately. The defer above is kept
	// as a safety net for panics. Calling Close() twice is safe (second returns error).
	if err := listener.Close(); err != nil {
		slog.Debug("listener close during shutdown", "error", err)
	}

	// Wait for the accept loop to exit before calling connWg.Wait, so no new
	// connWg.Go (internally Add(1)) calls race with Wait.
	<-acceptDone

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

	// Wait for active streams to finish. They keep running under drainCtx;
	// only if they outlive ShutdownTimeout are they cancelled, which makes
	// backends return promptly and lets the client see an SSE error event.
	slog.Info("waiting for active connections to finish", "count", atomic.LoadInt64(&activeConns))
	done := make(chan struct{})
	go func() {
		connWg.Wait()
		close(done)
	}()

	select {
	case <-done:
		slog.Info("all connections closed gracefully")
	case <-time.After(config.ShutdownTimeout):
		slog.Warn("timeout waiting for connections, cancelling remaining streams", "still_active", atomic.LoadInt64(&activeConns))
		drainCancel()
		select {
		case <-done:
			slog.Info("remaining streams cancelled")
		case <-time.After(config.DrainCancelGrace):
			slog.Warn("streams did not exit after cancellation", "still_active", atomic.LoadInt64(&activeConns))
		}
	}

	// Print benchmark summary if in benchmark mode
	if *benchmarkMode {
		benchStats.print()
	}

	// Cleanup
	if err := os.Remove(cfg.Server.SocketPath); err != nil && !os.IsNotExist(err) {
		slog.Warn("failed to remove socket file", "path", cfg.Server.SocketPath, "error", err)
	}
	slog.Info("daemon stopped")
}

// rejectHandoff answers a handed-off client that the daemon cannot serve
// (capacity limit or shutdown) with a 503, then closes everything.
//
// Simply closing the Apache-side SEQPACKET socket is not an option: the kernel
// would discard the queued SCM_RIGHTS fd, and Apache has already swapped in a
// dummy socket and marked its connection aborted, so nobody would ever answer
// the browser (it sees ERR_EMPTY_RESPONSE). Receiving the fd costs one
// recvmsg and lets us send a proper status with Retry-After.
func rejectHandoff(conn net.Conn, reason string) {
	defer conn.Close()

	clientFd, _, err := receiveFd(conn)
	if err != nil {
		slog.Debug("reject: could not receive fd", "reason", reason, "error", err)
		return
	}
	clientFile := os.NewFile(uintptr(clientFd), "rejected-client")
	if clientFile == nil {
		syscall.Close(clientFd)
		return
	}
	clientConn, err := net.FileConn(clientFile)
	clientFile.Close()
	if err != nil {
		slog.Debug("reject: could not wrap client fd", "reason", reason, "error", err)
		return
	}
	defer clientConn.Close()

	if _, err := writeErrorResponse(clientConn, "503 Service Unavailable",
		"Service temporarily unavailable, please retry\n", "Retry-After: 1"); err != nil {
		slog.Debug("reject: could not write 503", "reason", reason, "error", err)
	}
}

// safeHandleConnection wraps handleConnection with panic recovery.
// This outer recovery catches panics that occur before we have a client connection
// (e.g., during fd receive). Panics after client connection is established are
// handled inside handleConnection where we can send an error response.
func safeHandleConnection(ctx context.Context, conn net.Conn) {
	defer func() {
		if r := recover(); r != nil {
			// Close the Apache handoff connection on panic to avoid fd leaks.
			if closeErr := conn.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
				slog.Error("error closing Apache connection after panic", "error", closeErr)
			}
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

	// Response state shared with the panic handler below: once the SSE headers
	// are on the wire a status line can no longer be sent.
	var lc *lazyHeaderConn

	// Panic recovery with error response to client. This runs after we have
	// the client connection, so we can send an error before closing.
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic in connection handler", "panic", r, "stack", string(debug.Stack()))
			if lc != nil && lc.headersSent {
				// Headers already sent: a status line would corrupt the SSE body.
				if err := backends.SendSSEError(clientConn, "internal server error"); err != nil {
					slog.Debug("failed to send SSE error after panic", "error", err)
				}
				return
			}
			if _, err := writeErrorResponse(clientConn, "500 Internal Server Error", "Internal server error\n"); err != nil {
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
		slog.Debug("socket handoff received", "bytes", len(trimmedData))
		if err := json.Unmarshal(trimmedData, &handoff); err != nil {
			// Do not fall through to a default stream: the caller asked for
			// something specific and we could not understand it.
			slog.Error("failed to parse handoff data", "error", err, "bytes", len(trimmedData))
			if _, writeErr := writeErrorResponse(clientConn, "400 Bad Request", "Malformed handoff data\n"); writeErr != nil {
				slog.Debug("failed to write 400 response", "error", writeErr)
			}
			return
		}
	} else {
		slog.Debug("socket handoff received with empty data")
	}

	// Resolve image paths to inline base64 data
	if err := resolveImages(&handoff, dataDir); err != nil {
		slog.Error("image resolution failed", "error", err)
		discardStagedFiles(&handoff, dataDir)
		if _, writeErr := writeErrorResponse(clientConn, "400 Bad Request", "Image path not allowed\n"); writeErr != nil {
			slog.Debug("failed to write 400 response", "error", writeErr)
		}
		return
	}

	// Resolve generalized attachments (text files, images, etc.)
	if err := resolveAttachments(&handoff, dataDir); err != nil {
		slog.Error("attachment resolution failed", "error", err)
		discardStagedFiles(&handoff, dataDir)
		if _, writeErr := writeErrorResponse(clientConn, "400 Bad Request", "Attachment not allowed\n"); writeErr != nil {
			slog.Debug("failed to write 400 response", "error", writeErr)
		}
		return
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

	// Detect client disconnect while upstream is silent. Nothing else ever
	// reads the client socket, so without this a client that goes away during
	// a long upstream pause keeps the LLM run alive until the next write fails
	// (or max_stream_duration). Apache already consumed the request and the
	// response says Connection: close, so any EOF or error here means "gone".
	// Draining also keeps the eventual close() a FIN rather than a RST.
	var clientClosed atomic.Bool
	go func() {
		_, _ = io.Copy(io.Discard, clientConn)
		clientClosed.Store(true)
		streamCancel()
	}()

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
	slog.Debug("stream request", "assistant_id", handoff.AssistantID, "thread_id", handoff.ThreadID, "backend", handoff.Backend)

	// Accounting runs in a defer so a panic inside a backend (recovered above)
	// cannot leave the active-stream gauges permanently inflated. err starts
	// non-nil so a panic is counted as a failure.
	var bytesSent int64
	err = errStreamPanicked
	defer func() {
		if *benchmarkMode {
			benchStats.streamEnd(err == nil, bytesSent)
			return
		}
		current := atomic.AddInt64(&activeStreams, -1)
		metricActiveStreams.Set(float64(current))
		metricStreamDuration.Observe(time.Since(streamStart).Seconds())
		if err != nil {
			metricStreamErrors.WithLabelValues(classifyError(err)).Inc()
		}
	}()

	lc = &lazyHeaderConn{Conn: clientConn, handoff: handoff}
	bytesSent, err = streamToClientWithBytes(streamCtx, lc, handoff)
	if err != nil {
		if errors.Is(err, context.Canceled) && clientClosed.Load() {
			err = errClientDisconnected
		}
		if errors.Is(err, errClientDisconnected) {
			slog.Info("client disconnected", "bytes_sent", bytesSent, "duration", time.Since(streamStart))
		} else {
			slog.Error("stream error", "error", err, "bytes_sent", bytesSent)
		}
	}
}

// errClientDisconnected marks streams that ended because the client went away.
var errClientDisconnected = errors.New("client disconnected")

// errStreamPanicked is the initial stream error so that a panic inside a
// backend is counted as a failure by the deferred accounting.
var errStreamPanicked = errors.New("stream panicked")

// receiveFd receives a file descriptor and data from the Unix socket connection.
// Apache sends the client socket fd via SCM_RIGHTS along with the handoff data.
func receiveFd(conn net.Conn) (int, []byte, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return -1, nil, fmt.Errorf("not a Unix connection")
	}

	// Set read deadline for timeout
	if err := unixConn.SetReadDeadline(time.Now().Add(config.HandoffTimeout)); err != nil {
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
		return -1, nil, fmt.Errorf("handoff data truncated (exceeded %d byte buffer); increase MaxHandoffDataSize", config.MaxHandoffDataSize)
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

// streamToClientWithBytes runs the resolved backend against conn and returns
// the total bytes sent (headers + body).
//
// conn is normally a *lazyHeaderConn, so the SSE headers go out with the
// backend's first write. If the backend fails before writing anything, the
// client gets a real 502/504 status; if it fails mid-stream, the client gets an
// SSE error event so EventSource consumers can distinguish failure from a
// normal end of stream.
func streamToClientWithBytes(ctx context.Context, conn net.Conn, handoff backends.HandoffData) (int64, error) {
	lc, lazy := conn.(*lazyHeaderConn)
	if !lazy {
		lc = &lazyHeaderConn{Conn: conn, handoff: handoff}
		conn = lc
	}

	// Resolve backend: per-request override or default.
	// Uses GetInitialized (not Get) so only successfully initialized backends
	// can be routed to. Log at Debug to prevent client-triggered log amplification.
	b := activeBackend
	if handoff.Backend != "" {
		if override := backends.GetInitialized(handoff.Backend); override != nil {
			b = override
		} else if backends.Get(handoff.Backend) != nil {
			slog.Debug("backend not initialized, using default", "requested", handoff.Backend, "default", activeBackend.Name())
		} else {
			slog.Debug("unknown backend in handoff, using default", "requested", handoff.Backend, "default", activeBackend.Name())
		}
	}

	// Stream the response using the resolved backend
	bodyBytes, err := b.Stream(ctx, conn, handoff)
	totalBytes := lc.headerBytes + bodyBytes

	if err != nil && classifyError(err) != "client_disconnected" {
		if !lc.headersSent {
			// Nothing reached the client yet: send a real status.
			status, body := "502 Bad Gateway", "Upstream error\n"
			if errors.Is(err, context.DeadlineExceeded) {
				status, body = "504 Gateway Timeout", "Upstream timeout\n"
			} else if errors.Is(err, context.Canceled) {
				status, body = "503 Service Unavailable", "Stream cancelled\n"
			}
			n, werr := writeErrorResponse(lc.Conn, status, body)
			totalBytes += n
			if werr != nil {
				slog.Debug("failed to write error status", "status", status, "error", werr)
			}
		} else {
			msg := "upstream error"
			if errors.Is(err, context.DeadlineExceeded) {
				msg = "stream timed out"
			} else if errors.Is(err, context.Canceled) {
				msg = "stream cancelled"
			}
			if serr := backends.SendSSEError(lc.Conn, msg); serr != nil {
				slog.Debug("failed to send SSE error event", "error", serr)
			}
		}
	}

	if !*benchmarkMode && totalBytes > 0 {
		metricBytesSent.Add(float64(totalBytes))
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

// containedPath resolves p to a symlink-free absolute path and verifies, both
// on the literal path (catches ../ traversal) and after symlink resolution
// (catches symlink escape), that it stays inside allowedDir. Relative paths are
// joined onto allowedDir. outside is true when the containment check failed;
// any other error means the path could not be resolved (typically a missing
// file), which callers treat as skippable.
func containedPath(allowedDir, p string) (resolved string, outside bool, err error) {
	if !filepath.IsAbs(p) {
		p = filepath.Join(allowedDir, p)
	}
	cleaned := filepath.Clean(p)

	// Canonicalize allowedDir so resolved paths compare correctly even when
	// allowedDir contains symlinks (e.g. /var/run -> /run).
	if allowedDir != "" {
		if canon, err := filepath.EvalSymlinks(allowedDir); err == nil {
			allowedDir = canon
		}
	}

	if !isInside(allowedDir, cleaned) {
		return "", true, fmt.Errorf("%q is outside allowed directory %q", cleaned, allowedDir)
	}
	resolved, err = filepath.EvalSymlinks(cleaned)
	if err != nil {
		return "", false, err
	}
	if !isInside(allowedDir, resolved) {
		return "", true, fmt.Errorf("%q resolves outside allowed directory %q", resolved, allowedDir)
	}
	return resolved, false, nil
}

// isInside reports whether path is dir itself or a descendant of it.
func isInside(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// readStagedFile reads one staged attachment file with the open-fstat-read
// pattern and deletes it afterwards (best effort). It enforces the per-file
// limit maxSize and the per-request budget config.MaxTotalAttachmentBytes,
// tracked in handoff.StagedBytes. kind names the file class in error messages.
//
// Returns skip=true (with a reason) for conditions that should be logged and
// ignored: open/stat/read failures and non-regular files. Any returned error
// is fatal for the request; the offending file is removed first.
//
// O_NOFOLLOW prevents symlink following on the final component at open time;
// parent directory swaps remain a theoretical risk (mitigable only with openat
// on a dirfd, not done here). O_NONBLOCK prevents blocking on FIFOs/devices
// (rejected by the IsRegular check below); for regular files it is a no-op.
func readStagedFile(path string, maxSize int64, kind string, handoff *backends.HandoffData) (data []byte, skip error, err error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open failed: %w", err), nil
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat failed: %w", err), nil
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file (mode %v)", info.Mode()), nil
	}
	size := info.Size()
	if size > maxSize {
		os.Remove(path) // best-effort cleanup of staged file
		return nil, nil, fmt.Errorf("%s exceeds %d byte limit (got %d)", kind, maxSize, size)
	}
	if handoff.StagedBytes+size > config.MaxTotalAttachmentBytes {
		os.Remove(path)
		return nil, nil, fmt.Errorf("total attachment size exceeds %d byte limit", config.MaxTotalAttachmentBytes)
	}

	// Use LimitReader to bound memory even if the file grows after stat
	data, err = io.ReadAll(io.LimitReader(f, maxSize+1))
	if int64(len(data)) > maxSize {
		os.Remove(path)
		return nil, nil, fmt.Errorf("%s exceeds %d byte limit during read", kind, maxSize)
	}
	if err != nil {
		return nil, fmt.Errorf("read failed: %w", err), nil
	}
	handoff.StagedBytes += int64(len(data))

	// Best-effort delete by symlink-resolved path
	if err := os.Remove(path); err != nil {
		slog.Warn("staged file delete failed", "path", path, "error", err)
	}
	return data, nil, nil
}

// resolveImages populates handoff.ResolvedImages from inline images and file paths.
// Legacy single-image fields are migrated to the plural fields for backward compatibility.
// Relative image paths are resolved under allowedDir (like attachments); absolute
// paths must already lie inside it. Files are read, base64-encoded, and deleted
// (best-effort). Returns error for path traversal violations and oversized files;
// other file read errors are logged and skipped.
func resolveImages(handoff *backends.HandoffData, allowedDir string) error {
	// Migrate legacy single-image fields
	if handoff.ImageBase64 != "" {
		mime := handoff.ImageMimeType
		if mime == "" {
			mime = "image/jpeg"
		}
		handoff.Images = append(handoff.Images, backends.ImageData{
			Base64:   handoff.ImageBase64,
			MimeType: mime,
		})
		handoff.ImageBase64 = ""
		handoff.ImageMimeType = ""
	}
	if handoff.ImagePath != "" {
		handoff.ImagePaths = append(handoff.ImagePaths, handoff.ImagePath)
		handoff.ImagePath = ""
	}

	// Reset ResolvedImages to prevent duplication if called more than once
	handoff.ResolvedImages = nil

	// Copy inline images to ResolvedImages
	handoff.ResolvedImages = append(handoff.ResolvedImages, handoff.Images...)

	// Load file images
	for _, path := range handoff.ImagePaths {
		resolved, outside, err := containedPath(allowedDir, path)
		if outside {
			return fmt.Errorf("image path %w", err)
		}
		if err != nil {
			slog.Warn("image file resolve failed", "path", path, "error", err)
			continue
		}

		data, skip, err := readStagedFile(resolved, maxBinaryFileSize, "image", handoff)
		if err != nil {
			return fmt.Errorf("image %q: %w", resolved, err)
		}
		if skip != nil {
			slog.Warn("image file skipped", "path", resolved, "reason", skip)
			continue
		}

		handoff.ResolvedImages = append(handoff.ResolvedImages, backends.ImageData{
			Base64:   base64.StdEncoding.EncodeToString(data),
			MimeType: mimeTypeFromExt(filepath.Ext(resolved)),
		})
	}

	return nil
}

// discardStagedFiles removes every image and attachment file referenced by a
// handoff that will not be served (parse failure, rejected attachment, ...).
// Files are normally deleted as they are consumed, but a request that fails on
// its second attachment would otherwise leave the rest on disk until the
// sweeper runs. Only regular files inside allowedDir are touched.
func discardStagedFiles(handoff *backends.HandoffData, allowedDir string) {
	if !filepath.IsAbs(allowedDir) {
		return
	}
	paths := make([]string, 0, len(handoff.ImagePaths)+len(handoff.Attachments)+1)
	paths = append(paths, handoff.ImagePaths...)
	if handoff.ImagePath != "" {
		paths = append(paths, handoff.ImagePath)
	}
	for _, rel := range handoff.Attachments {
		paths = append(paths, filepath.Join(allowedDir, rel))
	}
	for _, p := range paths {
		resolved, outside, err := containedPath(allowedDir, p)
		if outside || err != nil {
			continue
		}
		info, err := os.Lstat(resolved)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if err := os.Remove(resolved); err == nil {
			slog.Debug("discarded staged file", "path", resolved)
		}
	}
}

// runDataDirSweeper periodically removes stale regular files from dir until
// ctx is cancelled. It is the safety net for staged files whose request never
// reached attachment resolution (malformed JSON, rejected at capacity, daemon
// restart between PHP writing the file and the handoff arriving).
func runDataDirSweeper(ctx context.Context, dir string, maxAge time.Duration) {
	interval := maxAge / 2
	if interval < 10*time.Second {
		interval = 10 * time.Second
	}
	slog.Info("data directory sweeper started", "dir", dir, "max_age", maxAge, "interval", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweepDataDir(dir, maxAge)
		}
	}
}

// sweepDataDir removes regular files directly under dir whose modification
// time is older than maxAge. Subdirectories, sockets and symlinks are ignored.
// Returns the number of files removed.
func sweepDataDir(dir string, maxAge time.Duration) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		slog.Warn("data directory sweep failed", "dir", dir, "error", err)
		return 0
	}
	cutoff := time.Now().Add(-maxAge)
	removed := 0
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if err := os.Remove(path); err != nil {
			if !os.IsNotExist(err) {
				slog.Warn("could not remove stale staged file", "path", path, "error", err)
			}
			continue
		}
		slog.Debug("removed stale staged file", "path", path, "age", time.Since(info.ModTime()))
		removed++
	}
	if removed > 0 {
		slog.Info("swept stale staged files", "dir", dir, "removed", removed, "older_than", maxAge)
	}
	return removed
}

// mimeTypeFromExt returns the MIME type for a file extension.
func mimeTypeFromExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	default:
		return "image/jpeg"
	}
}

const (
	// maxTextFileSize is the maximum size for text file attachments (1MB).
	maxTextFileSize = 1 << 20
	// maxBinaryFileSize is the maximum size for binary file attachments (20MB).
	// Base64 encoding expands data by ~33%, so a 20MB file becomes ~27MB in the request body.
	maxBinaryFileSize = 20 << 20
)

// fileTypeFromExt returns the MIME type and whether it's a text type for a file extension.
// Returns an error for unknown extensions (in the attachments path, unknown types should error).
func fileTypeFromExt(ext string) (mimeType string, isText bool, err error) {
	switch strings.ToLower(ext) {
	// Images
	case ".png":
		return "image/png", false, nil
	case ".gif":
		return "image/gif", false, nil
	case ".webp":
		return "image/webp", false, nil
	case ".jpg", ".jpeg":
		return "image/jpeg", false, nil
	// Documents
	case ".pdf":
		return "application/pdf", false, nil
	// Text
	case ".txt":
		return "text/plain", true, nil
	case ".md":
		return "text/markdown", true, nil
	case ".csv":
		return "text/csv", true, nil
	case ".json":
		return "application/json", true, nil
	case ".xml":
		return "text/xml", true, nil
	case ".html":
		return "text/html", true, nil
	case ".yaml", ".yml":
		return "text/yaml", true, nil
	case ".log":
		return "text/plain", true, nil
	default:
		return "", false, fmt.Errorf("unknown file extension %q", ext)
	}
}

// mimeIsText returns true if the MIME type represents text content that should be
// inlined rather than base64-encoded. Used when the MIME type is provided explicitly
// via attachment_types (bypassing extension-based detection).
func mimeIsText(mimeType string) bool {
	// Strip parameters (e.g. "; charset=utf-8") and normalize case
	mediaType := strings.ToLower(strings.TrimSpace(mimeType))
	if i := strings.IndexByte(mediaType, ';'); i >= 0 {
		mediaType = strings.TrimSpace(mediaType[:i])
	}
	if strings.HasPrefix(mediaType, "text/") {
		return true
	}
	switch mediaType {
	case "application/json", "application/xml", "application/yaml":
		return true
	}
	return false
}

// resolveAttachments populates handoff.ResolvedAttachments from the Attachments map.
// Paths are relative to allowedDir. Files are read, typed, and deleted (best-effort).
// Returns error for path traversal violations, unknown file types, or oversized files.
// File read errors are logged and skipped (warning, not error).
func resolveAttachments(handoff *backends.HandoffData, allowedDir string) error {
	if len(handoff.Attachments) == 0 {
		return nil
	}

	// Fail fast if data_dir is not configured
	if !filepath.IsAbs(allowedDir) {
		return fmt.Errorf("data_dir is not configured (required for attachments)")
	}

	handoff.ResolvedAttachments = make(map[string]backends.ResolvedAttachment, len(handoff.Attachments))

	for refName, relPath := range handoff.Attachments {
		resolved, outside, err := containedPath(allowedDir, filepath.Join(allowedDir, relPath))
		if outside {
			return fmt.Errorf("attachment path (ref %q) %w", refName, err)
		}
		if err != nil {
			slog.Warn("attachment file resolve failed", "ref", refName, "path", relPath, "error", err)
			continue
		}

		// Determine MIME type: explicit override from attachment_types, or detect from extension
		var mimeType string
		var isText bool
		if override, ok := handoff.AttachmentTypes[refName]; ok {
			// Normalize: strip parameters (e.g. "; charset=utf-8") for clean data URLs
			mimeType = strings.TrimSpace(override)
			if i := strings.IndexByte(mimeType, ';'); i >= 0 {
				mimeType = strings.TrimSpace(mimeType[:i])
			}
			if mimeType == "" {
				return fmt.Errorf("attachment_types[%q] is empty or whitespace-only", refName)
			}
			isText = mimeIsText(override)
		} else {
			var err error
			mimeType, isText, err = fileTypeFromExt(filepath.Ext(resolved))
			if err != nil {
				os.Remove(resolved) // best-effort cleanup of staged file
				return fmt.Errorf("attachment %q: %w", refName, err)
			}
		}

		maxSize, kind := int64(maxBinaryFileSize), "binary file"
		if isText {
			maxSize, kind = maxTextFileSize, "text file"
		}
		data, skip, err := readStagedFile(resolved, maxSize, kind, handoff)
		if err != nil {
			return fmt.Errorf("attachment %q: %w", refName, err)
		}
		if skip != nil {
			slog.Warn("attachment file skipped", "ref", refName, "path", resolved, "reason", skip)
			continue
		}

		att := backends.ResolvedAttachment{MimeType: mimeType, IsText: isText}
		if isText {
			if !utf8.Valid(data) {
				return fmt.Errorf("attachment %q: text file contains invalid UTF-8", refName)
			}
			att.Text = string(data)
		} else {
			att.Base64 = base64.StdEncoding.EncodeToString(data)
		}
		handoff.ResolvedAttachments[refName] = att
	}

	return nil
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
	// Go 1.26: errors.AsType is type-safe, avoids declaring a target variable, no reflect
	if errno, ok := errors.AsType[syscall.Errno](err); ok {
		switch errno {
		case syscall.EPIPE, syscall.ECONNRESET:
			return "client_disconnected"
		case syscall.ETIMEDOUT:
			return "timeout"
		}
	}
	// Check for net.Error interface (includes timeouts)
	if netErr, ok := errors.AsType[net.Error](err); ok {
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
	if opErr, ok := errors.AsType[*net.OpError](err); ok {
		if syscallErr, ok := errors.AsType[*os.SyscallError](opErr.Err); ok {
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
