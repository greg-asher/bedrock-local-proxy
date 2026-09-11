package config

import (
	"fmt"
	"sort"
	"strings"
)

// CapabilityConfig describes limits and client-visible behavior separately
// from request defaults. Pointer booleans preserve omitted values so a
// metadata profile can supply them without making false indistinguishable
// from unspecified.
type CapabilityConfig struct {
	MetadataProfile string              `yaml:"metadata_profile,omitempty"`
	ContextWindow   *int64              `yaml:"context_window,omitempty"`
	MaxOutputTokens *int64              `yaml:"max_output_tokens,omitempty"`
	InputModalities []string            `yaml:"input_modalities,omitempty"`
	Reasoning       ReasoningCapability `yaml:"reasoning,omitempty"`
	Tools           ToolCapability      `yaml:"tools,omitempty"`
}

type ReasoningCapability struct {
	Supported *bool    `yaml:"supported,omitempty"`
	Efforts   []string `yaml:"efforts,omitempty"`
}

type ToolCapability struct {
	FunctionCalling *bool `yaml:"function_calling,omitempty"`
	ParallelCalls   *bool `yaml:"parallel_calls,omitempty"`
}

// ResolvedCapabilities is complete enough to generate deterministic client
// metadata. Hosted tools are deliberately absent: the fixed bedrock-runtime
// transport cannot execute them.
type ResolvedCapabilities struct {
	MetadataProfile    string
	MetadataRevision   string
	MetadataSource     string
	MetadataVerifiedAt string
	ContextWindow      int64
	MaxOutputTokens    int64
	InputModalities    []string
	ReasoningSupported bool
	ReasoningEfforts   []string
	FunctionCalling    bool
	ParallelCalls      bool
}

type capabilityProfile struct {
	ResolvedCapabilities
	ExactModelIDs []string
}

// The registry is intentionally small. A profile is added only when its hard
// limits and interface behavior have an authoritative source. Users can still
// configure complete capabilities for any other target.
var capabilityProfiles = map[string]capabilityProfile{
	"anthropic.claude-sonnet-4-5-20250929-v1:0": {
		ResolvedCapabilities: ResolvedCapabilities{
			MetadataProfile:    "anthropic.claude-sonnet-4-5-20250929-v1:0",
			MetadataRevision:   "2026-09-11",
			MetadataSource:     "https://docs.aws.amazon.com/bedrock/latest/userguide/model-card-anthropic-claude-sonnet-4-5.html",
			MetadataVerifiedAt: "2026-09-11",
			ContextWindow:      200000,
			MaxOutputTokens:    64000,
			InputModalities:    []string{"text", "image"},
			// The model supports extended thinking, but the documented
			// Bedrock interface does not establish an OpenAI-compatible
			// effort mapping for this target. Leave adjustable reasoning off.
			ReasoningSupported: false,
			ReasoningEfforts:   nil,
			FunctionCalling:    true,
			// Claude 4 models can return several independent tool calls in one
			// turn. The proxy preserves those Responses items without executing
			// them; the live doctor verifies the target-specific wire behavior.
			ParallelCalls: true,
		},
		ExactModelIDs: []string{
			"anthropic.claude-sonnet-4-5-20250929-v1:0",
			"global.anthropic.claude-sonnet-4-5-20250929-v1:0",
			"us.anthropic.claude-sonnet-4-5-20250929-v1:0",
		},
	},
}

var allowedModalities = map[string]struct{}{"text": {}, "image": {}}
var allowedReasoningEfforts = map[string]struct{}{
	"none": {}, "minimal": {}, "low": {}, "medium": {}, "high": {}, "xhigh": {}, "max": {},
}

