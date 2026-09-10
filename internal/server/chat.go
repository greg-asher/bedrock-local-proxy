package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gregasher/bedrock-local-proxy/internal/config"
	"github.com/gregasher/bedrock-local-proxy/internal/transport"
)

// RequestDoer is the shared transport contract used by protocol handlers.
// transport.Transport satisfies it; tests can provide a local fake with the
// same HTTP request and response shapes.
type RequestDoer interface {
	Do(context.Context, *http.Request) (*http.Response, error)
}

// CompletionOutcome describes the terminal state of one generation request.
type CompletionOutcome string

const (
	CompletionSucceeded CompletionOutcome = "success"
	CompletionFailed    CompletionOutcome = "failure"
	CompletionCanceled  CompletionOutcome = "canceled"
)

// CompletionResult is the metadata-only result consumed by request
// accounting. Unknown values remain nil rather than being reported as zero.
type CompletionResult struct {
	Endpoint                       string
	LocalModel                     string
	UpstreamModel                  string
	Elapsed                        time.Duration
	HTTPStatus                     *int
	Outcome                        CompletionOutcome
	InputTokens                    *int64
	OutputTokens                   *int64
	ObservedUncoveredBillingFields bool
}

const maxUsageObservationBytes = 1 << 20

func (s *Server) serveChatCompletions(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		s.finishCompletion(started, CompletionResult{
			Endpoint: "/v1/chat/completions",
			Outcome:  CompletionFailed,
		})
		return
	}

	localModel, payload, stream, err := s.transformChatRequest(r)
	if err != nil {
		s.writeOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		s.finishCompletion(started, CompletionResult{
			Endpoint:   "/v1/chat/completions",
			LocalModel: localModel,
			Outcome:    CompletionFailed,
		})
		return
	}
	if s.transport == nil {
		s.writeOpenAIError(w, http.StatusBadGateway, "AWS transport is not configured", "upstream_error")
		s.finishCompletion(started, CompletionResult{
			Endpoint:   "/v1/chat/completions",
			LocalModel: localModel,
			Outcome:    CompletionFailed,
		})
		return
	}

	upstreamRequest, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "/openai/v1/chat/completions"+querySuffix(r), bytes.NewReader(payload))
	if err != nil {
		s.writeOpenAIError(w, http.StatusBadGateway, "could not create upstream request", "upstream_error")
		s.finishCompletion(started, CompletionResult{Endpoint: "/v1/chat/completions", LocalModel: localModel, Outcome: CompletionFailed})
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
		s.finishCompletion(started, CompletionResult{Endpoint: "/v1/chat/completions", LocalModel: localModel, UpstreamModel: configuredTarget(s.cfg, localModel), Outcome: outcome})
		return
	}
	if response == nil {
		s.writeOpenAIError(w, http.StatusBadGateway, "AWS upstream returned no response", "upstream_error")
		s.finishCompletion(started, CompletionResult{Endpoint: "/v1/chat/completions", LocalModel: localModel, UpstreamModel: configuredTarget(s.cfg, localModel), Outcome: CompletionFailed})
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
		s.serveChatStream(w, r, started, localModel, response)
		return
	}
	observed, copyErr := relayAndObserve(w, body, maxUsageObservationBytes)
	status := response.StatusCode
	result := CompletionResult{
		Endpoint:      "/v1/chat/completions",
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
		result.InputTokens, result.OutputTokens, result.ObservedUncoveredBillingFields = parseChatUsage(observed)
	}
	s.finishCompletion(started, result)
}

