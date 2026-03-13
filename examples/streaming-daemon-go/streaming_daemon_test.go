// Tests for the streaming daemon's SCM_RIGHTS handling and per-request routing.
//
// These tests verify the core socket handoff mechanism without starting
// the full daemon. They test:
// - SCM_RIGHTS fd passing roundtrip
// - Concurrent fd sends
// - Large data handling
// - HTTP response format
// - Per-request backend routing

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"examples/backends"
	"examples/config"
)

// sendFd sends a file descriptor over a Unix socket along with data.
func sendFd(conn *net.UnixConn, fd int, data []byte) error {
	file, err := conn.File()
	if err != nil {
		return err
	}
	defer file.Close()

	rights := syscall.UnixRights(fd)
	return syscall.Sendmsg(int(file.Fd()), data, rights, nil, 0)
}

// recvFd receives a file descriptor from a Unix socket along with data.
func recvFd(conn *net.UnixConn) (int, []byte, error) {
	file, err := conn.File()
	if err != nil {
		return -1, nil, err
	}
	defer file.Close()

	buf := make([]byte, 65536)
	oob := make([]byte, syscall.CmsgSpace(4)) // Portable size for 1 fd

	n, oobn, _, _, err := syscall.Recvmsg(int(file.Fd()), buf, oob, 0)
	if err != nil {
		return -1, nil, err
	}

	msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return -1, nil, err
	}

	if len(msgs) == 0 {
		return -1, nil, errors.New("no control messages received")
	}

	fds, err := syscall.ParseUnixRights(&msgs[0])
	if err != nil {
		return -1, nil, err
	}

	if len(fds) == 0 {
		return -1, nil, errors.New("no file descriptors received")
	}

	return fds[0], buf[:n], nil
}

// createSocketPair creates a connected pair of Unix sockets.
func createSocketPair() (*net.UnixConn, *net.UnixConn, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET, 0)
	if err != nil {
		return nil, nil, err
	}

	file1 := os.NewFile(uintptr(fds[0]), "socket1")
	file2 := os.NewFile(uintptr(fds[1]), "socket2")
	defer file1.Close()
	defer file2.Close()

	conn1, err := net.FileConn(file1)
	if err != nil {
		return nil, nil, err
	}

	conn2, err := net.FileConn(file2)
	if err != nil {
		conn1.Close()
		return nil, nil, err
	}

	return conn1.(*net.UnixConn), conn2.(*net.UnixConn), nil
}

func TestScmRightsRoundtrip(t *testing.T) {
	// Create socket pair for "Apache" <-> "Daemon" communication
	sender, receiver, err := createSocketPair()
	if err != nil {
		t.Fatalf("createSocketPair failed: %v", err)
	}
	defer sender.Close()
	defer receiver.Close()

	// Create a TCP connection to send
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}
	defer listener.Close()

	// Connect to it
	clientConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	defer clientConn.Close()

	// Accept the connection
	serverConn, err := listener.Accept()
	if err != nil {
		t.Fatalf("Accept failed: %v", err)
	}
	defer serverConn.Close()

	// Get the fd from server side
	tcpConn := serverConn.(*net.TCPConn)
	file, err := tcpConn.File()
	if err != nil {
		t.Fatalf("File() failed: %v", err)
	}
	fd := int(file.Fd())

	// Send the fd with data
	data := []byte(`{"user_id": 123, "prompt": "test"}`)
	if err := sendFd(sender, fd, data); err != nil {
		t.Fatalf("sendFd failed: %v", err)
	}
	file.Close()

	// Receive the fd
	receivedFd, receivedData, err := recvFd(receiver)
	if err != nil {
		t.Fatalf("recvFd failed: %v", err)
	}
	defer syscall.Close(receivedFd)

	// Verify data
	if !bytes.Equal(receivedData, data) {
		t.Errorf("data mismatch: got %q, want %q", receivedData, data)
	}

	// Verify fd is valid by checking it's a socket
	sockType, err := syscall.GetsockoptInt(receivedFd, syscall.SOL_SOCKET, syscall.SO_TYPE)
	if err != nil {
		t.Fatalf("GetsockoptInt failed: %v", err)
	}
	if sockType != syscall.SOCK_STREAM {
		t.Errorf("unexpected socket type: %d", sockType)
	}
}

func TestConcurrentFdSends(t *testing.T) {
	const numConnections = 5
	var wg sync.WaitGroup
	errCh := make(chan error, numConnections)

	for i := range numConnections {
		wg.Add(1)
		go func(connID int) {
			defer wg.Done()

			sender, receiver, err := createSocketPair()
			if err != nil {
				errCh <- err
				return
			}
			defer sender.Close()
			defer receiver.Close()

			// Create TCP connection
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				errCh <- err
				return
			}
			defer listener.Close()

			clientConn, err := net.Dial("tcp", listener.Addr().String())
			if err != nil {
				errCh <- err
				return
			}
			defer clientConn.Close()

			serverConn, err := listener.Accept()
			if err != nil {
				errCh <- err
				return
			}
			defer serverConn.Close()

			tcpConn := serverConn.(*net.TCPConn)
			file, err := tcpConn.File()
			if err != nil {
				errCh <- err
				return
			}

			payload := struct {
				ConnectionID int `json:"connection_id"`
			}{ConnectionID: connID}
			data, err := json.Marshal(payload)
			if err != nil {
				errCh <- err
				file.Close()
				return
			}
			if err := sendFd(sender, int(file.Fd()), data); err != nil {
				errCh <- err
				file.Close()
				return
			}
			file.Close()

			receivedFd, _, err := recvFd(receiver)
			if err != nil {
				errCh <- err
				return
			}
			syscall.Close(receivedFd)
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrent test error: %v", err)
	}
}

