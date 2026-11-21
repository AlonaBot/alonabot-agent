package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"

	pb "github.com/AlonaBot/AlonaBot/contracts/pb/alonabotv1"
	"github.com/AlonaBot/AlonaBot/sdk"
	"github.com/AlonaBot/AlonaBot/sdk/gormdb"
	"github.com/AlonaBot/AlonaBot/sdk/options"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const defaultSystemPrompt = "You are AlonaBot, a helpful assistant. Reply in the user's language and keep answers concise."

type persona struct {
	Name   string `gorm:"primaryKey;autoIncrement:false" json:"name"`
	Prompt string `gorm:"not null" json:"prompt"`
}

// TableName 固定人格目录的受管表名。
func (persona) TableName() string { return "persona" }

type personaReference struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Sessions uint64 `json:"sessions"`
}

func (a *component) readPersona(ctx context.Context, name string) (persona, error) {
	db, err := gormdb.Open(ctx)
	if err != nil {
		return persona{}, err
	}
	var selected persona
	err = db.WithContext(ctx).Where(clause.Eq{Column: clause.PrimaryColumn, Value: name}).Take(&selected).Error
	return selected, err
}

func (a *component) personaPrompt(ctx context.Context, name string) (string, error) {
	if name == "" {
		return defaultSystemPrompt, nil
	}
	selected, err := a.readPersona(ctx, name)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		slog.WarnContext(ctx, "selected persona no longer exists", "persona", name)
		return defaultSystemPrompt, nil
	}
	if err != nil {
		return "", fmt.Errorf("read persona: %w", err)
	}
	return selected.Prompt, nil
}

func (a *component) personaOptions(ctx context.Context, q options.Query) (options.Page, error) {
	result := options.Page{Options: []options.Option{}}
	db, err := gormdb.Open(ctx)
	if err != nil {
		return result, err
	}
	var rows []persona
	if err := db.WithContext(ctx).Order("name").Find(&rows).Error; err != nil {
		return result, err
	}
	for _, row := range rows {
		if len(q.Values) > 0 {
			if !slices.Contains(q.Values, row.Name) {
				continue
			}
		} else if !strings.Contains(strings.ToLower(row.Name), strings.ToLower(q.Search)) {
			continue
		}
		if len(q.Values) == 0 && len(result.Options) == q.Limit {
			result.More = true
			break
		}
		result.Options = append(result.Options, options.Option{Value: row.Name, Label: row.Name})
	}
	return result, nil
}

func (a *component) personaReferences(ctx context.Context) (map[string][]personaReference, error) {
	overrides, err := sdk.ProfileOverrides(ctx)
	if err != nil {
		return nil, fmt.Errorf("read profile overrides: %w", err)
	}
	profiles, err := profileList.Call(ctx, &pb.ProfileListRequest{})
	if err != nil {
		return nil, fmt.Errorf("list profiles: %w", err)
	}
	result := make(map[string][]personaReference)
	for _, profile := range profiles.Profiles {
		name := a.basePersona
		if raw, ok := overrides[profile.Id]["default_persona"]; ok {
			if err := json.Unmarshal(raw, &name); err != nil {
				return nil, fmt.Errorf("decode persona override: %w", err)
			}
		}
		if name != "" {
			result[name] = append(result[name], personaReference{ID: profile.Id, Name: profile.Name, Sessions: profile.Sessions})
		}
	}
	for name := range result {
		sort.Slice(result[name], func(i, j int) bool { return result[name][i].ID < result[name][j].ID })
	}
	return result, nil
}
