package server

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gregasher/bedrock-local-proxy/internal/config"
	"github.com/gregasher/bedrock-local-proxy/internal/transport"
)

// serveResponses relays the native OpenAI Responses protocol. Responses uses
// a different request and stream contract from Chat Completions, so this
// handler only rewrites the configured model and leaves input items, tool
// calls/results, and unknown fields as raw JSON values.
func (s *Server) serveResponses(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		status := http.StatusMethodNotAllowed
		w.WriteHeader(status)
		s.finishCompletion(started, CompletionResult{Endpoint: "/v1/responses", HTTPStatus: &status, Outcome: CompletionFailed})
		return
	}

	localModel, payload, stream, err := s.transformResponsesRequest(r)
	if err != nil {
		status := http.StatusBadRequest
		result := s.responsesCompletionMetadata(r, localModel)
		result.Endpoint, result.HTTPStatus, result.Outcome = "/v1/responses", &status, CompletionFailed
		var invalid *responsesValidationError
		if errors.As(err, &invalid) {
			s.writeOpenAIErrorDetails(w, status, invalid.Message, "invalid_request_error", invalid.Param, invalid.Code)
			if invalid.Code == "unsupported_hosted_tool" || invalid.Code == "unsupported_model_api" {
				result.UnsupportedFeatureRejections = 1
			}
		} else {
			s.writeOpenAIError(w, status, err.Error(), "invalid_request_error")
		}
		s.finishCompletion(started, result)
		return
	}
	if s.transport == nil {
		status := http.StatusBadGateway
		s.writeOpenAIError(w, status, "AWS transport is not configured", "upstream_error")
		s.finishCompletion(started, s.responsesFailureResult(r, localModel, status, CompletionFailed))
		return
	}

	upstreamRequest, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "/openai/v1/responses"+querySuffix(r), bytes.NewReader(payload))
	if err != nil {
		status := http.StatusBadGateway
		s.writeOpenAIError(w, status, "could not create upstream request", "upstream_error")
		s.finishCompletion(started, s.responsesFailureResult(r, localModel, status, CompletionFailed))
		return
	}
	upstreamRequest.Header = r.Header.Clone()
	upstreamRequest.Header.Del("X-Bedrock-Proxy-Catalog")
	upstreamRequest.Header.Set("Content-Type", "application/json")

	response, err := s.transport.Do(r.Context(), upstreamRequest)
	if err != nil {
		s.writeTransportError(w, err)
		status := transportErrorStatus(err)
		outcome := CompletionFailed
		if errors.Is(err, context.Canceled) || transport.ClassOf(err) == transport.FailureCanceled {
			outcome = CompletionCanceled
		}
		s.finishCompletion(started, s.responsesFailureResult(r, localModel, status, outcome))
		return
	}
	if response == nil {
		status := http.StatusBadGateway
		s.writeOpenAIError(w, status, "AWS upstream returned no response", "upstream_error")
		s.finishCompletion(started, s.responsesFailureResult(r, localModel, status, CompletionFailed))
		return
	}
	body := response.Body
	if body == nil {
		body = io.NopCloser(strings.NewReader(""))
	}
	defer body.Close()
	response.Body = body

	copyHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	if stream {
		s.serveResponsesStream(w, r, started, localModel, response)
		return
	}

	observed, copyErr := relayAndObserve(w, body, maxUsageObservationBytes)
	status := response.StatusCode
	result := CompletionResult{
		Endpoint:      "/v1/responses",
		LocalModel:    localModel,
		UpstreamModel: configuredTarget(s.cfg, localModel),
		HTTPStatus:    &status,
		Outcome:       CompletionFailed,
	}
	mergeCompletionMetadata(&result, s.responsesCompletionMetadata(r, localModel))
	if copyErr == nil && response.StatusCode >= 200 && response.StatusCode < 300 {
		result.Outcome = CompletionSucceeded
	}
	if copyErr != nil && (errors.Is(copyErr, context.Canceled) || errors.Is(r.Context().Err(), context.Canceled)) {
		result.Outcome = CompletionCanceled
	}
	if observed != nil {
		applyCompletionUsage(&result, parseResponsesUsage(observed))
		result.FunctionToolCalls = countResponseFunctionCalls(observed)
	}
	s.finishCompletion(started, result)
}

