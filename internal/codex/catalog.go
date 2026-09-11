// Package codex generates and validates the client-side model metadata used by
// Codex CLI profiles that point at Bedrock Local Proxy.
package codex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/gregasher/bedrock-local-proxy/internal/config"
)

const (
	CatalogSchemaVersion = 1
	ProviderID           = "bedrock-local"
	DefaultProfileName   = "bedrock-local"
)

type CommandRunner interface {
	Run(context.Context, string, ...string) ([]byte, []byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return []byte(stdout.String()), []byte(stderr.String()), err
}

type GenerateOptions struct {
	Config       config.Config
	ConfigPath   string
	ModelAlias   string
	CatalogPath  string
	ProxyVersion string
	CodexCommand string
	Runner       CommandRunner
}

type GenerateResult struct {
	CatalogPath        string
	ModelAlias         string
	CodexVersion       string
	BundledCatalogHash string
	MetadataRevision   string
	CatalogHash        string
	Warnings           []string
	TOML               string
}

type catalogDocument struct {
	Models            []json.RawMessage `json:"models"`
	BedrockLocalProxy CatalogMetadata   `json:"bedrock_local_proxy"`
}

type CatalogMetadata struct {
	SchemaVersion      int                    `json:"schema_version"`
	ProxyVersion       string                 `json:"proxy_version"`
	CodexVersion       string                 `json:"codex_version"`
	BundledCatalogHash string                 `json:"bundled_catalog_hash"`
	CatalogHash        string                 `json:"catalog_hash"`
	OutputLimitVisible bool                   `json:"output_limit_visible"`
	BundledModels      map[string]string      `json:"bundled_models"`
	Models             map[string]modelSource `json:"models"`
}

type modelSource struct {
	ConfigurationHash  string   `json:"configuration_hash"`
	MetadataProfile    string   `json:"metadata_profile,omitempty"`
	MetadataRevision   string   `json:"metadata_revision,omitempty"`
	MetadataSource     string   `json:"metadata_source,omitempty"`
	VerifiedAt         string   `json:"verified_at,omitempty"`
	ResponsesAPI       bool     `json:"responses_api"`
	ContextWindow      int64    `json:"context_window"`
	MaxOutputTokens    int64    `json:"max_output_tokens"`
	InputModalities    []string `json:"input_modalities"`
	ReasoningSupported bool     `json:"reasoning_supported"`
	ReasoningEfforts   []string `json:"reasoning_efforts"`
	FunctionCalling    bool     `json:"function_calling"`
	ParallelCalls      bool     `json:"parallel_calls"`
}

type catalogModel struct {
	Slug                        string           `json:"slug"`
	DisplayName                 string           `json:"display_name"`
	Description                 string           `json:"description"`
	DefaultReasoningLevel       *string          `json:"default_reasoning_level,omitempty"`
	SupportedReasoningLevels    []reasoningLevel `json:"supported_reasoning_levels"`
	ShellType                   string           `json:"shell_type"`
	Visibility                  string           `json:"visibility"`
	SupportedInAPI              bool             `json:"supported_in_api"`
	Priority                    int              `json:"priority"`
	AdditionalSpeedTiers        []string         `json:"additional_speed_tiers"`
	ServiceTiers                []any            `json:"service_tiers"`
	AvailabilityNUX             any              `json:"availability_nux"`
	Upgrade                     any              `json:"upgrade"`
	BaseInstructions            string           `json:"base_instructions"`
	ModelMessages               json.RawMessage  `json:"model_messages"`
	SupportsReasoningSummaries  bool             `json:"supports_reasoning_summaries"`
	DefaultReasoningSummary     string           `json:"default_reasoning_summary"`
	SupportVerbosity            bool             `json:"support_verbosity"`
	DefaultVerbosity            string           `json:"default_verbosity"`
	WebSearchToolType           string           `json:"web_search_tool_type"`
	TruncationPolicy            map[string]any   `json:"truncation_policy"`
	SupportsParallelToolCalls   bool             `json:"supports_parallel_tool_calls"`
	SupportsImageDetailOriginal bool             `json:"supports_image_detail_original"`
	ContextWindow               int64            `json:"context_window"`
	MaxContextWindow            int64            `json:"max_context_window"`
	MaxOutputTokens             int64            `json:"max_output_tokens"`
	CompHash                    string           `json:"comp_hash"`
	EffectiveContextPercent     int              `json:"effective_context_window_percent"`
	ExperimentalSupportedTools  []string         `json:"experimental_supported_tools"`
	InputModalities             []string         `json:"input_modalities"`
	SupportsSearchTool          bool             `json:"supports_search_tool"`
	UseResponsesLite            bool             `json:"use_responses_lite"`
}

type reasoningLevel struct {
	Effort      string `json:"effort"`
	Description string `json:"description"`
}

type catalogIdentity struct {
	Slug             string `json:"slug"`
	BaseInstructions string `json:"base_instructions"`
}

func Generate(ctx context.Context, options GenerateOptions) (GenerateResult, error) {
	alias, model, err := selectModel(options.Config.Models, options.ModelAlias)
	if err != nil {
		return GenerateResult{}, err
	}
	capabilities, warnings, err := config.ResolveModelCapabilities(model)
	if err != nil {
		return GenerateResult{}, fmt.Errorf("resolve capabilities for model %q: %w", alias, err)
	}
	if err := config.ValidateCodexCompatibility(model, capabilities); err != nil {
		return GenerateResult{}, fmt.Errorf("model %q is not Codex-compatible: %w", alias, err)
	}
	runner := options.Runner
	if runner == nil {
		runner = ExecRunner{}
	}
	command := strings.TrimSpace(options.CodexCommand)
	if command == "" {
		command = "codex"
	}
	versionOut, versionErr, err := runner.Run(ctx, command, "--version")
	if err != nil {
		return GenerateResult{}, commandError("read Codex version", err, versionErr)
	}
	codexVersion := normalizeCodexVersion(string(versionOut))
	if codexVersion == "" {
		return GenerateResult{}, fmt.Errorf("read Codex version: unexpected output %q", strings.TrimSpace(string(versionOut)))
	}
	bundled, bundledErr, err := runner.Run(ctx, command, "debug", "models", "--bundled")
	if err != nil {
		return GenerateResult{}, commandError("read bundled Codex model catalog", err, bundledErr)
	}
	var source struct {
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(bundled, &source); err != nil || len(source.Models) == 0 {
		if err == nil {
			err = errors.New("catalog contains no models")
		}
		return GenerateResult{}, fmt.Errorf("parse bundled Codex model catalog: %w", err)
	}
	bundledHash := sha256Hex(bundled)
	bundledModels, err := modelHashes(source.Models)
	if err != nil {
		return GenerateResult{}, fmt.Errorf("index bundled Codex model catalog: %w", err)
	}
	if _, conflict := bundledModels[alias]; conflict {
		return GenerateResult{}, fmt.Errorf("local model alias %q conflicts with a bundled Codex model; choose a distinct alias", alias)
	}
	outputLimitVisible := catalogHasField(source.Models, "max_output_tokens")
	instructions, modelMessages, err := providerNeutralInstructions(source.Models)
	if err != nil {
		return GenerateResult{}, err
	}
	entry := newCatalogModel(alias, model, capabilities, instructions, modelMessages)
	encodedEntry, err := json.Marshal(entry)
	if err != nil {
		return GenerateResult{}, fmt.Errorf("encode Codex model metadata: %w", err)
	}
	catalogHash := sha256Hex(encodedEntry)
	models, err := replaceModel(source.Models, alias, encodedEntry)
	if err != nil {
		return GenerateResult{}, err
	}
	document := catalogDocument{
		Models: models,
		BedrockLocalProxy: CatalogMetadata{
			SchemaVersion:      CatalogSchemaVersion,
			ProxyVersion:       options.ProxyVersion,
			CodexVersion:       codexVersion,
			BundledCatalogHash: bundledHash,
			CatalogHash:        catalogHash,
			OutputLimitVisible: outputLimitVisible,
			BundledModels:      bundledModels,
			Models: map[string]modelSource{alias: {
				ConfigurationHash:  configurationHash(alias, model, capabilities),
				MetadataProfile:    capabilities.MetadataProfile,
				MetadataRevision:   capabilities.MetadataRevision,
				MetadataSource:     capabilities.MetadataSource,
				VerifiedAt:         capabilities.MetadataVerifiedAt,
				ResponsesAPI:       capabilities.ResponsesSupported,
				ContextWindow:      capabilities.ContextWindow,
				MaxOutputTokens:    capabilities.MaxOutputTokens,
				InputModalities:    append([]string(nil), capabilities.InputModalities...),
				ReasoningSupported: capabilities.ReasoningSupported,
				ReasoningEfforts:   append([]string(nil), capabilities.ReasoningEfforts...),
				FunctionCalling:    capabilities.FunctionCalling,
				ParallelCalls:      capabilities.ParallelCalls,
			}},
		},
	}
	catalogPath, err := resolveCatalogPath(options.ConfigPath, options.CatalogPath)
	if err != nil {
		return GenerateResult{}, err
	}
	validate := func(candidate string) error {
		return ValidateWithCodex(ctx, command, candidate, []string{alias}, runner)
	}
	if err := writeCatalogAtomic(catalogPath, document, validate); err != nil {
		return GenerateResult{}, fmt.Errorf("write Codex model catalog: %w", err)
	}
	if !outputLimitVisible {
		message := fmt.Sprintf("Codex %s does not expose max_output_tokens in its model catalog; the proxy will enforce the %d-token ceiling", codexVersion, capabilities.MaxOutputTokens)
		if model.MaxTokens != nil {
			message += fmt.Sprintf(" and apply the configured models.%s.max_tokens default when requests omit a limit", alias)
		}
		warnings = append(warnings, message)
	}
	return GenerateResult{
		CatalogPath:        catalogPath,
		ModelAlias:         alias,
		CodexVersion:       codexVersion,
		BundledCatalogHash: bundledHash,
		MetadataRevision:   capabilities.MetadataRevision,
		CatalogHash:        catalogHash,
		Warnings:           append([]string(nil), warnings...),
		TOML:               ProfileTOML(alias, catalogPath, options.Config.Listen, catalogHash),
	}, nil
}

func ProfileTOML(alias, catalogPath, listen, catalogHash string) string {
	if strings.TrimSpace(listen) == "" {
		listen = config.DefaultListen
	}
	baseURL := "http://" + listen + "/v1"
	return fmt.Sprintf("model = %s\nmodel_provider = %q\nmodel_catalog_json = %s\nweb_search = %q\ntools.web_search = false\nfeatures.apps = false\n\n[model_providers.%s]\nname = %q\nbase_url = %q\nwire_api = %q\nhttp_headers = { Authorization = %q, X-Bedrock-Proxy-Catalog = %q }\nrequires_openai_auth = false\nsupports_standalone_web_search = false\nsupports_websockets = false\nrequest_max_retries = 0\nstream_max_retries = 0\n",
		strconv.Quote(alias), ProviderID, strconv.Quote(catalogPath), "disabled", ProviderID, "Bedrock Local Proxy", baseURL, "responses", "Bearer local", catalogHash)
}

func DefaultCatalogPath(configPath string) (string, error) {
	path := strings.TrimSpace(configPath)
	if path == "" {
		var err error
		path, err = config.DefaultPath()
		if err != nil {
			return "", err
		}
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve config path: %w", err)
	}
	return filepath.Join(filepath.Dir(absolute), "clients", "codex", "models.json"), nil
}

func Inspect(path string) (CatalogMetadata, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return CatalogMetadata{}, nil, err
	}
	var document catalogDocument
	if err := json.Unmarshal(data, &document); err != nil {
		return CatalogMetadata{}, nil, err
	}
	if document.BedrockLocalProxy.SchemaVersion != CatalogSchemaVersion {
		return CatalogMetadata{}, nil, fmt.Errorf("unsupported generated catalog schema %d", document.BedrockLocalProxy.SchemaVersion)
	}
	actualModels, err := modelHashes(document.Models)
	if err != nil {
		return CatalogMetadata{}, nil, fmt.Errorf("index generated Codex model catalog: %w", err)
	}
	if len(document.BedrockLocalProxy.BundledModels) == 0 {
		return CatalogMetadata{}, nil, errors.New("generated catalog does not record bundled model preservation; regenerate it")
	}
	for slug, expectedHash := range document.BedrockLocalProxy.BundledModels {
		if actualModels[slug] != expectedHash {
			return CatalogMetadata{}, nil, fmt.Errorf("bundled Codex model %q is missing or changed; regenerate the catalog", slug)
		}
	}
	aliases := make([]string, 0, len(document.BedrockLocalProxy.Models))
	for alias := range document.BedrockLocalProxy.Models {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	return document.BedrockLocalProxy, aliases, nil
}

// ValidateConfiguration detects changes to the selected proxy model after the
// catalog was generated. Any mismatch requires regeneration before Codex can
// rely on the metadata.
func ValidateConfiguration(metadata CatalogMetadata, alias string, model config.ModelConfig, capabilities config.ResolvedCapabilities) error {
	stored, ok := metadata.Models[alias]
	if !ok {
		return fmt.Errorf("catalog does not contain model %q", alias)
	}
	wanted := configurationHash(alias, model, capabilities)
	if stored.ConfigurationHash == "" || stored.ConfigurationHash != wanted {
		return fmt.Errorf("catalog metadata for model %q does not match the current proxy configuration; regenerate the catalog", alias)
	}
	return nil
}

// Probe reads the installed Codex version and bundled catalog identity without
// loading proxy configuration or contacting AWS.
func Probe(ctx context.Context, command string, runner CommandRunner) (string, string, error) {
	if runner == nil {
		runner = ExecRunner{}
	}
	if strings.TrimSpace(command) == "" {
		command = "codex"
	}
	versionOut, versionErr, err := runner.Run(ctx, command, "--version")
	if err != nil {
		return "", "", commandError("read Codex version", err, versionErr)
	}
	version := normalizeCodexVersion(string(versionOut))
	if version == "" {
		return "", "", fmt.Errorf("read Codex version: unexpected output %q", strings.TrimSpace(string(versionOut)))
	}
	bundled, bundledErr, err := runner.Run(ctx, command, "debug", "models", "--bundled")
	if err != nil {
		return "", "", commandError("read bundled Codex model catalog", err, bundledErr)
	}
	var document struct {
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(bundled, &document); err != nil || len(document.Models) == 0 {
		if err == nil {
			err = errors.New("catalog contains no models")
		}
		return "", "", fmt.Errorf("parse bundled Codex model catalog: %w", err)
	}
	return version, sha256Hex(bundled), nil
}

// ValidateWithCodex asks the installed CLI to parse the generated catalog and
// verifies that every generated alias survives Codex's schema decoding.
func ValidateWithCodex(ctx context.Context, command, path string, aliases []string, runner CommandRunner) error {
	if runner == nil {
		runner = ExecRunner{}
	}
	if strings.TrimSpace(command) == "" {
		command = "codex"
	}
	override := "model_catalog_json=" + strconv.Quote(path)
	stdout, stderr, err := runner.Run(ctx, command, "debug", "models", "-c", override)
	if err != nil {
		return commandError("validate generated Codex model catalog", err, stderr)
	}
	var document struct {
		Models []catalogIdentity `json:"models"`
	}
	if err := json.Unmarshal(stdout, &document); err != nil {
		return fmt.Errorf("parse Codex catalog validation output: %w", err)
	}
	found := make(map[string]struct{}, len(document.Models))
	for _, model := range document.Models {
		found[model.Slug] = struct{}{}
	}
	for _, alias := range aliases {
		if _, ok := found[alias]; !ok {
			return fmt.Errorf("Codex catalog validation did not contain model %q", alias)
		}
	}
	return nil
}

func selectModel(models map[string]config.ModelConfig, requested string) (string, config.ModelConfig, error) {
	requested = strings.TrimSpace(requested)
	if requested != "" {
		model, ok := models[requested]
		if !ok {
			return "", config.ModelConfig{}, fmt.Errorf("unknown model %q (configured: %s)", requested, strings.Join(sortedModelNames(models), ", "))
		}
		return requested, model, nil
	}
	if len(models) != 1 {
		return "", config.ModelConfig{}, fmt.Errorf("--model is required when configuration contains multiple models (configured: %s)", strings.Join(sortedModelNames(models), ", "))
	}
	for alias, model := range models {
		return alias, model, nil
	}
	return "", config.ModelConfig{}, errors.New("configuration contains no models")
}

func sortedModelNames(models map[string]config.ModelConfig) []string {
	names := make([]string, 0, len(models))
	for name := range models {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func resolveCatalogPath(configPath, override string) (string, error) {
	if strings.TrimSpace(override) == "" {
		return DefaultCatalogPath(configPath)
	}
	path, err := filepath.Abs(override)
	if err != nil {
		return "", fmt.Errorf("resolve catalog path: %w", err)
	}
	return filepath.Clean(path), nil
}

func normalizeCodexVersion(output string) string {
	fields := strings.Fields(strings.TrimSpace(output))
	if len(fields) < 2 || fields[0] != "codex-cli" {
		return ""
	}
	return fields[1]
}

func commandError(action string, runErr error, stderr []byte) error {
	message := strings.TrimSpace(string(stderr))
	if message == "" {
		return fmt.Errorf("%s: %w", action, runErr)
	}
	return fmt.Errorf("%s: %w: %s", action, runErr, message)
}

func providerNeutralInstructions(models []json.RawMessage) (string, json.RawMessage, error) {
	for _, raw := range models {
		var identity struct {
			BaseInstructions string                     `json:"base_instructions"`
			ModelMessages    map[string]json.RawMessage `json:"model_messages"`
		}
		if json.Unmarshal(raw, &identity) != nil || strings.TrimSpace(identity.BaseInstructions) == "" || len(identity.ModelMessages) == 0 {
			continue
		}
		instructions := providerNeutralText(identity.BaseInstructions)
		if rawTemplate, ok := identity.ModelMessages["instructions_template"]; ok {
			var template string
			if json.Unmarshal(rawTemplate, &template) == nil {
				encoded, _ := json.Marshal(providerNeutralText(template))
				identity.ModelMessages["instructions_template"] = encoded
			}
		}
		messages, err := json.Marshal(identity.ModelMessages)
		if err != nil {
			return "", nil, fmt.Errorf("encode provider-neutral model messages: %w", err)
		}
		return instructions, messages, nil
	}
	return "", nil, errors.New("bundled Codex catalog has no reusable base instructions and model messages")
}

func providerNeutralText(value string) string {
	if first, rest, ok := strings.Cut(value, "\n"); ok && strings.HasPrefix(first, "You are Codex") {
		return "You are Codex, a coding agent.\n" + rest
	}
	return value
}

func newCatalogModel(alias string, model config.ModelConfig, capabilities config.ResolvedCapabilities, instructions string, modelMessages json.RawMessage) catalogModel {
	levels := make([]reasoningLevel, 0, len(capabilities.ReasoningEfforts))
	for _, effort := range capabilities.ReasoningEfforts {
		levels = append(levels, reasoningLevel{Effort: effort, Description: reasoningDescription(effort)})
	}
	var defaultLevel *string
	if capabilities.ReasoningSupported && len(capabilities.ReasoningEfforts) > 0 {
		value := capabilities.ReasoningEfforts[0]
		for _, effort := range capabilities.ReasoningEfforts {
			if effort == "medium" {
				value = effort
				break
			}
		}
		defaultLevel = &value
	}
	displayName := strings.TrimSpace(model.DisplayName)
	if displayName == "" {
		displayName = alias
	}
	description := fmt.Sprintf("Bedrock Local Proxy model %s", displayName)
	metadataHashInput := fmt.Sprintf("%s\x00%s\x00%d\x00%d\x00%v\x00%v\x00%v", alias, model.BedrockModelID, capabilities.ContextWindow, capabilities.MaxOutputTokens, capabilities.InputModalities, capabilities.ReasoningEfforts, capabilities.ParallelCalls)
	return catalogModel{
		Slug:                        alias,
		DisplayName:                 displayName,
		Description:                 description,
		DefaultReasoningLevel:       defaultLevel,
		SupportedReasoningLevels:    levels,
		ShellType:                   "shell_command",
		Visibility:                  "list",
		SupportedInAPI:              true,
		Priority:                    0,
		AdditionalSpeedTiers:        []string{},
		ServiceTiers:                []any{},
		BaseInstructions:            instructions,
		ModelMessages:               append(json.RawMessage(nil), modelMessages...),
		SupportsReasoningSummaries:  false,
		DefaultReasoningSummary:     "none",
		SupportVerbosity:            false,
		DefaultVerbosity:            "low",
		WebSearchToolType:           "text",
		TruncationPolicy:            map[string]any{"mode": "tokens", "limit": 10000},
		SupportsParallelToolCalls:   capabilities.ParallelCalls,
		SupportsImageDetailOriginal: false,
		ContextWindow:               capabilities.ContextWindow,
		MaxContextWindow:            capabilities.ContextWindow,
		MaxOutputTokens:             capabilities.MaxOutputTokens,
		CompHash:                    sha256Hex([]byte(metadataHashInput))[:16],
		EffectiveContextPercent:     100,
		ExperimentalSupportedTools:  []string{},
		InputModalities:             append([]string(nil), capabilities.InputModalities...),
		SupportsSearchTool:          false,
		UseResponsesLite:            false,
	}
}

func catalogHasField(models []json.RawMessage, field string) bool {
	for _, raw := range models {
		var value map[string]json.RawMessage
		if json.Unmarshal(raw, &value) == nil {
			if _, ok := value[field]; ok {
				return true
			}
		}
	}
	return false
}

func modelHashes(models []json.RawMessage) (map[string]string, error) {
	result := make(map[string]string, len(models))
	for _, raw := range models {
		var identity struct {
			Slug string `json:"slug"`
		}
		if err := json.Unmarshal(raw, &identity); err != nil || strings.TrimSpace(identity.Slug) == "" {
			if err == nil {
				err = errors.New("model slug is missing")
			}
			return nil, err
		}
		if _, duplicate := result[identity.Slug]; duplicate {
			return nil, fmt.Errorf("duplicate model slug %q", identity.Slug)
		}
		canonical, err := json.Marshal(raw)
		if err != nil {
			return nil, err
		}
		result[identity.Slug] = sha256Hex(canonical)
	}
	return result, nil
}

func configurationHash(alias string, model config.ModelConfig, capabilities config.ResolvedCapabilities) string {
	value := struct {
		Alias              string   `json:"alias"`
		DisplayName        string   `json:"display_name"`
		BedrockModelID     string   `json:"bedrock_model_id"`
		MetadataProfile    string   `json:"metadata_profile"`
		MetadataRevision   string   `json:"metadata_revision"`
		ResponsesAPI       bool     `json:"responses_api"`
		ContextWindow      int64    `json:"context_window"`
		MaxOutputTokens    int64    `json:"max_output_tokens"`
		InputModalities    []string `json:"input_modalities"`
		ReasoningSupported bool     `json:"reasoning_supported"`
		ReasoningEfforts   []string `json:"reasoning_efforts"`
		FunctionCalling    bool     `json:"function_calling"`
		ParallelCalls      bool     `json:"parallel_calls"`
	}{
		Alias: alias, DisplayName: strings.TrimSpace(model.DisplayName), BedrockModelID: strings.TrimSpace(model.BedrockModelID),
		MetadataProfile: capabilities.MetadataProfile, MetadataRevision: capabilities.MetadataRevision,
		ResponsesAPI:  capabilities.ResponsesSupported,
		ContextWindow: capabilities.ContextWindow, MaxOutputTokens: capabilities.MaxOutputTokens,
		InputModalities: append([]string(nil), capabilities.InputModalities...), ReasoningSupported: capabilities.ReasoningSupported,
		ReasoningEfforts: append([]string(nil), capabilities.ReasoningEfforts...), FunctionCalling: capabilities.FunctionCalling,
		ParallelCalls: capabilities.ParallelCalls,
	}
	encoded, _ := json.Marshal(value)
	return sha256Hex(encoded)
}

func reasoningDescription(effort string) string {
	switch effort {
	case "none":
		return "Do not request adjustable reasoning"
	case "minimal":
		return "Use minimal reasoning"
	case "low":
		return "Use lighter reasoning"
	case "medium":
		return "Balance speed and reasoning depth"
	case "high":
		return "Use greater reasoning depth"
	case "xhigh":
		return "Use extra-high reasoning depth"
	case "max":
		return "Use maximum supported reasoning depth"
	default:
		return effort
	}
}

func replaceModel(models []json.RawMessage, alias string, replacement json.RawMessage) ([]json.RawMessage, error) {
	result := make([]json.RawMessage, 0, len(models)+1)
	for _, raw := range models {
		var identity catalogIdentity
		if err := json.Unmarshal(raw, &identity); err != nil || strings.TrimSpace(identity.Slug) == "" {
			return nil, errors.New("bundled Codex catalog contains a model without a valid slug")
		}
		if identity.Slug == alias {
			continue
		}
		result = append(result, append(json.RawMessage(nil), raw...))
	}
	return append(result, replacement), nil
}

func writeCatalogAtomic(path string, document catalogDocument, validate func(string) error) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".models-*.json")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(document); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if validate != nil {
		if err := validate(temporaryPath); err != nil {
			return err
		}
	}
	return os.Rename(temporaryPath, path)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
