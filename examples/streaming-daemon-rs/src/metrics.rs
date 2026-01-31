//! Prometheus metrics for the streaming daemon.

use metrics::{counter, describe_counter, describe_gauge, describe_histogram, gauge, histogram};
use metrics_exporter_prometheus::PrometheusBuilder;
use std::net::SocketAddr;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Instant;

/// Global benchmark mode flag. When true, Prometheus updates are skipped.
static BENCHMARK_MODE: AtomicBool = AtomicBool::new(false);

/// Benchmark statistics (only updated when BENCHMARK_MODE is true).
pub struct BenchmarkStats {
    peak_streams: std::sync::atomic::AtomicI64,
    active_streams: std::sync::atomic::AtomicI64,
    total_started: std::sync::atomic::AtomicU64,
    total_completed: std::sync::atomic::AtomicU64,
    total_failed: std::sync::atomic::AtomicU64,
    total_bytes: std::sync::atomic::AtomicU64,
}

impl BenchmarkStats {
    const fn new() -> Self {
        use std::sync::atomic::{AtomicI64, AtomicU64};
        Self {
            peak_streams: AtomicI64::new(0),
            active_streams: AtomicI64::new(0),
            total_started: AtomicU64::new(0),
            total_completed: AtomicU64::new(0),
            total_failed: AtomicU64::new(0),
            total_bytes: AtomicU64::new(0),
        }
    }
}

static BENCH_STATS: BenchmarkStats = BenchmarkStats::new();

/// Set benchmark mode.
pub fn set_benchmark_mode(enabled: bool) {
    BENCHMARK_MODE.store(enabled, Ordering::SeqCst);
}

/// Check if benchmark mode is enabled.
#[inline]
pub fn is_benchmark_mode() -> bool {
    BENCHMARK_MODE.load(Ordering::Relaxed)
}

/// Record stream start for benchmarking.
pub fn bench_stream_start() {
    if !is_benchmark_mode() {
        return;
    }
    BENCH_STATS.total_started.fetch_add(1, Ordering::Relaxed);
    let current = BENCH_STATS.active_streams.fetch_add(1, Ordering::Relaxed) + 1;
    // Update peak if needed
    loop {
        let peak = BENCH_STATS.peak_streams.load(Ordering::Relaxed);
        if current <= peak {
            break;
        }
        if BENCH_STATS
            .peak_streams
            .compare_exchange_weak(peak, current, Ordering::Relaxed, Ordering::Relaxed)
            .is_ok()
        {
            break;
        }
    }
}

/// Record stream end for benchmarking.
pub fn bench_stream_end(success: bool, bytes: u64) {
    if !is_benchmark_mode() {
        return;
    }
    BENCH_STATS.active_streams.fetch_sub(1, Ordering::Relaxed);
    if success {
        BENCH_STATS.total_completed.fetch_add(1, Ordering::Relaxed);
    } else {
        BENCH_STATS.total_failed.fetch_add(1, Ordering::Relaxed);
    }
    BENCH_STATS.total_bytes.fetch_add(bytes, Ordering::Relaxed);
}

/// Print benchmark summary.
pub fn print_benchmark_summary() {
    if !is_benchmark_mode() {
        return;
    }
    eprintln!("\n=== Benchmark Summary ===");
    eprintln!(
        "Peak concurrent streams: {}",
        BENCH_STATS.peak_streams.load(Ordering::Relaxed)
    );
    eprintln!(
        "Total started: {}",
        BENCH_STATS.total_started.load(Ordering::Relaxed)
    );
    eprintln!(
        "Total completed: {}",
        BENCH_STATS.total_completed.load(Ordering::Relaxed)
    );
    eprintln!(
        "Total failed: {}",
        BENCH_STATS.total_failed.load(Ordering::Relaxed)
    );
    eprintln!(
        "Total bytes sent: {}",
        BENCH_STATS.total_bytes.load(Ordering::Relaxed)
    );
    eprintln!("=========================");
}

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
    if is_benchmark_mode() {
        return;
    }
    counter!("daemon_connections_total").increment(1);
}

/// Record connection rejected due to capacity.
pub fn record_connection_rejected() {
    if is_benchmark_mode() {
        return;
    }
    counter!("daemon_connections_rejected").increment(1);
}

/// Update active connection gauge.
pub fn set_active_connections(count: u64) {
    if is_benchmark_mode() {
        return;
    }
    gauge!("daemon_active_connections").set(count as f64);
}

/// Record successful handoff.
pub fn record_handoff_success(duration: std::time::Duration) {
    if is_benchmark_mode() {
        return;
    }
    counter!("daemon_handoffs_total").increment(1);
    histogram!("daemon_handoff_duration_seconds").record(duration.as_secs_f64());
}

/// Record handoff error.
pub fn record_handoff_error() {
    if is_benchmark_mode() {
        return;
    }
    counter!("daemon_handoff_errors").increment(1);
}

/// Record backend request.
pub fn record_backend_request(backend: &'static str) {
    if is_benchmark_mode() {
        return;
    }
    counter!("daemon_backend_requests", "backend" => backend).increment(1);
}

/// Record backend error.
pub fn record_backend_error(backend: &'static str) {
    if is_benchmark_mode() {
        return;
    }
    counter!("daemon_backend_errors", "backend" => backend).increment(1);
}

/// Record backend request duration.
pub fn record_backend_duration(backend: &'static str, duration: std::time::Duration) {
    if is_benchmark_mode() {
        return;
    }
    histogram!("daemon_backend_duration_seconds", "backend" => backend)
        .record(duration.as_secs_f64());
}

/// Record time to first byte from backend.
pub fn record_backend_ttfb(backend: &'static str, duration: std::time::Duration) {
    if is_benchmark_mode() {
        return;
    }
    histogram!("daemon_backend_ttfb_seconds", "backend" => backend).record(duration.as_secs_f64());
}

/// Record bytes sent to client.
pub fn record_bytes_sent(bytes: u64) {
    if is_benchmark_mode() {
        return;
    }
    counter!("daemon_bytes_sent").increment(bytes);
}

/// Record SSE chunk sent.
pub fn record_chunk_sent() {
    if is_benchmark_mode() {
        return;
    }
    counter!("daemon_chunks_sent").increment(1);
}

/// Record stream error.
pub fn record_stream_error() {
    if is_benchmark_mode() {
        return;
    }
    counter!("daemon_stream_errors").increment(1);
}

/// Record total stream duration.
pub fn record_stream_duration(duration: std::time::Duration) {
    if is_benchmark_mode() {
        return;
    }
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
