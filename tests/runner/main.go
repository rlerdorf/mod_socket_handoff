// Test runner for streaming daemon validation.
//
// Runs test scenarios against streaming daemons via SCM_RIGHTS handoff,
// validating response correctness for Unicode, chunk handling, and protocol conformance.
//
// Usage:
//
//	./test-runner -socket /var/run/streaming-daemon.sock -backend http://127.0.0.1:8080
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/rlerdorf/mod_socket_handoff/tests/testclient"
)

// Config holds command-line configuration.
type Config struct {
	SocketPath string
	BackendURL string
	DaemonName string
	Timeout    time.Duration
	Category   string
	Scenario   string
	Format     string
	Verbose    bool
	List       bool
}

func main() {
	cfg := Config{}

	flag.StringVar(&cfg.SocketPath, "socket", "/var/run/streaming-daemon.sock",
		"Path to daemon Unix socket")
	flag.StringVar(&cfg.BackendURL, "backend", "http://127.0.0.1:8080",
		"Backend API URL for daemon to connect to")
	flag.StringVar(&cfg.DaemonName, "daemon", "unknown",
		"Daemon name for reporting (e.g., 'go', 'rust', 'php')")
	flag.DurationVar(&cfg.Timeout, "timeout", 30*time.Second,
		"Timeout for each test")
	flag.StringVar(&cfg.Category, "category", "all",
		"Test category to run (basic, unicode, chunks, abort, finish, all)")
	flag.StringVar(&cfg.Scenario, "scenario", "",
		"Run a specific scenario by name")
	flag.StringVar(&cfg.Format, "format", "human",
		"Output format (human, tap, json)")
	flag.BoolVar(&cfg.Verbose, "verbose", false,
		"Enable verbose output")
	flag.BoolVar(&cfg.List, "list", false,
		"List available scenarios and exit")

	flag.Parse()

	// List mode
	if cfg.List {
		listScenarios()
		return
	}

	// Verify socket exists
	if _, err := os.Stat(cfg.SocketPath); os.IsNotExist(err) {
		log.Fatalf("Socket does not exist: %s", cfg.SocketPath)
	}

	// Run tests
	os.Exit(runTests(cfg))
}

func listScenarios() {
	fmt.Println("Available test scenarios:")
	fmt.Println()

	categories := map[string][]Scenario{}
	for _, s := range AllScenarios() {
		categories[s.Category] = append(categories[s.Category], s)
	}

	for cat, scenarios := range categories {
		fmt.Printf("%s:\n", cat)
		for _, s := range scenarios {
			fmt.Printf("  %-20s  %s\n", s.Name, s.Description)
		}
		fmt.Println()
	}
}

func runTests(cfg Config) int {
	client := testclient.NewClient(cfg.SocketPath, cfg.Timeout)
	reporter := NewReporter(os.Stdout, cfg.Format)

	// Get scenarios to run
	var scenarios []Scenario
	if cfg.Scenario != "" {
		s := ScenarioByName(cfg.Scenario)
		if s == nil {
			log.Fatalf("Unknown scenario: %s", cfg.Scenario)
		}
		scenarios = []Scenario{*s}
	} else {
		scenarios = ScenariosByCategory(cfg.Category)
	}

	if len(scenarios) == 0 {
		log.Fatalf("No scenarios to run")
	}

	if cfg.Verbose {
		log.Printf("Running %d scenarios against %s", len(scenarios), cfg.SocketPath)
	}

	// Run scenarios
	summary := TestSummary{
		DaemonName: cfg.DaemonName,
		BackendURL: cfg.BackendURL,
		Results:    make([]TestResult, 0, len(scenarios)),
	}
	startTime := time.Now()

	for _, scenario := range scenarios {
		result := runScenario(client, scenario, cfg)
		summary.Results = append(summary.Results, result)
		summary.TotalTests++
		if result.Passed {
			summary.PassedTests++
		} else {
			summary.FailedTests++
		}

		// Report individual result (for human/tap formats)
		if cfg.Format == "human" {
			reporter.ReportResult(result)
		}
	}

	summary.TotalTime = time.Since(startTime)

	// Report summary
	reporter.ReportSummary(summary)

	// Exit code
	if summary.FailedTests > 0 {
		return 1
	}
	return 0
}

func runScenario(client *testclient.Client, scenario Scenario, cfg Config) TestResult {
	startTime := time.Now()

	result := TestResult{
		Scenario: scenario.Name,
		Category: scenario.Category,
	}

	// Build handoff data
	handoffData, err := BuildHandoffData(&scenario, cfg.BackendURL)
	if err != nil {
		result.Passed = false
		result.Error = fmt.Sprintf("build handoff data: %v", err)
		result.Duration = time.Since(startTime)
		return result
	}

	// Perform handoff
	handoff, err := client.Handoff(handoffData)
	if err != nil {
		result.Passed = false
		result.Error = fmt.Sprintf("handoff: %v", err)
		result.Duration = time.Since(startTime)
		return result
	}
	defer handoff.ResponseConn.Close()

	// Read response
	response := testclient.ReadResponse(handoff.ResponseConn)

	// Run assertions
	assertions := testclient.NewAssertions(response)
	scenario.Assertions(assertions)

	result.Assertions = assertions.Results()
	result.Passed = assertions.AllPassed()
	result.Duration = time.Since(startTime)

	return result
}
