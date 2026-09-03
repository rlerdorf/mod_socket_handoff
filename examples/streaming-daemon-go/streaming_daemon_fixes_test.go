package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"examples/backends"
	"examples/config"
)

// scriptedBackend is a test backend whose Stream behaviour is a closure.
type scriptedBackend struct {
	stream func(ctx context.Context, conn net.Conn) (int64, error)
}

func (s *scriptedBackend) Name() string                     { return "scripted" }
func (s *scriptedBackend) Description() string              { return "test" }
func (s *scriptedBackend) Init(*config.BackendConfig) error { return nil }
func (s *scriptedBackend) Stream(ctx context.Context, conn net.Conn, _ backends.HandoffData) (int64, error) {
	return s.stream(ctx, conn)
}

func withActiveBackend(t *testing.T, b backends.Backend) {
	t.Helper()
	orig := activeBackend
	activeBackend = b
	t.Cleanup(func() { activeBackend = orig })
}

// readAllFrom runs streamToClientWithBytes on a pipe and returns what the client saw.
func runStreamToPipe(t *testing.T, ctx context.Context) (string, error) {
	t.Helper()
	server, client := net.Pipe()
	defer client.Close()
	errCh := make(chan error, 1)
	go func() {
		defer server.Close()
		_, err := streamToClientWithBytes(ctx, server, backends.HandoffData{})
		errCh <- err
	}()
	resp, err := io.ReadAll(client)
	if err != nil {
		t.Fatal(err)
	}
	return string(resp), <-errCh
}

// Before the backend has written anything, an upstream failure must produce a
// real error status rather than an empty "200 text/event-stream".
func TestStreamErrorBeforeHeadersSendsStatus(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus string
	}{
		{"generic upstream error", errors.New("API error 500: boom"), "HTTP/1.1 502 Bad Gateway"},
		{"deadline exceeded", fmt.Errorf("http: %w", context.DeadlineExceeded), "HTTP/1.1 504 Gateway Timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withActiveBackend(t, &scriptedBackend{stream: func(context.Context, net.Conn) (int64, error) {
				return 0, tt.err
			}})
			resp, err := runStreamToPipe(t, context.Background())
			if err == nil {
				t.Error("expected error to be returned")
			}
			if !strings.HasPrefix(resp, tt.wantStatus) {
				t.Errorf("response = %q, want prefix %q", resp, tt.wantStatus)
			}
			if strings.Contains(resp, "text/event-stream") {
				t.Error("SSE headers were sent despite backend failing before first write")
			}
		})
	}
}

// Once SSE headers are out, a failure must be signalled as an SSE error event.
func TestStreamErrorAfterHeadersSendsSSEError(t *testing.T) {
	withActiveBackend(t, &scriptedBackend{stream: func(_ context.Context, conn net.Conn) (int64, error) {
		n, err := backends.SendSSE(conn, "partial")
		if err != nil {
			return int64(n), err
		}
		return int64(n), errors.New("upstream died")
	}})
	resp, _ := runStreamToPipe(t, context.Background())
	if !strings.HasPrefix(resp, "HTTP/1.1 200 OK") || !strings.Contains(resp, "text/event-stream") {
		t.Errorf("expected SSE response, got %q", resp)
	}
	if !strings.Contains(resp, `data: {"content":"partial"}`) {
		t.Errorf("partial content missing: %q", resp)
	}
	if !strings.HasSuffix(resp, `data: {"error":"upstream error"}`+"\n\n") {
		t.Errorf("SSE error event missing at end: %q", resp)
	}
}

// Headers must be written exactly once and counted in the byte total.
func TestLazyHeaderConnWritesHeadersOnce(t *testing.T) {
	withActiveBackend(t, &scriptedBackend{stream: func(_ context.Context, conn net.Conn) (int64, error) {
		var total int64
		for _, s := range []string{"a", "b"} {
			n, err := backends.SendSSE(conn, s)
			total += int64(n)
			if err != nil {
				return total, err
			}
		}
		return total, nil
	}})
	resp, err := runStreamToPipe(t, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(resp, "HTTP/1.1 200 OK") != 1 {
		t.Errorf("status line count = %d, want 1: %q", strings.Count(resp, "HTTP/1.1 200 OK"), resp)
	}
	want := string(sseHeadersBytes) + `data: {"content":"a"}` + "\n\n" + `data: {"content":"b"}` + "\n\n"
	if resp != want {
		t.Errorf("response mismatch:\ngot:  %q\nwant: %q", resp, want)
	}
}

// tcpPair returns a connected loopback TCP pair (client side, server side).
func tcpPair(t *testing.T) (client net.Conn, server *net.TCPConn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			accepted <- nil
			return
		}
		accepted <- c
	}()
	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	s := <-accepted
	if s == nil {
		t.Fatal("accept failed")
	}
	return client, s.(*net.TCPConn)
}

