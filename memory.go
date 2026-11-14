package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/AlonaBot/AlonaBot/sdk"
	"github.com/AlonaBot/AlonaBot/sdk/errcode"
	"github.com/AlonaBot/AlonaBot/sdk/gormdb"
	"github.com/AlonaBot/AlonaBot/sdk/message"
	"gorm.io/gorm"
)

const (
	memoryTool      = "alonabot_agent_official/memory"
	memoryTarget    = "memory"
	userTarget      = "user"
	memoryLimit     = 2200 // 会话笔记的 Unicode 码点上限，包含条目间的换行。
	userMemoryLimit = 1375 // 用户档案的 Unicode 码点上限，包含条目间的换行。
	memorySnapshot  = "memory_snapshot"
)

type memoryOperation struct {
	Op      string `json:"op"`     // add、patch、remove。
	Target  string `json:"target"` // memory、user。
	Text    string `json:"text,omitempty"`
	OldText string `json:"old_text,omitempty"`
}
type memoryRequest struct {
	Operations []memoryOperation `json:"operations"`
}
type memoryResponse struct{}

// memoryRecord 在同一行保存完整有序条目，使换行文本与码点校验使用相同的数据。
type memoryRecord struct {
	Target   string   `gorm:"primaryKey;autoIncrement:false"`
	Scope    string   `gorm:"primaryKey;autoIncrement:false"` // 会话键或 Actor 的外部用户 ID。
	Instance string   `gorm:"primaryKey;autoIncrement:false"`
	Entries  []string `gorm:"serializer:json;not null"`
}

// TableName 固定 Agent 受管数据库中记忆的表名。
func (memoryRecord) TableName() string { return "memory_record" }

func sessionKey(c *sdk.Context) string { return c.Session() + "@" + c.Instance() }

func memoryScope(c *sdk.Context, target string) (memoryRecord, error) {
	record := memoryRecord{Target: target, Instance: c.Instance()}
	switch target {
	case memoryTarget:
		if c.Session() == "" || record.Instance == "" {
			return record, errcode.FailedPrecondition.New("memory requires a default target")
		}
		record.Scope = sessionKey(c)
	case userTarget:
		_, user, err := message.ParseUID(c.User())
		if err != nil || record.Instance == "" {
			return record, errcode.FailedPrecondition.New("user memory requires an actor and instance")
		}
		record.Scope = user
	default:
		return record, errcode.InvalidArgument.New("unknown memory target")
	}
	return record, nil
}

func readMemory(db *gorm.DB, scope memoryRecord) (memoryRecord, error) {
	err := db.Where(&scope).Limit(1).Find(&scope).Error
	return scope, err
}

func registerMemory(p *sdk.Plugin) {
	sdk.Serve(p, sdk.JSON[memoryRequest, memoryResponse](memoryTool)).
		Access(sdk.AccessPrivate).
		Tool("Update chat notes or the current user's profile with ordered add, patch, or remove operations. patch and remove require old_text matching exactly one entry. The whole group is atomic.", sdk.ToolAllow).
		Handle(func(c *sdk.Context, in memoryRequest) (memoryResponse, error) {
			if len(in.Operations) == 0 {
				return memoryResponse{}, errcode.InvalidArgument.New("memory operations must not be empty")
			}
			scopes := make(map[string]memoryRecord)
			for _, operation := range in.Operations {
				scope, err := memoryScope(c, operation.Target)
				if err != nil {
					return memoryResponse{}, err
				}
				scopes[operation.Target] = scope
			}
			db, err := gormdb.Open(c.Context())
			if err != nil {
				return memoryResponse{}, err
			}
			return memoryResponse{}, updateMemory(db.WithContext(c.Context()), scopes, in.Operations)
		})
}

