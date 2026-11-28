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

	"github.com/AlonaBot/AlonaBot/sdk"
	conversationapi "github.com/AlonaBot/AlonaBot/sdk/conversation"
	"github.com/AlonaBot/AlonaBot/sdk/errcode"
	"github.com/AlonaBot/AlonaBot/sdk/gormdb"
	"github.com/AlonaBot/AlonaBot/sdk/message"
	"github.com/AlonaBot/alonabot-agent/provider"
)

const (
	roleUser      = "user"
	roleAssistant = "assistant"
	roleSystem    = "system"
)

type conversationEntry struct {
	Role       string   `json:"role"`
	Kind       string   `json:"kind,omitempty"`
	Speaker    string   `json:"speaker,omitempty"` // 群聊 user 条目的平台外部用户 ID。
	Content    string   `json:"content"`
	Images     []uint64 `json:"images,omitempty"`
	ImagePaths []string `json:"image_paths,omitempty"` // 与 Images 同序的节点路径；普通顶层图片省略。
	seq        uint64   // 读取时保留 Core 序号，校验图片所属条目；不写入 JSON。
}

func (a *component) registerConversation(p *sdk.Plugin) {
	p.Command("ask").Args("text").Permission(sdk.User).Description("Ask the agent").Menu().Handle(func(c *sdk.Context, chain message.Chain) error {
		c.StopPropagation()
		return c.Continue(func(c *sdk.Context) error { return a.converse(c, chain) })
	})
	p.On(sdk.MessageCreated).Filter(sdk.MessageFilter{TriggersConfig: "triggers", WakeWordsConfig: "wake_words"}).Rule(sdk.NotCommand()).Handle(func(c *sdk.Context) error {
		chain := c.Event().Payload.(*message.CreatedPayload).Chain
		return c.Continue(func(c *sdk.Context) error { return a.converse(c, chain) })
	})
}

func (a *component) converse(c *sdk.Context, chain message.Chain) error {
	// 1. 在延续调用内读取当前选择与最近的历史，按原序组装请求。
	db, err := gormdb.Open(c)
	if err != nil {
		return memoryError(c, err)
	}
	session := conversationKey(c)
	lock := a.sessionLock(session)
	// 等 reset 的代次递增和历史清空一起完成，再取得新轮次的代次。
	lock.Lock()
	settings, err := a.settings(c, session, db)
	if err != nil {
		lock.Unlock()
		return err
	}
	selected := a.config.model(c.Config().String("chat_model"))
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
	images, err := storedImages(c, db, session, settings.ImageCount)
	lock.Unlock()
	if err != nil {
		return err
	}
	input, err := readConversationInput(c, chain)
	if err != nil {
		c.Logger().ErrorContext(c, "message content read failed", "error", err)
		return errors.Join(err, c.Reply(c.T("agent.input_error", "error", err.Error())))
	}
	incoming := input.images
	imagesToSave := incoming
	plainText := input.text.String()
	attached := make([]bool, len(incoming))
	for index := range attached {
		attached[index] = true
	}
	var prefix []provider.Message
	prompt, err := a.personaPrompt(c, c.Config().String("default_persona"))
	if err != nil {
		return err
	}
	prefix = append(prefix, provider.Message{Role: roleSystem, Content: prompt})
	skills, _ := a.skillSnapshotFor(c)
	if len(skills) > 0 {
		prefix = append(prefix, provider.Message{Role: roleSystem, Content: skillPrompt(skills, c.T("agent.skills.header"))})
	}
	captionName := c.Config().String("image_caption_model")
	captionModel := a.config.model(captionName)
	if captionName != "" && (captionModel == nil || captionModel.modelType != modelChat || !captionModel.imageInput) {
		return fmt.Errorf("$.image_caption_model: must name a configured chat model with image_input enabled")
	}
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
			current.Content = imageLegend(plainText, input.imagePaths, attached)
		case attemptUsedCaption:
			// 转述每轮只发一次；带图片的回退模型仍读取原图。
			if captioned == nil {
				captioned = new(a.captionImages(c, db, captionModel, plainText, incoming, input.imagePaths))
			}
			current.Content = *captioned
		default:
			current.Content = imageLegend(prependImages(plainText, len(imagesToSave)), input.imagePaths, make([]bool, len(incoming)))
		}
		request.Messages = append(append(messages, current), request.Messages...)
		return request
	}

	catalog, err := a.listTools(c, skills)
	if err != nil {
		if c.Err() != nil {
			return c.Err()
		}
		c.Logger().ErrorContext(c, "tool directory failed", "error", err)
		return c.Reply(c.T("agent.tools_error", "code", errcode.Unavailable.String()))
	}

	// 2. Pull 将 Provider 的推送回调转为 Stream 的拉取回调；stop 交还控制权并关闭响应体。
	var assistant strings.Builder
	var providerError error
	var toolLimitReached bool
	var usedCaption bool
	retry := a.config.retryStream(c, c.Logger(), selected, prepare)
	stream := func(request provider.Request, emit func(provider.Event) error) (string, provider.Result, error) {
		model, result, err := retry(request, emit)
		if err == nil && attemptUsedCaption {
			usedCaption = true
		}
		return model, result, err
	}
	next, stop := iter.Pull(func(yield func(string) bool) {
		providerError = runToolLoop(c, db, provider.Request{}, catalog, stream, func(text string) error {
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
		if c.Err() != nil {
			return c.Err()
		}
		if providerError == nil {
			return err
		}
		if errors.Is(providerError, errModelUsage) {
			return providerError
		}
		c.Logger().ErrorContext(c, "provider conversation failed", "error", providerError)
		return c.Reply(c.T("agent.error", "code", errcode.Unavailable.String()))
	}
	if toolLimitReached {
		return c.Reply(c.T("agent.tool_limit"))
	}

	// 3. 生成和显示成功后，仅将仍属于当前 reset 代次的轮次原子追加。
	user := conversationEntry{Role: roleUser, Content: plainText, ImagePaths: input.imagePaths}
	if usedCaption {
		user.Content = *captioned
		imagesToSave = nil
		user.ImagePaths = nil
	}
	if c.ChatType() == message.ChatGroup && c.Event().Actor != nil {
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
