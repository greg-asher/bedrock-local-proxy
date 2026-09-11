// Package doctor performs configuration and compatibility checks without
// loading AWS credentials unless the caller explicitly selects live checks.
package doctor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/gregasher/bedrock-local-proxy/internal/claude"
	"github.com/gregasher/bedrock-local-proxy/internal/codex"
	"github.com/gregasher/bedrock-local-proxy/internal/config"
)

type Status string

const (
	Passed  Status = "pass"
	Warning Status = "warning"
	Failed  Status = "fail"
)

type Check struct {
	Name     string `json:"name"`
	Status   Status `json:"status"`
	Category string `json:"category"`
	Message  string `json:"message"`
}

type Result struct {
	Checks []Check `json:"checks"`
}

func (r Result) OK() bool {
	for _, check := range r.Checks {
		if check.Status == Failed {
			return false
		}
	}
	return true
}

type Options struct {
	Config             config.Config
	ConfigPath         string
	Client             string
	ModelAlias         string
	CatalogPath        string
	ProfilePath        string
	CodexCommand       string
	CodexRunner        codex.CommandRunner
	ClaudeSettingsPath string
	ClaudeCommand      string
	ClaudeRunner       claude.CommandRunner
	Live               bool
	HTTPClient         HTTPDoer
}

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