func updateMemory(db *gorm.DB, scopes map[string]memoryRecord, operations []memoryOperation) error {
	// 1. 所有操作只改事务内的副本，混合笔记和档案也共用同一次提交。
	return db.Transaction(func(tx *gorm.DB) error {
		records := make(map[string]memoryRecord, len(scopes))
		for target, scope := range scopes {
			record, err := readMemory(tx, scope)
			if err != nil {
				return err
			}
			records[target] = record
		}
		for _, operation := range operations {
			record := records[operation.Target]
			switch operation.Op {
			case "add":
				if operation.Text == "" {
					return errcode.InvalidArgument.New("memory text must not be empty")
				}
				record.Entries = append(record.Entries, operation.Text)
			case "patch", "remove":
				if operation.OldText == "" {
					return errcode.InvalidArgument.New("old_text must not be empty")
				}
				found := -1
				for index, text := range record.Entries {
					if strings.Contains(text, operation.OldText) {
						if found != -1 {
							return errcode.InvalidArgument.New("old_text matches multiple entries")
						}
						found = index
					}
				}
				if found == -1 {
					return errcode.InvalidArgument.New("old_text does not match an entry")
				}
				if operation.Op == "patch" {
					if operation.Text == "" {
						return errcode.InvalidArgument.New("memory text must not be empty")
					}
					record.Entries[found] = operation.Text
				} else {
					record.Entries = append(record.Entries[:found], record.Entries[found+1:]...)
				}
			default:
				return errcode.InvalidArgument.New("unknown memory operation")
			}
			records[operation.Target] = record
		}
		// 2. 检查最终保存和注入的完整文本，全部通过后才开始写库。
		for target, record := range records {
			limit := memoryLimit
			if target == userTarget {
				limit = userMemoryLimit
			}
			if utf8.RuneCountInString(strings.Join(record.Entries, "\n")) > limit {
				return errcode.ResourceExhausted.New("memory exceeds Unicode code point limit")
			}
			seen := make(map[string]bool, len(record.Entries))
			for _, text := range record.Entries {
				if seen[text] {
					return errcode.InvalidArgument.New("duplicate memory entry")
				}
				seen[text] = true
				if strings.ContainsFunc(text, func(r rune) bool { return unicode.Is(unicode.Cf, r) }) {
					return errcode.InvalidArgument.New("memory contains Unicode format characters")
				}
			}
		}
		for _, target := range []string{memoryTarget, userTarget} {
			if record, ok := records[target]; ok {
				if err := tx.Save(&record).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func memoryText(ctx context.Context, db *gorm.DB, c *sdk.Context, target string) (string, error) {
	scope, err := memoryScope(c, target)
	if err != nil {
		return "", err
	}
	record, err := readMemory(db.WithContext(ctx), scope)
	if err != nil {
		return "", err
	}
	return strings.Join(record.Entries, "\n"), nil
}

// conversationMemory 须持有会话锁，实际空会话才允许写入第一份冻结快照。
func (a *component) conversationMemory(c *sdk.Context, db *gorm.DB, key string) ([]conversationEntry, string, error) {
	profile, err := memoryText(c.Context(), db, c, userTarget)
	if err != nil {
		return nil, "", err
	}
	entries, _, _, err := sdk.ListConversation(c.Context(), key, 0)
	if err != nil {
		return nil, "", err
	}
	if len(entries) == 0 {
		notes, err := memoryText(c.Context(), db, c, memoryTarget)
		if err != nil {
			return nil, "", err
		}
		encoded, err := json.Marshal(conversationEntry{Role: roleSystem, Kind: memorySnapshot, Content: notes})
		if err != nil {
			return nil, "", fmt.Errorf("encode memory snapshot: %w", err)
		}
		if _, err := sdk.AppendConversation(c.Context(), key, encoded); err != nil {
			return nil, "", err
		}
	}
	history, err := recentConversation(c.Context(), key, a.config.historyEntries)
	return history, profile, err
}

func memoryError(c *sdk.Context, err error) error {
	if c.Context().Err() != nil {
		return c.Context().Err()
	}
	c.Logger().ErrorContext(c.Context(), "memory read failed", "error", err)
	code := errcode.Of(err)
	if code == 0 {
		code = errcode.Unavailable
	}
	return c.Reply(c.T("agent.memory_error", "code", code.String()))
}
