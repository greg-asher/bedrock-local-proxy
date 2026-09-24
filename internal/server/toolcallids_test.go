package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"testing/iotest"
)

func rewriteAll(t *testing.T, src io.Reader, suffix string) string {
	t.Helper()
	out, err := io.ReadAll(newToolCallIDRewriter(src, suffix))
	if err != nil {
		t.Fatalf("read rewritten body: %v", err)
	}
	return string(out)
}

func TestToolCallIDRewriterSuffixesOnlyIndexStyleIDs(t *testing.T) {
	in := `{"id":"chatcmpl_1","choices":[{"message":{"tool_calls":[` +
		`{"id":"call_0","type":"function","function":{"name":"a","arguments":"{\"id\":\"call_0\"}"}},` +
		`{"id": "call_12","type":"function","function":{"name":"b","arguments":"{}"}},` +
		`{"id":"call_Xy9z","type":"function","function":{"name":"c","arguments":"{}"}}]}}]}` + "\n"
	got := rewriteAll(t, strings.NewReader(in), "s1")
	want := strings.NewReplacer(`{"id":"call_0",`, `{"id":"call_0_s1",`, `{"id": "call_12"`, `{"id": "call_12_s1"`).Replace(in)
	if got != want {
		t.Fatalf("rewritten = %s\nwant      = %s", got, want)
	}
	if !strings.Contains(got, `\"id\":\"call_0\"`) {
		t.Fatal("an id inside escaped tool arguments was rewritten")
	}
}

func TestToolCallIDRewriterHandlesIdsSplitAcrossReadsAndUnterminatedTail(t *testing.T) {
	in := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_0\"}]}}]}\n\ndata: [DONE]\n\n" +
		`{"id":"call_3"}`
	got := rewriteAll(t, iotest.OneByteReader(strings.NewReader(in)), "s2")
	if !strings.Contains(got, `"id":"call_0_s2"`) || !strings.HasSuffix(got, `{"id":"call_3_s2"}`) {
		t.Fatalf("rewritten = %q", got)
	}
	if strings.ReplaceAll(got, "_s2", "") != in {
		t.Fatalf("bytes other than the ids changed: %q", got)
	}
}

// Bedrock's OpenAI-compatible endpoint answers every turn with call_0. After the
// proxy, the two turns' calls must differ, or an agent sees a replayed call.
func TestChatCompletionsGiveEachResponseDistinctToolCallIDs(t *testing.T) {
	const upstream = `{"id":"chatcmpl_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_0","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`
	ids := regexp.MustCompile(`"id":"(call_0_[0-9a-f]+)"`)
	seen := map[string]bool{}
	for turn := 0; turn < 2; turn++ {
		fake := &fakeRequestDoer{response: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(upstream))}}
		s := NewWithTransport(chatTestConfig(), fake)
		recorder := httptest.NewRecorder()
		s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding","messages":[]}`)))
		match := ids.FindStringSubmatch(recorder.Body.String())
		if recorder.Code != http.StatusOK || match == nil {
			t.Fatalf("turn %d response = (%d, %s)", turn, recorder.Code, recorder.Body.String())
		}
		if seen[match[1]] {
			t.Fatalf("turn %d repeated tool call id %s", turn, match[1])
		}
		seen[match[1]] = true
	}
}

func TestChatCompletionsLeaveErrorBodiesUnchanged(t *testing.T) {
	const upstream = `{"error":{"message":"bad call_0","id":"call_0"}}`
	fake := &fakeRequestDoer{response: &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(upstream))}}
	s := NewWithTransport(chatTestConfig(), fake)
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding","messages":[]}`)))
	if recorder.Body.String() != upstream {
		t.Fatalf("error body changed: %s", recorder.Body.String())
	}
}