func TestHandoffDataParsing(t *testing.T) {
	testCases := []struct {
		name     string
		json     string
		wantUser int64
	}{
		{
			name:     "basic",
			json:     `{"user_id": 123, "prompt": "Hello world"}`,
			wantUser: 123,
		},
		{
			name:     "with all fields",
			json:     `{"user_id": 456, "prompt": "test", "model": "gpt-4", "max_tokens": 100}`,
			wantUser: 456,
		},
		{
			name:     "empty prompt",
			json:     `{"user_id": 789}`,
			wantUser: 789,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var data backends.HandoffData
			if err := json.Unmarshal([]byte(tc.json), &data); err != nil {
				t.Fatalf("Unmarshal failed: %v", err)
			}
			if data.UserID != tc.wantUser {
				t.Errorf("UserID = %d, want %d", data.UserID, tc.wantUser)
			}
		})
	}
}

func TestSocketPairCommunication(t *testing.T) {
	// Create "Apache" <-> "Daemon" channel
	apacheSide, daemonSide, err := createSocketPair()
	if err != nil {
		t.Fatalf("createSocketPair failed: %v", err)
	}
	defer apacheSide.Close()
	defer daemonSide.Close()

	// Create "client" connection
	clientA, clientB, err := createSocketPair()
	if err != nil {
		t.Fatalf("createSocketPair for client failed: %v", err)
	}

	// Get fd for clientA
	fileA, err := clientA.File()
	if err != nil {
		t.Fatalf("File() failed: %v", err)
	}

	// Apache sends clientA to daemon
	data := []byte(`{"prompt": "hello"}`)
	if err := sendFd(apacheSide, int(fileA.Fd()), data); err != nil {
		t.Fatalf("sendFd failed: %v", err)
	}
	fileA.Close()
	clientA.Close() // Close our copy - daemon now owns it

	// Daemon receives clientA
	daemonClientFd, _, err := recvFd(daemonSide)
	if err != nil {
		t.Fatalf("recvFd failed: %v", err)
	}

	// Convert fd to file for daemon to write
	daemonClient := os.NewFile(uintptr(daemonClientFd), "client")
	defer daemonClient.Close()

	// Daemon writes HTTP response
	response := "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nHello from daemon!"
	if _, err := daemonClient.WriteString(response); err != nil {
		t.Fatalf("WriteString failed: %v", err)
	}
	daemonClient.Close() // Signal EOF

	// Client (holding clientB) reads the response
	reader := bufio.NewReader(clientB)
	result, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}

	if !strings.Contains(string(result), "HTTP/1.1 200 OK") {
		t.Errorf("response missing status line: %s", result)
	}
	if !strings.Contains(string(result), "Hello from daemon!") {
		t.Errorf("response missing body: %s", result)
	}
}

func TestSSEFormatVerification(t *testing.T) {
	// Verify SSE event format matches what daemon should send
	content := "test content"
	data, err := json.Marshal(map[string]string{"content": content})
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	sseChunk := "data: " + string(data) + "\n\n"
	if !strings.HasPrefix(sseChunk, "data:") {
		t.Errorf("SSE chunk should start with 'data:': %s", sseChunk)
	}
	if !strings.Contains(sseChunk, content) {
		t.Errorf("SSE chunk should contain content: %s", sseChunk)
	}

	doneMarker := "data: [DONE]\n\n"
	if !strings.HasPrefix(doneMarker, "data:") {
		t.Error("Done marker should start with 'data:'")
	}
}

func TestLargeHandoffData(t *testing.T) {
	sender, receiver, err := createSocketPair()
	if err != nil {
		t.Fatalf("createSocketPair failed: %v", err)
	}
	defer sender.Close()
	defer receiver.Close()

	// Create TCP connection
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}
	defer listener.Close()

	clientConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	defer clientConn.Close()

	serverConn, err := listener.Accept()
	if err != nil {
		t.Fatalf("Accept failed: %v", err)
	}
	defer serverConn.Close()

	tcpConn := serverConn.(*net.TCPConn)
	file, err := tcpConn.File()
	if err != nil {
		t.Fatalf("File() failed: %v", err)
	}

	// Create large prompt (~10KB)
	longPrompt := strings.Repeat("test ", 2000)
	data := []byte(`{"user_id": 789, "prompt": "` + longPrompt + `"}`)

	if err := sendFd(sender, int(file.Fd()), data); err != nil {
		t.Fatalf("sendFd failed: %v", err)
	}
	file.Close()

	receivedFd, receivedData, err := recvFd(receiver)
	if err != nil {
		t.Fatalf("recvFd failed: %v", err)
	}
	defer syscall.Close(receivedFd)

	if len(receivedData) != len(data) {
		t.Errorf("data length mismatch: got %d, want %d", len(receivedData), len(data))
	}

	// Verify JSON can be parsed
	var parsed map[string]any
	if err := json.Unmarshal(receivedData, &parsed); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if parsed["user_id"].(float64) != 789 {
		t.Errorf("user_id mismatch: got %v, want 789", parsed["user_id"])
	}
}

func TestHealthHandler(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()

	healthHandler(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	ct := resp.Header.Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode JSON body: %v", err)
	}

	if body["status"] != "ok" {
		t.Errorf("status = %v, want %q", body["status"], "ok")
	}
	if _, ok := body["active_streams"]; !ok {
		t.Error("missing active_streams field")
	}
	if _, ok := body["active_connections"]; !ok {
		t.Error("missing active_connections field")
	}
}

