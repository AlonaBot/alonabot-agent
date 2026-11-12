package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

const testAPIKey = "test-secret"

// 模拟服务返回的 SSE 流。
const (
	openAITextAndTool = "data: {\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"x\\\"}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":7}}\n\n" +
		"data: [DONE]\n\n"
	anthropicTextAndTool = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3,\"cache_read_input_tokens\":2,\"output_tokens\":1}}}\n\n" +
		// 未知事件类型按 Anthropic 的版本策略忽略。
		"event: future_event\ndata: {\"type\":\"future_event\"}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hel\"}}\n\n" +
		": keep-alive\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"lo\"}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"c1\",\"name\":\"lookup\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"q\\\":\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"\\\"x\\\"}\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":7}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
)

// TestStream 用本地模拟 Provider 验证各协议的完整请求与流解析。
func TestStream(t *testing.T) {
	stopErr := errors.New("stop")
	ollamaTextAndTool := `{"message":{"content":"hel"}}
{"message":{"content":"lo"}}
{"message":{"tool_calls":[{"function":{"name":"lookup","arguments":{"q":"x"}}}]}}
{"done":true,"done_reason":"stop","prompt_eval_count":5,"eval_count":7}
`
	for _, tc := range []struct {
		name       string
		protocol   Protocol
		status     int
		stream     string
		emitErr    error // emit 返回的错误
		cancel     bool  // 发请求前取消 context
		wantText   string
		wantCalls  int
		wantErr    error  // 非 nil 时要求 errors.Is 匹配；为 nil 且 failed 时只要求出错
		errText    string // 非空时要求错误文本包含它
		failed     bool
		wantReplay string
		images     bool
	}{
		{name: "openai image", protocol: OpenAI, images: true, stream: openAITextAndTool, wantText: "hello", wantCalls: 1},
		{name: "openai-compatible image", protocol: OpenAICompatible, images: true, stream: openAITextAndTool, wantText: "hello", wantCalls: 1},
		{name: "anthropic image", protocol: AnthropicMessages, images: true, stream: anthropicTextAndTool, wantText: "hello", wantCalls: 1},
		{name: "ollama image", protocol: OllamaChat, images: true, stream: ollamaTextAndTool, wantText: "hello", wantCalls: 1},
		{name: "responses image", protocol: OpenAIResponses, images: true, wantText: "hello", wantCalls: 1,
			stream: "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"call_id\":\"c1\",\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\\\"x\\\"}\"}}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":5,\"output_tokens\":7}}}\n\n"},
		{name: "gemini image", protocol: GeminiGenerateContent, images: true, wantText: "hello", wantCalls: 1,
			stream: "data: " + `{"candidates":[{"content":{"parts":[{"text":"hello"},{"functionCall":{"id":"c1","name":"lookup","args":{"q":"x"}},"thoughtSignature":"response-signature"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":4,"thoughtsTokenCount":3}}` + "\n\n"},
		{name: "responses text and tool", protocol: OpenAIResponses, wantText: "hello", wantCalls: 1,
			wantReplay: `[{"type":"reasoning","encrypted_content":"opaque","summary":[]}]`,
			stream: "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hel\"}\n\n" +
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"lo\"}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\",\"id\":\"r1\",\"encrypted_content\":\"opaque\",\"summary\":[]}}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"id\":\"fc1\",\"call_id\":\"c1\",\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\\\"x\\\"}\"}}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":5,\"output_tokens\":7}}}\n\n"},
		{name: "responses missing completed", protocol: OpenAIResponses, wantText: "hello", failed: true,
			stream: "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"},
		{name: "openai text and tool", protocol: OpenAI, stream: openAITextAndTool, wantText: "hello", wantCalls: 1},
		{name: "openai-compatible text and tool", protocol: OpenAICompatible, stream: openAITextAndTool, wantText: "hello", wantCalls: 1},
		// 有的兼容实现在每个工具调用分段重复 id 与 name。
		{name: "openai-compatible repeated tool call ID", protocol: OpenAICompatible, wantText: "hello", wantCalls: 1,
			stream: strings.Replace(openAITextAndTool, `{\"index\":0,\"function\":{\"arguments\"`, `{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"lookup\",\"arguments\"`, 1)},
		{name: "anthropic text and tool", protocol: AnthropicMessages, stream: anthropicTextAndTool, wantText: "hello", wantCalls: 1},
		{name: "openai HTTP status", protocol: OpenAICompatible, status: http.StatusUnauthorized, stream: testAPIKey, failed: true},
		{name: "anthropic HTTP status with message", protocol: AnthropicMessages, status: http.StatusBadRequest, failed: true,
			stream: `{"type":"error","error":{"type":"invalid_request_error","message":"bad key ` + testAPIKey + `"}}`, errText: "status 400: bad key [REDACTED]"},
		{name: "anthropic HTTP status", protocol: AnthropicMessages, status: http.StatusTooManyRequests, stream: testAPIKey, failed: true},
		{name: "openai missing finish_reason", protocol: OpenAICompatible, stream: strings.Replace(openAITextAndTool, `,"finish_reason":"tool_calls"`, "", 1), wantText: "hello", failed: true},
		{name: "anthropic missing message_stop", protocol: AnthropicMessages, stream: strings.Split(anthropicTextAndTool, "event: content_block_start")[0], wantText: "hello", failed: true},
		{name: "openai cut mid-event", protocol: OpenAICompatible, stream: "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}", failed: true},
		{name: "openai malformed JSON", protocol: OpenAICompatible, stream: "data: {not json}\n\n", failed: true},
		{name: "openai error event", protocol: OpenAICompatible, stream: "data: {\"error\":{\"message\":\"overloaded\"}}\n\n", failed: true, errText: "error: overloaded"},
		{name: "anthropic error event", protocol: AnthropicMessages, stream: "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"overloaded\"}}\n\n", failed: true, errText: "error: overloaded"},
		{name: "anthropic tool arguments not object", protocol: AnthropicMessages, stream: strings.ReplaceAll(anthropicTextAndTool, `\"x\"}`, `\"x\"]`), wantText: "hello", failed: true},
		{name: "emit error stops stream", protocol: OpenAICompatible, stream: openAITextAndTool, emitErr: stopErr, wantErr: stopErr, failed: true},
		{name: "canceled context", protocol: AnthropicMessages, stream: anthropicTextAndTool, cancel: true, wantErr: context.Canceled, failed: true},
		{name: "ollama text and tool", protocol: OllamaChat, stream: ollamaTextAndTool, wantText: "hello", wantCalls: 1},
		{name: "ollama missing done", protocol: OllamaChat, stream: strings.Split(ollamaTextAndTool, `{"message":{"tool_calls"`)[0], wantText: "hello", failed: true},
		{name: "gemini text and tool", protocol: GeminiGenerateContent, wantText: "hello", wantCalls: 1,
			stream: "data: " + `{"candidates":[{"content":{"parts":[{"text":"hel"}]}}]}` + "\n\n" +
				"data: " + `{"candidates":[{"content":{"parts":[{"text":"lo"}]}}]}` + "\n\n" +
				"data: " + `{"candidates":[{"content":{"parts":[{"functionCall":{"id":"c1","name":"lookup","args":{"q":"x"}},"thoughtSignature":"response-signature"}]}}]}` + "\n\n" +
				"data: " + `{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":4,"thoughtsTokenCount":3}}` + "\n\n"},
		{name: "gemini missing finishReason", protocol: GeminiGenerateContent, wantText: "hello", failed: true, errText: "without finishReason",
			stream: "data: " + `{"candidates":[{"content":{"parts":[{"text":"hel"}]}}]}` + "\n\n" +
				"data: " + `{"candidates":[{"content":{"parts":[{"text":"lo"}]}}]}` + "\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(mockProvider(t, tc.protocol, tc.status, tc.stream, tc.images))
			t.Cleanup(server.Close)
			baseURL := server.URL + "/v1"
			if tc.protocol == AnthropicMessages {
				baseURL = server.URL
			}
			client, err := New(Config{Protocol: tc.protocol, BaseURL: baseURL, APIKey: testAPIKey, Model: "test-model"}, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				cancel()
			}

			var text strings.Builder
			var calls []ToolCall
			req := testRequest()
			if tc.images {
				png, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAAAAAA6fptVAAAACklEQVR4nGP4DwABAQEAsTj2FAAAAABJRU5ErkJggg==")
				if err != nil {
					t.Fatal(err)
				}
				req.Messages[2].Images = []Image{{MediaType: "image/png", Data: png}}
			}
			if tc.protocol == OpenAIResponses {
				req.Messages[3].ToolCalls[0].Replay = json.RawMessage(`[{"type":"reasoning","encrypted_content":"history","summary":[]}]`)
			}
			if tc.protocol == GeminiGenerateContent {
				req.Messages[3].ToolCalls[0].Replay = json.RawMessage(`"history-signature"`)
			}
			result, err := client.Stream(ctx, req, func(e Event) error {
				if e.ToolCall != nil {
					calls = append(calls, *e.ToolCall)
				}
				text.WriteString(e.Text)
				return tc.emitErr
			})

			if tc.failed != (err != nil) || (tc.wantErr != nil && !errors.Is(err, tc.wantErr)) {
				t.Fatalf("Stream() error = %v, want failed=%v (%v)", err, tc.failed, tc.wantErr)
			}
			if err != nil && (strings.Contains(err.Error(), testAPIKey) || !strings.Contains(err.Error(), tc.errText)) {
				t.Fatalf("error = %v, want text %q without API key", err, tc.errText)
			}
			// 成功的流都以 tool_use 结束，用量为 5 输入、7 输出（Anthropic 的缓存读取计入输入）。
			if err == nil && (result.StopReason != StopToolUse || result.Usage == nil || *result.Usage != (Usage{InputTokens: 5, OutputTokens: 7})) {
				t.Fatalf("result = %+v (usage %+v)", result, result.Usage)
			}
			if tc.emitErr == nil && text.String() != tc.wantText {
				t.Fatalf("text = %q, want %q", text.String(), tc.wantText)
			}
			if len(calls) != tc.wantCalls {
				t.Fatalf("tool calls = %+v, want %d", calls, tc.wantCalls)
			}
			if tc.wantCalls > 0 && (calls[0].ID == "" || (tc.protocol != OllamaChat && calls[0].ID != "c1") ||
				calls[0].Name != "lookup" || string(calls[0].Arguments) != `{"q":"x"}`) {
				t.Fatalf("tool call = %+v", calls[0])
			}
			if tc.wantReplay != "" {
				var got, want any
				if err := json.Unmarshal(calls[0].Replay, &got); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal([]byte(tc.wantReplay), &want); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("Replay = %s, want %s", calls[0].Replay, tc.wantReplay)
				}
			}
			if tc.protocol == GeminiGenerateContent && tc.wantCalls > 0 && string(calls[0].Replay) != `"response-signature"` {
				t.Fatalf("Replay = %s", calls[0].Replay)
			}
		})
	}
}

