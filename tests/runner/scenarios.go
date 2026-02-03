package main

import (
	"encoding/json"

	"github.com/rlerdorf/mod_socket_handoff/tests/testclient"
)

// Scenario defines a test scenario.
type Scenario struct {
	// Name is the scenario identifier
	Name string
	// Description explains what is being tested
	Description string
	// Category for grouping (e.g., "basic", "unicode", "chunks", "errors")
	Category string
	// TestPattern is the X-Test-Pattern header value (empty for default)
	TestPattern string
	// HandoffData overrides for the handoff JSON
	HandoffData map[string]interface{}
	// Assertions to run on the response
	Assertions func(a *testclient.Assertions) *testclient.Assertions
}

// AllScenarios returns all defined test scenarios.
func AllScenarios() []Scenario {
	return []Scenario{
		// Basic tests
		{
			Name:        "basic_response",
			Description: "Basic HTTP 200 with SSE format and [DONE] marker",
			Category:    "basic",
			TestPattern: "",
			Assertions: func(a *testclient.Assertions) *testclient.Assertions {
				return a.
					AssertNoError().
					AssertHTTPStatus(200).
					AssertSSEFormat().
					AssertDoneMarker()
				// Note: finish_reason is not forwarded by all daemons (e.g., Rust uses simplified format)
			},
		},
		{
			Name:        "basic_content",
			Description: "Verify basic response has content",
			Category:    "basic",
			TestPattern: "",
			Assertions: func(a *testclient.Assertions) *testclient.Assertions {
				return a.
					AssertNoError().
					AssertHTTPStatus(200).
					AssertChunkCountAtLeast(1).
					AssertUTF8Valid()
			},
		},

		// Unicode tests
		{
			Name:        "unicode_content",
			Description: "Unicode content including emoji, CJK, and 4-byte UTF-8",
			Category:    "unicode",
			TestPattern: "unicode",
			Assertions: func(a *testclient.Assertions) *testclient.Assertions {
				return a.
					AssertNoError().
					AssertHTTPStatus(200).
					AssertSSEFormat().
					AssertUTF8Valid().
					AssertDoneMarker()
			},
		},
		{
			Name:        "unicode_emoji",
			Description: "Verify emoji are preserved",
			Category:    "unicode",
			TestPattern: "unicode",
			Assertions: func(a *testclient.Assertions) *testclient.Assertions {
				return a.
					AssertNoError().
					AssertHTTPStatus(200).
					AssertUTF8Valid().
					// Check for some emoji from the test data
					AssertContentContains("\U0001F600"). // 😀
					AssertContentContains("\U0001F680") // 🚀
			},
		},
		{
			Name:        "unicode_cjk",
			Description: "Verify CJK characters are preserved",
			Category:    "unicode",
			TestPattern: "unicode",
			Assertions: func(a *testclient.Assertions) *testclient.Assertions {
				return a.
					AssertNoError().
					AssertHTTPStatus(200).
					AssertUTF8Valid().
					// Check for CJK from the test data
					AssertContentContains("\u4E2D"). // 中
					AssertContentContains("\u6587") // 文
			},
		},
		{
			Name:        "unicode_4byte",
			Description: "Verify 4-byte UTF-8 characters are preserved",
			Category:    "unicode",
			TestPattern: "unicode",
			Assertions: func(a *testclient.Assertions) *testclient.Assertions {
				return a.
					AssertNoError().
					AssertHTTPStatus(200).
					AssertUTF8Valid().
					// Check for mathematical symbols (4-byte)
					AssertContentContains("\U0001D400") // 𝐀
			},
		},

		// Chunk variation tests
		{
			Name:        "short_chunks",
			Description: "Short (1-2 byte) SSE chunks",
			Category:    "chunks",
			TestPattern: "short",
			Assertions: func(a *testclient.Assertions) *testclient.Assertions {
				return a.
					AssertNoError().
					AssertHTTPStatus(200).
					AssertSSEFormat().
					AssertChunkCountAtLeast(10). // Short chunks should have many
					AssertUTF8Valid().
					AssertDoneMarker()
			},
		},
		{
			Name:        "long_chunks",
			Description: "Long (~4KB) SSE chunks",
			Category:    "chunks",
			TestPattern: "long",
			Assertions: func(a *testclient.Assertions) *testclient.Assertions {
				return a.
					AssertNoError().
					AssertHTTPStatus(200).
					AssertSSEFormat().
					AssertChunkCountAtLeast(1).
					AssertUTF8Valid().
					AssertDoneMarker()
			},
		},

		// Abort tests
		// Note: Some daemons (e.g., Go) always send [DONE] to properly close the SSE stream,
		// even when the upstream stream is truncated. These tests verify the stream is
		// truncated at the expected point but don't require [DONE] to be absent.
		{
			Name:        "abort_stream",
			Description: "Stream aborts after N chunks",
			Category:    "abort",
			TestPattern: "abort:5",
			Assertions: func(a *testclient.Assertions) *testclient.Assertions {
				return a.
					AssertNoError().
					AssertHTTPStatus(200).
					AssertSSEFormat().
					AssertChunkCountAtLeast(1) // At least some content before truncation
			},
		},
		{
			Name:        "abort_early",
			Description: "Stream aborts after 1 chunk",
			Category:    "abort",
			TestPattern: "abort:1",
			Assertions: func(a *testclient.Assertions) *testclient.Assertions {
				return a.
					AssertNoError().
					AssertHTTPStatus(200).
					AssertSSEFormat().
					AssertChunkCountAtLeast(1)
			},
		},

		// Finish reason tests
		// Note: Most daemons use simplified SSE format and don't forward finish_reason.
		// These tests verify the stream completes with [DONE] marker.
		{
			Name:        "finish_stop",
			Description: "Stream completes with [DONE] marker (finish_reason=stop pattern)",
			Category:    "finish",
			TestPattern: "finish:stop",
			Assertions: func(a *testclient.Assertions) *testclient.Assertions {
				return a.
					AssertNoError().
					AssertHTTPStatus(200).
					AssertSSEFormat().
					AssertDoneMarker()
			},
		},
		{
			Name:        "finish_length",
			Description: "Stream completes with [DONE] marker (finish_reason=length pattern)",
			Category:    "finish",
			TestPattern: "finish:length",
			Assertions: func(a *testclient.Assertions) *testclient.Assertions {
				return a.
					AssertNoError().
					AssertHTTPStatus(200).
					AssertSSEFormat().
					AssertDoneMarker()
			},
		},
	}
}

// ScenariosByCategory returns scenarios filtered by category.
func ScenariosByCategory(category string) []Scenario {
	all := AllScenarios()
	if category == "" || category == "all" {
		return all
	}
	result := make([]Scenario, 0)
	for _, s := range all {
		if s.Category == category {
			result = append(result, s)
		}
	}
	return result
}

// ScenarioByName finds a scenario by name.
func ScenarioByName(name string) *Scenario {
	for _, s := range AllScenarios() {
		if s.Name == name {
			return &s
		}
	}
	return nil
}

// BuildHandoffData creates the handoff JSON for a scenario.
func BuildHandoffData(s *Scenario, backendURL string) ([]byte, error) {
	data := map[string]interface{}{
		"prompt":      "test prompt",
		"user_id":     1,
		"backend_url": backendURL,
	}

	// Add test pattern as header hint in handoff data
	if s.TestPattern != "" {
		data["test_pattern"] = s.TestPattern
	}

	// Merge scenario-specific data
	for k, v := range s.HandoffData {
		data[k] = v
	}

	return json.Marshal(data)
}