func (s *Server) transformResponsesRequest(r *http.Request) (string, []byte, bool, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return "", nil, false, errors.New("could not read request body")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	var fields map[string]json.RawMessage
	if err := dec.Decode(&fields); err != nil || fields == nil {
		return "", nil, false, errors.New("request body must be a JSON object")
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return "", nil, false, errors.New("request body must contain one JSON object")
	}

	modelRaw, ok := fields["model"]
	if !ok {
		return "", nil, false, errors.New("model must be a nonempty string")
	}
	var localModel string
	if err := json.Unmarshal(modelRaw, &localModel); err != nil || strings.TrimSpace(localModel) == "" {
		return "", nil, false, errors.New("model must be a nonempty string")
	}
	model, ok := s.cfg.Models[localModel]
	if !ok {
		return localModel, nil, false, fmt.Errorf("unknown model %q; configured models: %s", localModel, configuredModels(s.cfg))
	}
	if err := validateResponsesCapabilities(fields, model); err != nil {
		return localModel, nil, false, err
	}

	var stream bool
	if raw, ok := fields["stream"]; ok {
		if err := json.Unmarshal(raw, &stream); err != nil {
			return localModel, nil, false, errors.New("stream must be a boolean")
		}
	}
	fields["model"] = json.RawMessage(strconv.Quote(model.BedrockModelID))
	if model.Temperature != nil {
		if _, exists := fields["temperature"]; !exists {
			fields["temperature"] = json.RawMessage(strconv.FormatFloat(*model.Temperature, 'g', -1, 64))
		}
	}
	// Responses calls this field max_output_tokens. Do not translate or
	// overwrite the endpoint's explicit value with the Chat Completions name.
	if model.MaxTokens != nil {
		if _, exists := fields["max_output_tokens"]; !exists {
			fields["max_output_tokens"] = json.RawMessage(strconv.Itoa(*model.MaxTokens))
		}
	}
	transformed, err := json.Marshal(fields)
	if err != nil {
		return localModel, nil, stream, errors.New("could not encode request body")
	}
	return localModel, transformed, stream, nil
}

type responsesValidationError struct {
	Message string
	Param   string
	Code    string
}

func (e *responsesValidationError) Error() string { return e.Message }

var hostedResponseTools = map[string]struct{}{
	"web_search": {}, "web_search_preview": {}, "file_search": {},
	"code_interpreter": {}, "computer": {}, "computer_use": {},
	"image_generation": {}, "mcp": {}, "tool_search": {},
}

type responseToolDefinition struct {
	Type  string                   `json:"type"`
	Tools []responseToolDefinition `json:"tools"`
}

