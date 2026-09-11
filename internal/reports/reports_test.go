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
		{Event: "request", Timestamp: start.Add(time.Hour).Format(time.RFC3339Nano), SessionTag: "nightly", Endpoint: "/v1/chat/completions", LocalModel: "coding", Outcome: "success", InputTokens: int64ptr(10), OutputTokens: int64ptr(20), EstimatedCost: float64ptr(0.03), UsageStatus: "known", CostStatus: "estimated"},
		{Event: "request", Timestamp: start.Add(2 * time.Hour).Format(time.RFC3339Nano), SessionTag: "nightly", Endpoint: "/v1/messages", LocalModel: "coding", Outcome: "canceled", UsageStatus: "unknown", CostStatus: "unavailable"},
		{Event: "request", Timestamp: start.Add(3 * time.Hour).Format(time.RFC3339Nano), SessionTag: "nightly", Endpoint: "/v1/models", Outcome: "success"},
	})
	writeSession(t, parent, "session-b", []event{
		{Event: "request", Timestamp: start.Add(25 * time.Hour).Format(time.RFC3339Nano), Endpoint: "/v1/responses", LocalModel: "fast", Outcome: "failure", InputTokens: int64ptr(1), OutputTokens: int64ptr(2), EstimatedCost: float64ptr(0.02), UsageStatus: "known", CostStatus: "estimated"},
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
	if len(report.Series) != 2 || len(report.Models) != 2 || len(report.Endpoints) != 3 || len(report.SessionTags) != 2 {
		t.Fatalf("breakdowns series=%d models=%d endpoints=%d tags=%d", len(report.Series), len(report.Models), len(report.Endpoints), len(report.SessionTags))
	}
	html, err := HTML(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"usage report", "Requests over time", "Estimated cost over time", "Model breakdown", "nightly"} {
		if !strings.Contains(string(html), text) {
			t.Fatalf("HTML does not contain %q", text)
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
