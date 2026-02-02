use clap::Parser;

/// High-performance mock OpenAI-compatible streaming API server
#[derive(Parser, Debug, Clone)]
#[command(name = "mock-llm-api")]
#[command(about = "Mock LLM API server for benchmarking streaming daemons")]
pub struct Config {
    /// Listen address (TCP) for HTTP/2 multiplexing clients
    #[arg(short, long, default_value = "127.0.0.1:8080")]
    pub listen: String,

    /// Unix socket path (optional, in addition to TCP)
    /// This provides an alternative endpoint for high-concurrency benchmarks
    /// where TCP ephemeral port limits may be a concern
    #[arg(short, long)]
    pub socket: Option<String>,

    /// Number of chunks to stream per response
    #[arg(short = 'c', long, default_value = "18")]
    pub chunk_count: usize,

    /// Delay between chunks in milliseconds
    #[arg(short = 'd', long, default_value = "50")]
    pub chunk_delay_ms: u64,

    /// Number of Tokio worker threads (0 = num_cpus)
    #[arg(short = 'w', long, default_value = "0")]
    pub workers: usize,

    /// Minimal logging output
    #[arg(long)]
    pub quiet: bool,

    // HTTP/2 tuning options for high-concurrency scenarios

    /// Max concurrent streams per HTTP/2 connection.
    /// For 100k streams across 1k connections, use 100.
    #[arg(long, default_value = "100")]
    pub h2_max_streams: u32,

    /// HTTP/2 initial stream window size in KB (flow control per stream).
    /// Larger values reduce flow control blocking for streaming responses.
    #[arg(long, default_value = "1024")]
    pub h2_stream_window_kb: u32,

    /// HTTP/2 initial connection window size in KB (flow control per connection).
    /// Should be large enough for all concurrent streams: streams × stream_window.
    #[arg(long, default_value = "16384")]
    pub h2_connection_window_kb: u32,

    /// HTTP/2 max send buffer size per stream in KB.
    /// Buffers data during flow control waits.
    #[arg(long, default_value = "1024")]
    pub h2_send_buffer_kb: u32,

    /// HTTP/2 keep-alive ping interval in seconds (0 = disabled).
    /// Keeps idle connections alive during streaming gaps.
    #[arg(long, default_value = "20")]
    pub h2_keepalive_secs: u64,
}

impl Config {
    pub fn worker_threads(&self) -> usize {
        if self.workers == 0 {
            num_cpus()
        } else {
            self.workers
        }
    }
}

fn num_cpus() -> usize {
    std::thread::available_parallelism()
        .map(|p| p.get())
        .unwrap_or(4)
}