func Run(ctx context.Context, options Options) Result {
	result := Result{}
	result.Checks = append(result.Checks, Check{Name: "configuration", Status: Passed, Category: "configuration", Message: "configuration version 1 is valid"})
	result.Checks = append(result.Checks, reportDirectoryCheck(options.ConfigPath, options.Config))
	client := strings.TrimSpace(options.Client)
	if client == "" && !options.Live {
		return result
	}
	if client != "" && client != "codex" && client != "claude" {
		result.Checks = append(result.Checks, Check{Name: "client", Status: Failed, Category: "configuration", Message: fmt.Sprintf("unsupported client %q; use codex or claude", client)})
		return result
	}
	if client == "claude" {
		result.Checks = append(result.Checks, runClaude(ctx, options)...)
		return result
	}
	if _, err := codex.ResolveCatalogModels(options.Config, options.ModelAlias); err != nil {
		category := "configuration"
		if strings.Contains(err.Error(), "capabilit") || strings.Contains(err.Error(), "Codex-compatible") || strings.Contains(err.Error(), "Responses API") {
			category = "model_capability"
		}
		result.Checks = append(result.Checks, Check{Name: "model selection", Status: Failed, Category: category, Message: err.Error()})
		return result
	}
	version, bundledHash, err := codex.Probe(ctx, options.CodexCommand, options.CodexRunner)
	if err != nil {
		result.Checks = append(result.Checks, Check{Name: "Codex CLI", Status: Failed, Category: "codex_compatibility", Message: err.Error()})
		return result
	}
	resolved, err := codex.ResolveCatalogModelsForVersion(options.Config, options.ModelAlias, version)
	if err != nil {
		category := "configuration"
		if strings.Contains(err.Error(), "capabilit") || strings.Contains(err.Error(), "Codex-compatible") || strings.Contains(err.Error(), "Responses API") {
			category = "model_capability"
		}
		result.Checks = append(result.Checks, Check{Name: "model selection", Status: Failed, Category: category, Message: err.Error()})
		return result
	}
	alias := resolved.DefaultAlias
	capabilities := resolved.Capabilities[alias]
	status := Passed
	message := fmt.Sprintf("default=%s; eligible=%s", alias, strings.Join(resolved.Aliases, ","))
	if len(resolved.Skipped) != 0 {
		status = Warning
		message += "; skipped=" + formatSkipped(resolved.Skipped)
	}
	if len(resolved.Warnings) != 0 {
		status = Warning
		message += "; review overrides: " + strings.Join(resolved.Warnings, "; ")
	}
	result.Checks = append(result.Checks, Check{Name: "model capabilities", Status: status, Category: "model_capability", Message: message})

	profilePath := strings.TrimSpace(options.ProfilePath)
	if profilePath == "" {
		profilePath, err = DefaultProfilePath()
		if err != nil {
			result.Checks = append(result.Checks, Check{Name: "Codex profile", Status: Failed, Category: "codex_compatibility", Message: err.Error()})
			return result
		}
	}
	catalogPath := strings.TrimSpace(options.CatalogPath)
	if catalogPath == "" {
		if configured, parseErr := profileCatalogPath(profilePath); parseErr == nil {
			catalogPath = configured
		}
	}
	if catalogPath == "" {
		catalogPath, err = codex.DefaultCatalogPath(options.ConfigPath)
		if err != nil {
			result.Checks = append(result.Checks, Check{Name: "Codex catalog", Status: Failed, Category: "codex_compatibility", Message: err.Error()})
			return result
		}
	}
	metadata, aliases, err := codex.Inspect(catalogPath)
	if err != nil {
		result.Checks = append(result.Checks, Check{Name: "Codex catalog", Status: Failed, Category: "codex_compatibility", Message: fmt.Sprintf("%v; run bedrock-proxy configure codex", err)})
		return result
	}
	if err := codex.ValidateCatalogConfigurations(metadata, resolved); err != nil {
		result.Checks = append(result.Checks, Check{Name: "Codex catalog configuration", Status: Failed, Category: "codex_compatibility", Message: err.Error()})
		return result
	}
	result.Checks = append(result.Checks, Check{Name: "Codex catalog configuration", Status: Passed, Category: "codex_compatibility", Message: "generated metadata matches all eligible proxy models: " + strings.Join(aliases, ", ")})
	result.Checks = append(result.Checks, Check{Name: "Codex catalog", Status: Passed, Category: "codex_compatibility", Message: catalogPath})

	if version != metadata.CodexVersion || bundledHash != metadata.BundledCatalogHash {
		result.Checks = append(result.Checks, Check{Name: "Codex catalog drift", Status: Failed, Category: "codex_compatibility", Message: fmt.Sprintf("generated for Codex %s but installed Codex is %s; regenerate the catalog", metadata.CodexVersion, version)})
		return result
	}
	result.Checks = append(result.Checks, Check{Name: "Codex catalog drift", Status: Passed, Category: "codex_compatibility", Message: "generated catalog matches installed Codex " + version})
	if err := codex.ValidateWithCodex(ctx, options.CodexCommand, catalogPath, aliases, options.CodexRunner); err != nil {
		result.Checks = append(result.Checks, Check{Name: "Codex catalog parsing", Status: Failed, Category: "codex_compatibility", Message: err.Error()})
		return result
	}
	result.Checks = append(result.Checks, Check{Name: "Codex catalog parsing", Status: Passed, Category: "codex_compatibility", Message: "installed Codex accepts the generated catalog"})
	if metadata.OutputLimitVisible {
		result.Checks = append(result.Checks, Check{Name: "Codex output ceiling", Status: Passed, Category: "codex_compatibility", Message: "Codex consumes configured output ceilings for all catalog models"})
	} else {
		ceilings := make([]string, 0, len(resolved.Aliases))
		for _, localAlias := range resolved.Aliases {
			ceilings = append(ceilings, fmt.Sprintf("%s=%d", localAlias, resolved.Capabilities[localAlias].MaxOutputTokens))
		}
		message := fmt.Sprintf("Codex %s does not expose max_output_tokens in its model catalog; the proxy enforces configured ceilings (%s)", metadata.CodexVersion, strings.Join(ceilings, ", "))
		result.Checks = append(result.Checks, Check{Name: "Codex output ceiling", Status: Warning, Category: "codex_compatibility", Message: message})
	}

	if err := validateProfile(profilePath, alias, catalogPath, options.Config.Listen, metadata.CatalogHash); err != nil {
		result.Checks = append(result.Checks, Check{Name: "Codex profile", Status: Failed, Category: "codex_compatibility", Message: fmt.Sprintf("%v; save the TOML printed by bedrock-proxy configure codex", err)})
		return result
	}
	result.Checks = append(result.Checks, Check{Name: "Codex profile", Status: Passed, Category: "codex_compatibility", Message: profilePath})

	if options.Live {
		result.Checks = append(result.Checks, runLive(ctx, options, alias, capabilities)...)
	}
	return result
}

