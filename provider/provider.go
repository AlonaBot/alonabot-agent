// Package provider 以流式方式调用 Agent 的模型服务（Provider），
// 支持 OpenAI Chat Completions（官方与兼容实现）、OpenAI Responses、Anthropic Messages 与 Gemini GenerateContent 协议。
//
// [Client.Stream] 逐个解析 SSE 事件，按到达顺序把文本增量交给回调；
// 工具调用的参数分段到达，组装完整且是 JSON 对象后才交出。流正常结束时返回停止原因与用量（[Result]）。
// 只有收到协议的终止标记才算成功，请求失败不自动重试。接口与流语义见 spec/PLAN.md T-12 步骤 1。
package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// Protocol 是 Provider 的 HTTP API 协议。
type Protocol string

// 支持的 Provider 协议。OpenAI 与 OpenAICompatible 的请求与流格式相同，只差两个参数：
// OpenAI 官方推理模型只接受 max_completion_tokens，许多兼容实现只认 max_tokens；
// stream_options 只发给 OpenAI，避免不认识它的兼容实现拒绝请求，兼容实现只在自行返回用量时才有用量。
const (
	OpenAI                Protocol = "openai"
	OpenAICompatible      Protocol = "openai-compatible"
	AnthropicMessages     Protocol = "anthropic-messages"
	OpenAIResponses       Protocol = "openai-responses"
	GeminiGenerateContent Protocol = "gemini-generate-content"
)

const (
	openAIEndpoint    = "chat/completions"
	anthropicEndpoint = "messages"
	// anthropicVersion 是 Anthropic Messages API 要求的 anthropic-version 请求头。
	anthropicVersion = "2023-06-01"
	// openAIStreamDone 是 OpenAI 兼容流的终止标记。
	openAIStreamDone = "[DONE]"
	// redactedAPIKey 替换 Provider 错误说明中回显的 API key。
	redactedAPIKey = "[REDACTED]"
)

// Message 的角色。
const (
	roleSystem    = "system"
	roleUser      = "user"
	roleAssistant = "assistant"
)

// Config 是一个 Provider 的连接设置。
type Config struct {
	Protocol Protocol
	BaseURL  string // API 根路径，例如 https://api.openai.com/v1；请求路径追加在其后
	APIKey   string
	Model    string
}

// Message 是对话中的一条消息。
type Message struct {
	Role        string // system、user 或 assistant
	Content     string
	ToolCalls   []ToolCall   // 只出现在 assistant 消息中
	ToolResults []ToolResult // 只出现在 user 消息中
}

// Tool 是提供给模型的一个工具。
type Tool struct {
	Name        string // 同一 Request 中唯一
	Description string
	InputSchema json.RawMessage // JSON Schema，必须是 JSON 对象
}

// Request 是一次流式请求。
type Request struct {
	Messages  []Message
	Tools     []Tool
	MaxTokens int // 必须大于 0；由调用方显式给出，不猜缺省值
}

// ToolCall 是模型发起的一次工具调用。
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage // 完整的 JSON 对象
	Replay    json.RawMessage // 协议要求随这次调用原样回传的不透明 JSON，由流解析写入；调用方把整个 ToolCall 原样放回 assistant 消息，不解析。openai、openai-compatible 与 anthropic-messages 不产生也不读取它
}

// ToolResult 是一次工具调用的结果，回传给模型。
type ToolResult struct {
	ToolCallID string
	Content    string
}

// Event 是流中的一个事件：ToolCall 非 nil 时是一次组装完整的工具调用，否则 Text 是一段文本增量。
type Event struct {
	Text     string
	ToolCall *ToolCall
}

// StopReason 是模型结束生成的原因。两种协议的常见取值统一为下列常量，
// 其他取值（例如 content_filter、refusal）原样保留 Provider 给出的字符串。
type StopReason string

// 统一的停止原因。
const (
	StopEnd       StopReason = "end"        // 自然结束或命中停止序列
	StopToolUse   StopReason = "tool_use"   // 等待工具结果
	StopMaxTokens StopReason = "max_tokens" // 达到 Request.MaxTokens，输出被截断
)

// Usage 是一次请求的 token 用量。
type Usage struct {
	InputTokens  int // Anthropic 的缓存写入与缓存读取 token 计入其中
	OutputTokens int
}

