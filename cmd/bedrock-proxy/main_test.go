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

func TestMain(m *testing.M) {
	if os.Getenv("BEDROCK_PROXY_HELPER") != "1" {
		os.Exit(m.Run())
	}
	os.Exit(run([]string{"--config", os.Getenv("BEDROCK_PROXY_CONFIG")}, os.Stdout, os.Stderr))
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
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "BEDROCK_PROXY_HELPER=1", "BEDROCK_PROXY_CONFIG="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(stdout)
	var listening string
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