// mockProvider 返回模拟 Provider：校验路径、认证头与请求体编码后，按 status 返回 stream。
func mockProvider(t *testing.T, protocol Protocol, status int, stream string, images ...bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages            []map[string]any `json:"messages"`
			System              string           `json:"system"`
			Tools               []map[string]any `json:"tools"`
			MaxTokens           int              `json:"max_tokens"`
			MaxCompletionTokens int              `json:"max_completion_tokens"`
			StreamOptions       map[string]any   `json:"stream_options"`
			Options             struct {
				NumPredict int `json:"num_predict"`
			} `json:"options"`
			Input           []map[string]any `json:"input"`
			Instructions    string           `json:"instructions"`
			Store           *bool            `json:"store"`
			MaxOutputTokens int              `json:"max_output_tokens"`
			Include         []string         `json:"include"`
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if len(images) > 0 && images[0] {
			const png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAAAAAA6fptVAAAACklEQVR4nGP4DwABAQEAsTj2FAAAAABJRU5ErkJggg=="
			var message map[string]any
			var got, want any
			switch protocol {
			case OpenAI, OpenAICompatible:
				message = body.Messages[2]
				got = message["content"]
				want = []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + png}}, map[string]any{"type": "text", "text": "hi"}}
			case OpenAIResponses:
				message = body.Input[0]
				got = message["content"]
				want = []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + png}, map[string]any{"type": "input_text", "text": "hi"}}
			case AnthropicMessages:
				message = body.Messages[0]
				got = message["content"]
				want = []any{map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": png}}, map[string]any{"type": "text", "text": "hi"}}
			case OllamaChat:
				message = body.Messages[2]
				got = message["images"]
				want = []any{png}
				if message["content"] != "hi" {
					t.Errorf("image text = %#v", message["content"])
				}
			case GeminiGenerateContent:
				var fields map[string]any
				if err := json.Unmarshal(raw, &fields); err != nil {
					t.Error(err)
					return
				}
				message = fields["contents"].([]any)[0].(map[string]any)
				got = message["parts"]
				want = []any{map[string]any{"inlineData": map[string]any{"mimeType": "image/png", "data": png}}, map[string]any{"text": "hi"}}
				message["parts"] = []any{map[string]any{"text": "hi"}}
				var err error
				raw, err = json.Marshal(fields)
				if err != nil {
					t.Error(err)
					return
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("image content = %#v, want %#v", got, want)
			}
			// 图片已单独核对，复用原有断言检查请求的其余字段。
			message["content"] = "hi"
		}
		switch protocol {
		case OpenAIResponses:
			if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer "+testAPIKey || r.Header.Get("Accept") != "text/event-stream" {
				t.Errorf("method %s, path %s, headers %v", r.Method, r.URL.Path, r.Header)
			}
			if body.Store == nil || *body.Store || body.Instructions != "one\n\ntwo" || body.MaxOutputTokens != 20 || !reflect.DeepEqual(body.Include, []string{"reasoning.encrypted_content"}) {
				t.Errorf("request = %s", raw)
			}
			var wantInput []map[string]any
			if err := json.Unmarshal([]byte(`[
				{"role":"user","content":"hi"},
				{"role":"assistant","content":"checking"},
				{"type":"reasoning","encrypted_content":"history","summary":[]},
				{"type":"function_call","call_id":"c0","name":"lookup","arguments":"{\"q\":\"a\"}"},
				{"type":"function_call","call_id":"c00","name":"lookup","arguments":"{\"q\":\"b\"}"},
				{"type":"function_call_output","call_id":"c0","output":"A"},
				{"type":"function_call_output","call_id":"c00","output":"B"},
				{"role":"user","content":"continue"}
			]`), &wantInput); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(body.Input, wantInput) {
				t.Errorf("input = %s", raw)
			}
			if len(body.Tools) == 1 && (body.Tools[0]["strict"] != false || body.Tools[0]["type"] != "function" || body.Tools[0]["name"] != "lookup" || body.Tools[0]["description"] != "look up" || !reflect.DeepEqual(body.Tools[0]["parameters"], map[string]any{"type": "object"})) {
				t.Errorf("tools = %s", raw)
			}
			var fields map[string]any
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			if fields["model"] != "test-model" || fields["stream"] != true {
				t.Errorf("request = %s", raw)
			}
			if _, exists := fields["previous_response_id"]; exists {
				t.Errorf("unexpected previous_response_id: %s", raw)
			}
		case OpenAI, OpenAICompatible:
			if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer "+testAPIKey {
				t.Errorf("path %s, Authorization %q", r.URL.Path, r.Header.Get("Authorization"))
			}
			// 官方 OpenAI 用 max_completion_tokens 并请求用量，兼容实现用 max_tokens 且不发 stream_options。
			if (protocol == OpenAI) != (body.MaxCompletionTokens == 20 && body.MaxTokens == 0 && body.StreamOptions["include_usage"] == true) ||
				(protocol == OpenAICompatible && body.StreamOptions != nil) {
				t.Errorf("token limit = %s", raw)
			}
			// system、user、assistant、每个工具结果各一条 tool 消息，工具结果消息的文本随后成为 user 消息。
			if len(body.Messages) != 7 || body.Messages[4]["role"] != "tool" || body.Messages[5]["tool_call_id"] != "c00" ||
				body.Messages[6]["content"] != "continue" {
				t.Errorf("messages = %s", raw)
			}
		case AnthropicMessages:
			if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != testAPIKey || r.Header.Get("anthropic-version") != anthropicVersion {
				t.Errorf("path %s, headers %v", r.URL.Path, r.Header)
			}
			// system 合并到顶层；assistant 的文本在 tool_use 之前，user 的 tool_result 在文本之前。
			if body.System != "one\n\ntwo" || len(body.Messages) != 3 ||
				blockTypes(body.Messages[1]) != "text,tool_use,tool_use" || blockTypes(body.Messages[2]) != "tool_result,tool_result,text" {
				t.Errorf("request = %s", raw)
			}
		case OllamaChat:
			if r.URL.Path != "/v1/api/chat" || r.Header.Get("Authorization") != "Bearer "+testAPIKey ||
				r.Header.Get("Accept") != "application/x-ndjson" || body.Options.NumPredict != 20 {
				t.Errorf("path %s, headers %v, request %s", r.URL.Path, r.Header, raw)
			}
			// system 原序保留，工具结果带名称且在同条消息的 user 文本之前。
			if len(body.Messages) != 7 {
				t.Errorf("messages = %s", raw)
				break
			}
			if body.Messages[0]["role"] != "system" || body.Messages[0]["content"] != "one" ||
				body.Messages[1]["role"] != "system" || body.Messages[1]["content"] != "two" ||
				body.Messages[4]["role"] != "tool" || body.Messages[4]["tool_name"] != "lookup" || body.Messages[4]["content"] != "A" ||
				body.Messages[5]["role"] != "tool" || body.Messages[5]["tool_name"] != "lookup" || body.Messages[5]["content"] != "B" ||
				body.Messages[6]["role"] != "user" || body.Messages[6]["content"] != "continue" {
				t.Errorf("messages = %s", raw)
			}
			calls, _ := body.Messages[3]["tool_calls"].([]any)
			if body.Messages[3]["role"] != "assistant" || len(calls) != 2 {
				t.Errorf("assistant calls = %s", raw)
				break
			}
			for i, call := range calls {
				function := call.(map[string]any)["function"].(map[string]any)
				arguments, ok := function["arguments"].(map[string]any)
				if function["name"] != "lookup" || !ok || arguments["q"] != []string{"a", "b"}[i] {
					t.Errorf("tool arguments = %s", raw)
				}
			}
		case GeminiGenerateContent:
			if r.Method != http.MethodPost || r.URL.Path != "/v1/models/test-model:streamGenerateContent" || r.URL.RawQuery != "alt=sse" ||
				r.Header.Get("x-goog-api-key") != testAPIKey || r.Header.Get("Authorization") != "" || r.Header.Get("Accept") != "text/event-stream" {
				t.Errorf("request %s %s, headers %v", r.Method, r.URL, r.Header)
			}
			// 完整比对历史顺序、调用签名、结果关联的工具名与 Schema 原样编码。
			var got, want any
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Errorf("decode Gemini request: %v", err)
			}
			if err := json.Unmarshal([]byte(`{
				"systemInstruction":{"parts":[{"text":"one\n\ntwo"}]},
				"contents":[
					{"role":"user","parts":[{"text":"hi"}]},
					{"role":"model","parts":[
						{"text":"checking"},
						{"functionCall":{"id":"c0","name":"lookup","args":{"q":"a"}},"thoughtSignature":"history-signature"},
						{"functionCall":{"id":"c00","name":"lookup","args":{"q":"b"}}}
					]},
					{"role":"user","parts":[
						{"functionResponse":{"id":"c0","name":"lookup","response":{"output":"A"}}},
						{"functionResponse":{"id":"c00","name":"lookup","response":{"output":"B"}}},
						{"text":"continue"}
					]}
				],
				"tools":[{"functionDeclarations":[{"name":"lookup","description":"look up","parametersJsonSchema":{"type":"object"}}]}],
				"generationConfig":{"maxOutputTokens":20}
			}`), &want); err != nil {
				t.Errorf("decode expected Gemini request: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Gemini request = %s", raw)
			}
		}
		if len(body.Tools) != 1 {
			t.Errorf("tools = %s", raw)
		}
		if status != 0 {
			w.WriteHeader(status)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if protocol == OllamaChat {
			w.Header().Set("Content-Type", "application/x-ndjson")
		}
		fmt.Fprint(w, stream)
	}
}

// blockTypes 返回 Anthropic 消息各 content block 的 type，以逗号连接。
func blockTypes(message map[string]any) string {
	blocks, _ := message["content"].([]any)
	types := []string{}
	for _, block := range blocks {
		types = append(types, fmt.Sprint(block.(map[string]any)["type"]))
	}
	return strings.Join(types, ",")
}

func testRequest() Request {
	return Request{
		Messages: []Message{
			{Role: "system", Content: "one"},
			{Role: "system", Content: "two"},
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "checking", ToolCalls: []ToolCall{{ID: "c0", Name: "lookup", Arguments: json.RawMessage(`{"q":"a"}`)}, {ID: "c00", Name: "lookup", Arguments: json.RawMessage(`{"q":"b"}`)}}},
			{Role: "user", Content: "continue", ToolResults: []ToolResult{{ToolCallID: "c0", Content: "A"}, {ToolCallID: "c00", Content: "B"}}},
		},
		Tools:     []Tool{{Name: "lookup", Description: "look up", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		MaxTokens: 20,
	}
}
