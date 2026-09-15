package reports

import (
	"encoding/json"
	"github.com/gregasher/bedrock-local-proxy/internal/config"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecordedAndStrictRepricedAccounting(t *testing.T) {
	p := t.TempDir()
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	cost := 9.0
	writeSession(t, p, "s", []event{{Event: "request", Timestamp: start.Add(time.Hour).Format(time.RFC3339Nano), Endpoint: "/v1/responses", LocalModel: "coding", UpstreamModel: "us.openai.gpt-5.6-luna", Outcome: "success", InputTokens: int64ptr(1000), OutputTokens: int64ptr(100), EstimatedCost: &cost, UsageStatus: "known", ObservedUncoveredBillingFields: true}})
	r, e := Generate(Options{Directory: p, Start: start, Stop: start.Add(24 * time.Hour)})
	if e != nil || r.Metrics.KnownEstimatedCost != 9 || len(r.UsageWarnings) != 1 {
		t.Fatalf("recorded %#v %v", r, e)
	}
	in, out := 2.0, 10.0
	r, e = Generate(Options{Directory: p, Start: start, Stop: start.Add(24 * time.Hour), Reprice: true, Models: map[string]config.ModelConfig{"luna": {BedrockModelID: "us.openai.gpt-5.6-luna", InputPerMillion: &in, OutputPerMillion: &out}}})
	if e != nil || r.Metrics.RepricedCostRequests != 1 || r.Metrics.StoredCostRequests != 0 || r.Models[0].Name != "us.openai.gpt-5.6-luna" {
		t.Fatalf("repriced %#v %v", r, e)
	}
}
func writeSession(t *testing.T, parent, id string, events []event) {
	t.Helper()
	d := filepath.Join(parent, id)
	if e := os.Mkdir(d, 0700); e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(manifest{SchemaVersion: 1, SessionID: id, ReportPath: d})
	os.WriteFile(filepath.Join(d, "session.json"), b, 0600)
	f, _ := os.Create(filepath.Join(d, "events.jsonl"))
	defer f.Close()
	for _, e := range events {
		b, _ := json.Marshal(e)
		f.Write(append(b, '\n'))
	}
}
func int64ptr(v int64) *int64 { return &v }

func TestRepriceUsesLegacyCacheInclusiveEndpointsAndCurrentAlias(t *testing.T) {
	parent := t.TempDir()
	start := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	writeSession(t, parent, "legacy", []event{{
		Event: "request", Timestamp: start.Add(time.Hour).Format(time.RFC3339Nano), Endpoint: "/v1/responses",
		LocalModel: "coding", UpstreamModel: "us.openai.gpt-5.6-luna", Outcome: "success",
		InputTokens: int64ptr(1000), OutputTokens: int64ptr(100), CacheReadInputTokens: int64ptr(400),
	}})
	input, output := 2.0, 10.0
	report, err := Generate(Options{Directory: parent, Start: start, Stop: start.Add(24 * time.Hour), Reprice: true, Models: map[string]config.ModelConfig{
		"luna": {BedrockModelID: "us.openai.gpt-5.6-luna", InputPerMillion: &input, OutputPerMillion: &output},
	}})
	if err != nil {
		t.Fatal(err)
	}
	// 600 uncached input at $2/M, 400 cached input at derived $0.20/M, and 100 output at $10/M.
	if got, want := report.Metrics.KnownEstimatedCost, 0.00228; math.Abs(got-want) > 1e-12 {
		t.Fatalf("cost = %.8f, want %.8f", got, want)
	}
	if report.Models[0].CurrentAlias != "luna" || len(report.Models[0].Aliases) != 1 || report.Models[0].Aliases[0] != "coding" {
		t.Fatalf("model aliases = %#v", report.Models[0])
	}
}

func TestRepriceAcceptsEquivalentDerivedAndExplicitCacheRates(t *testing.T) {
	input, output, cacheRead, cacheWrite := 2.0, 10.0, 0.2, 2.5
	models := map[string]config.ModelConfig{
		"derived":  {BedrockModelID: "us.openai.gpt-5.6-luna", InputPerMillion: &input, OutputPerMillion: &output},
		"explicit": {BedrockModelID: "us.openai.gpt-5.6-luna", InputPerMillion: &input, OutputPerMillion: &output, CacheReadInputPerMillion: &cacheRead, CacheWriteInputPerMillion: &cacheWrite},
	}
	model, reason := modelForReprice(event{UpstreamModel: "us.openai.gpt-5.6-luna"}, models)
	if reason != "" || model.BedrockModelID == "" {
		t.Fatalf("model=%#v reason=%q", model, reason)
	}
}

func TestOutcomeCountsAreMutuallyExclusive(t *testing.T) {
	var metrics Metrics
	for _, outcome := range []string{"success", "canceled", "failure"} {
		applyMetrics(&metrics, event{Outcome: outcome})
	}
	if metrics.Successes != 1 || metrics.Canceled != 1 || metrics.Failures != 1 || metrics.Successes+metrics.Canceled+metrics.Failures != metrics.Requests {
		t.Fatalf("metrics=%+v", metrics)
	}
}

func TestProjectMonthEndUsesCurrentMonthCalendarDays(t *testing.T) {
	projection := projectMonthEnd([]Point{
		{Start: "2026-08-31T00:00:00Z", Requests: 99, KnownEstimatedCost: 99},
		{Start: "2026-09-11T00:00:00Z", Requests: 10, KnownEstimatedCost: 100},
		{Start: "2026-09-14T00:00:00Z", Requests: 30, KnownEstimatedCost: 300},
	})
	if projection == nil {
		t.Fatal("projection is nil")
	}
	if projection.TargetMonth != "2026-09" || projection.ObservedThrough != "2026-09-14" || projection.ElapsedDays != 14 || projection.DaysInMonth != 30 {
		t.Fatalf("projection period = %#v", projection)
	}
	if math.Abs(projection.KnownEstimatedCost-400) > 1e-12 || math.Abs(projection.ProjectedKnownEstimatedCost-400/14.0*30) > 1e-12 || projection.Requests != 40 || projection.ProjectedRequests != 86 {
		t.Fatalf("projection values = %#v", projection)
	}
}

func TestProjectMonthEndReturnsNilWithoutTrend(t *testing.T) {
	if projection := projectMonthEnd(nil); projection != nil {
		t.Fatalf("projection = %#v", projection)
	}
}

func TestHTMLRendersStaticDashboardCharts(t *testing.T) {
	report := Report{
		Metrics:           Metrics{Requests: 10, Successes: 7, Canceled: 2, Failures: 1, KnownInputTokens: 100, KnownOutputTokens: 20, KnownCacheReadInputTokens: 30, KnownCacheWriteInputTokens: 10, KnownEstimatedCost: 4},
		Series:            []Point{{Start: "2026-09-02T00:00:00Z", Requests: 10, Successes: 7, Canceled: 2, Failures: 1, KnownEstimatedCost: 4}},
		Models:            []Breakdown{{Name: "model", CurrentAlias: "coding", Requests: 10, KnownEstimatedCost: 4}},
		FailureBreakdowns: []FailureBreakdown{{Category: "transport", Endpoint: "/v1/responses", Requests: 1}},
	}
	report.Projection = projectMonthEnd(report.Series)
	contents, err := HTML(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"Estimated cost by day", "Requests by day", "Estimated cost by model", "Tokens by type", "Failed requests by cause", "<svg", "projected total"} {
		if !strings.Contains(string(contents), text) {
			t.Fatalf("HTML omitted %q", text)
		}
	}
}

func TestHTMLUsesPlainCostAndPricingLabels(t *testing.T) {
	report := Report{
		Start: "2026-09-01T00:00:00Z", Stop: "2026-10-01T00:00:00Z",
		PricingSource: "current configuration (strict repricing)",
		Metrics:       Metrics{Requests: 10, Successes: 7, Canceled: 2, Failures: 1, KnownEstimatedCost: 4, MissingCostRequests: 2},
		Projection:    &Projection{TargetMonth: "2026-09", ElapsedDays: 14, DaysInMonth: 30, ProjectedKnownEstimatedCost: 8, ProjectedRequests: 20},
	}
	contents, err := HTML(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"September 2026", "Current prices", "Request outcomes", "7 successful", "2 canceled", "Estimated cost so far", "Based on 8 of 10 requests", "Projected September cost", "Based on September 1–14"} {
		if !strings.Contains(string(contents), text) {
			t.Fatalf("HTML omitted plain label %q", text)
		}
	}
	if strings.Contains(string(contents), "strict repricing") || strings.Contains(string(contents), "(partial)") || strings.Contains(string(contents), "coverage") {
		t.Fatalf("HTML retained accounting jargon: %s", contents)
	}
}

func TestDonutLegendRendersBelowTheChart(t *testing.T) {
	chart := string(donutChart("Example", "Example shares.", "$10.00", []chartSlice{{Name: "One", Value: 6, Class: "slice-0", Text: "$6.00"}, {Name: "Two", Value: 4, Class: "slice-1", Text: "$4.00"}}))
	legend := strings.Index(chart, `<div class="donut-legend">`)
	svgEnd := strings.Index(chart, `</svg>`)
	if legend < svgEnd || !strings.Contains(chart, `class="legend-swatch slice-0"`) || !strings.Contains(chart, `class="donut-value"`) {
		t.Fatalf("donut chart does not place its legend below the SVG: %s", chart)
	}
}
