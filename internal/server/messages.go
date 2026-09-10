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

	"github.com/gregasher/bedrock-local-proxy/internal/transport"
)

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
		s.finishCompletion(started, CompletionResult{Endpoint: "/v1/messages", HTTPStatus: &status, Outcome: CompletionFailed})
		return
	}

	localModel, payload, stream, err := s.transformMessagesRequest(r)
	if err != nil {
		status := http.StatusBadRequest
		s.writeAnthropicError(w, status, err.Error(), "invalid_request_error")
		s.finishCompletion(started, CompletionResult{
			Endpoint:   "/v1/messages",
			HTTPStatus: &status,
			LocalModel: localModel,
			Outcome:    CompletionFailed,
		})
		return
	}
	if s.transport == nil {
		status := http.StatusBadGateway
		s.writeAnthropicError(w, status, "AWS transport is not configured", "api_error")
		s.finishCompletion(started, CompletionResult{
			Endpoint:   "/v1/messages",
			HTTPStatus: &status,
			LocalModel: localModel,
			Outcome:    CompletionFailed,
		})
		return
	}

	upstreamRequest, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "/anthropic/v1/messages"+querySuffix(r), bytes.NewReader(payload))
	if err != nil {
		status := http.StatusBadGateway
		s.writeAnthropicError(w, status, "could not create upstream request", "api_error")
		s.finishCompletion(started, CompletionResult{Endpoint: "/v1/messages", LocalModel: localModel, HTTPStatus: &status, Outcome: CompletionFailed})
		return
	}
	upstreamRequest.Header = r.Header.Clone()
	upstreamRequest.Header.Set("Content-Type", "application/json")

	response, err := s.transport.Do(r.Context(), upstreamRequest)
	if err != nil {
		s.writeAnthropicTransportError(w, err)
		status := transportErrorStatus(err)
		outcome := CompletionFailed
		if errors.Is(err, context.Canceled) || transport.ClassOf(err) == transport.FailureCanceled {
			outcome = CompletionCanceled
		}
		s.finishCompletion(started, CompletionResult{
			Endpoint:      "/v1/messages",
			LocalModel:    localModel,
			UpstreamModel: configuredTarget(s.cfg, localModel),
			HTTPStatus:    &status,
			Outcome:       outcome,
		})
		return
	}
	if response == nil {
		status := http.StatusBadGateway
		s.writeAnthropicError(w, status, "AWS upstream returned no response", "api_error")
		s.finishCompletion(started, CompletionResult{
			Endpoint:      "/v1/messages",
			LocalModel:    localModel,
			UpstreamModel: configuredTarget(s.cfg, localModel),
			HTTPStatus:    &status,
			Outcome:       CompletionFailed,
		})
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
	result := CompletionResult{
		Endpoint:      "/v1/messages",
		LocalModel:    localModel,
		UpstreamModel: configuredTarget(s.cfg, localModel),
		HTTPStatus:    &status,
		Outcome:       CompletionFailed,
	}
	if copyErr == nil && response.StatusCode >= 200 && response.StatusCode < 300 {
		result.Outcome = CompletionSucceeded
	}
	if copyErr != nil && (errors.Is(copyErr, context.Canceled) || errors.Is(r.Context().Err(), context.Canceled)) {
		result.Outcome = CompletionCanceled
	}
	if observed != nil {
		result.InputTokens, result.OutputTokens, result.ObservedUncoveredBillingFields = parseMessagesUsage(observed)
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

func parseMessagesUsage(body []byte) (*int64, *int64, bool) {
	var response struct {
		Usage map[string]json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(body, &response); err != nil || response.Usage == nil {
		return nil, nil, false
	}
	var inputTokens, outputTokens *int64
	if raw, ok := response.Usage["input_tokens"]; ok {
		inputTokens, _ = decodeOptionalInt64(raw)
	}
	if raw, ok := response.Usage["output_tokens"]; ok {
		outputTokens, _ = decodeOptionalInt64(raw)
	}
	uncovered := false
	for key := range response.Usage {
		if key != "input_tokens" && key != "output_tokens" {
			uncovered = true
			break
		}
	}
	return inputTokens, outputTokens, uncovered
}

// messagesStreamObserver understands the native Anthropic Messages stream.
// The relay has already delivered every byte to the caller; this observer only
// extracts terminal state and usage metadata from bounded SSE records.
type messagesStreamObserver struct {
	inputTokens  *int64
	outputTokens *int64
	uncovered    bool
	usageInvalid bool
	sawStop      bool
	sawError     bool
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
	if raw, ok := usage["input_tokens"]; ok {
		value, valid := decodeOptionalInt64(raw)
		if !valid {
			o.inputTokens = nil
		} else {
			o.inputTokens = value
		}
	}
	if raw, ok := usage["output_tokens"]; ok {
		value, valid := decodeOptionalInt64(raw)
		if !valid {
			o.outputTokens = nil
		} else {
			o.outputTokens = value
		}
	}
	for key := range usage {
		if key != "input_tokens" && key != "output_tokens" {
			o.uncovered = true
		}
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
	result := CompletionResult{
		Endpoint:      "/v1/messages",
		LocalModel:    localModel,
		UpstreamModel: configuredTarget(s.cfg, localModel),
		HTTPStatus:    &status,
		Outcome:       CompletionFailed,
	}
	if !observer.usageInvalid {
		result.InputTokens = observer.inputTokens
		result.OutputTokens = observer.outputTokens
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