func validateResponsesCapabilities(fields map[string]json.RawMessage, model config.ModelConfig) error {
	if err := config.ValidateResponsesTarget(model); err != nil {
		return &responsesValidationError{Message: err.Error(), Param: "model", Code: "unsupported_model_api"}
	}
	var resolved *config.ResolvedCapabilities
	if model.Capabilities != nil {
		value, _, err := config.ResolveModelCapabilities(model)
		if err != nil {
			return &responsesValidationError{Message: "configured model capabilities are invalid: " + err.Error(), Param: "model", Code: "invalid_model_capabilities"}
		}
		if value.ResponsesKnown && !value.ResponsesSupported {
			return &responsesValidationError{Message: "the configured model does not support the Responses API on bedrock-runtime", Param: "model", Code: "unsupported_model_api"}
		}
		resolved = &value
	}
	if raw, ok := fields["tools"]; ok {
		var tools []responseToolDefinition
		if err := json.Unmarshal(raw, &tools); err != nil {
			return &responsesValidationError{Message: "tools must be an array of tool definitions", Param: "tools", Code: "invalid_tools"}
		}
		if err := validateResponseTools(tools, resolved); err != nil {
			return err
		}
	}
	if resolved == nil {
		return nil
	}
	outputLimit, hasOutputLimit := fields["max_output_tokens"]
	if !hasOutputLimit && model.MaxTokens != nil {
		outputLimit = json.RawMessage(strconv.Itoa(*model.MaxTokens))
		hasOutputLimit = true
	}
	if hasOutputLimit {
		var limit int64
		if err := json.Unmarshal(outputLimit, &limit); err != nil || limit <= 0 {
			return &responsesValidationError{Message: "max_output_tokens must be a positive integer", Param: "max_output_tokens", Code: "invalid_max_output_tokens"}
		}
		if limit > resolved.MaxOutputTokens {
			return &responsesValidationError{Message: fmt.Sprintf("max_output_tokens %d exceeds the configured model ceiling %d", limit, resolved.MaxOutputTokens), Param: "max_output_tokens", Code: "max_output_tokens_exceeded"}
		}
	}
	if raw, ok := fields["reasoning"]; ok && len(bytes.TrimSpace(raw)) > 0 && string(bytes.TrimSpace(raw)) != "null" && !resolved.ReasoningSupported {
		return &responsesValidationError{Message: "the configured model does not support adjustable reasoning effort", Param: "reasoning", Code: "unsupported_reasoning"}
	}
	if raw, ok := fields["reasoning"]; ok && resolved.ReasoningSupported && string(bytes.TrimSpace(raw)) != "null" {
		var reasoning struct {
			Effort string `json:"effort"`
		}
		if err := json.Unmarshal(raw, &reasoning); err != nil {
			return &responsesValidationError{Message: "reasoning must be an object", Param: "reasoning", Code: "invalid_reasoning"}
		}
		if reasoning.Effort != "" && !containsString(resolved.ReasoningEfforts, reasoning.Effort) {
			return &responsesValidationError{Message: fmt.Sprintf("reasoning effort %q is not supported by the configured model", reasoning.Effort), Param: "reasoning.effort", Code: "unsupported_reasoning_effort"}
		}
	}
	if !containsString(resolved.InputModalities, "image") {
		if raw, ok := fields["input"]; ok && containsJSONType(raw, "input_image") {
			return &responsesValidationError{Message: "the configured model does not support image input", Param: "input", Code: "unsupported_input_modality"}
		}
	}
	if raw, ok := fields["parallel_tool_calls"]; ok {
		var parallel bool
		if err := json.Unmarshal(raw, &parallel); err != nil {
			return &responsesValidationError{Message: "parallel_tool_calls must be a boolean", Param: "parallel_tool_calls", Code: "invalid_parallel_tool_calls"}
		}
		if parallel && !resolved.ParallelCalls {
			return &responsesValidationError{Message: "the configured model does not support parallel function calls", Param: "parallel_tool_calls", Code: "unsupported_parallel_tool_calls"}
		}
	}
	return nil
}

func validateResponseTools(tools []responseToolDefinition, resolved *config.ResolvedCapabilities) error {
	for _, tool := range tools {
		kind := strings.TrimSpace(tool.Type)
		if _, hosted := hostedResponseTools[kind]; hosted {
			return &responsesValidationError{
				Message: fmt.Sprintf("hosted tool %q is unavailable through Bedrock Runtime; configure the capability as an MCP in Codex so Codex executes it client-side", kind),
				Param:   "tools",
				Code:    "unsupported_hosted_tool",
			}
		}
		if kind == "function" && resolved != nil && !resolved.FunctionCalling {
			return &responsesValidationError{Message: "the configured model does not support client-side function tools", Param: "tools", Code: "unsupported_function_tool"}
		}
		if err := validateResponseTools(tool.Tools, resolved); err != nil {
			return err
		}
	}
	return nil
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func containsJSONType(raw json.RawMessage, wanted string) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	var visit func(any) bool
	visit = func(current any) bool {
		switch typed := current.(type) {
		case map[string]any:
			if kind, _ := typed["type"].(string); kind == wanted {
				return true
			}
			for _, child := range typed {
				if visit(child) {
					return true
				}
			}
		case []any:
			for _, child := range typed {
				if visit(child) {
					return true
				}
			}
		}
		return false
	}
	return visit(value)
}

func (s *Server) responsesCompletionMetadata(r *http.Request, alias string) CompletionResult {
	result := CompletionResult{CatalogHash: normalizedCatalogHash(r.Header.Get("X-Bedrock-Proxy-Catalog"))}
	result.ClientFamily, result.ClientVersion = normalizedClient(r.UserAgent())
	if model, ok := s.cfg.Models[alias]; ok && model.Capabilities != nil {
		if capabilities, _, err := config.ResolveModelCapabilities(model); err == nil {
			result.ContextWindow = capabilities.ContextWindow
			result.MaxOutputTokens = capabilities.MaxOutputTokens
			result.MetadataProfile = capabilities.MetadataProfile
			result.MetadataRevision = capabilities.MetadataRevision
		}
	}
	return result
}

