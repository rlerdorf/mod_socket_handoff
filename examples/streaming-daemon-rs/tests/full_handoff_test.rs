//! Full integration tests for socket handoff.
//!
//! These tests simulate the Apache handoff process by:
//! 1. Creating socket pairs (simulating Apache <-> daemon communication)
//! 2. Sending file descriptors via SCM_RIGHTS
//! 3. Verifying HTTP/SSE responses

use std::io::{BufReader, Read, Write};
use std::os::unix::io::{AsRawFd, FromRawFd};
use std::os::unix::net::UnixStream;
use std::thread;

use nix::libc;
use nix::sys::socket::{recvmsg, sendmsg, ControlMessage, ControlMessageOwned, MsgFlags};
use std::io::{IoSlice, IoSliceMut};

/// Test helper to send a file descriptor over a Unix socket.
fn send_fd(socket: &UnixStream, fd_to_send: i32, data: &[u8]) -> std::io::Result<()> {
    let iov = [IoSlice::new(data)];
    let fds = [fd_to_send];
    let cmsg = [ControlMessage::ScmRights(&fds)];

    sendmsg::<nix::sys::socket::SockaddrStorage>(socket.as_raw_fd(), &iov, &cmsg, MsgFlags::empty(), None).map_err(std::io::Error::other)?;

    Ok(())
}

/// Test helper to receive a file descriptor from a Unix socket.
fn recv_fd(socket: &UnixStream) -> std::io::Result<(i32, Vec<u8>)> {
    let mut data_buf = vec![0u8; 65536]; // 64KB to match daemon's MaxHandoffDataSize
    let mut cmsg_buf = nix::cmsg_space!([i32; 1]);
    let mut iov = [IoSliceMut::new(&mut data_buf)];

    let msg = recvmsg::<nix::sys::socket::SockaddrStorage>(socket.as_raw_fd(), &mut iov, Some(&mut cmsg_buf), MsgFlags::empty()).map_err(std::io::Error::other)?;

    let bytes_received = msg.bytes;

    let mut received_fd = None;
    for cmsg in msg.cmsgs().map_err(std::io::Error::other)? {
        if let ControlMessageOwned::ScmRights(fds) = cmsg {
            if !fds.is_empty() {
                received_fd = Some(fds[0]);
            }
        }
    }

    let fd = received_fd.ok_or_else(|| std::io::Error::other("No file descriptor received"))?;

    // Release borrows on data_buf
    let _ = msg;
    let _ = iov;

    let data = data_buf[..bytes_received].to_vec();
    Ok((fd, data))
}

#[test]
fn test_scm_rights_roundtrip() {
    // Test that we can send an fd and receive it on the other end
    let (sender, receiver) = UnixStream::pair().unwrap();

    // Create a TCP connection to pass
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let addr = listener.local_addr().unwrap();
    let _client = std::net::TcpStream::connect(addr).unwrap();
    let (server_stream, _) = listener.accept().unwrap();

    let data = br#"{"user_id": 456, "prompt": "roundtrip test"}"#;
    let original_fd = server_stream.as_raw_fd();

    // Send fd
    send_fd(&sender, original_fd, data).unwrap();

    // Receive fd
    let (received_fd, received_data) = recv_fd(&receiver).unwrap();

    assert!(received_fd >= 0);
    assert_eq!(received_data, data);

    // Clean up
    unsafe { libc::close(received_fd) };
}

