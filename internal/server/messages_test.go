package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gregasher/bedrock-local-proxy/internal/transport"
)

func TestMessagesTransformsNativeAnthropicRequest(t *testing.T) {
	const responseBody = `{"id":"msg_test","type":"message","role":"assistant","content":[{"type":"text","text":"hello"}],"model":"us.anthropic.claude-sonnet-test-v1:0","stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":12,"output_tokens":7}}`
	fake := &fakeRequestDoer{response: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(responseBody)),
	}}
	s := NewWithTransport(chatTestConfig(), fake)
	requestBody := `{"model":"coding","max_tokens":0,"temperature":null,"system":[{"type":"text","text":"follow the policy","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"look this up"}]},{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"n":9007199254740993123456789}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"result"}]}]}],"tools":[{"name":"lookup","description":"lookup a value","input_schema":{"type":"object","properties":{"n":{"type":"integer"}}}}],"thinking":{"type":"enabled","budget_tokens":1024},"unknown_number":9007199254740993123456789}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages?beta=true", strings.NewReader(requestBody))
	request.Header.Set("anthropic-version", "2023-06-01")
	request.Header.Set("anthropic-beta", "prompt-caching-2024-07-31")
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK || recorder.Body.String() != responseBody {
		t.Fatalf("response = (%d, %q), want unchanged Messages response", recorder.Code, recorder.Body.String())
	}
	if fake.request == nil {
		t.Fatal("fake transport did not receive request")
	}
	if fake.request.URL.Path != "/anthropic/v1/messages" || fake.request.URL.RawQuery != "beta=true" {
		t.Fatalf("upstream URL = %s, want native Bedrock Anthropic route with query", fake.request.URL)
	}
	if fake.request.Header.Get("anthropic-version") != "2023-06-01" || fake.request.Header.Get("anthropic-beta") != "prompt-caching-2024-07-31" {
		t.Fatalf("Anthropic headers were not forwarded: version=%q beta=%q", fake.request.Header.Get("anthropic-version"), fake.request.Header.Get("anthropic-beta"))
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
	if string(got["temperature"]) != "null" || string(got["max_tokens"]) != "0" {
		t.Fatalf("explicit defaults were overwritten: temperature=%s max_tokens=%s", got["temperature"], got["max_tokens"])
	}
	if string(got["unknown_number"]) != "9007199254740993123456789" {
		t.Fatalf("large number changed: %s", got["unknown_number"])
	}
	for _, field := range []string{"system", "messages", "tools", "thinking"} {
		if len(got[field]) == 0 {
			t.Fatalf("native request field %q was lost", field)
		}
	}
}

func TestMessagesAppliesOnlyAbsentConfiguredDefaults(t *testing.T) {
	fake := &fakeRequestDoer{response: &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"id":"msg_test","type":"message","role":"assistant","content":[],"model":"target","stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`)),
	}}
	s := NewWithTransport(chatTestConfig(), fake)
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"coding","messages":[{"role":"user","content":"hello"}]}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	body, _ := io.ReadAll(fake.request.Body)
	if !strings.Contains(string(body), `"temperature":0.2`) || !strings.Contains(string(body), `"max_tokens":8192`) {
		t.Fatalf("absent defaults missing from request: %s", body)
	}
}

