package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/openai/openai-go/v3"
	openaioption "github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

func responsesParams(model string, req Request) (responses.ResponseNewParams, error) {
	// 1. 完整历史中，推理项紧邻它所属的工具调用。
	input := responses.ResponseInputParam{}
	var systems []string
	for _, message := range req.Messages {
		if message.Role == roleSystem {
			systems = append(systems, message.Content)
			continue
		}
		for _, result := range message.ToolResults {
			input = append(input, responses.ResponseInputItemUnionParam{
				OfFunctionCallOutput: &responses.ResponseInputItemFunctionCallOutputParam{
					CallID: openai.String(result.ToolCallID),
					Output: responses.ResponseInputItemFunctionCallOutputOutputUnionParam{OfString: openai.String(result.Content)},
				},
			})
		}
		if len(message.Images) > 0 {
			var parts []responses.ResponseInputContentUnionParam
			for _, image := range message.Images {
				parts = append(parts, responses.ResponseInputContentUnionParam{OfInputImage: &responses.ResponseInputImageParam{
					ImageURL: openai.String("data:" + image.MediaType + ";base64," + base64.StdEncoding.EncodeToString(image.Data)),
				}})
			}
			if message.Content != "" {
				parts = append(parts, responses.ResponseInputContentUnionParam{OfInputText: &responses.ResponseInputTextParam{Text: message.Content}})
			}
			input = append(input, responses.ResponseInputItemUnionParam{OfMessage: &responses.EasyInputMessageParam{
				Role:    responses.EasyInputMessageRole(message.Role),
				Content: responses.EasyInputMessageContentUnionParam{OfInputItemContentList: parts},
			}})
		} else if message.Content != "" {
			input = append(input, responses.ResponseInputItemUnionParam{OfMessage: &responses.EasyInputMessageParam{
				Role:    responses.EasyInputMessageRole(message.Role),
				Content: responses.EasyInputMessageContentUnionParam{OfString: openai.String(message.Content)},
			}})
		}
		for _, call := range message.ToolCalls {
			if call.replayProtocol == OpenAIResponses {
				var items []json.RawMessage
				if bytes.HasPrefix(bytes.TrimSpace(call.Replay), []byte("[")) {
					if err := json.Unmarshal(call.Replay, &items); err != nil {
						return responses.ResponseNewParams{}, fmt.Errorf("decode Responses replay: %w", err)
					}
				} else {
					items = []json.RawMessage{call.Replay}
				}
				for _, item := range items {
					input = append(input, param.Override[responses.ResponseInputItemUnionParam](item))
				}
			}
			input = append(input, responses.ResponseInputItemUnionParam{OfFunctionCall: &responses.ResponseFunctionToolCallParam{
				CallID: call.ID, Name: call.Name, Arguments: string(call.Arguments),
			}})
		}
	}
	// 2. 明确禁用服务端存储和工具严格模式，原样发送 Schema。
	params := responses.ResponseNewParams{
		Model: shared.ResponsesModel(model), Store: openai.Bool(false),
		MaxOutputTokens: openai.Int(int64(req.MaxTokens)),
		Include:         []responses.ResponseIncludable{"reasoning.encrypted_content"},
		Input:           responses.ResponseNewParamsInputUnion{OfInputItemList: input},
	}
	if len(systems) > 0 {
		params.Instructions = openai.String(strings.Join(systems, "\n\n"))
	}
	for _, tool := range req.Tools {
		function := responses.FunctionToolParam{Name: tool.Name, Description: openai.String(tool.Description), Strict: openai.Bool(false)}
		function.SetExtraFields(map[string]any{"parameters": tool.InputSchema})
		params.Tools = append(params.Tools, responses.ToolUnionParam{OfFunction: &function})
	}
	return params, nil
}

