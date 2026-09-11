package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gregasher/bedrock-local-proxy/internal/transport"
)

func TestResponsesTransformsNativeRequestAndRecordsUsage(t *testing.T) {
	const responseBody = `{"id":"resp_123","object":"response","created_at":1730000000,"status":"completed","completed_at":1730000001,"error":null,"incomplete_details":null,"model":"us.openai.gpt-test-v1:0","output":[{"id":"msg_123","type":"message","role":"assistant","content":[{"type":"output_text","text":"hello","annotations":[]}]}],"usage":{"input_tokens":12,"input_tokens_details":{"cached_tokens":3},"output_tokens":7,"output_tokens_details":{"reasoning_tokens":2},"total_tokens":19},"metadata":{}}`
	fake := &fakeRequestDoer{response: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "X-Request-Id": []string{"resp-1"}},
		Body:       io.NopCloser(strings.NewReader(responseBody)),
	}}
	s := NewWithTransport(chatTestConfig(), fake)
	requestBody := `{"model":"coding","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"look this up"}]},{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"n\":9007199254740993123456789}"},{"type":"function_call_output","call_id":"call_1","output":"result"}],"tools":[{"type":"function","name":"lookup","description":"lookup a value","parameters":{"type":"object","properties":{"n":{"type":"integer"}}}}],"temperature":null,"max_output_tokens":0,"unknown_number":9007199254740993123456789}`
	request := httptest.NewRequest(http.MethodPost, "/v1/responses?trace=1", strings.NewReader(requestBody))
	request.Header.Set("Authorization", "Bearer local")
	recorder := httptest.NewRecorder()
	var result CompletionResult
	s.SetCompletionRecorder(func(got CompletionResult) { result = got })
	s.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK || recorder.Body.String() != responseBody {
		t.Fatalf("response = (%d, %q), want unchanged Responses response", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("X-Request-Id") != "resp-1" {
		t.Fatal("upstream response header was not preserved")
	}
	if fake.request == nil || fake.request.URL.Path != "/openai/v1/responses" || fake.request.URL.RawQuery != "trace=1" {
		t.Fatalf("upstream request = %#v, want native Responses route with query", fake.request)
	}
	if fake.request.Header.Get("Authorization") != "Bearer local" {
		t.Fatalf("handler should pass headers to shared transport for sanitization, got %q", fake.request.Header.Get("Authorization"))
	}
	body, err := io.ReadAll(fake.request.Body)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["model"]) != `"us.anthropic.claude-sonnet-test-v1:0"` {
		t.Fatalf("model = %s, want configured Bedrock target", got["model"])
	}
	if string(got["temperature"]) != "null" || string(got["max_output_tokens"]) != "0" {
		t.Fatalf("explicit Responses values were overwritten: temperature=%s max_output_tokens=%s", got["temperature"], got["max_output_tokens"])
	}
	if string(got["unknown_number"]) != "9007199254740993123456789" || !strings.Contains(string(got["input"]), "function_call_output") {
		t.Fatalf("input items or numeric precision changed: %s", body)
	}
	if result.Endpoint != "/v1/responses" || result.Outcome != CompletionSucceeded || result.InputTokens == nil || *result.InputTokens != 12 || result.OutputTokens == nil || *result.OutputTokens != 7 || !result.ObservedUncoveredBillingFields {
		t.Fatalf("completion result = %+v, want successful usage with detail dimensions", result)
	}
}

func TestResponsesAppliesOnlyAbsentDocumentedDefaults(t *testing.T) {
	fake := &fakeRequestDoer{response: &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"id":"resp_123","object":"response","status":"completed","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}`)),
	}}
	s := NewWithTransport(chatTestConfig(), fake)
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"coding","input":"hello"}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	body, _ := io.ReadAll(fake.request.Body)
	if !strings.Contains(string(body), `"temperature":0.2`) || !strings.Contains(string(body), `"max_output_tokens":8192`) {
		t.Fatalf("absent Responses defaults missing: %s", body)
	}
	if strings.Contains(string(body), `"max_tokens"`) {
		t.Fatalf("Chat Completions max_tokens was injected into Responses request: %s", body)
	}
}

