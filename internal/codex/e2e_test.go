package codex_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gregasher/bedrock-local-proxy/internal/codex"
	"github.com/gregasher/bedrock-local-proxy/internal/config"
	"github.com/gregasher/bedrock-local-proxy/internal/server"
)

const fakeSearchResult = "FAKE_SEARCH_RESULT_42"

// TestCodexOfflineMCPToolLoop exercises the installed Codex CLI against a
// localhost proxy, a captured Responses-compatible fake, and a stdio MCP fake.
// It skips only when Codex is not installed; CI can install the compatibility
// baseline to make this a required integration check.
func TestCodexOfflineMCPToolLoop(t *testing.T) {
	codexCommand, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("Codex CLI is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	root := t.TempDir()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxyConfig := integrationConfig(listener.Addr().String())
	upstream := &codexResponsesFake{}
	proxy := server.NewWithTransport(proxyConfig, upstream)
	serveDone := make(chan error, 1)
	go func() { serveDone <- proxy.Serve(listener) }()
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
		defer shutdownCancel()
		_ = proxy.Shutdown(shutdownCtx)
		<-serveDone
	})

	catalogPath := filepath.Join(root, "models.json")
	generated, err := codex.Generate(ctx, codex.GenerateOptions{
		Config: proxyConfig, ConfigPath: filepath.Join(root, "proxy.yaml"), CatalogPath: catalogPath,
		ProxyVersion: "integration-test", CodexCommand: codexCommand,
	})
	if err != nil {
		t.Fatalf("generate Codex catalog: %v", err)
	}
	markerPath := filepath.Join(root, "mcp-called")
	profile := generated.TOML + fmt.Sprintf("\n[mcp_servers.search]\ncommand = %s\nargs = [\"-test.run=TestCodexMCPHelperProcess\"]\nenv = { BEDROCK_PROXY_MCP_HELPER = \"1\", BEDROCK_PROXY_MCP_MARKER = %s }\n", strconv.Quote(os.Args[0]), strconv.Quote(markerPath))
	if err := os.WriteFile(filepath.Join(root, "bedrock-local.config.toml"), []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}

	// The process is confined to a temporary directory and local fakes. Bypass
	// interactive approvals so a headless test can execute the MCP call.
	command := exec.CommandContext(ctx, codexCommand, "exec", "--profile", "bedrock-local", "--ephemeral", "--skip-git-repo-check", "--ignore-rules", "--dangerously-bypass-approvals-and-sandbox", "--json", "Use the search MCP exactly once. Search for the diagnostic marker, then report the returned value verbatim.")
	command.Dir = root
	command.Env = append(os.Environ(), "CODEX_HOME="+root)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("Codex offline MCP run failed: %v\n%s", err, output)
	}
	if !bytes.Contains(output, []byte(fakeSearchResult)) {
		t.Fatalf("Codex output omitted the MCP result:\n%s", output)
	}
	for _, warning := range []string{"missing model metadata", "fallback model metadata", "not found in model catalog"} {
		if bytes.Contains(bytes.ToLower(output), []byte(warning)) {
			t.Fatalf("Codex emitted a model metadata warning %q:\n%s", warning, output)
		}
	}
	if marker, err := os.ReadFile(markerPath); err != nil || strings.TrimSpace(string(marker)) != fakeSearchResult {
		t.Fatalf("fake MCP was not called correctly: marker=%q err=%v requests=%s", marker, err, upstream.RequestSummary())
	}
	if calls := upstream.Calls(); calls < 2 {
		t.Fatalf("proxy received %d upstream calls, want tool call and continuation", calls)
	}
	if !upstream.ContinuationContains(fakeSearchResult) {
		t.Fatal("Codex continuation omitted the MCP result")
	}
}

func TestCodexMCPHelperProcess(t *testing.T) {
	if os.Getenv("BEDROCK_PROXY_MCP_HELPER") != "1" {
		return
	}
	serveMCP(os.Stdin, os.Stdout, os.Getenv("BEDROCK_PROXY_MCP_MARKER"))
	os.Exit(0)
}

func serveMCP(input io.Reader, output io.Writer, markerPath string) {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	encoder := json.NewEncoder(output)
	for scanner.Scan() {
		var request struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil || len(request.ID) == 0 {
			continue
		}
		var result any
		switch request.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "bedrock-proxy-fake-search", "version": "1.0.0"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "query", "description": "Return the offline diagnostic search result.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}, "required": []string{"query"}}}}}
		case "tools/call":
			_ = os.WriteFile(markerPath, []byte(fakeSearchResult+"\n"), 0o600)
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": fakeSearchResult}}, "isError": false}
		case "ping":
			result = map[string]any{}
		default:
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(request.ID), "error": map[string]any{"code": -32601, "message": "method not found"}})
			continue
		}
		_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(request.ID), "result": result})
	}
}

type codexResponsesFake struct {
	mu     sync.Mutex
	calls  int
	bodies [][]byte
}

func (f *codexResponsesFake) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *codexResponsesFake) ContinuationContains(value string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, body := range f.bodies[1:] {
		if bytes.Contains(body, []byte(value)) {
			return true
		}
	}
	return false
}

func (f *codexResponsesFake) RequestSummary() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var summaries []string
	for index, body := range f.bodies {
		var payload struct {
			Input json.RawMessage `json:"input"`
			Tools json.RawMessage `json:"tools"`
		}
		if json.Unmarshal(body, &payload) != nil {
			summaries = append(summaries, fmt.Sprintf("%d:invalid", index+1))
			continue
		}
		summaries = append(summaries, fmt.Sprintf("%d:input=%s tools=%d", index+1, summarizeInputTypes(payload.Input), len(payload.Tools)))
	}
	return strings.Join(summaries, "; ")
}

