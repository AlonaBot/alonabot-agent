package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/AlonaBot/AlonaBot/agent/provider"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type modelUsage struct {
	Model        string `gorm:"primaryKey" json:"model"`
	Date         string `gorm:"primaryKey" json:"-"` // Agent 本地日期，YYYY-MM-DD。
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
}

// TableName 指定受管 SQLite 中的用量表名。
func (modelUsage) TableName() string { return "model_usage" }

func recordUsage(ctx context.Context, db *gorm.DB, model string, at time.Time, usage *provider.Usage) error {
	if usage == nil {
		return nil
	}
	if usage.InputTokens < 0 || usage.OutputTokens < 0 {
		return errors.New("provider token usage must be non-negative")
	}
	row := modelUsage{Model: model, Date: at.Local().Format(time.DateOnly), InputTokens: int64(usage.InputTokens), OutputTokens: int64(usage.OutputTokens)}
	// 同一条 upsert 完成累加，避免并发请求读到相同基数。
	if err := db.WithContext(ctx).Clauses(clause.OnConflict{DoUpdates: clause.Assignments(map[string]any{
		"input_tokens":  gorm.Expr("input_tokens + excluded.input_tokens"),
		"output_tokens": gorm.Expr("output_tokens + excluded.output_tokens"),
	})}).Create(&row).Error; err != nil {
		return fmt.Errorf("record model usage: %w", err)
	}
	return nil
}

func readUsage(ctx context.Context, db *gorm.DB, at time.Time) ([]modelUsage, error) {
	rows := []modelUsage{}
	err := db.WithContext(ctx).Where(&modelUsage{Date: at.Local().Format(time.DateOnly)}).
		Order(clause.OrderByColumn{Column: clause.Column{Name: "model"}}).Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("read model usage: %w", err)
	}
	return rows, nil
}
