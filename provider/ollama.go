package provider

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	ollamaEndpoint = "api/chat"
	ollamaAccept   = "application/x-ndjson"
)

func ollamaBody(model string, req Request) (map[string]any, error) {
	// 1. 保持消息顺序，将工具结果关联到此前的调用名称。
	messages := []map[string]any{}
	for i, message := range req.Messages {
		for _, result := range message.ToolResults {
			name, err := toolCallName(req.Messages[:i], result.ToolCallID)
			if err != nil {
				return nil, err
			}
			messages = append(messages, map[string]any{"role": "tool", "content": result.Content, "tool_name": name})
		}
		if message.Role == roleUser && message.Content == "" {
			continue
		}
		encoded := map[string]any{"role": message.Role, "content": message.Content}
		if len(message.ToolCalls) > 0 {
			calls := []map[string]any{}
			for _, call := range message.ToolCalls {
				calls = append(calls, map[string]any{"function": map[string]any{"name": call.Name, "arguments": call.Arguments}})
			}
			encoded["tool_calls"] = calls
		}
		messages = append(messages, encoded)
	}

	// 2. 加入工具声明与调用方指定的输出上限。
	body := map[string]any{"model": model, "stream": true, "options": map[string]any{"num_predict": req.MaxTokens}, "messages": messages}
	if len(req.Tools) > 0 {
		tools := []map[string]any{}
		for _, tool := range req.Tools {
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
				"name": tool.Name, "description": tool.Description, "parameters": tool.InputSchema,
			}})
		}
		body["tools"] = tools
	}
	return body, nil
}

type ollamaChunk struct {
	Message struct {
		Content   string `json:"content"`
		ToolCalls []struct {
			Function struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	} `json:"message"`
	Done            bool            `json:"done"`
	DoneReason      string          `json:"done_reason"`
	PromptEvalCount int             `json:"prompt_eval_count"`
	EvalCount       *int            `json:"eval_count"` // 显式 0 也表示 Provider 返回了用量。
	Error           json.RawMessage `json:"error"`
}

func (c *Client) streamOllama(r io.Reader, emit func(Event) error) (Result, error) {
	reader := bufio.NewReader(r)
	toolUsed := false
	for {
		// 1. 按行读取；末行即使没有换行也参与解析。
		line, readErr := reader.ReadBytes('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return Result{}, fmt.Errorf("read Ollama stream: %w", readErr)
		}
		if len(bytes.TrimSpace(line)) > 0 {
			var chunk *ollamaChunk
			if err := json.Unmarshal(line, &chunk); err != nil {
				return Result{}, fmt.Errorf("decode Ollama stream line: %w", err)
			}
			if chunk == nil {
				return Result{}, errors.New("Ollama stream line must be a JSON object")
			}
			if len(chunk.Error) > 0 {
				return Result{}, fmt.Errorf("Ollama provider reported an error%s", c.describeOllamaError(chunk.Error))
			}

			// 2. 先交出本行内容，再处理终止标记，避免丢失终止行的输出。
			if chunk.Message.Content != "" {
				if err := emit(Event{Text: chunk.Message.Content}); err != nil {
					return Result{}, err
				}
			}
			for _, item := range chunk.Message.ToolCalls {
				function := item.Function
				if function.Name == "" || !isJSONObject(function.Arguments) {
					return Result{}, errors.New("invalid tool call in Ollama stream")
				}
				call := &ToolCall{ID: newToolCallID(), Name: function.Name, Arguments: function.Arguments}
				if err := emit(Event{ToolCall: call}); err != nil {
					return Result{}, err
				}
				toolUsed = true
			}
			if chunk.Done {
				result := Result{StopReason: StopReason(chunk.DoneReason)}
				switch chunk.DoneReason {
				case "stop":
					result.StopReason = StopEnd
					if toolUsed {
						result.StopReason = StopToolUse
					}
				case "length":
					result.StopReason = StopMaxTokens
				}
				if chunk.EvalCount != nil {
					result.Usage = &Usage{InputTokens: chunk.PromptEvalCount, OutputTokens: *chunk.EvalCount}
				}
				return result, nil
			}
		}
		if errors.Is(readErr, io.EOF) {
			return Result{}, errors.New("Ollama stream ended without done")
		}
	}
}

// describeOllamaError 在本地服务未配置 key 时保留说明，避免用空字符串执行脱敏替换。
func (c *Client) describeOllamaError(raw json.RawMessage) string {
	if c.config.APIKey != "" {
		return c.describeError(raw)
	}
	var message string
	if json.Unmarshal(raw, &message) != nil || message == "" {
		return ""
	}
	return ": " + message
}