func runClaude(ctx context.Context, options Options) []Check {
	checks := []Check{}
	version, err := claude.Probe(ctx, options.ClaudeCommand, options.ClaudeRunner)
	if err != nil {
		return append(checks, Check{Name: "Claude CLI", Status: Failed, Category: "claude_compatibility", Message: err.Error()})
	}
	resolved, err := claude.ResolveModels(options.Config, options.ModelAlias)
	if err != nil {
		category := "configuration"
		if strings.Contains(err.Error(), "capabilit") || strings.Contains(err.Error(), "Claude-compatible") || strings.Contains(err.Error(), "Messages API") {
			category = "model_capability"
		}
		return append(checks, Check{Name: "model selection", Status: Failed, Category: category, Message: err.Error()})
	}
	status := Passed
	message := fmt.Sprintf("default=%s; subagent=%s; eligible=%s", resolved.DefaultAlias, resolved.SubagentAlias, strings.Join(resolved.Aliases, ","))
	if len(resolved.Skipped) != 0 {
		status = Warning
		message += "; skipped=" + formatClaudeSkipped(resolved.Skipped)
	}
	if len(resolved.Warnings) != 0 {
		status = Warning
		message += "; review overrides: " + strings.Join(resolved.Warnings, "; ")
	}
	checks = append(checks, Check{Name: "model capabilities", Status: status, Category: "model_capability", Message: message})

	settingsPath := strings.TrimSpace(options.ClaudeSettingsPath)
	if settingsPath == "" {
		settingsPath, err = claude.DefaultSettingsPath(options.ConfigPath)
		if err != nil {
			return append(checks, Check{Name: "Claude settings", Status: Failed, Category: "claude_compatibility", Message: err.Error()})
		}
	}
	hash, err := claude.ValidateSettings(settingsPath, options.Config.Listen, resolved)
	if err != nil {
		return append(checks, Check{Name: "Claude settings", Status: Failed, Category: "claude_compatibility", Message: fmt.Sprintf("%v; run bedrock-proxy configure claude", err)})
	}
	checks = append(checks, Check{Name: "Claude settings", Status: Passed, Category: "claude_compatibility", Message: settingsPath + " (hash " + hash + ")"})
	if err := claude.ValidateWithClaude(ctx, options.ClaudeCommand, settingsPath, options.ClaudeRunner); err != nil {
		return append(checks, Check{Name: "Claude settings parsing", Status: Failed, Category: "claude_compatibility", Message: err.Error()})
	}
	if err := claude.ValidateOfflineRequest(ctx, options.ClaudeCommand, settingsPath, options.ClaudeRunner); err != nil {
		return append(checks, Check{Name: "Claude offline request", Status: Failed, Category: "claude_compatibility", Message: err.Error()})
	}
	checks = append(checks,
		Check{Name: "Claude CLI", Status: Passed, Category: "claude_compatibility", Message: "installed Claude Code " + version + " supports generated multi-model settings"},
		Check{Name: "Claude settings parsing", Status: Passed, Category: "claude_compatibility", Message: "installed Claude Code accepts the generated settings"},
		Check{Name: "Claude offline request", Status: Passed, Category: "claude_compatibility", Message: "isolated Claude Code request used the configured alias without AWS credentials"},
	)
	if options.Live {
		alias := resolved.DefaultAlias
		checks = append(checks, runLiveClaude(ctx, options, alias, resolved.Capabilities[alias], hash)...)
	}
	return checks
}

func formatClaudeSkipped(models []claude.SkippedModel) string {
	values := make([]string, 0, len(models))
	for _, model := range models {
		values = append(values, model.Alias+" ("+model.Reason+")")
	}
	return strings.Join(values, "; ")
}

