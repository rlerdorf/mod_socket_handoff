# Streaming Daemon Test Framework

Functional and integration testing framework for validating streaming daemon implementations. Tests response correctness rather than performance.

## Quick Start

```bash
# Run all daemon tests (Go, Rust, PHP, Swoole, Swow, Python, io_uring)
make test

# Test a specific daemon
make test-go
make test-rust
make test-php
make test-swoole
make test-swow
make test-python-http2
make test-uring

# List available test scenarios
make list
```

## Architecture

```
Test Runner → Daemon (via SCM_RIGHTS) → Mock API (with test patterns) → SSE Response
     ↑                                                                        │
     └────────────────── Validate Response ───────────────────────────────────┘
```

The test framework:
1. Starts the mock-llm-api with test pattern support
2. Starts the daemon under test
3. Sends requests via SCM_RIGHTS socket handoff (same as Apache)
4. Validates the SSE response against expected patterns

## Test Categories

| Category | Tests | Description |
|----------|-------|-------------|
| basic | 2 | HTTP 200, SSE format, [DONE] marker |
| unicode | 4 | Emoji, CJK, 4-byte UTF-8 handling |
| chunks | 2 | Short (1-2 byte) and long (4KB) chunks |
| abort | 2 | Mid-stream disconnect handling |
| finish | 2 | Custom finish_reason values |

## Response Format

**Full OpenAI format** (what mock-llm-api sends):
```
data: {"id":"chatcmpl-mock","object":"chat.completion.chunk","choices":[{"delta":{"content":"Hello"},"finish_reason":null}]}
data: {"id":"chatcmpl-mock","object":"chat.completion.chunk","choices":[{"delta":{},"finish_reason":"stop"}]}
data: [DONE]
```

**Simplified format** (what most daemons send to clients):
```
data: {"content":"Hello"}
data: [DONE]
```

The daemons extract content from the OpenAI response and forward just the content. The test framework handles both formats automatically.

## Test Patterns

The mock-llm-api supports the `X-Test-Pattern` header to generate specific test content:

| Pattern | Description |
|---------|-------------|
| `unicode` | Emoji, CJK, 4-byte UTF-8 characters |
| `short` | 1-2 byte chunks |
| `long` | ~4KB chunks |
| `abort:N` | Abort after N chunks (no [DONE]) |
| `finish:reason` | Custom finish_reason (e.g., `finish:length`) |

## Output Formats

```bash
# Human-readable (default)
make test

# TAP (Test Anything Protocol) for CI
make test-tap

# JSON for analysis
make test-json
```

## Directory Structure

```
tests/
├── README.md
├── Makefile
├── go.mod
├── testclient/          # Core test client (Go)
│   ├── client.go        # SCM_RIGHTS socket handoff
│   ├── response.go      # Response reading and SSE parsing
│   └── assertions.go    # Validation functions
├── runner/
│   ├── main.go          # CLI entry point
│   ├── scenarios.go     # Test scenario definitions
│   └── reporter.go      # TAP/JSON output formatters
├── patterns/            # Test pattern definitions (documentation)
│   ├── unicode.json
│   ├── short-chunks.json
│   ├── long-chunks.json
│   └── abort.json
└── integration/         # Per-daemon test scripts
    ├── common.sh        # Shared utilities and mock API helpers
    ├── go_test.sh       # Go daemon tests
    ├── rust_test.sh     # Rust daemon tests
    ├── php_test.sh      # PHP AMP daemon tests
    ├── swoole_test.sh   # PHP Swoole daemon tests
    ├── swow_test.sh     # PHP Swow daemon tests
    ├── uring_test.sh    # C io_uring daemon tests
    └── python_http2_test.sh  # Python asyncio daemon tests
```

## Adding Test Scenarios

Edit `runner/scenarios.go` to add new test scenarios:

```go
{
    Name:        "my_test",
    Description: "Description of what is tested",
    Category:    "basic",
    TestPattern: "unicode",  // X-Test-Pattern header value
    Assertions: func(a *testclient.Assertions) *testclient.Assertions {
        return a.
            AssertNoError().
            AssertHTTPStatus(200).
            AssertContentContains("expected content")
    },
},
```

## Available Assertions

| Assertion | Description |
|-----------|-------------|
| `AssertNoError()` | No read errors |
| `AssertHTTPStatus(code)` | HTTP status code |
| `AssertSSEFormat()` | Valid SSE format |
| `AssertContent(expected)` | Exact content match |
| `AssertContentContains(substring)` | Content contains substring |
| `AssertUTF8Valid()` | Valid UTF-8 encoding |
| `AssertChunkCount(n)` | Exact chunk count |
| `AssertChunkCountAtLeast(n)` | Minimum chunks |
| `AssertFinishReason(reason)` | finish_reason value |
| `AssertDoneMarker()` | [DONE] marker present |
| `AssertNoDoneMarker()` | No [DONE] marker (abort tests) |
| `AssertHeader(name, value)` | Header value |
| `AssertHeaderContains(name, substring)` | Header contains |

## Daemon Requirements

For test patterns to work, daemons must:

1. Parse `test_pattern` from handoff data JSON
2. Pass it to the backend as the `X-Test-Pattern` header

Example Go implementation:
```go
if handoff.TestPattern != "" {
    req.Header.Set("X-Test-Pattern", handoff.TestPattern)
}
```

Example PHP implementation:
```php
if (isset($handoffData['test_pattern'])) {
    $headers['X-Test-Pattern'] = $handoffData['test_pattern'];
}
```

## CI Integration

The test runner returns exit code 0 on success, 1 on failure. Use TAP output for CI systems:

```yaml
# GitHub Actions example
- name: Run daemon tests
  run: make -C tests test-tap
```

## Troubleshooting

### Socket permission denied
- Ensure the test scripts set socket mode to 0666
- Or run tests with appropriate permissions

### Mock API not starting
- Check if port 28080 is already in use (default test port)
- Set `MOCK_API_PORT` environment variable to use a different port

### Daemon not starting
- Check daemon logs for errors
- Ensure required dependencies are installed
- Verify the daemon binary is built

### Test failures
- Use `make test FORMAT=json` for detailed failure information
- Check if the daemon correctly forwards X-Test-Pattern header