func (s *Server) transformChatRequest(r *http.Request) (string, []byte, bool, error) {
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

func querySuffix(r *http.Request) string {
	if r.URL == nil || r.URL.RawQuery == "" {
		return ""
	}
	return "?" + r.URL.RawQuery
}

func configuredTarget(cfg config.Config, name string) string {
	if model, ok := cfg.Models[name]; ok {
		return model.BedrockModelID
	}
	return ""
}

func configuredModels(cfg config.Config) string {
	names := make([]string, 0, len(cfg.Models))
	for name := range cfg.Models {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func (s *Server) writeTransportError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	message := "AWS upstream request failed"
	switch transport.ClassOf(err) {
	case transport.FailureCredentialsExpired:
		status = http.StatusUnauthorized
		message = fmt.Sprintf("AWS authentication expired for profile %s. Run: aws sso login --profile %s", s.cfg.AWS.Profile, s.cfg.AWS.Profile)
	case transport.FailureCredentialsUnavailable:
		status = http.StatusUnauthorized
		message = fmt.Sprintf("AWS credentials are unavailable for profile %s", s.cfg.AWS.Profile)
	}
	s.writeOpenAIError(w, status, message, "upstream_error")
}

func (s *Server) writeOpenAIError(w http.ResponseWriter, status int, message, kind string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(openAIErrorEnvelope{Error: openAIError{
		Message: message,
		Type:    kind,
	}})
}

type openAIErrorEnvelope struct {
	Error openAIError `json:"error"`
}

type openAIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   any    `json:"param"`
	Code    any    `json:"code"`
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func relayAndObserve(dst io.Writer, src io.Reader, limit int64) ([]byte, error) {
	var observed bytes.Buffer
	buf := make([]byte, 32*1024)
	observationEnabled := true
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, err := dst.Write(buf[:n]); err != nil {
				return nil, err
			}
			if observationEnabled {
				if int64(observed.Len()+n) <= limit {
					_, _ = observed.Write(buf[:n])
				} else {
					observationEnabled = false
					observed.Reset()
				}
			}
		}
		if readErr != nil {
			if readErr == io.EOF && observationEnabled {
				return observed.Bytes(), nil
			}
			return nil, readErr
		}
	}
}

// sseEvent is the bounded observer view of one SSE record. The original bytes
// are always written directly to the caller before the observer sees them.
// Oversized records are deliberately represented without their data so a
// large upstream event cannot grow observer memory.
type sseEvent struct {
	Data      []byte
	Oversized bool
}

// relayAndObserveStream is the shared byte relay for streaming protocols. It
// preserves every upstream byte, flushes after each read, and gives a bounded
// SSE observer one completed record at a time. Protocol handlers decide which
// event means success or failure; the relay only handles delivery and parsing.
func relayAndObserveStream(dst io.Writer, flush func(), src io.Reader, limit int64, observe func(sseEvent)) error {
	parser := newSSEParser(limit, observe)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, err := dst.Write(buf[:n]); err != nil {
				return err
			}
			parser.consume(buf[:n])
			flush()
		}
		if readErr != nil {
			if readErr == io.EOF {
				parser.finish()
				return nil
			}
			return readErr
		}
	}
}

type sseParser struct {
	limit         int
	line          []byte
	lineOversized bool
	data          bytes.Buffer
	dataOversized bool
	observe       func(sseEvent)
}

func newSSEParser(limit int64, observe func(sseEvent)) *sseParser {
	if limit < 1 {
		limit = 1
	}
	return &sseParser{limit: int(limit), observe: observe}
}

func (p *sseParser) consume(chunk []byte) {
	for _, b := range chunk {
		if b == '\n' {
			p.processLine()
			p.line = p.line[:0]
			p.lineOversized = false
			continue
		}
		if p.lineOversized {
			continue
		}
		if len(p.line) >= p.limit {
			p.lineOversized = true
			continue
		}
		p.line = append(p.line, b)
	}
}

func (p *sseParser) finish() {
	if len(p.line) > 0 || p.lineOversized {
		p.processLine()
	}
	if p.data.Len() > 0 || p.dataOversized {
		p.dispatch()
	}
}

func (p *sseParser) processLine() {
	if p.lineOversized {
		p.dataOversized = true
		return
	}
	line := p.line
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	if len(line) == 0 {
		p.dispatch()
		return
	}
	if line[0] == ':' {
		return
	}
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	value := line[len("data:"):]
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	if p.dataOversized {
		return
	}
	if p.data.Len()+len(value)+1 > p.limit {
		p.data.Reset()
		p.dataOversized = true
		return
	}
	_, _ = p.data.Write(value)
	_ = p.data.WriteByte('\n')
}

