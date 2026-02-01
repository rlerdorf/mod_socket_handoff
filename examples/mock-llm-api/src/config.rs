use clap::Parser;

/// High-performance mock OpenAI-compatible streaming API server
#[derive(Parser, Debug, Clone)]
#[command(name = "mock-llm-api")]
#[command(about = "Mock LLM API server for benchmarking streaming daemons")]
pub struct Config {
    /// Listen address
    #[arg(short, long, default_value = "127.0.0.1:8080")]
    pub listen: String,

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
