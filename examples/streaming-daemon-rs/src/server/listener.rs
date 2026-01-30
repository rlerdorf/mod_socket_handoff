//! Unix socket listener for receiving connections from Apache.

use std::os::unix::fs::{FileTypeExt, PermissionsExt};
use std::path::{Path, PathBuf};
use std::sync::Arc;
use tokio::net::{UnixListener, UnixStream};
use tokio::sync::Semaphore;

use crate::config::ServerConfig;
use crate::error::{DaemonError, Result};
use crate::metrics;
use crate::shutdown::ShutdownCoordinator;

/// Unix socket listener that accepts connections from Apache.
pub struct UnixSocketListener {
    listener: UnixListener,
    socket_path: PathBuf,
    connection_semaphore: Arc<Semaphore>,
    shutdown: ShutdownCoordinator,
    config: ServerConfig,
}

impl UnixSocketListener {
    /// Create and bind a new Unix socket listener.
    pub async fn bind(config: &ServerConfig, shutdown: ShutdownCoordinator) -> Result<Self> {
        let socket_path = PathBuf::from(&config.socket_path);

        // Remove stale socket if it exists (verify it's actually a socket first)
        if socket_path.exists() {
            let metadata = std::fs::symlink_metadata(&socket_path).map_err(|e| {
                DaemonError::Socket(format!("Failed to stat {}: {}", socket_path.display(), e))
            })?;

            if metadata.file_type().is_socket() {
                std::fs::remove_file(&socket_path).map_err(|e| {
                    DaemonError::Socket(format!(
                        "Failed to remove stale socket {}: {}",
                        socket_path.display(),
                        e
                    ))
                })?;
            } else {
                return Err(DaemonError::Socket(format!(
                    "Path {} exists but is not a socket",
                    socket_path.display()
                )));
            }
        }

        // Create parent directory if needed
        if let Some(parent) = socket_path.parent() {
            if !parent.exists() {
                std::fs::create_dir_all(parent).map_err(|e| {
                    DaemonError::Socket(format!(
                        "Failed to create socket directory {}: {}",
                        parent.display(),
                        e
                    ))
                })?;
            }
        }

        // Bind the listener
        let listener = UnixListener::bind(&socket_path).map_err(|e| {
            DaemonError::Socket(format!(
                "Failed to bind socket {}: {}",
                socket_path.display(),
                e
            ))
        })?;

        // Set permissions; if this fails, clean up the bound socket before returning
        if let Err(e) = set_socket_permissions(&socket_path, config.socket_mode) {
            // Ensure the listener is closed before attempting to remove the socket file.
            drop(listener);
            if let Err(remove_err) = std::fs::remove_file(&socket_path) {
                tracing::warn!(
                    error = %remove_err,
                    socket_path = %socket_path.display(),
                    "Failed to remove socket after permission-setting error"
                );
            }
            return Err(e);
        }

        // Create connection semaphore
        let connection_semaphore = Arc::new(Semaphore::new(config.max_connections));

        tracing::info!(
            socket_path = %socket_path.display(),
            max_connections = config.max_connections,
            "Unix socket listener bound"
        );

        Ok(Self {
            listener,
            socket_path,
            connection_semaphore,
            shutdown,
            config: config.clone(),
        })
    }

    /// Accept a new connection from Apache.
    ///
    /// Returns None if shutdown has been signaled (or the listener/semaphore
    /// is no longer available). Transient accept errors are logged and
    /// retried with a brief backoff.
    ///
    /// Uses accept-first-then-gate pattern: accepts the connection immediately
    /// to free Apache, then checks capacity. If at capacity, closes the
    /// connection and records a rejection metric.
    pub async fn accept(&self) -> Option<AcceptedConnection> {
        loop {
            // Check shutdown before accepting
            if self.shutdown.is_shutdown() {
                return None;
            }

            // Accept connection first (to free Apache worker immediately)
            let mut shutdown_rx = self.shutdown.subscribe();
            let accept_result = tokio::select! {
                biased;
                _ = shutdown_rx.changed() => {
                    return None;
                }
                result = self.listener.accept() => result
            };

            let stream = match accept_result {
                Ok((stream, _addr)) => stream,
                Err(e) => {
                    tracing::error!(error = %e, "Accept error");
                    // Brief backoff on error
                    tokio::time::sleep(std::time::Duration::from_millis(100)).await;
                    continue;
                }
            };

            // Try to acquire semaphore permit (non-blocking)
            let permit = match self.connection_semaphore.clone().try_acquire_owned() {
                Ok(permit) => permit,
                Err(_) => {
                    // At capacity - reject this connection promptly
                    metrics::record_connection_rejected();
                    // Use debug level to avoid log flooding under sustained overload
                    tracing::debug!("Connection rejected: at capacity");
                    // Dropping stream closes the connection
                    drop(stream);
                    continue;
                }
            };

            // Connection accepted and we have capacity
            metrics::record_connection_accepted();
            // register_connection updates the active connections gauge atomically
            let guard = self.shutdown.register_connection();

            return Some(AcceptedConnection {
                stream,
                _permit: permit,
                guard,
                config: self.config.clone(),
            });
        }
    }

    /// Get the socket path.
    pub fn socket_path(&self) -> &Path {
        &self.socket_path
    }

    /// Get current active connection count.
    pub fn active_connections(&self) -> u64 {
        self.shutdown.active_connections()
    }

    /// Get available permits (remaining capacity).
    pub fn available_permits(&self) -> usize {
        self.connection_semaphore.available_permits()
    }
}

impl Drop for UnixSocketListener {
    fn drop(&mut self) {
        // Clean up socket file safely: re-validate that it's still a Unix socket
        // to prevent removing an unexpected file if an attacker replaced it.
        let metadata = match std::fs::symlink_metadata(&self.socket_path) {
            Ok(metadata) => metadata,
            Err(e) => {
                // If the file is already gone, nothing to do; otherwise log and return.
                if e.kind() != std::io::ErrorKind::NotFound {
                    tracing::warn!(
                        error = %e,
                        path = %self.socket_path.display(),
                        "Failed to stat socket file during cleanup"
                    );
                }
                return;
            }
        };

        if !metadata.file_type().is_socket() {
            tracing::warn!(
                path = %self.socket_path.display(),
                "Refusing to remove non-socket path during cleanup"
            );
            return;
        }

        if let Err(e) = std::fs::remove_file(&self.socket_path) {
            tracing::warn!(
                error = %e,
                path = %self.socket_path.display(),
                "Failed to remove socket file"
            );
        }
    }
}

/// An accepted connection with its associated permit and guard.
pub struct AcceptedConnection {
    pub stream: UnixStream,
    /// Semaphore permit - released when dropped.
    _permit: tokio::sync::OwnedSemaphorePermit,
    /// Connection guard - decrements active count when dropped.
    pub guard: crate::shutdown::ConnectionGuard,
    /// Server config for this connection.
    pub config: ServerConfig,
}

/// Set socket file permissions.
fn set_socket_permissions(path: &Path, mode: u32) -> Result<()> {
    let permissions = std::fs::Permissions::from_mode(mode);
    std::fs::set_permissions(path, permissions).map_err(|e| {
        DaemonError::Socket(format!(
            "Failed to set socket permissions on {}: {}",
            path.display(),
            e
        ))
    })?;
    Ok(())
}
