package claude

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gregasher/bedrock-local-proxy/internal/config"
)

type testRunner struct {
	version string
	err     error
}

func (r testRunner) Run(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
	if r.err != nil {
		return nil, []byte("missing Claude CLI"), r.err
	}
	if len(args) == 1 && args[0] == "--version" {
		return []byte(r.version + " (Claude Code)\n"), nil, nil
	}
	if len(args) == 3 && args[0] == "--settings" && args[2] == "--version" {
		if _, err := os.ReadFile(args[1]); err != nil {
			return nil, nil, err
		}
		return []byte(r.version + " (Claude Code)\n"), nil, nil
	}
	return nil, []byte("unexpected arguments"), errors.New("unexpected arguments")
}

func TestGenerateWritesCompleteDeterministicSettings(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "nested", "settings.json")
	cfg := multiModelConfig()
	result, err := Generate(context.Background(), GenerateOptions{
		Config: cfg, ConfigPath: filepath.Join(root, "config.yaml"), SettingsPath: path,
		Runner: testRunner{version: MinimumVersion},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ModelAlias != "fable" || result.SubagentAlias != "sonnet" || strings.Join(result.ModelAliases, ",") != "fable,sonnet" {
		t.Fatalf("result = %+v", result)
	}
	if len(result.SkippedModels) != 1 || result.SkippedModels[0].Alias != "responses-only" {
		t.Fatalf("skipped = %+v", result.SkippedModels)
	}
	settings, hash, err := Inspect(path)
	if err != nil {
		t.Fatal(err)
	}
	if hash != result.SettingsHash || settings.Model != "claude-fable-5" || settings.Env["ANTHROPIC_DEFAULT_MODEL"] != "claude-fable-5" || settings.Env["CLAUDE_CODE_SUBAGENT_MODEL"] != "claude-sonnet-5" {
		t.Fatalf("settings=%+v hash=%q result=%+v", settings, hash, result)
	}
	if settings.ModelOverrides["claude-fable-5"] != "fable" || settings.ModelOverrides["claude-sonnet-5"] != "sonnet" || !settings.ModelPicker.ReplaceBuiltInOptions || len(settings.ModelPicker.Options) != 2 {
		t.Fatalf("picker or overrides = %+v", settings)
	}
	if settings.Env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:8787" || settings.Env["ANTHROPIC_API_KEY"] != "local" || settings.Env["ANTHROPIC_CUSTOM_HEADERS"] != SettingsHeader+": "+hash {
		t.Fatalf("environment = %+v", settings.Env)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("permissions=%v err=%v", info.Mode(), err)
	}
	first, _ := os.ReadFile(path)
	result2, err := Generate(context.Background(), GenerateOptions{Config: cfg, ConfigPath: filepath.Join(root, "config.yaml"), SettingsPath: path, Runner: testRunner{version: MinimumVersion}})
	if err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(path)
	if result.SettingsHash != result2.SettingsHash || string(first) != string(second) {
		t.Fatal("repeated generation changed settings bytes or hash")
	}
}

func TestGenerateDefaultOverrideChangesOnlySelectionFields(t *testing.T) {
	root := t.TempDir()
	cfg := multiModelConfig()
	one, err := Generate(context.Background(), GenerateOptions{Config: cfg, ConfigPath: filepath.Join(root, "config.yaml"), SettingsPath: filepath.Join(root, "one.json"), Runner: testRunner{version: MinimumVersion}})
	if err != nil {
		t.Fatal(err)
	}
	two, err := Generate(context.Background(), GenerateOptions{Config: cfg, ConfigPath: filepath.Join(root, "config.yaml"), ModelAlias: "sonnet", SettingsPath: filepath.Join(root, "two.json"), Runner: testRunner{version: MinimumVersion}})
	if err != nil {
		t.Fatal(err)
	}
	if one.SettingsHash == two.SettingsHash || two.ModelAlias != "sonnet" || two.SubagentAlias != "sonnet" {
		t.Fatalf("default override results: one=%+v two=%+v", one, two)
	}
	a, _, _ := Inspect(one.SettingsPath)
	b, _, _ := Inspect(two.SettingsPath)
	if strings.Join(modelOptionIDs(a.ModelPicker.Options), ",") != strings.Join(modelOptionIDs(b.ModelPicker.Options), ",") || len(a.ModelOverrides) != len(b.ModelOverrides) {
		t.Fatalf("default override changed picker set: a=%+v b=%+v", a, b)
	}
}

func TestResolveModelsSelectionAndValidation(t *testing.T) {
	cfg := multiModelConfig()
	if got, err := ResolveModels(cfg, "sonnet"); err != nil || got.DefaultAlias != "sonnet" {
		t.Fatalf("override result=%+v err=%v", got, err)
	}

	collision := multiModelConfig()
	model := collision.Models["sonnet"]
	model.Capabilities.AnthropicModelID = "claude-fable-5"
	collision.Models["sonnet"] = model
	if _, err := ResolveModels(collision, ""); err == nil || !strings.Contains(err.Error(), "same anthropic_model_id") {
		t.Fatalf("collision error = %v", err)
	}

	ineligible := multiModelConfig()
	ineligible.Claude.DefaultModel = "responses-only"
	if _, err := ResolveModels(ineligible, ""); err == nil || !strings.Contains(err.Error(), "not Claude-compatible") {
		t.Fatalf("ineligible default error = %v", err)
	}

	partial := multiModelConfig()
	broken := partial.Models["fable"]
	broken.Capabilities.ContextWindow = nil
	partial.Models["fable"] = broken
	if _, err := ResolveModels(partial, ""); err == nil || !strings.Contains(err.Error(), "missing capability metadata") {
		t.Fatalf("partial metadata error = %v", err)
	}
}

