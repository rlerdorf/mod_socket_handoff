#!/usr/bin/env python3
"""
Analyze benchmark results and generate comparison report.

This script processes benchmark results from all four daemon implementations
(Python, PHP, Go, Rust) and generates a comprehensive comparison report
including latency, memory, and CPU metrics.

Usage:
    python3 analyze.py results/YYYY-MM-DD-HHMMSS/
"""

import csv
import json
import sys
from pathlib import Path
from typing import Any, Dict, List, Optional


def load_json(path: Path) -> Optional[Dict[str, Any]]:
    """Load JSON file if it exists."""
    if path.exists():
        with open(path) as f:
            return json.load(f)
    return None


def load_resources_csv(path: Path) -> Optional[Dict[str, Any]]:
    """Load resources CSV and calculate statistics.

    Handles the enhanced CSV format with columns:
    timestamp,rss_kb,vsz_kb,threads,utime,stime,cpu_pct,active_conns
    """
    if not path.exists():
        return None

    rss_values: List[int] = []
    vsz_values: List[int] = []
    thread_values: List[int] = []
    cpu_values: List[float] = []
    conn_values: List[int] = []

    # Track samples with connections for memory-per-conn calculation
    conn_rss_pairs: List[tuple] = []

    with open(path) as f:
        reader = csv.DictReader(f)
        for row in reader:
            try:
                rss = int(row.get("rss_kb", 0))
                if rss > 0:
                    rss_values.append(rss)

                vsz = int(row.get("vsz_kb", 0))
                if vsz > 0:
                    vsz_values.append(vsz)

                threads = int(row.get("threads", 0))
                if threads > 0:
                    thread_values.append(threads)

                cpu_str = row.get("cpu_pct", "0")
                if cpu_str:
                    cpu = float(cpu_str)
                    cpu_values.append(cpu)

                conn_str = row.get("active_conns", "")
                if conn_str:
                    conns = int(float(conn_str))
                    conn_values.append(conns)
                    conn_rss_pairs.append((conns, rss))

            except (ValueError, KeyError):
                pass  # Skip rows with invalid or missing numeric fields

    if not rss_values:
        return None

    result: Dict[str, Any] = {
        "min_rss_kb": min(rss_values),
        "max_rss_kb": max(rss_values),
        "avg_rss_kb": sum(rss_values) / len(rss_values),
        "samples": len(rss_values),
    }

    if vsz_values:
        result["min_vsz_kb"] = min(vsz_values)
        result["max_vsz_kb"] = max(vsz_values)

    if thread_values:
        result["max_threads"] = max(thread_values)

    if cpu_values:
        result["avg_cpu_pct"] = sum(cpu_values) / len(cpu_values)
        result["max_cpu_pct"] = max(cpu_values)
        result["min_cpu_pct"] = min(cpu_values)

    if conn_values:
        result["max_conns"] = max(conn_values)

        # Calculate memory per connection
        # Use baseline (low conn count) vs peak comparison
        memory_per_conn = calculate_memory_per_connection(conn_rss_pairs)
        if memory_per_conn is not None:
            result["memory_per_conn_kb"] = memory_per_conn

    return result


def calculate_memory_per_connection(
    conn_rss_pairs: List[tuple],
) -> Optional[float]:
    """Calculate memory overhead per connection from (connections, rss_kb) pairs.

    Uses the difference between baseline (low connection count) and peak
    memory divided by the number of connections at peak.
    """
    if len(conn_rss_pairs) < 10:
        return None

    # Sort by connection count
    sorted_pairs = sorted(conn_rss_pairs, key=lambda x: x[0])

    # Find baseline: average RSS when connections < 10
    baseline_samples = [rss for conns, rss in sorted_pairs if conns < 10]
    if not baseline_samples:
        # Use minimum RSS as baseline
        baseline = min(rss for _, rss in sorted_pairs)
    else:
        baseline = sum(baseline_samples) / len(baseline_samples)

    # Find peak: sample with highest connection count
    peak_conns, peak_rss = max(sorted_pairs, key=lambda x: x[0])

    # Need at least 100 connections for meaningful calculation
    if peak_conns < 100:
        return None

    memory_delta = peak_rss - baseline
    if memory_delta <= 0:
        return None

    return memory_delta / peak_conns