// Result 是一次成功的流的结束信息。
type Result struct {
	StopReason StopReason // Provider 未给出时为空
	Usage      *Usage     // Provider 未返回用量时为 nil
}

// Client 向一个 Provider 发起流式请求。
type Client struct {
	config     Config
	httpClient *http.Client
	baseURL    *url.URL
}

// New 校验 config 并返回 Client。httpClient 不能为 nil，由调用方注入以便复用代理设置。
// BaseURL 必须是不含用户信息、查询串与片段的 HTTP(S) 地址。
func New(config Config, httpClient *http.Client) (*Client, error) {
	if config.Protocol != OpenAI && config.Protocol != OpenAICompatible && config.Protocol != AnthropicMessages && config.Protocol != OpenAIResponses && config.Protocol != GeminiGenerateContent {
		return nil, fmt.Errorf("unsupported provider protocol %q", config.Protocol)
	}
	if config.Model == "" || config.APIKey == "" {
		return nil, errors.New("provider model and API key are required")
	}
	baseURL, err := url.Parse(config.BaseURL)
	if err != nil || baseURL.Host == "" || (baseURL.Scheme != "http" && baseURL.Scheme != "https") ||
		baseURL.User != nil || baseURL.RawQuery != "" || baseURL.Fragment != "" {
		// 不回显原始地址，避免把其中的凭证写进错误。
		return nil, errors.New("provider base URL must be HTTP(S) without credentials, query, or fragment")
	}
	return &Client{config: config, httpClient: httpClient, baseURL: baseURL}, nil
}

