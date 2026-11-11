package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const responsesEndpoint = "responses"

func responsesBody(model string, req Request) (map[string]any, error) {
	// 1. 展开完整历史，让推理项紧邻它所属的工具调用。
	input := []any{}
	systems := []string{}
	for _, message := range req.Messages {
		if message.Role == roleSystem {
			systems = append(systems, message.Content)
			continue
		}
		for _, result := range message.ToolResults {
			input = append(input, map[string]any{"type": "function_call_output", "call_id": result.ToolCallID, "output": result.Content})
		}
		if message.Content != "" {
			input = append(input, map[string]any{"role": message.Role, "content": message.Content})
		}
		for _, call := range message.ToolCalls {
			if len(call.Replay) > 0 {
				if bytes.HasPrefix(bytes.TrimSpace(call.Replay), []byte("[")) {
					var items []json.RawMessage
					if err := json.Unmarshal(call.Replay, &items); err != nil {
						return nil, fmt.Errorf("decode Responses replay: %w", err)
					}
					for _, item := range items {
						input = append(input, item)
					}
				} else {
					input = append(input, call.Replay)
				}
			}
			input = append(input, map[string]any{
				"type": "function_call", "call_id": call.ID, "name": call.Name, "arguments": string(call.Arguments),
			})
		}
	}

	// 2. 明确禁用服务端存储和工具严格模式，保留现有工具的 Schema。
	body := map[string]any{
		"model": model, "input": input, "stream": true, "store": false,
		"max_output_tokens": req.MaxTokens, "include": []string{"reasoning.encrypted_content"},
	}
	if len(systems) > 0 {
		body["instructions"] = strings.Join(systems, "\n\n")
	}
	if len(req.Tools) > 0 {
		tools := []map[string]any{}
		for _, tool := range req.Tools {
			tools = append(tools, map[string]any{
				"type": "function", "name": tool.Name, "description": tool.Description,
				"parameters": tool.InputSchema, "strict": false,
			})
		}
		body["tools"] = tools
	}
	return body, nil
}

type responsesEvent struct {
	Type     string          `json:"type"`
	Delta    string          `json:"delta"`
	Item     json.RawMessage `json:"item"`
	Message  json.RawMessage `json:"message"`
	Response struct {
		IncompleteDetails struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
		Error json.RawMessage `json:"error"`
	} `json:"response"`
}

func (c *Client) streamResponses(r io.Reader, emit func(Event) error) (Result, error) {
	var result Result
	var reasoning []json.RawMessage
	hasToolCall := false
	// 收到终止事件立即结束 readSSE，后续字节无需读取或解析。
	finished := errors.New("Responses stream finished")
	err := readSSE(r, func(_, data string) error {
		var kind struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(data), &kind); err != nil {
			return fmt.Errorf("decode Responses stream event: %w", err)
		}
		switch kind.Type {
		case "response.output_text.delta", "response.output_item.done", "response.completed", "response.incomplete", "response.failed", "error":
		default:
			return nil
		}
		var event responsesEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return fmt.Errorf("decode Responses stream event: %w", err)
		}
		switch event.Type {
		case "response.output_text.delta":
			if event.Delta != "" {
				return emit(Event{Text: event.Delta})
			}
		case "response.output_item.done":
			var item struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(event.Item, &item); err != nil {
				return fmt.Errorf("decode Responses output item: %w", err)
			}
			switch item.Type {
			case "reasoning":
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(event.Item, &fields); err != nil {
					return fmt.Errorf("decode Responses reasoning item: %w", err)
				}
				delete(fields, "id")
				raw, err := json.Marshal(fields)
				if err != nil {
					return fmt.Errorf("encode Responses reasoning replay: %w", err)
				}
				reasoning = append(reasoning, raw)
			case "function_call":
				var function struct {
					CallID    string `json:"call_id"`
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}
				if err := json.Unmarshal(event.Item, &function); err != nil {
					return fmt.Errorf("decode Responses function call: %w", err)
				}
				call := &ToolCall{ID: function.CallID, Name: function.Name, Arguments: json.RawMessage(function.Arguments)}
				if call.ID == "" || call.Name == "" || !isJSONObject(call.Arguments) {
					return errors.New("invalid tool call in Responses stream")
				}
				if len(reasoning) > 0 {
					var err error
					call.Replay, err = json.Marshal(reasoning)
					if err != nil {
						return fmt.Errorf("encode Responses tool replay: %w", err)
					}
					reasoning = nil
				}
				if err := emit(Event{ToolCall: call}); err != nil {
					return err
				}
				hasToolCall = true
			}
		case "response.completed", "response.incomplete":
			result.StopReason = StopEnd
			if hasToolCall {
				result.StopReason = StopToolUse
			}
			if event.Type == "response.incomplete" {
				result.StopReason = StopReason(event.Response.IncompleteDetails.Reason)
				if result.StopReason == "max_output_tokens" {
					result.StopReason = StopMaxTokens
				}
			}
			if usage := event.Response.Usage; usage != nil {
				result.Usage = &Usage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens}
			}
			return finished
		case "response.failed":
			return fmt.Errorf("Responses provider reported a failure%s", c.describeError(event.Response.Error))
		case "error":
			return fmt.Errorf("Responses provider reported an error%s", c.describeError(event.Message))
		}
		return nil
	})
	if err == finished {
		return result, nil
	}
	if err != nil {
		return Result{}, err
	}
	return Result{}, errors.New("Responses stream ended without a terminal event")
}
