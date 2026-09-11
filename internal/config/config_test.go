package config

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validConfig() Config {
	return Config{
		Version: 1,
		AWS:     AWSConfig{Profile: "YOUR_AWS_PROFILE", Region: "us-east-2"},
		Models: map[string]ModelConfig{
			"coding": {BedrockModelID: "model-a"},
			"fast":   {BedrockModelID: "model-a"},
		},
	}
}

func TestValidateDefaultsListenAndAllowsSharedTarget(t *testing.T) {
	cfg := validConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != DefaultListen {
		t.Fatalf("listen = %q, want %q", cfg.Listen, DefaultListen)
	}
}

func TestValidateAcceptsConfiguredCodexDefault(t *testing.T) {
	cfg := validConfig()
	cfg.Codex.DefaultModel = "coding"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRejectsInvalidCodexDefault(t *testing.T) {
	for _, value := range []string{"missing", " coding"} {
		cfg := validConfig()
		cfg.Codex.DefaultModel = value
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "codex.default_model") {
			t.Fatalf("default %q error=%v", value, err)
		}
	}
}

func TestLoadFileReadsCodexDefaultModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := "version: 1\naws:\n  profile: p\n  region: us-east-2\ncodex:\n  default_model: luna\nmodels:\n  luna:\n    bedrock_model_id: target\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Codex.DefaultModel != "luna" {
		t.Fatalf("default model=%q", cfg.Codex.DefaultModel)
	}
}

func TestLoadFileReadsClaudeDefaultsWithoutChangingVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := "version: 1\naws:\n  profile: p\n  region: us-east-2\nclaude:\n  default_model: fable\n  subagent_model: sonnet\nmodels:\n  fable:\n    bedrock_model_id: target-a\n  sonnet:\n    bedrock_model_id: target-b\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Version != 1 || cfg.Claude.DefaultModel != "fable" || cfg.Claude.SubagentModel != "sonnet" {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestDefaultPathIsHomeScoped(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".config", "bedrock-proxy", "config.yaml")
	if path != want {
		t.Fatalf("DefaultPath() = %q, want %q", path, want)
	}
}

func TestDefaultReportDirectoryUsesXDGStateOrHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "")
	path, err := DefaultReportDirectory()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".local", "state", "bedrock-proxy", "sessions"); path != want {
		t.Fatalf("default report directory = %q, want %q", path, want)
	}
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	path, err = DefaultReportDirectory()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(state, "bedrock-proxy", "sessions"); path != want {
		t.Fatalf("XDG report directory = %q, want %q", path, want)
	}
	t.Setenv("XDG_STATE_HOME", "relative-state")
	path, err = DefaultReportDirectory()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".local", "state", "bedrock-proxy", "sessions"); path != want {
		t.Fatalf("relative XDG report directory = %q, want %q", path, want)
	}
}

func TestResolveReportDirectoryPrecedenceAndRelativePaths(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "nested", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveReportDirectory(configPath, "reports", "")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(filepath.Dir(configPath), "reports"); got != want {
		t.Fatalf("config-relative report directory = %q, want %q", got, want)
	}
	working := t.TempDir()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(working); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	got, err = ResolveReportDirectory(configPath, "reports", "cli-reports")
	if err != nil {
		t.Fatal(err)
	}
	resolvedWorking, err := filepath.EvalSymlinks(working)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(resolvedWorking, "cli-reports"); got != want {
		t.Fatalf("CLI-relative report directory = %q, want %q", got, want)
	}
}

func TestLoadFileAcceptsOptionalReportingBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := "version: 1\naws:\n  profile: p\n  region: r\nreporting:\n  directory: reports\nmodels:\n  coding:\n    bedrock_model_id: a\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Reporting.Directory != "reports" {
		t.Fatalf("reporting directory = %q", cfg.Reporting.Directory)
	}
}

func TestLoadFileAcceptsOmittedCapabilitiesForVersionOne(t *testing.T) {
	cfg := validConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("legacy version 1 config failed: %v", err)
	}
	if cfg.Models["coding"].Capabilities != nil {
		t.Fatal("omitted capabilities were populated")
	}
}