// Stream 发出 req，并把流中的事件依次交给 emit；流正常结束时返回停止原因与用量。
//
// emit 返回错误时立即停止读取并返回该错误。提前结束、无效事件、非 2xx 状态与 Provider
// 报告的错误都返回错误，Provider 给出的错误说明附在其后；已交给 emit 的文本增量由调用方保留，
// Stream 不重发请求。错误文本不含 API key 与认证头，但 Provider 的说明可能引用请求内容，
// 只适合写日志，不能原样放进 safe_message。
func (c *Client) Stream(ctx context.Context, req Request, emit func(Event) error) (Result, error) {
	// 1. 校验请求：消息与工具来自对话记录和模型输出，在发出前检查。
	if err := validateRequest(req); err != nil {
		return Result{}, err
	}

	// 2. 按协议编码请求体，拼出端点并设置认证头。
	anthropic := c.config.Protocol == AnthropicMessages
	endpoint, body := openAIEndpoint, openAIBody(c.config.Protocol, c.config.Model, req)
	if anthropic {
		endpoint, body = anthropicEndpoint, anthropicBody(c.config.Model, req)
	}
	if c.config.Protocol == OpenAIResponses {
		var err error
		endpoint = responsesEndpoint
		body, err = responsesBody(c.config.Model, req)
		if err != nil {
			return Result{}, err
		}
	}
	if c.config.Protocol == GeminiGenerateContent {
		var err error
		body, err = geminiBody(req)
		if err != nil {
			return Result{}, err
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return Result{}, fmt.Errorf("encode provider request: %w", err)
	}
	target := c.baseURL.JoinPath(endpoint)
	if c.config.Protocol == GeminiGenerateContent {
		target = c.geminiURL()
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(raw))
	if err != nil {
		return Result{}, fmt.Errorf("build provider request: %w", err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "text/event-stream")
	if anthropic {
		httpRequest.Header.Set("x-api-key", c.config.APIKey)
		httpRequest.Header.Set("anthropic-version", anthropicVersion)
	} else if c.config.Protocol == GeminiGenerateContent {
		httpRequest.Header.Set("x-goog-api-key", c.config.APIKey)
	} else {
		httpRequest.Header.Set("Authorization", "Bearer "+c.config.APIKey)
	}

	// 3. 发出请求；非 2xx 时报告状态码与 Provider 的错误说明（其中回显的 API key 已替换）。
	response, err := c.httpClient.Do(httpRequest)
	if err != nil {
		if c.config.Protocol == GeminiGenerateContent {
			err = c.geminiRequestError(err)
		}
		return Result{}, fmt.Errorf("send provider request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var errorBody struct {
			Error json.RawMessage `json:"error"`
		}
		// 响应体不是 JSON 时没有可附加的说明，只报告状态码。
		_ = json.NewDecoder(response.Body).Decode(&errorBody)
		return Result{}, fmt.Errorf("provider returned HTTP status %d%s", response.StatusCode, c.describeError(errorBody.Error))
	}

	// 4. 逐事件解析响应流。
	if anthropic {
		return c.streamAnthropic(response.Body, emit)
	}
	if c.config.Protocol == OpenAIResponses {
		return c.streamResponses(response.Body, emit)
	}
	if c.config.Protocol == GeminiGenerateContent {
		return c.streamGemini(response.Body, emit)
	}
	return c.streamOpenAI(response.Body, emit)
}

// describeError 从 Provider 的错误字段（{"message": ...} 对象或字符串）取出说明，
// 替换其中回显的 API key 后以 ": 说明" 的形式返回；取不到说明时返回空串。
func (c *Client) describeError(raw json.RawMessage) string {
	var message string
	if json.Unmarshal(raw, &message) != nil {
		var object struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &object) // 不是对象时没有说明
		message = object.Message
	}
	if message == "" {
		return ""
	}
	return ": " + strings.ReplaceAll(message, c.config.APIKey, redactedAPIKey)
}

func validateRequest(req Request) error {
	if req.MaxTokens <= 0 || len(req.Messages) == 0 {
		return errors.New("messages and positive max tokens are required")
	}
	for _, m := range req.Messages {
		if m.Role != roleSystem && m.Role != roleUser && m.Role != roleAssistant {
			return fmt.Errorf("invalid message role %q", m.Role)
		}
		if len(m.ToolCalls) > 0 && m.Role != roleAssistant {
			return errors.New("tool calls require assistant role")
		}
		if len(m.ToolResults) > 0 && m.Role != roleUser {
			return errors.New("tool results require user role")
		}
		for _, result := range m.ToolResults {
			if result.ToolCallID == "" {
				return errors.New("tool result call ID is required")
			}
		}
	}
	names := make(map[string]bool, len(req.Tools))
	for _, tool := range req.Tools {
		if tool.Name == "" || names[tool.Name] {
			return errors.New("tool names must be non-empty and unique")
		}
		names[tool.Name] = true
		if !isJSONObject(tool.InputSchema) {
			return fmt.Errorf("input schema of tool %q must be a JSON object", tool.Name)
		}
	}
	return nil
}

func isJSONObject(raw []byte) bool {
	var object map[string]json.RawMessage
	return json.Unmarshal(raw, &object) == nil && object != nil
}

// openAIBody 编码 Chat Completions 请求：每个工具结果单独成为一条 tool 角色消息，
// 同一条消息的文本随后作为 user 消息。
func openAIBody(protocol Protocol, model string, req Request) map[string]any {
	messages := []map[string]any{}
	for _, m := range req.Messages {
		for _, result := range m.ToolResults {
			messages = append(messages, map[string]any{"role": "tool", "tool_call_id": result.ToolCallID, "content": result.Content})
		}
		if len(m.ToolResults) > 0 && m.Content == "" {
			continue
		}
		message := map[string]any{"role": m.Role, "content": m.Content}
		if len(m.ToolCalls) > 0 {
			calls := []map[string]any{}
			for _, call := range m.ToolCalls {
				calls = append(calls, map[string]any{
					"id":       call.ID,
					"type":     "function",
					"function": map[string]any{"name": call.Name, "arguments": string(call.Arguments)},
				})
			}
			message["tool_calls"] = calls
		}
		messages = append(messages, message)
	}
	maxTokensField := "max_tokens"
	if protocol == OpenAI {
		maxTokensField = "max_completion_tokens"
	}
	body := map[string]any{"model": model, "messages": messages, maxTokensField: req.MaxTokens, "stream": true}
	if protocol == OpenAI {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	if len(req.Tools) > 0 {
		tools := []map[string]any{}
		for _, tool := range req.Tools {
			tools = append(tools, map[string]any{
				"type":     "function",
				"function": map[string]any{"name": tool.Name, "description": tool.Description, "parameters": tool.InputSchema},
			})
		}
		body["tools"] = tools
	}
	return body
}

// anthropicBody 编码 Messages 请求：system 消息按顺序合并到顶层 system 字段，
// 带工具调用或工具结果的消息转成 content block，依次为 tool_result、text、tool_use，
// 满足 user 消息中工具结果在前、assistant 消息中文本在工具调用之前的要求。
func anthropicBody(model string, req Request) map[string]any {
	messages := []map[string]any{}
	systems := []string{}
	for _, m := range req.Messages {
		if m.Role == roleSystem {
			systems = append(systems, m.Content)
			continue
		}
		message := map[string]any{"role": m.Role, "content": m.Content}
		if len(m.ToolCalls) > 0 || len(m.ToolResults) > 0 {
			blocks := []map[string]any{}
			for _, result := range m.ToolResults {
				blocks = append(blocks, map[string]any{"type": "tool_result", "tool_use_id": result.ToolCallID, "content": result.Content})
			}
			if m.Content != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": m.Content})
			}
			for _, call := range m.ToolCalls {
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": call.ID, "name": call.Name, "input": call.Arguments})
			}
			message["content"] = blocks
		}
		messages = append(messages, message)
	}
	body := map[string]any{"model": model, "messages": messages, "max_tokens": req.MaxTokens, "stream": true}
	if len(systems) > 0 {
		body["system"] = strings.Join(systems, "\n\n")
	}
	if len(req.Tools) > 0 {
		tools := []map[string]any{}
		for _, tool := range req.Tools {
			tools = append(tools, map[string]any{"name": tool.Name, "description": tool.Description, "input_schema": tool.InputSchema})
		}
		body["tools"] = tools
	}
	return body
}

// readSSE 按 SSE 格式读取 r，每遇到一个空行就把累积的 event 名和 data 交给 handle。
// 流在两个事件之间结束时返回 nil，在事件中途结束时返回错误。
func readSSE(r io.Reader, handle func(event, data string) error) error {
	reader := bufio.NewReader(r)
	event := ""
	var data strings.Builder
	for {
		line, readErr := reader.ReadString('\n')
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch {
		case line == "" && readErr == nil:
			// 空行结束一个事件。
			if data.Len() > 0 {
				if err := handle(event, strings.TrimSuffix(data.String(), "\n")); err != nil {
					return err
				}
			}
			event = ""
			data.Reset()
		case field == "event":
			event = value
		case field == "data":
			data.WriteString(value)
			data.WriteByte('\n')
		}
		// 以 ":" 开头的注释行与未知字段按 SSE 规范忽略。
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return fmt.Errorf("read provider stream: %w", readErr)
			}
			if data.Len() > 0 {
				return errors.New("provider stream ended in the middle of an event")
			}
			return nil
		}
	}
}

