package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"strings"

	"github.com/AlonaBot/AlonaBot/agent/provider"
	"github.com/AlonaBot/AlonaBot/sdk"
	"github.com/AlonaBot/AlonaBot/sdk/errcode"
	"github.com/AlonaBot/AlonaBot/sdk/gormdb"
	"github.com/AlonaBot/AlonaBot/sdk/message"
)

const (
	roleUser      = "user"
	roleAssistant = "assistant"
	roleSystem    = "system"
)

type conversationEntry struct {
	Role    string `json:"role"`
	Speaker string `json:"speaker,omitempty"` // 群聊 user 条目的平台外部用户 ID。
	Content string `json:"content"`
}

func (a *component) registerConversation(p *sdk.Plugin) {
	p.Command("ask").Args("text").RawTail().Permission(sdk.User).Description("Ask the agent").Handle(func(c *sdk.Context, text string) error {
		c.StopPropagation()
		return c.Continue(func(c *sdk.Context) error { return a.converse(c, text) })
	})
	p.On("chat", sdk.MessageCreated).Rule(sdk.ToMe(), sdk.NotCommand()).Handle(func(c *sdk.Context) error {
		db, err := gormdb.Open(c.Context())
		if err != nil {
			return err
		}
		settings, err := a.settings(c.Context(), string(c.Session()), db)
		if err != nil || !settings.Enabled {
			return err
		}
		return c.Continue(func(c *sdk.Context) error { return a.converse(c, c.Text()) })
	})
}

func (a *component) converse(c *sdk.Context, text string) error {
	// 1. 在延续调用内读取当前选择与最近的历史，按原序组装请求。
	db, err := gormdb.Open(c.Context())
	if err != nil {
		return err
	}
	session := string(c.Session())
	lock := a.sessionLock(session)
	// 等 reset 的代次递增和历史清空一起完成，再取得新轮次的代次。
	lock.Lock()
	settings, err := a.settings(c.Context(), session, db)
	lock.Unlock()
	if err != nil {
		return err
	}
	selected := a.config.provider(settings.Provider)
	if selected == nil {
		return c.Reply(c.T("agent.no_provider"))
	}
	history, err := recentConversation(c.Context(), string(c.Session()), a.config.historyEntries)
	if err != nil {
		return err
	}
	request := provider.Request{MaxTokens: selected.maxTokens}
	if persona := a.config.persona(settings.Persona); persona != nil && persona.prompt != "" {
		request.Messages = append(request.Messages, provider.Message{Role: roleSystem, Content: persona.prompt})
	}
	for _, entry := range history {
		request.Messages = append(request.Messages, provider.Message{Role: entry.Role, Content: entry.Content})
	}
	request.Messages = append(request.Messages, provider.Message{Role: roleUser, Content: text})

	// 2. Pull2 将 Provider 的推送回调转为 Stream 的拉取回调；stop 交还控制权并关闭响应体。
	var assistant strings.Builder
	var providerError error
	next, stop := iter.Pull2(func(yield func(string, error) bool) {
		_, providerError = selected.client.Stream(c.Context(), request, func(event provider.Event) error {
			if event.Text != "" {
				assistant.WriteString(event.Text)
				if !yield(event.Text, nil) {
					return context.Canceled
				}
			}
			return nil
		})
		if providerError != nil {
			yield("", providerError)
			return
		}
		yield("", io.EOF)
	})
	defer stop()
	_, err = c.Stream(func(context.Context) (string, error) {
		text, err, _ := next()
		return text, err
	})
	if err != nil {
		if providerError == nil {
			return err
		}
		c.Logger().ErrorContext(c.Context(), "provider conversation failed", "error", providerError)
		return c.Reply(c.T("agent.error", "code", errcode.Unavailable.String()))
	}

	// 3. 生成和显示成功后，仅将仍属于当前 reset 代次的轮次原子追加。
	user := conversationEntry{Role: roleUser, Content: text}
	if c.Event().ChatType == message.ChatGroup {
		user.Speaker = c.Event().Actor.ExternalUserID
	}
	userJSON, err := json.Marshal(user)
	if err != nil {
		return fmt.Errorf("encode user conversation entry: %w", err)
	}
	assistantJSON, err := json.Marshal(conversationEntry{Role: roleAssistant, Content: assistant.String()})
	if err != nil {
		return fmt.Errorf("encode assistant conversation entry: %w", err)
	}
	lock.Lock()
	defer lock.Unlock()
	current, err := a.settings(c.Context(), session, db)
	if err != nil {
		return err
	}
	if current.ResetGeneration != settings.ResetGeneration {
		return nil
	}
	_, err = sdk.AppendConversation(c.Context(), session, userJSON, assistantJSON)
	return err
}

func recentConversation(ctx context.Context, key string, count int) ([]conversationEntry, error) {
	if count == 0 {
		return nil, nil
	}
	var recent []sdk.ConversationEntry
	var from uint64
	for {
		entries, next, more, err := sdk.ListConversation(ctx, key, from)
		if err != nil {
			return nil, err
		}
		recent = append(recent, entries...)
		if len(recent) > count {
			recent = recent[len(recent)-count:]
		}
		if !more {
			break
		}
		from = next
	}
	result := make([]conversationEntry, len(recent))
	for i, entry := range recent {
		if err := json.Unmarshal(entry.Entry, &result[i]); err != nil {
			return nil, fmt.Errorf("decode conversation entry %d: %w", entry.Seq, err)
		}
	}
	return result, nil
}
