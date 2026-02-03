package testclient

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// AssertionResult represents the result of an assertion.
type AssertionResult struct {
	Passed  bool
	Name    string
	Message string
	Details string
}

// Assertions provides validation functions for responses.
type Assertions struct {
	response *Response
	results  []AssertionResult
}

// NewAssertions creates a new assertions helper for a response.
func NewAssertions(response *Response) *Assertions {
	return &Assertions{
		response: response,
		results:  make([]AssertionResult, 0),
	}
}

// Results returns all assertion results.
func (a *Assertions) Results() []AssertionResult {
	return a.results
}

// AllPassed returns true if all assertions passed.
func (a *Assertions) AllPassed() bool {
	for _, r := range a.results {
		if !r.Passed {
			return false
		}
	}
	return true
}

// FailedCount returns the number of failed assertions.
func (a *Assertions) FailedCount() int {
	count := 0
	for _, r := range a.results {
		if !r.Passed {
			count++
		}
	}
	return count
}

// AssertNoError checks that no error occurred during response reading.
func (a *Assertions) AssertNoError() *Assertions {
	passed := a.response.Error == nil
	msg := "no error"
	details := ""
	if !passed {
		msg = fmt.Sprintf("error occurred: %v", a.response.Error)
		details = a.response.Error.Error()
	}
	a.results = append(a.results, AssertionResult{
		Passed:  passed,
		Name:    "NoError",
		Message: msg,
		Details: details,
	})
	return a
}

// AssertHTTPStatus checks the HTTP status code.
func (a *Assertions) AssertHTTPStatus(expected int) *Assertions {
	passed := a.response.StatusCode == expected
	msg := fmt.Sprintf("HTTP status %d", expected)
	details := ""
	if !passed {
		msg = fmt.Sprintf("expected HTTP %d, got %d", expected, a.response.StatusCode)
		details = a.response.StatusLine
	}
	a.results = append(a.results, AssertionResult{
		Passed:  passed,
		Name:    "HTTPStatus",
		Message: msg,
		Details: details,
	})
	return a
}

// AssertSSEFormat checks that the response uses SSE format.
func (a *Assertions) AssertSSEFormat() *Assertions {
	// Check Content-Type header
	contentType := a.response.Headers["Content-Type"]
	isSSE := strings.Contains(contentType, "text/event-stream")

	// Check that chunks have proper SSE format
	hasChunks := len(a.response.Chunks) > 0
	allValid := true
	for _, chunk := range a.response.Chunks {
		if !strings.HasPrefix(chunk.Raw, "data: ") {
			allValid = false
			break
		}
	}

	passed := isSSE && hasChunks && allValid
	msg := "valid SSE format"
	details := ""
	if !passed {
		if !isSSE {
			msg = fmt.Sprintf("Content-Type not SSE: %s", contentType)
			details = contentType
		} else if !hasChunks {
			msg = "no SSE chunks received"
		} else {
			msg = "some chunks missing 'data: ' prefix"
		}
	}
	a.results = append(a.results, AssertionResult{
		Passed:  passed,
		Name:    "SSEFormat",
		Message: msg,
		Details: details,
	})
	return a
}

// AssertContent checks that the content exactly matches expected.
func (a *Assertions) AssertContent(expected string) *Assertions {
	passed := a.response.RawContent == expected
	msg := "content matches"
	details := ""
	if !passed {
		msg = "content mismatch"
		if len(a.response.RawContent) > 100 {
			details = fmt.Sprintf("got (first 100): %q...", a.response.RawContent[:100])
		} else {
			details = fmt.Sprintf("got: %q", a.response.RawContent)
		}
	}
	a.results = append(a.results, AssertionResult{
		Passed:  passed,
		Name:    "Content",
		Message: msg,
		Details: details,
	})
	return a
}

// AssertContentContains checks that the content contains a substring.
func (a *Assertions) AssertContentContains(substring string) *Assertions {
	passed := strings.Contains(a.response.RawContent, substring)
	msg := fmt.Sprintf("content contains %q", substring)
	details := ""
	if !passed {
		msg = fmt.Sprintf("content does not contain %q", substring)
		if len(a.response.RawContent) > 100 {
			details = fmt.Sprintf("got (first 100): %q...", a.response.RawContent[:100])
		} else {
			details = fmt.Sprintf("got: %q", a.response.RawContent)
		}
	}
	a.results = append(a.results, AssertionResult{
		Passed:  passed,
		Name:    "ContentContains",
		Message: msg,
		Details: details,
	})
	return a
}

