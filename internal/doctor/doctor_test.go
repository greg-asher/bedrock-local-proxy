package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gregasher/bedrock-local-proxy/internal/codex"
	"github.com/gregasher/bedrock-local-proxy/internal/config"
)

type doctorRunner struct {
	version string
	bundled []byte
}

func (r doctorRunner) Run(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
	if len(args) == 1 && args[0] == "--version" {
		return []byte("codex-cli " + r.version), nil, nil
	}
	if len(args) >= 3 && args[0] == "debug" && args[1] == "models" && args[2] == "-c" {
		return []byte(`{"models":[{"slug":"coding"}]}`), nil, nil
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
