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

const secretMessage = "secret upstream message text"

func serveRecorded(t *testing.T, doer RequestDoer, path, body string, header http.Header) (CompletionResult, *httptest.ResponseRecorder) {
	t.Helper()
	s := NewWithTransport(chatTestConfig(), doer)
	var result CompletionResult
	count := 0
	s.SetCompletionRecorder(func(got CompletionResult) { result, count = got, count+1 })
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	for key, values := range header {
		request.Header[key] = values
	}
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	if count != 1 {
		t.Fatalf("recorded %d completions, want 1", count)
	}
	if encoded, _ := json.Marshal(result); strings.Contains(string(encoded), secretMessage) {
		t.Fatalf("record retained upstream message text: %s", encoded)
	}
	return result, recorder
}

func requestBody(path string, stream bool) string {
	if path == "/v1/responses" {
		if stream {
			return `{"model":"coding","input":"hello","stream":true}`
		}
		return `{"model":"coding","input":"hello"}`
	}
	if stream {
		return `{"model":"coding","messages":[],"max_tokens":10,"stream":true}`
	}
	return `{"model":"coding","messages":[],"max_tokens":10}`
}

func upstream(status int, header http.Header, body string) *fakeRequestDoer {
	if header == nil {
		header = http.Header{}
	}
	header.Set("Content-Type", "application/json")
	return &fakeRequestDoer{response: &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}}
}

func checkClass(t *testing.T, result CompletionResult, class string, retryable *bool, code string) {
	t.Helper()
	if result.Outcome == CompletionSucceeded || result.FailureClass != class || result.UpstreamCode != code {
		t.Fatalf("result = outcome %s class %q code %q, want class %q code %q", result.Outcome, result.FailureClass, result.UpstreamCode, class, code)
	}
	if (retryable == nil) != (result.Retryable == nil) || (retryable != nil && *retryable != *result.Retryable) {
		t.Fatalf("retryable = %v, want %v", result.Retryable, retryable)
	}
}

var (
	yes = boolPtr(true)
	no  = boolPtr(false)
)

func boolPtr(value bool) *bool { return &value }

func TestFailureClassFromUpstreamIdentityAndStatus(t *testing.T) {
	message := `"message":"` + secretMessage + `"`
	for _, tt := range []struct {
		name      string
		path      string
		status    int
		errorType string
		body      string
		class     string
		retryable *bool
		code      string
	}{
		{name: "header identity", path: "/v1/chat/completions", status: 400, errorType: "ThrottlingException:http://internal.amazon.com/coral/com.amazon.bedrock/", body: `{` + message + `}`, class: "throttled", retryable: yes, code: "ThrottlingException"},
		{name: "header beats body", path: "/v1/responses", status: 500, errorType: "ModelTimeoutException", body: `{"error":{"type":"server_error",` + message + `}}`, class: "upstream_timeout", retryable: yes, code: "ModelTimeoutException"},
		{name: "body code", path: "/v1/chat/completions", status: 429, body: `{"error":{"code":"rate_limit_exceeded","type":"tokens",` + message + `}}`, class: "throttled", retryable: yes, code: "rate_limit_exceeded"},
		{name: "identity beats 429", path: "/v1/responses", status: 429, body: `{"error":{"code":"insufficient_quota",` + message + `}}`, class: "quota_exhausted", retryable: no, code: "insufficient_quota"},
		{name: "aws __type", path: "/v1/chat/completions", status: 400, body: `{"__type":"com.amazon.coral.validate#ValidationException",` + message + `}`, class: "invalid_request", retryable: no, code: "ValidationException"},
		{name: "anthropic overloaded", path: "/v1/messages", status: 529, body: `{"type":"error","error":{"type":"overloaded_error",` + message + `}}`, class: "service_unavailable", retryable: yes, code: "overloaded_error"},
		{name: "anthropic permission", path: "/v1/messages", status: 403, body: `{"type":"error","error":{"type":"permission_error",` + message + `}}`, class: "credentials", retryable: no, code: "permission_error"},
		{name: "model not found", path: "/v1/responses", status: 404, body: `{"error":{"code":"model_not_found",` + message + `}}`, class: "model_unavailable", retryable: no, code: "model_not_found"},
		{name: "status only 503", path: "/v1/chat/completions", status: 503, body: ``, class: "service_unavailable", retryable: yes},
		{name: "string error body", path: "/v1/chat/completions", status: 502, body: `{"error":"` + secretMessage + `"}`, class: "upstream_server", retryable: yes},
		{name: "unknown code keeps status", path: "/v1/responses", status: 400, body: `{"error":{"code":"odd_code",` + message + `}}`, class: "invalid_request", retryable: no, code: "odd_code"},
		{name: "message in code dropped", path: "/v1/chat/completions", status: 401, body: `{"error":{"code":"` + secretMessage + `"}}`, class: "credentials", retryable: no},
		{name: "unmapped status", path: "/v1/messages", status: 418, body: `{"type":"error","error":{"type":"teapot",` + message + `}}`, class: "upstream_error", code: "teapot"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			header := http.Header{}
			if tt.errorType != "" {
				header.Set("X-Amzn-Errortype", tt.errorType)
			}
			result, recorder := serveRecorded(t, upstream(tt.status, header, tt.body), tt.path, requestBody(tt.path, false), nil)
			if recorder.Body.String() != tt.body {
				t.Fatalf("relayed body = %q, want unchanged", recorder.Body.String())
			}
			if result.FailureCategory != "upstream_http" || result.RetryAfterMS != nil {
				t.Fatalf("category %q retry-after %v", result.FailureCategory, result.RetryAfterMS)
			}
			checkClass(t, result, tt.class, tt.retryable, tt.code)
		})
	}
}

