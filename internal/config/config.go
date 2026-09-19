// Package config loads and validates the user-editable proxy configuration.
package config

import (
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	CurrentVersion = 1
	DefaultListen  = "127.0.0.1:8787"
)

// Config is the complete version 1 configuration file.
type Config struct {
	Version   int                     `yaml:"version"`
	AWS       AWSConfig               `yaml:"aws"`
	Listen    string                  `yaml:"listen"`
	Reporting ReportingConfig         `yaml:"reporting,omitempty"`
	Codex     CodexConfig             `yaml:"codex,omitempty"`
	Claude    ClaudeConfig            `yaml:"claude,omitempty"`
	Models    map[string]ModelConfig  `yaml:"models"`
	Presets   map[string]PresetConfig `yaml:"presets,omitempty"`
}

// PresetConfig narrows the shared model catalog and gives one named setup its
// own port and session tag. Presets never redefine a model; they select from
// the single top-level catalog so pricing and capabilities are written once.
type PresetConfig struct {
	Listen     string       `yaml:"listen"`
	SessionTag string       `yaml:"session_tag,omitempty"`
	Models     []string     `yaml:"models,omitempty"`
	Codex      CodexConfig  `yaml:"codex,omitempty"`
	Claude     ClaudeConfig `yaml:"claude,omitempty"`
}

type AWSConfig struct {
	Profile string `yaml:"profile"`
	Region  string `yaml:"region"`
}

// ReportingConfig controls the parent directory for durable per-session
// reports. An empty directory uses the user-scoped default.
type ReportingConfig struct {
	Directory string `yaml:"directory,omitempty"`
}

// CodexConfig controls the generated Codex client profile. The catalog still
// contains every eligible proxy model.
type CodexConfig struct {
	DefaultModel string `yaml:"default_model,omitempty"`
}

// ClaudeConfig controls the generated Claude Code settings. SubagentModel
// defaults to the effective main model when omitted.
type ClaudeConfig struct {
	DefaultModel  string `yaml:"default_model,omitempty"`
	SubagentModel string `yaml:"subagent_model,omitempty"`
}

// ModelConfig describes a public model name and its Bedrock target. Pointer
// defaults preserve the distinction between an omitted value and zero.
type ModelConfig struct {
	DisplayName               string            `yaml:"display_name,omitempty"`
	BedrockModelID            string            `yaml:"bedrock_model_id"`
	InputPerMillion           *float64          `yaml:"input_per_million,omitempty"`
	OutputPerMillion          *float64          `yaml:"output_per_million,omitempty"`
	CacheReadInputPerMillion  *float64          `yaml:"cache_read_input_per_million,omitempty"`
	CacheWriteInputPerMillion *float64          `yaml:"cache_write_input_per_million,omitempty"`
	Temperature               *float64          `yaml:"temperature,omitempty"`
	MaxTokens                 *int              `yaml:"max_tokens,omitempty"`
	Capabilities              *CapabilityConfig `yaml:"capabilities,omitempty"`
}

// DefaultPath returns the user-scoped configuration location. It does not
// depend on the process working directory.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home directory: %w", err)
	}
	return filepath.Join(home, ".config", "bedrock-proxy", "config.yaml"), nil
}

// DefaultReportDirectory returns the user-scoped parent directory for session
// reports. XDG_STATE_HOME must be absolute when supplied; a relative value is
// ignored so reports never depend on an incidental working directory.
func DefaultReportDirectory() (string, error) {
	if stateHome := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); stateHome != "" && filepath.IsAbs(stateHome) {
		return filepath.Join(stateHome, "bedrock-proxy", "sessions"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "bedrock-proxy", "sessions"), nil
}

