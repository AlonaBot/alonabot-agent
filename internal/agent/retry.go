package agent

import (
	"context"
	"log/slog"
	"time"

	"github.com/AlonaBot/alonabot-agent/provider"
)

const providerRetryInterval = time.Second

// conversationStream 返回实际作答的模型配置名与该次请求的结果。
type conversationStream func(provider.Request, func(provider.Event) error) (string, provider.Result, error)

// retryStream 为工具循环的每次请求重新尝试模型链；工具结果由循环持有。
func (s *configuration) retryStream(ctx context.Context, logger *slog.Logger, selected *configuredModel, prepare func(*configuredModel, provider.Request) provider.Request) conversationStream {
	models := []*configuredModel{selected}
	seen := map[string]bool{selected.name: true}
	for _, name := range s.fallbackChatModels {
		if !seen[name] {
			models = append(models, s.model(name))
			seen[name] = true
		}
	}
	return func(request provider.Request, emit func(provider.Event) error) (string, provider.Result, error) {
		var lastError error
		for _, model := range models {
			for attempt := int64(1); ; attempt++ {
				if lastError != nil {
					timer := time.NewTimer(providerRetryInterval)
					select {
					case <-ctx.Done():
						timer.Stop()
						return "", provider.Result{}, ctx.Err()
					case <-timer.C:
					}
				}
				if err := ctx.Err(); err != nil {
					return "", provider.Result{}, err
				}
				current := request
				if prepare != nil {
					current = prepare(model, request)
				}
				if err := ctx.Err(); err != nil {
					return "", provider.Result{}, err
				}
				current.MaxTokens = model.maxTokens
				delivered := false
				result, err := model.client.Stream(ctx, current, func(event provider.Event) error {
					if err := ctx.Err(); err != nil {
						return err
					}
					delivered = delivered || event.Text != "" || event.ToolCall != nil
					return emit(event)
				})
				if err == nil {
					return model.name, result, nil
				}
				logger.WarnContext(ctx, "provider conversation request failed", "model", model.name, "attempt", attempt, "error", err)
				if ctxErr := ctx.Err(); ctxErr != nil {
					return "", provider.Result{}, ctxErr
				}
				if delivered || !provider.Retryable(err) {
					return "", provider.Result{}, err
				}
				lastError = err
				if attempt == s.maxAttempts {
					break
				}
			}
		}
		return "", provider.Result{}, lastError
	}
}