func TestReloadConfig(t *testing.T) {
	// This test mutates global state (flags, logger, currentLogging,
	// debug.SetMemoryLimit, debug.SetGCPercent). Subtests run serially
	// (no t.Parallel) and the parent's t.Cleanup restores everything
	// after all subtests complete.
	origConfigFile := *configFile
	origMemLimitFlag := *memLimitFlag
	origGcPercentFlag := *gcPercentFlag
	origLogger := slog.Default()
	currentLoggingMu.Lock()
	origLogging := currentLogging
	currentLoggingMu.Unlock()
	origMemLimit := debug.SetMemoryLimit(-1)
	origGCPercent := debug.SetGCPercent(100)
	debug.SetGCPercent(origGCPercent)
	t.Cleanup(func() {
		*configFile = origConfigFile
		*memLimitFlag = origMemLimitFlag
		*gcPercentFlag = origGcPercentFlag
		slog.SetDefault(origLogger)
		currentLoggingMu.Lock()
		currentLogging = origLogging
		currentLoggingMu.Unlock()
		debug.SetMemoryLimit(origMemLimit)
		debug.SetGCPercent(origGCPercent)
	})

	t.Run("no config file warns", func(t *testing.T) {
		*configFile = ""
		// Should not panic — just logs a warning
		reloadConfig()
	})

	t.Run("missing config file logs error", func(t *testing.T) {
		*configFile = "/nonexistent/path/config.yaml"
		// Should not panic — logs error and returns
		reloadConfig()
	})

	t.Run("valid config reloads logging", func(t *testing.T) {
		// Create a temp config file with debug logging
		tmpFile := filepath.Join(t.TempDir(), "config.yaml")
		content := []byte("logging:\n  level: debug\n  format: json\n")
		if err := os.WriteFile(tmpFile, content, 0644); err != nil {
			t.Fatal(err)
		}

		*configFile = tmpFile
		*memLimitFlag = ""
		*gcPercentFlag = -1

		reloadConfig()

		// Verify logging was changed to debug/json
		handler := slog.Default().Handler()
		if !handler.Enabled(context.Background(), slog.LevelDebug) {
			t.Error("expected debug level to be enabled after reload")
		}
		if _, ok := handler.(*slog.JSONHandler); !ok {
			t.Errorf("expected JSONHandler after reload, got %T", handler)
		}
	})

	t.Run("omitted logging section preserves current logger", func(t *testing.T) {
		// Set up a known logger state first
		knownCfg := config.LoggingConfig{Level: "error", Format: "json"}
		initLogging(knownCfg)
		currentLoggingMu.Lock()
		currentLogging = knownCfg
		currentLoggingMu.Unlock()
		handlerBefore := slog.Default().Handler()

		// Config with no logging section
		tmpFile := filepath.Join(t.TempDir(), "config.yaml")
		content := []byte("server:\n  gc_percent: 100\n")
		if err := os.WriteFile(tmpFile, content, 0644); err != nil {
			t.Fatal(err)
		}

		*configFile = tmpFile
		*memLimitFlag = ""
		*gcPercentFlag = -1

		reloadConfig()

		// Logger should not have changed
		handlerAfter := slog.Default().Handler()
		if handlerBefore != handlerAfter {
			t.Error("expected logger to be preserved when logging section is omitted")
		}
	})

	t.Run("partial logging config preserves unspecified fields", func(t *testing.T) {
		// Start with json format at error level
		knownCfg := config.LoggingConfig{Level: "error", Format: "json"}
		initLogging(knownCfg)
		currentLoggingMu.Lock()
		currentLogging = knownCfg
		currentLoggingMu.Unlock()

		// Reload with only level changed — format should stay json
		tmpFile := filepath.Join(t.TempDir(), "config.yaml")
		content := []byte("logging:\n  level: debug\n")
		if err := os.WriteFile(tmpFile, content, 0644); err != nil {
			t.Fatal(err)
		}

		*configFile = tmpFile
		*memLimitFlag = ""
		*gcPercentFlag = -1

		reloadConfig()

		handler := slog.Default().Handler()
		if !handler.Enabled(context.Background(), slog.LevelDebug) {
			t.Error("expected debug level after partial reload")
		}
		if _, ok := handler.(*slog.JSONHandler); !ok {
			t.Errorf("expected format to stay JSON after partial reload, got %T", handler)
		}
	})

	t.Run("valid config reloads memlimit", func(t *testing.T) {
		tmpFile := filepath.Join(t.TempDir(), "config.yaml")
		content := []byte("server:\n  mem_limit: 512MiB\n")
		if err := os.WriteFile(tmpFile, content, 0644); err != nil {
			t.Fatal(err)
		}

		*configFile = tmpFile
		*memLimitFlag = ""
		*gcPercentFlag = -1

		reloadConfig()

		// Verify the memory limit was actually applied.
		// Per Go docs: "If a negative limit is provided, SetMemoryLimit does
		// not change the limit, and returns the previously set value."
		got := debug.SetMemoryLimit(-1)
		want := config.ParseMemLimit("512MiB")
		if got != want {
			t.Errorf("memory limit = %d, want %d", got, want)
		}
	})

	t.Run("invalid memlimit flag warns during reload", func(t *testing.T) {
		// The -memlimit flag overrides config; test that an invalid flag
		// value logs a warning instead of crashing.
		tmpFile := filepath.Join(t.TempDir(), "config.yaml")
		content := []byte("logging:\n  level: info\n")
		if err := os.WriteFile(tmpFile, content, 0644); err != nil {
			t.Fatal(err)
		}

		*configFile = tmpFile
		*memLimitFlag = "not-a-number"
		*gcPercentFlag = -1

		// Should not panic — logs warning about invalid memlimit from flag
		reloadConfig()
	})

	t.Run("gc percent reloads", func(t *testing.T) {
		tmpFile := filepath.Join(t.TempDir(), "config.yaml")
		content := []byte("server:\n  gc_percent: 200\n")
		if err := os.WriteFile(tmpFile, content, 0644); err != nil {
			t.Fatal(err)
		}

		*configFile = tmpFile
		*memLimitFlag = ""
		*gcPercentFlag = -1

		reloadConfig()

		// Verify the GC percent was actually applied.
		// SetGCPercent returns the previous value, so we set a temp and check.
		prev := debug.SetGCPercent(100)
		debug.SetGCPercent(prev) // restore
		if prev != 200 {
			t.Errorf("GC percent = %d, want 200", prev)
		}
	})
}

