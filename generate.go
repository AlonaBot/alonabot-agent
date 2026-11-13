package agent

import (
	"context"
	"errors"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/AlonaBot/AlonaBot/agent/provider"
	"github.com/AlonaBot/AlonaBot/sdk"
	"github.com/AlonaBot/AlonaBot/sdk/errcode"
	"github.com/AlonaBot/AlonaBot/sdk/llm"
)

func (a *component) generate(c *sdk.Context, req llm.Request) (llm.Response, error) {
	// 1. 许可只取 Core 认证的直接调用方，在请求 Provider 前完成准入。
	caller := c.Caller()
	allowed := a.config.llmServiceAllowedPlugins
	if caller == "" || !slices.Contains(allowed, "*") && !slices.Contains(allowed, caller) {
		return llm.Response{}, errcode.PermissionDenied.New("text generation access denied")
	}
	if len(req.Messages) == 0 {
		return llm.Response{}, errcode.InvalidArgument.New("messages must not be empty")
	}
	for _, message := range req.Messages {
		if message.Role != roleUser && message.Role != roleAssistant {
			return llm.Response{}, errcode.InvalidArgument.New("message role must be user or assistant")
		}
	}
	if req.MaxOutputTokens < 0 {
		return llm.Response{}, errcode.InvalidArgument.New("max_output_tokens must be non-negative")
	}
	if len(a.config.providers) == 0 {
		return llm.Response{}, errcode.FailedPrecondition.New("no provider is configured")
	}
	name := req.Provider
	if name == "" {
		name = a.config.defaultProvider
	}
	selected := a.config.provider(name)
	if selected == nil {
		return llm.Response{}, errcode.InvalidArgument.New("unknown provider")
	}

	// 2. 独立组装文本请求，Provider 复用已有协议转换和流终止判断。
	request := provider.Request{MaxTokens: req.MaxOutputTokens}
	if request.MaxTokens == 0 {
		request.MaxTokens = selected.maxTokens
	}
	if req.System != "" {
		request.Messages = append(request.Messages, provider.Message{Role: roleSystem, Content: req.System})
	}
	for _, message := range req.Messages {
		request.Messages = append(request.Messages, provider.Message{Role: message.Role, Content: message.Text})
	}
	var text strings.Builder
	started := time.Now()
	result, err := selected.client.Stream(c.Context(), request, func(event provider.Event) error {
		text.WriteString(event.Text)
		return nil
	})
	fields := []any{"caller_plugin_id", caller, "provider", selected.name, "model", selected.model, "duration_ms", time.Since(started).Milliseconds()}
	if result.Usage != nil {
		fields = append(fields, "input_tokens", result.Usage.InputTokens, "output_tokens", result.Usage.OutputTokens)
	}
	if err != nil {
		c.Logger().DebugContext(c.Context(), "provider generation failed", append(fields, "error_kind", providerErrorKind(err))...)
		return llm.Response{}, errcode.Unavailable.New("provider generation failed")
	}
	c.Logger().DebugContext(c.Context(), "provider generation completed", fields...)

	// 3. nil 用量保持缺失，有用量时通过指针保留零值。
	response := llm.Response{Text: text.String(), Provider: selected.name, Model: selected.model}
	if result.Usage != nil {
		response.InputTokens = &result.Usage.InputTokens
		response.OutputTokens = &result.Usage.OutputTokens
	}
	return response, nil
}

// providerErrorKind 只按错误类型分类，Provider 的任意说明可能包含请求正文。
func providerErrorKind(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	var network net.Error
	if errors.As(err, &network) {
		if network.Timeout() {
			return "timeout"
		}
		return "network_error"
	}
	return "provider_error"
}
