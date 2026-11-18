// Package provider 调用 Agent 的模型服务（Provider），
// 支持 OpenAI Chat Completions（官方与兼容实现）、OpenAI Responses、Anthropic Messages、Gemini GenerateContent 与 Ollama Chat 协议。
//
// [Client.Stream] 逐个解析 SSE 事件或 NDJSON 行，按到达顺序把文本增量交给回调；
// 工具调用的参数完整且是 JSON 对象后才交出。流正常结束时返回停止原因与用量（[Result]）。
// Chat Completions 以 finish_reason 和无错误结束判定成功，其他协议要求终止事件。
// 另提供模型发现与五种预留模型的单次请求；请求失败不自动重试。
package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/v2"
	anthropicoption "github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/openai/openai-go/v3"
	openaioption "github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/openai/openai-go/v3/responses"
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
	OllamaChat            Protocol = "ollama-chat"
)

const (
	// anthropicVersion 是 Anthropic Messages API 要求的 anthropic-version 请求头。
	anthropicVersion = "2023-06-01"
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
	BaseURL  string // API 根路径；OpenAI 含 /v1，Anthropic 为服务根地址
	APIKey   string
	Model    string
}

// Image 携带一张内联图片的媒体类型与原始字节。
type Image struct {
	MediaType string // image/jpeg、image/png、image/gif 或 image/webp
	Data      []byte // 图片的原始字节，编码时转为 base64
}

// Message 保存对话中的一条消息。
type Message struct {
	Role        string // system、user 或 assistant
	Content     string
	ToolCalls   []ToolCall   // 只出现在 assistant 消息中
	ToolResults []ToolResult // 只出现在 user 消息中
	Images      []Image      // 只出现在 user 消息中
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

// StopReason 是模型结束生成的原因。各协议的常见取值统一为下列常量，
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

// Client 向一个 Provider 的指定模型发起请求。
type Client struct {
	config     Config
	httpClient *http.Client
	baseURL    *url.URL
	chat       openai.ChatCompletionService
	responses  responses.ResponseService
	messages   anthropic.MessageService
}

// New 校验 config 并返回 Client。httpClient 不能为 nil，由调用方注入以便复用代理设置。
// BaseURL 必须是不含用户信息、查询串与片段的 HTTP(S) 地址；OllamaChat 允许空 APIKey。
func New(config Config, httpClient *http.Client) (*Client, error) {
	if !supportedProtocol(config.Protocol) {
		return nil, fmt.Errorf("unsupported provider protocol %q", config.Protocol)
	}
	if config.Model == "" || config.APIKey == "" && config.Protocol != OllamaChat {
		return nil, errors.New("provider model and API key are required")
	}
	baseURL, err := connectionURL(config)
	if err != nil {
		return nil, err
	}
	// 只构造当前协议需要的 Service，后续请求复用其显式选项。
	c := &Client{config: config, httpClient: httpClient, baseURL: baseURL}
	switch config.Protocol {
	case OpenAI, OpenAICompatible, OpenAIResponses:
		options := []openaioption.RequestOption{
			openaioption.WithHTTPClient(httpClient), openaioption.WithBaseURL(config.BaseURL),
			openaioption.WithAPIKey(config.APIKey), openaioption.WithMaxRetries(0),
			openaioption.WithHeader("Accept", "text/event-stream"),
		}
		// Service 构造器不读取进程环境，配置只来自显式选项。
		if config.Protocol == OpenAIResponses {
			c.responses = responses.NewResponseService(options...)
		} else {
			c.chat = openai.NewChatCompletionService(options...)
		}
	case AnthropicMessages:
		c.messages = anthropic.NewClient(
			anthropicoption.WithoutEnvironmentDefaults(),
			anthropicoption.WithHTTPClient(httpClient), anthropicoption.WithBaseURL(config.BaseURL),
			anthropicoption.WithAPIKey(config.APIKey), anthropicoption.WithMaxRetries(0),
			// 锁定认证头与 API 版本，保留已有 Messages 请求契约。
			anthropicoption.WithHeaderDel("Authorization"),
			anthropicoption.WithHeader("x-api-key", config.APIKey),
			anthropicoption.WithHeader("anthropic-version", anthropicVersion),
			anthropicoption.WithHeader("Accept", "text/event-stream"),
		).Messages
	}
	return c, nil
}

func supportedProtocol(protocol Protocol) bool {
	switch protocol {
	case OpenAI, OpenAICompatible, AnthropicMessages, OpenAIResponses, GeminiGenerateContent, OllamaChat:
		return true
	default:
		return false
	}
}

func connectionURL(config Config) (*url.URL, error) {
	baseURL, err := url.Parse(config.BaseURL)
	if err != nil || baseURL.Host == "" || (baseURL.Scheme != "http" && baseURL.Scheme != "https") ||
		baseURL.User != nil || baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return nil, errors.New("provider base URL must be HTTP(S) without credentials, query, or fragment")
	}
	return baseURL, nil
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

	// 2. SDK 协议直接复用 New 构造的 Service。
	switch c.config.Protocol {
	case OpenAI, OpenAICompatible, OpenAIResponses, AnthropicMessages:
		originalEmit := emit
		emit = func(event Event) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := originalEmit(event); err != nil {
				return err
			}
			// SDK 可能已缓冲后续事件，回调取消后立即停止交付。
			return ctx.Err()
		}
	}
	switch c.config.Protocol {
	case OpenAI, OpenAICompatible:
		return c.streamOpenAI(ctx, req, emit)
	case OpenAIResponses:
		return c.streamResponses(ctx, req, emit)
	case AnthropicMessages:
		return c.streamAnthropic(ctx, req, emit)
	}

	// 3. Gemini 与 Ollama 沿用标准库请求和流解析。
	ollama := c.config.Protocol == OllamaChat
	var endpoint string
	var body map[string]any
	if c.config.Protocol == GeminiGenerateContent {
		var err error
		body, err = geminiBody(req)
		if err != nil {
			return Result{}, err
		}
	}
	if ollama {
		var err error
		endpoint = ollamaEndpoint
		body, err = ollamaBody(c.config.Model, req)
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
	if ollama {
		httpRequest.Header.Set("Accept", ollamaAccept)
	}
	if c.config.Protocol == GeminiGenerateContent {
		httpRequest.Header.Set("x-goog-api-key", c.config.APIKey)
	} else if c.config.APIKey != "" {
		httpRequest.Header.Set("Authorization", "Bearer "+c.config.APIKey)
	}

	// 4. 发出请求；非 2xx 时报告状态码与 Provider 的错误说明（其中回显的 API key 已替换）。
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
		var description string
		if ollama {
			description = c.describeOllamaError(errorBody.Error)
		} else {
			description = c.describeError(errorBody.Error)
		}
		return Result{}, fmt.Errorf("provider returned HTTP status %d%s", response.StatusCode, description)
	}

	// 5. 逐事件解析响应流。
	if c.config.Protocol == GeminiGenerateContent {
		return c.streamGemini(response.Body, emit)
	}
	return c.streamOllama(response.Body, emit)
}

