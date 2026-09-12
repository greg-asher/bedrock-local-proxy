package server

import (
	"bytes"
	"context"
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

type messagesValidationError struct {
	message     string
	unsupported bool
}

func (e *messagesValidationError) Error() string { return e.message }

// serveMessages relays the native Bedrock Anthropic Messages protocol. The
// body is intentionally treated as an envelope of raw JSON values: system
// blocks, tool results, thinking fields, cache controls, unknown fields, and
// large numeric values must reach AWS without a lossy intermediate schema.
func (s *Server) serveMessages(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		status := http.StatusMethodNotAllowed
		w.WriteHeader(status)
		result := s.messagesCompletionMetadata(r, "")
		result.Endpoint, result.HTTPStatus, result.Outcome = "/v1/messages", &status, CompletionFailed
		s.finishCompletion(started, result)
		return
	}

	localModel, payload, stream, err := s.transformMessagesRequest(r)
	if err != nil {
		status := http.StatusBadRequest
		s.writeAnthropicError(w, status, err.Error(), "invalid_request_error")
		result := s.messagesFailureResult(r, localModel, status, CompletionFailed)
		var validation *messagesValidationError
		if errors.As(err, &validation) && validation.unsupported {
			result.UnsupportedFeatureRejections = 1
		}
		s.finishCompletion(started, result)
		return
	}
	if s.transport == nil {
		status := http.StatusBadGateway
		s.writeAnthropicError(w, status, "AWS transport is not configured", "api_error")
		s.finishCompletion(started, s.messagesFailureResult(r, localModel, status, CompletionFailed))
		return
	}

	upstreamRequest, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "/anthropic/v1/messages"+querySuffix(r), bytes.NewReader(payload))
	if err != nil {
		status := http.StatusBadGateway
		s.writeAnthropicError(w, status, "could not create upstream request", "api_error")
		s.finishCompletion(started, s.messagesFailureResult(r, localModel, status, CompletionFailed))
		return
	}
	upstreamRequest.Header = r.Header.Clone()
	upstreamRequest.Header.Set("Content-Type", "application/json")
	upstreamRequest.Header.Del("X-Bedrock-Proxy-Claude-Settings")

	response, err := s.transport.Do(r.Context(), upstreamRequest)
	if err != nil {
		s.writeAnthropicTransportError(w, err)
		status := transportErrorStatus(err)
		outcome := CompletionFailed
		if errors.Is(err, context.Canceled) || transport.ClassOf(err) == transport.FailureCanceled {
			outcome = CompletionCanceled
		}
		s.finishCompletion(started, s.messagesFailureResult(r, localModel, status, outcome))
		return
	}
	if response == nil {
		status := http.StatusBadGateway
		s.writeAnthropicError(w, status, "AWS upstream returned no response", "api_error")
		s.finishCompletion(started, s.messagesFailureResult(r, localModel, status, CompletionFailed))
		return
	}
	body := response.Body
	if body == nil {
		body = io.NopCloser(strings.NewReader(""))
	}
	response.Body = body
	defer body.Close()

	copyHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	if stream {
		s.serveMessagesStream(w, r, started, localModel, response)
		return
	}
	observed, copyErr := relayAndObserve(w, body, maxUsageObservationBytes)
	status := response.StatusCode
	result := s.messagesFailureResult(r, localModel, status, CompletionFailed)
	if copyErr == nil && response.StatusCode >= 200 && response.StatusCode < 300 {
		result.Outcome = CompletionSucceeded
	}
	if copyErr != nil && (errors.Is(copyErr, context.Canceled) || errors.Is(r.Context().Err(), context.Canceled)) {
		result.Outcome = CompletionCanceled
	}
	if observed != nil {
		applyCompletionUsage(&result, parseMessagesUsage(observed))
		result.FunctionToolCalls = countMessageToolCalls(observed)
	}
	s.finishCompletion(started, result)
}