func TestCapabilityResolutionListsAllMissingExplicitMetadata(t *testing.T) {
	contextWindow := int64(1000)
	_, _, err := ResolveModelCapabilities(ModelConfig{BedrockModelID: "unknown", Capabilities: &CapabilityConfig{ContextWindow: &contextWindow}})
	if err == nil {
		t.Fatal("incomplete capabilities unexpectedly resolved")
	}
	for _, field := range []string{"max_output_tokens", "input_modalities", "reasoning.supported", "tools.function_calling", "tools.parallel_calls"} {
		if !strings.Contains(err.Error(), field) {
			t.Fatalf("error %q omitted %s", err, field)
		}
	}
}

func TestResolveModelCapabilitiesUsesExactProfileAndOverrides(t *testing.T) {
	model := ModelConfig{
		BedrockModelID: "global.anthropic.claude-sonnet-4-5-20250929-v1:0",
		Capabilities:   &CapabilityConfig{},
	}
	resolved, warnings, err := ResolveModelCapabilities(model)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 || resolved.MetadataProfile != "anthropic.claude-sonnet-4-5-20250929-v1:0" {
		t.Fatalf("profile resolution = %+v warnings=%v", resolved, warnings)
	}
	if resolved.ContextWindow != 200000 || resolved.MaxOutputTokens != 64000 || !resolved.FunctionCalling || !resolved.ParallelCalls {
		t.Fatalf("resolved capabilities = %+v", resolved)
	}
	if resolved.ResponsesSupported || !resolved.ResponsesKnown {
		t.Fatalf("Sonnet Responses compatibility = %+v", resolved)
	}
	if err := ValidateCodexCompatibility(model, resolved); err == nil || !strings.Contains(err.Error(), "does not support the Responses API") {
		t.Fatalf("Codex compatibility error = %v", err)
	}
}

func TestResolveModelCapabilitiesUsesResponsesCompatibleGPTOSSProfile(t *testing.T) {
	model := ModelConfig{BedrockModelID: "openai.gpt-oss-120b-1:0", Capabilities: &CapabilityConfig{}}
	resolved, warnings, err := ResolveModelCapabilities(model)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 || !resolved.ResponsesKnown || !resolved.ResponsesSupported || !resolved.MessagesKnown || resolved.MessagesSupported || resolved.ContextWindow != 128000 || resolved.MaxOutputTokens != 16000 || !resolved.FunctionCalling || resolved.ParallelCalls {
		t.Fatalf("resolved capabilities = %+v warnings=%v", resolved, warnings)
	}
	if err := ValidateCodexCompatibility(model, resolved); err != nil {
		t.Fatalf("Codex compatibility = %v", err)
	}
}

func TestResolveModelCapabilitiesUsesClaudeMessagesIdentity(t *testing.T) {
	model := ModelConfig{BedrockModelID: "global.anthropic.claude-sonnet-4-5-20250929-v1:0", Capabilities: &CapabilityConfig{}}
	resolved, warnings, err := ResolveModelCapabilities(model)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 || !resolved.MessagesKnown || !resolved.MessagesSupported || resolved.AnthropicModelID != "claude-sonnet-4-5-20250929" || !resolved.FunctionCalling {
		t.Fatalf("resolved capabilities = %+v warnings=%v", resolved, warnings)
	}
	if err := ValidateMessagesTarget(model); err != nil {
		t.Fatal(err)
	}
}

func TestResolveModelCapabilitiesValidatesAnthropicIdentity(t *testing.T) {
	messages := true
	complete := CapabilityConfig{
		MessagesAPI: &messages, AnthropicModelID: "claude-custom-1",
		ContextWindow: int64Pointer(1000), MaxOutputTokens: int64Pointer(100), InputModalities: []string{"text"},
		Reasoning: ReasoningCapability{Supported: boolPointer(false)},
		Tools:     ToolCapability{FunctionCalling: boolPointer(true), ParallelCalls: boolPointer(false)},
	}
	if resolved, _, err := ResolveModelCapabilities(ModelConfig{BedrockModelID: "custom", Capabilities: &complete}); err != nil || resolved.AnthropicModelID != "claude-custom-1" {
		t.Fatalf("resolved=%+v err=%v", resolved, err)
	}
	for _, test := range []struct {
		name string
		edit func(*CapabilityConfig)
		want string
	}{
		{name: "noncanonical", edit: func(c *CapabilityConfig) { c.AnthropicModelID = "fable" }, want: "beginning with claude-"},
		{name: "missing messages declaration", edit: func(c *CapabilityConfig) { c.MessagesAPI = nil }, want: "requires messages_api"},
		{name: "messages disabled", edit: func(c *CapabilityConfig) { disabled := false; c.MessagesAPI = &disabled }, want: "requires messages_api: true"},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := complete
			test.edit(&value)
			if _, _, err := ResolveModelCapabilities(ModelConfig{BedrockModelID: "custom", Capabilities: &value}); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v want=%q", err, test.want)
			}
		})
	}
}