func TestFailureClassFromTransportErrors(t *testing.T) {
	for _, tt := range []struct {
		class     transport.FailureClass
		want      string
		category  string
		retryable *bool
	}{
		{transport.FailureCredentialsExpired, "credentials", "transport", no},
		{transport.FailureCredentialsUnavailable, "credentials", "transport", no},
		{transport.FailureConnectivity, "connectivity", "transport", yes},
		{transport.FailureTimeout, "upstream_timeout", "transport", yes},
		{transport.FailureSigning, "proxy_configuration", "transport", no},
		{transport.FailureRedirect, "proxy_configuration", "transport", no},
		{transport.FailureCanceled, "canceled", "canceled", no},
	} {
		for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
			t.Run(string(tt.class)+path, func(t *testing.T) {
				result, recorder := serveRecorded(t, &fakeRequestDoer{err: &transport.Failure{Class: tt.class}}, path, requestBody(path, false), nil)
				if result.FailureCategory != tt.category {
					t.Fatalf("category = %q, want %q", result.FailureCategory, tt.category)
				}
				checkClass(t, result, tt.want, tt.retryable, "")
				var envelope struct {
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil || envelope.Error.Code != transportFailureClass(&transport.Failure{Class: tt.class}) {
					t.Fatalf("envelope = %s, want code from class", recorder.Body.String())
				}
			})
		}
	}
}

func TestMissingTransportIsProxyConfiguration(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		result, _ := serveRecorded(t, nil, path, requestBody(path, false), nil)
		if result.FailureCategory != "transport" {
			t.Fatalf("%s category = %q, want transport", path, result.FailureCategory)
		}
		checkClass(t, result, "proxy_configuration", no, "")
	}
}

func TestLocalValidationFailureClass(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		result, _ := serveRecorded(t, &fakeRequestDoer{}, path, `{"model":"coding","stream":"yes"}`, nil)
		checkClass(t, result, "local_validation", no, "")
	}
}

func TestStreamedFailureClasses(t *testing.T) {
	for _, tt := range []struct {
		name      string
		path      string
		body      string
		class     string
		retryable *bool
		code      string
	}{
		{name: "responses failed", path: "/v1/responses", body: "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"" + secretMessage + "\"}}}\n\n", class: "throttled", retryable: yes, code: "rate_limit_exceeded"},
		{name: "responses top-level error", path: "/v1/responses", body: "data: {\"type\":\"error\",\"code\":\"server_is_overloaded\",\"message\":\"" + secretMessage + "\"}\n\n", class: "service_unavailable", retryable: yes, code: "server_is_overloaded"},
		{name: "responses nested error", path: "/v1/responses", body: "data: {\"type\":\"error\",\"error\":{\"type\":\"ModelStreamErrorException\",\"message\":\"" + secretMessage + "\"}}\n\n", class: "stream_interrupted", retryable: yes, code: "ModelStreamErrorException"},
		{name: "responses unknown error", path: "/v1/responses", body: "data: {\"type\":\"error\",\"code\":\"mystery\"}\n\n", class: "upstream_error", code: "mystery"},
		{name: "responses incomplete output limit", path: "/v1/responses", body: "data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"}}}\n\n", class: "invalid_request", retryable: no, code: "max_output_tokens"},
		{name: "responses no terminal", path: "/v1/responses", body: "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n", class: "stream_interrupted", retryable: yes},
		{name: "chat error", path: "/v1/chat/completions", body: "data: {\"error\":{\"code\":\"ThrottlingException\",\"message\":\"" + secretMessage + "\"}}\n\n", class: "throttled", retryable: yes, code: "ThrottlingException"},
		{name: "chat no done", path: "/v1/chat/completions", body: "data: {\"choices\":[]}\n\n", class: "stream_interrupted", retryable: yes},
		{name: "messages error", path: "/v1/messages", body: "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"" + secretMessage + "\"}}\n\n", class: "service_unavailable", retryable: yes, code: "overloaded_error"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeRequestDoer{response: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(tt.body))}}
			result, recorder := serveRecorded(t, fake, tt.path, requestBody(tt.path, true), nil)
			if recorder.Body.String() != tt.body {
				t.Fatalf("relayed stream = %q, want unchanged", recorder.Body.String())
			}
			checkClass(t, result, tt.class, tt.retryable, tt.code)
		})
	}
}