func summarizeInputTypes(raw json.RawMessage) string {
	var items []struct {
		Type      string `json:"type"`
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		CallID    string `json:"call_id"`
	}
	if json.Unmarshal(raw, &items) != nil {
		return "non-array"
	}
	var values []string
	for _, item := range items {
		values = append(values, strings.Trim(strings.Join([]string{item.Type, item.Namespace, item.Name, item.CallID}, "/"), "/"))
	}
	return strings.Join(values, ",")
}

func (f *codexResponsesFake) Do(_ context.Context, request *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.calls++
	callNumber := f.calls
	f.bodies = append(f.bodies, bytes.Clone(body))
	f.mu.Unlock()
	if callNumber > 1 {
		return eventStreamResponse(finalResponseEvents()), nil
	}
	var payload struct {
		Tools []struct {
			Type  string `json:"type"`
			Name  string `json:"name"`
			Tools []struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return jsonErrorResponse(http.StatusBadRequest, "invalid request JSON"), nil
	}
	namespace, toolName := "", ""
	for _, tool := range payload.Tools {
		if tool.Type != "namespace" || !strings.Contains(tool.Name, "search") {
			continue
		}
		for _, nested := range tool.Tools {
			if nested.Type == "function" && nested.Name == "query" {
				namespace, toolName = tool.Name, nested.Name
				break
			}
		}
	}
	if toolName == "" {
		return jsonErrorResponse(http.StatusBadRequest, fmt.Sprintf("search MCP namespace was not offered: %+v", payload.Tools)), nil
	}
	return eventStreamResponse(functionCallEvents(namespace, toolName)), nil
}

func functionCallEvents(namespace, name string) []sseEvent {
	item := map[string]any{"id": "fc_1", "type": "function_call", "status": "completed", "call_id": "call_1", "namespace": namespace, "name": name, "arguments": `{"query":"diagnostic marker"}`}
	response := map[string]any{"id": "resp_1", "object": "response", "created_at": 1, "status": "completed", "model": "fake-bedrock-target", "output": []any{item}, "usage": usage(10, 8)}
	return []sseEvent{
		{"response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_1", "object": "response", "created_at": 1, "status": "in_progress", "model": "fake-bedrock-target", "output": []any{}}}},
		{"response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item, "sequence_number": 1}},
		{"response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "item_id": "fc_1", "output_index": 0, "name": name, "arguments": item["arguments"], "sequence_number": 2}},
		{"response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item, "sequence_number": 3}},
		{"response.completed", map[string]any{"type": "response.completed", "response": response, "sequence_number": 4}},
	}
}

func finalResponseEvents() []sseEvent {
	part := map[string]any{"type": "output_text", "text": fakeSearchResult, "annotations": []any{}, "logprobs": []any{}}
	item := map[string]any{"id": "msg_1", "type": "message", "status": "completed", "role": "assistant", "content": []any{part}}
	response := map[string]any{"id": "resp_2", "object": "response", "created_at": 2, "status": "completed", "model": "fake-bedrock-target", "output": []any{item}, "usage": usage(18, 5)}
	return []sseEvent{
		{"response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_2", "object": "response", "created_at": 2, "status": "in_progress", "model": "fake-bedrock-target", "output": []any{}}}},
		{"response.output_text.delta", map[string]any{"type": "response.output_text.delta", "item_id": "msg_1", "output_index": 0, "content_index": 0, "delta": fakeSearchResult, "sequence_number": 1}},
		{"response.output_text.done", map[string]any{"type": "response.output_text.done", "item_id": "msg_1", "output_index": 0, "content_index": 0, "text": fakeSearchResult, "sequence_number": 2}},
		{"response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item, "sequence_number": 3}},
		{"response.completed", map[string]any{"type": "response.completed", "response": response, "sequence_number": 4}},
	}
}

type sseEvent struct {
	name string
	data any
}

func eventStreamResponse(events []sseEvent) *http.Response {
	var body strings.Builder
	for _, event := range events {
		encoded, _ := json.Marshal(event.data)
		fmt.Fprintf(&body, "event: %s\ndata: %s\n\n", event.name, encoded)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body.String()))}
}

func jsonErrorResponse(status int, message string) *http.Response {
	body, _ := json.Marshal(map[string]any{"error": map[string]any{"message": message, "type": "invalid_request_error"}})
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(body))}
}

func usage(input, output int) map[string]any {
	return map[string]any{"input_tokens": input, "output_tokens": output, "total_tokens": input + output, "input_tokens_details": map[string]any{"cached_tokens": 0}, "output_tokens_details": map[string]any{"reasoning_tokens": 0}}
}

func integrationConfig(listen string) config.Config {
	contextWindow, maxOutput := int64(200000), int64(64000)
	responses, reasoning, functions, parallel := true, false, true, true
	maxTokens := 8192
	return config.Config{Version: 1, AWS: config.AWSConfig{Profile: "must-not-load", Region: "us-east-2"}, Listen: listen, Models: map[string]config.ModelConfig{"coding": {
		DisplayName: "Offline MCP target", BedrockModelID: "fake-bedrock-target", MaxTokens: &maxTokens,
		Capabilities: &config.CapabilityConfig{ResponsesAPI: &responses, ContextWindow: &contextWindow, MaxOutputTokens: &maxOutput, InputModalities: []string{"text"}, Reasoning: config.ReasoningCapability{Supported: &reasoning}, Tools: config.ToolCapability{FunctionCalling: &functions, ParallelCalls: &parallel}},
	}}}
}
