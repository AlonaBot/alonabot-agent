package provider

import (
	"crypto/rand"
	"errors"
)

func newToolCallID() string {
	return rand.Text()
}

// toolCallName 从结果消息之前的历史中查找最近一次匹配的 assistant 调用。
func toolCallName(messages []Message, id string) (string, error) {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != roleAssistant {
			continue
		}
		for _, call := range messages[i].ToolCalls {
			if call.ID == id {
				return call.Name, nil
			}
		}
	}
	return "", errors.New("tool result has no matching assistant call")
}
