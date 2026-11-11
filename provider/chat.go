package provider

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"slices"

	"github.com/openai/openai-go/v3"
	openaioption "github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

func chatParams(protocol Protocol, model string, req Request) openai.ChatCompletionNewParams {
	params := openai.ChatCompletionNewParams{Model: shared.ChatModel(model)}
	for _, message := range req.Messages {
		for _, result := range message.ToolResults {
			params.Messages = append(params.Messages, openai.ToolMessage(result.Content, result.ToolCallID))
		}
		if len(message.ToolResults) > 0 && message.Content == "" {
			continue
		}
		var encoded openai.ChatCompletionMessageParamUnion
		switch message.Role {
		case roleSystem:
			encoded = openai.SystemMessage(message.Content)
		case roleUser:
			encoded = openai.UserMessage(message.Content)
		case roleAssistant:
			assistant := openai.ChatCompletionAssistantMessageParam{
				Content: openai.ChatCompletionAssistantMessageParamContentUnion{OfString: openai.String(message.Content)},
			}
			for _, call := range message.ToolCalls {
				assistant.ToolCalls = append(assistant.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{
					OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
						ID: call.ID,
						Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
							Name: call.Name, Arguments: string(call.Arguments),
						},
					},
				})
			}
			encoded.OfAssistant = &assistant
		}
		params.Messages = append(params.Messages, encoded)
	}
	if protocol == OpenAI {
		params.MaxCompletionTokens = openai.Int(int64(req.MaxTokens))
		params.StreamOptions = openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)}
	} else {
		params.MaxTokens = openai.Int(int64(req.MaxTokens))
	}
	for _, tool := range req.Tools {
		function := shared.FunctionDefinitionParam{Name: tool.Name, Description: openai.String(tool.Description)}
		// Schema 已在请求入口验证为对象，按 RawMessage 保留所有字段及数字原文。
		function.SetExtraFields(map[string]any{"parameters": tool.InputSchema})
		params.Tools = append(params.Tools, openai.ChatCompletionToolUnionParam{
			OfFunction: &openai.ChatCompletionFunctionToolParam{Function: function},
		})
	}
	return params
}

func (c *Client) streamOpenAI(ctx context.Context, req Request, emit func(Event) error) (Result, error) {
	// 1. 逐个读取 SDK chunk，自行按工具索引累积参数。
	var response *http.Response
	stream := c.chat.NewStreaming(ctx, chatParams(c.config.Protocol, c.config.Model, req), openaioption.WithResponseInto(&response))
	defer stream.Close()
	if response != nil && (response.StatusCode < 200 || response.StatusCode >= 300) {
		return Result{}, c.sdkError(stream.Err(), response)
	}
	calls := map[int64]*ToolCall{}
	done := false
	var result Result
	for stream.Next() {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		chunk := stream.Current()
		if !sdkFieldsValid[[]struct{}](chunk.JSON.Choices) || !sdkFieldsValid[struct{}](chunk.JSON.Usage) {
			return Result{}, errors.New("invalid field type in OpenAI-compatible stream")
		}

		if chunk.JSON.Usage.Valid() {
			if !sdkFieldsValid[int64](chunk.Usage.JSON.PromptTokens, chunk.Usage.JSON.CompletionTokens) {
				return Result{}, errors.New("invalid field type in OpenAI-compatible stream")
			}
			result.Usage = &Usage{InputTokens: int(chunk.Usage.PromptTokens), OutputTokens: int(chunk.Usage.CompletionTokens)}
		}
		for _, choice := range chunk.Choices {
			if !sdkFieldsValid[string](choice.JSON.FinishReason, choice.Delta.JSON.Content) || !sdkFieldsValid[struct{}](choice.JSON.Delta) || !sdkFieldsValid[[]struct{}](choice.Delta.JSON.ToolCalls) {
				return Result{}, errors.New("invalid field type in OpenAI-compatible stream")
			}

			if choice.FinishReason != "" {
				done = true
				result.StopReason = openAIStopReason(choice.FinishReason)
			}
			if choice.Delta.Content != "" {
				if err := emit(Event{Text: choice.Delta.Content}); err != nil {
					return Result{}, err
				}
			}
			for _, part := range choice.Delta.ToolCalls {
				if !sdkFieldsValid[int64](part.JSON.Index) || !sdkFieldsValid[string](part.JSON.ID, part.Function.JSON.Name, part.Function.JSON.Arguments) || !sdkFieldsValid[struct{}](part.JSON.Function) {
					return Result{}, errors.New("invalid field type in OpenAI-compatible stream")
				}

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
	}
	if err := stream.Err(); err != nil {
		return Result{}, c.sdkError(err, response)
	}
	if !done {
		return Result{}, errors.New("OpenAI-compatible stream ended without finish_reason")
	}
	// 2. 流无错误且收到 finish_reason 后，按索引交出完整工具调用。
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