func (s *Server) transformMessagesRequest(r *http.Request) (string, []byte, bool, error) {
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
	if err := config.ValidateMessagesTarget(model); err != nil {
		return localModel, nil, false, &messagesValidationError{message: err.Error(), unsupported: true}
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
	if model.MaxTokens != nil {
		if _, exists := fields["max_tokens"]; !exists {
			fields["max_tokens"] = json.RawMessage(strconv.Itoa(*model.MaxTokens))
		}
	}
	if raw, exists := fields["max_tokens"]; exists {
		var limit int64
		if err := json.Unmarshal(raw, &limit); err != nil || limit <= 0 {
			return localModel, nil, stream, &messagesValidationError{message: "max_tokens must be a positive integer"}
		}
		if model.Capabilities != nil {
			if capabilities, _, err := config.ResolveModelCapabilities(model); err == nil && capabilities.MaxOutputTokens > 0 && limit > capabilities.MaxOutputTokens {
				return localModel, nil, stream, &messagesValidationError{message: fmt.Sprintf("max_tokens %d exceeds the configured model ceiling %d", limit, capabilities.MaxOutputTokens), unsupported: true}
			}
		}
	}
	transformed, err := json.Marshal(fields)
	if err != nil {
		return localModel, nil, stream, errors.New("could not encode request body")
	}
	return localModel, transformed, stream, nil
}

type anthropicErrorEnvelope struct {
	Type  string         `json:"type"`
	Error anthropicError `json:"error"`
}

type anthropicError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func (s *Server) writeAnthropicError(w http.ResponseWriter, status int, message, kind string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(anthropicErrorEnvelope{
		Type: "error",
		Error: anthropicError{
			Type:    kind,
			Message: message,
		},
	})
}

func (s *Server) writeAnthropicTransportError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	message := "AWS upstream request failed"
	kind := "api_error"
	switch transport.ClassOf(err) {
	case transport.FailureCredentialsExpired:
		status = http.StatusUnauthorized
		kind = "authentication_error"
		message = fmt.Sprintf("AWS authentication expired for profile %s. Run: aws sso login --profile %s", s.cfg.AWS.Profile, s.cfg.AWS.Profile)
	case transport.FailureCredentialsUnavailable:
		status = http.StatusUnauthorized
		kind = "authentication_error"
		message = fmt.Sprintf("AWS credentials are unavailable for profile %s", s.cfg.AWS.Profile)
	}
	s.writeAnthropicError(w, status, message, kind)
}

func parseMessagesUsage(body []byte) completionUsage {
	var response struct {
		Usage map[string]json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(body, &response); err != nil || response.Usage == nil {
		return completionUsage{}
	}
	return parseMessagesUsageFields(response.Usage)
}

func parseMessagesUsageFields(usage map[string]json.RawMessage) completionUsage {
	var inputTokens, outputTokens *int64
	if raw, ok := usage["input_tokens"]; ok {
		inputTokens, _ = decodeOptionalInt64(raw)
	}
	if raw, ok := usage["output_tokens"]; ok {
		outputTokens, _ = decodeOptionalInt64(raw)
	}
	var cacheRead, cacheWrite *int64
	uncovered := false
	if raw, ok := usage["cache_read_input_tokens"]; ok {
		var valid bool
		cacheRead, valid = decodeOptionalInt64(raw)
		uncovered = uncovered || !valid
	}
	if raw, ok := usage["cache_creation_input_tokens"]; ok {
		var valid bool
		cacheWrite, valid = decodeOptionalInt64(raw)
		uncovered = uncovered || !valid
	}
	if raw, ok := usage["cache_creation"]; ok {
		detailedWrite, invalid := parseCacheCreationDetails(raw)
		uncovered = uncovered || invalid
		if cacheWrite == nil {
			cacheWrite = detailedWrite
		} else if detailedWrite != nil && *cacheWrite != *detailedWrite {
			uncovered = true
		}
	}
	for key, raw := range usage {
		switch key {
		case "input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens", "cache_creation":
			continue
		case "server_tool_use":
			if billingValueNonzeroOrUnknown(raw) {
				uncovered = true
			}
		default:
			uncovered = true
		}
	}
	return completionUsage{inputTokens: inputTokens, outputTokens: outputTokens, cacheReadInputTokens: cacheRead, cacheWriteInputTokens: cacheWrite, uncovered: uncovered}
}

// messagesStreamObserver understands the native Anthropic Messages stream.
// The relay has already delivered every byte to the caller; this observer only
// extracts terminal state and usage metadata from bounded SSE records.
type messagesStreamObserver struct {
	inputTokens           *int64
	outputTokens          *int64
	cacheReadInputTokens  *int64
	cacheWriteInputTokens *int64
	uncovered             bool
	usageInvalid          bool
	sawStop               bool
	sawError              bool
	toolCalls             int
}

func (o *messagesStreamObserver) observe(event sseEvent) {
	if event.Oversized {
		o.usageInvalid = true
		return
	}
	data := bytes.TrimSpace(event.Data)
	if len(data) == 0 {
		return
	}
	var envelope struct {
		Type    string `json:"type"`
		Message *struct {
			Usage map[string]json.RawMessage `json:"usage"`
		} `json:"message"`
		Usage map[string]json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		o.usageInvalid = true
		return
	}
	switch envelope.Type {
	case "content_block_start":
		var block struct {
			ContentBlock struct {
				Type string `json:"type"`
			} `json:"content_block"`
		}
		if json.Unmarshal(data, &block) == nil && block.ContentBlock.Type == "tool_use" {
			o.toolCalls++
		}
	case "message_start":
		if envelope.Message != nil {
			o.observeUsage(envelope.Message.Usage)
		}
	case "message_delta":
		o.observeUsage(envelope.Usage)
	case "message_stop":
		o.sawStop = true
	case "error":
		o.sawError = true
	}
}

func (o *messagesStreamObserver) observeUsage(usage map[string]json.RawMessage) {
	if usage == nil {
		return
	}
	parsed := parseMessagesUsageFields(usage)
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

func parseCacheCreationDetails(raw json.RawMessage) (*int64, bool) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, false
	}
	var details map[string]json.RawMessage
	if err := json.Unmarshal(raw, &details); err != nil {
		return nil, true
	}
	var total int64
	found := false
	for key, raw := range details {
		if key != "ephemeral_5m_input_tokens" && key != "ephemeral_1h_input_tokens" {
			return nil, true
		}
		value, valid := decodeOptionalInt64(raw)
		if !valid {
			return nil, true
		}
		if value != nil {
			total += *value
			found = true
		}
	}
	if !found {
		return nil, false
	}
	return &total, false
}

