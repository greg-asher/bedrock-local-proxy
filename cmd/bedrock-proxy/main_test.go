package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/gregasher/bedrock-local-proxy/internal/accounting"
	"github.com/gregasher/bedrock-local-proxy/internal/config"
)

func TestVersionHonorsJSONFormatWithoutConfig(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"--version", "--log-format", "json"}, &out, &errOut); code != 0 {
		t.Fatalf("run() code = %d, stderr = %s", code, errOut.String())
	}
	var record map[string]any
	if err := json.Unmarshal(out.Bytes(), &record); err != nil {
		t.Fatalf("version output is not JSON: %v (%q)", err, out.String())
	}
	if record["event"] != "version" || record["version"] != version {
		t.Fatalf("unexpected version record: %#v", record)
	}
}

func TestFlagErrorsHonorRequestedJSONFormat(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"--log-format", "json", "--unknown"}, &out, &errOut); code != 2 {
		t.Fatalf("run() code = %d, want 2", code)
	}
	var record map[string]any
	if err := json.Unmarshal(errOut.Bytes(), &record); err != nil {
		t.Fatalf("flag diagnostic is not JSON: %v (%q)", err, errOut.String())
	}
	if record["event"] != "error" {
		t.Fatalf("unexpected flag diagnostic: %#v", record)
	}
}

func TestInvalidLogFormatUsesHumanDiagnostic(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"--log-format", "yaml"}, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "must be text or json") {
		t.Fatalf("invalid format code=%d stderr=%q", code, errOut.String())
	}
}

func TestRunUsesExplicitConfigPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "explicit.yaml")
	var out, errOut bytes.Buffer
	if code := run([]string{"--config", missing}, &out, &errOut); code == 0 || !strings.Contains(errOut.String(), missing) {
		t.Fatalf("explicit config code=%d stderr=%q", code, errOut.String())
	}
}

func TestRunUsesHomeDefaultConfigPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var out, errOut bytes.Buffer
	if code := run(nil, &out, &errOut); code == 0 || !strings.Contains(errOut.String(), filepath.Join(home, ".config", "bedrock-proxy", "config.yaml")) {
		t.Fatalf("default config code=%d stderr=%q", code, errOut.String())
	}
}

func TestStartupJSONIsMachineReadable(t *testing.T) {
	path := writeTestConfig(t, "127.0.0.1:0")
	cmd, _, line := startHelperWith(t, path, "json")
	defer cmd.Process.Kill()
	var record map[string]any
	if err := json.Unmarshal([]byte(line), &record); err != nil {
		t.Fatalf("startup output is not JSON: %v (%q)", err, line)
	}
	listen, ok := record["listen"].(string)
	reportPath, reportOK := record["report_path"].(string)
	if record["event"] != "startup" || !ok || !strings.Contains(listen, "/v1") || !reportOK || !filepath.IsAbs(reportPath) || record["session_tag"] != "startup-test" {
		t.Fatalf("unexpected startup record: %#v", record)
	}
}

func TestStartupTextShowsSessionReportAndTag(t *testing.T) {
	var output bytes.Buffer
	cfg := config.Config{AWS: config.AWSConfig{Profile: "YOUR_AWS_PROFILE", Region: "us-east-2"}, Models: map[string]config.ModelConfig{"coding": {BedrockModelID: "model-a"}}}
	if err := startup(&output, "text", cfg, "127.0.0.1:8787", accounting.SessionInfo{Directory: "/tmp/report", SessionTag: "ingest"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Session report: /tmp/report") || !strings.Contains(output.String(), "Session tag: ingest") {
		t.Fatalf("startup text = %q", output.String())
	}
}

func TestRunRejectsDuplicateOrInvalidSessionTag(t *testing.T) {
	for _, args := range [][]string{
		{"--session-tag", "one", "--session-tag", "two"},
		{"--session-tag", " \t "},
	} {
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "session-tag") {
			t.Fatalf("run(%q) code=%d stderr=%q", args, code, errOut.String())
		}
	}
}

