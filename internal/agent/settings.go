package agent

import (
	"context"
	"errors"
	"hash/crc32"
	"sort"
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
	ResetGeneration uint64 // reset 前开始的轮次按此代次跳过历史追加。
	ImageCount      uint64 `gorm:"not null;default:0"` // 成功保存的最大图片编号，reset 时归零。
}

// TableName 固定受管数据库中会话运行状态的表名。
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
		return chatSettings{Session: session}, nil
	}
	return settings, nil
}

func (a *component) updateSettingsFor(c *sdk.Context, session string, update func(*chatSettings)) error {
	db, err := gormdb.Open(c)
	if err != nil {
		return err
	}
	return db.WithContext(c).Transaction(func(tx *gorm.DB) error {
		settings, err := a.settings(c, session, tx)
		if err != nil {
			return err
		}
		update(&settings)
		return tx.Save(&settings).Error
	})
}

func (a *component) registerCommands(p *sdk.Plugin) {
	p.Command("agent", "on").Permission(sdk.Admin).Description("Enable free conversation in the current profile").Handle(func(c *sdk.Context) error {
		c.StopPropagation()
		return setProfile(c, "triggers", []string{"mention", "wake_word", "private"})
	})
	p.Command("agent", "off").Permission(sdk.Admin).Description("Disable free conversation in the current profile").Handle(func(c *sdk.Context) error {
		c.StopPropagation()
		return setProfile(c, "triggers", []string{})
	})
	p.Command("persona").Args("name").Optional("name").Permission(sdk.Admin).Description("List personas or select one for the current profile").Menu().Handle(a.selectPersona)
	p.Command("provider").Args("name").Optional("name").Permission(sdk.Admin).Description("List chat models or select one for the current profile").Menu().Handle(a.selectModel)
	p.Command("profile").Args("name").Optional("name").Permission(sdk.Admin).Description("Show or select a configuration profile").Menu().Handle(a.selectProfile)
	p.Command("reset").Permission(sdk.User).Description("Clear this conversation").Menu().Handle(func(c *sdk.Context) error {
		c.StopPropagation()
		if err := a.resetConversation(c); err != nil {
			return err
		}
		return c.Reply(c.T("agent.reset"))
	})
}

func (a *component) resetConversation(c *sdk.Context) error {
	session := conversationKey(c)
	lock := a.sessionLock(session)
	lock.Lock()
	defer lock.Unlock()
	// 与轮次最终的代次核对和追加共用锁，清空后旧轮次只能跳过追加。
	if err := a.updateSettingsFor(c, session, func(s *chatSettings) { s.ResetGeneration++ }); err != nil {
		return err
	}
	if err := conversationapi.Clear(c, session); err != nil {
		return err
	}
	db, err := gormdb.Open(c)
	if err != nil {
		return err
	}
	return db.WithContext(c).Transaction(func(tx *gorm.DB) error {
		if err := tx.Delete(&conversationRecord{Key: session}).Error; err != nil {
			return err
		}
		if err := tx.Where("session = ?", session).Delete(&conversationImage{}).Error; err != nil {
			return err
		}
		return tx.Model(&chatSettings{}).Where("session = ?", session).Update("image_count", 0).Error
	})
}

func (a *component) selectPersona(c *sdk.Context, name string) error {
	c.StopPropagation()
	db, err := gormdb.Open(c)
	if err != nil {
		return err
	}
	if name == "" {
		var rows []persona
		if err := db.WithContext(c).Find(&rows).Error; err != nil {
			return err
		}
		names := make([]string, len(rows))
		for i, row := range rows {
			names[i] = row.Name
		}
		sort.Strings(names)
		return c.Reply(c.T("agent.personas", "names", strings.Join(names, ", ")))
	}
	if _, err := a.readPersona(c, name); errors.Is(err, gorm.ErrRecordNotFound) {
		return c.Reply(c.T("agent.unknown_persona", "name", name))
	} else if err != nil {
		return err
	}
	return setProfile(c, "default_persona", name)
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
	return setProfile(c, "chat_model", name)
}
