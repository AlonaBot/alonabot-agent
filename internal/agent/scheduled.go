package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/AlonaBot/AlonaBot/sdk"
	"github.com/AlonaBot/AlonaBot/sdk/conversation"
	"github.com/AlonaBot/AlonaBot/sdk/errcode"
	"github.com/AlonaBot/AlonaBot/sdk/gormdb"
	"github.com/AlonaBot/AlonaBot/sdk/message"
	"github.com/AlonaBot/alonabot-agent/provider"
)

func (a *component) scheduled(c *sdk.Context, task scheduledPrompt) error {
	key := c.Event().Payload.(*message.ScheduledPayload).TaskKey
	err := a.runScheduled(c, "task/"+key, task)
	if err != nil {
		if errcode.Of(err) == errcode.PermissionDenied {
			c.Logger().WarnContext(c, "scheduled agent task denied", "task_key", key, "error", err)
		} else {
			c.Logger().ErrorContext(c, "scheduled agent task failed", "task_key", key, "error", err)
		}
	}
	return err
}

func (a *component) runScheduled(c *sdk.Context, session string, task scheduledPrompt) error {
	// 1. 使用调度调用的配置视图，历史仅使用独立的任务会话；不读取记忆服务。
	db, err := gormdb.Open(c)
	if err != nil {
		return err
	}
	selected := a.config.model(c.Config().String("chat_model"))
	if selected == nil || selected.modelType != modelChat {
		return fmt.Errorf("scheduled task has no configured chat model")
	}
	history, err := recentConversation(c, session, a.config.historyEntries)
	if err != nil {
		return err
	}
	var request provider.Request
	prompt, err := a.personaPrompt(c, c.Config().String("default_persona"))
	if err != nil {
		return err
	}
	request.Messages = append(request.Messages, provider.Message{Role: roleSystem, Content: prompt})
	skills, _ := a.skillSnapshot()
	if len(skills) > 0 {
		request.Messages = append(request.Messages, provider.Message{Role: roleSystem, Content: skillPrompt(skills, c.T("agent.skills.header"))})
	}
	for _, entry := range history {
		request.Messages = append(request.Messages, provider.Message{Role: entry.Role, Content: entry.Content})
	}
	request.Messages = append(request.Messages, provider.Message{Role: roleUser, Content: task.Prompt})
	catalog, err := a.listTools(c, skills)
	if err != nil {
		return err
	}
	round, err := newMessageRound(c, task.Target, true, nil)
	if err != nil {
		return err
	}
	round.addTools(&catalog)
	// 2. 工具循环和发送继承 Core 的 job scope、期限与发信许可。
	var assistant strings.Builder
	flush := func() error {
		if assistant.Len() == 0 {
			return nil
		}
		var limit uint64
		var unit message.TextUnit
		if declared := round.textLimit(); declared != nil {
			limit, unit = declared.Max, declared.Unit
		}
		chunks, err := splitTaskText(assistant.String(), limit, unit)
		if err != nil {
			return err
		}
		for _, chunk := range chunks {
			result, err := sdk.Send(c, task.Target.session(), message.Text(chunk), sdk.Via(task.Target.Instance))
			if result != nil {
				content := chunk
				if err != nil || result.Status != message.SendAccepted {
					content = "[body delivery incomplete]\n" + chunk + "\n" + messageJSON(sendReceipt(result, err))
				}
				round.history = append(round.history, conversationEntry{Role: roleAssistant, Content: content})
			}
			if err != nil {
				return err
			}
		}
		assistant.Reset()
		return nil
	}
	stream := a.config.retryStream(c, c.Logger(), selected, nil)
	runError := runToolLoop(c, db, request, catalog, stream, func(delta conversationDelta) error {
		if delta.flush {
			return flush()
		}
		assistant.WriteString(delta.text)
		return nil
	})
	if runError == nil || errors.Is(runError, errToolLimit) {
		runError = errors.Join(runError, flush())
	}
	if len(round.history) == 0 {
		return runError
	}
	userJSON, err := json.Marshal(conversationEntry{Role: roleUser, Content: task.Prompt})
	if err != nil {
		return err
	}
	entries := []json.RawMessage{userJSON}
	for _, entry := range round.history {
		encoded, err := json.Marshal(entry)
		if err != nil {
			return errors.Join(runError, err)
		}
		entries = append(entries, encoded)
	}
	// 正文与目录登记共用会话锁，避免页面把尚未完成的登记当作空会话清走。
	lock := a.sessionLock(session)
	lock.Lock()
	defer lock.Unlock()
	// 与交互入口一样，取消后的已发送事实仍写入历史。
	ctx := context.WithoutCancel(c)
	if _, err := conversation.Append(ctx, session, entries...); err != nil {
		return errors.Join(runError, err)
	}
	return errors.Join(runError, db.WithContext(ctx).Save(&conversationRecord{Key: session}).Error)
}

// splitTaskText 沿用 Context.Stream 的计量方式，按当前出站描述在码点边界分段。
func splitTaskText(text string, limit uint64, unit message.TextUnit) ([]string, error) {
	if limit == 0 {
		return []string{text}, nil
	}
	var chunks []string
	start := 0
	var used uint64
	for offset, r := range text {
		units := uint64(1)
		switch unit {
		case message.TextUTF16CodeUnits:
			if r > 0xffff {
				units = 2
			}
		case message.TextBytes:
			_, width := utf8.DecodeRuneInString(text[offset:])
			units = uint64(width)
		}
		if units > limit {
			return nil, errcode.InvalidArgument.New("a code point exceeds the text limit")
		}
		if units > limit-used {
			chunks = append(chunks, text[start:offset])
			start, used = offset, 0
		}
		used += units
	}
	return append(chunks, text[start:]), nil
}
