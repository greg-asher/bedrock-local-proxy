package accounting

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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

type integrationDoer struct{}

func (integrationDoer) Do(_ context.Context, _ *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"chatcmpl_test","object":"chat.completion","created":1730000000,"model":"target","choices":[],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`))}, nil
}

type cancelIntegrationDoer struct {
	started chan struct{}
}

func (d *cancelIntegrationDoer) Do(ctx context.Context, _ *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: &cancelIntegrationBody{ctx: ctx, started: d.started}}, nil
}

type cancelIntegrationBody struct {
	ctx     context.Context
	started chan struct{}
}

func (b *cancelIntegrationBody) Read([]byte) (int, error) {
	select {
	case <-b.started:
	default:
		close(b.started)
	}
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *cancelIntegrationBody) Close() error { return nil }

func TestRecorderReceivesMixedRealEndpointOutcomes(t *testing.T) {
	inPrice, outPrice := 1.0, 2.0
	cfg := config.Config{Models: map[string]config.ModelConfig{"coding": {BedrockModelID: "target", InputPerMillion: &inPrice, OutputPerMillion: &outPrice}}}
	var output bytes.Buffer
	recorder := New(&output, FormatJSON, cfg.Models)
	s := server.NewWithTransport(cfg, integrationDoer{})
	s.SetCompletionRecorder(recorder.Record)

	response := httptest.NewRecorder()
	s.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding","messages":[]}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("chat status = %d", response.Code)
	}
	response = httptest.NewRecorder()
	s.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{`)))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("messages status = %d", response.Code)
	}
	response = httptest.NewRecorder()
	s.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("models status = %d", response.Code)
	}
	streamDoer := &cancelIntegrationDoer{started: make(chan struct{})}
	canceledServer := server.NewWithTransport(cfg, streamDoer)
	canceledServer.SetCompletionRecorder(recorder.Record)
	requestContext, cancel := context.WithCancel(context.Background())
	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding","messages":[],"stream":true}`)).WithContext(requestContext)
		canceledServer.ServeHTTP(httptest.NewRecorder(), request)
	}()
	select {
	case <-streamDoer.started:
	case <-time.After(time.Second):
		t.Fatal("streaming fake did not start")
	}
	cancel()
	select {
	case <-streamDone:
	case <-time.After(time.Second):
		t.Fatal("canceled stream did not finish")
	}

	recorder.WriteSummary()
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 5 {
		t.Fatalf("records = %d, want chat, messages, models, canceled stream, summary: %q", len(lines), output.String())
	}
	var rejected struct {
		HTTPStatus int `json:"http_status"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &rejected); err != nil {
		t.Fatal(err)
	}
	if rejected.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("rejected status = %d, want 400", rejected.HTTPStatus)
	}
	var summary summaryRecord
	if err := json.Unmarshal([]byte(lines[4]), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Requests != 3 || summary.Successes != 1 || summary.Failures != 2 || summary.IncompleteRequests != 0 {
		t.Fatalf("mixed summary = %+v", summary)
	}
}
