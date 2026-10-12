package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greg-asher/bedrock-local-proxy/internal/config"
	"github.com/greg-asher/bedrock-local-proxy/internal/server"
)

const presetTestConfig = `version: 1
aws:
  profile: offline
  region: us-east-2
listen: 127.0.0.1:8787

# the shared catalog, written once
models:
  luna:
    display_name: Luna
    bedrock_model_id: A
  fable:
    display_name: Fable
    bedrock_model_id: B
`

func writePresetConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPresetCreateScriptedWritesTheConfig(t *testing.T) {
	path := writePresetConfig(t, presetTestConfig)
	var stdout, stderr bytes.Buffer
	code := run([]string{"preset", "create", "--config", path, "--name", "cheap", "--models", "luna", "--session-tag", "cheap"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("the written configuration must load: %v", err)
	}
	preset, ok := cfg.Presets["cheap"]
	if !ok {
		t.Fatalf("preset missing; presets = %v", cfg.Presets)
	}
	// The scan proposes the first free port above the top-level listen.
	if preset.Listen != "127.0.0.1:8788" {
		t.Fatalf("listen = %q, want the scanned 127.0.0.1:8788", preset.Listen)
	}
	if preset.SessionTag != "cheap" || strings.Join(preset.Models, ",") != "luna" {
		t.Fatalf("preset = %+v", preset)
	}
	// Comments in the operator's file survive the insert.
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), "# the shared catalog, written once") {
		t.Fatalf("the original comment must survive:\n%s", written)
	}
	if !strings.Contains(stdout.String(), "configure codex --preset cheap") {
		t.Fatalf("stdout should name the follow-up commands:\n%s", stdout.String())
	}
}

func TestPresetCreateDoesNotTagSilentlyWhenScripted(t *testing.T) {
	path := writePresetConfig(t, presetTestConfig)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"preset", "create", "--config", path, "--name", "cheap"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if tag := cfg.Presets["cheap"].SessionTag; tag != "" {
		t.Fatalf("session tag = %q; off a terminal there is no prompt to see, so nothing may be applied by default", tag)
	}
}

func TestPresetCreateRequiresNameWhenNotATerminal(t *testing.T) {
	path := writePresetConfig(t, presetTestConfig)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"preset", "create", "--config", path}, &stdout, &stderr); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "--name is required when input is not a terminal") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestPresetCreatePromptsWhenInteractive(t *testing.T) {
	path := writePresetConfig(t, presetTestConfig)
	// name, port (accept the scanned default), models, session tag.
	answers := strings.NewReader("work\n\nluna, fable\nteam-work\n")
	var stdout, stderr bytes.Buffer
	if code := runWith([]string{"preset", "create", "--config", path}, answers, &stdout, &stderr, true); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "luna (Luna)") {
		t.Fatalf("the model prompt should list the catalog with display names:\n%s", stdout.String())
	}
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	preset := cfg.Presets["work"]
	if preset.Listen != "127.0.0.1:8788" || preset.SessionTag != "team-work" {
		t.Fatalf("preset = %+v", preset)
	}
	if strings.Join(preset.Models, ",") != "luna,fable" {
		t.Fatalf("models = %v", preset.Models)
	}
}

func TestPresetCreateRejectsADuplicateName(t *testing.T) {
	path := writePresetConfig(t, presetTestConfig+"presets:\n  work:\n    listen: 127.0.0.1:8788\n")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"preset", "create", "--config", path, "--name", "work"}, &stdout, &stderr); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "already exists") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestPresetCreateDeclinesAFlowMappingAndLeavesTheFileAlone(t *testing.T) {
	body := presetTestConfig + "presets: {work: {listen: \"127.0.0.1:8788\"}}\n"
	path := writePresetConfig(t, body)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"preset", "create", "--config", path, "--name", "cheap", "--models", "luna"}, &stdout, &stderr); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "will not edit safely") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if !strings.Contains(stdout.String(), "cheap:") {
		t.Fatalf("the block should be printed for pasting:\n%s", stdout.String())
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != body {
		t.Fatalf("a declined insert must leave the file byte-identical:\n%s", after)
	}
}

func TestPresetCreateDryRunWritesNothing(t *testing.T) {
	path := writePresetConfig(t, presetTestConfig)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"preset", "create", "--config", path, "--name", "cheap", "--models", "luna", "--dry-run"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != presetTestConfig {
		t.Fatalf("--dry-run must not write:\n%s", after)
	}
	if !strings.Contains(stdout.String(), "Would add preset") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestPresetCreateRejectsAnInvalidName(t *testing.T) {
	path := writePresetConfig(t, presetTestConfig)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"preset", "create", "--config", path, "--name", "../escape"}, &stdout, &stderr); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "must start with a lowercase letter") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestPresetCreateRequiresTheCreateSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"preset"}, &stdout, &stderr); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "preset requires the create subcommand") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestUnknownPresetOnTheProxyListsTheConfiguredOnes(t *testing.T) {
	path := writePresetConfig(t, presetTestConfig+"presets:\n  work:\n    listen: 127.0.0.1:8788\n")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--config", path, "--preset", "missing"}, &stdout, &stderr); code == 0 {
		t.Fatal("an unknown preset must fail")
	}
	if !strings.Contains(stderr.String(), "configured presets: work") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

// A preset narrows the effective model map, so the existing model_not_found
// path refuses a blocked alias with no new code in internal/server.
func TestPresetNarrowsTheServedModels(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	body := "version: 1\naws:\n  profile: offline\n  region: us-east-2\nlisten: 127.0.0.1:0\nmodels:\n  luna:\n    bedrock_model_id: A\n  fable:\n    bedrock_model_id: B\npresets:\n  cheap:\n    listen: 127.0.0.1:8799\n    session_tag: cheap\n    models: [luna]\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	resolved, err := cfg.Resolve("cheap")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	srv := server.New(resolved)
	listing := httptest.NewRecorder()
	srv.Handler().ServeHTTP(listing, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if body := listing.Body.String(); !strings.Contains(body, "luna") || strings.Contains(body, "fable") {
		t.Fatalf("/v1/models must list only this preset's models: %s", body)
	}
	// Every generation endpoint refuses a blocked alias locally, before any
	// AWS request, and the message names what this preset does allow.
	for _, endpoint := range []struct{ path, body string }{
		{"/v1/chat/completions", `{"model":"fable","messages":[{"role":"user","content":"hi"}]}`},
		{"/v1/responses", `{"model":"fable","input":"hi"}`},
		{"/v1/messages", `{"model":"fable","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`},
	} {
		blocked := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, endpoint.path, strings.NewReader(endpoint.body))
		request.Header.Set("Content-Type", "application/json")
		srv.Handler().ServeHTTP(blocked, request)
		if blocked.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400; body = %s", endpoint.path, blocked.Code, blocked.Body.String())
		}
		if !strings.Contains(blocked.Body.String(), "unknown model") || !strings.Contains(blocked.Body.String(), "fable") || !strings.Contains(blocked.Body.String(), "configured models: luna") {
			t.Fatalf("%s: body = %s", endpoint.path, blocked.Body.String())
		}
	}
	// The single-model lookup route keeps its own 404 shape.
	missing := httptest.NewRecorder()
	srv.Handler().ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/v1/models/fable", nil))
	if missing.Code != http.StatusNotFound || !strings.Contains(missing.Body.String(), "model_not_found") {
		t.Fatalf("/v1/models/fable: status = %d body = %s", missing.Code, missing.Body.String())
	}
}