// AssertUTF8Valid checks that all content is valid UTF-8.
func (a *Assertions) AssertUTF8Valid() *Assertions {
	passed := utf8.ValidString(a.response.RawContent)
	msg := "valid UTF-8"
	details := ""
	if !passed {
		msg = "invalid UTF-8 in content"
		// Find first invalid byte
		for i, r := range a.response.RawContent {
			if r == utf8.RuneError {
				details = fmt.Sprintf("invalid at byte %d", i)
				break
			}
		}
	}
	a.results = append(a.results, AssertionResult{
		Passed:  passed,
		Name:    "UTF8Valid",
		Message: msg,
		Details: details,
	})
	return a
}

// AssertChunkCount checks the number of content chunks.
func (a *Assertions) AssertChunkCount(expected int) *Assertions {
	count := a.response.ChunkCount()
	passed := count == expected
	msg := fmt.Sprintf("%d chunks", expected)
	details := ""
	if !passed {
		msg = fmt.Sprintf("expected %d chunks, got %d", expected, count)
		details = fmt.Sprintf("chunk count: %d", count)
	}
	a.results = append(a.results, AssertionResult{
		Passed:  passed,
		Name:    "ChunkCount",
		Message: msg,
		Details: details,
	})
	return a
}

// AssertChunkCountAtLeast checks that there are at least N content chunks.
func (a *Assertions) AssertChunkCountAtLeast(min int) *Assertions {
	count := a.response.ChunkCount()
	passed := count >= min
	msg := fmt.Sprintf("at least %d chunks", min)
	details := ""
	if !passed {
		msg = fmt.Sprintf("expected at least %d chunks, got %d", min, count)
		details = fmt.Sprintf("chunk count: %d", count)
	}
	a.results = append(a.results, AssertionResult{
		Passed:  passed,
		Name:    "ChunkCountAtLeast",
		Message: msg,
		Details: details,
	})
	return a
}

// AssertFinishReason checks the finish_reason value.
func (a *Assertions) AssertFinishReason(expected string) *Assertions {
	passed := a.response.FinishReason == expected
	msg := fmt.Sprintf("finish_reason=%q", expected)
	details := ""
	if !passed {
		msg = fmt.Sprintf("expected finish_reason=%q, got %q", expected, a.response.FinishReason)
		details = a.response.FinishReason
	}
	a.results = append(a.results, AssertionResult{
		Passed:  passed,
		Name:    "FinishReason",
		Message: msg,
		Details: details,
	})
	return a
}

// AssertDoneMarker checks for the [DONE] marker.
func (a *Assertions) AssertDoneMarker() *Assertions {
	passed := a.response.HasDoneMarker
	msg := "[DONE] marker present"
	if !passed {
		msg = "[DONE] marker missing"
	}
	a.results = append(a.results, AssertionResult{
		Passed:  passed,
		Name:    "DoneMarker",
		Message: msg,
	})
	return a
}

// AssertNoDoneMarker checks that [DONE] marker is absent (for abort tests).
func (a *Assertions) AssertNoDoneMarker() *Assertions {
	passed := !a.response.HasDoneMarker
	msg := "[DONE] marker absent"
	if !passed {
		msg = "[DONE] marker unexpectedly present"
	}
	a.results = append(a.results, AssertionResult{
		Passed:  passed,
		Name:    "NoDoneMarker",
		Message: msg,
	})
	return a
}

// AssertHeader checks for a specific header value.
func (a *Assertions) AssertHeader(name, expected string) *Assertions {
	value := a.response.Headers[name]
	passed := value == expected
	msg := fmt.Sprintf("%s: %s", name, expected)
	details := ""
	if !passed {
		msg = fmt.Sprintf("expected %s=%q, got %q", name, expected, value)
		details = value
	}
	a.results = append(a.results, AssertionResult{
		Passed:  passed,
		Name:    "Header",
		Message: msg,
		Details: details,
	})
	return a
}

// AssertHeaderContains checks that a header contains a substring.
func (a *Assertions) AssertHeaderContains(name, substring string) *Assertions {
	value := a.response.Headers[name]
	passed := strings.Contains(value, substring)
	msg := fmt.Sprintf("%s contains %q", name, substring)
	details := ""
	if !passed {
		msg = fmt.Sprintf("%s does not contain %q", name, substring)
		details = value
	}
	a.results = append(a.results, AssertionResult{
		Passed:  passed,
		Name:    "HeaderContains",
		Message: msg,
		Details: details,
	})
	return a
}

// AssertNoFinishReason checks that there is no finish_reason (for abort tests).
func (a *Assertions) AssertNoFinishReason() *Assertions {
	passed := a.response.FinishReason == ""
	msg := "no finish_reason"
	details := ""
	if !passed {
		msg = fmt.Sprintf("unexpected finish_reason: %s", a.response.FinishReason)
		details = a.response.FinishReason
	}
	a.results = append(a.results, AssertionResult{
		Passed:  passed,
		Name:    "NoFinishReason",
		Message: msg,
		Details: details,
	})
	return a
}
