package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMessagesStreamingForwardsRealTextAndToolEventsIncrementally(t *testing.T) {
	const first = ": keep-alive\n\nevent: message_start\ndata: {" +
		`"type":"message_start","message":{"id":"msg_test","type":"message","role":"assistant","content":[],"model":"target","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":25,"cache_read_input_tokens":3,"output_tokens":1}}}` + "\n\n"
	const splitRecord = "event: content_block_start\ndata: {" + `"type":"content_block_`
	const firstWithSplitRecord = first + splitRecord
	const rest = `start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: ping\ndata: {" + `"type":"ping"}` + "\n\n" +
		"event: content_block_delta\ndata: {" + `"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}` + "\n\n" +
		"event: content_block_stop\ndata: {" + `"type":"content_block_stop","index":0}` + "\n\n" +
		"event: content_block_start\ndata: {" + `"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}}` + "\n\n" +
		"event: content_block_delta\ndata: {" + `"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\""}}` + "\n\n" +
		"event: content_block_delta\ndata: {" + `"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"x\"}"}}` + "\n\n" +
		"event: content_block_stop\ndata: {" + `"type":"content_block_stop","index":1}` + "\n\n" +
		"event: message_delta\ndata: {" + `"type":"message_delta","delta":{"stop_reason":null,"stop_sequence":null},"usage":{"output_tokens":4,"server_tool_use":{"web_search_requests":0}}}` + "\n\n" +
		"event: message_delta\ndata: {" + `"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":6}}` + "\n\n" +
		"event: message_stop\ndata: {" + `"type":"message_stop"}` + "\n\n"
	body := &gatedStreamBody{first: []byte(firstWithSplitRecord), rest: []byte(rest), blocked: make(chan struct{}), release: make(chan struct{}), closeCalled: make(chan struct{})}
	fake := &fakeRequestDoer{response: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "X-Request-Id": []string{"msg-stream-1"}},
		Body:       body,
	}}
	s := NewWithTransport(chatTestConfig(), fake)
	var result CompletionResult
	var resultCount int
	s.SetCompletionRecorder(func(got CompletionResult) {
		result = got
		resultCount++
	})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages?beta=true", strings.NewReader(`{"model":"coding","messages":[{"role":"user","content":"hello"}],"stream":true}`))
	request.Header.Set("anthropic-version", "2023-06-01")
	done := make(chan struct{})
	go func() {
		s.ServeHTTP(recorder, request)
		close(done)
	}()
	select {
	case <-body.blocked:
	case <-time.After(time.Second):
		t.Fatal("stream did not wait after first Messages events")
	}
	if got := recorder.Body.String(); got != firstWithSplitRecord {
		t.Fatalf("first client bytes = %q, want only first upstream events %q", got, firstWithSplitRecord)
	}
	select {
	case <-done:
		t.Fatal("handler completed before upstream released terminal events")
	default:
	}
	close(body.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream did not complete after upstream release")
	}
	if recorder.Code != http.StatusOK || recorder.Body.String() != firstWithSplitRecord+rest {
		t.Fatalf("stream response = (%d, %q), want unchanged Anthropic SSE bytes", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Content-Type") != "text/event-stream" || recorder.Header().Get("X-Request-Id") != "msg-stream-1" {
		t.Fatalf("stream headers not preserved: %v", recorder.Header())
	}
	if resultCount != 1 || result.Endpoint != "/v1/messages" || result.Outcome != CompletionSucceeded || result.InputTokens == nil || *result.InputTokens != 25 || result.OutputTokens == nil || *result.OutputTokens != 6 || !result.ObservedUncoveredBillingFields {
		t.Fatalf("completion result = %+v (count=%d), want one successful result with latest cumulative usage", result, resultCount)
	}
	select {
	case <-body.closeCalled:
	case <-time.After(time.Second):
		t.Fatal("upstream stream body was not closed")
	}
	if fake.request == nil {
		t.Fatal("fake transport did not receive request")
	}
	if fake.request.URL.Path != "/anthropic/v1/messages" || fake.request.URL.RawQuery != "beta=true" {
		t.Fatalf("upstream URL = %s, want native Anthropic route with query", fake.request.URL)
	}
	if fake.request.Header.Get("anthropic-version") != "2023-06-01" {
		t.Fatalf("Anthropic version header was not forwarded")
	}
	requestBody, _ := io.ReadAll(fake.request.Body)
	if !strings.Contains(string(requestBody), `"stream":true`) || !strings.Contains(string(requestBody), `"model":"us.anthropic.claude-sonnet-test-v1:0"`) {
		t.Fatalf("stream request was not forwarded with mapped model: %s", requestBody)
	}
}

