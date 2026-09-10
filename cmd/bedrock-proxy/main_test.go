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
	if record["event"] != "startup" || !ok || !strings.Contains(listen, "/v1") {
		t.Fatalf("unexpected startup record: %#v", record)
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
	var out, errOut bytes.Buffer
	if code := run([]string{"--config", occupied}, &out, &errOut); code == 0 || !strings.Contains(errOut.String(), "listen") {
		t.Fatalf("occupied port code=%d stderr=%q", code, errOut.String())
	}
}

func TestRunShutdownViaSIGTERM(t *testing.T) {
	path := writeTestConfig(t, "127.0.0.1:0")
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "BEDROCK_PROXY_HELPER=1", "BEDROCK_PROXY_CONFIG="+path)
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
}

func TestRunShutdownViaSIGINT(t *testing.T) {
	path := writeTestConfig(t, "127.0.0.1:0")
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "BEDROCK_PROXY_HELPER=1", "BEDROCK_PROXY_CONFIG="+path)
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
	os.Exit(run(args, os.Stdout, os.Stderr))
}

func writeTestConfig(t *testing.T, listen string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := "version: 1\naws:\n  profile: Halo-Win-Agent-Execution\n  region: us-east-2\nlisten: " + listen + "\nmodels:\n  coding:\n    bedrock_model_id: model-a\n"
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
	cmd.Env = append(os.Environ(), "BEDROCK_PROXY_HELPER=1", "BEDROCK_PROXY_CONFIG="+path, "BEDROCK_PROXY_FORMAT="+format)
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
