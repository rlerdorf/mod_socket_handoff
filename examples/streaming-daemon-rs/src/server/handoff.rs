//! SCM_RIGHTS file descriptor receiving.
//!
//! Receives client socket fd from Apache via Unix domain socket.

use nix::cmsg_space;
use nix::fcntl::{fcntl, FcntlArg, OFlag};
use nix::sys::socket::{
    getsockopt, recvmsg, setsockopt, sockopt::ReceiveTimeout, sockopt::SockType,
    ControlMessageOwned, MsgFlags, SockaddrStorage,
};
use serde::Deserialize;
use std::io::IoSliceMut;
use std::os::unix::io::{AsRawFd, FromRawFd, OwnedFd, RawFd};
use std::time::Duration;
use tokio::io::Interest;
use tokio::net::UnixStream;

use crate::error::HandoffError;

/// Data passed from PHP via X-Handoff-Data header.
/// Uses `Box<str>` instead of `String` for immutable string fields
/// to save 8 bytes per field (no capacity needed since we never grow them).
#[derive(Debug, Clone, Default, Deserialize)]
pub struct HandoffData {
    #[serde(default)]
    pub user_id: Option<i64>,
    #[serde(default)]
    pub prompt: Option<Box<str>>,
    #[serde(default)]
    pub model: Option<Box<str>>,
    #[serde(default)]
    pub max_tokens: Option<u32>,
    #[serde(default)]
    pub temperature: Option<f32>,
    #[serde(default)]
    pub system: Option<Box<str>>,
    #[serde(default)]
    pub request_id: Option<Box<str>>,
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
/// This function takes ownership of the UnixStream because:
/// 1. We need to clear O_NONBLOCK for SO_RCVTIMEO to work
/// 2. dup() shares the file description, so clearing flags would affect tokio
/// 3. Taking ownership lets us safely convert to a blocking fd
///
/// This function:
/// 1. Waits for the Unix socket to be readable
/// 2. Uses recvmsg to receive both the data and the control message
/// 3. Extracts the file descriptor from SCM_RIGHTS
/// 4. Validates the socket type
/// 5. Parses the JSON handoff data
pub async fn receive_handoff(
    stream: UnixStream,
    timeout: Duration,
    buffer_size: usize,
) -> Result<HandoffResult, HandoffError> {
    // Wait for readable with timeout. Without this, a peer that connects but
    // never sends data would hang forever, consuming capacity and blocking
    // shutdown drain.
    let ready = tokio::time::timeout(timeout, stream.ready(Interest::READABLE))
        .await
        .map_err(|_| HandoffError::Timeout)?
        .map_err(|e| HandoffError::ReceiveFailed(e.to_string()))?;

    if !ready.is_readable() {
        return Err(HandoffError::ReceiveFailed(
            "Socket not readable".to_string(),
        ));
    }

    // Convert tokio UnixStream to OwnedFd by going through std UnixStream.
    // This transfers ownership cleanly without sharing file descriptions.
    let std_stream = stream.into_std().map_err(|e| {
        HandoffError::ReceiveFailed(format!("Failed to convert to std stream: {}", e))
    })?;
    let owned_fd = OwnedFd::from(std_stream);

    // Use block_in_place to run blocking recvmsg without spawn_blocking overhead.
    // This temporarily converts this worker into a blocking thread, avoiding the
    // scheduling latency of spawn_blocking while still allowing tokio to spawn
    // replacement workers if needed. SO_RCVTIMEO bounds the blocking duration.
    tokio::task::block_in_place(|| receive_fd_blocking(owned_fd, timeout, buffer_size))
}

/// Blocking recvmsg implementation.
fn receive_fd_blocking(
    owned_fd: OwnedFd,
    timeout: Duration,
    buffer_size: usize,
) -> Result<HandoffResult, HandoffError> {
    // Clear O_NONBLOCK on the duplicated fd so SO_RCVTIMEO works correctly.
    // The original tokio UnixStream has O_NONBLOCK set, which we inherit via dup().
    let fd = owned_fd.as_raw_fd();
    let flags = fcntl(fd, FcntlArg::F_GETFL)
        .map_err(|e| HandoffError::ReceiveFailed(format!("Failed to get fd flags: {}", e)))?;
    let new_flags = OFlag::from_bits_truncate(flags) & !OFlag::O_NONBLOCK;
    fcntl(fd, FcntlArg::F_SETFL(new_flags))
        .map_err(|e| HandoffError::ReceiveFailed(format!("Failed to clear O_NONBLOCK: {}", e)))?;

    // Set SO_RCVTIMEO so recvmsg returns even if peer is slow/malicious.
    // This ensures the blocking thread is released and not leaked.
    let timeval =
        nix::sys::time::TimeVal::new(timeout.as_secs() as i64, timeout.subsec_micros() as i64);
    setsockopt(&owned_fd, ReceiveTimeout, &timeval)
        .map_err(|e| HandoffError::ReceiveFailed(format!("Failed to set SO_RCVTIMEO: {}", e)))?;

    let mut data_buf = vec![0u8; buffer_size];
    let mut cmsg_buf = cmsg_space!([RawFd; 1]);

    let mut iov = [IoSliceMut::new(&mut data_buf)];

    // Use MSG_CMSG_CLOEXEC to set FD_CLOEXEC on received fds, preventing
    // them from leaking into any future exec calls.
    let fd = owned_fd.as_raw_fd();
    let msg = recvmsg::<SockaddrStorage>(
        fd,
        &mut iov,
        Some(&mut cmsg_buf),
        MsgFlags::MSG_CMSG_CLOEXEC,
    )
    .map_err(|e| {
        // Translate SO_RCVTIMEO errors (EAGAIN/EWOULDBLOCK) to Timeout
        use nix::errno::Errno;
        if e == Errno::EAGAIN || e == Errno::EWOULDBLOCK {
            return HandoffError::Timeout;
        }
        HandoffError::System(e)
    })?;

    // Extract file descriptor from control message FIRST, before checking truncation.
    // This ensures any received fds are properly closed even on error paths.
    // Note: cmsg_space!([RawFd; 1]) only allocates room for one fd.
    let mut received_fd: Option<OwnedFd> = None;

    if let Ok(cmsgs) = msg.cmsgs() {
        for cmsg in cmsgs {
            if let ControlMessageOwned::ScmRights(fds) = cmsg {
                if fds.is_empty() {
                    continue;
                }
                // Take the fd (only one can fit in our cmsg buffer)
                // SAFETY: The fd was received via SCM_RIGHTS and is valid
                received_fd = Some(unsafe { OwnedFd::from_raw_fd(fds[0]) });
            }
        }
    }

    // Now check for truncation - any received fds are safely wrapped in OwnedFd
    // and will be closed when dropped on error return
    if msg.flags.contains(MsgFlags::MSG_TRUNC) {
        return Err(HandoffError::DataTruncated);
    }
    if msg.flags.contains(MsgFlags::MSG_CTRUNC) {
        return Err(HandoffError::ControlMessageTruncated);
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
        assert_eq!(data.prompt.as_deref(), Some("Hello world"));
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
