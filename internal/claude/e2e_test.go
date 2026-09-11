package claude

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

	"github.com/gregasher/bedrock-local-proxy/internal/config"
	"github.com/gregasher/bedrock-local-proxy/internal/server"
)

const claudeFakeSearchResult = "CLAUDE_FAKE_SEARCH_RESULT_42"

// TestClaudeCodeOfflineSettingsFlow exercises an installed Claude Code CLI
// against the same isolated localhost Messages fake used by doctor. It skips
// when Claude Code is absent; CI can install the minimum supported release to
// make this a required compatibility check.
func TestClaudeCodeOfflineSettingsFlow(t *testing.T) {
	command, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("Claude Code CLI is not installed")
	}
	if _, err := Probe(context.Background(), command, nil); err != nil {
		t.Skipf("installed Claude Code does not support generated multi-model settings: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	root := t.TempDir()
	contextWindow, output := int64(200000), int64(32000)
	messages, functions, parallel, reasoning := true, true, true, false
	cfg := config.Config{
		Version: 1, Listen: "127.0.0.1:8787", AWS: config.AWSConfig{Profile: "offline", Region: "us-east-2"},
		Claude: config.ClaudeConfig{DefaultModel: "fable"},
		Models: map[string]config.ModelConfig{
			"fable": {
				DisplayName: "Fable", BedrockModelID: "anthropic.claude-sonnet-4-5-20250929-v1:0",
				Capabilities: &config.CapabilityConfig{},
			},
			"opus": {
				DisplayName: "Opus", BedrockModelID: "fake-bedrock-opus",
				Capabilities: &config.CapabilityConfig{
					MessagesAPI: &messages, AnthropicModelID: "claude-opus-5",
					ContextWindow: &contextWindow, MaxOutputTokens: &output, InputModalities: []string{"text", "image"},
					Reasoning: config.ReasoningCapability{Supported: &reasoning},
					Tools:     config.ToolCapability{FunctionCalling: &functions, ParallelCalls: &parallel},
				},
			},
		},
	}
	for _, selected := range []string{"fable", "opus"} {
		t.Run(selected, func(t *testing.T) {
			generated, err := Generate(ctx, GenerateOptions{
				Config: cfg, ConfigPath: filepath.Join(root, "config.yaml"), ModelAlias: selected,
				SettingsPath: filepath.Join(root, selected+"-settings.json"), ClaudeCommand: command,
			})
			if err != nil {
				t.Fatalf("generate Claude settings: %v", err)
			}
			if len(generated.ModelAliases) != 2 {
				t.Fatalf("picker aliases = %v", generated.ModelAliases)
			}
			if err := ValidateOfflineRequest(ctx, command, generated.SettingsPath, nil); err != nil {
				t.Fatalf("Claude Code offline settings flow: %v", err)
			}
		})
	}
}

func TestClaudeCodeOfflineMCPToolLoop(t *testing.T) {
	command, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("Claude Code CLI is not installed")
	}
	if _, err := Probe(context.Background(), command, nil); err != nil {
		t.Skipf("installed Claude Code does not support generated multi-model settings: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	root := t.TempDir()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Version: 1, Listen: listener.Addr().String(), AWS: config.AWSConfig{Profile: "offline", Region: "us-east-2"},
		Claude: config.ClaudeConfig{DefaultModel: "fable"},
		Models: map[string]config.ModelConfig{
			"fable": {DisplayName: "Fable", BedrockModelID: "anthropic.claude-sonnet-4-5-20250929-v1:0", Capabilities: &config.CapabilityConfig{}},
		},
	}
	upstream := &claudeMessagesFake{}
	proxy := server.NewWithTransport(cfg, upstream)
	done := make(chan error, 1)
	go func() { done <- proxy.Serve(listener) }()
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
		defer shutdownCancel()
		_ = proxy.Shutdown(shutdownCtx)
		<-done
	})
	settingsPath := filepath.Join(root, "settings.json")
	if _, err := Generate(ctx, GenerateOptions{Config: cfg, ConfigPath: filepath.Join(root, "config.yaml"), SettingsPath: settingsPath, ClaudeCommand: command}); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(root, "mcp-called")
	mcpConfig := map[string]any{"mcpServers": map[string]any{"search": map[string]any{
		"type": "stdio", "command": os.Args[0], "args": []string{"-test.run=TestClaudeMCPHelperProcess"},
		"env": map[string]string{"BEDROCK_PROXY_CLAUDE_MCP_HELPER": "1", "BEDROCK_PROXY_CLAUDE_MCP_MARKER": markerPath},
	}}}
	mcpData, _ := json.Marshal(mcpConfig)
	mcpPath := filepath.Join(root, "mcp.json")
	if err := os.WriteFile(mcpPath, mcpData, 0o600); err != nil {
		t.Fatal(err)
	}
	commandLine := exec.CommandContext(ctx, command,
		"--bare", "--settings", settingsPath, "-p", "--strict-mcp-config", "--mcp-config", mcpPath,
		"--dangerously-skip-permissions", "--output-format", "text",
		"Use the search MCP exactly once, then return its result verbatim.")
	commandLine.Dir = root
	commandLine.Env = []string{
		"HOME=" + filepath.Join(root, "home"), "CLAUDE_CONFIG_DIR=" + filepath.Join(root, "home", ".claude"),
		"PATH=" + os.Getenv("PATH"), "NO_PROXY=127.0.0.1,localhost", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_TELEMETRY=1",
	}
	output, err := commandLine.CombinedOutput()
	if err != nil {
		marker, markerErr := os.ReadFile(markerPath)
		t.Fatalf("Claude offline MCP run failed: %v\n%s\nupstream=%s marker=%q markerErr=%v", err, output, upstream.Summary(), marker, markerErr)
	}
	if !bytes.Contains(output, []byte(claudeFakeSearchResult)) {
		t.Fatalf("Claude output omitted MCP result:\n%s", output)
	}
	for _, warning := range []string{"unrecognized_model", "not a model this version of claude code recognizes"} {
		if bytes.Contains(bytes.ToLower(output), []byte(warning)) {
			t.Fatalf("Claude emitted model warning %q:\n%s", warning, output)
		}
	}
	if marker, err := os.ReadFile(markerPath); err != nil || strings.TrimSpace(string(marker)) != claudeFakeSearchResult {
		t.Fatalf("fake MCP marker=%q err=%v", marker, err)
	}
	if !upstream.ContinuationContains(claudeFakeSearchResult) {
		t.Fatalf("tool result did not return through Messages: %s", upstream.Summary())
	}
}

