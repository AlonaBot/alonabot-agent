package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/v2"
	anthropicoption "github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/anthropics/anthropic-sdk-go/v2/packages/param"
)

func anthropicParams(model string, req Request) anthropic.MessageNewParams {
	params := anthropic.MessageNewParams{Model: anthropic.Model(model), MaxTokens: int64(req.MaxTokens)}
	var systems []string
	for _, message := range req.Messages {
		if message.Role == roleSystem {
			systems = append(systems, message.Content)
			continue
		}
		var blocks []anthropic.ContentBlockParamUnion
		for _, result := range message.ToolResults {
			block := anthropic.NewToolResultBlock(result.ToolCallID, result.Content, false)
			// 保留已有 tool_result 请求体，不额外发送 is_error。
			block.OfToolResult.IsError = param.Opt[bool]{}
			blocks = append(blocks, block)
		}
		if message.Content != "" || (len(message.ToolCalls) == 0 && len(message.ToolResults) == 0) {
			blocks = append(blocks, anthropic.NewTextBlock(message.Content))
		}
		for _, call := range message.ToolCalls {
			blocks = append(blocks, anthropic.NewToolUseBlock(call.ID, call.Arguments, call.Name))
		}
		encoded := anthropic.NewUserMessage(blocks...)
		if message.Role == roleAssistant {
			encoded = anthropic.NewAssistantMessage(blocks...)
		}
		if len(message.ToolCalls) == 0 && len(message.ToolResults) == 0 {
			// 纯文本历史沿用字符串 content；SDK 类型只提供 block 数组。
			if encoded.OfUser != nil {
				encoded.OfUser.SetExtraFields(map[string]any{"content": message.Content})
			} else {
				encoded.OfAssistant.SetExtraFields(map[string]any{"content": message.Content})
			}
		}
		params.Messages = append(params.Messages, encoded)
	}
	if len(systems) > 0 {
		// SDK system 类型只提供 block 数组，保留协议现有的合并字符串。
		params.SetExtraFields(map[string]any{"system": strings.Join(systems, "\n\n")})
	}
	for _, tool := range req.Tools {
		params.Tools = append(params.Tools, anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
			Name: tool.Name, Description: anthropic.String(tool.Description),
			InputSchema: param.Override[anthropic.ToolInputSchemaParam](json.RawMessage(tool.InputSchema)),
		}})
	}
	return params
}

func (c *Client) streamAnthropic(ctx context.Context, req Request, emit func(Event) error) (Result, error) {
	// 1. 按 content block 索引累积工具参数，结束 block 时交出。
	var response *http.Response
	stream := c.messages.NewStreaming(ctx, anthropicParams(c.config.Model, req), anthropicoption.WithResponseInto(&response))
	defer stream.Close()
	if response != nil && (response.StatusCode < 200 || response.StatusCode >= 300) {
		return Result{}, c.sdkError(stream.Err(), response)
	}
	toolUses := map[int64]*ToolCall{} // 未结束的 tool_use block，按 block 序号索引
	stopped := false
	var result Result
	for stream.Next() {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		event := stream.Current()
		if !sdkFieldsValid[string](event.JSON.Type) {
			return Result{}, errors.New("invalid field type in Anthropic stream")
		}

		switch event.Type {
		case "message_start":
			if !sdkFieldsValid[struct{}](event.JSON.Message, event.Message.JSON.Usage) {
				return Result{}, errors.New("invalid field type in Anthropic stream")
			}

			if usage := event.Message.Usage; event.Message.JSON.Usage.Valid() {
				if !sdkFieldsValid[int64](usage.JSON.InputTokens, usage.JSON.CacheCreationInputTokens, usage.JSON.CacheReadInputTokens, usage.JSON.OutputTokens) {
					return Result{}, errors.New("invalid field type in Anthropic stream")
				}
				result.Usage = &Usage{
					InputTokens:  int(usage.InputTokens + usage.CacheCreationInputTokens + usage.CacheReadInputTokens),
					OutputTokens: int(usage.OutputTokens),
				}
			}
		case "message_delta":
			if !sdkFieldsValid[struct{}](event.JSON.Delta, event.JSON.Usage) || !sdkFieldsValid[string](event.Delta.JSON.StopReason) {
				return Result{}, errors.New("invalid field type in Anthropic stream")
			}

			if event.Delta.StopReason != "" {
				result.StopReason = anthropicStopReason(string(event.Delta.StopReason))
			}
			if event.JSON.Usage.Valid() {
				if !sdkFieldsValid[int64](event.Usage.JSON.OutputTokens) {
					return Result{}, errors.New("invalid field type in Anthropic stream")
				}

				if result.Usage == nil {
					result.Usage = &Usage{}
				}
				result.Usage.OutputTokens = int(event.Usage.OutputTokens)
			}
		case "content_block_start":
			if !sdkFieldsValid[struct{}](event.JSON.ContentBlock) || !sdkFieldsValid[string](event.ContentBlock.JSON.Type) {
				return Result{}, errors.New("invalid field type in Anthropic stream")
			}

			if event.ContentBlock.Type == "tool_use" {
				if !sdkFieldsValid[int64](event.JSON.Index) || !sdkFieldsValid[string](event.ContentBlock.JSON.ID, event.ContentBlock.JSON.Name) {
					return Result{}, errors.New("invalid field type in Anthropic stream")
				}

				toolUses[event.Index] = &ToolCall{ID: event.ContentBlock.ID, Name: event.ContentBlock.Name}
			}
		case "content_block_delta":
			if !sdkFieldsValid[struct{}](event.JSON.Delta) || !sdkFieldsValid[string](event.Delta.JSON.Type) {
				return Result{}, errors.New("invalid field type in Anthropic stream")
			}

			switch event.Delta.Type {
			case "text_delta":
				if !sdkFieldsValid[string](event.Delta.JSON.Text) {
					return Result{}, errors.New("invalid field type in Anthropic stream")
				}

				if event.Delta.Text != "" {
					if err := emit(Event{Text: event.Delta.Text}); err != nil {
						return Result{}, err
					}
				}
			case "input_json_delta":
				if !sdkFieldsValid[int64](event.JSON.Index) || !sdkFieldsValid[string](event.Delta.JSON.PartialJSON) {
					return Result{}, errors.New("invalid field type in Anthropic stream")
				}

				call := toolUses[event.Index]
				if call == nil {
					return Result{}, errors.New("Anthropic tool input delta has no matching tool_use block")
				}
				call.Arguments = append(call.Arguments, event.Delta.PartialJSON...)
			}
		case "content_block_stop":
			if !sdkFieldsValid[int64](event.JSON.Index) {
				return Result{}, errors.New("invalid field type in Anthropic stream")
			}

			call := toolUses[event.Index]
			if call == nil {
				continue
			}
			delete(toolUses, event.Index)
			if call.ID == "" || call.Name == "" || !isJSONObject(call.Arguments) {
				return Result{}, errors.New("invalid tool call in Anthropic stream")
			}
			if err := emit(Event{ToolCall: call}); err != nil {
				return Result{}, err
			}
		case "message_stop":
			stopped = true
		}
		if stopped {
			break
		}
	}
	if err := stream.Err(); err != nil {
		return Result{}, c.sdkError(err, response)
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
