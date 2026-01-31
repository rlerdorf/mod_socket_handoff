// Load generator for benchmarking streaming daemons.
//
// Simulates Apache's SCM_RIGHTS handoff behavior to test daemon concurrency
// and memory usage. Creates socket pairs, sends fds to daemon, and consumes
// the SSE response.
//
// Usage:
//
//	./load-generator -socket /var/run/streaming-daemon.sock -connections 1000
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Config holds command-line configuration.
type Config struct {
	SocketPath    string
	Connections   int
	RampUpTime    time.Duration
	HoldTime      time.Duration
	OutputFile    string
	HandoffData   []byte // Pre-converted to bytes
	Verbose       bool
	StreamTimeout time.Duration
}

// Stats tracks benchmark statistics using lock-free operations where possible.
type Stats struct {
	ConnectionsStarted   int64
	ConnectionsCompleted int64
	ConnectionsFailed    int64
	BytesReceived        int64

	// Error categorization (atomic)
	connectErrors  int64
	sendErrors     int64
	receiveErrors  int64
	timeoutErrors  int64
	daemonRejected int64

	// Latency tracking with sharded collection to reduce contention
	latencyShards []*latencyShard
	numShards     int
}

type latencyShard struct {
	mu               sync.Mutex
	handoffLatencies []time.Duration
	ttfbLatencies    []time.Duration
	streamLatencies  []time.Duration
}

func newStats(numShards int) *Stats {
	shards := make([]*latencyShard, numShards)
	for i := range shards {
		shards[i] = &latencyShard{
			handoffLatencies: make([]time.Duration, 0, 1024),
			ttfbLatencies:    make([]time.Duration, 0, 1024),
			streamLatencies:  make([]time.Duration, 0, 1024),
		}
	}
	return &Stats{
		latencyShards: shards,
		numShards:     numShards,
	}
}

func (s *Stats) getShard(id int) *latencyShard {
	return s.latencyShards[id%s.numShards]
}

func (s *Stats) recordLatencies(id int, handoff, ttfb, stream time.Duration) {
	shard := s.getShard(id)
	shard.mu.Lock()
	shard.handoffLatencies = append(shard.handoffLatencies, handoff)
	if ttfb > 0 {
		shard.ttfbLatencies = append(shard.ttfbLatencies, ttfb)
	}
	shard.streamLatencies = append(shard.streamLatencies, stream)
	shard.mu.Unlock()
}

func (s *Stats) collectLatencies() (handoff, ttfb, stream []time.Duration) {
	for _, shard := range s.latencyShards {
		shard.mu.Lock()
		handoff = append(handoff, shard.handoffLatencies...)
		ttfb = append(ttfb, shard.ttfbLatencies...)
		stream = append(stream, shard.streamLatencies...)
		shard.mu.Unlock()
	}
	return
}

func percentile(latencies []time.Duration, p float64) time.Duration {
	if len(latencies) == 0 {
		return 0
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	idx := int(float64(len(latencies)-1) * p)
	return latencies[idx]
}

// createSocketPair creates a connected pair of Unix sockets using net.UnixConn.
// This avoids raw syscalls while still being efficient.
func createSocketPair() (*net.UnixConn, *net.UnixConn, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}

	// Wrap in os.File (takes ownership of fds)
	file1 := os.NewFile(uintptr(fds[0]), "socketpair0")
	file2 := os.NewFile(uintptr(fds[1]), "socketpair1")

	// Convert to net.Conn (FileConn dups the fd, so we must close the files)
	conn1, err := net.FileConn(file1)
	file1.Close()
	if err != nil {
		file2.Close()
		return nil, nil, err
	}

	conn2, err := net.FileConn(file2)
	file2.Close()
	if err != nil {
		conn1.Close()
		return nil, nil, err
	}

	return conn1.(*net.UnixConn), conn2.(*net.UnixConn), nil
}

// sendFd sends a file descriptor over a Unix socket using WriteMsgUnix.
func sendFd(conn *net.UnixConn, fd int, data []byte) error {
	rights := syscall.UnixRights(fd)
	_, _, err := conn.WriteMsgUnix(data, rights, nil)
	return err
}