// openAIChunk 是 OpenAI 兼容流中一个 data 事件的载荷。
type openAIChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"` // 同一工具调用的各段共用一个 index
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	// Usage 只在请求了 include_usage 的末尾事件中出现，此时 choices 为空。
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error json.RawMessage `json:"error"`
}

// streamOpenAI 解析 OpenAI 兼容流。工具调用的分段按 index 累积，
// 在收到 [DONE] 后按 index 顺序逐个校验并交出。
func (c *Client) streamOpenAI(r io.Reader, emit func(Event) error) (Result, error) {
	calls := map[int]*ToolCall{}
	done := false
	var result Result
	err := readSSE(r, func(_, data string) error {
		if data == openAIStreamDone {
			done = true
			return nil
		}
		var chunk openAIChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return fmt.Errorf("decode OpenAI-compatible stream event: %w", err)
		}
		if len(chunk.Error) > 0 {
			return fmt.Errorf("OpenAI-compatible provider reported an error%s", c.describeError(chunk.Error))
		}
		if chunk.Usage != nil {
			result.Usage = &Usage{InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens}
		}
		for _, choice := range chunk.Choices {
			if choice.FinishReason != "" {
				result.StopReason = openAIStopReason(choice.FinishReason)
			}
			if choice.Delta.Content != "" {
				if err := emit(Event{Text: choice.Delta.Content}); err != nil {
					return err
				}
			}
			for _, part := range choice.Delta.ToolCalls {
				call := calls[part.Index]
				if call == nil {
					call = &ToolCall{}
					calls[part.Index] = call
				}
				// id 与 name 按规范只在首个分段出现；有的兼容实现在每个分段重复它们，所以取值而不拼接。
				if part.ID != "" {
					call.ID = part.ID
				}
				if part.Function.Name != "" {
					call.Name = part.Function.Name
				}
				call.Arguments = append(call.Arguments, part.Function.Arguments...)
			}
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	if !done {
		return Result{}, errors.New("OpenAI-compatible stream ended without [DONE]")
	}
	for _, index := range slices.Sorted(maps.Keys(calls)) {
		call := calls[index]
		if call.ID == "" || call.Name == "" || !isJSONObject(call.Arguments) {
			return Result{}, errors.New("invalid tool call in OpenAI-compatible stream")
		}
		if err := emit(Event{ToolCall: call}); err != nil {
			return Result{}, err
		}
	}
	return result, nil
}

func openAIStopReason(reason string) StopReason {
	switch reason {
	case "stop":
		return StopEnd
	case "length":
		return StopMaxTokens
	case "tool_calls", "function_call":
		return StopToolUse
	}
	return StopReason(reason)
}

// anthropicEvent 是 Anthropic 流中一个 data 事件的载荷；不同 type 只使用其中部分字段。
type anthropicEvent struct {
	Type         string `json:"type"`
	Index        int    `json:"index"` // content block 的序号
	ContentBlock struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"` // 只出现在 message_delta 中
	} `json:"delta"`
	Message struct {
		Usage *anthropicUsage `json:"usage"`
	} `json:"message"` // 只出现在 message_start 中
	Usage *anthropicUsage `json:"usage"` // message_delta 中的累计输出用量
	Error json.RawMessage `json:"error"`
}

// anthropicUsage 是 Anthropic 报告的用量；input_tokens 不含缓存写入与缓存读取的 token。
type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// streamAnthropic 解析 Anthropic Messages 流。tool_use block 在 content_block_stop 时交出。
// Anthropic 会新增事件类型与 block 类型，未知的按其版本策略忽略。
func (c *Client) streamAnthropic(r io.Reader, emit func(Event) error) (Result, error) {
	toolUses := map[int]*ToolCall{} // 未结束的 tool_use block，按 block 序号索引
	stopped := false
	var result Result
	err := readSSE(r, func(name, data string) error {
		var event anthropicEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return fmt.Errorf("decode Anthropic stream event: %w", err)
		}
		if name == "error" || event.Type == "error" {
			return fmt.Errorf("Anthropic provider reported an error%s", c.describeError(event.Error))
		}
		switch event.Type {
		case "message_start":
			if usage := event.Message.Usage; usage != nil {
				result.Usage = &Usage{
					InputTokens:  usage.InputTokens + usage.CacheCreationInputTokens + usage.CacheReadInputTokens,
					OutputTokens: usage.OutputTokens,
				}
			}
		case "message_delta":
			if event.Delta.StopReason != "" {
				result.StopReason = anthropicStopReason(event.Delta.StopReason)
			}
			if event.Usage != nil {
				if result.Usage == nil {
					result.Usage = &Usage{}
				}
				result.Usage.OutputTokens = event.Usage.OutputTokens
			}
		case "content_block_start":
			if event.ContentBlock.Type == "tool_use" {
				toolUses[event.Index] = &ToolCall{ID: event.ContentBlock.ID, Name: event.ContentBlock.Name}
			}
		case "content_block_delta":
			switch event.Delta.Type {
			case "text_delta":
				if event.Delta.Text != "" {
					return emit(Event{Text: event.Delta.Text})
				}
			case "input_json_delta":
				call := toolUses[event.Index]
				if call == nil {
					return errors.New("Anthropic tool input delta has no matching tool_use block")
				}
				call.Arguments = append(call.Arguments, event.Delta.PartialJSON...)
			}
		case "content_block_stop":
			call := toolUses[event.Index]
			if call == nil {
				return nil
			}
			delete(toolUses, event.Index)
			if call.ID == "" || call.Name == "" || !isJSONObject(call.Arguments) {
				return errors.New("invalid tool call in Anthropic stream")
			}
			return emit(Event{ToolCall: call})
		case "message_stop":
			stopped = true
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	if !stopped {
		return Result{}, errors.New("Anthropic stream ended without message_stop")
	}
	if len(toolUses) > 0 {
		return Result{}, errors.New("Anthropic stream has unfinished tool_use blocks")
	}
	return result, nil
}

func anthropicStopReason(reason string) StopReason {
	switch reason {
	case "end_turn", "stop_sequence":
		return StopEnd
	case "max_tokens":
		return StopMaxTokens
	case "tool_use":
		return StopToolUse
	}
	return StopReason(reason)
}