func (c *Client) streamResponses(ctx context.Context, req Request, emit func(Event) error) (Result, error) {
	var result Result
	var reasoning []json.RawMessage
	hasToolCall := false
	params, err := responsesParams(c.config.Model, req)
	if err != nil {
		return Result{}, err
	}
	var response *http.Response
	stream := c.responses.NewStreaming(ctx, params, openaioption.WithResponseInto(&response))
	defer stream.Close()
	if response != nil && (response.StatusCode < 200 || response.StatusCode >= 300) {
		return Result{}, c.sdkError(stream.Err(), response)
	}
	for stream.Next() {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		event := stream.Current()
		if !sdkFieldsValid[string](event.JSON.Type) {
			return Result{}, errors.New("invalid field type in Responses stream")
		}

		switch event.Type {
		case "response.output_text.delta":
			if !sdkFieldsValid[string](event.JSON.Delta) {
				return Result{}, errors.New("invalid field type in Responses stream")
			}

			if event.Delta != "" {
				if err := emit(Event{Text: event.Delta}); err != nil {
					return Result{}, err
				}
			}
		case "response.output_item.done":
			if !sdkFieldsValid[struct{}](event.JSON.Item) || !sdkFieldsValid[string](event.Item.JSON.Type) {
				return Result{}, errors.New("invalid field type in Responses stream")
			}

			switch event.Item.Type {
			case "reasoning":
				var fields map[string]json.RawMessage
				if err := json.Unmarshal([]byte(event.Item.RawJSON()), &fields); err != nil {
					return Result{}, fmt.Errorf("decode Responses reasoning item: %w", err)
				}
				delete(fields, "id")
				raw, err := json.Marshal(fields)
				if err != nil {
					return Result{}, fmt.Errorf("encode Responses reasoning replay: %w", err)
				}
				reasoning = append(reasoning, raw)
			case "function_call":
				item := event.Item.AsFunctionCall()
				if !sdkFieldsValid[string](item.JSON.CallID, item.JSON.Name, item.JSON.Arguments) {
					return Result{}, errors.New("invalid field type in Responses stream")
				}
				call := &ToolCall{ID: item.CallID, Name: item.Name, Arguments: json.RawMessage(item.Arguments)}
				if call.ID == "" || call.Name == "" || !isJSONObject(call.Arguments) {
					return Result{}, errors.New("invalid tool call in Responses stream")
				}
				if len(reasoning) > 0 {
					var err error
					call.Replay, err = json.Marshal(reasoning)
					if err != nil {
						return Result{}, fmt.Errorf("encode Responses tool replay: %w", err)
					}
					call.replayProtocol = OpenAIResponses
					reasoning = nil
				}
				if err := emit(Event{ToolCall: call}); err != nil {
					return Result{}, err
				}
				hasToolCall = true
			}
		case "response.completed", "response.incomplete":
			if !sdkFieldsValid[struct{}](event.JSON.Response, event.Response.JSON.Usage) {
				return Result{}, errors.New("invalid field type in Responses stream")
			}

			result.StopReason = StopEnd
			if hasToolCall {
				result.StopReason = StopToolUse
			}
			if event.Type == "response.incomplete" {
				if !sdkFieldsValid[struct{}](event.Response.JSON.IncompleteDetails) || !sdkFieldsValid[string](event.Response.IncompleteDetails.JSON.Reason) {
					return Result{}, errors.New("invalid field type in Responses stream")
				}
				result.StopReason = StopReason(event.Response.IncompleteDetails.Reason)
				if result.StopReason == "max_output_tokens" {
					result.StopReason = StopMaxTokens
				}
			}
			if usage := event.Response.Usage; event.Response.JSON.Usage.Valid() {
				if !sdkFieldsValid[int64](usage.JSON.InputTokens, usage.JSON.OutputTokens) {
					return Result{}, errors.New("invalid field type in Responses stream")
				}
				result.Usage = &Usage{InputTokens: int(usage.InputTokens), OutputTokens: int(usage.OutputTokens)}
			}
			return result, nil
		case "response.failed":
			if !sdkFieldsValid[struct{}](event.JSON.Response, event.Response.JSON.Error) || !sdkFieldsValid[string](event.Response.Error.JSON.Message) {
				return Result{}, errors.New("invalid field type in Responses stream")
			}

			return Result{}, fmt.Errorf("Responses provider reported a failure%s", c.errorMessage(event.Response.Error.Message))
		case "error":
			if !sdkFieldsValid[string](event.JSON.Message) {
				return Result{}, errors.New("invalid field type in Responses stream")
			}

			return Result{}, fmt.Errorf("Responses provider reported an error%s", c.errorMessage(event.Message))
		}
	}
	if err := stream.Err(); err != nil {
		return Result{}, c.sdkError(err, response)
	}
	return Result{}, errors.New("Responses stream ended without a terminal event")
}
