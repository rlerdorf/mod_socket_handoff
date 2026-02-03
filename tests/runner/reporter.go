package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/rlerdorf/mod_socket_handoff/tests/testclient"
)

// TestResult represents the result of running a single test scenario.
type TestResult struct {
	Scenario   string
	Category   string
	Passed     bool
	Duration   time.Duration
	Assertions []testclient.AssertionResult
	Error      string
}

// TestSummary contains overall test run statistics.
type TestSummary struct {
	TotalTests   int
	PassedTests  int
	FailedTests  int
	TotalTime    time.Duration
	DaemonName   string
	BackendURL   string
	Results      []TestResult
}

// Reporter outputs test results in various formats.
type Reporter struct {
	output io.Writer
	format string
}

// NewReporter creates a new reporter.
func NewReporter(output io.Writer, format string) *Reporter {
	return &Reporter{
		output: output,
		format: format,
	}
}

// ReportResult outputs a single test result.
func (r *Reporter) ReportResult(result TestResult) {
	switch r.format {
	case "tap":
		// TAP format outputs all results in reportTAPSummary for proper numbering
	case "json":
		// JSON outputs at the end
	default:
		r.reportHuman(result)
	}
}

// ReportSummary outputs the final summary.
func (r *Reporter) ReportSummary(summary TestSummary) {
	switch r.format {
	case "tap":
		r.reportTAPSummary(summary)
	case "json":
		r.reportJSON(summary)
	default:
		r.reportHumanSummary(summary)
	}
}

// reportTAP outputs a single test result in TAP format.
// Note: This method is not used directly; reportTAPSummary handles all TAP output
// with proper test numbering. This method exists for potential future per-result streaming.
func (r *Reporter) reportTAP(result TestResult, testNum int) {
	status := "ok"
	if !result.Passed {
		status = "not ok"
	}
	fmt.Fprintf(r.output, "%s %d - %s\n", status, testNum, result.Scenario)

	// Output assertion details as diagnostics
	for _, a := range result.Assertions {
		if !a.Passed {
			fmt.Fprintf(r.output, "  # %s: %s\n", a.Name, a.Message)
			if a.Details != "" {
				fmt.Fprintf(r.output, "  #   %s\n", a.Details)
			}
		}
	}
}

func (r *Reporter) reportTAPSummary(summary TestSummary) {
	// TAP header
	fmt.Fprintf(r.output, "TAP version 14\n")
	fmt.Fprintf(r.output, "1..%d\n", summary.TotalTests)

	// Output all results with proper numbering
	for i, result := range summary.Results {
		status := "ok"
		if !result.Passed {
			status = "not ok"
		}
		fmt.Fprintf(r.output, "%s %d - %s [%s]\n", status, i+1, result.Scenario, result.Category)

		// Output failed assertions as diagnostics
		for _, a := range result.Assertions {
			if !a.Passed {
				fmt.Fprintf(r.output, "  # FAIL: %s - %s\n", a.Name, a.Message)
				if a.Details != "" {
					fmt.Fprintf(r.output, "  #       %s\n", a.Details)
				}
			}
		}
	}

	// Summary comment
	fmt.Fprintf(r.output, "# Tests: %d, Passed: %d, Failed: %d, Time: %v\n",
		summary.TotalTests, summary.PassedTests, summary.FailedTests, summary.TotalTime)
	fmt.Fprintf(r.output, "# Daemon: %s, Backend: %s\n", summary.DaemonName, summary.BackendURL)
}

// JSON format
func (r *Reporter) reportJSON(summary TestSummary) {
	output := map[string]interface{}{
		"version":      "1.0",
		"daemon":       summary.DaemonName,
		"backend_url":  summary.BackendURL,
		"total_tests":  summary.TotalTests,
		"passed":       summary.PassedTests,
		"failed":       summary.FailedTests,
		"total_time":   summary.TotalTime.String(),
		"success":      summary.FailedTests == 0,
		"results":      summary.Results,
	}

	enc := json.NewEncoder(r.output)
	enc.SetIndent("", "  ")
	enc.Encode(output)
}

// Human-readable format
func (r *Reporter) reportHuman(result TestResult) {
	status := "\033[32mPASS\033[0m"
	if !result.Passed {
		status = "\033[31mFAIL\033[0m"
	}
	fmt.Fprintf(r.output, "[%s] %s (%v)\n", status, result.Scenario, result.Duration)

	// Show failed assertions
	for _, a := range result.Assertions {
		if !a.Passed {
			fmt.Fprintf(r.output, "  \033[31m✗\033[0m %s: %s\n", a.Name, a.Message)
			if a.Details != "" {
				fmt.Fprintf(r.output, "      %s\n", a.Details)
			}
		}
	}
}

func (r *Reporter) reportHumanSummary(summary TestSummary) {
	fmt.Fprintf(r.output, "\n%s\n", strings.Repeat("=", 60))
	fmt.Fprintf(r.output, "Test Summary for %s\n", summary.DaemonName)
	fmt.Fprintf(r.output, "%s\n", strings.Repeat("=", 60))
	fmt.Fprintf(r.output, "Backend: %s\n", summary.BackendURL)
	fmt.Fprintf(r.output, "Total:   %d tests\n", summary.TotalTests)

	passColor := "\033[32m"
	if summary.PassedTests < summary.TotalTests {
		passColor = "\033[33m"
	}
	fmt.Fprintf(r.output, "Passed:  %s%d\033[0m\n", passColor, summary.PassedTests)

	if summary.FailedTests > 0 {
		fmt.Fprintf(r.output, "Failed:  \033[31m%d\033[0m\n", summary.FailedTests)
		fmt.Fprintf(r.output, "\nFailed tests:\n")
		for _, result := range summary.Results {
			if !result.Passed {
				fmt.Fprintf(r.output, "  - %s\n", result.Scenario)
			}
		}
	} else {
		fmt.Fprintf(r.output, "Failed:  0\n")
	}

	fmt.Fprintf(r.output, "Time:    %v\n", summary.TotalTime)

	if summary.FailedTests == 0 {
		fmt.Fprintf(r.output, "\n\033[32m✓ All tests passed!\033[0m\n")
	} else {
		fmt.Fprintf(r.output, "\n\033[31m✗ Some tests failed\033[0m\n")
	}
}
