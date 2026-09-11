package codex

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

type fakeRunner struct {
	version []byte
	catalog []byte
	err     error
}

func (f fakeRunner) Run(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
	if f.err != nil {
		return nil, []byte("command failed"), f.err
	}
	if len(args) == 1 && args[0] == "--version" {
		return f.version, nil, nil
	}
	if len(args) >= 3 && args[0] == "debug" && args[1] == "models" && args[2] == "-c" {
		return []byte(`{"models":[{"slug":"coding"}]}`), nil, nil
	}
	return f.catalog, nil, nil
}

func TestGeneratePreservesBundledModelsAndAddsResolvedAlias(t *testing.T) {
	output := filepath.Join(t.TempDir(), "nested", "models.json")
	cfg := catalogTestConfig()
	result, err := Generate(context.Background(), GenerateOptions{
		Config:       cfg,
		ConfigPath:   filepath.Join(t.TempDir(), "config.yaml"),
		CatalogPath:  output,
		ProxyVersion: "v1.2.3",
		Runner: fakeRunner{
			version: []byte("codex-cli 0.142.5\n"),
			catalog: bundledFixture(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ModelAlias != "coding" || result.CodexVersion != "0.142.5" || result.CatalogPath != output {
		t.Fatalf("result = %+v", result)
	}
	for _, expected := range []string{"model = \"coding\"", "web_search = \"disabled\"", "tools.web_search = false", "features.apps = false", "supports_standalone_web_search = false", "Authorization = \"Bearer local\"", "X-Bedrock-Proxy-Catalog", output} {
		if !strings.Contains(result.TOML, expected) {
			t.Fatalf("TOML missing %q:\n%s", expected, result.TOML)
		}
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var document catalogDocument
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Models) != 2 || len(document.BedrockLocalProxy.BundledModels) != 1 || document.BedrockLocalProxy.CodexVersion != "0.142.5" || document.BedrockLocalProxy.ProxyVersion != "v1.2.3" || document.BedrockLocalProxy.CatalogHash == "" || result.CatalogHash != document.BedrockLocalProxy.CatalogHash || document.BedrockLocalProxy.Models["coding"].ConfigurationHash == "" {
		t.Fatalf("catalog = %+v", document)
	}
	var local catalogModel
	if err := json.Unmarshal(document.Models[1], &local); err != nil {
		t.Fatal(err)
	}
	if local.Slug != "coding" || local.ContextWindow != 100000 || local.MaxContextWindow != 100000 || local.MaxOutputTokens != 32000 || local.EffectiveContextPercent != 100 || local.SupportsSearchTool || !local.SupportsParallelToolCalls {
		t.Fatalf("local catalog model = %+v", local)
	}
	if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "does not expose max_output_tokens") {
		t.Fatalf("output-limit warnings = %v", result.Warnings)
	}
	if strings.Contains(result.Warnings[0], "apply the configured") {
		t.Fatalf("warning claimed an absent max_tokens default: %v", result.Warnings)
	}
	if local.BaseInstructions == "" || strings.Contains(strings.SplitN(local.BaseInstructions, "\n", 2)[0], "GPT") {
		t.Fatalf("instructions were not provider-neutral: %q", local.BaseInstructions)
	}
	if len(local.ModelMessages) == 0 || strings.Contains(string(local.ModelMessages), "based on GPT") {
		t.Fatalf("model messages were missing or provider-specific: %s", local.ModelMessages)
	}
	info, err := os.Stat(output)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("catalog permissions=%v err=%v", info.Mode(), err)
	}
}

func TestGenerateRejectsBundledAliasCollision(t *testing.T) {
	cfg := catalogTestConfig()
	cfg.Models["gpt-test"] = cfg.Models["coding"]
	delete(cfg.Models, "coding")
	_, err := Generate(context.Background(), GenerateOptions{Config: cfg, CatalogPath: filepath.Join(t.TempDir(), "models.json"), Runner: fakeRunner{version: []byte("codex-cli 0.142.5"), catalog: bundledFixture()}})
	if err == nil || !strings.Contains(err.Error(), "conflicts with a bundled Codex model") {
		t.Fatalf("collision error = %v", err)
	}
}

func TestGenerateRequiresModelForMultipleAliases(t *testing.T) {
	cfg := catalogTestConfig()
	cfg.Models["fast"] = cfg.Models["coding"]
	_, err := Generate(context.Background(), GenerateOptions{Config: cfg, Runner: fakeRunner{}})
	if err == nil || !strings.Contains(err.Error(), "--model is required") {
		t.Fatalf("error = %v", err)
	}
}

func TestGenerateRejectsMissingCapabilitiesAndFunctionCalling(t *testing.T) {
	missingResponses := completeModel(true)
	missingResponses.Capabilities.ResponsesAPI = nil
	for _, tt := range []struct {
		name  string
		model config.ModelConfig
		want  string
	}{
		{name: "missing", model: config.ModelConfig{BedrockModelID: "custom"}, want: "capabilities are required"},
		{name: "unknown Responses support", model: missingResponses, want: "responses_api is required"},
		{name: "no functions", model: completeModel(false), want: "must support client-side function calling"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := catalogTestConfig()
			cfg.Models["coding"] = tt.model
			_, err := Generate(context.Background(), GenerateOptions{Config: cfg, Runner: fakeRunner{}})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestGenerateReportsDocumentedLimitOverridesWithoutReplacingThem(t *testing.T) {
	cfg := catalogTestConfig()
	contextWindow, maxOutput := int64(128001), int64(16001)
	functions, reasoning := true, false
	cfg.Models["coding"] = config.ModelConfig{
		BedrockModelID: "openai.gpt-oss-120b-1:0",
		Capabilities: &config.CapabilityConfig{
			ContextWindow: &contextWindow, MaxOutputTokens: &maxOutput, InputModalities: []string{"text", "image"},
			Reasoning: config.ReasoningCapability{Supported: &reasoning},
			Tools:     config.ToolCapability{FunctionCalling: &functions},
		},
	}
	result, err := Generate(context.Background(), GenerateOptions{Config: cfg, CatalogPath: filepath.Join(t.TempDir(), "models.json"), Runner: fakeRunner{version: []byte("codex-cli 0.142.5"), catalog: bundledFixture()}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Warnings) != 3 || !strings.Contains(strings.Join(result.Warnings, " "), "exceeds profile value") || !strings.Contains(strings.Join(result.Warnings, " "), "does not expose max_output_tokens") {
		t.Fatalf("warnings = %v", result.Warnings)
	}
}

func TestGenerateRejectsExactTargetWithoutResponsesSupport(t *testing.T) {
	cfg := catalogTestConfig()
	cfg.Models["coding"] = config.ModelConfig{
		BedrockModelID: "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
		Capabilities:   &config.CapabilityConfig{},
	}
	_, err := Generate(context.Background(), GenerateOptions{Config: cfg, Runner: fakeRunner{}})
	if err == nil || !strings.Contains(err.Error(), "does not support the Responses API") {
		t.Fatalf("compatibility error = %v", err)
	}
}

func TestValidateConfigurationDetectsCatalogDrift(t *testing.T) {
	cfg := catalogTestConfig()
	path := filepath.Join(t.TempDir(), "models.json")
	_, err := Generate(context.Background(), GenerateOptions{Config: cfg, CatalogPath: path, Runner: fakeRunner{version: []byte("codex-cli 0.142.5"), catalog: bundledFixture()}})
	if err != nil {
		t.Fatal(err)
	}
	metadata, _, err := Inspect(path)
	if err != nil {
		t.Fatal(err)
	}
	model := cfg.Models["coding"]
	capabilities, _, err := config.ResolveModelCapabilities(model)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateConfiguration(metadata, "coding", model, capabilities); err != nil {
		t.Fatalf("unchanged configuration was rejected: %v", err)
	}
	model.DisplayName = "Changed display name"
	if err := ValidateConfiguration(metadata, "coding", model, capabilities); err == nil || !strings.Contains(err.Error(), "regenerate") {
		t.Fatalf("configuration drift error = %v", err)
	}
}

func TestGenerateReportsCodexAndCatalogFailures(t *testing.T) {
	_, err := Generate(context.Background(), GenerateOptions{Config: catalogTestConfig(), Runner: fakeRunner{err: errors.New("exit")}})
	if err == nil || !strings.Contains(err.Error(), "read Codex version") {
		t.Fatalf("command error = %v", err)
	}
	_, err = Generate(context.Background(), GenerateOptions{Config: catalogTestConfig(), Runner: fakeRunner{version: []byte("codex-cli 0.142.5\n"), catalog: []byte(`{"models":[]}`)}})
	if err == nil || !strings.Contains(err.Error(), "contains no models") {
		t.Fatalf("catalog error = %v", err)
	}
}

func TestInspectGeneratedCatalog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	_, err := Generate(context.Background(), GenerateOptions{
		Config:      catalogTestConfig(),
		CatalogPath: path,
		Runner:      fakeRunner{version: []byte("codex-cli 0.142.5"), catalog: bundledFixture()},
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata, aliases, err := Inspect(path)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.CodexVersion != "0.142.5" || len(aliases) != 1 || aliases[0] != "coding" {
		t.Fatalf("metadata=%+v aliases=%v", metadata, aliases)
	}
}

func TestInspectRejectsChangedBundledModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	_, err := Generate(context.Background(), GenerateOptions{
		Config: catalogTestConfig(), CatalogPath: path,
		Runner: fakeRunner{version: []byte("codex-cli 0.142.5"), catalog: bundledFixture()},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document catalogDocument
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	document.Models[0] = json.RawMessage(`{"slug":"gpt-test","display_name":"changed"}`)
	tampered, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Inspect(path); err == nil || !strings.Contains(err.Error(), "missing or changed") {
		t.Fatalf("bundled preservation error = %v", err)
	}
}

func TestGenerateIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	options := GenerateOptions{Config: catalogTestConfig(), CatalogPath: path, ProxyVersion: "test", Runner: fakeRunner{version: []byte("codex-cli 0.142.5"), catalog: bundledFixture()}}
	if _, err := Generate(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("identical inputs produced different catalogs")
	}
}

func TestGenerateReportsUnwritableOutputParent(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(parent, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Generate(context.Background(), GenerateOptions{Config: catalogTestConfig(), CatalogPath: filepath.Join(parent, "models.json"), Runner: fakeRunner{version: []byte("codex-cli 0.142.5"), catalog: bundledFixture()}})
	if err == nil || !strings.Contains(err.Error(), "write Codex model catalog") {
		t.Fatalf("error = %v", err)
	}
}

func catalogTestConfig() config.Config {
	return config.Config{
		Version: 1,
		AWS:     config.AWSConfig{Profile: "profile", Region: "us-east-2"},
		Listen:  "127.0.0.1:8787",
		Models:  map[string]config.ModelConfig{"coding": completeModel(true)},
	}
}

func completeModel(functionCalling bool) config.ModelConfig {
	responses := true
	return config.ModelConfig{
		DisplayName:    "Coding target",
		BedrockModelID: "custom-target",
		Capabilities: &config.CapabilityConfig{
			ResponsesAPI:    &responses,
			ContextWindow:   int64p(100000),
			MaxOutputTokens: int64p(32000),
			InputModalities: []string{"text", "image"},
			Reasoning: config.ReasoningCapability{
				Supported: boolp(true),
				Efforts:   []string{"low", "medium", "high"},
			},
			Tools: config.ToolCapability{
				FunctionCalling: boolp(functionCalling),
				ParallelCalls:   boolp(functionCalling),
			},
		},
	}
}

func bundledFixture() []byte {
	return []byte(`{"models":[{"slug":"gpt-test","display_name":"GPT Test","description":"fixture","default_reasoning_level":"medium","supported_reasoning_levels":[],"shell_type":"shell_command","visibility":"list","supported_in_api":true,"priority":10,"base_instructions":"You are Codex, a coding agent based on GPT.\n\nFollow the client instructions.","model_messages":{"instructions_template":"You are Codex, a coding agent based on GPT.\nFollow instructions.","instructions_variables":{"personality_default":""}},"supports_reasoning_summaries":true,"default_reasoning_summary":"none","support_verbosity":true,"default_verbosity":"low","web_search_tool_type":"text","truncation_policy":{"mode":"tokens","limit":10000},"supports_parallel_tool_calls":true,"supports_image_detail_original":false,"context_window":1000,"max_context_window":1000,"comp_hash":"x","effective_context_window_percent":95,"experimental_supported_tools":[],"input_modalities":["text"],"supports_search_tool":true,"use_responses_lite":false}]}`)
}

func boolp(value bool) *bool    { return &value }
func int64p(value int64) *int64 { return &value }
