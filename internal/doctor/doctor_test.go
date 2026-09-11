package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gregasher/bedrock-local-proxy/internal/claude"
	"github.com/gregasher/bedrock-local-proxy/internal/codex"
	"github.com/gregasher/bedrock-local-proxy/internal/config"
)

type doctorRunner struct {
	version string
	bundled []byte
}

type doctorClaudeRunner struct{ version string }

func (r doctorClaudeRunner) Run(_ context.Context, command string, args ...string) ([]byte, []byte, error) {
	if command == "" {
		return nil, []byte("empty command"), os.ErrInvalid
	}
	if len(args) == 1 && args[0] == "--version" {
		return []byte(r.version + " (Claude Code)"), nil, nil
	}
	if len(args) == 3 && args[0] == "--settings" && args[2] == "--version" {
		if _, err := os.ReadFile(args[1]); err != nil {
			return nil, nil, err
		}
		return []byte(r.version + " (Claude Code)"), nil, nil
	}
	return nil, []byte("unexpected arguments"), os.ErrInvalid
}

func (r doctorClaudeRunner) RunWithEnv(ctx context.Context, _ string, _ []string, args ...string) ([]byte, []byte, error) {
	var settingsPath string
	for index := 0; index+1 < len(args); index++ {
		if args[index] == "--settings" {
			settingsPath = args[index+1]
			break
		}
	}
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return nil, nil, err
	}
	var settings claude.Settings
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, nil, err
	}
	body, _ := json.Marshal(map[string]any{"model": settings.ModelOverrides[settings.Model], "max_tokens": 1, "messages": []any{}})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, settings.Env["ANTHROPIC_BASE_URL"]+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	parts := strings.SplitN(settings.Env["ANTHROPIC_CUSTOM_HEADERS"], ": ", 2)
	if len(parts) == 2 {
		request.Header.Set(parts[0], parts[1])
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	return []byte("BEDROCK_PROXY_OFFLINE_OK"), nil, nil
}

func (r doctorRunner) Run(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
	if len(args) == 1 && args[0] == "--version" {
		return []byte("codex-cli " + r.version), nil, nil
	}
	if len(args) >= 3 && args[0] == "debug" && args[1] == "models" && args[2] == "-c" {
		path, err := strconv.Unquote(strings.TrimPrefix(args[3], "model_catalog_json="))
		if err != nil {
			return nil, nil, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, err
		}
		var document struct {
			Models []struct {
				Slug string `json:"slug"`
			} `json:"models"`
		}
		if err := json.Unmarshal(data, &document); err != nil {
			return nil, nil, err
		}
		encoded, err := json.Marshal(document)
		return encoded, nil, err
	}
	return r.bundled, nil, nil
}