func (s *Server) responsesFailureResult(r *http.Request, alias string, status int, outcome CompletionOutcome) CompletionResult {
	result := s.responsesCompletionMetadata(r, alias)
	result.Endpoint = "/v1/responses"
	result.LocalModel = alias
	result.UpstreamModel = configuredTarget(s.cfg, alias)
	result.HTTPStatus = &status
	result.Outcome = outcome
	return result
}

func mergeCompletionMetadata(target *CompletionResult, source CompletionResult) {
	target.ClientFamily, target.ClientVersion = source.ClientFamily, source.ClientVersion
	target.ContextWindow, target.MaxOutputTokens = source.ContextWindow, source.MaxOutputTokens
	target.MetadataProfile, target.MetadataRevision = source.MetadataProfile, source.MetadataRevision
	target.CatalogHash = source.CatalogHash
	target.SettingsHash = source.SettingsHash
}

func normalizedClient(userAgent string) (string, string) {
	fields := strings.Fields(strings.TrimSpace(userAgent))
	if len(fields) == 0 {
		return "", ""
	}
	name, version, found := strings.Cut(fields[0], "/")
	switch strings.ToLower(name) {
	case "codex_cli_rs", "codex-cli", "codex":
		if found {
			return "codex", normalizedVersion(version)
		}
		return "codex", ""
	case "pi":
		return "pi", normalizedVersion(version)
	case "claude-code", "claude_cli", "claude-cli":
		return "claude-code", normalizedVersion(version)
	default:
		return "", ""
	}
}

func normalizedVersion(value string) string {
	if value == "" || len(value) > 32 || value[0] < '0' || value[0] > '9' {
		return ""
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && r != '.' && r != '-' && r != '+' {
			return ""
		}
	}
	return value
}

func normalizedCatalogHash(value string) string {
	value = strings.TrimSpace(value)
	if len(value) != 64 {
		return ""
	}
	if _, err := hex.DecodeString(value); err != nil {
		return ""
	}
	return strings.ToLower(value)
}

func countResponseFunctionCalls(body []byte) int {
	var response struct {
		Output []struct {
			Type string `json:"type"`
		} `json:"output"`
	}
	if json.Unmarshal(body, &response) != nil {
		return 0
	}
	count := 0
	for _, item := range response.Output {
		if item.Type == "function_call" {
			count++
		}
	}
	return count
}