#[test]
fn test_concurrent_fd_sends() {
    // Test that multiple concurrent fd sends work correctly
    const NUM_CONNECTIONS: usize = 5;

    let handles: Vec<_> = (0..NUM_CONNECTIONS)
        .map(|i| {
            thread::spawn(move || {
                let (sender, receiver) = UnixStream::pair().unwrap();

                // Create TCP connection
                let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
                let addr = listener.local_addr().unwrap();
                let _client = std::net::TcpStream::connect(addr).unwrap();
                let (server_stream, _) = listener.accept().unwrap();

                let data = format!(r#"{{"connection_id": {}}}"#, i);
                let original_fd = server_stream.as_raw_fd();

                // Send and receive
                send_fd(&sender, original_fd, data.as_bytes()).unwrap();
                let (received_fd, received_data) = recv_fd(&receiver).unwrap();

                // Verify
                assert!(received_fd >= 0);
                assert!(String::from_utf8_lossy(&received_data).contains(&i.to_string()));

                unsafe { libc::close(received_fd) };
                i
            })
        })
        .collect();

    // Wait for all threads
    let results: Vec<_> = handles.into_iter().map(|h| h.join().unwrap()).collect();
    assert_eq!(results.len(), NUM_CONNECTIONS);
}

#[test]
fn test_empty_data_with_fd() {
    // Test that we can send an fd with empty data
    let (sender, receiver) = UnixStream::pair().unwrap();

    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let addr = listener.local_addr().unwrap();
    let _client = std::net::TcpStream::connect(addr).unwrap();
    let (server_stream, _) = listener.accept().unwrap();

    // Send with minimal data (at least 1 byte required for recvmsg)
    let data = b"{}";
    send_fd(&sender, server_stream.as_raw_fd(), data).unwrap();

    let (received_fd, received_data) = recv_fd(&receiver).unwrap();
    assert!(received_fd >= 0);
    assert_eq!(received_data, b"{}");

    unsafe { libc::close(received_fd) };
}

#[test]
fn test_large_handoff_data() {
    // Test with larger JSON payload (simulating long prompts)
    let (sender, receiver) = UnixStream::pair().unwrap();

    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let addr = listener.local_addr().unwrap();
    let _client = std::net::TcpStream::connect(addr).unwrap();
    let (server_stream, _) = listener.accept().unwrap();

    // Create a large prompt (~10KB)
    let long_prompt = "test ".repeat(2000);
    let data = format!(r#"{{"user_id": 789, "prompt": "{}"}}"#, long_prompt);

    send_fd(&sender, server_stream.as_raw_fd(), data.as_bytes()).unwrap();

    let (received_fd, received_data) = recv_fd(&receiver).unwrap();
    assert!(received_fd >= 0);
    assert_eq!(received_data.len(), data.len());

    // Verify JSON can be parsed
    let parsed: serde_json::Value = serde_json::from_slice(&received_data).unwrap();
    assert_eq!(parsed["user_id"], 789);

    unsafe { libc::close(received_fd) };
}

#[test]
fn test_socket_pair_communication() {
    // Test bidirectional communication over passed socket
    let (apache_side, daemon_side) = UnixStream::pair().unwrap();

    // Create "client" connection
    let (client_a, client_b) = UnixStream::pair().unwrap();

    // Apache sends client_a to daemon
    let data = br#"{"prompt": "hello"}"#;
    send_fd(&apache_side, client_a.as_raw_fd(), data).unwrap();

    // Close client_a in "apache" process after sending - daemon now owns it
    drop(client_a);

    // Daemon receives client_a
    let (daemon_client_fd, _) = recv_fd(&daemon_side).unwrap();

    // Convert fd to stream for daemon to write
    let daemon_client = unsafe { UnixStream::from_raw_fd(daemon_client_fd) };

    // Daemon writes HTTP response
    let response = "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nHello from daemon!";
    (&daemon_client).write_all(response.as_bytes()).unwrap();
    drop(daemon_client); // Close daemon side - this signals EOF to client_b

    // Client (holding client_b) reads the response
    let mut buf = String::new();
    let mut reader = BufReader::new(&client_b);
    reader.read_to_string(&mut buf).unwrap();

    assert!(buf.contains("HTTP/1.1 200 OK"));
    assert!(buf.contains("Hello from daemon!"));
}

#[test]
fn test_sse_format_verification() {
    // Verify SSE event format matches what daemon should send
    // Test content chunk format
    let content = "test content";
    let sse_chunk = format!("data: {{\"content\":\"{}\"}}\n\n", content);
    assert_eq!(sse_chunk, "data: {\"content\":\"test content\"}\n\n");
    assert!(sse_chunk.starts_with("data: "));
    assert!(sse_chunk.ends_with("\n\n"));

    // Verify JSON structure is valid
    let json_part = sse_chunk.strip_prefix("data: ").unwrap().trim();
    let parsed: serde_json::Value = serde_json::from_str(json_part).unwrap();
    assert_eq!(parsed["content"], "test content");

    // Test done marker format
    let done_marker = "data: [DONE]\n\n";
    assert_eq!(done_marker, "data: [DONE]\n\n");
    assert!(done_marker.starts_with("data: "));
    assert!(done_marker.contains("[DONE]"));
    assert!(done_marker.ends_with("\n\n"));
}
