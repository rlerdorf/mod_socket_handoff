//! Integration tests for socket handoff.
//!
//! These tests require a Unix-like environment with socket support.

use std::io::{Read, Write};
use std::os::unix::io::{AsRawFd, FromRawFd, OwnedFd, RawFd};
use std::os::unix::net::UnixStream;

use nix::cmsg_space;
use nix::sys::socket::{
    recvmsg, sendmsg, ControlMessage, ControlMessageOwned, MsgFlags, SockaddrStorage,
};
use std::io::{IoSlice, IoSliceMut};

/// Test helper to send a file descriptor over a Unix socket.
fn send_fd(socket: &UnixStream, fd_to_send: i32, data: &[u8]) -> std::io::Result<()> {
    let iov = [IoSlice::new(data)];
    let fds = [fd_to_send];
    let cmsg = [ControlMessage::ScmRights(&fds)];

    sendmsg::<SockaddrStorage>(
        socket.as_raw_fd(),
        &iov,
        &cmsg,
        MsgFlags::empty(),
        None,
    )
    .map_err(|e| std::io::Error::new(std::io::ErrorKind::Other, e))?;

    Ok(())
}

/// Test helper to receive a file descriptor over a Unix socket.
fn recv_fd(socket: &UnixStream, buffer_size: usize) -> std::io::Result<(OwnedFd, Vec<u8>)> {
    let mut data_buf = vec![0u8; buffer_size];
    let mut cmsg_buf = cmsg_space!([RawFd; 1]);
    let mut iov = [IoSliceMut::new(&mut data_buf)];

    let msg = recvmsg::<SockaddrStorage>(
        socket.as_raw_fd(),
        &mut iov,
        Some(&mut cmsg_buf),
        MsgFlags::empty(),
    )
    .map_err(|e| std::io::Error::new(std::io::ErrorKind::Other, e))?;

    let bytes_received = msg.bytes;

    // Extract file descriptor from control message
    let mut received_fd: Option<OwnedFd> = None;

    for cmsg in msg
        .cmsgs()
        .map_err(|e| std::io::Error::new(std::io::ErrorKind::Other, e))?
    {
        if let ControlMessageOwned::ScmRights(fds) = cmsg {
            if !fds.is_empty() {
                // SAFETY: The fd was received via SCM_RIGHTS and is valid
                received_fd = Some(unsafe { OwnedFd::from_raw_fd(fds[0]) });
                break;
            }
        }
    }

    let fd = received_fd.ok_or_else(|| {
        std::io::Error::new(std::io::ErrorKind::Other, "No file descriptor received")
    })?;

    // Truncate data_buf to actual received bytes
    data_buf.truncate(bytes_received);

    Ok((fd, data_buf))
}

#[test]
fn test_scm_rights_send() {
    // Test that SCM_RIGHTS sending and receiving works
    // This validates both sending and receiving file descriptors

    // Create a socket pair
    let (sender, receiver) = UnixStream::pair().unwrap();

    // Create a dummy TCP socket to send
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let addr = listener.local_addr().unwrap();
    let client = std::net::TcpStream::connect(addr).unwrap();
    let (accepted, _) = listener.accept().unwrap();

    let data = br#"{"user_id": 123, "prompt": "test"}"#;

    // Send the fd
    let fd = accepted.as_raw_fd();
    send_fd(&sender, fd, data).unwrap();

    // Receive the fd on the other end
    let (received_fd, received_data) = recv_fd(&receiver, 1024).unwrap();

    // Verify the data was received correctly
    assert_eq!(received_data, data);

    // Verify the fd works by converting it to a TcpStream and performing I/O
    // SAFETY: received_fd is a valid TCP socket fd received via SCM_RIGHTS
    let mut received_stream =
        unsafe { std::net::TcpStream::from_raw_fd(received_fd.as_raw_fd()) };

    // Write some data through the received socket
    let test_data = b"Hello from received fd";
    received_stream.write_all(test_data).unwrap();

    // Read it back on the client side to verify the fd works
    let mut client_buf = vec![0u8; test_data.len()];
    let mut client = client;
    client.read_exact(&mut client_buf).unwrap();
    assert_eq!(client_buf, test_data);

    // Clean up: received_fd is still owned by OwnedFd, so we need to forget the TcpStream
    // to avoid double-close
    std::mem::forget(received_stream);
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