func TestMessagesPreservesResponseAndRecordsUsage(t *testing.T) {
	const responseBody = `{"id":"msg_test","type":"message","role":"assistant","content":[{"type":"text","text":"hello"}],"model":"target","stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":12,"output_tokens":7,"cache_read_input_tokens":3}}`
	fake := &fakeRequestDoer{response: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "X-Request-Id": []string{"req-123"}},
		Body:       io.NopCloser(strings.NewReader(responseBody)),
	}}
	s := NewWithTransport(chatTestConfig(), fake)
	var result CompletionResult
	s.SetCompletionRecorder(func(got CompletionResult) { result = got })
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"coding","messages":[]}`)))
	if recorder.Code != http.StatusOK || recorder.Body.String() != responseBody {
		t.Fatalf("response = (%d, %q), want unchanged upstream response", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("X-Request-Id") != "req-123" {
		t.Fatalf("upstream response header was not preserved")
	}
	if result.Endpoint != "/v1/messages" || result.Outcome != CompletionSucceeded || result.HTTPStatus == nil || *result.HTTPStatus != http.StatusOK {
		t.Fatalf("completion result = %+v, want successful Messages completion", result)
	}
	if result.InputTokens == nil || *result.InputTokens != 12 || result.OutputTokens == nil || *result.OutputTokens != 7 || !result.ObservedUncoveredBillingFields {
		t.Fatalf("usage result = %+v, want token totals and cache dimension", result)
	}
}

func TestMessagesValidationUsesAnthropicErrorsAndDoesNotCallUpstream(t *testing.T) {
	for _, tt := range []struct {
		name     string
		body     string
		wantText string
	}{
		{name: "malformed", body: `{`, wantText: "JSON object"},
		{name: "missing model", body: `{"messages":[]}`, wantText: "model must be"},
		{name: "unknown model", body: `{"model":"missing","messages":[]}`, wantText: "configured models: coding, fast"},
		{name: "stream must be boolean", body: `{"model":"coding","messages":[],"stream":"true"}`, wantText: "stream must be a boolean"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeRequestDoer{}
			s := NewWithTransport(chatTestConfig(), fake)
			recorder := httptest.NewRecorder()
			s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(tt.body)))
			if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), tt.wantText) {
				t.Fatalf("response = (%d, %q), want 400 containing %q", recorder.Code, recorder.Body.String(), tt.wantText)
			}
			var envelope anthropicErrorEnvelope
			if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil || envelope.Type != "error" || envelope.Error.Type != "invalid_request_error" {
				t.Fatalf("response is not an Anthropic error envelope: %s", recorder.Body.Bytes())
			}
			if fake.request != nil {
				t.Fatal("invalid request reached upstream")
			}
		})
	}
}

func TestMessagesTransportErrorsUseAnthropicErrors(t *testing.T) {
	t.Run("expired credentials", func(t *testing.T) {
		s := NewWithTransport(chatTestConfig(), &fakeRequestDoer{err: &transport.Failure{Class: transport.FailureCredentialsExpired}})
		recorder := httptest.NewRecorder()
		s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"coding","messages":[]}`)))
		if recorder.Code != http.StatusUnauthorized || !strings.Contains(recorder.Body.String(), "aws sso login --profile YOUR_AWS_PROFILE") {
			t.Fatalf("response = (%d, %q), want actionable expired-profile error", recorder.Code, recorder.Body.String())
		}
		var envelope anthropicErrorEnvelope
		if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil || envelope.Error.Type != "authentication_error" {
			t.Fatalf("response is not an Anthropic API error: %s", recorder.Body.Bytes())
		}
	})

	t.Run("connectivity", func(t *testing.T) {
		s := NewWithTransport(chatTestConfig(), &fakeRequestDoer{err: &transport.Failure{Class: transport.FailureConnectivity}})
		recorder := httptest.NewRecorder()
		s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"coding","messages":[]}`)))
		if recorder.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", recorder.Code)
		}
	})
}

func TestMessagesPreservesUpstreamErrorEnvelope(t *testing.T) {
	const body = `{"type":"error","error":{"type":"invalid_request_error","message":"model access denied"}}`
	fake := &fakeRequestDoer{response: &http.Response{
		StatusCode: http.StatusForbidden,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}}
	s := NewWithTransport(chatTestConfig(), fake)
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"coding","messages":[]}`)))
	if recorder.Code != http.StatusForbidden || recorder.Body.String() != body {
		t.Fatalf("upstream error = (%d, %q), want unchanged Anthropic 403 envelope", recorder.Code, recorder.Body.String())
	}
}

func TestMessagesMalformedUsageRemainsUnknown(t *testing.T) {
	fake := &fakeRequestDoer{response: &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"id":"msg_test","type":"message","role":"assistant","content":[],"model":"target","stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":12,"output_tokens":"seven"}}`)),
	}}
	s := NewWithTransport(chatTestConfig(), fake)
	var result CompletionResult
	s.SetCompletionRecorder(func(got CompletionResult) { result = got })
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"coding","messages":[]}`)))
	if recorder.Code != http.StatusOK || result.InputTokens == nil || *result.InputTokens != 12 || result.OutputTokens != nil {
		t.Fatalf("malformed usage was not kept partially unknown: status=%d result=%+v", recorder.Code, result)
	}
}

func TestMessagesNullUsageCountsRemainUnknownAndZeroRemainsKnown(t *testing.T) {
	for _, tt := range []struct {
		name       string
		usage      string
		wantInput  *int64
		wantOutput *int64
	}{
		{name: "null input", usage: `{"input_tokens":null,"output_tokens":7}`, wantOutput: int64Pointer(7)},
		{name: "null output", usage: `{"input_tokens":0,"output_tokens":null}`, wantInput: int64Pointer(0)},
		{name: "zero counts", usage: `{"input_tokens":0,"output_tokens":0}`, wantInput: int64Pointer(0), wantOutput: int64Pointer(0)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			responseBody := `{"id":"msg_test","type":"message","role":"assistant","content":[{"type":"text","text":"hello"}],"model":"target","stop_reason":"end_turn","stop_sequence":null,"usage":` + tt.usage + `}`
			fake := &fakeRequestDoer{response: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(responseBody)),
			}}
			s := NewWithTransport(chatTestConfig(), fake)
			var result CompletionResult
			s.SetCompletionRecorder(func(got CompletionResult) { result = got })
			recorder := httptest.NewRecorder()
			s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"coding","messages":[]}`)))
			if recorder.Code != http.StatusOK || recorder.Body.String() != responseBody {
				t.Fatalf("response = (%d, %q), want unchanged Messages response", recorder.Code, recorder.Body.String())
			}
			if !sameInt64Pointer(result.InputTokens, tt.wantInput) || !sameInt64Pointer(result.OutputTokens, tt.wantOutput) {
				t.Fatalf("usage result = %+v, want input=%v output=%v", result, tt.wantInput, tt.wantOutput)
			}
		})
	}
}

func int64Pointer(value int64) *int64 {
	return &value
}

func sameInt64Pointer(got, want *int64) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	return *got == *want
}

func TestMessagesCanceledTransportRecordsCanceledOutcome(t *testing.T) {
	s := NewWithTransport(chatTestConfig(), &fakeRequestDoer{err: &transport.Failure{Class: transport.FailureCanceled}})
	var result CompletionResult
	s.SetCompletionRecorder(func(got CompletionResult) { result = got })
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"coding","messages":[]}`)))
	if result.Outcome != CompletionCanceled {
		t.Fatalf("completion result = %+v, want canceled outcome", result)
	}
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want generic 502 for canceled transport fake", recorder.Code)
	}
}