func TestStreamedNon2xxBodyIsObservedAndRelayed(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		const body = `{"error":{"type":"rate_limit_error","message":"` + secretMessage + `"}}`
		header := http.Header{"Retry-After": []string{"7"}}
		result, recorder := serveRecorded(t, upstream(http.StatusTooManyRequests, header, body), path, requestBody(path, true), nil)
		if recorder.Code != http.StatusTooManyRequests || recorder.Body.String() != body {
			t.Fatalf("%s relayed (%d, %q), want unchanged 429", path, recorder.Code, recorder.Body.String())
		}
		checkClass(t, result, "throttled", yes, "rate_limit_error")
		if result.RetryAfterMS == nil || *result.RetryAfterMS != 7000 {
			t.Fatalf("%s retry_after_ms = %v, want 7000", path, result.RetryAfterMS)
		}
	}
}

func TestRetryAfterAcceptsOnlyIntegerSeconds(t *testing.T) {
	for value, want := range map[string]int64{"0": 0, " 12 ": 12000, "Wed, 21 Oct 2026 07:28:00 GMT": -1, "-3": -1, "1.5": -1, "": -1} {
		got := retryAfterMS(http.Header{"Retry-After": []string{value}})
		if (want < 0) != (got == nil) || (got != nil && *got != want) {
			t.Fatalf("Retry-After %q = %v, want %d", value, got, want)
		}
	}
}

func TestReasoningReplayRejectedNeedsEncryptedInput(t *testing.T) {
	const body = `{"error":{"type":"invalid_request_error","code":null,"message":"` + secretMessage + `"}}`
	for _, tt := range []struct {
		name    string
		request string
		class   string
	}{
		{"encrypted reasoning", `{"model":"coding","input":[{"type":"reasoning","summary":[],"encrypted_content":"gAAAA"},{"role":"user","content":"hi"}]}`, "reasoning_replay_rejected"},
		{"empty encrypted content", `{"model":"coding","input":[{"type":"reasoning","summary":[],"encrypted_content":""}]}`, "invalid_request"},
		{"plain input", `{"model":"coding","input":[{"role":"user","content":"hi"}]}`, "invalid_request"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			result, _ := serveRecorded(t, upstream(http.StatusBadRequest, nil, body), "/v1/responses", tt.request, nil)
			checkClass(t, result, tt.class, no, "invalid_request_error")
		})
	}
	encrypted := `{"model":"coding","input":[{"type":"reasoning","encrypted_content":"gAAAA"}]}`
	result, _ := serveRecorded(t, upstream(http.StatusTooManyRequests, nil, `{"error":{"type":"invalid_request_error"}}`), "/v1/responses", encrypted, nil)
	checkClass(t, result, "invalid_request", no, "invalid_request_error")
}

func TestClientIdentityHeadersAreRecordedAndRunIsNotForwarded(t *testing.T) {
	const session = "0199a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"
	success := map[string]string{
		"/v1/chat/completions": `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
		"/v1/responses":        `{"output":[],"usage":{"input_tokens":1,"output_tokens":1}}`,
		"/v1/messages":         `{"content":[],"usage":{"input_tokens":1,"output_tokens":1}}`,
	}
	for path, body := range success {
		fake := upstream(http.StatusOK, nil, body)
		result, _ := serveRecorded(t, fake, path, requestBody(path, false), http.Header{"Session-Id": []string{session}, "X-Bedrock-Proxy-Run": []string{"run_42"}})
		if result.Outcome != CompletionSucceeded || result.ClientSessionID != session || result.ClientRun != "run_42" {
			t.Fatalf("%s result = %+v, want recorded client identity", path, result)
		}
		if result.FailureClass != "" || result.Retryable != nil {
			t.Fatalf("%s success has failure class %q", path, result.FailureClass)
		}
		if fake.request.Header.Get("X-Bedrock-Proxy-Run") != "" || fake.request.Header.Get("Session-Id") != session {
			t.Fatalf("%s upstream headers = %v", path, fake.request.Header)
		}
	}
	for _, invalid := range []string{"has space", strings.Repeat("a", 65), "semi;colon"} {
		result, _ := serveRecorded(t, upstream(http.StatusOK, nil, success["/v1/responses"]), "/v1/responses", requestBody("/v1/responses", false), http.Header{"Session-Id": []string{invalid}, "X-Bedrock-Proxy-Run": []string{invalid}})
		if result.ClientSessionID != "" || result.ClientRun != "" {
			t.Fatalf("invalid identity %q was recorded: %+v", invalid, result)
		}
	}
}

func TestCodexExecUserAgentIsCodexFamily(t *testing.T) {
	family, version := normalizedClient("codex_exec/0.159.0 (Mac OS 26.0.0; arm64) iTerm.app/3.6")
	if family != "codex" || version != "0.159.0" {
		t.Fatalf("client = %q %q, want codex 0.159.0", family, version)
	}
}
