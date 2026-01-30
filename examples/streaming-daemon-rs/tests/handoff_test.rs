//! Integration tests for socket handoff.
//!
//! These tests require a Unix-like environment with socket support.

#[allow(unused_imports)]
use std::io::{Read, Write};
use std::os::unix::io::AsRawFd;
use std::os::unix::net::UnixStream;

use nix::sys::socket::{sendmsg, ControlMessage, MsgFlags, SockaddrStorage};
use std::io::IoSlice;

/// Test helper to send a file descriptor over a Unix socket.
fn send_fd(socket: &UnixStream, fd_to_send: i32, data: &[u8]) -> std::io::Result<()> {
    let iov = [IoSlice::new(data)];
    let fds = [fd_to_send];
    let cmsg = [ControlMessage::ScmRights(&fds)];

    sendmsg::<SockaddrStorage>(socket.as_raw_fd(), &iov, &cmsg, MsgFlags::empty(), None)
        .map_err(std::io::Error::other)?;

    Ok(())
}

#[test]
fn test_scm_rights_send() {
    // This is a basic test that SCM_RIGHTS sending works
    // A full integration test would require starting the daemon

    // Create a socket pair
    let (sender, receiver) = UnixStream::pair().unwrap();

    // Create a dummy TCP socket to send
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let addr = listener.local_addr().unwrap();
    let _client = std::net::TcpStream::connect(addr).unwrap();
    let (accepted, _) = listener.accept().unwrap();

    let data = br#"{"user_id": 123, "prompt": "test"}"#;

    // Send the fd
    let fd = accepted.as_raw_fd();
    send_fd(&sender, fd, data).unwrap();

    // For a real test, we'd receive on the other end
    // and verify the fd works
    drop(receiver);
}

#[test]
fn test_handoff_data_parsing() {
    use serde::Deserialize;

    #[derive(Debug, Deserialize)]
    struct HandoffData {
        user_id: Option<i64>,
        prompt: Option<String>,
    }

    let json = r#"{"user_id": 123, "prompt": "Hello world"}"#;
    let data: HandoffData = serde_json::from_str(json).unwrap();

    assert_eq!(data.user_id, Some(123));
    assert_eq!(data.prompt, Some("Hello world".to_string()));
}
