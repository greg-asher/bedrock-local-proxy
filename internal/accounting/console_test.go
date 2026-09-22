package accounting

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/gregasher/bedrock-local-proxy/internal/console"
	"github.com/gregasher/bedrock-local-proxy/internal/server"
)

// The console footer reads live totals back from the recorder, so a recorder
// that held its own lock across a console write would deadlock the proxy on
// the first request. This pins the lock ordering rather than the output.
func TestRecordDoesNotDeadlockAgainstConsoleStatus(t *testing.T) {
	var output bytes.Buffer
	out := console.New(&output, console.ColorNever)
	recorder := New(&output, FormatText, nil)
	recorder.SetConsole(out)
	out.StartStatus(recorder.Status)
	defer out.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		status := 200
		recorder.Record(server.CompletionResult{Endpoint: "/v1/messages", LocalModel: "coding", HTTPStatus: &status, Outcome: server.CompletionSucceeded})
		recorder.WriteSummary()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("recording deadlocked against the console status callback")
	}
	if !strings.Contains(output.String(), "messages") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestTextOutputStaysPlainForNonTerminalWriters(t *testing.T) {
	var output bytes.Buffer
	out := console.New(&output, console.ColorAuto)
	if out.Rich() {
		t.Fatal("a buffer must never be treated as an interactive terminal")
	}
	recorder := New(&output, FormatText, nil)
	recorder.SetConsole(out)
	status := 402
	recorder.Record(server.CompletionResult{Endpoint: "/v1/chat/completions", LocalModel: "coding", HTTPStatus: &status, Outcome: server.CompletionFailed, FailureCategory: "budget_exhausted"})
	recorder.WriteSummary()
	text := output.String()
	if strings.Contains(text, "\x1b[") {
		t.Fatalf("redirected output contains escape sequences: %q", text)
	}
	for _, want := range []string{"ERR", "chat", "coding", "402 budget_exhausted", "requests"} {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q:\n%s", want, text)
		}
	}
}

func TestRequestLineRendersSuccessCostAndCache(t *testing.T) {
	out := console.New(&bytes.Buffer{}, console.ColorNever)
	status := 200
	input, output, cacheRead, cost := int64(1240), int64(340), int64(12000), 0.0048
	line := requestLine(out, requestRecord{
		Endpoint: "/v1/messages", LocalModel: "coding", Outcome: "success", HTTPStatus: &status, LatencyMS: 1200,
		InputTokens: &input, OutputTokens: &output, CacheReadInputTokens: &cacheRead, EstimatedCost: &cost,
	}, time.Date(2026, 9, 20, 14, 13, 49, 0, time.Local))
	for _, want := range []string{"14:13:49", "ok", "messages", "coding", "1.2s", "in 1.2k", "out 340", "cache 12k", "$0.0048"} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q missing %q", line, want)
		}
	}
	if strings.Contains(line, "unavailable") {
		t.Errorf("compact line should omit empty fields, got %q", line)
	}
}

func TestRequestLineOmitsAbsentUsageInsteadOfPrintingUnavailable(t *testing.T) {
	out := console.New(&bytes.Buffer{}, console.ColorNever)
	status := 401
	line := requestLine(out, requestRecord{
		Endpoint: "/v1/chat/completions", LocalModel: "coding", Outcome: "failure", HTTPStatus: &status, LatencyMS: 12, FailureCategory: "transport",
	}, time.Date(2026, 9, 20, 14, 13, 49, 0, time.Local))
	if !strings.Contains(line, "401 transport") || strings.Contains(line, "unavailable") || strings.Contains(line, "in ") {
		t.Errorf("line = %q", line)
	}
}

func TestRequestLineAdaptsFromFortyColumnsUp(t *testing.T) {
	out := console.New(&bytes.Buffer{}, console.ColorNever)
	status := 200
	input, output, cacheRead, cost := int64(1240), int64(340), int64(12000), 0.0048
	entry := requestRecord{
		Endpoint: "/v1/messages", LocalModel: "coding", Outcome: "success", HTTPStatus: &status, LatencyMS: 1200,
		InputTokens: &input, OutputTokens: &output, CacheReadInputTokens: &cacheRead, EstimatedCost: &cost,
	}
	for _, test := range []struct {
		width int
		want  []string
		drop  []string
	}{
		{40, []string{"messages", "coding", "1.2s"}, []string{"14:13:49", "in 1.2k", "$0.0048"}},
		{48, []string{"$0.0048"}, []string{"14:13:49", "in 1.2k", "cache 12k"}},
		{54, []string{"14:13:49", "$0.0048"}, []string{"in 1.2k", "cache 12k"}},
		{78, []string{"in 1.2k", "out 340"}, []string{"cache 12k"}},
		{91, []string{"cache 12k"}, nil},
	} {
		line := requestLineAtWidth(out, entry, time.Date(2026, 9, 20, 14, 13, 49, 0, time.Local), test.width)
		if len([]rune(stripStyles(line))) > test.width {
			t.Errorf("width %d overflowed: %q", test.width, line)
		}
		for _, want := range test.want {
			if !strings.Contains(line, want) {
				t.Errorf("width %d missing %q: %q", test.width, want, line)
			}
		}
		for _, drop := range test.drop {
			if strings.Contains(line, drop) {
				t.Errorf("width %d kept %q: %q", test.width, drop, line)
			}
		}
	}
}

func TestNarrowFailureLineKeepsTheFailure(t *testing.T) {
	out := console.New(&bytes.Buffer{}, console.ColorNever)
	status := 429
	line := requestLineAtWidth(out, requestRecord{
		Endpoint: "/v1/chat/completions", LocalModel: "coding", Outcome: "failure", HTTPStatus: &status,
		LatencyMS: 120, FailureCategory: "upstream_http_response",
	}, time.Time{}, 40)
	if len([]rune(stripStyles(line))) > 40 || !strings.Contains(line, "429 upstream") || strings.Contains(line, "coding") {
		t.Fatalf("40-column failure line = %q", line)
	}
}