// ResolveModelCapabilities applies explicit values over a named profile or an
// exact model-ID match. It never performs fuzzy model-name inference.
func ResolveModelCapabilities(model ModelConfig) (ResolvedCapabilities, []string, error) {
	if model.Capabilities == nil {
		return ResolvedCapabilities{}, nil, fmt.Errorf("capabilities are required for Codex metadata")
	}
	cfg := model.Capabilities
	var resolved ResolvedCapabilities
	var profile capabilityProfile
	var hasProfile bool
	profileName := strings.TrimSpace(cfg.MetadataProfile)
	if profileName != "" {
		var ok bool
		profile, ok = capabilityProfiles[profileName]
		if !ok {
			return ResolvedCapabilities{}, nil, fmt.Errorf("unknown metadata_profile %q (available: %s)", profileName, strings.Join(MetadataProfileNames(), ", "))
		}
		hasProfile = true
	} else if matched, ok := profileForExactModelID(strings.TrimSpace(model.BedrockModelID)); ok {
		profile = matched
		hasProfile = true
	}
	if hasProfile {
		resolved = cloneResolved(profile.ResolvedCapabilities)
	}

	var warnings []string
	if cfg.ContextWindow != nil {
		if hasProfile && *cfg.ContextWindow > profile.ContextWindow {
			warnings = append(warnings, fmt.Sprintf("context_window %d exceeds profile value %d", *cfg.ContextWindow, profile.ContextWindow))
		}
		resolved.ContextWindow = *cfg.ContextWindow
	}
	if cfg.MaxOutputTokens != nil {
		if hasProfile && *cfg.MaxOutputTokens > profile.MaxOutputTokens {
			warnings = append(warnings, fmt.Sprintf("max_output_tokens %d exceeds profile value %d", *cfg.MaxOutputTokens, profile.MaxOutputTokens))
		}
		resolved.MaxOutputTokens = *cfg.MaxOutputTokens
	}
	if cfg.InputModalities != nil {
		resolved.InputModalities = append([]string(nil), cfg.InputModalities...)
	}
	if cfg.Reasoning.Supported != nil {
		resolved.ReasoningSupported = *cfg.Reasoning.Supported
	}
	if cfg.Reasoning.Efforts != nil {
		resolved.ReasoningEfforts = append([]string(nil), cfg.Reasoning.Efforts...)
	}
	if cfg.Tools.FunctionCalling != nil {
		resolved.FunctionCalling = *cfg.Tools.FunctionCalling
	}
	if cfg.Tools.ParallelCalls != nil {
		resolved.ParallelCalls = *cfg.Tools.ParallelCalls
	}
	if resolved.ContextWindow <= 0 && (hasProfile || cfg.ContextWindow != nil) {
		return ResolvedCapabilities{}, warnings, fmt.Errorf("context_window must be a positive integer")
	}
	if resolved.MaxOutputTokens <= 0 && (hasProfile || cfg.MaxOutputTokens != nil) {
		return ResolvedCapabilities{}, warnings, fmt.Errorf("max_output_tokens must be a positive integer")
	}
	if resolved.ContextWindow > 0 && resolved.MaxOutputTokens > resolved.ContextWindow {
		return ResolvedCapabilities{}, warnings, fmt.Errorf("max_output_tokens must not exceed context_window")
	}
	if hasProfile || cfg.InputModalities != nil {
		modalities, err := normalizeValues(resolved.InputModalities, allowedModalities, "input modality")
		if err != nil {
			return ResolvedCapabilities{}, warnings, err
		}
		if len(modalities) == 0 {
			return ResolvedCapabilities{}, warnings, fmt.Errorf("input_modalities must contain at least one value")
		}
		resolved.InputModalities = modalities
	}
	efforts, err := normalizeValues(resolved.ReasoningEfforts, allowedReasoningEfforts, "reasoning effort")
	if err != nil {
		return ResolvedCapabilities{}, warnings, err
	}
	if !resolved.ReasoningSupported && len(efforts) != 0 {
		return ResolvedCapabilities{}, warnings, fmt.Errorf("reasoning efforts require reasoning.supported: true")
	}
	if resolved.ReasoningSupported && len(efforts) == 0 {
		return ResolvedCapabilities{}, warnings, fmt.Errorf("reasoning.efforts must contain at least one value when reasoning is supported")
	}
	resolved.ReasoningEfforts = efforts
	if resolved.ParallelCalls && !resolved.FunctionCalling {
		return ResolvedCapabilities{}, warnings, fmt.Errorf("tools.parallel_calls requires tools.function_calling: true")
	}
	if !hasProfile {
		missing := make([]string, 0, 6)
		if cfg.ContextWindow == nil {
			missing = append(missing, "context_window")
		}
		if cfg.MaxOutputTokens == nil {
			missing = append(missing, "max_output_tokens")
		}
		if cfg.InputModalities == nil {
			missing = append(missing, "input_modalities")
		}
		if cfg.Reasoning.Supported == nil {
			missing = append(missing, "reasoning.supported")
		}
		if cfg.Tools.FunctionCalling == nil {
			missing = append(missing, "tools.function_calling")
		}
		if cfg.Tools.ParallelCalls == nil {
			missing = append(missing, "tools.parallel_calls")
		}
		if len(missing) > 0 {
			return ResolvedCapabilities{}, warnings, fmt.Errorf("missing capability metadata: %s", strings.Join(missing, ", "))
		}
	}
	return resolved, warnings, nil
}

func MetadataProfileNames() []string {
	names := make([]string, 0, len(capabilityProfiles))
	for name := range capabilityProfiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func profileForExactModelID(id string) (capabilityProfile, bool) {
	for _, profile := range capabilityProfiles {
		for _, candidate := range profile.ExactModelIDs {
			if id == candidate {
				return profile, true
			}
		}
	}
	return capabilityProfile{}, false
}

func cloneResolved(value ResolvedCapabilities) ResolvedCapabilities {
	value.InputModalities = append([]string(nil), value.InputModalities...)
	value.ReasoningEfforts = append([]string(nil), value.ReasoningEfforts...)
	return value
}

func normalizeValues(values []string, allowed map[string]struct{}, label string) ([]string, error) {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if _, ok := allowed[value]; !ok {
			return nil, fmt.Errorf("unsupported %s %q", label, value)
		}
		if _, duplicate := seen[value]; duplicate {
			return nil, fmt.Errorf("duplicate %s %q", label, value)
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result, nil
}
