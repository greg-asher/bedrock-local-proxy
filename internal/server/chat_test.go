package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gregasher/bedrock-local-proxy/internal/config"
	"github.com/gregasher/bedrock-local-proxy/internal/transport"
)

type fakeRequestDoer struct {
	request  *http.Request
	response *http.Response
	err      error
}

func (f *fakeRequestDoer) Do(_ context.Context, request *http.Request) (*http.Response, error) {
	f.request = request
	return f.response, f.err
}

func chatTestConfig() config.Config {
	return config.Config{
		AWS: config.AWSConfig{Profile: "YOUR_AWS_PROFILE", Region: "us-east-2"},
		Models: map[string]config.ModelConfig{
			"coding": {
				BedrockModelID: "us.anthropic.claude-sonnet-test-v1:0",
				Temperature:    float64Ptr(0.2),
				MaxTokens:      intPtr(8192),
			},
			"fast": {BedrockModelID: "us.anthropic.claude-haiku-test-v1:0"},
		},
	}
}

func float64Ptr(value float64) *float64 { return &value }
func intPtr(value int) *int             { return &value }

func TestChatCompletionTransformsRequestUsingOpenAIShape(t *testing.T) {
	fake := &fakeRequestDoer{response: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"id":"chatcmpl-test","object":"chat.completion","created":1730000000,"model":"us.anthropic.claude-sonnet-test-v1:0","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)),
	}}
	s := NewWithTransport(chatTestConfig(), fake)

	requestBody := `{"model":"coding","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"n\":9007199254740993123456789}"}}]},{"role":"tool","tool_call_id":"call_1","content":"result"}],"temperature":null,"max_tokens":0,"unknown_number":9007199254740993123456789,"unknown_object":{"nested":true}}`
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?trace=1", strings.NewReader(requestBody))
	request.Header.Set("Authorization", "Bearer local")
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	if fake.request == nil {
		t.Fatal("fake transport did not receive request")
	}
	if fake.request.URL.Path != "/openai/v1/chat/completions" || fake.request.URL.RawQuery != "trace=1" {
		t.Fatalf("upstream URL = %s, want OpenAI Bedrock route with query", fake.request.URL)
	}
	if fake.request.Header.Get("Authorization") != "Bearer local" {
		t.Fatalf("handler should pass headers to shared transport for sanitization, got %q", fake.request.Header.Get("Authorization"))
	}
	gotBody, err := io.ReadAll(fake.request.Body)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(gotBody, &got); err != nil {
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
	if !strings.Contains(string(got["messages"]), `9007199254740993123456789`) || !strings.Contains(string(got["messages"]), `"tool_call_id":"call_1"`) {
		t.Fatalf("tool content or numeric precision changed: %s", got["messages"])
	}
}

func TestChatCompletionAppliesOnlyAbsentConfiguredDefaults(t *testing.T) {
	fake := &fakeRequestDoer{response: &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"id":"chatcmpl-test","object":"chat.completion","created":1730000000,"model":"target","choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`)),
	}}
	s := NewWithTransport(chatTestConfig(), fake)
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding","messages":[]}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	body, _ := io.ReadAll(fake.request.Body)
	if !strings.Contains(string(body), `"temperature":0.2`) || !strings.Contains(string(body), `"max_tokens":8192`) {
		t.Fatalf("absent defaults missing from request: %s", body)
	}
}

func TestChatCompletionDoesNotAddLegacyTokenDefaultAlongsideMaxCompletionTokens(t *testing.T) {
	fake := &fakeRequestDoer{response: &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"id":"chatcmpl-test","object":"chat.completion","created":1730000000,"model":"target","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)),
	}}
	s := NewWithTransport(chatTestConfig(), fake)
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding","messages":[],"max_completion_tokens":123}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	body, _ := io.ReadAll(fake.request.Body)
	var got map[string]json.RawMessage
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["max_completion_tokens"]) != "123" {
		t.Fatalf("explicit max_completion_tokens changed: %s", body)
	}
	if _, exists := got["max_tokens"]; exists {
		t.Fatalf("legacy max_tokens default was added alongside max_completion_tokens: %s", body)
	}
}

