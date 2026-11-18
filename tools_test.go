//go:build unix

package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/AlonaBot/AlonaBot/agent/provider"
	genresources "github.com/AlonaBot/AlonaBot/internal/generated"
	"github.com/AlonaBot/AlonaBot/sdk"
	"github.com/AlonaBot/AlonaBot/sdk/errcode"
	"github.com/AlonaBot/AlonaBot/sdk/service"
	toolapi "github.com/AlonaBot/AlonaBot/sdk/tool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const toolPluginID = "agent_tools"

type toolInput struct {
	Text string `json:"text"`
	Fail bool   `json:"fail,omitempty"`
}

type toolOutput struct {
	Text string `json:"text"`
}

func toolSchema[T any]() string { typ := reflect.TypeFor[T](); return typ.PkgPath() + "." + typ.Name() }

func toolPlugin(t *testing.T, policy toolapi.Default, calls *atomic.Int32) *sdk.Plugin {
	t.Helper()
	p := sdk.NewPlugin()
	genresources.Bind(p, genresources.Resources{Manifest: []byte("id: " + toolPluginID + "\nname: Agent tools test\nversion: 0.1.0\nauthor: AlonaBot\nsource: github.com/AlonaBot/AlonaBot\n"), ServiceSchemas: map[string][]byte{
		toolSchema[toolInput]():  []byte(`{"type":"object","properties":{"text":{"type":"string"},"fail":{"type":"boolean"}},"required":["text"]}`),
		toolSchema[toolOutput](): []byte(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`),
	}})
	service.Serve(p, service.JSON[toolInput, toolOutput](toolPluginID+"/echo")).Access(service.AccessPublic).Tool("Core echo", policy).Handle(func(c *sdk.Context, in toolInput) (toolOutput, error) {
		calls.Add(1)
		if c.User() != "agentloop:member" || in.Text != "actual parameters" {
			return toolOutput{}, errcode.InvalidArgument.New("unexpected actor or arguments")
		}
		if in.Fail {
			return toolOutput{Text: "partial result"}, errcode.Unavailable.New("tool failed")
		}
		return toolOutput{Text: "core result"}, nil
	})
	return p
}

func buildMCP(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "mcptest")
	command := exec.Command("go", "build", "-o", binary, "./mcptest")
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build MCP server: %v\n%s", err, out)
	}
	return binary
}

func httpMCP(t *testing.T, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "agent-test", Version: "1.0.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "echo/tool", Description: "MCP echo"}, func(_ context.Context, _ *mcp.CallToolRequest, in toolInput) (*mcp.CallToolResult, any, error) {
		calls.Add(1)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: in.Text}, &mcp.ImageContent{Data: []byte("image"), MIMEType: "image/png"}}, StructuredContent: map[string]any{"text": in.Text}, IsError: in.Fail}, nil, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	result := httptest.NewServer(handler)
	t.Cleanup(result.Close)
	return result
}

type completionRequest struct {
	Tools []struct {
		Function struct{ Name, Description string }
	} `json:"tools"`
	Messages []struct {
		Role, Content string
		ToolCallID    string `json:"tool_call_id"`
		ToolCalls     []struct {
			ID       string
			Function struct{ Name, Arguments string }
		} `json:"tool_calls"`
	} `json:"messages"`
}

func writeToolCall(w http.ResponseWriter, id, name string, input toolInput) {
	args, _ := json.Marshal(input)
	event := map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": id, "type": "function", "function": map[string]any{"name": name, "arguments": string(args)}}}}}}}
	raw, _ := json.Marshal(event)
	fmt.Fprintf(w, "data: %s\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", raw)
	w.(http.Flusher).Flush()
}

func checkToolResult(t *testing.T, request completionRequest, id, expected string) {
	t.Helper()
	last := request.Messages[len(request.Messages)-1]
	previous := request.Messages[len(request.Messages)-2]
	if last.Role != "tool" || last.ToolCallID != id || !strings.Contains(last.Content, expected) {
		t.Errorf("tool result = %+v, want %s/%s", last, id, expected)
	}
	if previous.Role != "assistant" || len(previous.ToolCalls) != 1 || previous.ToolCalls[0].ID != id {
		t.Errorf("assistant call = %+v", previous)
	}
}

// TestAgentTools 经真实 Core、SDK Session、SQLite 与两个 MCP 传输验证 V-58。
func TestAgentTools(t *testing.T) {
	t.Run("effects", func(t *testing.T) {
		cases := []struct {
			name, effect string
			annotations  *mcp.ToolAnnotations
		}{
			{"read", "read_only", &mcp.ToolAnnotations{ReadOnlyHint: true}},
			{"write", "write", &mcp.ToolAnnotations{DestructiveHint: new(false)}},
			{"remove", "destructive", &mcp.ToolAnnotations{DestructiveHint: new(true)}},
		}
		server := mcp.NewServer(&mcp.Implementation{Name: "effects", Version: "1.0.0"}, nil)
		var calls, requests atomic.Int32
		for _, entry := range cases {
			mcp.AddTool(server, &mcp.Tool{Name: entry.name, Description: entry.name, Annotations: entry.annotations}, func(_ context.Context, _ *mcp.CallToolRequest, in toolInput) (*mcp.CallToolResult, any, error) {
				calls.Add(1)
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: in.Text}}}, nil, nil
			})
		}
		endpoint := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
		t.Cleanup(endpoint.Close)
		model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var request completionRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			step := int(requests.Add(1)) - 1
			w.Header().Set("Content-Type", "text/event-stream")
			if step > 0 {
				checkToolResult(t, request, cases[step-1].name, "actual parameters")
			}
			if step == len(cases) {
				writeDelta(w, provider.OpenAICompatible, "effects finished")
				writeEnd(w, provider.OpenAICompatible)
				return
			}
			for _, tool := range request.Tools {
				if tool.Function.Description == cases[step].name {
					writeToolCall(w, cases[step].name, tool.Function.Name, toolInput{Text: "actual parameters"})
					return
				}
			}
			t.Errorf("missing MCP tool %s", cases[step].name)
			w.WriteHeader(400)
		}))
		t.Cleanup(model.Close)
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		address := listener.Addr().String()
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		configuration := agentConfig(model.URL, provider.OpenAICompatible)
		configuration["mcp_servers"] = []any{map[string]any{"name": "effects", "transport": "streamable-http", "url": endpoint.URL}}
		loop := newLoop(false, 1024)
		loop.buttons = true
		h := startAgent(t, loop, configuration, map[string]any{"listen": address})
		token, err := os.ReadFile(filepath.Join(h.dir, "admin_token"))
		if err != nil {
			t.Fatal(err)
		}
		h.deliver(t, "/ask effects", "member", "en-US", false)
		next := 0
		for _, entry := range cases {
			confirmation := h.confirmation(t, next)
			if !strings.Contains(confirmation.text, "mcp/effects/"+entry.name) {
				t.Fatalf("confirmation = %s", confirmation.text)
			}
			request, err := http.NewRequestWithContext(h.ctx, http.MethodGet, "http://"+address+"/api/v1/tools/policies", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			var directory struct {
				Policies []struct {
					PluginID                 string `json:"plugin_id"`
					Service, Effect, Default string
				}
			}
			decodeError := json.NewDecoder(response.Body).Decode(&directory)
			closeError := response.Body.Close()
			if response.StatusCode != http.StatusOK || decodeError != nil || closeError != nil {
				t.Fatalf("policies: status=%d decode=%v close=%v", response.StatusCode, decodeError, closeError)
			}
			found := false
			for _, policy := range directory.Policies {
				if policy.PluginID == agentID && policy.Service == "mcp/effects/"+entry.name {
					found = true
					if policy.Effect != entry.effect || policy.Default != "confirm" {
						t.Errorf("policy = %+v, want %s/confirm", policy, entry.effect)
					}
				}
			}
			if !found {
				t.Fatalf("missing recorded operation %s", entry.name)
			}
			next = len(loop.snapshot())
			h.click(t, confirmation, true, "member")
		}
		h.waitText(t, "effects finished")
		if calls.Load() != 3 || requests.Load() != 4 {
			t.Fatalf("calls=%d provider=%d", calls.Load(), requests.Load())
		}
	})
	binary := buildMCP(t)
	for _, scenario := range []struct {
		name, transport string
		deny, fail      bool
	}{
		{name: "stdio", transport: "stdio"},
		{name: "streamable-http", transport: "streamable-http"},
		{name: "confirmation-denied", transport: "streamable-http", deny: true},
		{name: "tool-error", transport: "streamable-http", fail: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var coreCalls, mcpCalls, requests atomic.Int32
			pidFile := filepath.Join(t.TempDir(), "pid")
			serverConfig := map[string]any{"name": "local", "transport": scenario.transport, "command": binary, "args": []string{"--pid-file", pidFile}}
			if scenario.transport == "streamable-http" {
				serverConfig["url"] = httpMCP(t, &mcpCalls).URL
			}
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request completionRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				var coreName, mcpName string
				for _, tool := range request.Tools {
					switch tool.Function.Description {
					case "Core echo":
						coreName = tool.Function.Name
					case "MCP echo":
						mcpName = tool.Function.Name
					}
				}
				if coreName == "" || mcpName == "" {
					t.Errorf("missing tools: %+v", request.Tools)
					w.WriteHeader(400)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				switch requests.Add(1) {
				case 1:
					writeToolCall(w, "core-call", coreName, toolInput{Text: "actual parameters", Fail: scenario.fail})
				case 2:
					expected := "core result"
					if scenario.fail {
						expected = "error: ERROR_CODE_UNAVAILABLE: tool failed"
					}
					checkToolResult(t, request, "core-call", expected)
					writeToolCall(w, "mcp-call", mcpName, toolInput{Text: "actual parameters", Fail: scenario.fail})
				case 3:
					expected := "actual parameters"
					if scenario.deny {
						expected = "error: ERROR_CODE_PERMISSION_DENIED: user denied confirmation"
					}
					checkToolResult(t, request, "mcp-call", expected)
					if !scenario.deny {
						result := request.Messages[len(request.Messages)-1].Content
						if scenario.transport == "stdio" && !strings.Contains(result, `"calls":1`) {
							t.Errorf("unexpected stdio MCP execution count: %s", result)
						}
						for _, field := range []string{`"structuredContent"`, `"type":"image"`, `"data":"aW1hZ2U="`} {
							if !strings.Contains(result, field) {
								t.Errorf("lost MCP result field %s: %s", field, result)
							}
						}
						if scenario.fail && !strings.Contains(result, `"isError":true`) {
							t.Errorf("lost MCP error: %s", result)
						}
					}
					writeDelta(w, provider.OpenAICompatible, "finished")
					writeEnd(w, provider.OpenAICompatible)
				default:
					t.Error("unexpected provider retry")
					w.WriteHeader(500)
				}
			}))
			t.Cleanup(model.Close)
			configuration := agentConfig(model.URL, provider.OpenAICompatible)
			configuration["mcp_servers"] = []any{serverConfig}
			loop := newLoop(false, 1024)
			loop.buttons = true
			h := startAgent(t, loop, configuration, nil, toolPlugin(t, toolapi.DefaultConfirm, &coreCalls))
			h.deliver(t, "/ask tools", "member", "en-US", false)
			coreConfirmation := h.confirmation(t, 0)
			if !strings.Contains(coreConfirmation.text, toolPluginID+"/echo") || !strings.Contains(coreConfirmation.text, "actual parameters") {
				t.Fatalf("core confirmation = %s", coreConfirmation.text)
			}
			next := len(loop.snapshot())
			h.click(t, coreConfirmation, true, "member")
			mcpConfirmation := h.confirmation(t, next)
			if !strings.Contains(mcpConfirmation.text, "mcp/local/echo/tool") || !strings.Contains(mcpConfirmation.text, `"text":"actual parameters"`) {
				t.Fatalf("MCP confirmation = %s", mcpConfirmation.text)
			}
			h.click(t, mcpConfirmation, !scenario.deny, "member")
			h.waitText(t, "finished")
			entries := h.waitEntries(t, 2)
			if entries[1]["content"] != "finished" || len(entries[1]) != 2 {
				t.Errorf("history contains tool state: %v", entries)
			}
			if coreCalls.Load() != 1 || requests.Load() != 3 {
				t.Errorf("execution counts core=%d provider=%d", coreCalls.Load(), requests.Load())
			}
			if scenario.transport == "streamable-http" {
				want := int32(1)
				if scenario.deny {
					want = 0
				}
				if mcpCalls.Load() != want {
					t.Errorf("MCP executions=%d, want %d", mcpCalls.Load(), want)
				}
			}
			if err := h.stop(); err != nil {
				t.Fatal(err)
			}
			if scenario.transport == "stdio" {
				raw, err := os.ReadFile(pidFile)
				if err != nil {
					t.Fatal(err)
				}
				var pid int
				if _, err := fmt.Sscan(string(raw), &pid); err != nil {
					t.Fatal(err)
				}
				if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
					t.Fatalf("MCP process %d remains: %v", pid, err)
				}
			}
		})
	}
}
