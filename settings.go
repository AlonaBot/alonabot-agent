package agent

import (
	"context"
	"hash/crc32"
	"strings"
	"sync"

	"github.com/AlonaBot/AlonaBot/sdk"
	conversationapi "github.com/AlonaBot/AlonaBot/sdk/conversation"
	"github.com/AlonaBot/AlonaBot/sdk/gormdb"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const sessionLockCount = 64 // 固定分段锁，内存占用不随会话数量增长。

type chatSettings struct {
	Session         string `gorm:"primaryKey;autoIncrement:false"`
	Model           string
	Persona         string
	Enabled         bool
	ResetGeneration uint64 // reset 前开始的轮次按此代次跳过历史追加。
	ImageCount      uint64 `gorm:"not null;default:0"` // 成功保存的最大图片编号，reset 时归零。
}

// TableName 固定受管数据库中聊天设置的表名。
func (chatSettings) TableName() string { return "chat_settings" }

func (a *component) sessionLock(session string) *sync.Mutex {
	return &a.sessionLocks[crc32.ChecksumIEEE([]byte(session))%sessionLockCount]
}

func (a *component) settings(ctx context.Context, session string, db *gorm.DB) (chatSettings, error) {
	var settings chatSettings
	query := db.WithContext(ctx).Where(clause.Eq{Column: clause.PrimaryColumn, Value: session}).Limit(1).Find(&settings)
	if query.Error != nil {
		return settings, query.Error
	}
	if query.RowsAffected == 0 {
		return chatSettings{Session: session, Model: a.config.chatModel, Persona: a.config.defaultPersona}, nil
	}
	return settings, nil
}

func (a *component) updateSettings(c *sdk.Context, update func(*chatSettings)) error {
	db, err := gormdb.Open(c.Context())
	if err != nil {
		return err
	}
	return db.WithContext(c.Context()).Transaction(func(tx *gorm.DB) error {
		settings, err := a.settings(c.Context(), sessionKey(c), tx)
		if err != nil {
			return err
		}
		update(&settings)
		return tx.Save(&settings).Error
	})
}

func (a *component) registerCommands(p *sdk.Plugin) {
	p.Command("agent", "on").Permission(sdk.Admin).Description("Enable free conversation in this chat").Handle(func(c *sdk.Context) error {
		c.StopPropagation()
		if err := a.updateSettings(c, func(s *chatSettings) { s.Enabled = true }); err != nil {
			return err
		}
		return c.Reply(c.T("agent.enabled"))
	})
	p.Command("agent", "off").Permission(sdk.Admin).Description("Disable free conversation in this chat").Handle(func(c *sdk.Context) error {
		c.StopPropagation()
		if err := a.updateSettings(c, func(s *chatSettings) { s.Enabled = false }); err != nil {
			return err
		}
		return c.Reply(c.T("agent.disabled"))
	})
	p.Command("persona").Args("name").Optional("name").Permission(sdk.Admin).Description("List or select a persona").Handle(a.selectPersona)
	p.Command("provider").Args("name").Optional("name").Permission(sdk.Admin).Description("List or select a chat model").Handle(a.selectModel)
	p.Command("reset").Permission(sdk.User).Description("Clear this conversation").Handle(func(c *sdk.Context) error {
		c.StopPropagation()
		if err := a.resetConversation(c); err != nil {
			return err
		}
		return c.Reply(c.T("agent.reset"))
	})
}

func (a *component) resetConversation(c *sdk.Context) error {
	lock := a.sessionLock(sessionKey(c))
	lock.Lock()
	defer lock.Unlock()
	// 与轮次最终的代次核对和追加共用锁，清空后旧轮次只能跳过追加。
	if err := a.updateSettings(c, func(s *chatSettings) { s.ResetGeneration++ }); err != nil {
		return err
	}
	session := sessionKey(c)
	if err := conversationapi.Clear(c.Context(), session); err != nil {
		return err
	}
	db, err := gormdb.Open(c.Context())
	if err != nil {
		return err
	}
	return db.WithContext(c.Context()).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("session = ?", session).Delete(&conversationImage{}).Error; err != nil {
			return err
		}
		return tx.Model(&chatSettings{}).Where("session = ?", session).Update("image_count", 0).Error
	})
}

func (a *component) selectPersona(c *sdk.Context, name string) error {
	c.StopPropagation()
	if name == "" {
		names := make([]string, len(a.config.personas))
		for i, persona := range a.config.personas {
			names[i] = persona.name
		}
		return c.Reply(c.T("agent.personas", "names", strings.Join(names, ", ")))
	}
	if a.config.persona(name) == nil {
		return c.Reply(c.T("agent.unknown_persona", "name", name))
	}
	if err := a.updateSettings(c, func(s *chatSettings) { s.Persona = name }); err != nil {
		return err
	}
	return c.Reply(c.T("agent.persona_selected", "name", name))
}

func (a *component) selectModel(c *sdk.Context, name string) error {
	c.StopPropagation()
	if name == "" {
		var names []string
		for _, model := range a.config.models {
			if model.modelType == modelChat {
				names = append(names, model.name)
			}
		}
		return c.Reply(c.T("agent.models", "names", strings.Join(names, ", ")))
	}
	if selected := a.config.model(name); selected == nil || selected.modelType != modelChat {
		return c.Reply(c.T("agent.unknown_model", "name", name))
	}
	if err := a.updateSettings(c, func(s *chatSettings) { s.Model = name }); err != nil {
		return err
	}
	return c.Reply(c.T("agent.model_selected", "name", name))
}