// handoffTCPClient simulates Apache: sends the server side of a TCP pair plus
// handoff JSON to the daemon over a SEQPACKET socket. Returns the browser-side
// conn and the daemon-side Unix conn.
func handoffTCPClient(t *testing.T, data string) (browser net.Conn, daemonSide *net.UnixConn) {
	t.Helper()
	apacheSide, daemonSide, err := createSocketPair()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { apacheSide.Close() })

	browser, serverSide := tcpPair(t)
	file, err := serverSide.File()
	if err != nil {
		t.Fatal(err)
	}
	if err := sendFd(apacheSide, int(file.Fd()), []byte(data)); err != nil {
		t.Fatal(err)
	}
	// Apache would now swap in a dummy socket and drop its references.
	file.Close()
	serverSide.Close()
	return browser, daemonSide
}

// Over-capacity and shutdown-path connections must get a 503, not a dropped socket.
func TestRejectHandoffSends503(t *testing.T) {
	browser, daemonSide := handoffTCPClient(t, `{"prompt":"x"}`)
	defer browser.Close()

	rejectHandoff(daemonSide, "test")

	browser.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := io.ReadAll(browser)
	if err != nil {
		t.Fatal(err)
	}
	got := string(resp)
	if !strings.HasPrefix(got, "HTTP/1.1 503 Service Unavailable") {
		t.Errorf("response = %q, want 503", got)
	}
	if !strings.Contains(got, "Retry-After: 1\r\n") {
		t.Errorf("missing Retry-After header: %q", got)
	}
}

// Malformed handoff JSON must be answered with a 400, not streamed as a
// default request.
func TestMalformedHandoffReturns400(t *testing.T) {
	withActiveBackend(t, &scriptedBackend{stream: func(context.Context, net.Conn) (int64, error) {
		t.Error("backend must not run for malformed handoff data")
		return 0, nil
	}})
	browser, daemonSide := handoffTCPClient(t, `{"prompt":`)
	defer browser.Close()

	handleConnection(context.Background(), daemonSide)

	browser.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, _ := io.ReadAll(browser)
	if !strings.HasPrefix(string(resp), "HTTP/1.1 400 Bad Request") {
		t.Errorf("response = %q, want 400", resp)
	}
}

// A client that disconnects while upstream is silent must cancel the stream
// promptly instead of keeping the upstream run alive until the next write.
func TestClientDisconnectCancelsStream(t *testing.T) {
	var sawCancel atomic.Bool
	withActiveBackend(t, &scriptedBackend{stream: func(ctx context.Context, conn net.Conn) (int64, error) {
		n, err := backends.SendSSE(conn, "first")
		if err != nil {
			return int64(n), err
		}
		// Simulate a long upstream pause: nothing is written until ctx ends.
		select {
		case <-ctx.Done():
			sawCancel.Store(true)
			return int64(n), ctx.Err()
		case <-time.After(20 * time.Second):
			return int64(n), errors.New("upstream pause was never interrupted")
		}
	}})

	browser, daemonSide := handoffTCPClient(t, `{"prompt":"x"}`)
	before := atomic.LoadInt64(&activeStreams)

	done := make(chan struct{})
	go func() {
		handleConnection(context.Background(), daemonSide)
		close(done)
	}()

	// Wait for the first event so we know the stream is in its silent phase.
	reader := bufio.NewReader(browser)
	browser.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("reading first event: %v", err)
		}
		if strings.HasPrefix(line, "data:") {
			break
		}
	}
	browser.Close()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handleConnection did not return within 5s of client disconnect")
	}
	if !sawCancel.Load() {
		t.Error("backend context was not cancelled on client disconnect")
	}
	if after := atomic.LoadInt64(&activeStreams); after != before {
		t.Errorf("activeStreams = %d after stream, want %d", after, before)
	}
}

