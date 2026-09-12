package reports

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGenerateAggregatesOnlyTheRequestedWindow(t *testing.T) {
	parent := t.TempDir()
	start := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	writeSession(t, parent, "session-a", []event{
		{Event: "request", Timestamp: start.Add(time.Hour).Format(time.RFC3339Nano), SessionTag: "nightly", Endpoint: "/v1/chat/completions", LocalModel: "coding", Outcome: "success", InputTokens: int64ptr(10), OutputTokens: int64ptr(20), CacheReadInputTokens: int64ptr(4), CacheWriteInputTokens: int64ptr(5), EstimatedCost: float64ptr(0.03), UsageStatus: "known", CostStatus: "estimated", ClientFamily: "codex", ClientVersion: "0.142.5", MetadataProfile: "profile", MetadataRevision: "r1", CatalogHash: "catalog", ContextUtilization: float64ptr(0.1), OutputUtilization: float64ptr(0.2), FunctionToolCalls: 2},
		{Event: "request", Timestamp: start.Add(2 * time.Hour).Format(time.RFC3339Nano), SessionTag: "nightly", Endpoint: "/v1/messages", LocalModel: "coding", Outcome: "canceled", UsageStatus: "unknown", CostStatus: "unavailable", ClientFamily: "claude-code", ClientVersion: "2.1.242", SettingsHash: "claude-settings"},
		{Event: "request", Timestamp: start.Add(3 * time.Hour).Format(time.RFC3339Nano), SessionTag: "nightly", Endpoint: "/v1/models/coding", Outcome: "success"},
	})
	writeSession(t, parent, "session-b", []event{
		{Event: "request", Timestamp: start.Add(25 * time.Hour).Format(time.RFC3339Nano), Endpoint: "/v1/responses", LocalModel: "fast", Outcome: "failure", InputTokens: int64ptr(1), OutputTokens: int64ptr(2), EstimatedCost: float64ptr(0.02), UsageStatus: "known", CostStatus: "estimated", UnsupportedFeatureRejections: 1},
		{Event: "request", Timestamp: start.Add(49 * time.Hour).Format(time.RFC3339Nano), Endpoint: "/v1/responses", LocalModel: "fast", Outcome: "success"}, // stop is exclusive
	})
	if err := os.Mkdir(filepath.Join(parent, "unknown"), 0o700); err != nil {
		t.Fatal(err)
	}

	report, err := Generate(Options{Directory: parent, Start: start, Stop: start.Add(49 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if report.Sessions != 2 || report.SkippedSessions != 0 {
		t.Fatalf("sessions=%d skipped=%d", report.Sessions, report.SkippedSessions)
	}
	metrics := report.Metrics
	if metrics.Requests != 3 || metrics.Successes != 1 || metrics.Failures != 2 || metrics.Canceled != 1 || metrics.ModelListEvents != 1 {
		t.Fatalf("metrics = %+v", metrics)
	}
	if metrics.KnownInputTokens != 11 || metrics.KnownOutputTokens != 22 || metrics.KnownEstimatedCost != 0.05 || metrics.MissingUsageRequests != 1 || metrics.MissingCostRequests != 1 {
		t.Fatalf("cost and token metrics = %+v", metrics)
	}
	if metrics.KnownCacheReadInputTokens != 4 || metrics.KnownCacheWriteInputTokens != 5 {
		t.Fatalf("cache token metrics = %+v", metrics)
	}
	if metrics.FunctionToolCalls != 2 || metrics.UnsupportedFeatureRejections != 1 || metrics.AverageContextUtilization != 0.1 || metrics.AverageOutputUtilization != 0.2 || metrics.ContextUtilizationSamples != 1 || metrics.OutputUtilizationSamples != 1 {
		t.Fatalf("compatibility metrics = %+v", metrics)
	}
	if len(report.Series) != 2 || len(report.Models) != 2 || len(report.Endpoints) != 3 || len(report.SessionTags) != 2 || len(report.Clients) != 3 || len(report.MetadataProfiles) != 2 || len(report.Catalogs) != 2 || len(report.ClaudeSettings) != 2 {
		t.Fatalf("breakdowns series=%d models=%d endpoints=%d tags=%d", len(report.Series), len(report.Models), len(report.Endpoints), len(report.SessionTags))
	}
	html, err := HTML(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"Usage report", "Request activity", "Estimated spend", "Cost and usage by model", "Cost and usage by session tag", "Endpoints", "Clients", "Compatibility details", "Metadata profiles", "Codex catalogs", "Claude settings", "claude-settings", "nightly", "$0.050 partial", "4 / 5", "10.0% / 20.0%"} {
		if !strings.Contains(string(html), text) {
			t.Fatalf("HTML does not contain %q", text)
		}
	}
}

func TestHTMLShowsUnavailableInsteadOfZeroWhenNoCostsAreKnown(t *testing.T) {
	report := Report{
		Metrics: Metrics{Requests: 1, MissingCostRequests: 1},
		Models:  []Breakdown{{Name: "coding", Requests: 1, MissingCostRequests: 1}},
	}
	html, err := HTML(report)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(html), "unavailable") || !strings.Contains(string(html), "Cost coverage is incomplete") || strings.Contains(string(html), "$0.0000") || strings.Contains(string(html), "$0.000000") {
		t.Fatalf("missing estimates rendered as zero: %s", html)
	}
}

func TestHTMLExplainsAnEmptyPeriod(t *testing.T) {
	report := Report{Start: "2026-09-01T00:00:00Z", Stop: "2026-09-02T00:00:00Z", GeneratedAt: "2026-09-02T00:00:01Z", ReportDirectory: "/private/reports"}
	html, err := HTML(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"No generation requests", "No request activity in this period", "No estimated spend in this period", "n/a cost coverage"} {
		if !strings.Contains(string(html), text) {
			t.Fatalf("empty dashboard does not contain %q: %s", text, html)
		}
	}
}

func TestGenerateRejectsInvalidWindow(t *testing.T) {
	now := time.Now().UTC()
	if _, err := Generate(Options{Directory: t.TempDir(), Start: now, Stop: now}); err == nil {
		t.Fatal("invalid window unexpectedly succeeded")
	}
}

func TestGenerateAllowsAnEmptyReportDirectory(t *testing.T) {
	start := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	report, err := Generate(Options{Directory: filepath.Join(t.TempDir(), "missing"), Start: start, Stop: start.Add(24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if report.Metrics.Requests != 0 || report.Sessions != 0 {
		t.Fatalf("empty report = %+v", report)
	}
}

func writeSession(t *testing.T, parent, id string, events []event) {
	t.Helper()
	directory := filepath.Join(parent, id)
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(manifest{SchemaVersion: schemaVersion, SessionID: id, ReportPath: directory})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "session.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(directory, "events.jsonl"), os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range events {
		data, err := json.Marshal(item)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(append(data, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func int64ptr(value int64) *int64       { return &value }
func float64ptr(value float64) *float64 { return &value }
