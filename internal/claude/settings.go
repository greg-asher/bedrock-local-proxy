// Package claude generates and validates Claude Code settings for models
// exposed through Bedrock Local Proxy's Anthropic Messages endpoint.
package claude

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gregasher/bedrock-local-proxy/internal/config"
)

const (
	MinimumVersion = "2.1.242"
	SettingsHeader = "X-Bedrock-Proxy-Claude-Settings"
)

type CommandRunner interface {
	Run(context.Context, string, ...string) ([]byte, []byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	return runCommand(ctx, name, nil, args...)
}

type EnvironmentRunner interface {
	RunWithEnv(context.Context, string, []string, ...string) ([]byte, []byte, error)
}

func (ExecRunner) RunWithEnv(ctx context.Context, name string, environment []string, args ...string) ([]byte, []byte, error) {
	return runCommand(ctx, name, environment, args...)
}

func runCommand(ctx context.Context, name string, environment []string, args ...string) ([]byte, []byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	if environment != nil {
		command.Env = environment
	}
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return []byte(stdout.String()), []byte(stderr.String()), err
}

type GenerateOptions struct {
	Config        config.Config
	ConfigPath    string
	ModelAlias    string
	SettingsPath  string
	ClaudeCommand string
	Runner        CommandRunner
}

type GenerateResult struct {
	SettingsPath  string
	ModelAlias    string
	SubagentAlias string
	ModelAliases  []string
	SkippedModels []SkippedModel
	ClaudeVersion string
	SettingsHash  string
	Warnings      []string
}

type SkippedModel struct {
	Alias  string
	Reason string
}

type Models struct {
	DefaultAlias   string
	SubagentAlias  string
	Aliases        []string
	Models         map[string]config.ModelConfig
	Capabilities   map[string]config.ResolvedCapabilities
	Warnings       []string
	Skipped        []SkippedModel
	CanonicalAlias map[string]string
}

type Settings struct {
	Model          string            `json:"model"`
	ModelOverrides map[string]string `json:"modelOverrides"`
	ModelPicker    ModelPicker       `json:"modelPicker"`
	Env            map[string]string `json:"env"`
}

type ModelPicker struct {
	Options               []ModelOption `json:"options"`
	ReplaceBuiltInOptions bool          `json:"replaceBuiltInOptions"`
}

type ModelOption struct {
	Model       string `json:"model"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

func Generate(ctx context.Context, options GenerateOptions) (GenerateResult, error) {
	runner := options.Runner
	if runner == nil {
		runner = ExecRunner{}
	}
	command := strings.TrimSpace(options.ClaudeCommand)
	if command == "" {
		command = "claude"
	}
	version, err := Probe(ctx, command, runner)
	if err != nil {
		return GenerateResult{}, err
	}
	resolved, err := ResolveModels(options.Config, options.ModelAlias)
	if err != nil {
		return GenerateResult{}, err
	}
	settings, hash := buildSettings(options.Config.Listen, resolved)
	path, err := resolveSettingsPath(options.ConfigPath, options.SettingsPath)
	if err != nil {
		return GenerateResult{}, err
	}
	validate := func(candidate string) error {
		return ValidateWithClaude(ctx, command, candidate, runner)
	}
	if err := writeSettingsAtomic(path, settings, validate); err != nil {
		return GenerateResult{}, fmt.Errorf("write Claude Code settings: %w", err)
	}
	return GenerateResult{
		SettingsPath: path, ModelAlias: resolved.DefaultAlias, SubagentAlias: resolved.SubagentAlias,
		ModelAliases: append([]string(nil), resolved.Aliases...), SkippedModels: append([]SkippedModel(nil), resolved.Skipped...),
		ClaudeVersion: version, SettingsHash: hash, Warnings: append([]string(nil), resolved.Warnings...),
	}, nil
}

func Probe(ctx context.Context, command string, runner CommandRunner) (string, error) {
	if runner == nil {
		runner = ExecRunner{}
	}
	if strings.TrimSpace(command) == "" {
		command = "claude"
	}
	stdout, stderr, err := runner.Run(ctx, command, "--version")
	if err != nil {
		return "", fmt.Errorf("%w; install Claude Code %s or newer for generated settings, or use the manual environment-variable setup", commandError("read Claude Code version", err, stderr), MinimumVersion)
	}
	version := normalizeVersion(string(stdout))
	if version == "" {
		return "", fmt.Errorf("read Claude Code version: unexpected output %q", strings.TrimSpace(string(stdout)))
	}
	if !versionAtLeast(version, MinimumVersion) {
		return "", fmt.Errorf("Claude Code %s is too old for generated multi-model settings; version %s or newer is required, or use the manual environment-variable setup", version, MinimumVersion)
	}
	return version, nil
}

func ResolveModels(cfg config.Config, requested string) (Models, error) {
	result := Models{
		Models: make(map[string]config.ModelConfig), Capabilities: make(map[string]config.ResolvedCapabilities),
		CanonicalAlias: make(map[string]string),
	}
	aliases := make([]string, 0, len(cfg.Models))
	for alias := range cfg.Models {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	for _, alias := range aliases {
		model := cfg.Models[alias]
		if model.Capabilities == nil {
			result.Skipped = append(result.Skipped, SkippedModel{Alias: alias, Reason: "capabilities are required for Claude Code metadata"})
			continue
		}
		capabilities, warnings, err := config.ResolveModelCapabilities(model)
		if err != nil {
			return Models{}, fmt.Errorf("resolve capabilities for model %q: %w", alias, err)
		}
		reason := ""
		switch {
		case !capabilities.MessagesKnown:
			reason = "messages_api is required for Claude Code metadata"
		case !capabilities.MessagesSupported:
			reason = "configured target does not support the Messages API on bedrock-runtime"
		case capabilities.AnthropicModelID == "":
			reason = "anthropic_model_id is required for Claude Code metadata"
		case !capabilities.FunctionCalling:
			reason = "configured target must support client-side function calling for Claude Code"
		}
		if reason != "" {
			result.Skipped = append(result.Skipped, SkippedModel{Alias: alias, Reason: reason})
			continue
		}
		if other, exists := result.CanonicalAlias[capabilities.AnthropicModelID]; exists {
			return Models{}, fmt.Errorf("models %q and %q use the same anthropic_model_id %q; each generated Claude picker entry requires a distinct canonical identity", other, alias, capabilities.AnthropicModelID)
		}
		result.CanonicalAlias[capabilities.AnthropicModelID] = alias
		result.Aliases = append(result.Aliases, alias)
		result.Models[alias] = model
		result.Capabilities[alias] = capabilities
		for _, warning := range warnings {
			result.Warnings = append(result.Warnings, fmt.Sprintf("model %s: %s", alias, warning))
		}
	}

	selected := strings.TrimSpace(requested)
	if selected == "" {
		selected = strings.TrimSpace(cfg.Claude.DefaultModel)
	}
	if selected != "" {
		if _, configured := cfg.Models[selected]; !configured {
			return Models{}, fmt.Errorf("unknown model %q (configured: %s)", selected, strings.Join(aliases, ", "))
		}
		if _, eligible := result.Models[selected]; !eligible {
			return Models{}, fmt.Errorf("selected default model %q is not Claude-compatible: %s", selected, skippedReason(result.Skipped, selected))
		}
	}
	if len(result.Aliases) == 0 {
		return Models{}, fmt.Errorf("configuration contains no Claude-compatible models: %s", formatSkipped(result.Skipped))
	}
	if selected == "" && len(result.Aliases) == 1 {
		selected = result.Aliases[0]
	}
	if selected == "" {
		return Models{}, fmt.Errorf("claude.default_model or --model is required when multiple Claude-compatible models are configured (eligible: %s)", strings.Join(result.Aliases, ", "))
	}
	result.DefaultAlias = selected
	result.SubagentAlias = strings.TrimSpace(cfg.Claude.SubagentModel)
	if result.SubagentAlias == "" {
		result.SubagentAlias = selected
	}
	if _, eligible := result.Models[result.SubagentAlias]; !eligible {
		if _, configured := cfg.Models[result.SubagentAlias]; !configured {
			return Models{}, fmt.Errorf("claude.subagent_model %q is not a configured model", result.SubagentAlias)
		}
		return Models{}, fmt.Errorf("claude.subagent_model %q is not Claude-compatible: %s", result.SubagentAlias, skippedReason(result.Skipped, result.SubagentAlias))
	}
	return result, nil
}

func DefaultSettingsPath(configPath string) (string, error) {
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
	return filepath.Join(filepath.Dir(absolute), "clients", "claude", "settings.json"), nil
}

func Inspect(path string) (Settings, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Settings{}, "", err
	}
	var settings Settings
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&settings); err != nil {
		return Settings{}, "", fmt.Errorf("parse generated Claude Code settings: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return Settings{}, "", errors.New("parse generated Claude Code settings: multiple JSON values are not supported")
	} else if !errors.Is(err, io.EOF) {
		return Settings{}, "", fmt.Errorf("parse generated Claude Code settings: %w", err)
	}
	header := settings.Env["ANTHROPIC_CUSTOM_HEADERS"]
	prefix := SettingsHeader + ": "
	hash := normalizedHash(strings.TrimPrefix(header, prefix))
	if !strings.HasPrefix(header, prefix) || hash == "" {
		return Settings{}, "", errors.New("generated Claude Code settings hash is missing or stale; regenerate the settings")
	}
	return settings, hash, nil
}

func ValidateSettings(path, listen string, resolved Models) (string, error) {
	actual, hash, err := Inspect(path)
	if err != nil {
		return "", err
	}
	expected, expectedHash := buildSettings(listen, resolved)
	actualJSON, _ := json.Marshal(actual)
	expectedJSON, _ := json.Marshal(expected)
	if hash != expectedHash || !bytes.Equal(actualJSON, expectedJSON) {
		return "", errors.New("generated Claude Code settings do not match the current proxy configuration; regenerate the settings")
	}
	return hash, nil
}

func ValidateWithClaude(ctx context.Context, command, path string, runner CommandRunner) error {
	if runner == nil {
		runner = ExecRunner{}
	}
	if strings.TrimSpace(command) == "" {
		command = "claude"
	}
	stdout, stderr, err := runner.Run(ctx, command, "--settings", path, "--version")
	if err != nil {
		return commandError("validate generated Claude Code settings", err, stderr)
	}
	if normalizeVersion(string(stdout)) == "" {
		return fmt.Errorf("validate generated Claude Code settings: unexpected version output %q", strings.TrimSpace(string(stdout)))
	}
	return nil
}

// ValidateOfflineRequest runs Claude Code in an isolated home against a
// process-local fake Messages endpoint. It proves that the installed client
// loads the generated settings and applies modelOverrides without reading AWS
// credentials or contacting Bedrock.
func ValidateOfflineRequest(ctx context.Context, command, path string, runner CommandRunner) error {
	if runner == nil {
		runner = ExecRunner{}
	}
	if strings.TrimSpace(command) == "" {
		command = "claude"
	}
	environmentRunner, ok := runner.(EnvironmentRunner)
	if !ok {
		return errors.New("validate Claude Code offline request: command runner does not support an isolated environment")
	}
	settings, _, err := Inspect(path)
	if err != nil {
		return err
	}
	wantedAlias := settings.ModelOverrides[settings.Model]
	if wantedAlias == "" {
		return errors.New("validate Claude Code offline request: selected model has no override")
	}
	const marker = "BEDROCK_PROXY_OFFLINE_OK"
	var mu sync.Mutex
	sawRequest := false
	sawSettingsHash := false
	observedModels := map[string]struct{}{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/v1/messages") {
			http.NotFound(w, r)
			return
		}
		var request struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 2*1024*1024)).Decode(&request)
		mu.Lock()
		observedModels[request.Model] = struct{}{}
		if request.Model == wantedAlias {
			sawRequest = true
		}
		if normalizedHash(r.Header.Get(SettingsHeader)) != "" {
			sawSettingsHash = true
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "msg_bedrock_proxy_offline", "type": "message", "role": "assistant", "model": request.Model,
			"content": []map[string]string{{"type": "text", "text": marker}}, "stop_reason": "end_turn", "stop_sequence": nil,
			"usage": map[string]int{"input_tokens": 1, "output_tokens": 1},
		})
	}))
	defer server.Close()

	temporaryRoot, err := os.MkdirTemp("", "bedrock-proxy-claude-doctor-*")
	if err != nil {
		return fmt.Errorf("validate Claude Code offline request: %w", err)
	}
	defer os.RemoveAll(temporaryRoot)
	temporarySettings := filepath.Join(temporaryRoot, "settings.json")
	emptyMCPConfig := filepath.Join(temporaryRoot, "mcp.json")
	settings.Env["ANTHROPIC_BASE_URL"] = server.URL
	hash, err := settingsHash(settings)
	if err != nil {
		return err
	}
	settings.Env["ANTHROPIC_CUSTOM_HEADERS"] = SettingsHeader + ": " + hash
	if err := writeSettingsAtomic(temporarySettings, settings, nil); err != nil {
		return fmt.Errorf("validate Claude Code offline request: %w", err)
	}
	if err := os.WriteFile(emptyMCPConfig, []byte("{\"mcpServers\":{}}\n"), 0o600); err != nil {
		return fmt.Errorf("validate Claude Code offline request: %w", err)
	}
	home := filepath.Join(temporaryRoot, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return fmt.Errorf("validate Claude Code offline request: %w", err)
	}
	environment := []string{
		"HOME=" + home,
		"CLAUDE_CONFIG_DIR=" + filepath.Join(home, ".claude"),
		"PATH=" + os.Getenv("PATH"),
		"NO_PROXY=127.0.0.1,localhost",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"DISABLE_TELEMETRY=1",
	}
	requestContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stdout, stderr, err := environmentRunner.RunWithEnv(requestContext, command, environment,
		"--bare", "--settings", temporarySettings, "-p", "--strict-mcp-config", "--mcp-config", emptyMCPConfig,
		"--tools", "", "--output-format", "text", "Reply with "+marker+".")
	if err != nil {
		return commandError("validate Claude Code offline request", err, stderr)
	}
	lowerStderr := strings.ToLower(string(stderr))
	if strings.Contains(lowerStderr, "unrecognized_model") || strings.Contains(lowerStderr, "not a model this version of claude code recognizes") {
		return fmt.Errorf("validate Claude Code offline request: client reported unrecognized model metadata: %s", strings.TrimSpace(string(stderr)))
	}
	mu.Lock()
	requestOK, hashOK := sawRequest, sawSettingsHash
	models := make([]string, 0, len(observedModels))
	for model := range observedModels {
		models = append(models, model)
	}
	mu.Unlock()
	sort.Strings(models)
	if !requestOK {
		return fmt.Errorf("validate Claude Code offline request: client did not send selected proxy alias %q (observed: %s)", wantedAlias, strings.Join(models, ", "))
	}
	if !hashOK {
		return errors.New("validate Claude Code offline request: generated settings hash header was not sent")
	}
	if !strings.Contains(string(stdout), marker) {
		return fmt.Errorf("validate Claude Code offline request: output did not contain the expected marker: %q", strings.TrimSpace(string(stdout)))
	}
	return nil
}

func buildSettings(listen string, resolved Models) (Settings, string) {
	if strings.TrimSpace(listen) == "" {
		listen = config.DefaultListen
	}
	defaultID := resolved.Capabilities[resolved.DefaultAlias].AnthropicModelID
	subagentID := resolved.Capabilities[resolved.SubagentAlias].AnthropicModelID
	settings := Settings{
		Model:          defaultID,
		ModelOverrides: make(map[string]string, len(resolved.Aliases)),
		ModelPicker:    ModelPicker{ReplaceBuiltInOptions: true},
		Env: map[string]string{
			"ANTHROPIC_BASE_URL":         "http://" + listen,
			"ANTHROPIC_API_KEY":          "local",
			"ANTHROPIC_DEFAULT_MODEL":    defaultID,
			"CLAUDE_CODE_SUBAGENT_MODEL": subagentID,
		},
	}
	for _, alias := range resolved.Aliases {
		model := resolved.Models[alias]
		canonical := resolved.Capabilities[alias].AnthropicModelID
		label := strings.TrimSpace(model.DisplayName)
		if label == "" {
			label = alias
		}
		settings.ModelOverrides[canonical] = alias
		settings.ModelPicker.Options = append(settings.ModelPicker.Options, ModelOption{
			Model: canonical, Label: label, Description: "Bedrock Local Proxy alias: " + alias,
		})
	}
	hash := configurationHash(listen, resolved)
	settings.Env["ANTHROPIC_CUSTOM_HEADERS"] = SettingsHeader + ": " + hash
	return settings, hash
}

func configurationHash(listen string, resolved Models) string {
	type modelFingerprint struct {
		Alias          string                      `json:"alias"`
		DisplayName    string                      `json:"display_name"`
		BedrockModelID string                      `json:"bedrock_model_id"`
		Capabilities   config.ResolvedCapabilities `json:"capabilities"`
	}
	payload := struct {
		Listen        string             `json:"listen"`
		DefaultAlias  string             `json:"default_alias"`
		SubagentAlias string             `json:"subagent_alias"`
		Models        []modelFingerprint `json:"models"`
	}{Listen: listen, DefaultAlias: resolved.DefaultAlias, SubagentAlias: resolved.SubagentAlias}
	for _, alias := range resolved.Aliases {
		payload.Models = append(payload.Models, modelFingerprint{
			Alias: alias, DisplayName: resolved.Models[alias].DisplayName,
			BedrockModelID: resolved.Models[alias].BedrockModelID, Capabilities: resolved.Capabilities[alias],
		})
	}
	encoded, _ := json.Marshal(payload)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func settingsHash(settings Settings) (string, error) {
	copySettings := settings
	copySettings.Env = make(map[string]string, len(settings.Env))
	for key, value := range settings.Env {
		if key != "ANTHROPIC_CUSTOM_HEADERS" {
			copySettings.Env[key] = value
		}
	}
	encoded, err := json.Marshal(copySettings)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func normalizedHash(value string) string {
	value = strings.TrimSpace(value)
	if len(value) != sha256.Size*2 {
		return ""
	}
	if _, err := hex.DecodeString(value); err != nil {
		return ""
	}
	return strings.ToLower(value)
}

func resolveSettingsPath(configPath, override string) (string, error) {
	if value := strings.TrimSpace(override); value != "" {
		path, err := filepath.Abs(value)
		if err != nil {
			return "", fmt.Errorf("resolve Claude Code settings path: %w", err)
		}
		return path, nil
	}
	return DefaultSettingsPath(configPath)
}

func writeSettingsAtomic(path string, settings Settings, validate func(string) error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".settings-*.json")
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
	if err := encoder.Encode(settings); err != nil {
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

func normalizeVersion(output string) string {
	for _, field := range strings.Fields(output) {
		candidate := strings.Trim(strings.TrimSpace(field), "vV(),")
		parts := strings.Split(candidate, ".")
		if len(parts) != 3 {
			continue
		}
		valid := true
		for _, part := range parts {
			if _, err := strconv.Atoi(part); err != nil {
				valid = false
				break
			}
		}
		if valid {
			return candidate
		}
	}
	return ""
}

func versionAtLeast(version, minimum string) bool {
	left, right := strings.Split(version, "."), strings.Split(minimum, ".")
	for index := 0; index < 3; index++ {
		l, _ := strconv.Atoi(left[index])
		r, _ := strconv.Atoi(right[index])
		if l != r {
			return l > r
		}
	}
	return true
}

func skippedReason(models []SkippedModel, alias string) string {
	for _, model := range models {
		if model.Alias == alias {
			return model.Reason
		}
	}
	return "not Claude-compatible"
}

func formatSkipped(models []SkippedModel) string {
	values := make([]string, 0, len(models))
	for _, model := range models {
		values = append(values, fmt.Sprintf("%s (%s)", model.Alias, model.Reason))
	}
	if len(values) == 0 {
		return "no configured models"
	}
	return strings.Join(values, "; ")
}

func commandError(action string, err error, stderr []byte) error {
	message := strings.TrimSpace(string(stderr))
	if message == "" {
		message = err.Error()
	}
	return fmt.Errorf("%s: %s", action, message)
}