func TestOfflineCodexDoctorAcceptsCustomCatalogFromProfile(t *testing.T) {
	root := t.TempDir()
	cfg := doctorTestConfig(root)
	bundled := doctorBundledCatalog()
	runner := doctorRunner{version: "0.142.5", bundled: bundled}
	catalogPath := filepath.Join(root, "custom", "catalog.json")
	generated, err := codex.Generate(context.Background(), codex.GenerateOptions{
		Config: cfg, ConfigPath: filepath.Join(root, "config.yaml"), CatalogPath: catalogPath,
		ProxyVersion: "test", Runner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	profilePath := filepath.Join(root, "bedrock-local.config.toml")
	if err := os.WriteFile(profilePath, []byte(generated.TOML), 0o600); err != nil {
		t.Fatal(err)
	}
	result := Run(context.Background(), Options{
		Config: cfg, ConfigPath: filepath.Join(root, "config.yaml"), Client: "codex",
		ProfilePath: profilePath, CodexRunner: runner,
	})
	if !result.OK() {
		t.Fatalf("doctor failed: %+v", result.Checks)
	}
	for _, wanted := range []string{"report directory", "model capabilities", "Codex catalog drift", "Codex catalog parsing", "Codex profile"} {
		if !hasPassingCheck(result, wanted) {
			t.Fatalf("missing passing %q check: %+v", wanted, result.Checks)
		}
	}
	if !hasCheck(result, "Codex output ceiling", Warning, "does not expose max_output_tokens") {
		t.Fatalf("missing explicit output-limit warning: %+v", result.Checks)
	}
}

func TestOfflineClaudeDoctorValidatesCompleteSettings(t *testing.T) {
	root := t.TempDir()
	cfg := doctorClaudeConfig(root)
	runner := doctorClaudeRunner{version: claude.MinimumVersion}
	settingsPath := filepath.Join(root, "custom", "settings.json")
	generated, err := claude.Generate(context.Background(), claude.GenerateOptions{
		Config: cfg, ConfigPath: filepath.Join(root, "config.yaml"), SettingsPath: settingsPath, Runner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := Run(context.Background(), Options{
		Config: cfg, ConfigPath: filepath.Join(root, "config.yaml"), Client: "claude",
		ClaudeSettingsPath: generated.SettingsPath, ClaudeRunner: runner,
	})
	if !result.OK() {
		t.Fatalf("doctor failed: %+v", result.Checks)
	}
	for _, wanted := range []string{"report directory", "Claude settings", "Claude CLI", "Claude settings parsing", "Claude offline request"} {
		if !hasPassingCheck(result, wanted) {
			t.Fatalf("missing passing %q check: %+v", wanted, result.Checks)
		}
	}
	if !hasCheck(result, "model capabilities", Warning, "responses-only") {
		t.Fatalf("missing skipped model warning: %+v", result.Checks)
	}

	changed := cfg
	model := changed.Models["fable"]
	output := int64(32000)
	model.Capabilities.MaxOutputTokens = &output
	changed.Models["fable"] = model
	stale := Run(context.Background(), Options{Config: changed, ConfigPath: filepath.Join(root, "config.yaml"), Client: "claude", ClaudeSettingsPath: settingsPath, ClaudeRunner: runner})
	if stale.OK() || !hasFailedCategory(stale, "claude_compatibility", "do not match") {
		t.Fatalf("settings drift result = %+v", stale.Checks)
	}
}

func TestOfflineClaudeDoctorRejectsOldCLI(t *testing.T) {
	root := t.TempDir()
	result := Run(context.Background(), Options{
		Config: doctorClaudeConfig(root), ConfigPath: filepath.Join(root, "config.yaml"), Client: "claude",
		ClaudeRunner: doctorClaudeRunner{version: "2.1.241"},
	})
	if result.OK() || !hasFailedCategory(result, "claude_compatibility", "too old") {
		t.Fatalf("old CLI result = %+v", result.Checks)
	}
}

func TestOfflineCodexDoctorReportsCatalogDrift(t *testing.T) {
	root := t.TempDir()
	cfg := doctorTestConfig(root)
	runner := doctorRunner{version: "0.142.5", bundled: doctorBundledCatalog()}
	generated, err := codex.Generate(context.Background(), codex.GenerateOptions{Config: cfg, ConfigPath: filepath.Join(root, "config.yaml"), Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	profilePath := filepath.Join(root, "profile.toml")
	if err := os.WriteFile(profilePath, []byte(generated.TOML), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := runner
	stale.version = "0.143.0"
	result := Run(context.Background(), Options{Config: cfg, ConfigPath: filepath.Join(root, "config.yaml"), Client: "codex", ProfilePath: profilePath, CodexRunner: stale})
	if result.OK() || !hasFailedCategory(result, "codex_compatibility", "regenerate") {
		t.Fatalf("drift result = %+v", result.Checks)
	}
}

func TestOfflineCodexDoctorReportsProxyConfigurationDrift(t *testing.T) {
	root := t.TempDir()
	cfg := doctorTestConfig(root)
	runner := doctorRunner{version: "0.142.5", bundled: doctorBundledCatalog()}
	generated, err := codex.Generate(context.Background(), codex.GenerateOptions{Config: cfg, ConfigPath: filepath.Join(root, "config.yaml"), Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	profilePath := filepath.Join(root, "profile.toml")
	if err := os.WriteFile(profilePath, []byte(generated.TOML), 0o600); err != nil {
		t.Fatal(err)
	}
	model := cfg.Models["coding"]
	changed := int64(63000)
	model.Capabilities.MaxOutputTokens = &changed
	cfg.Models["coding"] = model
	result := Run(context.Background(), Options{Config: cfg, ConfigPath: filepath.Join(root, "config.yaml"), Client: "codex", ProfilePath: profilePath, CodexRunner: runner})
	if result.OK() || !hasFailedCategory(result, "codex_compatibility", "does not match the current proxy configuration") {
		t.Fatalf("configuration drift result = %+v", result.Checks)
	}
}

func TestOfflineCodexDoctorValidatesCompleteEligibleSet(t *testing.T) {
	root := t.TempDir()
	cfg := doctorTestConfig(root)
	cfg.Codex.DefaultModel = "coding"
	second := cfg.Models["coding"]
	second.BedrockModelID = "second-target"
	cfg.Models["second"] = second
	cfg.Models["fable"] = config.ModelConfig{BedrockModelID: "messages-only"}
	runner := doctorRunner{version: "0.154.0", bundled: doctorBundledCatalog()}
	generated, err := codex.Generate(context.Background(), codex.GenerateOptions{Config: cfg, ConfigPath: filepath.Join(root, "config.yaml"), Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	profilePath := filepath.Join(root, "profile.toml")
	if err := os.WriteFile(profilePath, []byte(generated.TOML), 0o600); err != nil {
		t.Fatal(err)
	}
	result := Run(context.Background(), Options{Config: cfg, ConfigPath: filepath.Join(root, "config.yaml"), Client: "codex", ProfilePath: profilePath, CodexRunner: runner})
	if !result.OK() || !hasCheck(result, "model capabilities", Warning, "fable") || !hasCheck(result, "Codex catalog configuration", Passed, "coding, second") {
		t.Fatalf("multi-model doctor checks=%+v", result.Checks)
	}
	delete(cfg.Models, "second")
	result = Run(context.Background(), Options{Config: cfg, ConfigPath: filepath.Join(root, "config.yaml"), Client: "codex", ProfilePath: profilePath, CodexRunner: runner})
	if result.OK() || !hasFailedCategory(result, "codex_compatibility", "model set") {
		t.Fatalf("removed-model drift checks=%+v", result.Checks)
	}
	cfg.Models["second"] = second
	added := cfg.Models["coding"]
	added.BedrockModelID = "third-target"
	cfg.Models["third"] = added
	result = Run(context.Background(), Options{Config: cfg, ConfigPath: filepath.Join(root, "config.yaml"), Client: "codex", ProfilePath: profilePath, CodexRunner: runner})
	if result.OK() || !hasFailedCategory(result, "codex_compatibility", "model set") {
		t.Fatalf("added-model drift checks=%+v", result.Checks)
	}
}

func TestOfflineCodexDoctorRejectsCommentedProfileValues(t *testing.T) {
	root := t.TempDir()
	cfg := doctorTestConfig(root)
	runner := doctorRunner{version: "0.142.5", bundled: doctorBundledCatalog()}
	generated, err := codex.Generate(context.Background(), codex.GenerateOptions{Config: cfg, ConfigPath: filepath.Join(root, "config.yaml"), Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	profile := strings.Replace(generated.TOML, `model = "coding"`, `# model = "coding"`, 1)
	profilePath := filepath.Join(root, "profile.toml")
	if err := os.WriteFile(profilePath, []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
	result := Run(context.Background(), Options{Config: cfg, ConfigPath: filepath.Join(root, "config.yaml"), Client: "codex", ProfilePath: profilePath, CodexRunner: runner})
	if result.OK() || !hasFailedCategory(result, "codex_compatibility", "selects model") {
		t.Fatalf("commented profile result = %+v", result.Checks)
	}
}

func TestOfflineCodexDoctorRejectsMissingHostedSearchControls(t *testing.T) {
	root := t.TempDir()
	cfg := doctorTestConfig(root)
	runner := doctorRunner{version: "0.142.5", bundled: doctorBundledCatalog()}
	generated, err := codex.Generate(context.Background(), codex.GenerateOptions{Config: cfg, ConfigPath: filepath.Join(root, "config.yaml"), Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	profile := strings.Replace(generated.TOML, `web_search = "disabled"`, `# web_search = "disabled"`, 1)
	profilePath := filepath.Join(root, "profile.toml")
	if err := os.WriteFile(profilePath, []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
	result := Run(context.Background(), Options{Config: cfg, ConfigPath: filepath.Join(root, "config.yaml"), Client: "codex", ProfilePath: profilePath, CodexRunner: runner})
	if result.OK() || !hasFailedCategory(result, "codex_compatibility", "disable hosted web search") {
		t.Fatalf("hosted-search profile result = %+v", result.Checks)
	}
}

func TestOfflineCodexDoctorRejectsEnabledChatGPTApps(t *testing.T) {
	root := t.TempDir()
	cfg := doctorTestConfig(root)
	runner := doctorRunner{version: "0.142.5", bundled: doctorBundledCatalog()}
	generated, err := codex.Generate(context.Background(), codex.GenerateOptions{Config: cfg, ConfigPath: filepath.Join(root, "config.yaml"), Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	profile := strings.Replace(generated.TOML, `features.apps = false`, `features.apps = true`, 1)
	profilePath := filepath.Join(root, "profile.toml")
	if err := os.WriteFile(profilePath, []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
	result := Run(context.Background(), Options{Config: cfg, ConfigPath: filepath.Join(root, "config.yaml"), Client: "codex", ProfilePath: profilePath, CodexRunner: runner})
	if result.OK() || !hasFailedCategory(result, "codex_compatibility", "codex_apps") {
		t.Fatalf("apps profile result = %+v", result.Checks)
	}
}

func TestOfflineCodexDoctorRejectsTargetWithoutResponsesAPI(t *testing.T) {
	root := t.TempDir()
	cfg := doctorTestConfig(root)
	model := cfg.Models["coding"]
	responses := false
	model.Capabilities.ResponsesAPI = &responses
	cfg.Models["coding"] = model
	result := Run(context.Background(), Options{Config: cfg, ConfigPath: filepath.Join(root, "config.yaml"), Client: "codex", ModelAlias: "coding"})
	if result.OK() || !hasFailedCategory(result, "model_capability", "does not support the Responses API") {
		t.Fatalf("Responses compatibility result = %+v", result.Checks)
	}
}

func TestDiagnosticOutputLimitNeverExceedsCapability(t *testing.T) {
	for _, test := range []struct {
		ceiling int64
		wanted  int64
		expect  int64
	}{{32, 512, 32}, {64000, 512, 512}, {1, 16, 1}} {
		if got := diagnosticOutputLimit(test.ceiling, test.wanted); got != test.expect {
			t.Fatalf("diagnosticOutputLimit(%d, %d) = %d, want %d", test.ceiling, test.wanted, got, test.expect)
		}
	}
}

func TestClaudeReasoningDiagnosticUsesConfiguredEffort(t *testing.T) {
	client := HTTPDoerFunc(func(request *http.Request) (*http.Response, error) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if _, present := body["thinking"]; present {
			t.Fatalf("legacy fixed-budget thinking must not be sent: %s", body["thinking"])
		}
		var outputConfig struct {
			Effort string `json:"effort"`
		}
		if err := json.Unmarshal(body["output_config"], &outputConfig); err != nil || outputConfig.Effort != "high" {
			t.Fatalf("output_config=%s err=%v", body["output_config"], err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"type":"message","usage":{"input_tokens":1,"output_tokens":1}}`)),
		}, nil
	})
	if err := checkClaudeReasoning(context.Background(), client, "http://127.0.0.1:8787", "fable", 64000, []string{"low", "medium", "high"}, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
}

type HTTPDoerFunc func(*http.Request) (*http.Response, error)

func (f HTTPDoerFunc) Do(request *http.Request) (*http.Response, error) { return f(request) }

func TestDoctorNeedsCompleteMetadataOnlyForCodex(t *testing.T) {
	root := t.TempDir()
	cfg := doctorTestConfig(root)
	model := cfg.Models["coding"]
	model.Capabilities = nil
	cfg.Models["coding"] = model
	plain := Run(context.Background(), Options{Config: cfg, ConfigPath: filepath.Join(root, "config.yaml")})
	if !plain.OK() {
		t.Fatalf("plain doctor should allow legacy config: %+v", plain.Checks)
	}
	codexResult := Run(context.Background(), Options{Config: cfg, ConfigPath: filepath.Join(root, "config.yaml"), Client: "codex", ModelAlias: "coding"})
	if codexResult.OK() || !hasFailedCategory(codexResult, "model_capability", "capabilities are required") {
		t.Fatalf("Codex doctor result = %+v", codexResult.Checks)
	}
}

func doctorTestConfig(root string) config.Config {
	contextWindow, maxOutput := int64(200000), int64(64000)
	responses, reasoning, functions, parallel := true, true, true, true
	return config.Config{
		Version:   1,
		AWS:       config.AWSConfig{Profile: "unused", Region: "us-east-2"},
		Listen:    "127.0.0.1:8787",
		Reporting: config.ReportingConfig{Directory: filepath.Join(root, "reports")},
		Models: map[string]config.ModelConfig{"coding": {
			BedrockModelID: "custom-target",
			Capabilities: &config.CapabilityConfig{
				ResponsesAPI:  &responses,
				ContextWindow: &contextWindow, MaxOutputTokens: &maxOutput, InputModalities: []string{"text", "image"},
				Reasoning: config.ReasoningCapability{Supported: &reasoning, Efforts: []string{"low", "medium", "high"}},
				Tools:     config.ToolCapability{FunctionCalling: &functions, ParallelCalls: &parallel},
			},
		}},
	}
}

func doctorClaudeConfig(root string) config.Config {
	contextWindow, maxOutput := int64(200000), int64(64000)
	messages, reasoning, functions, parallel := true, true, true, true
	model := func(target, canonical string) config.ModelConfig {
		return config.ModelConfig{
			BedrockModelID: target,
			Capabilities: &config.CapabilityConfig{
				MessagesAPI: &messages, AnthropicModelID: canonical,
				ContextWindow: &contextWindow, MaxOutputTokens: &maxOutput, InputModalities: []string{"text", "image"},
				Reasoning: config.ReasoningCapability{Supported: &reasoning, Efforts: []string{"low", "medium", "high"}},
				Tools:     config.ToolCapability{FunctionCalling: &functions, ParallelCalls: &parallel},
			},
		}
	}
	return config.Config{
		Version: 1, AWS: config.AWSConfig{Profile: "unused", Region: "us-east-2"}, Listen: "127.0.0.1:8787",
		Reporting: config.ReportingConfig{Directory: filepath.Join(root, "reports")},
		Claude:    config.ClaudeConfig{DefaultModel: "fable"},
		Models: map[string]config.ModelConfig{
			"fable":          model("target-fable", "claude-fable-5"),
			"sonnet":         model("target-sonnet", "claude-sonnet-5"),
			"responses-only": {BedrockModelID: "target-responses"},
		},
	}
}

func doctorBundledCatalog() []byte {
	return []byte(`{"models":[{"slug":"gpt-test","base_instructions":"You are Codex, a coding agent based on GPT.\nFollow instructions.","model_messages":{"instructions_template":"You are Codex, a coding agent based on GPT.\nFollow instructions.","instructions_variables":{"personality_default":""}}}]}`)
}

func hasPassingCheck(result Result, name string) bool {
	for _, check := range result.Checks {
		if check.Name == name && check.Status == Passed {
			return true
		}
	}
	return false
}

func hasFailedCategory(result Result, category, text string) bool {
	for _, check := range result.Checks {
		if check.Status == Failed && check.Category == category && strings.Contains(check.Message, text) {
			return true
		}
	}
	return false
}

func hasCheck(result Result, name string, status Status, text string) bool {
	for _, check := range result.Checks {
		if check.Name == name && check.Status == status && strings.Contains(check.Message, text) {
			return true
		}
	}
	return false
}