func billingValueNonzeroOrUnknown(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return false
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return true
	}
	return billingValueNonzero(value)
}

func billingValueNonzero(value any) bool {
	switch value := value.(type) {
	case nil:
		return false
	case json.Number:
		parsed, err := value.Float64()
		return err != nil || parsed != 0
	case map[string]any:
		for _, item := range value {
			if billingValueNonzero(item) {
				return true
			}
		}
		return false
	case []any:
		for _, item := range value {
			if billingValueNonzero(item) {
				return true
			}
		}
		return false
	default:
		return true
	}
}

func (s *Server) serveMessagesStream(w http.ResponseWriter, r *http.Request, started time.Time, localModel string, response *http.Response) {
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
	observer := &messagesStreamObserver{}
	copyErr := relayAndObserveStream(w, flush, body, maxUsageObservationBytes, observer.observe)
	status := response.StatusCode
	result := s.messagesFailureResult(r, localModel, status, CompletionFailed)
	result.FunctionToolCalls = observer.toolCalls
	if !observer.usageInvalid {
		result.InputTokens = observer.inputTokens
		result.OutputTokens = observer.outputTokens
		result.CacheReadInputTokens = observer.cacheReadInputTokens
		result.CacheWriteInputTokens = observer.cacheWriteInputTokens
		result.ObservedUncoveredBillingFields = observer.uncovered
	}
	normalEnd := copyErr == nil || errors.Is(copyErr, io.EOF)
	if response.StatusCode >= 200 && response.StatusCode < 300 && normalEnd && observer.sawStop && !observer.sawError {
		result.Outcome = CompletionSucceeded
	}
	if errors.Is(r.Context().Err(), context.Canceled) || errors.Is(copyErr, context.Canceled) {
		result.Outcome = CompletionCanceled
	}
	s.finishCompletion(started, result)
}

func (s *Server) messagesCompletionMetadata(r *http.Request, alias string) CompletionResult {
	result := CompletionResult{SettingsHash: normalizedCatalogHash(r.Header.Get("X-Bedrock-Proxy-Claude-Settings"))}
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

func (s *Server) messagesFailureResult(r *http.Request, alias string, status int, outcome CompletionOutcome) CompletionResult {
	result := s.messagesCompletionMetadata(r, alias)
	result.Endpoint = "/v1/messages"
	result.LocalModel = alias
	result.UpstreamModel = configuredTarget(s.cfg, alias)
	result.HTTPStatus = &status
	result.Outcome = outcome
	return result
}

func countMessageToolCalls(body []byte) int {
	var response struct {
		Content []struct {
			Type string `json:"type"`
		} `json:"content"`
	}
	if json.Unmarshal(body, &response) != nil {
		return 0
	}
	count := 0
	for _, block := range response.Content {
		if block.Type == "tool_use" {
			count++
		}
	}
	return count
}
