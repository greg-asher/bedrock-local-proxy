package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type gatedStreamBody struct {
	first       []byte
	rest        []byte
	blocked     chan struct{}
	release     chan struct{}
	closeCalled chan struct{}
	onceBlock   sync.Once
	onceClose   sync.Once
}

func (b *gatedStreamBody) Read(p []byte) (int, error) {
	if len(b.first) > 0 {
		n := copy(p, b.first)
		b.first = b.first[n:]
		return n, nil
	}
	b.onceBlock.Do(func() { close(b.blocked) })
	<-b.release
	if len(b.rest) == 0 {
		return 0, io.EOF
	}
	n := copy(p, b.rest)
	b.rest = b.rest[n:]
	return n, nil
}

func (b *gatedStreamBody) Close() error {
	b.onceClose.Do(func() { close(b.closeCalled) })
	return nil
}

func TestChatStreamingForwardsIncrementallyAndObservesRealOpenAIChunks(t *testing.T) {
	const first = ": keep-alive\n\ndata: {\"id\":\"chatcmpl_test\",\"object\":\"chat.completion.chunk\",\"created\":1730000000,\"model\":\"target\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hel\"},\"finish_reason\":null}]}\n\n"
	const rest = "data: {\"id\":\"chatcmpl_test\",\"object\":\"chat.completion.chunk\",\"created\":1730000000,\"model\":\"target\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\\\"\"}}]},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chatcmpl_test\",\"object\":\"chat.completion.chunk\",\"created\":1730000000,\"model\":\"target\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"x\\\"}\"}}]},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chatcmpl_test\",\"object\":\"chat.completion.chunk\",\"created\":1730000000,\"model\":\"target\",\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":7,\"total_tokens\":19}}\n\n" +
		"data: {\"id\":\"chatcmpl_test\",\"object\":\"chat.completion.chunk\",\"created\":1730000000,\"model\":\"target\",\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":7,\"total_tokens\":19}}\n\n" +
		"data: [DONE]\n\n"
	body := &gatedStreamBody{first: []byte(first), rest: []byte(rest), blocked: make(chan struct{}), release: make(chan struct{}), closeCalled: make(chan struct{})}
	fake := &fakeRequestDoer{response: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}, "X-Request-Id": []string{"stream-1"}}, Body: body}}
	s := NewWithTransport(chatTestConfig(), fake)
	var result CompletionResult
	var resultCount int
	s.SetCompletionRecorder(func(got CompletionResult) {
		result = got
		resultCount++
	})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding","messages":[],"stream":true}`))
	done := make(chan struct{})
	go func() {
		s.ServeHTTP(recorder, request)
		close(done)
	}()
	select {
	case <-body.blocked:
	case <-time.After(time.Second):
		t.Fatal("stream did not wait after first chunk")
	}
	if got := recorder.Body.String(); got != first {
		t.Fatalf("first client bytes = %q, want only first upstream chunk %q", got, first)
	}
	select {
	case <-done:
		t.Fatal("handler completed before upstream released the terminal events")
	default:
	}
	close(body.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream did not complete after upstream release")
	}
	if recorder.Code != http.StatusOK || recorder.Body.String() != first+rest {
		t.Fatalf("stream response = (%d, %q), want unchanged SSE bytes", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Content-Type") != "text/event-stream" || recorder.Header().Get("X-Request-Id") != "stream-1" {
		t.Fatalf("stream headers not preserved: %v", recorder.Header())
	}
	if resultCount != 1 || result.Outcome != CompletionSucceeded || result.InputTokens == nil || *result.InputTokens != 12 || result.OutputTokens == nil || *result.OutputTokens != 7 {
		t.Fatalf("completion result = %+v (count=%d), want one successful result with cumulative usage", result, resultCount)
	}
	select {
	case <-body.closeCalled:
	case <-time.After(time.Second):
		t.Fatal("upstream stream body was not closed")
	}
	if fake.request == nil {
		t.Fatal("fake transport did not receive request")
	}
	requestBody, _ := io.ReadAll(fake.request.Body)
	if !strings.Contains(string(requestBody), `"stream":true`) {
		t.Fatalf("stream flag was not forwarded: %s", requestBody)
	}
}

func TestChatStreamingMalformedOrOversizedUsageDoesNotBreakTerminalDetection(t *testing.T) {
	oversized := "data: {\"id\":\"chatcmpl_large\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"padding\":\"" + strings.Repeat("x", maxUsageObservationBytes) + "\"}\n\n"
	const malformed = "data: {\"id\":\"chatcmpl_bad\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":\"many\"}}\n\n"
	const terminal = "data: {\"id\":\"chatcmpl_final\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":7,\"total_tokens\":19}}\n\ndata: [DONE]\n\n"
	for _, tt := range []struct {
		name string
		body string
	}{
		{name: "oversized record", body: oversized + terminal},
		{name: "malformed usage", body: malformed + terminal},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeRequestDoer{response: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(tt.body))}}
			s := NewWithTransport(chatTestConfig(), fake)
			var result CompletionResult
			s.SetCompletionRecorder(func(got CompletionResult) { result = got })
			recorder := httptest.NewRecorder()
			s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding","messages":[],"stream":true}`)))
			if recorder.Code != http.StatusOK || recorder.Body.String() != tt.body {
				t.Fatalf("response = (%d, %q), want unchanged stream", recorder.Code, recorder.Body.String())
			}
			if result.Outcome != CompletionSucceeded {
				t.Fatalf("completion result = %+v, want terminal success despite observer failure", result)
			}
			if result.InputTokens != nil || result.OutputTokens != nil {
				t.Fatalf("invalid usage became token totals: %+v", result)
			}
		})
	}
}

