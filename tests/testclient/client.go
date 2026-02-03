// Package testclient provides SCM_RIGHTS socket handoff for testing streaming daemons.
//
// This package reuses the socket pair and fd passing code from the load generator
// to create connections that can be handed off to streaming daemons.
package testclient

import (
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
)

// Client provides methods for testing streaming daemons via SCM_RIGHTS handoff.
type Client struct {
	daemonSocket string
	timeout      time.Duration
}

// NewClient creates a new test client.
func NewClient(daemonSocket string, timeout time.Duration) *Client {
	return &Client{
		daemonSocket: daemonSocket,
		timeout:      timeout,
	}
}

// HandoffResult contains the result of a handoff operation.
type HandoffResult struct {
	// ResponseConn is the connection to read the daemon's response from.
	// The caller is responsible for closing this connection.
	ResponseConn net.Conn
	// HandoffDuration is how long the handoff took.
	HandoffDuration time.Duration
}

// Handoff sends handoff data to the daemon and returns a connection to read the response.
// The caller must close ResponseConn when done.
func (c *Client) Handoff(handoffData []byte) (*HandoffResult, error) {
	// Create socket pair - clientConn will be sent to daemon, responseConn is for reading
	clientConn, responseConn, err := createSocketPair()
	if err != nil {
		return nil, fmt.Errorf("create socket pair: %w", err)
	}

	// Connect to daemon Unix socket
	daemonConn, err := net.DialTimeout("unix", c.daemonSocket, c.timeout)
	if err != nil {
		clientConn.Close()
		responseConn.Close()
		return nil, fmt.Errorf("dial daemon: %w", err)
	}

	// Get the raw fd from clientConn to send via SCM_RIGHTS
	clientFile, err := clientConn.File()
	if err != nil {
		clientConn.Close()
		responseConn.Close()
		daemonConn.Close()
		return nil, fmt.Errorf("get fd: %w", err)
	}

	handoffStart := time.Now()

	// Send fd to daemon via SCM_RIGHTS
	unixDaemon := daemonConn.(*net.UnixConn)
	if err := sendFd(unixDaemon, int(clientFile.Fd()), handoffData); err != nil {
		clientFile.Close()
		clientConn.Close()
		responseConn.Close()
		daemonConn.Close()
		return nil, fmt.Errorf("send fd: %w", err)
	}

	handoffDuration := time.Since(handoffStart)

	// Close our copies - daemon now owns the fd
	clientFile.Close()
	clientConn.Close()
	daemonConn.Close()

	// Set read timeout on response connection
	if c.timeout > 0 {
		responseConn.SetDeadline(time.Now().Add(c.timeout))
	}

	return &HandoffResult{
		ResponseConn:    responseConn,
		HandoffDuration: handoffDuration,
	}, nil
}

// createSocketPair creates a connected pair of Unix sockets.
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

// sendFd sends a file descriptor over a Unix socket using SCM_RIGHTS.
func sendFd(conn *net.UnixConn, fd int, data []byte) error {
	rights := syscall.UnixRights(fd)
	_, _, err := conn.WriteMsgUnix(data, rights, nil)
	return err
}