// doConnection performs a single connection handoff and reads the response
func doConnection(cfg *Config, stats *Stats, connID int, readerPool *sync.Pool) {
	atomic.AddInt64(&stats.ConnectionsStarted, 1)

	// Create socket pair - clientConn will be sent to daemon, responseConn is for reading
	clientConn, responseConn, err := createSocketPair()
	if err != nil {
		atomic.AddInt64(&stats.ConnectionsFailed, 1)
		atomic.AddInt64(&stats.connectErrors, 1)
		if cfg.Verbose {
			log.Printf("[%d] createSocketPair failed: %v", connID, err)
		}
		return
	}
	defer responseConn.Close()

	// Connect to daemon Unix socket
	daemonConn, err := net.Dial("unix", cfg.SocketPath)
	if err != nil {
		clientConn.Close()
		atomic.AddInt64(&stats.ConnectionsFailed, 1)
		atomic.AddInt64(&stats.connectErrors, 1)
		if cfg.Verbose {
			log.Printf("[%d] dial daemon failed: %v", connID, err)
		}
		return
	}

	// Get the raw fd from clientConn to send via SCM_RIGHTS
	clientFile, err := clientConn.File()
	if err != nil {
		clientConn.Close()
		daemonConn.Close()
		atomic.AddInt64(&stats.ConnectionsFailed, 1)
		atomic.AddInt64(&stats.sendErrors, 1)
		if cfg.Verbose {
			log.Printf("[%d] File() failed: %v", connID, err)
		}
		return
	}

	handoffStart := time.Now()

	// Send fd to daemon via SCM_RIGHTS using WriteMsgUnix
	unixDaemon := daemonConn.(*net.UnixConn)
	if err := sendFd(unixDaemon, int(clientFile.Fd()), cfg.HandoffData); err != nil {
		clientFile.Close()
		clientConn.Close()
		daemonConn.Close()
		atomic.AddInt64(&stats.ConnectionsFailed, 1)
		atomic.AddInt64(&stats.sendErrors, 1)
		if cfg.Verbose {
			log.Printf("[%d] sendFd failed: %v", connID, err)
		}
		return
	}

	// Close our copies - daemon now owns the fd
	clientFile.Close()
	clientConn.Close()
	daemonConn.Close()
	handoffDuration := time.Since(handoffStart)

	// Set read timeout on response connection
	responseConn.SetDeadline(time.Now().Add(cfg.StreamTimeout))

	// Get or create a bufio.Reader from pool
	readerIface := readerPool.Get()
	var reader *bufio.Reader
	if readerIface != nil {
		reader = readerIface.(*bufio.Reader)
		reader.Reset(responseConn)
	} else {
		reader = bufio.NewReaderSize(responseConn, 4096)
	}

	// Read response
	streamStart := time.Now()
	var ttfb time.Duration
	firstByte := false
	bytesRead := int64(0)

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			bytesRead += int64(len(line))
			if !firstByte {
				ttfb = time.Since(streamStart)
				firstByte = true
			}
		}
		if err != nil {
			if err != io.EOF {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					atomic.AddInt64(&stats.timeoutErrors, 1)
				} else {
					atomic.AddInt64(&stats.receiveErrors, 1)
				}
				if cfg.Verbose {
					log.Printf("[%d] read error: %v", connID, err)
				}
			}
			break
		}
	}

	streamDuration := time.Since(streamStart)

	// Return reader to pool
	reader.Reset(nil)
	readerPool.Put(reader)

	// Record stats
	stats.recordLatencies(connID, handoffDuration, ttfb, streamDuration)
	atomic.AddInt64(&stats.BytesReceived, bytesRead)
	atomic.AddInt64(&stats.ConnectionsCompleted, 1)

	if cfg.Verbose {
		log.Printf("[%d] handoff=%v ttfb=%v stream=%v bytes=%d",
			connID, handoffDuration, ttfb, streamDuration, bytesRead)
	}
}

// runBenchmark executes the benchmark with ramp-up and hold phases.
func runBenchmark(cfg *Config, stats *Stats, stopCh <-chan struct{}) {
	var wg sync.WaitGroup
	readerPool := &sync.Pool{}

	// Calculate ramp-up delay between connections
	var rampDelay time.Duration
	if cfg.Connections > 0 && cfg.RampUpTime > 0 {
		rampDelay = cfg.RampUpTime / time.Duration(cfg.Connections)
	}

	log.Printf("Starting benchmark: %d connections, ramp-up %v (delay %v), hold %v",
		cfg.Connections, cfg.RampUpTime, rampDelay, cfg.HoldTime)

	startTime := time.Now()

	// Ramp-up phase: spawn goroutines gradually
	for i := 0; i < cfg.Connections; i++ {
		select {
		case <-stopCh:
			log.Println("Interrupted during ramp-up")
			goto wait
		default:
		}

		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			doConnection(cfg, stats, id, readerPool)
		}(i)

		if rampDelay > 0 {
			time.Sleep(rampDelay)
		}
	}

	// Hold phase: wait for hold duration
	if cfg.HoldTime > 0 {
		log.Printf("Ramp-up complete, holding for %v", cfg.HoldTime)
		select {
		case <-time.After(cfg.HoldTime):
		case <-stopCh:
			log.Println("Interrupted during hold")
		}
	}

