package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"slices"
	"strings"

	"github.com/AlonaBot/AlonaBot/agent/provider"
	"github.com/AlonaBot/AlonaBot/sdk"
	conversationapi "github.com/AlonaBot/AlonaBot/sdk/conversation"
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
	Role    string   `json:"role"`
	Kind    string   `json:"kind,omitempty"`
	Speaker string   `json:"speaker,omitempty"` // 群聊 user 条目的平台外部用户 ID。
	Content string   `json:"content"`
	Images  []uint64 `json:"images,omitempty"`
	seq     uint64   // 读取时保留 Core 序号，校验图片所属条目；不写入 JSON。
}

func (a *component) registerConversation(p *sdk.Plugin) {
	p.Command("ask").Args("text").Permission(sdk.User).Description("Ask the agent").Handle(func(c *sdk.Context, chain message.Chain) error {
		c.StopPropagation()
		return c.Continue(func(c *sdk.Context) error { return a.converse(c, chain.Text()) })
	})
	p.On("chat", sdk.MessageCreated).Rule(sdk.ToMe(), sdk.NotCommand()).Handle(func(c *sdk.Context) error {
		db, err := gormdb.Open(c.Context())
		if err != nil {
			return err
		}
		settings, err := a.settings(c.Context(), sessionKey(c), db)
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
		return memoryError(c, err)
	}
	session := sessionKey(c)
	lock := a.sessionLock(session)
	// 等 reset 的代次递增和历史清空一起完成，再取得新轮次的代次。
	lock.Lock()
	settings, err := a.settings(c.Context(), session, db)
	if err != nil {
		lock.Unlock()
		return err
	}
	selected := a.config.model(settings.Model)
	if selected == nil || selected.modelType != modelChat {
		lock.Unlock()
		return c.Reply(c.T("agent.no_model"))
	}
	// 快照初始化与 reset 共用会话锁；生成期间释放，由代次保护最终追加。
	history, profile, err := a.conversationMemory(c, db, session)
	if err != nil {
		lock.Unlock()
		return memoryError(c, err)
	}
	images, err := storedImages(c.Context(), db, session, settings.ImageCount)
	lock.Unlock()
	if err != nil {
		return err
	}
	incoming := readImages(c)
	var imagesToSave []provider.Image
	plainText := text
	for _, image := range incoming {
		if len(image.Data) > 0 {
			imagesToSave = append(imagesToSave, image)
		} else {
			plainText = prependImages(plainText, 1)
		}
	}
	var prefix []provider.Message
	if persona := a.config.persona(settings.Persona); persona != nil && persona.prompt != "" {
		prefix = append(prefix, provider.Message{Role: roleSystem, Content: persona.prompt})
	}
	if len(a.skills) > 0 {
		prefix = append(prefix, provider.Message{Role: roleSystem, Content: a.skillPrompt(c.T("agent.skills.header"))})
	}
	captionModel := a.config.model(a.config.imageCaptionModel)
	var captioned *string
	var attemptUsedCaption bool
	prepare := func(model *configuredModel, request provider.Request) provider.Request {
		messages := slices.Clone(prefix)
		for _, entry := range history {
			messages = append(messages, replayImages(entry, images, model.imageInput))
		}
		if profile != "" {
			messages = append(messages, provider.Message{Role: roleSystem, Content: profile})
		}
		current := provider.Message{Role: roleUser, Content: plainText}
		attemptUsedCaption = !model.imageInput && captionModel != nil && len(imagesToSave) > 0
		switch {
		case model.imageInput:
			current.Images = imagesToSave
		case attemptUsedCaption:
			// 转述每轮只发一次；带图片的回退模型仍读取原图。
			if captioned == nil {
				captioned = new(a.captionImages(c, db, captionModel, text, incoming))
			}
			current.Content = *captioned
		default:
			current.Content = prependImages(plainText, len(imagesToSave))
		}
		request.Messages = append(append(messages, current), request.Messages...)
		return request
	}

	catalog, err := a.listTools(c)
	if err != nil {
		if c.Context().Err() != nil {
			return c.Context().Err()
		}
		c.Logger().ErrorContext(c.Context(), "tool directory failed", "error", err)
		return c.Reply(c.T("agent.tools_error", "code", errcode.Unavailable.String()))
	}

	// 2. Pull 将 Provider 的推送回调转为 Stream 的拉取回调；stop 交还控制权并关闭响应体。
	var assistant strings.Builder
	var providerError error
	var toolLimitReached bool
	var usedCaption bool
	retry := a.config.retryStream(c.Context(), c.Logger(), selected, prepare)
	stream := func(request provider.Request, emit func(provider.Event) error) (string, provider.Result, error) {
		model, result, err := retry(request, emit)
		if err == nil && attemptUsedCaption {
			usedCaption = true
		}
		return model, result, err
	}
	next, stop := iter.Pull(func(yield func(string) bool) {
		providerError = runToolLoop(c.Context(), db, provider.Request{}, catalog, stream, func(text string) error {
			assistant.WriteString(text)
			if !yield(text) {
				return context.Canceled
			}
			return nil
		})
		if errors.Is(providerError, errToolLimit) {
			// 工具预算耗尽仍正常收尾，以发送无编辑能力实例的缓冲文本。
			toolLimitReached, providerError = true, nil
		}
	})
	defer stop()
	_, err = c.Stream(func(context.Context) (string, error) {
		if text, ok := next(); ok {
			return text, nil
		}
		if providerError != nil {
			// Provider 错误链可能含 io.EOF；SDK 只用独立信号判断流是否结束。
			return "", errcode.Unavailable.New("provider conversation failed")
		}
		return "", io.EOF
	})
	if err != nil {
		if c.Context().Err() != nil {
			return c.Context().Err()
		}
		if providerError == nil {
			return err
		}
		if errors.Is(providerError, errModelUsage) {
			return providerError
		}
		c.Logger().ErrorContext(c.Context(), "provider conversation failed", "error", providerError)
		return c.Reply(c.T("agent.error", "code", errcode.Unavailable.String()))
	}
	if toolLimitReached {
		return c.Reply(c.T("agent.tool_limit"))
	}

	// 3. 生成和显示成功后，仅将仍属于当前 reset 代次的轮次原子追加。
	user := conversationEntry{Role: roleUser, Content: plainText}
	if usedCaption {
		user.Content = *captioned
		imagesToSave = nil
	}
	if c.Event().ChatType == message.ChatGroup {
		user.Speaker = c.Event().Actor.ExternalUserID
	}
	assistantJSON, err := json.Marshal(conversationEntry{Role: roleAssistant, Content: assistant.String()})
	if err != nil {
		return fmt.Errorf("encode assistant conversation entry: %w", err)
	}
	lock.Lock()
	defer lock.Unlock()
	return a.appendImages(c, db, settings.ResetGeneration, user, assistantJSON, imagesToSave)
}

// recentConversation 独立保留冻结快照，count 仅限制普通历史条数。
func recentConversation(ctx context.Context, key string, count int) ([]conversationEntry, error) {
	var recent []conversationEntry
	var snapshot *conversationEntry
	var from uint64
	for {
		entries, next, more, err := conversationapi.List(ctx, key, from)
		if err != nil {
			return nil, err
		}
		for _, stored := range entries {
			var entry conversationEntry
			if err := json.Unmarshal(stored.Entry, &entry); err != nil {
				return nil, fmt.Errorf("decode conversation entry %d: %w", stored.Seq, err)
			}
			entry.seq = stored.Seq
			if entry.Kind == memorySnapshot {
				snapshot = &entry
			} else if count > 0 {
				recent = append(recent, entry)
				if len(recent) > count {
					recent = recent[len(recent)-count:]
				}
			}
		}
		if !more {
			break
		}
		from = next
	}
	if snapshot != nil {
		recent = append([]conversationEntry{*snapshot}, recent...)
	}
	return recent, nil
}
