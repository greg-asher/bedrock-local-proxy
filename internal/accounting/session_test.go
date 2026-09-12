package accounting

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gregasher/bedrock-local-proxy/internal/config"
	"github.com/gregasher/bedrock-local-proxy/internal/server"
)

func TestSessionReporterWritesPrivateSafeReport(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "reports")
	reporter, err := NewSessionReporter(SessionOptions{
		ParentDirectory: parent,
		SessionTag:      "  ingest-2026-09  ",
		Version:         "test",
		RequestedListen: "127.0.0.1:0",
		Models: map[string]config.ModelConfig{
			"coding": sessionCapabilityModel(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reporter.Finalize(SessionCompleted) })
	if err := reporter.Start("http://127.0.0.1:1234/v1"); err != nil {
		t.Fatal(err)
	}
	var terminal bytes.Buffer
	inputPrice, outputPrice := 2.0, 3.0
	cacheReadPrice, cacheWritePrice := 0.2, 2.5
	recorder := New(&terminal, FormatJSON, map[string]config.ModelConfig{"coding": {
		BedrockModelID: "anthropic.claude-test", InputPerMillion: &inputPrice, OutputPerMillion: &outputPrice,
		CacheReadInputPerMillion: &cacheReadPrice, CacheWriteInputPerMillion: &cacheWritePrice,
	}})
	recorder.SetSessionReporter(reporter)
	status := 200
	input, output := int64(3), int64(4)
	cacheRead, cacheWrite := int64(1), int64(1)
	recorder.Record(server.CompletionResult{Endpoint: "/v1/chat/completions", LocalModel: "coding", UpstreamModel: "anthropic.claude-test", HTTPStatus: &status, Outcome: server.CompletionSucceeded, InputTokens: &input, OutputTokens: &output, CacheReadInputTokens: &cacheRead, CacheWriteInputTokens: &cacheWrite, InputTokensIncludeCache: true, ClientFamily: "codex", ClientVersion: "0.142.5", MetadataProfile: "test-profile", MetadataRevision: "r1", CatalogHash: strings.Repeat("a", 64), ContextWindow: 100, MaxOutputTokens: 20, FunctionToolCalls: 1})
	recorder.Record(server.CompletionResult{Endpoint: "/v1/messages", LocalModel: "coding", Outcome: server.CompletionCanceled, ClientFamily: "claude-code", ClientVersion: "2.1.242", SettingsHash: strings.Repeat("b", 64)})
	recorder.Record(server.CompletionResult{Endpoint: "/v1/models", HTTPStatus: &status, Outcome: server.CompletionSucceeded})
	recorder.WriteSummary()
	reporter.Finalize(SessionCompleted)

	info := reporter.Info()
	if info.SessionTag != "ingest-2026-09" {
		t.Fatalf("session tag = %q", info.SessionTag)
	}
	for _, name := range []string{"session.json", "events.jsonl", "summary.json"} {
		fileInfo, err := os.Stat(filepath.Join(info.Directory, name))
		if err != nil {
			t.Fatal(err)
		}
		if fileInfo.Mode().Perm() != 0o600 {
			t.Fatalf("%s permissions = %o, want 600", name, fileInfo.Mode().Perm())
		}
	}
	if temporary, err := filepath.Glob(filepath.Join(info.Directory, ".tmp-*")); err != nil || len(temporary) != 0 {
		t.Fatalf("atomic finalization left temporary files: %v, %v", temporary, err)
	}
	dirInfo, err := os.Stat(info.Directory)
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("session directory permissions = %o, want 700", dirInfo.Mode().Perm())
	}

	var manifest sessionManifest
	readJSON(t, filepath.Join(info.Directory, "session.json"), &manifest)
	if manifest.Status != SessionCompleted || manifest.SessionTag != info.SessionTag || manifest.Models[0].BedrockModelID != "anthropic.claude-test" || manifest.Models[0].ContextWindow != 100 || manifest.Models[0].MaxOutputTokens != 20 {
		t.Fatalf("manifest = %+v", manifest)
	}
	var summary summaryRecord
	readJSON(t, filepath.Join(info.Directory, "summary.json"), &summary)
	if summary.SessionTag != info.SessionTag || summary.Status != string(SessionCompleted) || summary.FinishedAt == "" || summary.Requests != 2 || summary.Successes != 1 || summary.Failures != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	if summary.KnownCacheReadInputTokens != 1 || summary.KnownCacheWriteInputTokens != 1 || math.Abs(summary.KnownEstimatedCost-0.0000167) > 1e-12 || summary.CostStatus != "partial" {
		t.Fatalf("cache-aware summary = %+v", summary)
	}
	data, err := os.ReadFile(filepath.Join(info.Directory, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 5 { // startup, 3 requests (model list included), summary
		t.Fatalf("events = %d, want 5: %s", len(lines), data)
	}
	for _, line := range lines {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event["session_tag"] != info.SessionTag {
			t.Fatalf("event tag = %#v, want %q", event["session_tag"], info.SessionTag)
		}
	}
	all := string(data)
	if !strings.Contains(all, `"client_family":"codex"`) || !strings.Contains(all, `"catalog_hash":"`+strings.Repeat("a", 64)+`"`) || !strings.Contains(all, `"client_family":"claude-code"`) || !strings.Contains(all, `"settings_hash":"`+strings.Repeat("b", 64)+`"`) || !strings.Contains(all, `"function_tool_calls":1`) || !strings.Contains(all, `"cache_read_input_tokens":1`) || !strings.Contains(all, `"cache_write_input_tokens":1`) || !strings.Contains(all, `"input_tokens_include_cache":true`) || !strings.Contains(all, `"estimated_cost":0.0000167`) {
		t.Fatalf("report omitted safe compatibility metadata: %s", all)
	}
	for _, sentinel := range []string{"prompt-secret", "completion-secret", "tool-secret", "credential-secret", "header-secret", "raw-upstream-error"} {
		if strings.Contains(all, sentinel) {
			t.Fatalf("report leaked %q", sentinel)
		}
	}
}

func sessionCapabilityModel() config.ModelConfig {
	contextWindow, maxOutput := int64(100), int64(20)
	reasoning, functions, parallel := false, true, false
	return config.ModelConfig{BedrockModelID: "anthropic.claude-test", Capabilities: &config.CapabilityConfig{
		ContextWindow: &contextWindow, MaxOutputTokens: &maxOutput, InputModalities: []string{"text"},
		Reasoning: config.ReasoningCapability{Supported: &reasoning},
		Tools:     config.ToolCapability{FunctionCalling: &functions, ParallelCalls: &parallel},
	}}
}

func TestSessionTagValidation(t *testing.T) {
	long := strings.Repeat("a", 129)
	for _, value := range []string{"", " \t ", "has\nnewline", long, string([]byte{0xff})} {
		if _, err := NormalizeSessionTag(value); err == nil {
			t.Fatalf("NormalizeSessionTag(%q) unexpectedly succeeded", value)
		}
	}
	got, err := NormalizeSessionTag("  valid tag  ")
	if err != nil || got != "valid tag" {
		t.Fatalf("NormalizeSessionTag = %q, %v", got, err)
	}
}

func TestSessionReporterOmitsTagWhenNotProvided(t *testing.T) {
	reporter, err := NewSessionReporter(SessionOptions{ParentDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var initial sessionManifest
	readJSON(t, filepath.Join(reporter.Info().Directory, "session.json"), &initial)
	if initial.Status != SessionRunning {
		t.Fatalf("unfinalized session status = %q, want running", initial.Status)
	}
	if err := reporter.Start("http://127.0.0.1:1/v1"); err != nil {
		t.Fatal(err)
	}
	reporter.Finalize(SessionCompleted)
	data, err := os.ReadFile(filepath.Join(reporter.Info().Directory, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "session_tag") {
		t.Fatalf("untagged manifest includes session_tag: %s", data)
	}
}

func TestCleanupExpiredReportsOnlyRemovesRecognizedOldSessions(t *testing.T) {
	parent := t.TempDir()
	now := time.Now().UTC()
	old := filepath.Join(parent, "old-session")
	if err := os.Mkdir(old, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := sessionManifest{SchemaVersion: reportSchemaVersion, SessionID: "old-session", ReportPath: old, StartedAt: now.Add(-31 * 24 * time.Hour).Format(time.RFC3339Nano), Status: SessionRunning}
	if err := writeJSONAtomic(filepath.Join(old, "session.json"), manifest); err != nil {
		t.Fatal(err)
	}
	unknown := filepath.Join(parent, "do-not-delete")
	if err := os.Mkdir(unknown, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unknown, "session.json"), []byte(`{"unexpected":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "linked-session")
	if err := os.Symlink(old, link); err != nil {
		t.Fatal(err)
	}
	reporter, err := NewSessionReporter(SessionOptions{ParentDirectory: parent})
	if err != nil {
		t.Fatal(err)
	}
	reporter.Finalize(SessionCompleted)
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("expired recognized session still exists: %v", err)
	}
	if _, err := os.Stat(unknown); err != nil {
		t.Fatalf("unknown directory was removed: %v", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("session symlink was removed: %v", err)
	}
}

func TestCleanupFailureIsWarning(t *testing.T) {
	if !cleanupExpiredReports(filepath.Join(t.TempDir(), "missing"), "", time.Now().UTC()) {
		t.Fatal("cleanup of unreadable parent did not request a warning")
	}
}

type reportSecretDoer struct {
	err error
}

func (d reportSecretDoer) Do(_ context.Context, _ *http.Request) (*http.Response, error) {
	if d.err != nil {
		return nil, d.err
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"id":"test","object":"chat.completion","created":1,"model":"target","choices":[{"message":{"role":"assistant","content":"completion-secret"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)),
	}, nil
}

func TestReportOmitsSecretsFromRealHandlerFlow(t *testing.T) {
	reporter, err := NewSessionReporter(SessionOptions{ParentDirectory: t.TempDir(), Models: map[string]config.ModelConfig{"coding": {BedrockModelID: "target"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := reporter.Start("http://127.0.0.1:1/v1"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Models: map[string]config.ModelConfig{"coding": {BedrockModelID: "target"}}}
	recorder := New(io.Discard, FormatJSON, cfg.Models)
	recorder.SetSessionReporter(reporter)

	success := server.NewWithTransport(cfg, reportSecretDoer{})
	success.SetCompletionRecorder(recorder.Record)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding","messages":[{"role":"user","content":"prompt-secret tool-secret"}]}`))
	request.Header.Set("Authorization", "Bearer credential-secret")
	request.Header.Set("X-Test-Header", "header-secret")
	success.ServeHTTP(httptest.NewRecorder(), request)

	failure := server.NewWithTransport(cfg, reportSecretDoer{err: errors.New("raw-upstream-error")})
	failure.SetCompletionRecorder(recorder.Record)
	failure.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding","messages":[]}`)))
	recorder.WriteSummary()
	reporter.Finalize(SessionCompleted)

	for _, name := range []string{"session.json", "events.jsonl", "summary.json"} {
		data, err := os.ReadFile(filepath.Join(reporter.Info().Directory, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, sentinel := range []string{"prompt-secret", "tool-secret", "credential-secret", "header-secret", "completion-secret", "raw-upstream-error"} {
			if strings.Contains(string(data), sentinel) {
				t.Fatalf("%s leaked %q: %s", name, sentinel, data)
			}
		}
	}
}

func readJSON(t *testing.T, path string, target any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatal(err)
	}
}