wait:
	log.Println("Waiting for connections to complete...")
	wg.Wait()

	elapsed := time.Since(startTime)
	log.Printf("Benchmark complete in %v", elapsed)
}

// printResults outputs benchmark statistics.
func printResults(stats *Stats, outputFile string) {
	handoff, ttfb, stream := stats.collectLatencies()

	results := map[string]interface{}{
		"connections_started":   stats.ConnectionsStarted,
		"connections_completed": stats.ConnectionsCompleted,
		"connections_failed":    stats.ConnectionsFailed,
		"bytes_received":        stats.BytesReceived,
		"errors": map[string]int64{
			"connect":  stats.connectErrors,
			"send":     stats.sendErrors,
			"receive":  stats.receiveErrors,
			"timeout":  stats.timeoutErrors,
			"rejected": stats.daemonRejected,
		},
		"handoff_latency_ms": map[string]float64{
			"p50": float64(percentile(handoff, 0.50)) / float64(time.Millisecond),
			"p95": float64(percentile(handoff, 0.95)) / float64(time.Millisecond),
			"p99": float64(percentile(handoff, 0.99)) / float64(time.Millisecond),
		},
		"ttfb_latency_ms": map[string]float64{
			"p50": float64(percentile(ttfb, 0.50)) / float64(time.Millisecond),
			"p95": float64(percentile(ttfb, 0.95)) / float64(time.Millisecond),
			"p99": float64(percentile(ttfb, 0.99)) / float64(time.Millisecond),
		},
		"stream_duration_ms": map[string]float64{
			"p50": float64(percentile(stream, 0.50)) / float64(time.Millisecond),
			"p95": float64(percentile(stream, 0.95)) / float64(time.Millisecond),
			"p99": float64(percentile(stream, 0.99)) / float64(time.Millisecond),
		},
	}

	output, _ := json.MarshalIndent(results, "", "  ")
	fmt.Println(string(output))

	if outputFile != "" {
		if err := os.WriteFile(outputFile, output, 0644); err != nil {
			log.Printf("Failed to write output file: %v", err)
		} else {
			log.Printf("Results written to %s", outputFile)
		}
	}
}

func main() {
	var handoffData string
	cfg := Config{}

	flag.StringVar(&cfg.SocketPath, "socket", "/var/run/streaming-daemon.sock",
		"Path to daemon Unix socket")
	flag.IntVar(&cfg.Connections, "connections", 100,
		"Number of connections to establish")
	flag.DurationVar(&cfg.RampUpTime, "ramp-up", 10*time.Second,
		"Time to gradually ramp up to target connections")
	flag.DurationVar(&cfg.HoldTime, "hold", 60*time.Second,
		"Time to hold after ramp-up completes")
	flag.StringVar(&cfg.OutputFile, "output", "",
		"Output file for results (JSON)")
	flag.StringVar(&handoffData, "data", `{"prompt":"benchmark test","user_id":1}`,
		"JSON handoff data to send")
	flag.BoolVar(&cfg.Verbose, "verbose", false,
		"Enable verbose logging")
	flag.DurationVar(&cfg.StreamTimeout, "stream-timeout", 120*time.Second,
		"Timeout for reading stream response")

	flag.Parse()

	cfg.HandoffData = []byte(handoffData)

	// Increase fd limit
	var rLimit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rLimit); err == nil {
		rLimit.Cur = rLimit.Max
		if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rLimit); err == nil {
			log.Printf("Set fd limit to %d", rLimit.Cur)
		}
	}

	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.Printf("Load generator starting")
	log.Printf("Target: %s", cfg.SocketPath)

	// Verify socket exists
	if _, err := os.Stat(cfg.SocketPath); os.IsNotExist(err) {
		log.Fatalf("Socket does not exist: %s", cfg.SocketPath)
	}

	// Use sharded stats to reduce contention
	stats := newStats(runtime.NumCPU() * 2)

	// Handle Ctrl+C
	stopCh := make(chan struct{})
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("Received interrupt signal")
		close(stopCh)
	}()

	runBenchmark(&cfg, stats, stopCh)
	printResults(stats, cfg.OutputFile)
}
