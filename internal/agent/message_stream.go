package agent

import (
	"context"
	"errors"
	"io"
	"iter"
	"strings"

	"github.com/AlonaBot/AlonaBot/sdk/errcode"
	"github.com/AlonaBot/AlonaBot/sdk/message"
)

func (r *messageRound) streamConversation(run func(func(conversationDelta) error) error) error {
	// Pull 暂停在 flush 上；下一次拉取发生在当前 Stream 完成投递之后。
	var providerError error
	next, stop := iter.Pull(func(yield func(conversationDelta) bool) {
		providerError = run(func(delta conversationDelta) error {
			if !yield(delta) {
				return context.Canceled
			}
			return nil
		})
	})
	defer stop()
	for {
		first, ok := next()
		if !ok {
			return providerError
		}
		if first.text == "" {
			continue
		}
		var text strings.Builder
		pending := first.text
		results, err := r.context.Stream(func(context.Context) (string, error) {
			if pending != "" {
				delta := pending
				pending = ""
				text.WriteString(delta)
				return delta, nil
			}
			for {
				delta, ok := next()
				if !ok {
					if providerError != nil && !errors.Is(providerError, errToolLimit) {
						return "", errcode.Unavailable.New("provider conversation failed")
					}
					return "", io.EOF
				}
				if delta.flush {
					return "", io.EOF
				}
				if delta.text != "" {
					text.WriteString(delta.text)
					return delta.text, nil
				}
			}
		})
		complete := err == nil
		for _, result := range results {
			complete = complete && result.Status == message.SendAccepted
		}
		if complete {
			r.history = append(r.history, conversationEntry{Role: roleAssistant, Content: text.String()})
		} else if len(results) > 0 {
			// 编辑或分块失败时保留已发事实，并明确生成文本的投递未全部确认。
			var receipts []messageReceipt
			for _, result := range results {
				receipts = append(receipts, sendReceipt(result, nil))
			}
			content := "[body delivery incomplete]\n" + text.String() + "\n" + messageJSON(receipts)
			if err != nil {
				content += "\n" + err.Error()
			}
			r.history = append(r.history, conversationEntry{Role: roleAssistant, Content: content})
		}
		if err != nil {
			return errors.Join(providerError, err)
		}
	}
}

func (r *messageRound) textLimit() *message.TextLimit {
	for _, format := range r.description.Formats {
		if format.Name == "text" {
			return format.TextLimit
		}
	}
	return nil
}