func TestValidateMessagesTargetRejectsKnownIncompatibleModels(t *testing.T) {
	if err := ValidateMessagesTarget(ModelConfig{BedrockModelID: "openai.gpt-oss-120b-1:0"}); err == nil || !strings.Contains(err.Error(), "does not support the Messages API") {
		t.Fatalf("known incompatible error = %v", err)
	}
	if err := ValidateMessagesTarget(ModelConfig{BedrockModelID: "unknown-legacy-target"}); err != nil {
		t.Fatalf("legacy unknown target = %v", err)
	}
	if err := ValidateMessagesTarget(ModelConfig{BedrockModelID: "custom-target", Capabilities: &CapabilityConfig{MetadataProfile: "openai.gpt-oss-120b-1:0"}}); err == nil || !strings.Contains(err.Error(), "metadata profile") {
		t.Fatalf("known incompatible profile error = %v", err)
	}
}

func TestValidateClaudeDefaultsReferenceConfiguredModels(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*Config)
		want string
	}{
		{name: "unknown default", edit: func(c *Config) { c.Claude.DefaultModel = "missing" }, want: "claude.default_model"},
		{name: "unknown subagent", edit: func(c *Config) { c.Claude.SubagentModel = "missing" }, want: "claude.subagent_model"},
		{name: "default whitespace", edit: func(c *Config) { c.Claude.DefaultModel = " coding" }, want: "surrounding whitespace"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := validConfig()
			test.edit(&cfg)
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error=%v want=%q", err, test.want)
			}
		})
	}
}

func TestResolveModelCapabilitiesValidatesCompleteExplicitMetadata(t *testing.T) {
	responses := true
	model := ModelConfig{
		BedrockModelID: "custom-target",
		Capabilities: &CapabilityConfig{
			ResponsesAPI:    &responses,
			ContextWindow:   int64Pointer(100000),
			MaxOutputTokens: int64Pointer(32000),
			InputModalities: []string{"text"},
			Reasoning: ReasoningCapability{
				Supported: boolPointer(true),
				Efforts:   []string{"low", "medium", "high"},
			},
			Tools: ToolCapability{
				FunctionCalling: boolPointer(true),
				ParallelCalls:   boolPointer(false),
			},
		},
	}
	resolved, warnings, err := ResolveModelCapabilities(model)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 || !resolved.ReasoningSupported || len(resolved.ReasoningEfforts) != 3 {
		t.Fatalf("resolved capabilities = %+v warnings=%v", resolved, warnings)
	}
}

