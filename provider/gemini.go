package provider

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
)

func (c *Client) geminiURL() *url.URL {
	target := c.baseURL.JoinPath("models", url.PathEscape(c.config.Model)+":streamGenerateContent")
	target.RawQuery = "alt=sse"
	return target
}

// geminiTransportError 隐去请求地址和凭证，同时保留原始错误链供调用方判断取消等原因。
type geminiTransportError struct {
	cause   error
	message string
}

func (e *geminiTransportError) Error() string { return e.message }
func (e *geminiTransportError) Unwrap() error { return e.cause }

func (c *Client) geminiRequestError(err error) error {
	cause := err
	var urlError *url.Error
	if errors.As(err, &urlError) {
		err = urlError.Err // net/http 的包装会把完整 URL 写进 Error，说明取内部错误。
	}
	message := strings.ReplaceAll(err.Error(), c.geminiURL().String(), redactedAPIKey)
	message = strings.ReplaceAll(message, c.config.APIKey, redactedAPIKey)
	return &geminiTransportError{cause: cause, message: message}
}

func geminiBody(req Request) (map[string]any, error) {
	// 1. 保持历史顺序，将工具结果关联到此前的调用。
	contents := []map[string]any{}
	systems := []string{}
	for i, message := range req.Messages {
		if message.Role == roleSystem {
			systems = append(systems, message.Content)
			continue
		}
		parts := []map[string]any{}
		for _, result := range message.ToolResults {
			name, err := toolCallName(req.Messages[:i], result.ToolCallID)
			if err != nil {
				return nil, err
			}
			parts = append(parts, map[string]any{"functionResponse": map[string]any{
				"id": result.ToolCallID, "name": name, "response": map[string]any{"output": result.Content},
			}})
		}
		for _, image := range message.Images {
			parts = append(parts, map[string]any{"inlineData": map[string]any{
				"mimeType": image.MediaType, "data": base64.StdEncoding.EncodeToString(image.Data),
			}})
		}
		if message.Content != "" {
			parts = append(parts, map[string]any{"text": message.Content})
		}
		for _, call := range message.ToolCalls {
			part := map[string]any{"functionCall": map[string]any{"id": call.ID, "name": call.Name, "args": call.Arguments}}
			if len(call.Replay) > 0 {
				part["thoughtSignature"] = call.Replay
			}
			parts = append(parts, part)
		}
		role := message.Role
		if role == roleAssistant {
			role = "model"
		}
		contents = append(contents, map[string]any{"role": role, "parts": parts})
	}

	// 2. 加入独立的 system 指令、工具声明与调用方给出的输出上限。
	body := map[string]any{"contents": contents, "generationConfig": map[string]any{"maxOutputTokens": req.MaxTokens}}
	if len(systems) > 0 {
		body["systemInstruction"] = map[string]any{"parts": []map[string]any{{"text": strings.Join(systems, "\n\n")}}}
	}
	if len(req.Tools) > 0 {
		declarations := []map[string]any{}
		for _, tool := range req.Tools {
			declarations = append(declarations, map[string]any{
				"name": tool.Name, "description": tool.Description, "parametersJsonSchema": tool.InputSchema,
			})
		}
		body["tools"] = []map[string]any{{"functionDeclarations": declarations}}
	}
	return body, nil
}

// geminiChunk 对应一个 GenerateContentResponse，只消费首个候选与累计用量。
type geminiChunk struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text             string `json:"text"`
				Thought          bool   `json:"thought"`
				ThoughtSignature string `json:"thoughtSignature"`
				FunctionCall     *struct {
					ID   string          `json:"id"`
					Name string          `json:"name"`
					Args json.RawMessage `json:"args"`
				} `json:"functionCall"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata *struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		ThoughtsTokenCount   int `json:"thoughtsTokenCount"`
	} `json:"usageMetadata"`
	PromptFeedback struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
	Error json.RawMessage `json:"error"`
}

func (c *Client) streamGemini(r io.Reader, emit func(Event) error) (Result, error) {
	var result Result
	var finishReason string
	hasToolCall := false
	err := readSSE(r, func(_, data string) error {
		// 1. 读取事件级错误与用量；结束原因之后仍可能收到最终用量。
		var chunk geminiChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return fmt.Errorf("decode Gemini stream event: %w", err)
		}
		if len(chunk.Error) > 0 {
			return fmt.Errorf("Gemini provider reported an error%s", c.describeError(chunk.Error))
		}
		if usage := chunk.UsageMetadata; usage != nil {
			result.Usage = &Usage{InputTokens: usage.PromptTokenCount, OutputTokens: usage.CandidatesTokenCount + usage.ThoughtsTokenCount}
		}
		if len(chunk.Candidates) == 0 {
			if reason := chunk.PromptFeedback.BlockReason; reason != "" {
				raw, _ := json.Marshal(reason) // 字符串编码不会失败。
				return fmt.Errorf("Gemini provider blocked the prompt%s", c.describeError(raw))
			}
			return nil
		}

		// 2. 按首个候选的 part 顺序交出文本和完整调用，签名随对应调用一起保存。
		candidate := chunk.Candidates[0]
		for _, part := range candidate.Content.Parts {
			if part.Thought {
				continue
			}
			if part.Text != "" {
				if err := emit(Event{Text: part.Text}); err != nil {
					return err
				}
			}
			if function := part.FunctionCall; function != nil {
				call := &ToolCall{ID: function.ID, Name: function.Name, Arguments: function.Args}
				if len(call.Arguments) == 0 {
					call.Arguments = json.RawMessage(`{}`)
				}
				if call.Name == "" || !isJSONObject(call.Arguments) {
					return errors.New("invalid tool call in Gemini stream")
				}
				if call.ID == "" {
					call.ID = newToolCallID()
				}
				if part.ThoughtSignature != "" {
					call.Replay, _ = json.Marshal(part.ThoughtSignature) // 字符串编码不会失败。
				}
				if err := emit(Event{ToolCall: call}); err != nil {
					return err
				}
				hasToolCall = true
			}
		}
		if candidate.FinishReason != "" {
			finishReason = candidate.FinishReason
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	if finishReason == "" {
		return Result{}, errors.New("Gemini stream ended without finishReason")
	}
	// 3. 正常 EOF 后统一结束原因，只有完整读完流才算成功。
	switch finishReason {
	case "STOP":
		result.StopReason = StopEnd
		if hasToolCall {
			result.StopReason = StopToolUse
		}
	case "MAX_TOKENS":
		result.StopReason = StopMaxTokens
	default:
		result.StopReason = StopReason(finishReason)
	}
	return result, nil
}
