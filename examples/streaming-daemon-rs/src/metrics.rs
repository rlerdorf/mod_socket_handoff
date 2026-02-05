//! Prometheus metrics for the streaming daemon.

use metrics::{counter, describe_counter, describe_gauge, describe_histogram, gauge, histogram};
use metrics_exporter_prometheus::PrometheusBuilder;
use nix::libc;
use std::io;
use std::net::SocketAddr;
use std::sync::atomic::{AtomicBool, AtomicI64, Ordering};
use std::time::Instant;

/// Error reason labels for metrics (matches Go daemon classification).
#[derive(Debug, Clone, Copy)]
pub enum ErrorReason {
    /// Client disconnected (EPIPE, ECONNRESET, BrokenPipe, ConnectionReset)
    ClientDisconnected,
    /// Timeout (deadline exceeded, ETIMEDOUT)
    Timeout,
    /// Operation was canceled
    Canceled,
    /// Network error (other network-related errors)
    Network,
    /// Other/unknown error
    Other,
}

impl ErrorReason {
    /// Convert to static string for metrics label.
    pub fn as_str(&self) -> &'static str {
        match self {
            ErrorReason::ClientDisconnected => "client_disconnected",
            ErrorReason::Timeout => "timeout",
            ErrorReason::Canceled => "canceled",
            ErrorReason::Network => "network",
            ErrorReason::Other => "other",
        }
    }

    /// Classify an I/O error into an ErrorReason.
    pub fn from_io_error(err: &io::Error) -> Self {
        match err.kind() {
            io::ErrorKind::BrokenPipe | io::ErrorKind::ConnectionReset => {
                ErrorReason::ClientDisconnected
            }
            io::ErrorKind::TimedOut => ErrorReason::Timeout,
            io::ErrorKind::WouldBlock => ErrorReason::Other, // WouldBlock handled by tokio, not a timeout
            io::ErrorKind::Interrupted => ErrorReason::Canceled,
            io::ErrorKind::ConnectionRefused
            | io::ErrorKind::ConnectionAborted
            | io::ErrorKind::NotConnected
            | io::ErrorKind::AddrInUse
            | io::ErrorKind::AddrNotAvailable => ErrorReason::Network,
            _ => {
                // Check raw OS error for more specific classification
                if let Some(errno) = err.raw_os_error() {
                    match errno {
                        libc::EPIPE | libc::ECONNRESET => ErrorReason::ClientDisconnected,
                        libc::ETIMEDOUT => ErrorReason::Timeout,
                        libc::ECANCELED => ErrorReason::Canceled,
                        _ => ErrorReason::Other,
                    }
                } else {
                    ErrorReason::Other
                }
            }
        }
    }
}

/// Global peak streams tracker (atomic for thread safety).
static PEAK_STREAMS: AtomicI64 = AtomicI64::new(0);
/// Global active streams tracker.
static ACTIVE_STREAMS: AtomicI64 = AtomicI64::new(0);

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
        "daemon_connections_rejected_total",
        "Connections rejected due to capacity"
    );

    // Handoff metrics
    describe_counter!("daemon_handoffs_total", "Total handoffs received");
    describe_counter!("daemon_handoff_errors_total", "Handoff receive errors");
    describe_histogram!(
        "daemon_handoff_duration_seconds",
        "Time to receive handoff from Apache"
    );

    // Backend metrics
    describe_counter!("daemon_backend_requests_total", "Backend API requests");
    describe_counter!("daemon_backend_errors_total", "Backend API errors");
    describe_histogram!(
        "daemon_backend_duration_seconds",
        "Backend request duration"
    );
    describe_histogram!(
        "daemon_backend_ttfb_seconds",
        "Time to first byte from backend"
    );

    // Streaming metrics
    describe_gauge!(
        "daemon_active_streams",
        "Number of currently active streaming connections"
    );
    describe_gauge!(
        "daemon_peak_streams",
        "Peak number of concurrent streaming connections seen"
    );
    describe_counter!("daemon_bytes_sent_total", "Total bytes sent to clients");
    describe_counter!("daemon_chunks_sent_total", "Total SSE chunks sent");
    describe_counter!("daemon_stream_errors_total", "Stream write errors");
    describe_histogram!(
        "daemon_stream_duration_seconds",
        "Total stream duration per request"
    );
}

