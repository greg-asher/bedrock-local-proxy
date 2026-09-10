// Package accounting records metadata-only request outcomes and session totals.
package accounting

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/gregasher/bedrock-local-proxy/internal/config"
	"github.com/gregasher/bedrock-local-proxy/internal/server"
)

type Format string

const (
	FormatText Format = "text"
	FormatJSON Format = "json"
)

type totals struct {
	requests, successes, failures          int
	knownInputTokens, knownOutputTokens    int64
	knownEstimatedCost                     float64
	missingUsageRequests, missingEstimates int
}

type Recorder struct {
	mu      sync.Mutex
	w       io.Writer
	format  Format
	models  map[string]config.ModelConfig
	started time.Time
	totals  totals
}

type requestRecord struct {
	Event                          string   `json:"event"`
	Timestamp                      string   `json:"timestamp"`
	Endpoint                       string   `json:"endpoint"`
	LocalModel                     string   `json:"local_model,omitempty"`
	UpstreamModel                  string   `json:"upstream_model,omitempty"`
	LatencyMS                      float64  `json:"latency_ms"`
	HTTPStatus                     *int     `json:"http_status,omitempty"`
	Outcome                        string   `json:"outcome"`
	InputTokens                    *int64   `json:"input_tokens,omitempty"`
	OutputTokens                   *int64   `json:"output_tokens,omitempty"`
	EstimatedCost                  *float64 `json:"estimated_cost,omitempty"`
	CostStatus                     string   `json:"cost_status"`
	UsageStatus                    string   `json:"usage_status"`
	ObservedUncoveredBillingFields bool     `json:"observed_uncovered_billing_fields,omitempty"`
}

type summaryRecord struct {
	Event                   string  `json:"event"`
	RuntimeSeconds          float64 `json:"runtime_seconds"`
	Requests                int     `json:"requests"`
	Successes               int     `json:"successes"`
	Failures                int     `json:"failures"`
	KnownInputTokens        int64   `json:"known_input_tokens"`
	KnownOutputTokens       int64   `json:"known_output_tokens"`
	KnownEstimatedCost      float64 `json:"known_estimated_cost"`
	MissingUsageRequests    int     `json:"missing_usage_requests"`
	MissingEstimateRequests int     `json:"missing_estimate_requests"`
	CostStatus              string  `json:"cost_status"`
}

func New(w io.Writer, format Format, models map[string]config.ModelConfig) *Recorder {
	copyModels := make(map[string]config.ModelConfig, len(models))
	for name, model := range models {
		copyModels[name] = model
	}
	return &Recorder{w: w, format: format, models: copyModels, started: time.Now()}
}

func (r *Recorder) Record(result server.CompletionResult) {
	input, output := result.InputTokens, result.OutputTokens
	cost, costOK := r.estimate(result)
	usageStatus := "known"
	if input == nil || output == nil {
		usageStatus = "unknown"
	}
	entry := requestRecord{Event: "request", Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Endpoint: result.Endpoint,
		LocalModel: result.LocalModel, UpstreamModel: result.UpstreamModel, LatencyMS: float64(result.Elapsed) / float64(time.Millisecond),
		HTTPStatus: result.HTTPStatus, Outcome: string(result.Outcome), InputTokens: input, OutputTokens: output,
		CostStatus: "unavailable", UsageStatus: usageStatus, ObservedUncoveredBillingFields: result.ObservedUncoveredBillingFields}
	if costOK {
		entry.EstimatedCost = &cost
		entry.CostStatus = "estimated"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if result.Endpoint != "/v1/models" {
		r.totals.requests++
		if result.Outcome == server.CompletionSucceeded {
			r.totals.successes++
		} else {
			r.totals.failures++
		}
		if input != nil {
			r.totals.knownInputTokens += *input
		}
		if output != nil {
			r.totals.knownOutputTokens += *output
		}
		if usageStatus != "known" {
			r.totals.missingUsageRequests++
		}
		if !costOK {
			r.totals.missingEstimates++
		} else {
			r.totals.knownEstimatedCost += cost
		}
	}
	if r.w == nil {
		return
	}
	if r.format == FormatJSON {
		_ = json.NewEncoder(r.w).Encode(entry)
		return
	}
	_, _ = fmt.Fprintf(r.w, "request endpoint=%s model=%s outcome=%s status=%s latency_ms=%.2f usage=%s cost=%s\n", entry.Endpoint, entry.LocalModel, entry.Outcome, statusText(entry.HTTPStatus), entry.LatencyMS, entry.UsageStatus, costText(entry))
}

func (r *Recorder) estimate(result server.CompletionResult) (float64, bool) {
	model, ok := r.models[result.LocalModel]
	if !ok || result.InputTokens == nil || result.OutputTokens == nil || result.ObservedUncoveredBillingFields || model.InputPerMillion == nil || model.OutputPerMillion == nil {
		return 0, false
	}
	return float64(*result.InputTokens)/1e6*(*model.InputPerMillion) + float64(*result.OutputTokens)/1e6*(*model.OutputPerMillion), true
}

func (r *Recorder) WriteSummary() {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := summaryRecord{Event: "summary", RuntimeSeconds: time.Since(r.started).Seconds(), Requests: r.totals.requests, Successes: r.totals.successes, Failures: r.totals.failures,
		KnownInputTokens: r.totals.knownInputTokens, KnownOutputTokens: r.totals.knownOutputTokens, KnownEstimatedCost: r.totals.knownEstimatedCost,
		MissingUsageRequests: r.totals.missingUsageRequests, MissingEstimateRequests: r.totals.missingEstimates, CostStatus: "unavailable"}
	if r.totals.missingEstimates == 0 && r.totals.requests > 0 {
		s.CostStatus = "estimated"
	}
	if r.w == nil {
		return
	}
	if r.format == FormatJSON {
		_ = json.NewEncoder(r.w).Encode(s)
		return
	}
	cost := "unavailable"
	if s.CostStatus == "estimated" {
		cost = fmt.Sprintf("$%.8f (estimated)", s.KnownEstimatedCost)
	}
	_, _ = fmt.Fprintf(r.w, "Session summary runtime=%.1fs requests=%d successes=%d failures=%d known_input_tokens=%d known_output_tokens=%d estimated_cost=%s missing_usage=%d missing_estimates=%d\n", s.RuntimeSeconds, s.Requests, s.Successes, s.Failures, s.KnownInputTokens, s.KnownOutputTokens, cost, s.MissingUsageRequests, s.MissingEstimateRequests)
}

func statusText(status *int) string {
	if status == nil {
		return "unavailable"
	}
	return fmt.Sprintf("%d", *status)
}
func costText(entry requestRecord) string {
	if entry.EstimatedCost == nil {
		return "unavailable"
	}
	return fmt.Sprintf("$%.8f (estimated)", *entry.EstimatedCost)
}