func TestProbeVersionBoundaryAndFailures(t *testing.T) {
	if version, err := Probe(context.Background(), "claude", testRunner{version: "2.1.242"}); err != nil || version != "2.1.242" {
		t.Fatalf("minimum version=%q err=%v", version, err)
	}
	if _, err := Probe(context.Background(), "claude", testRunner{version: "2.1.241"}); err == nil || !strings.Contains(err.Error(), "too old") {
		t.Fatalf("old version error = %v", err)
	}
	if _, err := Probe(context.Background(), "claude", testRunner{version: "not-a-version"}); err == nil || !strings.Contains(err.Error(), "unexpected output") {
		t.Fatalf("malformed version error = %v", err)
	}
	if _, err := Probe(context.Background(), "claude", testRunner{err: errors.New("not found")}); err == nil || !strings.Contains(err.Error(), "missing Claude CLI") {
		t.Fatalf("missing CLI error = %v", err)
	}
}

func TestValidateSettingsDetectsDriftAndExtraJSON(t *testing.T) {
	root := t.TempDir()
	cfg := multiModelConfig()
	result, err := Generate(context.Background(), GenerateOptions{Config: cfg, ConfigPath: filepath.Join(root, "config.yaml"), Runner: testRunner{version: MinimumVersion}})
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ := ResolveModels(cfg, "")
	if _, err := ValidateSettings(result.SettingsPath, cfg.Listen, resolved); err != nil {
		t.Fatal(err)
	}
	changed := multiModelConfig()
	changed.Listen = "127.0.0.1:9999"
	if _, err := ValidateSettings(result.SettingsPath, changed.Listen, resolved); err == nil || !strings.Contains(err.Error(), "do not match") {
		t.Fatalf("drift error = %v", err)
	}
	data, _ := os.ReadFile(result.SettingsPath)
	if err := os.WriteFile(result.SettingsPath, append(data, []byte("{}")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Inspect(result.SettingsPath); err == nil || !strings.Contains(err.Error(), "multiple JSON values") {
		t.Fatalf("extra JSON error = %v", err)
	}
}

func TestDefaultSettingsPathUsesConfigDirectory(t *testing.T) {
	path, err := DefaultSettingsPath(filepath.Join(t.TempDir(), "custom", "proxy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, filepath.Join("custom", "clients", "claude", "settings.json")) {
		t.Fatalf("path = %q", path)
	}
}

func multiModelConfig() config.Config {
	cfg := config.Config{
		Version: 1, Listen: "127.0.0.1:8787", AWS: config.AWSConfig{Profile: "offline", Region: "us-east-2"},
		Claude: config.ClaudeConfig{DefaultModel: "fable", SubagentModel: "sonnet"},
		Models: map[string]config.ModelConfig{},
	}
	cfg.Models["fable"] = claudeModel("Fable", "target-fable", "claude-fable-5")
	cfg.Models["sonnet"] = claudeModel("Sonnet", "target-sonnet", "claude-sonnet-5")
	responsesOnly := claudeModel("Responses only", "target-responses", "")
	noMessages := false
	responsesOnly.Capabilities.MessagesAPI = &noMessages
	cfg.Models["responses-only"] = responsesOnly
	return cfg
}

func claudeModel(display, target, canonical string) config.ModelConfig {
	contextWindow, output := int64(200000), int64(64000)
	messages, functions, parallel, reasoning := true, true, true, true
	model := config.ModelConfig{
		DisplayName: display, BedrockModelID: target,
		Capabilities: &config.CapabilityConfig{
			MessagesAPI: &messages, AnthropicModelID: canonical,
			ContextWindow: &contextWindow, MaxOutputTokens: &output, InputModalities: []string{"text", "image"},
			Reasoning: config.ReasoningCapability{Supported: &reasoning, Efforts: []string{"low", "medium", "high"}},
			Tools:     config.ToolCapability{FunctionCalling: &functions, ParallelCalls: &parallel},
		},
	}
	return model
}

func modelOptionIDs(options []ModelOption) []string {
	result := make([]string, 0, len(options))
	for _, option := range options {
		result = append(result, option.Model)
	}
	return result
}

func TestSettingsJSONContainsNoUnknownMutableFields(t *testing.T) {
	resolved, err := ResolveModels(multiModelConfig(), "")
	if err != nil {
		t.Fatal(err)
	}
	settings, _ := buildSettings("127.0.0.1:8787", resolved)
	data, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"context_window", "max_output_tokens", "bedrock_model_id"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("generated settings contain %q: %s", forbidden, data)
		}
	}
}