func TestClaudeMCPHelperProcess(t *testing.T) {
	if os.Getenv("BEDROCK_PROXY_CLAUDE_MCP_HELPER") != "1" {
		return
	}
	serveClaudeMCP(os.Stdin, os.Stdout, os.Getenv("BEDROCK_PROXY_CLAUDE_MCP_MARKER"))
	os.Exit(0)
}

func serveClaudeMCP(input io.Reader, output io.Writer, markerPath string) {
	scanner := bufio.NewScanner(input)
	encoder := json.NewEncoder(output)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil || len(request.ID) == 0 {
			continue
		}
		var result any
		switch request.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "claude-proxy-fake-search", "version": "1.0.0"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "query", "description": "Return the offline marker.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}, "required": []string{"query"}}}}}
		case "tools/call":
			_ = os.WriteFile(markerPath, []byte(claudeFakeSearchResult+"\n"), 0o600)
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": claudeFakeSearchResult}}, "isError": false}
		case "ping":
			result = map[string]any{}
		default:
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
			continue
		}
		_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}
}

type claudeMessagesFake struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (f *claudeMessagesFake) Do(_ context.Context, request *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.bodies = append(f.bodies, bytes.Clone(body))
	f.mu.Unlock()
	if bytes.Contains(body, []byte(claudeFakeSearchResult)) {
		return claudeSSEText(claudeFakeSearchResult), nil
	}
	var payload struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	_ = json.Unmarshal(body, &payload)
	for _, tool := range payload.Tools {
		if strings.Contains(tool.Name, "search") && strings.Contains(tool.Name, "query") {
			return claudeSSETool(tool.Name), nil
		}
	}
	return claudeSSEText("Offline session"), nil
}

func (f *claudeMessagesFake) ContinuationContains(value string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, body := range f.bodies {
		if bytes.Contains(body, []byte(value)) && bytes.Contains(body, []byte("tool_result")) {
			return true
		}
	}
	return false
}

func (f *claudeMessagesFake) Summary() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	limit := min(len(f.bodies), 10)
	values := make([]string, 0, limit+1)
	for index, body := range f.bodies[:limit] {
		values = append(values, fmt.Sprintf("%d:%s", index+1, string(body[:min(len(body), 200)])))
	}
	if len(f.bodies) > limit {
		values = append(values, fmt.Sprintf("... %d more requests", len(f.bodies)-limit))
	}
	return strings.Join(values, "; ")
}

func claudeSSEText(value string) *http.Response {
	events := []string{
		`{"type":"message_start","message":{"id":"msg_fake","type":"message","role":"assistant","content":[],"model":"target","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":` + strconv.Quote(value) + `}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}`,
		`{"type":"message_stop"}`,
	}
	return claudeEventResponse(events)
}

func claudeSSETool(name string) *http.Response {
	events := []string{
		`{"type":"message_start","message":{"id":"msg_fake","type":"message","role":"assistant","content":[],"model":"target","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_fake","name":` + strconv.Quote(name) + `,"input":{}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"marker\"}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":5}}`,
		`{"type":"message_stop"}`,
	}
	return claudeEventResponse(events)
}

func claudeEventResponse(events []string) *http.Response {
	var body strings.Builder
	for _, event := range events {
		var envelope struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(event), &envelope)
		fmt.Fprintf(&body, "event: %s\ndata: %s\n\n", envelope.Type, event)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body.String()))}
}
