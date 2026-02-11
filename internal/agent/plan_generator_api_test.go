package agent

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestNormalizeReasoningEffort(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty defaults to high", in: "", want: "high"},
		{name: "trim and lower", in: "  MEDIUM ", want: "medium"},
		{name: "unsupported defaults to high", in: "ultra", want: "high"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeReasoningEffort(tc.in)
			if got != tc.want {
				t.Fatalf("normalizeReasoningEffort(%q)=%q want=%q", tc.in, got, tc.want)
			}
		})
	}
}

func TestReadResponsesStreamDelta(t *testing.T) {
	stream := strings.Join([]string{
		"event: response.created",
		`data: {"type":"response.created","response":{"id":"resp_1"}}`,
		"",
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","delta":"{\"summary\":\"ok\",","sequence_number":1}`,
		"",
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","delta":"\"files\":[]}","sequence_number":2}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	got, err := readResponsesStream(strings.NewReader(stream), 1024*1024)
	if err != nil {
		t.Fatalf("readResponsesStream returned error: %v", err)
	}
	want := `{"summary":"ok","files":[]}`
	if got != want {
		t.Fatalf("readResponsesStream returned %q want %q", got, want)
	}
}

func TestReadResponsesStreamCompletedFallback(t *testing.T) {
	stream := strings.Join([]string{
		"event: response.created",
		`data: {"type":"response.created","response":{"id":"resp_2"}}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"{\"summary\":\"ok\",\"files\":[]}"}]}]}}`,
		"",
	}, "\n")

	got, err := readResponsesStream(strings.NewReader(stream), 1024*1024)
	if err != nil {
		t.Fatalf("readResponsesStream returned error: %v", err)
	}
	want := `{"summary":"ok","files":[]}`
	if got != want {
		t.Fatalf("readResponsesStream returned %q want %q", got, want)
	}
}

func TestIsRetryableAPIError(t *testing.T) {
	if !isRetryableAPIError(apiHTTPError{statusCode: 429}) {
		t.Fatalf("429 should be retryable")
	}
	if !isRetryableAPIError(apiHTTPError{statusCode: 502}) {
		t.Fatalf("5xx should be retryable")
	}
	if isRetryableAPIError(apiHTTPError{statusCode: 400}) {
		t.Fatalf("400 should not be retryable")
	}
	if isRetryableAPIError(errors.New("plain error")) {
		t.Fatalf("plain error should not be retryable")
	}
}

func TestParseCodexOutputFallbackToJSONBody(t *testing.T) {
	raw := []byte("prefix text\n{\"summary\":\"ok\",\"files\":[]}\ntrailing")
	plan, err := parseCodexOutput(raw)
	if err != nil {
		t.Fatalf("parseCodexOutput returned error: %v", err)
	}
	if plan.Summary != "ok" {
		t.Fatalf("summary=%q", plan.Summary)
	}
	if len(plan.Files) != 0 {
		t.Fatalf("files len=%d", len(plan.Files))
	}
}

func TestReadResponsesStreamTooLarge(t *testing.T) {
	delta := strings.Repeat("x", 20)
	stream := fmt.Sprintf("data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\n", delta)
	_, err := readResponsesStream(strings.NewReader(stream), 10)
	if err == nil {
		t.Fatalf("expected size error")
	}
}
