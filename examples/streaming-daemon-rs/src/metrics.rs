//! Prometheus metrics for the streaming daemon.

use metrics::{counter, describe_counter, describe_gauge, describe_histogram, gauge, histogram};
use metrics_exporter_prometheus::PrometheusBuilder;
use std::net::SocketAddr;
use std::time::Instant;

/// Initialize metrics descriptions.
pub fn init_metrics() {
    // Connection metrics
    describe_gauge!(
        "daemon_active_connections",
        "Number of currently active connections"
    );
    describe_counter!(
        "daemon_connections_total",
        "Total number of connections accepted"
    );
    describe_counter!(
        "daemon_connections_rejected",
        "Connections rejected due to capacity"
    );

    // Handoff metrics
    describe_counter!("daemon_handoffs_total", "Total handoffs received");
    describe_counter!("daemon_handoff_errors", "Handoff receive errors");
    describe_histogram!(
        "daemon_handoff_duration_seconds",
        "Time to receive handoff from Apache"
    );

    // Backend metrics
    describe_counter!("daemon_backend_requests", "Backend API requests");
    describe_counter!("daemon_backend_errors", "Backend API errors");
    describe_histogram!(
        "daemon_backend_duration_seconds",
        "Backend request duration"
    );
    describe_histogram!(
        "daemon_backend_ttfb_seconds",
        "Time to first byte from backend"
    );

    // Streaming metrics
    describe_counter!("daemon_bytes_sent", "Total bytes sent to clients");
    describe_counter!("daemon_chunks_sent", "Total SSE chunks sent");
    describe_counter!("daemon_stream_errors", "Stream write errors");
    describe_histogram!(
        "daemon_stream_duration_seconds",
        "Total stream duration per request"
    );
}

/// Start the Prometheus metrics HTTP server.
pub async fn start_metrics_server(addr: SocketAddr) -> anyhow::Result<()> {
    let builder = PrometheusBuilder::new();
    builder
        .with_http_listener(addr)
        .install()
        .map_err(|e| anyhow::anyhow!("Failed to start metrics server: {}", e))?;

    tracing::info!(%addr, "Metrics server started");
    Ok(())
}

/// Record connection accepted.
pub fn record_connection_accepted() {
    counter!("daemon_connections_total").increment(1);
}

/// Record connection rejected due to capacity.
pub fn record_connection_rejected() {
    counter!("daemon_connections_rejected").increment(1);
}

/// Update active connection gauge.
pub fn set_active_connections(count: u64) {
    gauge!("daemon_active_connections").set(count as f64);
}

/// Record successful handoff.
pub fn record_handoff_success(duration: std::time::Duration) {
    counter!("daemon_handoffs_total").increment(1);
    histogram!("daemon_handoff_duration_seconds").record(duration.as_secs_f64());
}

/// Record handoff error.
pub fn record_handoff_error() {
    counter!("daemon_handoff_errors").increment(1);
}

/// Record backend request.
pub fn record_backend_request(backend: &'static str) {
    counter!("daemon_backend_requests", "backend" => backend).increment(1);
}

/// Record backend error.
pub fn record_backend_error(backend: &'static str) {
    counter!("daemon_backend_errors", "backend" => backend).increment(1);
}

/// Record backend request duration.
pub fn record_backend_duration(backend: &'static str, duration: std::time::Duration) {
    histogram!("daemon_backend_duration_seconds", "backend" => backend)
        .record(duration.as_secs_f64());
}

/// Record time to first byte from backend.
pub fn record_backend_ttfb(backend: &'static str, duration: std::time::Duration) {
    histogram!("daemon_backend_ttfb_seconds", "backend" => backend)
        .record(duration.as_secs_f64());
}

/// Record bytes sent to client.
pub fn record_bytes_sent(bytes: u64) {
    counter!("daemon_bytes_sent").increment(bytes);
}

/// Record SSE chunk sent.
pub fn record_chunk_sent() {
    counter!("daemon_chunks_sent").increment(1);
}

/// Record stream error.
pub fn record_stream_error() {
    counter!("daemon_stream_errors").increment(1);
}

/// Record total stream duration.
pub fn record_stream_duration(duration: std::time::Duration) {
    histogram!("daemon_stream_duration_seconds").record(duration.as_secs_f64());
}

/// Timer for measuring durations.
pub struct Timer {
    start: Instant,
}

impl Timer {
    pub fn new() -> Self {
        Self {
            start: Instant::now(),
        }
    }

    pub fn elapsed(&self) -> std::time::Duration {
        self.start.elapsed()
    }
}

impl Default for Timer {
    fn default() -> Self {
        Self::new()
    }
}