func TestReportCommandWritesPeriodHTMLWithoutConfig(t *testing.T) {
	parent := t.TempDir()
	session := filepath.Join(parent, "session-a")
	if err := os.Mkdir(session, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(map[string]any{"schema_version": 1, "session_id": "session-a", "report_path": session})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(session, "session.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	events := `{"event":"request","timestamp":"2026-09-01T12:00:00Z","session_tag":"nightly","endpoint":"/v1/chat/completions","local_model":"coding","outcome":"success","input_tokens":5,"output_tokens":8,"estimated_cost":0.02,"usage_status":"known","cost_status":"estimated"}` + "\n"
	if err := os.WriteFile(filepath.Join(session, "events.jsonl"), []byte(events), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "usage.html")
	var stdout, stderr bytes.Buffer
	code := run([]string{"report", "--report-dir", parent, "--start", "2026-09-01T00:00:00Z", "--stop", "2026-09-02T00:00:00Z", "--output", output}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("report command code=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Report: "+output) || !strings.Contains(stdout.String(), "Requests: 1") {
		t.Fatalf("report stdout=%q", stdout.String())
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "Requests over time") || !strings.Contains(string(data), "nightly") {
		t.Fatalf("report HTML missing metrics: %s", data)
	}
	info, err := os.Stat(output)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("report permissions=%v err=%v", info.Mode(), err)
	}
}

func TestRunRejectsNonLoopbackConfigWithoutAWS(t *testing.T) {
	path := writeTestConfig(t, "0.0.0.0:8787")
	var out, errOut bytes.Buffer
	if code := run([]string{"--config", path}, &out, &errOut); code == 0 || !strings.Contains(errOut.String(), "not loopback") {
		t.Fatalf("non-loopback code=%d stderr=%q", code, errOut.String())
	}
}

func TestRunRejectsOccupiedPort(t *testing.T) {
	path := writeTestConfig(t, "127.0.0.1:0")
	// A first process owns an ephemeral port; a second config points at it.
	first, _, listening := startHelper(t, path)
	defer first.Process.Kill()
	line := listening
	port := line[strings.LastIndex(line, ":")+1 : strings.LastIndex(line, "/v1")]
	occupied := writeTestConfig(t, "127.0.0.1:"+port)
	reportParent := t.TempDir()
	var out, errOut bytes.Buffer
	if code := run([]string{"--config", occupied, "--report-dir", reportParent}, &out, &errOut); code == 0 || !strings.Contains(errOut.String(), "listen") {
		t.Fatalf("occupied port code=%d stderr=%q", code, errOut.String())
	}
	entries, err := os.ReadDir(reportParent)
	if err != nil || len(entries) != 1 {
		t.Fatalf("listener failure report entries=%v err=%v", entries, err)
	}
	data, err := os.ReadFile(filepath.Join(reportParent, entries[0].Name(), "session.json"))
	if err != nil || !strings.Contains(string(data), "startup_failed") {
		t.Fatalf("listener failure manifest=%q err=%v", data, err)
	}
}

func TestRunShutdownViaSIGTERM(t *testing.T) {
	path := writeTestConfig(t, "127.0.0.1:0")
	reportParent := t.TempDir()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "BEDROCK_PROXY_HELPER=1", "BEDROCK_PROXY_CONFIG="+path, "BEDROCK_PROXY_REPORT_DIR="+reportParent, "BEDROCK_PROXY_SESSION_TAG=term-test")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	if _, err := bufio.NewReader(stdout).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper did not shut down cleanly: %v", err)
	}
	entries, err := os.ReadDir(reportParent)
	if err != nil || len(entries) != 1 {
		t.Fatalf("report entries=%v err=%v", entries, err)
	}
	data, err := os.ReadFile(filepath.Join(reportParent, entries[0].Name(), "summary.json"))
	if err != nil || !strings.Contains(string(data), `"session_tag": "term-test"`) {
		t.Fatalf("summary=%q err=%v", data, err)
	}
}

func TestRunShutdownViaSIGINT(t *testing.T) {
	path := writeTestConfig(t, "127.0.0.1:0")
	reportParent := t.TempDir()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "BEDROCK_PROXY_HELPER=1", "BEDROCK_PROXY_CONFIG="+path, "BEDROCK_PROXY_REPORT_DIR="+reportParent)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	if _, err := bufio.NewReader(stdout).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper did not shut down cleanly: %v", err)
	}
}

func TestMain(m *testing.M) {
	if os.Getenv("BEDROCK_PROXY_HELPER") != "1" {
		os.Exit(m.Run())
	}
	args := []string{}
	if path := os.Getenv("BEDROCK_PROXY_CONFIG"); path != "" {
		args = append(args, "--config", path)
	}
	if format := os.Getenv("BEDROCK_PROXY_FORMAT"); format != "" {
		args = append(args, "--log-format", format)
	}
	if reportDir := os.Getenv("BEDROCK_PROXY_REPORT_DIR"); reportDir != "" {
		args = append(args, "--report-dir", reportDir)
	}
	if sessionTag := os.Getenv("BEDROCK_PROXY_SESSION_TAG"); sessionTag != "" {
		args = append(args, "--session-tag", sessionTag)
	}
	os.Exit(run(args, os.Stdout, os.Stderr))
}

func writeTestConfig(t *testing.T, listen string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := "version: 1\naws:\n  profile: YOUR_AWS_PROFILE\n  region: us-east-2\nlisten: " + listen + "\nmodels:\n  coding:\n    bedrock_model_id: model-a\n"
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func startHelper(t *testing.T, path string) (*exec.Cmd, *bufio.Reader, string) {
	return startHelperWith(t, path, "text")
}

func startHelperWith(t *testing.T, path, format string) (*exec.Cmd, *bufio.Reader, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	reportParent := t.TempDir()
	cmd.Env = append(os.Environ(), "BEDROCK_PROXY_HELPER=1", "BEDROCK_PROXY_CONFIG="+path, "BEDROCK_PROXY_FORMAT="+format, "BEDROCK_PROXY_REPORT_DIR="+reportParent, "BEDROCK_PROXY_SESSION_TAG=startup-test")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(stdout)
	var listening string
	if format == "json" {
		line, err := reader.ReadString('\n')
		if err != nil {
			cmd.Process.Kill()
			t.Fatal(err)
		}
		return cmd, reader, line
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			cmd.Process.Kill()
			t.Fatal(err)
		}
		if strings.Contains(line, "Listening:") {
			listening = line
			break
		}
	}
	return cmd, reader, listening
}