// ResolveReportDirectory chooses the report parent from a CLI override,
// configuration, or the default. Relative CLI paths use the working
// directory; relative configuration paths use the configuration file's
// directory.
func ResolveReportDirectory(configPath, configuredDirectory, override string) (string, error) {
	if value := strings.TrimSpace(override); value != "" {
		path, err := filepath.Abs(value)
		if err != nil {
			return "", fmt.Errorf("resolve report directory %q: %w", value, err)
		}
		return filepath.Clean(path), nil
	}
	if value := strings.TrimSpace(configuredDirectory); value != "" {
		if filepath.IsAbs(value) {
			return filepath.Clean(value), nil
		}
		absoluteConfig, err := filepath.Abs(configPath)
		if err != nil {
			return "", fmt.Errorf("resolve config path %q: %w", configPath, err)
		}
		return filepath.Clean(filepath.Join(filepath.Dir(absoluteConfig), value)), nil
	}
	return DefaultReportDirectory()
}

// LoadFile decodes and validates a configuration file. yaml.v3's KnownFields
// mode rejects unknown fields and its decoder rejects duplicate mapping keys.
func LoadFile(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}
	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %q: %w", path, err)
	}
	// A second YAML document would otherwise be silently ignored.
	var extra any
	if err := dec.Decode(&extra); err == nil {
		return Config{}, fmt.Errorf("parse config %q: multiple YAML documents are not supported", path)
	} else if !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("parse config %q: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("invalid config %q: %w", path, err)
	}
	return cfg, nil
}

func (c *Config) Validate() error {
	if c.Version != CurrentVersion {
		return fmt.Errorf("unsupported version %d (expected %d)", c.Version, CurrentVersion)
	}
	if strings.TrimSpace(c.AWS.Profile) == "" {
		return errors.New("aws.profile is required")
	}
	if c.AWS.Profile != strings.TrimSpace(c.AWS.Profile) {
		return errors.New("aws.profile must not have surrounding whitespace")
	}
	if strings.TrimSpace(c.AWS.Region) == "" {
		return errors.New("aws.region is required")
	}
	if c.AWS.Region != strings.TrimSpace(c.AWS.Region) {
		return errors.New("aws.region must not have surrounding whitespace")
	}
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if err := ValidateListenAddress(c.Listen); err != nil {
		return err
	}
	if len(c.Models) == 0 {
		return errors.New("models must contain at least one model")
	}
	if c.Codex.DefaultModel != strings.TrimSpace(c.Codex.DefaultModel) {
		return errors.New("codex.default_model must not have surrounding whitespace")
	}
	if c.Codex.DefaultModel != "" {
		if _, ok := c.Models[c.Codex.DefaultModel]; !ok {
			return fmt.Errorf("codex.default_model %q is not a configured model", c.Codex.DefaultModel)
		}
	}
	if c.Claude.DefaultModel != strings.TrimSpace(c.Claude.DefaultModel) {
		return errors.New("claude.default_model must not have surrounding whitespace")
	}
	if c.Claude.SubagentModel != strings.TrimSpace(c.Claude.SubagentModel) {
		return errors.New("claude.subagent_model must not have surrounding whitespace")
	}
	for _, selection := range []struct{ field, alias string }{
		{field: "claude.default_model", alias: c.Claude.DefaultModel},
		{field: "claude.subagent_model", alias: c.Claude.SubagentModel},
	} {
		if selection.alias != "" {
			if _, ok := c.Models[selection.alias]; !ok {
				return fmt.Errorf("%s %q is not a configured model", selection.field, selection.alias)
			}
		}
	}
	modelNames := make([]string, 0, len(c.Models))
	for name := range c.Models {
		modelNames = append(modelNames, name)
	}
	sort.Strings(modelNames)
	for _, name := range modelNames {
		model := c.Models[name]
		if strings.TrimSpace(name) == "" {
			return errors.New("model names must not be empty")
		}
		if strings.TrimSpace(model.BedrockModelID) == "" {
			return fmt.Errorf("models.%s.bedrock_model_id is required", name)
		}
		for _, price := range []struct {
			field string
			value *float64
		}{
			{field: "input_per_million", value: model.InputPerMillion},
			{field: "output_per_million", value: model.OutputPerMillion},
			{field: "cache_read_input_per_million", value: model.CacheReadInputPerMillion},
			{field: "cache_write_input_per_million", value: model.CacheWriteInputPerMillion},
		} {
			if price.value != nil && (!finite(*price.value) || *price.value < 0) {
				return fmt.Errorf("models.%s.%s must be a finite nonnegative number", name, price.field)
			}
		}
		if model.Temperature != nil && !finite(*model.Temperature) {
			return fmt.Errorf("models.%s.temperature must be finite", name)
		}
		if model.MaxTokens != nil && *model.MaxTokens < 0 {
			return fmt.Errorf("models.%s.max_tokens must not be negative", name)
		}
		if model.Capabilities != nil {
			if model.MaxTokens != nil && *model.MaxTokens <= 0 {
				return fmt.Errorf("models.%s.max_tokens must be a positive integer when capabilities are configured", name)
			}
			if model.MaxTokens != nil && model.Capabilities.MaxOutputTokens != nil && int64(*model.MaxTokens) > *model.Capabilities.MaxOutputTokens {
				return fmt.Errorf("models.%s.max_tokens must not exceed capabilities.max_output_tokens (%d)", name, *model.Capabilities.MaxOutputTokens)
			}
			resolved, _, err := ResolveModelCapabilities(model)
			if err != nil {
				return fmt.Errorf("models.%s.capabilities: %w", name, err)
			}
			if model.MaxTokens != nil && int64(*model.MaxTokens) > resolved.MaxOutputTokens {
				return fmt.Errorf("models.%s.max_tokens must not exceed capabilities.max_output_tokens (%d)", name, resolved.MaxOutputTokens)
			}
		}
	}
	return c.validatePresets()
}