func DefaultProfilePath() (string, error) {
	if home := strings.TrimSpace(os.Getenv("CODEX_HOME")); home != "" {
		if !filepath.IsAbs(home) {
			return "", errors.New("CODEX_HOME must be an absolute path")
		}
		return filepath.Join(home, codex.DefaultProfileName+".config.toml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".codex", codex.DefaultProfileName+".config.toml"), nil
}

func reportDirectoryCheck(configPath string, cfg config.Config) Check {
	path, err := config.ResolveReportDirectory(configPath, cfg.Reporting.Directory, "")
	if err != nil {
		return Check{Name: "report directory", Status: Failed, Category: "configuration", Message: err.Error()}
	}
	ancestor := path
	for {
		info, statErr := os.Stat(ancestor)
		if statErr == nil {
			if !info.IsDir() {
				return Check{Name: "report directory", Status: Failed, Category: "configuration", Message: ancestor + " is not a directory"}
			}
			break
		}
		if !os.IsNotExist(statErr) {
			return Check{Name: "report directory", Status: Failed, Category: "configuration", Message: statErr.Error()}
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			break
		}
		ancestor = parent
	}
	probe, err := os.CreateTemp(ancestor, ".bedrock-proxy-doctor-*")
	if err != nil {
		return Check{Name: "report directory", Status: Failed, Category: "configuration", Message: fmt.Sprintf("cannot write report directory through %s: %v", ancestor, err)}
	}
	probePath := probe.Name()
	if closeErr := probe.Close(); closeErr != nil {
		_ = os.Remove(probePath)
		return Check{Name: "report directory", Status: Failed, Category: "configuration", Message: fmt.Sprintf("cannot close report directory probe in %s: %v", ancestor, closeErr)}
	}
	if removeErr := os.Remove(probePath); removeErr != nil {
		return Check{Name: "report directory", Status: Failed, Category: "configuration", Message: fmt.Sprintf("cannot remove report directory probe in %s: %v", ancestor, removeErr)}
	}
	return Check{Name: "report directory", Status: Passed, Category: "configuration", Message: path}
}

func formatSkipped(models []codex.SkippedModel) string {
	values := make([]string, 0, len(models))
	for _, model := range models {
		values = append(values, model.Alias+" ("+model.Reason+")")
	}
	return strings.Join(values, "; ")
}

type profileFile struct {
	Model            string                     `toml:"model"`
	ModelProvider    string                     `toml:"model_provider"`
	ModelCatalogJSON string                     `toml:"model_catalog_json"`
	WebSearch        string                     `toml:"web_search"`
	Tools            profileTools               `toml:"tools"`
	Features         profileFeatures            `toml:"features"`
	ModelProviders   map[string]profileProvider `toml:"model_providers"`
}

type profileFeatures struct {
	Apps *bool `toml:"apps"`
}

type profileTools struct {
	WebSearch *bool `toml:"web_search"`
}

type profileProvider struct {
	BaseURL                     string            `toml:"base_url"`
	WireAPI                     string            `toml:"wire_api"`
	HTTPHeaders                 map[string]string `toml:"http_headers"`
	RequiresOpenAIAuth          *bool             `toml:"requires_openai_auth"`
	SupportsStandaloneWebSearch *bool             `toml:"supports_standalone_web_search"`
	SupportsWebsockets          *bool             `toml:"supports_websockets"`
	RequestMaxRetries           *int              `toml:"request_max_retries"`
	StreamMaxRetries            *int              `toml:"stream_max_retries"`
}

func readProfile(path string) (profileFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return profileFile{}, fmt.Errorf("read %s: %w", path, err)
	}
	var profile profileFile
	if _, err := toml.Decode(string(data), &profile); err != nil {
		return profileFile{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return profile, nil
}

func validateProfile(path, alias, catalogPath, listen, catalogHash string) error {
	profile, err := readProfile(path)
	if err != nil {
		return err
	}
	if profile.Model != alias {
		return fmt.Errorf("profile %s selects model %q, want %q", path, profile.Model, alias)
	}
	if profile.ModelProvider != codex.ProviderID {
		return fmt.Errorf("profile %s selects provider %q, want %q", path, profile.ModelProvider, codex.ProviderID)
	}
	if profile.ModelCatalogJSON != catalogPath || !filepath.IsAbs(profile.ModelCatalogJSON) {
		return fmt.Errorf("profile %s model_catalog_json must be the absolute path %q", path, catalogPath)
	}
	if profile.WebSearch != "disabled" || profile.Tools.WebSearch == nil || *profile.Tools.WebSearch {
		return fmt.Errorf("profile %s must disable hosted web search", path)
	}
	if profile.Features.Apps == nil || *profile.Features.Apps {
		return fmt.Errorf("profile %s must disable ChatGPT apps so their internal codex_apps MCP does not interrupt local-provider startup; configured MCP servers remain available", path)
	}
	provider, ok := profile.ModelProviders[codex.ProviderID]
	if !ok {
		return fmt.Errorf("profile %s is missing model_providers.%s", path, codex.ProviderID)
	}
	if provider.BaseURL != "http://"+listen+"/v1" || provider.WireAPI != "responses" {
		return fmt.Errorf("profile %s must use the Responses API at http://%s/v1", path, listen)
	}
	if provider.HTTPHeaders["Authorization"] != "Bearer local" || provider.HTTPHeaders["X-Bedrock-Proxy-Catalog"] != catalogHash {
		return fmt.Errorf("profile %s has incorrect local provider headers", path)
	}
	if provider.RequiresOpenAIAuth == nil || *provider.RequiresOpenAIAuth || provider.SupportsStandaloneWebSearch == nil || *provider.SupportsStandaloneWebSearch || provider.SupportsWebsockets == nil || *provider.SupportsWebsockets {
		return fmt.Errorf("profile %s has incorrect provider capability flags", path)
	}
	if provider.RequestMaxRetries == nil || *provider.RequestMaxRetries != 0 || provider.StreamMaxRetries == nil || *provider.StreamMaxRetries != 0 {
		return fmt.Errorf("profile %s must disable provider retries", path)
	}
	return nil
}

func profileCatalogPath(path string) (string, error) {
	profile, err := readProfile(path)
	if err != nil {
		return "", err
	}
	if profile.ModelCatalogJSON == "" {
		return "", errors.New("model_catalog_json is missing")
	}
	if !filepath.IsAbs(profile.ModelCatalogJSON) {
		return "", errors.New("model_catalog_json must be an absolute quoted path")
	}
	return profile.ModelCatalogJSON, nil
}

func runLive(ctx context.Context, options Options, alias string, capabilities config.ResolvedCapabilities) []Check {
	client := options.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 90 * time.Second}
	}
	baseURL := "http://" + options.Config.Listen
	checks := []Check{}
	if err := checkModels(ctx, client, baseURL, alias); err != nil {
		return append(checks, liveFailure("live model discovery", err))
	}
	checks = append(checks, Check{Name: "live model discovery", Status: Passed, Category: "protocol", Message: "list and retrieve routes contain " + alias})
	if err := checkNormalResponse(ctx, client, baseURL, alias, capabilities.MaxOutputTokens); err != nil {
		return append(checks, liveFailure("live nonstreaming response", err))
	}
	checks = append(checks, Check{Name: "live nonstreaming response", Status: Passed, Category: "protocol", Message: "completed response with usage"})
	if err := checkFunctionLoop(ctx, client, baseURL, alias, capabilities.MaxOutputTokens); err != nil {
		return append(checks, liveFailure("live function tool loop", err))
	}
	checks = append(checks, Check{Name: "live function tool loop", Status: Passed, Category: "model_capability", Message: "function call and result continuation completed"})
	if completed, err := checkStreamingCancellation(ctx, client, baseURL, alias, capabilities.MaxOutputTokens); err != nil {
		return append(checks, liveFailure("live streaming cancellation", err))
	} else if completed {
		checks = append(checks, Check{Name: "live streaming cancellation", Status: Warning, Category: "protocol", Message: "stream completed before cancellation could be observed"})
	} else {
		checks = append(checks, Check{Name: "live streaming cancellation", Status: Passed, Category: "protocol", Message: "stream opened and canceled cleanly"})
	}
	return checks
}

func runLiveClaude(ctx context.Context, options Options, alias string, capabilities config.ResolvedCapabilities, settingsHash string) []Check {
	client := options.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 90 * time.Second}
	}
	baseURL := "http://" + options.Config.Listen
	checks := []Check{}
	if err := checkModels(ctx, client, baseURL, alias); err != nil {
		return append(checks, liveFailure("live model discovery", err))
	}
	checks = append(checks, Check{Name: "live model discovery", Status: Passed, Category: "protocol", Message: "list and retrieve routes contain " + alias})
	if err := checkClaudeNormal(ctx, client, baseURL, alias, capabilities.MaxOutputTokens, settingsHash); err != nil {
		return append(checks, liveFailure("live Claude response", err))
	}
	checks = append(checks, Check{Name: "live Claude response", Status: Passed, Category: "protocol", Message: "completed Messages response with usage"})
	if err := checkClaudeFunctionLoop(ctx, client, baseURL, alias, capabilities.MaxOutputTokens, settingsHash); err != nil {
		return append(checks, liveFailure("live Claude tool loop", err))
	}
	checks = append(checks, Check{Name: "live Claude tool loop", Status: Passed, Category: "model_capability", Message: "tool use and result continuation completed"})
	if capabilities.ReasoningSupported {
		if err := checkClaudeReasoning(ctx, client, baseURL, alias, capabilities.MaxOutputTokens, capabilities.ReasoningEfforts, settingsHash); err != nil {
			return append(checks, liveFailure("live Claude reasoning", err))
		}
		checks = append(checks, Check{Name: "live Claude reasoning", Status: Passed, Category: "model_capability", Message: "configured effort request completed with usage"})
	}
	if completed, err := checkClaudeStreamingCancellation(ctx, client, baseURL, alias, capabilities.MaxOutputTokens, settingsHash); err != nil {
		return append(checks, liveFailure("live Claude streaming cancellation", err))
	} else if completed {
		checks = append(checks, Check{Name: "live Claude streaming cancellation", Status: Warning, Category: "protocol", Message: "stream completed before cancellation could be observed"})
	} else {
		checks = append(checks, Check{Name: "live Claude streaming cancellation", Status: Passed, Category: "protocol", Message: "stream opened and canceled cleanly"})
	}
	return checks
}