func TestChatCompletionPreservesResponseAndRecordsUsage(t *testing.T) {
	const responseBody = `{"id":"chatcmpl-test","object":"chat.completion","created":1730000000,"model":"target","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":7,"total_tokens":19,"prompt_tokens_details":{"cached_tokens":2}}}`
	fake := &fakeRequestDoer{response: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "X-Request-Id": []string{"req-123"}},
		Body:       io.NopCloser(strings.NewReader(responseBody)),
	}}
	s := NewWithTransport(chatTestConfig(), fake)
	var result CompletionResult
	s.SetCompletionRecorder(func(got CompletionResult) { result = got })
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding","messages":[]}`)))
	if recorder.Code != http.StatusOK || recorder.Body.String() != responseBody {
		t.Fatalf("response = (%d, %q), want unchanged upstream response", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("X-Request-Id") != "req-123" {
		t.Fatalf("upstream response header was not preserved")
	}
	if result.Outcome != CompletionSucceeded || result.HTTPStatus == nil || *result.HTTPStatus != http.StatusOK {
		t.Fatalf("completion result = %+v, want successful 200", result)
	}
	if result.InputTokens == nil || *result.InputTokens != 12 || result.OutputTokens == nil || *result.OutputTokens != 7 || !result.ObservedUncoveredBillingFields {
		t.Fatalf("usage result = %+v, want token totals and uncovered billing dimension", result)
	}
}

func TestUsageParsingTreatsNegativeCountsAsUnknown(t *testing.T) {
	input, output, uncovered := parseChatUsage([]byte(`{"usage":{"prompt_tokens":-1,"completion_tokens":2,"total_tokens":1}}`))
	if input != nil || output == nil || *output != 2 || uncovered {
		t.Fatalf("usage = input:%v output:%v uncovered:%v, want negative input unknown and valid output preserved", input, output, uncovered)
	}
}

func TestChatCompletionValidationAndTransportErrors(t *testing.T) {
	for _, tt := range []struct {
		name       string
		body       string
		wantStatus int
		wantText   string
	}{
		{name: "malformed", body: `{`, wantStatus: http.StatusBadRequest, wantText: "JSON object"},
		{name: "missing model", body: `{"messages":[]}`, wantStatus: http.StatusBadRequest, wantText: "model must be"},
		{name: "unknown model", body: `{"model":"missing","messages":[]}`, wantStatus: http.StatusBadRequest, wantText: "configured models: coding, fast"},
		{name: "stream must be boolean", body: `{"model":"coding","messages":[],"stream":"true"}`, wantStatus: http.StatusBadRequest, wantText: "stream must be a boolean"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeRequestDoer{}
			s := NewWithTransport(chatTestConfig(), fake)
			recorder := httptest.NewRecorder()
			s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tt.body)))
			if recorder.Code != tt.wantStatus || !strings.Contains(recorder.Body.String(), tt.wantText) {
				t.Fatalf("response = (%d, %q), want status %d containing %q", recorder.Code, recorder.Body.String(), tt.wantStatus, tt.wantText)
			}
			if fake.request != nil {
				t.Fatal("invalid request reached upstream")
			}
		})
	}

	t.Run("expired credentials", func(t *testing.T) {
		s := NewWithTransport(chatTestConfig(), &fakeRequestDoer{err: &transport.Failure{Class: transport.FailureCredentialsExpired}})
		recorder := httptest.NewRecorder()
		s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding","messages":[]}`)))
		if recorder.Code != http.StatusUnauthorized || !strings.Contains(recorder.Body.String(), "aws sso login --profile YOUR_AWS_PROFILE") {
			t.Fatalf("response = (%d, %q), want actionable expired-profile error", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("connectivity", func(t *testing.T) {
		s := NewWithTransport(chatTestConfig(), &fakeRequestDoer{err: &transport.Failure{Class: transport.FailureConnectivity}})
		recorder := httptest.NewRecorder()
		s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding","messages":[]}`)))
		if recorder.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", recorder.Code)
		}
	})
}

func TestChatCompletionPreservesUpstreamErrorEnvelope(t *testing.T) {
	const body = `{"error":{"message":"model access denied","type":"invalid_request_error","param":null,"code":"access_denied"}}`
	fake := &fakeRequestDoer{response: &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}}
	s := NewWithTransport(chatTestConfig(), fake)
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding","messages":[]}`)))
	if recorder.Code != http.StatusForbidden || recorder.Body.String() != body {
		t.Fatalf("upstream error = (%d, %q), want unchanged 403 envelope", recorder.Code, recorder.Body.String())
	}
}