func TestMessagesStreamingPrematureEOFAndStreamedErrorFailWithoutChangingBody(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
	}{
		{name: "premature EOF", body: "event: message_start\ndata: {" + `"type":"message_start","message":{"usage":{"input_tokens":2}}}` + "\n\n"},
		{name: "streamed error", body: "event: error\ndata: {" + `"type":"error","error":{"type":"overloaded_error","message":"try again"}}` + "\n\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeRequestDoer{response: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(tt.body))}}
			s := NewWithTransport(chatTestConfig(), fake)
			var result CompletionResult
			var count int
			s.SetCompletionRecorder(func(got CompletionResult) { result, count = got, count+1 })
			recorder := httptest.NewRecorder()
			s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"coding","messages":[],"stream":true}`)))
			if recorder.Code != http.StatusOK || recorder.Body.String() != tt.body {
				t.Fatalf("response = (%d, %q), want unchanged SSE body", recorder.Code, recorder.Body.String())
			}
			if count != 1 || result.Outcome != CompletionFailed {
				t.Fatalf("completion result = %+v (count=%d), want one failure", result, count)
			}
		})
	}
}

func TestMessagesStreamingOversizedUsageDoesNotBreakMessageStop(t *testing.T) {
	oversized := "event: message_delta\ndata: {" + `"type":"message_delta","usage":{"output_tokens":7,"padding":"` + strings.Repeat("x", maxUsageObservationBytes) + `"}}` + "\n\n"
	terminal := "event: message_stop\ndata: {" + `"type":"message_stop"}` + "\n\n"
	body := oversized + terminal
	fake := &fakeRequestDoer{response: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}}
	s := NewWithTransport(chatTestConfig(), fake)
	var result CompletionResult
	s.SetCompletionRecorder(func(got CompletionResult) { result = got })
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"coding","messages":[],"stream":true}`)))
	if recorder.Code != http.StatusOK || recorder.Body.String() != body {
		t.Fatalf("response = (%d, %q), want unchanged stream", recorder.Code, recorder.Body.String())
	}
	if result.Outcome != CompletionSucceeded || result.InputTokens != nil || result.OutputTokens != nil {
		t.Fatalf("completion result = %+v, want success with unknown usage after oversized event", result)
	}
}

func TestMessagesStreamingCancellationReachesUpstreamAndClosesBody(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	body := &cancelAwareStreamBody{ctx: ctx, started: make(chan struct{}), closeCalled: make(chan struct{})}
	fake := &contextBindingDoer{body: body, response: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}}
	s := NewWithTransport(chatTestConfig(), fake)
	var result CompletionResult
	s.SetCompletionRecorder(func(got CompletionResult) { result = got })
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"coding","messages":[],"stream":true}`)).WithContext(ctx)
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
		t.Fatal("canceled Messages stream did not return")
	}
	if result.Outcome != CompletionCanceled {
		t.Fatalf("completion result = %+v, want canceled outcome", result)
	}
	select {
	case <-body.closeCalled:
	case <-time.After(time.Second):
		t.Fatal("canceled upstream body was not closed")
	}
}

type messagesReadErrorBody struct {
	data []byte
	err  error
}

func (b *messagesReadErrorBody) Read(p []byte) (int, error) {
	if len(b.data) > 0 {
		n := copy(p, b.data)
		b.data = b.data[n:]
		return n, nil
	}
	return 0, b.err
}

func (b *messagesReadErrorBody) Close() error { return nil }

func TestMessagesStreamingPostHeaderReadErrorFailsWithoutInjectedErrorBody(t *testing.T) {
	partial := "event: message_start\ndata: {" + `"type":"message_start","message":{"usage":{"input_tokens":2}}}` + "\n\n"
	fake := &fakeRequestDoer{response: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: &messagesReadErrorBody{data: []byte(partial), err: errors.New("upstream reset")}}}
	s := NewWithTransport(chatTestConfig(), fake)
	var result CompletionResult
	s.SetCompletionRecorder(func(got CompletionResult) { result = got })
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"coding","messages":[],"stream":true}`)))
	if recorder.Code != http.StatusOK || recorder.Body.String() != partial {
		t.Fatalf("response = (%d, %q), want partial upstream bytes unchanged", recorder.Code, recorder.Body.String())
	}
	if result.Outcome != CompletionFailed || strings.Contains(recorder.Body.String(), `{"type":"error"}`) {
		t.Fatalf("completion result = %+v, response=%q; want failure without injected body", result, recorder.Body.String())
	}
}