/// Start the Prometheus metrics HTTP server.
pub async fn start_metrics_server(addr: SocketAddr) -> anyhow::Result<()> {
    // Define histogram buckets matching the Go daemon for consistency.
    // Handoff duration: 0.1ms to ~1.6s (exponential buckets base 0.0001, factor 2, count 15)
    let handoff_buckets: [f64; 15] = [
        0.0001, 0.0002, 0.0004, 0.0008, 0.0016, 0.0032, 0.0064, 0.0128, 0.0256, 0.0512, 0.1024,
        0.2048, 0.4096, 0.8192, 1.6384,
    ];

    // Stream/backend duration: 10ms to ~163s (exponential buckets base 0.01, factor 2, count 15)
    let duration_buckets: [f64; 15] = [
        0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12, 10.24, 20.48, 40.96, 81.92,
        163.84,
    ];

    // Backend TTFB: 1ms to ~16s (exponential buckets base 0.001, factor 2, count 15)
    let ttfb_buckets: [f64; 15] = [
        0.001, 0.002, 0.004, 0.008, 0.016, 0.032, 0.064, 0.128, 0.256, 0.512, 1.024, 2.048, 4.096,
        8.192, 16.384,
    ];

    let builder = PrometheusBuilder::new()
        .set_buckets_for_metric(
            metrics_exporter_prometheus::Matcher::Full("daemon_handoff_duration_seconds".to_string()),
            &handoff_buckets,
        )
        .expect("valid handoff buckets")
        .set_buckets_for_metric(
            metrics_exporter_prometheus::Matcher::Full("daemon_stream_duration_seconds".to_string()),
            &duration_buckets,
        )
        .expect("valid stream buckets")
        .set_buckets_for_metric(
            metrics_exporter_prometheus::Matcher::Full("daemon_backend_duration_seconds".to_string()),
            &duration_buckets,
        )
        .expect("valid backend duration buckets")
        .set_buckets_for_metric(
            metrics_exporter_prometheus::Matcher::Full("daemon_backend_ttfb_seconds".to_string()),
            &ttfb_buckets,
        )
        .expect("valid ttfb buckets");

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
    counter!("daemon_connections_rejected_total").increment(1);
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

/// Record handoff error with reason label.
pub fn record_handoff_error(reason: ErrorReason) {
    if is_benchmark_mode() {
        return;
    }
    counter!("daemon_handoff_errors_total", "reason" => reason.as_str()).increment(1);
}

/// Record backend request.
pub fn record_backend_request(backend: &'static str) {
    if is_benchmark_mode() {
        return;
    }
    counter!("daemon_backend_requests_total", "backend" => backend).increment(1);
}

/// Record backend error.
pub fn record_backend_error(backend: &'static str) {
    if is_benchmark_mode() {
        return;
    }
    counter!("daemon_backend_errors_total", "backend" => backend).increment(1);
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
    counter!("daemon_bytes_sent_total").increment(bytes);
}

/// Record SSE chunk sent.
pub fn record_chunk_sent() {
    if is_benchmark_mode() {
        return;
    }
    counter!("daemon_chunks_sent_total").increment(1);
}

/// Record stream error with reason label.
pub fn record_stream_error(reason: ErrorReason) {
    if is_benchmark_mode() {
        return;
    }
    counter!("daemon_stream_errors_total", "reason" => reason.as_str()).increment(1);
}

/// Record stream start - increments active streams and updates peak if necessary.
/// Note: Caller must check is_benchmark_mode() and call bench_stream_start() directly if true.
pub fn record_stream_start() {
    if is_benchmark_mode() {
        return;
    }
    let current = ACTIVE_STREAMS.fetch_add(1, Ordering::Relaxed) + 1;
    gauge!("daemon_active_streams").set(current as f64);

    // Update peak if needed using CAS loop
    loop {
        let peak = PEAK_STREAMS.load(Ordering::Relaxed);
        if current <= peak {
            break;
        }
        if PEAK_STREAMS
            .compare_exchange_weak(peak, current, Ordering::Relaxed, Ordering::Relaxed)
            .is_ok()
        {
            gauge!("daemon_peak_streams").set(current as f64);
            break;
        }
    }
}

/// Record stream end - decrements active streams.
pub fn record_stream_end() {
    if is_benchmark_mode() {
        return;
    }
    let current = ACTIVE_STREAMS.fetch_sub(1, Ordering::Relaxed) - 1;
    gauge!("daemon_active_streams").set(current as f64);
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
