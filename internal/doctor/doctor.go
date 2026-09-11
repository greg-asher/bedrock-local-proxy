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
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
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
	Config       config.Config
	ConfigPath   string
	Client       string
	ModelAlias   string
	CatalogPath  string
	ProfilePath  string
	CodexCommand string
	CodexRunner  codex.CommandRunner
	Live         bool
	HTTPClient   HTTPDoer
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
	if client != "" && client != "codex" {
		result.Checks = append(result.Checks, Check{Name: "client", Status: Failed, Category: "configuration", Message: fmt.Sprintf("unsupported client %q; use codex", client)})
		return result
	}
	alias, model, err := selectedModel(options.Config.Models, options.ModelAlias)
	if err != nil {
		result.Checks = append(result.Checks, Check{Name: "model selection", Status: Failed, Category: "configuration", Message: err.Error()})
		return result
	}
	capabilities, warnings, err := config.ResolveModelCapabilities(model)
	if err != nil {
		result.Checks = append(result.Checks, Check{Name: "model capabilities", Status: Failed, Category: "model_capability", Message: err.Error()})
		return result
	}
	status := Passed
	message := fmt.Sprintf("%s: context=%d output=%d modalities=%s", alias, capabilities.ContextWindow, capabilities.MaxOutputTokens, strings.Join(capabilities.InputModalities, ","))
	if len(warnings) != 0 {
		status = Warning
		message += "; review overrides: " + strings.Join(warnings, "; ")
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
	if !contains(aliases, alias) {
		result.Checks = append(result.Checks, Check{Name: "Codex catalog", Status: Failed, Category: "codex_compatibility", Message: fmt.Sprintf("catalog does not contain model %q; regenerate it", alias)})
		return result
	}
	if err := codex.ValidateConfiguration(metadata, alias, model, capabilities); err != nil {
		result.Checks = append(result.Checks, Check{Name: "Codex catalog configuration", Status: Failed, Category: "codex_compatibility", Message: err.Error()})
		return result
	}
	result.Checks = append(result.Checks, Check{Name: "Codex catalog configuration", Status: Passed, Category: "codex_compatibility", Message: "generated metadata matches the current proxy configuration"})
	result.Checks = append(result.Checks, Check{Name: "Codex catalog", Status: Passed, Category: "codex_compatibility", Message: catalogPath})

	version, bundledHash, err := codex.Probe(ctx, options.CodexCommand, options.CodexRunner)
	if err != nil {
		result.Checks = append(result.Checks, Check{Name: "Codex CLI", Status: Failed, Category: "codex_compatibility", Message: err.Error()})
		return result
	}
	if version != metadata.CodexVersion || bundledHash != metadata.BundledCatalogHash {
		result.Checks = append(result.Checks, Check{Name: "Codex catalog drift", Status: Failed, Category: "codex_compatibility", Message: fmt.Sprintf("generated for Codex %s but installed Codex is %s; regenerate the catalog", metadata.CodexVersion, version)})
		return result
	}
	result.Checks = append(result.Checks, Check{Name: "Codex catalog drift", Status: Passed, Category: "codex_compatibility", Message: "generated catalog matches installed Codex " + version})
	if err := codex.ValidateWithCodex(ctx, options.CodexCommand, catalogPath, []string{alias}, options.CodexRunner); err != nil {
		result.Checks = append(result.Checks, Check{Name: "Codex catalog parsing", Status: Failed, Category: "codex_compatibility", Message: err.Error()})
		return result
	}
	result.Checks = append(result.Checks, Check{Name: "Codex catalog parsing", Status: Passed, Category: "codex_compatibility", Message: "installed Codex accepts the generated catalog"})
	if metadata.OutputLimitVisible {
		result.Checks = append(result.Checks, Check{Name: "Codex output ceiling", Status: Passed, Category: "codex_compatibility", Message: fmt.Sprintf("Codex consumes the configured %d-token output ceiling", capabilities.MaxOutputTokens)})
	} else {
		message := fmt.Sprintf("Codex %s does not expose max_output_tokens in its model catalog; the proxy enforces the %d-token ceiling", metadata.CodexVersion, capabilities.MaxOutputTokens)
		if model.MaxTokens != nil {
			message += fmt.Sprintf(" and inserts the configured %d-token request default", *model.MaxTokens)
		}
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

func selectedModel(models map[string]config.ModelConfig, requested string) (string, config.ModelConfig, error) {
	requested = strings.TrimSpace(requested)
	if requested != "" {
		model, ok := models[requested]
		if !ok {
			return "", config.ModelConfig{}, fmt.Errorf("unknown model %q", requested)
		}
		return requested, model, nil
	}
	if len(models) != 1 {
		names := make([]string, 0, len(models))
		for name := range models {
			names = append(names, name)
		}
		sort.Strings(names)
		return "", config.ModelConfig{}, fmt.Errorf("--model is required when configuration contains multiple models (configured: %s)", strings.Join(names, ", "))
	}
	for alias, model := range models {
		return alias, model, nil
	}
	return "", config.ModelConfig{}, errors.New("configuration contains no models")
}

type profileFile struct {
	Model            string                     `toml:"model"`
	ModelProvider    string                     `toml:"model_provider"`
	ModelCatalogJSON string                     `toml:"model_catalog_json"`
	WebSearch        string                     `toml:"web_search"`
	Tools            profileTools               `toml:"tools"`
	ModelProviders   map[string]profileProvider `toml:"model_providers"`
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

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
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