func TestValidateCodexCompatibilityRequiresExplicitResponsesSupportForUnknownTarget(t *testing.T) {
	model := ModelConfig{BedrockModelID: "custom-target", Capabilities: &CapabilityConfig{}}
	for _, test := range []struct {
		name      string
		responses *bool
		want      string
	}{
		{name: "omitted", want: "responses_api is required"},
		{name: "disabled", responses: boolPointer(false), want: "does not support the Responses API"},
		{name: "enabled", responses: boolPointer(true)},
	} {
		t.Run(test.name, func(t *testing.T) {
			resolved := ResolvedCapabilities{ResponsesKnown: test.responses != nil, FunctionCalling: true}
			if test.responses != nil {
				resolved.ResponsesSupported = *test.responses
			}
			err := ValidateCodexCompatibility(model, resolved)
			if test.want == "" && err != nil {
				t.Fatal(err)
			}
			if test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestResolveModelCapabilitiesRejectsUnsafeCombinations(t *testing.T) {
	for _, tt := range []struct {
		name  string
		value CapabilityConfig
		want  string
	}{
		{name: "missing limits", value: CapabilityConfig{InputModalities: []string{"text"}}, want: "context_window"},
		{name: "output exceeds context", value: CapabilityConfig{ContextWindow: int64Pointer(10), MaxOutputTokens: int64Pointer(11), InputModalities: []string{"text"}}, want: "must not exceed"},
		{name: "unsupported modality", value: CapabilityConfig{ContextWindow: int64Pointer(10), MaxOutputTokens: int64Pointer(5), InputModalities: []string{"audio"}}, want: "unsupported input modality"},
		{name: "reasoning efforts while disabled", value: CapabilityConfig{ContextWindow: int64Pointer(10), MaxOutputTokens: int64Pointer(5), InputModalities: []string{"text"}, Reasoning: ReasoningCapability{Supported: boolPointer(false), Efforts: []string{"high"}}}, want: "require reasoning.supported"},
		{name: "parallel without tools", value: CapabilityConfig{ContextWindow: int64Pointer(10), MaxOutputTokens: int64Pointer(5), InputModalities: []string{"text"}, Tools: ToolCapability{ParallelCalls: boolPointer(true)}}, want: "requires tools.function_calling"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := ResolveModelCapabilities(ModelConfig{BedrockModelID: "custom", Capabilities: &tt.value})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestValidateRejectsRequestDefaultAboveCapabilityLimit(t *testing.T) {
	cfg := validConfig()
	max := 101
	cfg.Models["coding"] = ModelConfig{
		BedrockModelID: "custom",
		MaxTokens:      &max,
		Capabilities: &CapabilityConfig{
			ContextWindow:   int64Pointer(1000),
			MaxOutputTokens: int64Pointer(100),
			InputModalities: []string{"text"},
		},
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "must not exceed") {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestValidateRejectsNonpositiveRequestDefaultWithCapabilities(t *testing.T) {
	value := 0
	cfg := validConfig()
	model := ModelConfig{
		BedrockModelID: "anthropic.claude-sonnet-4-5-20250929-v1:0",
		MaxTokens:      &value,
		Capabilities:   &CapabilityConfig{},
	}
	cfg.Models["coding"] = model
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "positive integer") {
		t.Fatalf("max_tokens=%d Validate() error = %v", value, err)
	}
}

func TestValidateRejectsNonLoopbackAndInvalidValues(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Config)
		want string
	}{
		{"wildcard", func(c *Config) { c.Listen = "0.0.0.0:8787" }, "not loopback"},
		{"missing profile", func(c *Config) { c.AWS.Profile = "" }, "aws.profile is required"},
		{"negative price", func(c *Config) {
			v := -1.0
			model := c.Models["coding"]
			model.InputPerMillion = &v
			c.Models["coding"] = model
		}, "finite nonnegative"},
		{"non-finite price", func(c *Config) {
			v := math.NaN()
			model := c.Models["coding"]
			model.InputPerMillion = &v
			c.Models["coding"] = model
		}, "finite nonnegative"},
		{"non-finite temperature", func(c *Config) {
			v := math.Inf(1)
			model := c.Models["coding"]
			model.Temperature = &v
			c.Models["coding"] = model
		}, "temperature must be finite"},
		{"profile whitespace", func(c *Config) { c.AWS.Profile = " profile" }, "surrounding whitespace"},
		{"region whitespace", func(c *Config) { c.AWS.Region = "us-east-2 " }, "surrounding whitespace"},
		{"missing target", func(c *Config) { c.Models["coding"] = ModelConfig{} }, "bedrock_model_id is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.edit(&cfg)
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestLoadFileRejectsUnknownAndDuplicateFields(t *testing.T) {
	tests := []string{
		"version: 1\naws:\n  profile: p\n  region: r\nmodels:\n  coding:\n    bedrock_model_id: a\n    extra: x\n",
		"version: 1\nversion: 1\naws:\n  profile: p\n  region: r\nmodels:\n  coding:\n    bedrock_model_id: a\n",
	}
	for _, contents := range tests {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadFile(path); err == nil {
			t.Fatalf("LoadFile(%q) unexpectedly succeeded", contents)
		}
	}
}

func boolPointer(value bool) *bool    { return &value }
func int64Pointer(value int64) *int64 { return &value }