func (p *sseParser) dispatch() {
	if p.data.Len() == 0 && !p.dataOversized {
		return
	}
	event := sseEvent{Oversized: p.dataOversized}
	if !p.dataOversized {
		raw := p.data.Bytes()
		if len(raw) > 0 && raw[len(raw)-1] == '\n' {
			raw = raw[:len(raw)-1]
		}
		event.Data = append([]byte(nil), raw...)
	}
	p.data.Reset()
	p.dataOversized = false
	if p.observe != nil {
		p.observe(event)
	}
}

type chatStreamObserver struct {
	inputTokens  *int64
	outputTokens *int64
	uncovered    bool
	usageInvalid bool
	sawDone      bool
	sawError     bool
}

func (o *chatStreamObserver) observe(event sseEvent) {
	if event.Oversized {
		o.usageInvalid = true
		return
	}
	data := bytes.TrimSpace(event.Data)
	if bytes.Equal(data, []byte("[DONE]")) {
		o.sawDone = true
		return
	}
	if len(data) == 0 {
		return
	}
	var envelope struct {
		Error json.RawMessage `json:"error"`
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		o.usageInvalid = true
		return
	}
	if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
		o.sawError = true
	}
	if len(envelope.Usage) == 0 || string(envelope.Usage) == "null" {
		return
	}
	var usage map[string]json.RawMessage
	if err := json.Unmarshal(envelope.Usage, &usage); err != nil {
		o.usageInvalid = true
		return
	}
	if raw, ok := usage["prompt_tokens"]; ok {
		var value int64
		if err := json.Unmarshal(raw, &value); err != nil {
			o.usageInvalid = true
		} else {
			o.inputTokens = &value
		}
	}
	if raw, ok := usage["completion_tokens"]; ok {
		var value int64
		if err := json.Unmarshal(raw, &value); err != nil {
			o.usageInvalid = true
		} else {
			o.outputTokens = &value
		}
	}
	for key := range usage {
		if key != "prompt_tokens" && key != "completion_tokens" && key != "total_tokens" {
			o.uncovered = true
		}
	}
}

func (s *Server) serveChatStream(w http.ResponseWriter, r *http.Request, started time.Time, localModel string, response *http.Response) {
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
	observer := &chatStreamObserver{}
	copyErr := relayAndObserveStream(w, flush, body, maxUsageObservationBytes, observer.observe)
	status := response.StatusCode
	result := CompletionResult{
		Endpoint:      "/v1/chat/completions",
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
	if response.StatusCode >= 200 && response.StatusCode < 300 && normalEnd && observer.sawDone && !observer.sawError {
		result.Outcome = CompletionSucceeded
	}
	if errors.Is(r.Context().Err(), context.Canceled) || errors.Is(copyErr, context.Canceled) {
		result.Outcome = CompletionCanceled
	}
	s.finishCompletion(started, result)
}

func parseChatUsage(body []byte) (*int64, *int64, bool) {
	var response struct {
		Usage *struct {
			PromptTokens      *int64          `json:"prompt_tokens"`
			CompletionTokens  *int64          `json:"completion_tokens"`
			PromptDetails     json.RawMessage `json:"prompt_tokens_details"`
			CompletionDetails json.RawMessage `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &response); err != nil || response.Usage == nil {
		return nil, nil, false
	}
	uncovered := len(response.Usage.PromptDetails) > 0 && string(response.Usage.PromptDetails) != "null"
	uncovered = uncovered || len(response.Usage.CompletionDetails) > 0 && string(response.Usage.CompletionDetails) != "null"
	return response.Usage.PromptTokens, response.Usage.CompletionTokens, uncovered
}

func (s *Server) finishCompletion(started time.Time, result CompletionResult) {
	result.Elapsed = time.Since(started)
	if s.record != nil {
		s.record(result)
	}
}