func parseResponsesUsage(body []byte) completionUsage {
	var response struct {
		Usage map[string]json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(body, &response); err != nil || response.Usage == nil {
		return completionUsage{inputTokensIncludeCache: true}
	}
	return parseResponsesUsageFields(response.Usage)
}

func parseResponsesUsageFields(usage map[string]json.RawMessage) completionUsage {
	var inputTokens, outputTokens *int64
	if raw, ok := usage["input_tokens"]; ok {
		inputTokens, _ = decodeOptionalInt64(raw)
	}
	if raw, ok := usage["output_tokens"]; ok {
		outputTokens, _ = decodeOptionalInt64(raw)
	}
	cacheRead, cacheWrite, uncovered := parseCacheDetails(usage["input_tokens_details"], "cached_tokens", "cache_write_tokens", nil)
	if raw, ok := usage["output_tokens_details"]; ok {
		uncovered = uncovered || detailsContainUnknownBilling(raw, map[string]bool{"reasoning_tokens": true})
	}
	for key := range usage {
		if key != "input_tokens" && key != "output_tokens" && key != "total_tokens" && key != "input_tokens_details" && key != "output_tokens_details" {
			uncovered = true
			break
		}
	}
	return completionUsage{inputTokens: inputTokens, outputTokens: outputTokens, cacheReadInputTokens: cacheRead, cacheWriteInputTokens: cacheWrite, inputTokensIncludeCache: true, uncovered: uncovered}
}

type responsesStreamObserver struct {
	inputTokens           *int64
	outputTokens          *int64
	cacheReadInputTokens  *int64
	cacheWriteInputTokens *int64
	functionCalls         int
	uncovered             bool
	sawTerminal           bool
	sawSuccess            bool
	sawFailure            bool
}

func (o *responsesStreamObserver) observe(event sseEvent) {
	if event.Oversized {
		return
	}
	data := bytes.TrimSpace(event.Data)
	if len(data) == 0 {
		return
	}
	var envelope struct {
		Type     string          `json:"type"`
		Usage    json.RawMessage `json:"usage"`
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return
	}
	if len(envelope.Usage) > 0 && string(envelope.Usage) != "null" {
		o.observeUsage(envelope.Usage)
	}
	if len(envelope.Response) > 0 && string(envelope.Response) != "null" {
		var response struct {
			Status string                     `json:"status"`
			Usage  map[string]json.RawMessage `json:"usage"`
			Output []struct {
				Type string `json:"type"`
			} `json:"output"`
		}
		if json.Unmarshal(envelope.Response, &response) == nil {
			if response.Usage != nil {
				o.setUsage(response.Usage)
			}
			if response.Status == "failed" || response.Status == "incomplete" {
				o.sawTerminal = true
				o.sawFailure = true
			}
			if len(response.Output) > 0 {
				o.functionCalls = 0
				for _, item := range response.Output {
					if item.Type == "function_call" {
						o.functionCalls++
					}
				}
			}
		}
	}
	switch envelope.Type {
	case "response.completed":
		o.sawTerminal = true
		if !o.sawFailure {
			o.sawSuccess = true
		}
	case "response.failed", "response.incomplete", "error":
		o.sawTerminal = true
		o.sawFailure = true
		o.sawSuccess = false
	}
}

func (o *responsesStreamObserver) observeUsage(raw []byte) {
	var usage map[string]json.RawMessage
	if json.Unmarshal(raw, &usage) == nil && usage != nil {
		o.setUsage(usage)
	}
}

func (o *responsesStreamObserver) setUsage(usage map[string]json.RawMessage) {
	parsed := parseResponsesUsageFields(usage)
	if parsed.inputTokens != nil {
		o.inputTokens = parsed.inputTokens
	}
	if parsed.outputTokens != nil {
		o.outputTokens = parsed.outputTokens
	}
	if parsed.cacheReadInputTokens != nil {
		o.cacheReadInputTokens = parsed.cacheReadInputTokens
	}
	if parsed.cacheWriteInputTokens != nil {
		o.cacheWriteInputTokens = parsed.cacheWriteInputTokens
	}
	o.uncovered = o.uncovered || parsed.uncovered
}

func (s *Server) serveResponsesStream(w http.ResponseWriter, r *http.Request, started time.Time, localModel string, response *http.Response) {
	body := response.Body
	if body == nil {
		body = io.NopCloser(strings.NewReader(""))
	}
	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}
	observer := &responsesStreamObserver{}
	copyErr := relayAndObserveStream(w, flush, body, maxUsageObservationBytes, observer.observe)
	status := response.StatusCode
	result := CompletionResult{
		Endpoint:                       "/v1/responses",
		LocalModel:                     localModel,
		UpstreamModel:                  configuredTarget(s.cfg, localModel),
		HTTPStatus:                     &status,
		Outcome:                        CompletionFailed,
		InputTokens:                    observer.inputTokens,
		OutputTokens:                   observer.outputTokens,
		CacheReadInputTokens:           observer.cacheReadInputTokens,
		CacheWriteInputTokens:          observer.cacheWriteInputTokens,
		InputTokensIncludeCache:        true,
		ObservedUncoveredBillingFields: observer.uncovered,
		FunctionToolCalls:              observer.functionCalls,
	}
	mergeCompletionMetadata(&result, s.responsesCompletionMetadata(r, localModel))
	normalEnd := copyErr == nil || errors.Is(copyErr, io.EOF)
	if response.StatusCode >= 200 && response.StatusCode < 300 && normalEnd && observer.sawTerminal && observer.sawSuccess && !observer.sawFailure {
		result.Outcome = CompletionSucceeded
	}
	if errors.Is(r.Context().Err(), context.Canceled) || errors.Is(copyErr, context.Canceled) {
		result.Outcome = CompletionCanceled
	}
	s.finishCompletion(started, result)
}