// validatePresets checks every rule that can be decided from the single
// configuration document: name shape, catalog references, client defaults
// inside each preset's own subset, and port uniqueness. Deciding port
// conflicts here rather than at bind time is what makes simultaneously
// running presets safe by construction.
func (c *Config) validatePresets() error {
	if len(c.Presets) == 0 {
		return nil
	}
	names := make([]string, 0, len(c.Presets))
	for name := range c.Presets {
		names = append(names, name)
	}
	sort.Strings(names)
	// The unnamed default configuration stays startable, so its port is
	// reserved against every preset as well.
	claimed := map[string]string{c.Listen: "the top-level listen"}
	for _, name := range names {
		preset := c.Presets[name]
		if err := ValidatePresetName(name); err != nil {
			return fmt.Errorf("presets.%s: %w", name, err)
		}
		if strings.TrimSpace(preset.Listen) == "" {
			return fmt.Errorf("presets.%s.listen is required", name)
		}
		if err := ValidateListenAddress(preset.Listen); err != nil {
			return fmt.Errorf("presets.%s.%w", name, err)
		}
		if owner, taken := claimed[preset.Listen]; taken {
			return fmt.Errorf("presets.%s.listen %q is already used by %s", name, preset.Listen, owner)
		}
		claimed[preset.Listen] = "presets." + name
		allowed, err := preset.allowedModels(c.Models, name)
		if err != nil {
			return err
		}
		for _, selection := range []struct{ field, alias string }{
			{"codex.default_model", firstNonEmpty(preset.Codex.DefaultModel, c.Codex.DefaultModel)},
			{"claude.default_model", firstNonEmpty(preset.Claude.DefaultModel, c.Claude.DefaultModel)},
			{"claude.subagent_model", firstNonEmpty(preset.Claude.SubagentModel, c.Claude.SubagentModel)},
		} {
			if selection.alias == "" {
				continue
			}
			if _, ok := allowed[selection.alias]; !ok {
				return fmt.Errorf("presets.%s: effective %s %q is not in this preset's models; add it or set presets.%s.%s", name, selection.field, selection.alias, name, selection.field)
			}
		}
		if preset.SessionTag != "" {
			if err := validateSessionTag(preset.SessionTag); err != nil {
				return fmt.Errorf("presets.%s.session_tag %w", name, err)
			}
		}
	}
	return nil
}

