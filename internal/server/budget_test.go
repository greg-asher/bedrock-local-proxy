package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/greg-asher/bedrock-local-proxy/internal/config"
)

// budgetTestConfig prices every alias, so a test exercises the ceiling rather
// than the unpriced-model refusal.
func budgetTestConfig() config.Config {
	cfg := chatTestConfig()
	for alias, model := range cfg.Models {
		model.InputPerMillion, model.OutputPerMillion = float64Ptr(3), float64Ptr(15)
		cfg.Models[alias] = model
	}
	return cfg
}

func budgetTestServer(spend *BudgetSpend, limit float64) (*Server, *fakeRequestDoer) {
	fake := &fakeRequestDoer{response: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"id":"chatcmpl-test","object":"chat.completion","created":1730000000,"model":"target","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)),
	}}
	s := NewWithTransport(budgetTestConfig(), fake)
	s.SetBudget(limit, func() BudgetSpend { return *spend })
	return s, fake
}

func TestBudgetAllowsRequestsBelowTheCeiling(t *testing.T) {
	spend := BudgetSpend{Estimated: 0.25}
	s, fake := budgetTestServer(&spend, 1.0)
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding","messages":[{"role":"user","content":"hi"}]}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if fake.request == nil {
		t.Fatal("request below the budget was not proxied upstream")
	}
}

func TestBudgetRefusesGenerationOnceReached(t *testing.T) {
	for _, endpoint := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		t.Run(endpoint, func(t *testing.T) {
			spend := BudgetSpend{Estimated: 1.0}
			s, fake := budgetTestServer(&spend, 1.0)
			var recorded []CompletionResult
			s.SetCompletionRecorder(func(result CompletionResult) { recorded = append(recorded, result) })
			recorder := httptest.NewRecorder()
			s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(`{"model":"coding","messages":[{"role":"user","content":"hi"}]}`)))
			if recorder.Code != http.StatusPaymentRequired {
				t.Fatalf("status = %d, want 402", recorder.Code)
			}
			if fake.request != nil {
				t.Error("request over the budget reached the upstream transport")
			}
			if len(recorded) != 1 || recorded[0].FailureCategory != "budget_exhausted" || recorded[0].Outcome != CompletionFailed {
				t.Errorf("completion records = %+v", recorded)
			}
			var envelope map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("decode error body: %v", err)
			}
			body, ok := envelope["error"].(map[string]any)
			if !ok {
				t.Fatalf("error body = %s", recorder.Body.String())
			}
			message, _ := body["message"].(string)
			if !strings.Contains(message, "budget") {
				t.Errorf("message = %q, want it to explain the budget", message)
			}
			wantType := "insufficient_quota"
			if endpoint == "/v1/messages" {
				wantType = "permission_error"
			}
			if body["type"] != wantType {
				t.Errorf("error type = %v, want %q", body["type"], wantType)
			}
		})
	}
}

func TestBudgetDoesNotBlockModelDiscovery(t *testing.T) {
	spend := BudgetSpend{Estimated: 5.0}
	s, _ := budgetTestServer(&spend, 1.0)
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
}

func TestWithoutBudgetRequestsAreNeverRefused(t *testing.T) {
	fake := &fakeRequestDoer{response: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"id":"chatcmpl-test","object":"chat.completion","created":1730000000,"model":"target","choices":[],"usage":{}}`)),
	}}
	s := NewWithTransport(chatTestConfig(), fake)
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding","messages":[{"role":"user","content":"hi"}]}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
}

func TestBudgetRefusesModelWithoutConfiguredPrices(t *testing.T) {
	for _, endpoint := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		t.Run(endpoint, func(t *testing.T) {
			fake := &fakeRequestDoer{response: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{}`)),
			}}
			// chatTestConfig leaves every alias unpriced.
			s := NewWithTransport(chatTestConfig(), fake)
			s.SetBudget(10, func() BudgetSpend { return BudgetSpend{} })
			var recorded []CompletionResult
			s.SetCompletionRecorder(func(result CompletionResult) { recorded = append(recorded, result) })
			recorder := httptest.NewRecorder()
			s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(`{"model":"coding","messages":[{"role":"user","content":"hi"}]}`)))
			if recorder.Code != http.StatusPaymentRequired {
				t.Fatalf("status = %d, want 402", recorder.Code)
			}
			if fake.request != nil {
				t.Error("unpriced request reached the upstream transport")
			}
			if len(recorded) != 1 || recorded[0].LocalModel != "coding" || recorded[0].FailureCategory != "budget_exhausted" {
				t.Errorf("completion records = %+v", recorded)
			}
			if !strings.Contains(recorder.Body.String(), "input_per_million") {
				t.Errorf("body = %s, want the missing price named", recorder.Body.String())
			}
		})
	}
}

func TestUnpricedModelIsProxiedWithoutABudget(t *testing.T) {
	fake := &fakeRequestDoer{response: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"id":"x","object":"chat.completion","created":1,"model":"target","choices":[],"usage":{}}`)),
	}}
	s := NewWithTransport(chatTestConfig(), fake)
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding","messages":[{"role":"user","content":"hi"}]}`)))
	if recorder.Code != http.StatusOK || fake.request == nil {
		t.Fatalf("status = %d, proxied = %t; want an unpriced model to pass without --budget", recorder.Code, fake.request != nil)
	}
}

func TestBudgetFailsClosedAfterSpendGoesUncounted(t *testing.T) {
	spend := BudgetSpend{Estimated: 0.01, Uncounted: 1}
	s, fake := budgetTestServer(&spend, 100.0)
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding","messages":[{"role":"user","content":"hi"}]}`)))
	if recorder.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402 even far below the ceiling", recorder.Code)
	}
	if fake.request != nil {
		t.Error("request reached the transport after spend went uncounted")
	}
	if !strings.Contains(recorder.Body.String(), "could not be priced") {
		t.Errorf("body = %s", recorder.Body.String())
	}
}

// A wrong method is the endpoint's answer to give, not the budget's.
func TestBudgetDoesNotPreemptMethodNotAllowed(t *testing.T) {
	spend := BudgetSpend{Estimated: 99}
	s, _ := budgetTestServer(&spend, 1.0)
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/messages", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", recorder.Code)
	}
}