// A panicking backend must not leave the active-stream gauge inflated, and
// the client must still get an error status.
func TestBackendPanicRestoresGaugeAndSends500(t *testing.T) {
	withActiveBackend(t, &scriptedBackend{stream: func(context.Context, net.Conn) (int64, error) {
		panic("backend exploded")
	}})
	browser, daemonSide := handoffTCPClient(t, `{"prompt":"x"}`)
	defer browser.Close()
	before := atomic.LoadInt64(&activeStreams)

	handleConnection(context.Background(), daemonSide)

	if after := atomic.LoadInt64(&activeStreams); after != before {
		t.Errorf("activeStreams = %d after panic, want %d", after, before)
	}
	browser.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, _ := io.ReadAll(browser)
	if !strings.HasPrefix(string(resp), "HTTP/1.1 500 Internal Server Error") {
		t.Errorf("response = %q, want 500", resp)
	}
}

func TestSweepDataDir(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "old.png")
	fresh := filepath.Join(dir, "fresh.png")
	sub := filepath.Join(dir, "subdir")
	for _, p := range []string{old, fresh} {
		if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(sub, 0755); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-time.Hour)
	if err := os.Chtimes(old, stale, stale); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(sub, stale, stale); err != nil {
		t.Fatal(err)
	}

	if n := sweepDataDir(dir, 10*time.Minute); n != 1 {
		t.Errorf("removed = %d, want 1", n)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("stale file should have been removed")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("fresh file should have been kept")
	}
	if _, err := os.Stat(sub); err != nil {
		t.Error("subdirectory should have been ignored")
	}
}

// When one attachment is rejected, the others must not be left behind on disk.
func TestDiscardStagedFilesAfterAttachmentError(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "notes.txt")
	bad := filepath.Join(dir, "payload.exe")
	outside := filepath.Join(t.TempDir(), "keep.txt")
	for _, p := range []string{good, bad, outside} {
		if err := os.WriteFile(p, []byte("data"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	handoff := backends.HandoffData{
		Attachments: map[string]string{"n": "notes.txt", "p": "payload.exe", "o": "../" + filepath.Base(filepath.Dir(outside)) + "/keep.txt"},
	}
	if err := resolveAttachments(&handoff, dir); err == nil {
		t.Fatal("expected error for unknown extension / traversal")
	}
	discardStagedFiles(&handoff, dir)

	for _, p := range []string{good, bad} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s should have been discarded", p)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Error("file outside data_dir must never be touched")
	}
}

// Relative image_paths resolve under data_dir, matching the attachments API.
func TestRelativeImagePathResolvedUnderDataDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "photo.png"), []byte("png"), 0644); err != nil {
		t.Fatal(err)
	}
	handoff := backends.HandoffData{ImagePaths: []string{"photo.png"}}
	if err := resolveImages(&handoff, dir); err != nil {
		t.Fatal(err)
	}
	if len(handoff.ResolvedImages) != 1 || handoff.ResolvedImages[0].MimeType != "image/png" {
		t.Errorf("resolved = %+v, want one image/png", handoff.ResolvedImages)
	}
	if _, err := os.Stat(filepath.Join(dir, "photo.png")); !os.IsNotExist(err) {
		t.Error("image should have been deleted after read")
	}
}

// The per-request budget caps the sum of all staged files, not just each one.
func TestTotalAttachmentBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("writes ~34 MiB of temp files")
	}
	dir := t.TempDir()
	chunk := make([]byte, 17<<20) // 17 MiB each: under the 20 MiB per-file cap, over the 32 MiB total
	for _, name := range []string{"a.pdf", "b.pdf"} {
		if err := os.WriteFile(filepath.Join(dir, name), chunk, 0644); err != nil {
			t.Fatal(err)
		}
	}
	handoff := backends.HandoffData{Attachments: map[string]string{"a": "a.pdf", "b": "b.pdf"}}
	err := resolveAttachments(&handoff, dir)
	if err == nil || !strings.Contains(err.Error(), "total attachment size") {
		t.Fatalf("resolveAttachments error = %v, want total-size error", err)
	}
}