def analyze_daemon(results_dir: Path, daemon: str) -> Dict[str, Any]:
    """Analyze results for a single daemon."""
    daemon_dir = results_dir / daemon
    if not daemon_dir.exists():
        return {"error": "No results found"}

    result: Dict[str, Any] = {"daemon": daemon}

    # Load latency results (try both results.json and latency.json)
    latency_file = daemon_dir / "latency.json"
    if not latency_file.exists():
        latency_file = daemon_dir / "results.json"

    latency_data = load_json(latency_file)
    if latency_data:
        result["connections_started"] = latency_data.get("connections_started", 0)
        result["connections_completed"] = latency_data.get("connections_completed", 0)
        result["connections_failed"] = latency_data.get("connections_failed", 0)
        result["bytes_received"] = latency_data.get("bytes_received", 0)

        if "handoff_latency_ms" in latency_data:
            result["handoff_p50_ms"] = latency_data["handoff_latency_ms"].get("p50", 0)
            result["handoff_p95_ms"] = latency_data["handoff_latency_ms"].get("p95", 0)
            result["handoff_p99_ms"] = latency_data["handoff_latency_ms"].get("p99", 0)

        if "ttfb_latency_ms" in latency_data:
            result["ttfb_p50_ms"] = latency_data["ttfb_latency_ms"].get("p50", 0)
            result["ttfb_p95_ms"] = latency_data["ttfb_latency_ms"].get("p95", 0)
            result["ttfb_p99_ms"] = latency_data["ttfb_latency_ms"].get("p99", 0)

        if "stream_duration_ms" in latency_data:
            result["stream_p50_ms"] = latency_data["stream_duration_ms"].get("p50", 0)
            result["stream_p99_ms"] = latency_data["stream_duration_ms"].get("p99", 0)

        if "errors" in latency_data:
            result["errors"] = latency_data["errors"]

    # Load resource results (try both resources.csv and memory.csv)
    resources_file = daemon_dir / "resources.csv"
    if not resources_file.exists():
        resources_file = daemon_dir / "memory.csv"

    resources_data = load_resources_csv(resources_file)
    if resources_data:
        result["memory"] = {
            "baseline_mb": resources_data["min_rss_kb"] / 1024,
            "peak_mb": resources_data["max_rss_kb"] / 1024,
            "avg_mb": resources_data["avg_rss_kb"] / 1024,
            "delta_mb": (resources_data["max_rss_kb"] - resources_data["min_rss_kb"])
            / 1024,
        }

        if "memory_per_conn_kb" in resources_data:
            result["memory"]["per_conn_kb"] = resources_data["memory_per_conn_kb"]

        if "avg_cpu_pct" in resources_data:
            result["cpu"] = {
                "avg_pct": resources_data["avg_cpu_pct"],
                "peak_pct": resources_data["max_cpu_pct"],
            }

        if "max_threads" in resources_data:
            result["threads_max"] = resources_data["max_threads"]

        if "max_conns" in resources_data:
            result["peak_connections"] = resources_data["max_conns"]

    return result