// allowedModels resolves a preset's model selection against the shared
// catalog. An omitted list means the whole catalog; an empty list is an error
// rather than a proxy that can serve nothing.
func (p PresetConfig) allowedModels(catalog map[string]ModelConfig, name string) (map[string]ModelConfig, error) {
	if p.Models == nil {
		allowed := make(map[string]ModelConfig, len(catalog))
		for alias, model := range catalog {
			allowed[alias] = model
		}
		return allowed, nil
	}
	if len(p.Models) == 0 {
		return nil, fmt.Errorf("presets.%s.models must not be empty; omit it to allow every model", name)
	}
	allowed := make(map[string]ModelConfig, len(p.Models))
	for _, alias := range p.Models {
		if alias != strings.TrimSpace(alias) {
			return nil, fmt.Errorf("presets.%s.models entry %q must not have surrounding whitespace", name, alias)
		}
		if _, seen := allowed[alias]; seen {
			return nil, fmt.Errorf("presets.%s.models lists %q more than once", name, alias)
		}
		model, ok := catalog[alias]
		if !ok {
			return nil, fmt.Errorf("presets.%s.models %q is not a configured model", name, alias)
		}
		allowed[alias] = model
	}
	return allowed, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// Resolve returns the effective configuration for one preset: the shared
// catalog narrowed to the preset's models, its port, and its client defaults.
// The result is an ordinary Config, so nothing downstream needs to know that
// presets exist. An empty name returns the configuration unchanged.
func (c Config) Resolve(preset string) (Config, error) {
	name := strings.TrimSpace(preset)
	if name == "" {
		resolved := c
		resolved.Presets = nil
		return resolved, nil
	}
	selected, ok := c.Presets[name]
	if !ok {
		return Config{}, fmt.Errorf("preset %q is not configured%s", name, describePresets(c.Presets))
	}
	allowed, err := selected.allowedModels(c.Models, name)
	if err != nil {
		return Config{}, err
	}
	resolved := c
	resolved.Presets = nil
	resolved.Models = allowed
	resolved.Listen = selected.Listen
	if selected.Codex.DefaultModel != "" {
		resolved.Codex.DefaultModel = selected.Codex.DefaultModel
	}
	if selected.Claude.DefaultModel != "" {
		resolved.Claude.DefaultModel = selected.Claude.DefaultModel
	}
	if selected.Claude.SubagentModel != "" {
		resolved.Claude.SubagentModel = selected.Claude.SubagentModel
	}
	return resolved, nil
}

// PresetNames returns the configured preset names in sorted order.
func (c Config) PresetNames() []string {
	names := make([]string, 0, len(c.Presets))
	for name := range c.Presets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// SessionTag returns the configured tag for a preset, or empty when the
// preset is unnamed or sets none.
func (c Config) SessionTag(preset string) string {
	if selected, ok := c.Presets[strings.TrimSpace(preset)]; ok {
		return selected.SessionTag
	}
	return ""
}

func describePresets(presets map[string]PresetConfig) string {
	if len(presets) == 0 {
		return "; this configuration defines no presets"
	}
	names := make([]string, 0, len(presets))
	for name := range presets {
		names = append(names, name)
	}
	sort.Strings(names)
	return "; configured presets: " + strings.Join(names, ", ")
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

// ValidateListenAddress accepts only loopback TCP addresses. localhost is
// accepted as the conventional loopback hostname; wildcard and other host
// addresses are rejected before binding.
func ValidateListenAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" || port == "" {
		return fmt.Errorf("listen must be a loopback host and port (got %q)", address)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("listen address %q is not loopback; use %s", address, DefaultListen)
	}
	return nil
}
