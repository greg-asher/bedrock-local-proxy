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

// serveResponses relays the native OpenAI Responses protocol. Responses uses
// a different request and stream contract from Chat Completions, so this
// handler only rewrites the configured model and leaves input items, tool
// calls/results, and unknown fields as raw JSON values.
func (s *Server) serveResponses(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		s.finishCompletion(started, CompletionResult{Endpoint: "/v1/responses", Outcome: CompletionFailed})
		return
	}

	localModel, payload, stream, err := s.transformResponsesRequest(r)
	if err != nil {
		s.writeOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		s.finishCompletion(started, CompletionResult{Endpoint: "/v1/responses", LocalModel: localModel, Outcome: CompletionFailed})
		return
	}
	if s.transport == nil {
		s.writeOpenAIError(w, http.StatusBadGateway, "AWS transport is not configured", "upstream_error")
		s.finishCompletion(started, CompletionResult{Endpoint: "/v1/responses", LocalModel: localModel, Outcome: CompletionFailed})
		return
	}

	upstreamRequest, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "/openai/v1/responses"+querySuffix(r), bytes.NewReader(payload))
	if err != nil {
		s.writeOpenAIError(w, http.StatusBadGateway, "could not create upstream request", "upstream_error")
		s.finishCompletion(started, CompletionResult{Endpoint: "/v1/responses", LocalModel: localModel, Outcome: CompletionFailed})
		return
	}
	upstreamRequest.Header = r.Header.Clone()
	upstreamRequest.Header.Set("Content-Type", "application/json")

	response, err := s.transport.Do(r.Context(), upstreamRequest)
	if err != nil {
		s.writeTransportError(w, err)
		outcome := CompletionFailed
		if errors.Is(err, context.Canceled) || transport.ClassOf(err) == transport.FailureCanceled {
			outcome = CompletionCanceled
		}
		s.finishCompletion(started, CompletionResult{
			Endpoint:      "/v1/responses",
			LocalModel:    localModel,
			UpstreamModel: configuredTarget(s.cfg, localModel),
			Outcome:       outcome,
		})
		return
	}
	if response == nil {
		s.writeOpenAIError(w, http.StatusBadGateway, "AWS upstream returned no response", "upstream_error")
		s.finishCompletion(started, CompletionResult{Endpoint: "/v1/responses", LocalModel: localModel, UpstreamModel: configuredTarget(s.cfg, localModel), Outcome: CompletionFailed})
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
	if copyErr == nil && response.StatusCode >= 200 && response.StatusCode < 300 {
		result.Outcome = CompletionSucceeded
	}
	if copyErr != nil && (errors.Is(copyErr, context.Canceled) || errors.Is(r.Context().Err(), context.Canceled)) {
		result.Outcome = CompletionCanceled
	}
	if observed != nil {
		result.InputTokens, result.OutputTokens, result.ObservedUncoveredBillingFields = parseResponsesUsage(observed)
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

func parseResponsesUsage(body []byte) (*int64, *int64, bool) {
	var response struct {
		Usage map[string]json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(body, &response); err != nil || response.Usage == nil {
		return nil, nil, false
	}
	return parseResponsesUsageFields(response.Usage)
}

func parseResponsesUsageFields(usage map[string]json.RawMessage) (*int64, *int64, bool) {
	var inputTokens, outputTokens *int64
	if raw, ok := usage["input_tokens"]; ok {
		inputTokens, _ = decodeOptionalInt64(raw)
	}
	if raw, ok := usage["output_tokens"]; ok {
		outputTokens, _ = decodeOptionalInt64(raw)
	}
	uncovered := false
	for key := range usage {
		if key != "input_tokens" && key != "output_tokens" && key != "total_tokens" {
			uncovered = true
			break
		}
	}
	return inputTokens, outputTokens, uncovered
}

type responsesStreamObserver struct {
	inputTokens  *int64
	outputTokens *int64
	uncovered    bool
	sawTerminal  bool
	sawSuccess   bool
	sawFailure   bool
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
		}
		if json.Unmarshal(envelope.Response, &response) == nil {
			if response.Usage != nil {
				o.setUsage(response.Usage)
			}
			if response.Status == "failed" || response.Status == "incomplete" {
				o.sawTerminal = true
				o.sawFailure = true
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
	if raw, ok := usage["input_tokens"]; ok {
		value, valid := decodeOptionalInt64(raw)
		if valid {
			o.inputTokens = value
		} else {
			o.inputTokens = nil
		}
	}
	if raw, ok := usage["output_tokens"]; ok {
		value, valid := decodeOptionalInt64(raw)
		if valid {
			o.outputTokens = value
		} else {
			o.outputTokens = nil
		}
	}
	for key := range usage {
		if key != "input_tokens" && key != "output_tokens" && key != "total_tokens" {
			o.uncovered = true
		}
	}
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
		ObservedUncoveredBillingFields: observer.uncovered,
	}
	normalEnd := copyErr == nil || errors.Is(copyErr, io.EOF)
	if response.StatusCode >= 200 && response.StatusCode < 300 && normalEnd && observer.sawTerminal && observer.sawSuccess && !observer.sawFailure {
		result.Outcome = CompletionSucceeded
	}
	if errors.Is(r.Context().Err(), context.Canceled) || errors.Is(copyErr, context.Canceled) {
		result.Outcome = CompletionCanceled
	}
	s.finishCompletion(started, result)
}