def print_comparison(results: Dict[str, Dict[str, Any]]) -> None:
    """Print comparison table."""
    daemons = ["python", "php", "go", "rust"]
    available = [d for d in daemons if d in results and "error" not in results[d]]

    if not available:
        print("No results to compare")
        return

    print("\n" + "=" * 80)
    print("BENCHMARK COMPARISON")
    print("=" * 80)

    # Connection stats
    print("\n### Connection Statistics")
    print(
        f"{'Daemon':<12} {'Started':>10} {'Completed':>10} {'Failed':>10} {'Success %':>10}"
    )
    print("-" * 52)
    for d in available:
        r = results[d]
        started = r.get("connections_started", 0)
        completed = r.get("connections_completed", 0)
        failed = r.get("connections_failed", 0)
        pct = (completed / started * 100) if started > 0 else 0
        print(f"{d:<12} {started:>10} {completed:>10} {failed:>10} {pct:>9.1f}%")

    # Latency stats
    print("\n### Latency (milliseconds)")
    print(
        f"{'Daemon':<12} {'Handoff p50':>12} {'Handoff p99':>12} {'TTFB p50':>10} {'TTFB p99':>10}"
    )
    print("-" * 56)
    for d in available:
        r = results[d]
        h50 = r.get("handoff_p50_ms", 0)
        h99 = r.get("handoff_p99_ms", 0)
        t50 = r.get("ttfb_p50_ms", 0)
        t99 = r.get("ttfb_p99_ms", 0)
        print(f"{d:<12} {h50:>12.2f} {h99:>12.2f} {t50:>10.2f} {t99:>10.2f}")

    # Memory stats
    print("\n### Memory (RSS in MB)")
    print(
        f"{'Daemon':<12} {'Baseline':>10} {'Peak':>10} {'Delta':>10} {'Per-Conn KB':>12}"
    )
    print("-" * 56)
    for d in available:
        r = results[d]
        if "memory" in r:
            m = r["memory"]
            baseline = m.get("baseline_mb", 0)
            peak = m.get("peak_mb", 0)
            delta = m.get("delta_mb", 0)
            per_conn = m.get("per_conn_kb", None)
            per_conn_str = f"{per_conn:.1f}" if per_conn else "N/A"
            print(
                f"{d:<12} {baseline:>10.1f} {peak:>10.1f} {delta:>10.1f} {per_conn_str:>12}"
            )
        else:
            print(f"{d:<12} {'N/A':>10} {'N/A':>10} {'N/A':>10} {'N/A':>12}")

    # CPU stats (if available)
    has_cpu = any("cpu" in results[d] for d in available if d in results)
    if has_cpu:
        print("\n### CPU Usage")
        print(f"{'Daemon':<12} {'Avg %':>10} {'Peak %':>10} {'Threads':>10}")
        print("-" * 42)
        for d in available:
            r = results[d]
            if "cpu" in r:
                avg = r["cpu"].get("avg_pct", 0)
                peak = r["cpu"].get("peak_pct", 0)
                threads = r.get("threads_max", "N/A")
                threads_str = str(threads) if isinstance(threads, int) else threads
                print(f"{d:<12} {avg:>10.1f} {peak:>10.1f} {threads_str:>10}")
            else:
                print(f"{d:<12} {'N/A':>10} {'N/A':>10} {'N/A':>10}")

    # Peak connections (if available from Prometheus)
    has_peak_conns = any(
        "peak_connections" in results[d] for d in available if d in results
    )
    if has_peak_conns:
        print("\n### Peak Concurrent Connections (from Prometheus)")
        print(f"{'Daemon':<12} {'Peak Conns':>12}")
        print("-" * 24)
        for d in available:
            r = results[d]
            peak = r.get("peak_connections", "N/A")
            peak_str = str(peak) if isinstance(peak, int) else peak
            print(f"{d:<12} {peak_str:>12}")

    print("\n" + "=" * 80)


def main():
    if len(sys.argv) < 2:
        print("Usage: python3 analyze.py <results_directory>")
        print("")
        print("Analyzes benchmark results and generates a comparison report.")
        print("")
        print("Example:")
        print("  python3 analyze.py results/2024-01-15-120000")
        sys.exit(1)

    results_dir = Path(sys.argv[1])
    if not results_dir.exists():
        print(f"Directory not found: {results_dir}")
        sys.exit(1)

    print(f"Analyzing results in: {results_dir}")

    results: Dict[str, Dict[str, Any]] = {}
    for daemon in ["python", "php", "go", "rust"]:
        results[daemon] = analyze_daemon(results_dir, daemon)
        if "error" not in results[daemon]:
            print(f"  Found {daemon} results")

    print_comparison(results)

    # Save summary
    summary_file = results_dir / "summary.json"
    with open(summary_file, "w") as f:
        json.dump(results, f, indent=2)
    print(f"\nSummary saved to: {summary_file}")


if __name__ == "__main__":
    main()
