package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AlonaBot/AlonaBot/sdk/errcode"
	"github.com/AlonaBot/alonabot-agent/provider"
	"gorm.io/gorm"
)

const toolStepLimit = 20 // 按单轮实际提交执行的 ToolCall 计数，同一响应的每次调用各占一步。

var (
	errToolLimit  = errors.New("tool call limit reached")
	errModelUsage = errcode.Internal.New("could not record model usage")
)

// conversationDelta 将正文与发送边界交给外层；flush 返回后才执行工具。
type conversationDelta struct {
	text  string
	flush bool
}

func runToolLoop(ctx context.Context, db *gorm.DB, request provider.Request, catalog toolCatalog, stream conversationStream, emit func(conversationDelta) error) error {
	request.Tools = catalog.definitions
	steps := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var text strings.Builder
		var calls []provider.ToolCall
		model, result, err := stream(request, func(event provider.Event) error {
			if event.Text != "" {
				text.WriteString(event.Text)
				if err := emit(conversationDelta{text: event.Text}); err != nil {
					return err
				}
			}
			if event.ToolCall != nil {
				calls = append(calls, *event.ToolCall)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if err := recordUsage(ctx, db, model, time.Now(), result.Usage); err != nil {
			return fmt.Errorf("%w: %w", errModelUsage, err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(calls) == 0 {
			return nil
		}
		// 保留完整 ToolCall，由目标协议决定是否回传私有 Replay。
		request.Messages = append(request.Messages, provider.Message{Role: roleAssistant, Content: text.String(), ToolCalls: calls})
		results := make([]provider.ToolResult, 0, len(calls))
		for _, call := range calls {
			if err := ctx.Err(); err != nil {
				return err
			}
			if steps == toolStepLimit {
				return errToolLimit
			}
			var content string
			tool, exists := catalog.byName[call.Name]
			if !exists {
				content = toolResult("", errcode.NotFound.New("unknown tool"))
			} else {
				steps++
				if err := emit(conversationDelta{flush: call.Name == sendMessageToolName}); err != nil {
					return err
				}
				content = tool.invoke(ctx, call.Arguments)
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			results = append(results, provider.ToolResult{ToolCallID: call.ID, Content: content})
		}
		request.Messages = append(request.Messages, provider.Message{Role: roleUser, ToolResults: results})
	}
}