func TestResponsesValidationAndTransportErrorsUseOpenAIShape(t *testing.T) {
	for _, tt := range []struct {
		name     string
		body     string
		wantText string
	}{
		{name: "malformed", body: `{`, wantText: "JSON object"},
		{name: "missing model", body: `{"input":"hello"}`, wantText: "model must be"},
		{name: "unknown model", body: `{"model":"missing","input":"hello"}`, wantText: "configured models: coding, fast"},
		{name: "stream must be boolean", body: `{"model":"coding","input":"hello","stream":"true"}`, wantText: "stream must be a boolean"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeRequestDoer{}
			s := NewWithTransport(chatTestConfig(), fake)
			recorder := httptest.NewRecorder()
			s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(tt.body)))
			if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), tt.wantText) {
				t.Fatalf("response = (%d, %q), want 400 containing %q", recorder.Code, recorder.Body.String(), tt.wantText)
			}
			var envelope openAIErrorEnvelope
			if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil || envelope.Error.Type != "invalid_request_error" {
				t.Fatalf("response is not an OpenAI error envelope: %s", recorder.Body.Bytes())
			}
			if fake.request != nil {
				t.Fatal("invalid request reached upstream")
			}
		})
	}

	t.Run("upstream error is preserved", func(t *testing.T) {
		const body = `{"error":{"message":"responses unsupported for this model","type":"invalid_request_error","param":"model","code":"unsupported_model"}}`
		s := NewWithTransport(chatTestConfig(), &fakeRequestDoer{response: &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}})
		recorder := httptest.NewRecorder()
		s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"coding","input":"hello"}`)))
		if recorder.Code != http.StatusBadRequest || recorder.Body.String() != body {
			t.Fatalf("upstream error = (%d, %q), want unchanged Responses error", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("expired credentials", func(t *testing.T) {
		s := NewWithTransport(chatTestConfig(), &fakeRequestDoer{err: &transport.Failure{Class: transport.FailureCredentialsExpired}})
		recorder := httptest.NewRecorder()
		s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"coding","input":"hello"}`)))
		if recorder.Code != http.StatusUnauthorized || !strings.Contains(recorder.Body.String(), "aws sso login --profile YOUR_AWS_PROFILE") {
			t.Fatalf("response = (%d, %q), want actionable expired-profile error", recorder.Code, recorder.Body.String())
		}
	})
}