func TestInitLogging(t *testing.T) {
	tests := []struct {
		name      string
		cfg       config.LoggingConfig
		wantLevel slog.Level
		wantJSON  bool
	}{
		{
			name:      "defaults to info text",
			cfg:       config.LoggingConfig{},
			wantLevel: slog.LevelInfo,
			wantJSON:  false,
		},
		{
			name:      "debug level",
			cfg:       config.LoggingConfig{Level: "debug", Format: "text"},
			wantLevel: slog.LevelDebug,
			wantJSON:  false,
		},
		{
			name:      "warn level",
			cfg:       config.LoggingConfig{Level: "warn", Format: "text"},
			wantLevel: slog.LevelWarn,
			wantJSON:  false,
		},
		{
			name:      "warning alias",
			cfg:       config.LoggingConfig{Level: "warning", Format: "text"},
			wantLevel: slog.LevelWarn,
			wantJSON:  false,
		},
		{
			name:      "error level",
			cfg:       config.LoggingConfig{Level: "error", Format: "text"},
			wantLevel: slog.LevelError,
			wantJSON:  false,
		},
		{
			name:      "unknown level defaults to info",
			cfg:       config.LoggingConfig{Level: "bogus", Format: "text"},
			wantLevel: slog.LevelInfo,
			wantJSON:  false,
		},
		{
			name:      "case insensitive level",
			cfg:       config.LoggingConfig{Level: "DEBUG", Format: "text"},
			wantLevel: slog.LevelDebug,
			wantJSON:  false,
		},
		{
			name:      "json format",
			cfg:       config.LoggingConfig{Level: "info", Format: "json"},
			wantLevel: slog.LevelInfo,
			wantJSON:  true,
		},
		{
			name:      "json format case insensitive",
			cfg:       config.LoggingConfig{Level: "info", Format: "JSON"},
			wantLevel: slog.LevelInfo,
			wantJSON:  true,
		},
		{
			name:      "unknown format defaults to text",
			cfg:       config.LoggingConfig{Level: "info", Format: "xml"},
			wantLevel: slog.LevelInfo,
			wantJSON:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Save and restore the default logger
			original := slog.Default()
			defer slog.SetDefault(original)

			initLogging(tc.cfg)

			logger := slog.Default()
			handler := logger.Handler()

			// Verify handler type
			switch handler.(type) {
			case *slog.JSONHandler:
				if !tc.wantJSON {
					t.Errorf("got JSONHandler, want TextHandler")
				}
			case *slog.TextHandler:
				if tc.wantJSON {
					t.Errorf("got TextHandler, want JSONHandler")
				}
			default:
				t.Errorf("unexpected handler type: %T", handler)
			}

			// Verify log level by checking if the handler is enabled for the expected level
			if !handler.Enabled(context.Background(), tc.wantLevel) {
				t.Errorf("handler not enabled for expected level %v", tc.wantLevel)
			}
			// Verify a level just below is disabled (except for debug which is the lowest)
			if tc.wantLevel > slog.LevelDebug {
				belowLevel := tc.wantLevel - 1
				if handler.Enabled(context.Background(), belowLevel) {
					t.Errorf("handler should not be enabled for level %v (below %v)", belowLevel, tc.wantLevel)
				}
			}
		})
	}
}

