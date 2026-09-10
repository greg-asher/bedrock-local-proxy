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
		w.WriteHeader(http.StatusMethodNotAllowed)
		s.finishCompletion(started, CompletionResult{Endpoint: "/v1/messages", Outcome: CompletionFailed})
		return
	}

	localModel, payload, err := s.transformMessagesRequest(r)
	if err != nil {
		s.writeAnthropicError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		s.finishCompletion(started, CompletionResult{
			Endpoint:   "/v1/messages",
			LocalModel: localModel,
			Outcome:    CompletionFailed,
		})
		return
	}
	if s.transport == nil {
		s.writeAnthropicError(w, http.StatusBadGateway, "AWS transport is not configured", "api_error")
		s.finishCompletion(started, CompletionResult{
			Endpoint:   "/v1/messages",
			LocalModel: localModel,
			Outcome:    CompletionFailed,
		})
		return
	}

	upstreamRequest, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "/anthropic/v1/messages"+querySuffix(r), bytes.NewReader(payload))
	if err != nil {
		s.writeAnthropicError(w, http.StatusBadGateway, "could not create upstream request", "api_error")
		s.finishCompletion(started, CompletionResult{Endpoint: "/v1/messages", LocalModel: localModel, Outcome: CompletionFailed})
		return
	}
	upstreamRequest.Header = r.Header.Clone()
	upstreamRequest.Header.Set("Content-Type", "application/json")

	response, err := s.transport.Do(r.Context(), upstreamRequest)
	if err != nil {
		s.writeAnthropicTransportError(w, err)
		outcome := CompletionFailed
		if errors.Is(err, context.Canceled) || transport.ClassOf(err) == transport.FailureCanceled {
			outcome = CompletionCanceled
		}
		s.finishCompletion(started, CompletionResult{
			Endpoint:      "/v1/messages",
			LocalModel:    localModel,
			UpstreamModel: configuredTarget(s.cfg, localModel),
			Outcome:       outcome,
		})
		return
	}
	if response == nil {
		s.writeAnthropicError(w, http.StatusBadGateway, "AWS upstream returned no response", "api_error")
		s.finishCompletion(started, CompletionResult{
			Endpoint:      "/v1/messages",
			LocalModel:    localModel,
			UpstreamModel: configuredTarget(s.cfg, localModel),
			Outcome:       CompletionFailed,
		})
		return
	}
	body := response.Body
	if body == nil {
		body = io.NopCloser(strings.NewReader(""))
	}
	defer body.Close()

	copyHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
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

func (s *Server) transformMessagesRequest(r *http.Request) (string, []byte, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return "", nil, errors.New("could not read request body")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	var fields map[string]json.RawMessage
	if err := dec.Decode(&fields); err != nil || fields == nil {
		return "", nil, errors.New("request body must be a JSON object")
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return "", nil, errors.New("request body must contain one JSON object")
	}

	modelRaw, ok := fields["model"]
	if !ok {
		return "", nil, errors.New("model must be a nonempty string")
	}
	var localModel string
	if err := json.Unmarshal(modelRaw, &localModel); err != nil || strings.TrimSpace(localModel) == "" {
		return "", nil, errors.New("model must be a nonempty string")
	}
	model, ok := s.cfg.Models[localModel]
	if !ok {
		return localModel, nil, fmt.Errorf("unknown model %q; configured models: %s", localModel, configuredModels(s.cfg))
	}

	var stream bool
	if raw, ok := fields["stream"]; ok {
		if err := json.Unmarshal(raw, &stream); err != nil {
			return localModel, nil, errors.New("stream must be a boolean")
		}
		if stream {
			return localModel, nil, errors.New("stream=true is not supported yet")
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
		return localModel, nil, errors.New("could not encode request body")
	}
	return localModel, transformed, nil
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
		var value int64
		if err := json.Unmarshal(raw, &value); err != nil {
			inputTokens = nil
		} else {
			inputTokens = &value
		}
	}
	if raw, ok := response.Usage["output_tokens"]; ok {
		var value int64
		if err := json.Unmarshal(raw, &value); err != nil {
			outputTokens = nil
		} else {
			outputTokens = &value
		}
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
