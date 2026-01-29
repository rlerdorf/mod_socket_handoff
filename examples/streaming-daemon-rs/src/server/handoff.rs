//! SCM_RIGHTS file descriptor receiving.
//!
//! Receives client socket fd from Apache via Unix domain socket.

use nix::cmsg_space;
use nix::sys::socket::{
    getsockopt, recvmsg, sockopt::SockType, ControlMessageOwned, MsgFlags, SockaddrStorage,
};
use serde::Deserialize;
use std::io::IoSliceMut;
use std::os::unix::io::{AsRawFd, FromRawFd, OwnedFd, RawFd};
use std::time::Duration;
use tokio::io::Interest;
use tokio::net::UnixStream;

use crate::error::HandoffError;

/// Data passed from PHP via X-Handoff-Data header.
#[derive(Debug, Clone, Default, Deserialize)]
pub struct HandoffData {
    #[serde(default)]
    pub user_id: Option<i64>,
    #[serde(default)]
    pub prompt: Option<String>,
    #[serde(default)]
    pub model: Option<String>,
    #[serde(default)]
    pub max_tokens: Option<u32>,
    #[serde(default)]
    pub temperature: Option<f32>,
    #[serde(default)]
    pub system: Option<String>,
    #[serde(default)]
    pub request_id: Option<String>,
}

/// Result of receiving a handoff from Apache.
pub struct HandoffResult {
    /// The client socket file descriptor (owned).
    pub client_fd: OwnedFd,
    /// Parsed handoff data from PHP.
    pub data: HandoffData,
    /// Raw data bytes (for debugging).
    pub raw_data_len: usize,
}

/// Receive file descriptor and handoff data from Apache via SCM_RIGHTS.
///
/// This function:
/// 1. Waits for the Unix socket to be readable (with timeout)
/// 2. Uses recvmsg to receive both the data and the control message
/// 3. Extracts the file descriptor from SCM_RIGHTS
/// 4. Validates the socket type
/// 5. Parses the JSON handoff data
pub async fn receive_handoff(
    stream: &UnixStream,
    timeout: Duration,
    buffer_size: usize,
) -> Result<HandoffResult, HandoffError> {
    // Apply timeout to the entire handoff receive operation
    tokio::time::timeout(timeout, receive_handoff_inner(stream, buffer_size))
        .await
        .map_err(|_| HandoffError::Timeout)?
}

async fn receive_handoff_inner(
    stream: &UnixStream,
    buffer_size: usize,
) -> Result<HandoffResult, HandoffError> {
    // Wait for readable
    let ready = stream
        .ready(Interest::READABLE)
        .await
        .map_err(|e| HandoffError::ReceiveFailed(e.to_string()))?;

    if !ready.is_readable() {
        return Err(HandoffError::ReceiveFailed(
            "Socket not readable".to_string(),
        ));
    }

    // Get the raw fd for recvmsg
    let fd = stream.as_raw_fd();

    // Perform blocking recvmsg in spawn_blocking
    // Note: The outer timeout will cancel this if it takes too long
    let result = tokio::task::spawn_blocking(move || receive_fd_blocking(fd, buffer_size))
        .await
        .map_err(|e| HandoffError::ReceiveFailed(format!("spawn_blocking failed: {}", e)))??;

    Ok(result)
}

/// Blocking recvmsg implementation.
fn receive_fd_blocking(fd: RawFd, buffer_size: usize) -> Result<HandoffResult, HandoffError> {
    let mut data_buf = vec![0u8; buffer_size];
    let mut cmsg_buf = cmsg_space!([RawFd; 1]);

    let mut iov = [IoSliceMut::new(&mut data_buf)];

    let msg = recvmsg::<SockaddrStorage>(fd, &mut iov, Some(&mut cmsg_buf), MsgFlags::empty())
        .map_err(HandoffError::System)?;

    // Check for truncation
    if msg.flags.contains(MsgFlags::MSG_TRUNC) {
        return Err(HandoffError::DataTruncated);
    }
    if msg.flags.contains(MsgFlags::MSG_CTRUNC) {
        return Err(HandoffError::ControlMessageTruncated);
    }

    // Extract file descriptor from control message
    let mut received_fd: Option<OwnedFd> = None;

    for cmsg in msg.cmsgs()? {
        if let ControlMessageOwned::ScmRights(fds) = cmsg {
            if fds.is_empty() {
                continue;
            }
            // Take the first fd, close any extras
            // SAFETY: The fd was received via SCM_RIGHTS and is valid
            received_fd = Some(unsafe { OwnedFd::from_raw_fd(fds[0]) });

            // Close extra fds if any
            for &extra_fd in &fds[1..] {
                tracing::warn!(extra_fd, "Closing unexpected extra fd");
                // SAFETY: These are valid fds from SCM_RIGHTS
                unsafe {
                    let _ = OwnedFd::from_raw_fd(extra_fd);
                    // OwnedFd will close on drop
                }
            }
        }
    }

    let client_fd = received_fd.ok_or(HandoffError::NoFileDescriptor)?;

    // Validate socket type
    validate_socket(&client_fd)?;

    // Parse handoff data
    let bytes_received = msg.bytes;
    let data = if bytes_received > 0 {
        parse_handoff_data(&data_buf[..bytes_received])?
    } else {
        HandoffData::default()
    };

    Ok(HandoffResult {
        client_fd,
        data,
        raw_data_len: bytes_received,
    })
}

/// Validate that the received fd is a stream socket.
fn validate_socket(fd: &OwnedFd) -> Result<(), HandoffError> {
    let sock_type = getsockopt(fd, SockType).map_err(|_| HandoffError::InvalidSocketType)?;

    // SockType returns a nix::sys::socket::SockType enum
    if sock_type != nix::sys::socket::SockType::Stream {
        return Err(HandoffError::InvalidSocketType);
    }

    Ok(())
}

/// Parse JSON handoff data.
fn parse_handoff_data(data: &[u8]) -> Result<HandoffData, HandoffError> {
    // Try to parse as JSON, fall back to empty data on error
    match serde_json::from_slice(data) {
        Ok(handoff) => Ok(handoff),
        Err(e) => {
            // Log but don't fail - handoff data is optional
            tracing::warn!(
                error = %e,
                data_len = data.len(),
                "Failed to parse handoff data as JSON, using defaults"
            );
            Ok(HandoffData::default())
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_parse_handoff_data() {
        let json = r#"{"user_id": 123, "prompt": "Hello world"}"#;
        let data = parse_handoff_data(json.as_bytes()).unwrap();

        assert_eq!(data.user_id, Some(123));
        assert_eq!(data.prompt, Some("Hello world".to_string()));
        assert_eq!(data.model, None);
    }

    #[test]
    fn test_parse_empty_data() {
        let data = parse_handoff_data(&[]).unwrap();
        assert_eq!(data.user_id, None);
        assert_eq!(data.prompt, None);
    }

    #[test]
    fn test_parse_invalid_json() {
        let data = parse_handoff_data(b"not json").unwrap();
        // Should return defaults, not error
        assert_eq!(data.user_id, None);
    }
}