func TestPerRequestBackendRouting(t *testing.T) {
	// Initialize all backends so they're available via GetInitialized.
	// Save and restore activeBackend since tests mutate it.
	origActive := activeBackend
	t.Cleanup(func() { activeBackend = origActive })

	cfg := config.Default()
	cfg.Backend.Provider = "mock"
	cfg.Backend.Mock.MessageDelayMs = 1 // must be >0 or Mock.Init uses 50ms default
	backends.InitAll(&cfg.Backend)
	activeBackend = backends.GetInitialized("mock")
	if activeBackend == nil {
		t.Fatal("mock backend not initialized")
	}

	t.Run("empty backend field uses default", func(t *testing.T) {
		server, client := net.Pipe()
		defer client.Close()

		handoff := backends.HandoffData{Prompt: "hello"}
		go func() {
			defer server.Close()
			streamToClientWithBytes(context.Background(), server, handoff)
		}()

		resp, err := io.ReadAll(client)
		if err != nil {
			t.Fatal(err)
		}
		// Mock backend sends SSE data events
		if !strings.Contains(string(resp), "data:") {
			t.Errorf("expected SSE data from mock backend, got: %s", resp)
		}
	})

	t.Run("valid backend override routes correctly", func(t *testing.T) {
		server, client := net.Pipe()
		defer client.Close()

		// Use "typing" backend as the override with its known deterministic
		// prompt to avoid fortune/fallback. Read just the first SSE data line
		// and assert it contains typing-specific {char,index} JSON format to
		// confirm routing (mock backend uses a different format).
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		handoff := backends.HandoffData{Prompt: "Tell me about streaming", Backend: "typing"}
		go func() {
			defer server.Close()
			streamToClientWithBytes(ctx, server, handoff)
		}()

		reader := bufio.NewReader(client)
		var firstDataLine string
		for {
			line, err := reader.ReadString('\n')
			if strings.HasPrefix(line, "data:") {
				firstDataLine = line
				break
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				t.Fatal(err)
			}
		}

		// Typing backend sends {"char":"x","index":N} — mock backend doesn't
		if !strings.Contains(firstDataLine, `"char"`) || !strings.Contains(firstDataLine, `"index"`) {
			t.Errorf("expected typing backend {char,index} format, got: %s", firstDataLine)
		}
	})

	t.Run("unknown backend falls back to default", func(t *testing.T) {
		server, client := net.Pipe()
		defer client.Close()

		handoff := backends.HandoffData{Prompt: "hello", Backend: "nonexistent"}
		go func() {
			defer server.Close()
			streamToClientWithBytes(context.Background(), server, handoff)
		}()

		resp, err := io.ReadAll(client)
		if err != nil {
			t.Fatal(err)
		}
		// Should fall back to mock (default) and still produce output
		if !strings.Contains(string(resp), "data:") {
			t.Errorf("expected SSE data from default backend after fallback, got: %s", resp)
		}
	})
}

func TestGetInitializedVsGet(t *testing.T) {
	// All backends should be registered
	if b := backends.Get("mock"); b == nil {
		t.Fatal("mock should be in registry")
	}

	// After InitAll, initialized backends should be available
	cfg := config.Default()
	cfg.Backend.Provider = "mock"
	backends.InitAll(&cfg.Backend)

	if b := backends.GetInitialized("mock"); b == nil {
		t.Error("mock should be initialized")
	}

	// Nonexistent backend should return nil from both
	if b := backends.GetInitialized("nonexistent"); b != nil {
		t.Errorf("expected nil for nonexistent backend from GetInitialized, got %v", b)
	}
	if b := backends.Get("nonexistent"); b != nil {
		t.Errorf("expected nil for nonexistent backend from Get, got %v", b)
	}
}

