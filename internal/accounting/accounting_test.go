package accounting

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gregasher/bedrock-local-proxy/internal/config"
	"github.com/gregasher/bedrock-local-proxy/internal/server"
)

func TestJSONRecordsAndSummaryUseRealCompletionMetadata(t *testing.T) {
	inPrice, outPrice := 2.0, 3.0
	var output bytes.Buffer
	recorder := New(&output, FormatJSON, map[string]config.ModelConfig{
		"coding": {InputPerMillion: &inPrice, OutputPerMillion: &outPrice},
	})
	inTokens, outTokens, status := int64(100), int64(50), 200
	recorder.Record(server.CompletionResult{
		Endpoint: "/v1/responses", LocalModel: "coding", UpstreamModel: "provider/model",
		Elapsed: 12 * time.Millisecond, HTTPStatus: &status, Outcome: server.CompletionSucceeded,
		InputTokens: &inTokens, OutputTokens: &outTokens,
	})
	recorder.Record(server.CompletionResult{
		Endpoint: "/v1/models", HTTPStatus: &status, Outcome: server.CompletionSucceeded,
	})
	recorder.Record(server.CompletionResult{
		Endpoint: "/v1/messages", LocalModel: "coding", Outcome: server.CompletionCanceled,
	})
	recorder.WriteSummary()

	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("got %d JSON lines, want 4: %q", len(lines), output.String())
	}
	for _, line := range lines {
		var value map[string]any
		if err := json.Unmarshal([]byte(line), &value); err != nil {
			t.Fatalf("line is not JSON: %v (%q)", err, line)
		}
	}
	var request struct {
		EstimatedCost *float64 `json:"estimated_cost"`
		InputTokens   *int64   `json:"input_tokens"`
		Outcome       string   `json:"outcome"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &request); err != nil {
		t.Fatal(err)
	}
	if request.EstimatedCost == nil || *request.EstimatedCost != 0.00035 || request.InputTokens == nil || *request.InputTokens != 100 {
		t.Fatalf("successful request metadata = %+v", request)
	}
	var summary summaryRecord
	if err := json.Unmarshal([]byte(lines[3]), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Requests != 2 || summary.Successes != 1 || summary.Failures != 1 || summary.KnownInputTokens != 100 || summary.KnownOutputTokens != 50 || summary.KnownEstimatedCost != 0.00035 || summary.MissingUsageRequests != 1 || summary.MissingEstimateRequests != 1 {
		t.Fatalf("summary = %+v", summary)
	}
}

func TestEstimatesDistinguishZeroMissingAndUncoveredPrices(t *testing.T) {
	zero := 0.0
	var output bytes.Buffer
	recorder := New(&output, FormatJSON, map[string]config.ModelConfig{
		"zero":    {InputPerMillion: &zero, OutputPerMillion: &zero},
		"missing": {},
	})
	inTokens, outTokens := int64(0), int64(0)
	for _, result := range []server.CompletionResult{
		{Endpoint: "/v1/chat/completions", LocalModel: "zero", InputTokens: &inTokens, OutputTokens: &outTokens, Outcome: server.CompletionSucceeded},
		{Endpoint: "/v1/chat/completions", LocalModel: "missing", InputTokens: &inTokens, OutputTokens: &outTokens, Outcome: server.CompletionSucceeded},
		{Endpoint: "/v1/chat/completions", LocalModel: "zero", InputTokens: &inTokens, OutputTokens: &outTokens, ObservedUncoveredBillingFields: true, Outcome: server.CompletionSucceeded},
	} {
		recorder.Record(result)
	}
	recorder.WriteSummary()

	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	var first, second, third struct {
		EstimatedCost *float64 `json:"estimated_cost"`
	}
	for i, target := range []*struct {
		EstimatedCost *float64 `json:"estimated_cost"`
	}{&first, &second, &third} {
		if err := json.Unmarshal([]byte(lines[i]), target); err != nil {
			t.Fatal(err)
		}
	}
	if first.EstimatedCost == nil || *first.EstimatedCost != 0 {
		t.Fatalf("zero-priced estimate = %v, want pointer to zero", first.EstimatedCost)
	}
	if second.EstimatedCost != nil || third.EstimatedCost != nil {
		t.Fatalf("unknown estimates = %v and %v, want null", second.EstimatedCost, third.EstimatedCost)
	}
}

func TestConcurrentRecordsHaveConsistentTotals(t *testing.T) {
	recorder := New(io.Discard, FormatText, nil)
	const count = 200
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recorder.Record(server.CompletionResult{Endpoint: "/v1/messages", Outcome: server.CompletionFailed})
		}()
	}
	wg.Wait()
	recorder.mu.Lock()
	got := recorder.totals
	recorder.mu.Unlock()
	if got.requests != count || got.successes != 0 || got.failures != count || got.missingUsageRequests != count || got.missingEstimates != count {
		t.Fatalf("totals = %+v", got)
	}
}

func TestTextRecordsDoNotContainPromptOrHeaderContent(t *testing.T) {
	var output bytes.Buffer
	recorder := New(&output, FormatText, nil)
	recorder.Record(server.CompletionResult{Endpoint: "/v1/messages", LocalModel: "coding", Outcome: server.CompletionFailed})
	recorder.WriteSummary()
	if strings.Contains(output.String(), "prompt-secret") || strings.Contains(output.String(), "authorization-secret") {
		t.Fatalf("metadata output leaked sentinel content: %q", output.String())
	}
	for scanner := bufio.NewScanner(&output); scanner.Scan(); {
		if strings.TrimSpace(scanner.Text()) == "" {
			t.Fatal("empty accounting line")
		}
	}
}