// describeError 从 Provider 的错误字段（{"message": ...} 对象或字符串）取出说明，
// 替换其中回显的 API key 后以 ": 说明" 的形式返回；取不到说明时返回空串。
func (c *Client) describeError(raw json.RawMessage) string {
	message := providerErrorMessage(raw)
	if message == "" {
		return ""
	}
	return ": " + strings.ReplaceAll(message, c.config.APIKey, redactedAPIKey)
}

func providerErrorMessage(raw json.RawMessage) string {
	var message string
	if json.Unmarshal(raw, &message) != nil {
		var object struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &object) // 不是对象时没有说明
		message = object.Message
	}
	return message
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
		if len(m.Images) > 0 && m.Role != roleUser {
			return errors.New("images require user role")
		}
		for _, image := range m.Images {
			switch image.MediaType {
			case "image/jpeg", "image/png", "image/gif", "image/webp":
			default:
				return fmt.Errorf("unsupported image media type %q", image.MediaType)
			}
			if len(image.Data) == 0 {
				return errors.New("image data is required")
			}
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

// sdkError 保留 HTTP 状态与脱敏说明；流错误中的说明按协议报告。
func (c *Client) sdkError(err error, response *http.Response) error {
	var openAIError *openai.Error
	if errors.As(err, &openAIError) {
		return fmt.Errorf("provider returned HTTP status %d%s", openAIError.StatusCode, c.describeError(json.RawMessage(openAIError.RawJSON())))
	}
	var anthropicError *anthropic.Error
	if errors.As(err, &anthropicError) {
		var body struct {
			Error json.RawMessage `json:"error"`
		}
		_ = json.Unmarshal([]byte(anthropicError.RawJSON()), &body)
		if anthropicError.StatusCode < 200 || anthropicError.StatusCode >= 300 {
			return fmt.Errorf("provider returned HTTP status %d%s", anthropicError.StatusCode, c.describeError(body.Error))
		}
		return fmt.Errorf("Anthropic provider reported an error%s", c.describeError(body.Error))
	}
	if response != nil && (response.StatusCode < 200 || response.StatusCode >= 300) {
		// SDK 对字符串或非 JSON 错误体可能只返回解码错误，原始响应仍保留状态。
		var body struct {
			Error json.RawMessage `json:"error"`
		}
		_ = json.NewDecoder(response.Body).Decode(&body)
		return fmt.Errorf("provider returned HTTP status %d%s", response.StatusCode, c.describeError(body.Error))
	}
	var streamError *ssestream.StreamError
	if errors.As(err, &streamError) {
		var body struct {
			Error json.RawMessage `json:"error"`
		}
		if decodeErr := json.Unmarshal(streamError.Event.Data, &body); decodeErr != nil {
			return fmt.Errorf("decode provider error event: %w", decodeErr)
		}
		return fmt.Errorf("OpenAI-compatible provider reported an error%s", c.describeError(body.Error))
	}
	if strings.Contains(err.Error(), c.config.APIKey) {
		return errors.New(strings.ReplaceAll(err.Error(), c.config.APIKey, redactedAPIKey))
	}
	return fmt.Errorf("provider stream: %w", err)
}

func (c *Client) errorMessage(message string) string {
	if message == "" {
		return ""
	}
	return ": " + strings.ReplaceAll(message, c.config.APIKey, redactedAPIKey)
}

// sdkFieldsValid 核对 SDK 解码状态与原值类型；SDK 会把部分标量强转标为有效。
func sdkFieldsValid[T any](fields ...interface {
	Valid() bool
	Raw() string
}) bool {
	for _, field := range fields {
		raw := field.Raw()
		if raw == "" || raw == "null" {
			continue
		}
		var value T
		if !field.Valid() || json.Unmarshal([]byte(raw), &value) != nil {
			return false
		}
	}
	return true
}