func TestChatStreamingNullUsageCountsRemainUnknownAndZeroRemainsKnown(t *testing.T) {
	for _, tt := range []struct {
		name       string
		usage      string
		wantInput  *int64
		wantOutput *int64
	}{
		{name: "null input", usage: `{"prompt_tokens":null,"completion_tokens":7,"total_tokens":7}`, wantOutput: int64Pointer(7)},
		{name: "null output", usage: `{"prompt_tokens":0,"completion_tokens":null,"total_tokens":0}`, wantInput: int64Pointer(0)},
		{name: "zero counts", usage: `{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}`, wantInput: int64Pointer(0), wantOutput: int64Pointer(0)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := "data: {\"id\":\"chatcmpl_usage\",\"object\":\"chat.completion.chunk\",\"created\":1730000000,\"model\":\"target\",\"choices\":[],\"usage\":" + tt.usage + "}\n\ndata: [DONE]\n\n"
			fake := &fakeRequestDoer{response: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}}
			s := NewWithTransport(chatTestConfig(), fake)
			var result CompletionResult
			s.SetCompletionRecorder(func(got CompletionResult) { result = got })
			recorder := httptest.NewRecorder()
			s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding","messages":[],"stream":true}`)))
			if recorder.Code != http.StatusOK || recorder.Body.String() != body {
				t.Fatalf("response = (%d, %q), want unchanged stream", recorder.Code, recorder.Body.String())
			}
			if result.Outcome != CompletionSucceeded {
				t.Fatalf("completion result = %+v, want terminal success", result)
			}
			if !sameInt64Pointer(result.InputTokens, tt.wantInput) || !sameInt64Pointer(result.OutputTokens, tt.wantOutput) {
				t.Fatalf("usage result = %+v, want input=%v output=%v", result, tt.wantInput, tt.wantOutput)
			}
		})
	}
}

func TestChatStreamingEOFBeforeDoneAndUpstreamErrorAreFailedWithoutInjectedBody(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
	}{
		{name: "interrupted EOF", body: "data: {\"id\":\"chatcmpl_partial\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n"},
		{name: "upstream error event", body: "data: {\"error\":{\"message\":\"provider failed\",\"type\":\"server_error\"}}\n\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeRequestDoer{response: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(tt.body))}}
			s := NewWithTransport(chatTestConfig(), fake)
			var result CompletionResult
			var count int
			s.SetCompletionRecorder(func(got CompletionResult) { result, count = got, count+1 })
			recorder := httptest.NewRecorder()
			s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding","messages":[],"stream":true}`)))
			if recorder.Code != http.StatusOK || recorder.Body.String() != tt.body {
				t.Fatalf("response = (%d, %q), want unchanged SSE body", recorder.Code, recorder.Body.String())
			}
			if count != 1 || result.Outcome != CompletionFailed {
				t.Fatalf("completion result = %+v (count=%d), want one failure", result, count)
			}
			if strings.Contains(recorder.Body.String(), `{"error":{"message":"AWS`) {
				t.Fatal("handler appended a local JSON error after streaming headers")
			}
		})
	}
}

type cancelAwareStreamBody struct {
	ctx         context.Context
	started     chan struct{}
	closeCalled chan struct{}
	onceStart   sync.Once
	onceClose   sync.Once
}

type contextBindingDoer struct {
	body     *cancelAwareStreamBody
	response *http.Response
}

func (d *contextBindingDoer) Do(ctx context.Context, _ *http.Request) (*http.Response, error) {
	d.body.ctx = ctx
	return d.response, nil
}

func (b *cancelAwareStreamBody) Read(_ []byte) (int, error) {
	b.onceStart.Do(func() { close(b.started) })
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *cancelAwareStreamBody) Close() error {
	b.onceClose.Do(func() { close(b.closeCalled) })
	return nil
}

func TestChatStreamingCancellationReachesUpstreamAndClosesBody(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	body := &cancelAwareStreamBody{ctx: ctx, started: make(chan struct{}), closeCalled: make(chan struct{})}
	fake := &contextBindingDoer{body: body, response: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}}
	s := NewWithTransport(chatTestConfig(), fake)
	var result CompletionResult
	s.SetCompletionRecorder(func(got CompletionResult) { result = got })
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding","messages":[],"stream":true}`)).WithContext(ctx)
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
		t.Fatal("canceled stream did not return")
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

func TestChatStreamingShutdownCancelsAfterBoundedDrain(t *testing.T) {
	ctx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	body := &cancelAwareStreamBody{ctx: ctx, started: make(chan struct{}), closeCalled: make(chan struct{})}
	fake := &contextBindingDoer{body: body, response: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}}
	s := NewWithTransport(chatTestConfig(), fake)
	var result CompletionResult
	s.SetCompletionRecorder(func(got CompletionResult) { result = got })
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding","messages":[],"stream":true}`)).WithContext(ctx)
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
	shutdownContext, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := s.Shutdown(shutdownContext); err != nil {
		t.Fatalf("shutdown returned unexpected error: %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream did not finish after bounded shutdown drain")
	}
	if result.Outcome != CompletionCanceled {
		t.Fatalf("completion result = %+v, want canceled after shutdown", result)
	}
	select {
	case <-body.closeCalled:
	case <-time.After(time.Second):
		t.Fatal("shutdown-canceled upstream body was not closed")
	}
}
