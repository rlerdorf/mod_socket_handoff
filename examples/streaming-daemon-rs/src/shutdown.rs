//! Graceful shutdown coordination.
//!
//! Uses a watch channel to broadcast shutdown signal and track active connections.

use parking_lot::Mutex;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use tokio::sync::watch;

/// Shutdown coordinator for graceful termination.
#[derive(Clone)]
pub struct ShutdownCoordinator {
    inner: Arc<ShutdownInner>,
}

struct ShutdownInner {
    /// Broadcast channel for shutdown signal.
    shutdown_tx: watch::Sender<bool>,
    /// Receiver for shutdown signal (cloned for each connection).
    shutdown_rx: watch::Receiver<bool>,
    /// Active connection count.
    active_connections: AtomicU64,
    /// Waiters for connection drain.
    drain_notify: tokio::sync::Notify,
    /// Connection tracking for debugging.
    connection_ids: Mutex<Vec<u64>>,
    /// Next connection ID.
    next_conn_id: AtomicU64,
}

impl ShutdownCoordinator {
    /// Create a new shutdown coordinator.
    pub fn new() -> Self {
        let (shutdown_tx, shutdown_rx) = watch::channel(false);
        Self {
            inner: Arc::new(ShutdownInner {
                shutdown_tx,
                shutdown_rx,
                active_connections: AtomicU64::new(0),
                drain_notify: tokio::sync::Notify::new(),
                connection_ids: Mutex::new(Vec::new()),
                next_conn_id: AtomicU64::new(1),
            }),
        }
    }

    /// Signal shutdown to all connections.
    pub fn shutdown(&self) {
        let _ = self.inner.shutdown_tx.send(true);
    }

    /// Check if shutdown has been signaled.
    pub fn is_shutdown(&self) -> bool {
        *self.inner.shutdown_rx.borrow()
    }

    /// Get a receiver to watch for shutdown.
    pub fn subscribe(&self) -> watch::Receiver<bool> {
        self.inner.shutdown_rx.clone()
    }

    /// Get current active connection count.
    pub fn active_connections(&self) -> u64 {
        self.inner.active_connections.load(Ordering::Relaxed)
    }

    /// Register a new connection and return a guard that decrements on drop.
    pub fn register_connection(&self) -> ConnectionGuard {
        let id = self.inner.next_conn_id.fetch_add(1, Ordering::Relaxed);
        self.inner.active_connections.fetch_add(1, Ordering::Relaxed);
        self.inner.connection_ids.lock().push(id);

        ConnectionGuard {
            coordinator: self.clone(),
            id,
        }
    }

    /// Wait for all connections to drain.
    pub async fn wait_for_drain(&self) {
        loop {
            let count = self.inner.active_connections.load(Ordering::Relaxed);
            if count == 0 {
                return;
            }
            self.inner.drain_notify.notified().await;
        }
    }

    fn unregister_connection(&self, id: u64) {
        {
            let mut ids = self.inner.connection_ids.lock();
            if let Some(pos) = ids.iter().position(|&x| x == id) {
                ids.swap_remove(pos);
            }
        }

        let prev = self.inner.active_connections.fetch_sub(1, Ordering::Relaxed);
        if prev == 1 {
            self.inner.drain_notify.notify_waiters();
        }
    }
}

impl Default for ShutdownCoordinator {
    fn default() -> Self {
        Self::new()
    }
}

/// RAII guard for connection lifecycle tracking.
pub struct ConnectionGuard {
    coordinator: ShutdownCoordinator,
    id: u64,
}

impl ConnectionGuard {
    /// Get the connection ID.
    pub fn id(&self) -> u64 {
        self.id
    }

    /// Check if shutdown has been signaled.
    pub fn is_shutdown(&self) -> bool {
        self.coordinator.is_shutdown()
    }

    /// Get a receiver to watch for shutdown.
    pub fn subscribe(&self) -> watch::Receiver<bool> {
        self.coordinator.subscribe()
    }
}

impl Drop for ConnectionGuard {
    fn drop(&mut self) {
        self.coordinator.unregister_connection(self.id);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn test_connection_tracking() {
        let coordinator = ShutdownCoordinator::new();

        assert_eq!(coordinator.active_connections(), 0);

        let guard1 = coordinator.register_connection();
        assert_eq!(coordinator.active_connections(), 1);

        let guard2 = coordinator.register_connection();
        assert_eq!(coordinator.active_connections(), 2);

        drop(guard1);
        assert_eq!(coordinator.active_connections(), 1);

        drop(guard2);
        assert_eq!(coordinator.active_connections(), 0);
    }

    #[tokio::test]
    async fn test_shutdown_signal() {
        let coordinator = ShutdownCoordinator::new();

        assert!(!coordinator.is_shutdown());

        coordinator.shutdown();
        assert!(coordinator.is_shutdown());
    }
}