func checkClaudeNormal(ctx context.Context, client HTTPDoer, baseURL, alias string, ceiling int64, settingsHash string) error {
	body := map[string]any{
		"model": alias, "max_tokens": diagnosticOutputLimit(ceiling, 16),
		"messages": []any{map[string]any{"role": "user", "content": "Reply with OK."}},
	}
	response, err := doClaudeJSON(ctx, client, baseURL+"/v1/messages", body, settingsHash)
	if err != nil {
		return err
	}
	return validateClaudeMessage(response)
}

func checkClaudeFunctionLoop(ctx context.Context, client HTTPDoer, baseURL, alias string, ceiling int64, settingsHash string) error {
	tool := map[string]any{
		"name": "bedrock_proxy_doctor", "description": "Return a diagnostic marker supplied by the caller.",
		"input_schema": map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string"}}, "required": []string{"value"}},
	}
	first := map[string]any{
		"model": alias, "max_tokens": diagnosticOutputLimit(ceiling, 64),
		"messages": []any{map[string]any{"role": "user", "content": "Call bedrock_proxy_doctor once with value ok."}},
		"tools":    []any{tool}, "tool_choice": map[string]any{"type": "tool", "name": "bedrock_proxy_doctor"},
	}
	response, err := doClaudeJSON(ctx, client, baseURL+"/v1/messages", first, settingsHash)
	if err != nil {
		return err
	}
	data, err := readResponse(response)
	if err != nil {
		return err
	}
	var result struct {
		Content []struct {
			Type  string         `json:"type"`
			ID    string         `json:"id"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(data, &result) != nil || len(result.Usage) == 0 {
		return errors.New("tool response was malformed or missing usage")
	}
	var call map[string]any
	for _, block := range result.Content {
		if block.Type == "tool_use" && block.ID != "" && block.Name == "bedrock_proxy_doctor" {
			call = map[string]any{"type": block.Type, "id": block.ID, "name": block.Name, "input": block.Input}
			break
		}
	}
	if call == nil {
		return errors.New("model did not return the required tool_use block")
	}
	second := map[string]any{
		"model": alias, "max_tokens": diagnosticOutputLimit(ceiling, 64), "tools": []any{tool},
		"messages": []any{
			map[string]any{"role": "user", "content": "Call bedrock_proxy_doctor once with value ok."},
			map[string]any{"role": "assistant", "content": []any{call}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": call["id"], "content": "ok"}}},
		},
	}
	response, err = doClaudeJSON(ctx, client, baseURL+"/v1/messages", second, settingsHash)
	if err != nil {
		return err
	}
	return validateClaudeMessage(response)
}

func checkClaudeReasoning(ctx context.Context, client HTTPDoer, baseURL, alias string, ceiling int64, efforts []string, settingsHash string) error {
	effort := preferredReasoningEffort(efforts)
	if effort == "" {
		return errors.New("reasoning is enabled but no effort is configured")
	}
	body := map[string]any{
		"model": alias, "max_tokens": diagnosticOutputLimit(ceiling, 64),
		"output_config": map[string]any{"effort": effort},
		"messages":      []any{map[string]any{"role": "user", "content": "Reply with OK."}},
	}
	response, err := doClaudeJSON(ctx, client, baseURL+"/v1/messages", body, settingsHash)
	if err != nil {
		return err
	}
	return validateClaudeMessage(response)
}

func preferredReasoningEffort(efforts []string) string {
	for _, effort := range efforts {
		if effort == "high" {
			return effort
		}
	}
	if len(efforts) != 0 {
		return efforts[0]
	}
	return ""
}

func checkClaudeStreamingCancellation(ctx context.Context, client HTTPDoer, baseURL, alias string, ceiling int64, settingsHash string) (bool, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	body := map[string]any{
		"model": alias, "max_tokens": diagnosticOutputLimit(ceiling, 512), "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "Write a numbered list of 100 short items."}},
	}
	data, err := json.Marshal(body)
	if err != nil {
		return false, err
	}
	response, err := doClaudeRequest(streamCtx, client, baseURL+"/v1/messages", data, settingsHash)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
		return false, &liveError{Status: response.StatusCode}
	}
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "event:") {
			continue
		}
		if strings.Contains(line, `"type":"message_stop"`) || strings.Contains(line, `"type": "message_stop"`) {
			return true, nil
		}
		cancel()
		if scanner.Scan() {
			return false, errors.New("stream continued after cancellation")
		}
		return false, nil
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return false, err
	}
	return false, errors.New("stream ended before emitting an event")
}

func validateClaudeMessage(response *http.Response) error {
	data, err := readResponse(response)
	if err != nil {
		return err
	}
	var result struct {
		Type  string          `json:"type"`
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(data, &result) != nil || result.Type != "message" || len(result.Usage) == 0 || string(result.Usage) == "null" {
		return errors.New("Messages response was not a message with usage")
	}
	return nil
}

func doClaudeJSON(ctx context.Context, client HTTPDoer, url string, body any, settingsHash string) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return doClaudeRequest(ctx, client, url, data, settingsHash)
}

func doClaudeRequest(ctx context.Context, client HTTPDoer, url string, body []byte, settingsHash string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", "local")
	request.Header.Set("anthropic-version", "2023-06-01")
	if settingsHash != "" {
		request.Header.Set(claude.SettingsHeader, settingsHash)
	}
	return client.Do(request)
}

type liveError struct {
	Status int
}

func (e *liveError) Error() string { return fmt.Sprintf("proxy returned HTTP %d", e.Status) }

func liveFailure(name string, err error) Check {
	category := "protocol"
	var live *liveError
	if errors.As(err, &live) {
		switch live.Status {
		case http.StatusUnauthorized:
			category = "aws_credentials"
		case http.StatusForbidden:
			category = "bedrock_access"
		case http.StatusBadRequest:
			category = "model_capability"
		}
	}
	return Check{Name: name, Status: Failed, Category: category, Message: err.Error()}
}

func checkModels(ctx context.Context, client HTTPDoer, baseURL, alias string) error {
	for _, path := range []string{"/v1/models", "/v1/models/" + alias} {
		response, err := doRequest(ctx, client, http.MethodGet, baseURL+path, nil)
		if err != nil {
			return err
		}
		data, err := readResponse(response)
		if err != nil {
			return err
		}
		if !bytes.Contains(data, []byte(alias)) {
			return fmt.Errorf("%s did not contain model %q", path, alias)
		}
	}
	return nil
}

func checkNormalResponse(ctx context.Context, client HTTPDoer, baseURL, alias string, ceiling int64) error {
	body := map[string]any{"model": alias, "input": "Reply with OK.", "max_output_tokens": diagnosticOutputLimit(ceiling, 16), "store": false}
	response, err := doJSON(ctx, client, baseURL+"/v1/responses", body)
	if err != nil {
		return err
	}
	data, err := readResponse(response)
	if err != nil {
		return err
	}
	var result struct {
		Status string          `json:"status"`
		Usage  json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(data, &result) != nil || result.Status != "completed" || len(result.Usage) == 0 || string(result.Usage) == "null" {
		return errors.New("response was not completed with usage")
	}
	return nil
}

func checkFunctionLoop(ctx context.Context, client HTTPDoer, baseURL, alias string, ceiling int64) error {
	tool := map[string]any{
		"type":        "function",
		"name":        "bedrock_proxy_doctor",
		"description": "Return a diagnostic marker supplied by the caller.",
		"parameters": map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"value": map[string]any{"type": "string"}},
			"required":             []string{"value"},
			"additionalProperties": false,
		},
	}
	firstInput := "Call the bedrock_proxy_doctor function once with value ok. Do not answer in text."
	first := map[string]any{
		"model": alias, "input": firstInput, "max_output_tokens": diagnosticOutputLimit(ceiling, 64), "store": false,
		"tools": []any{tool}, "tool_choice": map[string]any{"type": "function", "name": "bedrock_proxy_doctor"},
	}
	response, err := doJSON(ctx, client, baseURL+"/v1/responses", first)
	if err != nil {
		return err
	}
	data, err := readResponse(response)
	if err != nil {
		return err
	}
	var result struct {
		Output []struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"output"`
	}
	if json.Unmarshal(data, &result) != nil {
		return errors.New("function response was not valid JSON")
	}
	var callID, arguments string
	for _, item := range result.Output {
		if item.Type == "function_call" && item.Name == "bedrock_proxy_doctor" && item.CallID != "" {
			callID, arguments = item.CallID, item.Arguments
			break
		}
	}
	if callID == "" {
		return errors.New("model did not return the required function call")
	}
	second := map[string]any{
		"model": alias,
		"input": []any{
			map[string]any{"role": "user", "content": firstInput},
			map[string]any{"type": "function_call", "call_id": callID, "name": "bedrock_proxy_doctor", "arguments": arguments},
			map[string]any{"type": "function_call_output", "call_id": callID, "output": "ok"},
		},
		"max_output_tokens": diagnosticOutputLimit(ceiling, 32), "store": false, "tools": []any{tool}, "tool_choice": "none",
	}
	response, err = doJSON(ctx, client, baseURL+"/v1/responses", second)
	if err != nil {
		return err
	}
	data, err = readResponse(response)
	if err != nil {
		return err
	}
	var final struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(data, &final) != nil || final.Status != "completed" {
		return errors.New("function result continuation did not complete")
	}
	return nil
}

func checkStreamingCancellation(ctx context.Context, client HTTPDoer, baseURL, alias string, ceiling int64) (bool, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	body := map[string]any{"model": alias, "input": "Write a numbered list of 100 short items.", "max_output_tokens": diagnosticOutputLimit(ceiling, 512), "stream": true, "store": false}
	data, err := json.Marshal(body)
	if err != nil {
		return false, err
	}
	response, err := doRequest(streamCtx, client, http.MethodPost, baseURL+"/v1/responses", data)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
		return false, &liveError{Status: response.StatusCode}
	}
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		if strings.Contains(scanner.Text(), "response.completed") {
			return true, nil
		}
		cancel()
		if scanner.Scan() {
			return false, errors.New("stream continued after cancellation")
		}
		return false, nil
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return false, err
	}
	return false, errors.New("stream ended before emitting an event")
}

func diagnosticOutputLimit(ceiling, wanted int64) int64 {
	if ceiling > 0 && ceiling < wanted {
		return ceiling
	}
	return wanted
}

func doJSON(ctx context.Context, client HTTPDoer, url string, body any) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return doRequest(ctx, client, http.MethodPost, url, data)
}

func doRequest(ctx context.Context, client HTTPDoer, method, url string, body []byte) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer local")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return client.Do(request)
}

func readResponse(response *http.Response) ([]byte, error) {
	if response == nil {
		return nil, errors.New("proxy returned no response")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024))
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &liveError{Status: response.StatusCode}
	}
	return data, nil
}
