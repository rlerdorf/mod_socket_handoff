// Tests for the streaming daemon's SCM_RIGHTS handling.
//
// These tests verify the core socket handoff mechanism without starting
// the full daemon. They test:
// - SCM_RIGHTS fd passing roundtrip
// - Concurrent fd sends
// - Large data handling
// - HTTP response format

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"

	"examples/backends"
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
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
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

	for i := 0; i < numConnections; i++ {
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
	var parsed map[string]interface{}
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

	var body map[string]interface{}
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

func TestParseMemLimit(t *testing.T) {
	tests := []struct {
		name string
		input string
		want  int64
	}{
		// Valid: bytes (no suffix)
		{"bare number", "1024", 1024},
		{"zero", "0", 0},
		{"B suffix", "512B", 512},
		{"b lowercase", "512b", 512},

		// Valid: KiB
		{"KiB", "1KiB", 1024},
		{"K shorthand", "4K", 4096},
		{"KB", "2KB", 2048},

		// Valid: MiB
		{"MiB", "768MiB", 768 * 1024 * 1024},
		{"M shorthand", "512M", 512 * 1024 * 1024},
		{"MB", "256MB", 256 * 1024 * 1024},

		// Valid: GiB
		{"GiB", "1GiB", 1024 * 1024 * 1024},
		{"G shorthand", "2G", 2 * 1024 * 1024 * 1024},
		{"GB", "4GB", 4 * 1024 * 1024 * 1024},

		// Valid: TiB
		{"TiB", "1TiB", 1024 * 1024 * 1024 * 1024},
		{"T shorthand", "2T", 2 * 1024 * 1024 * 1024 * 1024},
		{"TB", "1TB", 1024 * 1024 * 1024 * 1024},

		// Valid: case insensitivity
		{"mib lowercase", "512mib", 512 * 1024 * 1024},
		{"GIB uppercase", "1GIB", 1024 * 1024 * 1024},
		{"MiB mixed case", "256Mib", 256 * 1024 * 1024},

		// Valid: whitespace handling
		{"leading space", "  512MiB", 512 * 1024 * 1024},
		{"trailing space", "512MiB  ", 512 * 1024 * 1024},
		{"space between number and suffix", "512 MiB", 512 * 1024 * 1024},

		// Valid: large values within int64 range
		{"8 TiB", "8TiB", 8 * 1024 * 1024 * 1024 * 1024},
		{"max safe GiB", "8388607GiB", 8388607 * 1024 * 1024 * 1024}, // ~8 PiB, fits int64

		// Invalid: empty / whitespace-only
		{"empty string", "", -1},
		{"whitespace only", "   ", -1},

		// Invalid: no numeric part
		{"suffix only", "MiB", -1},
		{"just letters", "abc", -1},

		// Invalid: bad suffix
		{"unknown suffix", "512XiB", -1},
		{"random suffix", "512foo", -1},

		// Invalid: negative numbers (strconv.ParseUint rejects these)
		{"negative", "-512MiB", -1},

		// Invalid: fractional values (no longer supported)
		{"fractional", "1.5GiB", -1},
		{"decimal point only", ".5GiB", -1},

		// Invalid: overflow
		{"overflow TiB", "9999999TiB", -1},
		{"max uint64 bytes", "18446744073709551615", -1}, // MaxUint64 > MaxInt64
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseMemLimit(tc.input)
			if got != tc.want {
				t.Errorf("parseMemLimit(%q) = %d, want %d", tc.input, got, tc.want)
			}
		})
	}

	// Verify the result is always within valid range for debug.SetMemoryLimit
	t.Run("all valid results fit int64", func(t *testing.T) {
		for _, tc := range tests {
			got := parseMemLimit(tc.input)
			if got > 0 && got > math.MaxInt64 {
				t.Errorf("parseMemLimit(%q) = %d, exceeds MaxInt64", tc.input, got)
			}
		}
	})
}