func TestResolveImages(t *testing.T) {
	t.Run("legacy single image fields migrated", func(t *testing.T) {
		handoff := backends.HandoffData{
			ImageBase64:   "dGVzdA==",
			ImageMimeType: "image/png",
		}
		if err := resolveImages(&handoff, "/tmp"); err != nil {
			t.Fatal(err)
		}
		if len(handoff.ResolvedImages) != 1 {
			t.Fatalf("expected 1 resolved image, got %d", len(handoff.ResolvedImages))
		}
		if handoff.ResolvedImages[0].Base64 != "dGVzdA==" {
			t.Errorf("base64 = %q, want %q", handoff.ResolvedImages[0].Base64, "dGVzdA==")
		}
		if handoff.ResolvedImages[0].MimeType != "image/png" {
			t.Errorf("mime = %q, want %q", handoff.ResolvedImages[0].MimeType, "image/png")
		}
		// Legacy fields should be cleared
		if handoff.ImageBase64 != "" {
			t.Error("ImageBase64 should be cleared after migration")
		}
		if handoff.ImageMimeType != "" {
			t.Error("ImageMimeType should be cleared after migration")
		}
	})

	t.Run("legacy single image defaults to jpeg", func(t *testing.T) {
		handoff := backends.HandoffData{
			ImageBase64: "dGVzdA==",
		}
		if err := resolveImages(&handoff, "/tmp"); err != nil {
			t.Fatal(err)
		}
		if handoff.ResolvedImages[0].MimeType != "image/jpeg" {
			t.Errorf("mime = %q, want %q", handoff.ResolvedImages[0].MimeType, "image/jpeg")
		}
	})

	t.Run("inline images copied to resolved", func(t *testing.T) {
		handoff := backends.HandoffData{
			Images: []backends.ImageData{
				{Base64: "aW1n", MimeType: "image/png"},
				{Base64: "aW1nMg==", MimeType: "image/gif"},
			},
		}
		if err := resolveImages(&handoff, "/tmp"); err != nil {
			t.Fatal(err)
		}
		if len(handoff.ResolvedImages) != 2 {
			t.Fatalf("expected 2 resolved images, got %d", len(handoff.ResolvedImages))
		}
	})

	t.Run("image file read and deleted", func(t *testing.T) {
		dir := t.TempDir()
		imgPath := filepath.Join(dir, "test.png")
		if err := os.WriteFile(imgPath, []byte("fake png data"), 0644); err != nil {
			t.Fatal(err)
		}

		handoff := backends.HandoffData{
			ImagePaths: []string{imgPath},
		}
		if err := resolveImages(&handoff, dir); err != nil {
			t.Fatal(err)
		}
		if len(handoff.ResolvedImages) != 1 {
			t.Fatalf("expected 1 resolved image, got %d", len(handoff.ResolvedImages))
		}
		if handoff.ResolvedImages[0].MimeType != "image/png" {
			t.Errorf("mime = %q, want image/png", handoff.ResolvedImages[0].MimeType)
		}
		// File should be deleted
		if _, err := os.Stat(imgPath); !os.IsNotExist(err) {
			t.Error("image file should have been deleted")
		}
	})

	t.Run("legacy image_path migrated and read", func(t *testing.T) {
		dir := t.TempDir()
		imgPath := filepath.Join(dir, "legacy.jpg")
		if err := os.WriteFile(imgPath, []byte("jpeg data"), 0644); err != nil {
			t.Fatal(err)
		}

		handoff := backends.HandoffData{
			ImagePath: imgPath,
		}
		if err := resolveImages(&handoff, dir); err != nil {
			t.Fatal(err)
		}
		if len(handoff.ResolvedImages) != 1 {
			t.Fatalf("expected 1 resolved image, got %d", len(handoff.ResolvedImages))
		}
		if handoff.ImagePath != "" {
			t.Error("ImagePath should be cleared after migration")
		}
	})

	t.Run("path traversal blocked", func(t *testing.T) {
		handoff := backends.HandoffData{
			ImagePaths: []string{"/etc/passwd"},
		}
		err := resolveImages(&handoff, "/tmp")
		if err == nil {
			t.Fatal("expected error for path traversal")
		}
		if !strings.Contains(err.Error(), "outside allowed directory") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("missing file skipped with warning", func(t *testing.T) {
		dir := t.TempDir()
		handoff := backends.HandoffData{
			ImagePaths: []string{filepath.Join(dir, "nonexistent.png")},
		}
		if err := resolveImages(&handoff, dir); err != nil {
			t.Fatal(err)
		}
		if len(handoff.ResolvedImages) != 0 {
			t.Errorf("expected 0 resolved images for missing file, got %d", len(handoff.ResolvedImages))
		}
	})

	t.Run("mixed inline and file images", func(t *testing.T) {
		dir := t.TempDir()
		imgPath := filepath.Join(dir, "photo.webp")
		if err := os.WriteFile(imgPath, []byte("webp data"), 0644); err != nil {
			t.Fatal(err)
		}

		handoff := backends.HandoffData{
			Images:     []backends.ImageData{{Base64: "aW5saW5l", MimeType: "image/png"}},
			ImagePaths: []string{imgPath},
		}
		if err := resolveImages(&handoff, dir); err != nil {
			t.Fatal(err)
		}
		if len(handoff.ResolvedImages) != 2 {
			t.Fatalf("expected 2 resolved images, got %d", len(handoff.ResolvedImages))
		}
		// First should be the inline one
		if handoff.ResolvedImages[0].MimeType != "image/png" {
			t.Errorf("first image mime = %q, want image/png", handoff.ResolvedImages[0].MimeType)
		}
		// Second should be the file one
		if handoff.ResolvedImages[1].MimeType != "image/webp" {
			t.Errorf("second image mime = %q, want image/webp", handoff.ResolvedImages[1].MimeType)
		}
	})

	t.Run("no images is a no-op", func(t *testing.T) {
		handoff := backends.HandoffData{Prompt: "hello"}
		if err := resolveImages(&handoff, "/tmp"); err != nil {
			t.Fatal(err)
		}
		if len(handoff.ResolvedImages) != 0 {
			t.Errorf("expected 0 resolved images, got %d", len(handoff.ResolvedImages))
		}
	})

	t.Run("mime type from various extensions", func(t *testing.T) {
		dir := t.TempDir()
		exts := map[string]string{
			"test.jpg":  "image/jpeg",
			"test.jpeg": "image/jpeg",
			"test.png":  "image/png",
			"test.gif":  "image/gif",
			"test.webp": "image/webp",
			"test.bmp":  "image/jpeg", // unknown defaults to jpeg
		}
		for filename, wantMime := range exts {
			path := filepath.Join(dir, filename)
			if err := os.WriteFile(path, []byte("data"), 0644); err != nil {
				t.Fatal(err)
			}
			handoff := backends.HandoffData{
				ImagePaths: []string{path},
			}
			if err := resolveImages(&handoff, dir); err != nil {
				t.Fatalf("ext %s: %v", filename, err)
			}
			if len(handoff.ResolvedImages) != 1 {
				t.Fatalf("ext %s: expected 1 image, got %d", filename, len(handoff.ResolvedImages))
			}
			if handoff.ResolvedImages[0].MimeType != wantMime {
				t.Errorf("ext %s: mime = %q, want %q", filename, handoff.ResolvedImages[0].MimeType, wantMime)
			}
		}
	})
}

func TestMimeIsText(t *testing.T) {
	tests := []struct {
		mime string
		want bool
	}{
		{"text/plain", true},
		{"Text/Plain", true},
		{"text/html; charset=utf-8", true},
		{"application/json", true},
		{"APPLICATION/JSON", true},
		{"application/json; charset=utf-8", true},
		{"application/xml", true},
		{"application/yaml", true},
		{"image/png", false},
		{"application/octet-stream", false},
		{"  text/plain  ", true},
		{"", false},
	}
	for _, tt := range tests {
		got := mimeIsText(tt.mime)
		if got != tt.want {
			t.Errorf("mimeIsText(%q) = %v, want %v", tt.mime, got, tt.want)
		}
	}
}

func TestFileTypeFromExt(t *testing.T) {
	tests := []struct {
		ext      string
		wantMime string
		wantText bool
		wantErr  bool
	}{
		{".png", "image/png", false, false},
		{".jpg", "image/jpeg", false, false},
		{".jpeg", "image/jpeg", false, false},
		{".gif", "image/gif", false, false},
		{".webp", "image/webp", false, false},
		{".pdf", "application/pdf", false, false},
		{".txt", "text/plain", true, false},
		{".md", "text/markdown", true, false},
		{".csv", "text/csv", true, false},
		{".json", "application/json", true, false},
		{".xml", "text/xml", true, false},
		{".html", "text/html", true, false},
		{".yaml", "text/yaml", true, false},
		{".yml", "text/yaml", true, false},
		{".log", "text/plain", true, false},
		{".bmp", "", false, true},
		{".exe", "", false, true},
		{"", "", false, true},
	}

	for _, tt := range tests {
		t.Run(tt.ext, func(t *testing.T) {
			mime, isText, err := fileTypeFromExt(tt.ext)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if mime != tt.wantMime {
				t.Errorf("mime = %q, want %q", mime, tt.wantMime)
			}
			if isText != tt.wantText {
				t.Errorf("isText = %v, want %v", isText, tt.wantText)
			}
		})
	}
}

func TestResolveAttachments(t *testing.T) {
	t.Run("text file read and deleted", func(t *testing.T) {
		dir := t.TempDir()
		txtPath := filepath.Join(dir, "notes.txt")
		if err := os.WriteFile(txtPath, []byte("hello world"), 0644); err != nil {
			t.Fatal(err)
		}

		handoff := backends.HandoffData{
			Attachments: map[string]string{"notes": "notes.txt"},
		}
		if err := resolveAttachments(&handoff, dir); err != nil {
			t.Fatal(err)
		}
		att, ok := handoff.ResolvedAttachments["notes"]
		if !ok {
			t.Fatal("expected 'notes' in resolved attachments")
		}
		if !att.IsText {
			t.Error("expected IsText=true")
		}
		if att.Text != "hello world" {
			t.Errorf("text = %q, want %q", att.Text, "hello world")
		}
		if att.MimeType != "text/plain" {
			t.Errorf("mime = %q, want text/plain", att.MimeType)
		}
		// File should be deleted
		if _, err := os.Stat(txtPath); !os.IsNotExist(err) {
			t.Error("text file should have been deleted")
		}
	})

	t.Run("image file read and base64 encoded", func(t *testing.T) {
		dir := t.TempDir()
		imgPath := filepath.Join(dir, "photo.png")
		if err := os.WriteFile(imgPath, []byte("fake png"), 0644); err != nil {
			t.Fatal(err)
		}

		handoff := backends.HandoffData{
			Attachments: map[string]string{"photo": "photo.png"},
		}
		if err := resolveAttachments(&handoff, dir); err != nil {
			t.Fatal(err)
		}
		att, ok := handoff.ResolvedAttachments["photo"]
		if !ok {
			t.Fatal("expected 'photo' in resolved attachments")
		}
		if att.IsText {
			t.Error("expected IsText=false for image")
		}
		if att.Base64 == "" {
			t.Error("expected base64 to be populated")
		}
		if att.MimeType != "image/png" {
			t.Errorf("mime = %q, want image/png", att.MimeType)
		}
		// File should be deleted
		if _, err := os.Stat(imgPath); !os.IsNotExist(err) {
			t.Error("image file should have been deleted")
		}
	})

	t.Run("path traversal blocked", func(t *testing.T) {
		dir := t.TempDir()
		handoff := backends.HandoffData{
			Attachments: map[string]string{"evil": "../../../etc/passwd"},
		}
		err := resolveAttachments(&handoff, dir)
		if err == nil {
			t.Fatal("expected error for path traversal")
		}
		if !strings.Contains(err.Error(), "outside allowed directory") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("missing file skipped with warning", func(t *testing.T) {
		dir := t.TempDir()
		handoff := backends.HandoffData{
			Attachments: map[string]string{"missing": "nonexistent.txt"},
		}
		if err := resolveAttachments(&handoff, dir); err != nil {
			t.Fatal(err)
		}
		if _, ok := handoff.ResolvedAttachments["missing"]; ok {
			t.Error("expected missing file to be skipped")
		}
	})

	t.Run("mixed text and image attachments", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "doc.md"), []byte("# Title"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "pic.jpg"), []byte("jpeg data"), 0644); err != nil {
			t.Fatal(err)
		}

		handoff := backends.HandoffData{
			Attachments: map[string]string{
				"doc": "doc.md",
				"pic": "pic.jpg",
			},
		}
		if err := resolveAttachments(&handoff, dir); err != nil {
			t.Fatal(err)
		}
		if len(handoff.ResolvedAttachments) != 2 {
			t.Fatalf("expected 2 resolved attachments, got %d", len(handoff.ResolvedAttachments))
		}
		if !handoff.ResolvedAttachments["doc"].IsText {
			t.Error("doc should be text")
		}
		if handoff.ResolvedAttachments["pic"].IsText {
			t.Error("pic should not be text")
		}
	})

	t.Run("empty map is no-op", func(t *testing.T) {
		handoff := backends.HandoffData{Prompt: "hello"}
		if err := resolveAttachments(&handoff, "/tmp"); err != nil {
			t.Fatal(err)
		}
		if handoff.ResolvedAttachments != nil {
			t.Error("expected nil ResolvedAttachments for empty map")
		}
	})

	t.Run("text file over 1MB limit", func(t *testing.T) {
		dir := t.TempDir()
		bigFile := filepath.Join(dir, "big.txt")
		data := make([]byte, maxTextFileSize+1)
		for i := range data {
			data[i] = 'x'
		}
		if err := os.WriteFile(bigFile, data, 0644); err != nil {
			t.Fatal(err)
		}

		handoff := backends.HandoffData{
			Attachments: map[string]string{"big": "big.txt"},
		}
		err := resolveAttachments(&handoff, dir)
		if err == nil {
			t.Fatal("expected error for oversized text file")
		}
		if !strings.Contains(err.Error(), "limit") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("attachment_types overrides extension detection", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "photo.dat"), []byte("jpeg data"), 0644); err != nil {
			t.Fatal(err)
		}
		handoff := backends.HandoffData{
			Attachments:     map[string]string{"photo": "photo.dat"},
			AttachmentTypes: map[string]string{"photo": "image/jpeg"},
		}
		if err := resolveAttachments(&handoff, dir); err != nil {
			t.Fatal(err)
		}
		att, ok := handoff.ResolvedAttachments["photo"]
		if !ok {
			t.Fatal("expected 'photo' in resolved attachments")
		}
		if att.MimeType != "image/jpeg" {
			t.Errorf("mime = %q, want image/jpeg", att.MimeType)
		}
		if att.IsText {
			t.Error("image/jpeg should not be text")
		}
		if att.Base64 == "" {
			t.Error("expected base64 to be populated")
		}
	})

	t.Run("attachment_types text override inlines content", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "data.bin"), []byte("plain text"), 0644); err != nil {
			t.Fatal(err)
		}
		handoff := backends.HandoffData{
			Attachments:     map[string]string{"doc": "data.bin"},
			AttachmentTypes: map[string]string{"doc": "text/plain"},
		}
		if err := resolveAttachments(&handoff, dir); err != nil {
			t.Fatal(err)
		}
		att := handoff.ResolvedAttachments["doc"]
		if !att.IsText {
			t.Error("text/plain should be text")
		}
		if att.Text != "plain text" {
			t.Errorf("text = %q, want %q", att.Text, "plain text")
		}
	})

	t.Run("unknown extension allowed when attachment_types provided", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "file.xyz"), []byte("data"), 0644); err != nil {
			t.Fatal(err)
		}
		handoff := backends.HandoffData{
			Attachments:     map[string]string{"f": "file.xyz"},
			AttachmentTypes: map[string]string{"f": "application/octet-stream"},
		}
		// Should succeed because attachment_types bypasses extension detection
		if err := resolveAttachments(&handoff, dir); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := handoff.ResolvedAttachments["f"]; !ok {
			t.Error("expected 'f' in resolved attachments")
		}
	})

	t.Run("unknown extension errors", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "file.xyz"), []byte("data"), 0644); err != nil {
			t.Fatal(err)
		}
		handoff := backends.HandoffData{
			Attachments: map[string]string{"f": "file.xyz"},
		}
		err := resolveAttachments(&handoff, dir)
		if err == nil {
			t.Fatal("expected error for unknown extension")
		}
		if !strings.Contains(err.Error(), "unknown file extension") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("empty allowedDir rejected", func(t *testing.T) {
		handoff := backends.HandoffData{
			Attachments: map[string]string{"f": "test.txt"},
		}
		err := resolveAttachments(&handoff, "")
		if err == nil {
			t.Fatal("expected error for empty allowedDir")
		}
		if !strings.Contains(err.Error(), "data_dir is not configured") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("relative allowedDir rejected", func(t *testing.T) {
		handoff := backends.HandoffData{
			Attachments: map[string]string{"f": "test.txt"},
		}
		err := resolveAttachments(&handoff, "relative/path")
		if err == nil {
			t.Fatal("expected error for relative allowedDir")
		}
		if !strings.Contains(err.Error(), "data_dir is not configured") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("symlink escape blocked", func(t *testing.T) {
		dir := t.TempDir()
		// Create a symlink inside dir that points outside
		linkPath := filepath.Join(dir, "escape")
		if err := os.Symlink("/etc", linkPath); err != nil {
			t.Skip("symlinks not supported")
		}
		handoff := backends.HandoffData{
			Attachments: map[string]string{"evil": "escape/passwd"},
		}
		err := resolveAttachments(&handoff, dir)
		if err == nil {
			// EvalSymlinks may fail if /etc/passwd doesn't exist (containers).
			// Either way, the attachment must not be resolved.
			if _, ok := handoff.ResolvedAttachments["evil"]; ok {
				t.Error("symlink escape should not resolve successfully")
			}
		} else if !strings.Contains(err.Error(), "resolves outside allowed directory") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("binary file over 20MB limit", func(t *testing.T) {
		dir := t.TempDir()
		bigFile := filepath.Join(dir, "big.png")
		// Create a file just over the limit using sparse file (fast, no actual disk write)
		f, err := os.Create(bigFile)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(maxBinaryFileSize + 1); err != nil {
			f.Close()
			t.Fatal(err)
		}
		f.Close()

		handoff := backends.HandoffData{
			Attachments: map[string]string{"big": "big.png"},
		}
		err = resolveAttachments(&handoff, dir)
		if err == nil {
			t.Fatal("expected error for oversized binary file")
		}
		if !strings.Contains(err.Error(), "binary file exceeds") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("FIFO skipped", func(t *testing.T) {
		dir := t.TempDir()
		fifoPath := filepath.Join(dir, "pipe.txt")
		if err := syscall.Mkfifo(fifoPath, 0600); err != nil {
			t.Skip("cannot create FIFO:", err)
		}
		handoff := backends.HandoffData{
			Attachments: map[string]string{"p": "pipe.txt"},
		}
		err := resolveAttachments(&handoff, dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// FIFO should be skipped (not a regular file), not resolved
		if _, ok := handoff.ResolvedAttachments["p"]; ok {
			t.Error("FIFO should not be resolved as attachment")
		}
	})
}