func TestResponsesStreamingPreservesRealSSEAndUsesTerminalUsage(t *testing.T) {
	const streamBody = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_123\",\"object\":\"response\",\"status\":\"in_progress\",\"model\":\"target\"}}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_123\",\"output_index\":0,\"content_index\":0,\"delta\":\"Hel\",\"sequence_number\":1}\n\n" +
		"event: response.output_text.done\ndata: {\"type\":\"response.output_text.done\",\"item_id\":\"msg_123\",\"output_index\":0,\"content_index\":0,\"text\":\"Hello\",\"sequence_number\":2}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_123\",\"object\":\"response\",\"status\":\"completed\",\"output\":[{\"id\":\"msg_123\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"Hello\",\"annotations\":[]}]}],\"usage\":{\"input_tokens\":12,\"output_tokens\":7,\"total_tokens\":19,\"input_tokens_details\":{\"cached_tokens\":3}}}}\n\n"
	fake := &fakeRequestDoer{response: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}, "X-Request-Id": []string{"stream-1"}}, Body: io.NopCloser(strings.NewReader(streamBody))}}
	s := NewWithTransport(chatTestConfig(), fake)
	var result CompletionResult
	var count int
	s.SetCompletionRecorder(func(got CompletionResult) { result, count = got, count+1 })
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"coding","input":"hello","stream":true}`)))
	if recorder.Code != http.StatusOK || recorder.Body.String() != streamBody {
		t.Fatalf("stream response = (%d, %q), want unchanged Responses SSE", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("X-Request-Id") != "stream-1" || recorder.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream headers not preserved: %v", recorder.Header())
	}
	if count != 1 || result.Outcome != CompletionSucceeded || result.InputTokens == nil || *result.InputTokens != 12 || result.OutputTokens == nil || *result.OutputTokens != 7 || !result.ObservedUncoveredBillingFields {
		t.Fatalf("completion result = %+v (count=%d), want one successful terminal result", result, count)
	}
	if fake.request == nil {
		t.Fatal("fake transport did not receive request")
	}
	body, _ := io.ReadAll(fake.request.Body)
	if !strings.Contains(string(body), `"stream":true`) {
		t.Fatalf("stream flag was not forwarded: %s", body)
	}
}

func TestResponsesStreamingFailureAndInterruptedEOFDoNotAppendLocalBody(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
	}{
		{name: "failed terminal", body: "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_123\",\"object\":\"response\",\"status\":\"failed\",\"error\":{\"code\":\"model_error\",\"message\":\"provider failed\"}}}\n\n"},
		{name: "incomplete terminal", body: "event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp_123\",\"object\":\"response\",\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"}}}\n\n"},
		{name: "interrupted EOF", body: "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_123\",\"output_index\":0,\"content_index\":0,\"delta\":\"partial\",\"sequence_number\":1}\n\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeRequestDoer{response: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(tt.body))}}
			s := NewWithTransport(chatTestConfig(), fake)
			var result CompletionResult
			var count int
			s.SetCompletionRecorder(func(got CompletionResult) { result, count = got, count+1 })
			recorder := httptest.NewRecorder()
			s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"coding","input":"hello","stream":true}`)))
			if recorder.Code != http.StatusOK || recorder.Body.String() != tt.body {
				t.Fatalf("response = (%d, %q), want unchanged SSE body", recorder.Code, recorder.Body.String())
			}
			if count != 1 || result.Outcome != CompletionFailed {
				t.Fatalf("completion result = %+v (count=%d), want one failure", result, count)
			}
			if strings.Contains(recorder.Body.String(), `AWS upstream request failed`) {
				t.Fatal("handler appended a local JSON error after streaming headers")
			}
		})
	}
}

func TestResponsesStreamingObserverKeepsTerminalDetectionIndependentOfUsage(t *testing.T) {
	oversized := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"" + strings.Repeat("x", maxUsageObservationBytes) + "\"}\n\n"
	terminal := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_123\",\"object\":\"response\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":12,\"output_tokens\":7,\"total_tokens\":19}}}\n\n"
	body := oversized + terminal
	fake := &fakeRequestDoer{response: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}}
	s := NewWithTransport(chatTestConfig(), fake)
	var result CompletionResult
	s.SetCompletionRecorder(func(got CompletionResult) { result = got })
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"coding","input":"hello","stream":true}`)))
	if recorder.Body.String() != body || result.Outcome != CompletionSucceeded {
		t.Fatalf("response/result = (%q, %+v), want unchanged successful stream", recorder.Body.String(), result)
	}
	if result.InputTokens == nil || *result.InputTokens != 12 || result.OutputTokens == nil || *result.OutputTokens != 7 {
		t.Fatalf("terminal usage not observed after oversized event: %+v", result)
	}
}

func TestResponsesStreamingCancellationReachesUpstreamAndClosesBody(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	body := &cancelAwareStreamBody{ctx: ctx, started: make(chan struct{}), closeCalled: make(chan struct{})}
	fake := &contextBindingDoer{body: body, response: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}}
	s := NewWithTransport(chatTestConfig(), fake)
	var result CompletionResult
	s.SetCompletionRecorder(func(got CompletionResult) { result = got })
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"coding","input":"hello","stream":true}`)).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		s.ServeHTTP(recorder, request)
		close(done)
	}()
	select {
	case <-body.started:
	case <-time.After(time.Second):
		t.Fatal("upstream body did not start reading")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled Responses stream did not return")
	}
	if result.Outcome != CompletionCanceled {
		t.Fatalf("completion result = %+v, want canceled", result)
	}
	select {
	case <-body.closeCalled:
	case <-time.After(time.Second):
		t.Fatal("canceled upstream body was not closed")
	}
}
