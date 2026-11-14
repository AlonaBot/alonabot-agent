package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/AlonaBot/AlonaBot/agent/provider"
	"github.com/AlonaBot/AlonaBot/sdk"
	"github.com/AlonaBot/AlonaBot/sdk/message"
	"gorm.io/gorm"
)

const (
	imageBatchSize   = 3
	imagePlaceholder = "[image]"
)

type conversationImage struct {
	Session string `gorm:"primaryKey;autoIncrement:false"`
	Number  uint64 `gorm:"primaryKey;autoIncrement:false"`
	Seq     uint64 // 所属 Core user 条目的序号，避免本地写入失败后复用编号串图。
	MIME    string
	Bytes   []byte
}

func imageBoundary(count uint64) uint64 {
	if count == 0 {
		return 0
	}
	return (count - 1) / imageBatchSize * imageBatchSize
}

func prependImages(text string, count int) string {
	return strings.Repeat(imagePlaceholder+"\n", count) + text
}

func readImage(ctx context.Context, source message.MediaSource) (provider.Image, error) {
	incoming := source.Incoming
	if incoming == nil {
		return provider.Image{}, fmt.Errorf("image source has no incoming media")
	}
	if incoming.Readability != message.Readable {
		return provider.Image{}, fmt.Errorf("image is not readable: %d", incoming.Readability)
	}
	switch incoming.MIME {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
	default:
		return provider.Image{}, fmt.Errorf("unsupported image MIME: %q", incoming.MIME)
	}
	reader, err := sdk.OpenMedia(ctx, source)
	if err != nil {
		return provider.Image{}, err
	}
	data, readErr := io.ReadAll(reader)
	if err := errors.Join(readErr, reader.Close()); err != nil {
		return provider.Image{}, err
	}
	if len(data) == 0 {
		return provider.Image{}, fmt.Errorf("image is empty")
	}
	return provider.Image{MediaType: incoming.MIME, Data: data}, nil
}

func readImages(c *sdk.Context, text string) (string, []provider.Image) {
	var images []provider.Image
	for _, segment := range c.Event().Payload.(*message.CreatedPayload).Chain {
		image, ok := segment.(message.ImageSegment)
		if !ok {
			continue
		}
		loaded, err := readImage(c.Context(), image.Source)
		if err != nil {
			reason := err.Error()
			if incoming := image.Source.Incoming; incoming != nil && incoming.Token != "" {
				reason = strings.ReplaceAll(reason, incoming.Token, "[REDACTED]")
			}
			c.Logger().WarnContext(c.Context(), "image read failed", "session", sessionKey(c), "error", reason)
			text = prependImages(text, 1)
			continue
		}
		images = append(images, loaded)
	}
	return text, images
}

func storedImages(ctx context.Context, db *gorm.DB, session string, count uint64) (map[uint64]conversationImage, error) {
	images := make(map[uint64]conversationImage)
	if count == 0 {
		return images, nil
	}
	var rows []conversationImage
	if err := db.WithContext(ctx).Where("session = ? AND number > ?", session, imageBoundary(count)).Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		images[row.Number] = row
	}
	return images, nil
}

func replayImages(entry conversationEntry, images map[uint64]conversationImage, enabled bool) provider.Message {
	result := provider.Message{Role: entry.Role, Content: entry.Content}
	for _, number := range entry.Images {
		image, ok := images[number]
		if enabled && ok && image.Seq == entry.seq {
			result.Images = append(result.Images, provider.Image{MediaType: image.MIME, Data: image.Bytes})
		} else {
			result.Content = prependImages(result.Content, 1)
		}
	}
	return result
}

// appendImages 在会话锁内执行；Core 追加后本地事务失败时，文字历史仍保留。
func (a *component) appendImages(c *sdk.Context, db *gorm.DB, generation uint64, user conversationEntry, assistant json.RawMessage, images []provider.Image) error {
	session := sessionKey(c)
	var appended bool
	err := db.WithContext(c.Context()).Transaction(func(tx *gorm.DB) error {
		// 1. 以最终设置核对 reset 代次，并从最新计数分配连续编号。
		settings, err := a.settings(c.Context(), session, tx)
		if err != nil {
			return err
		}
		if settings.ResetGeneration != generation {
			return nil
		}
		for range images {
			settings.ImageCount++
			user.Images = append(user.Images, settings.ImageCount)
		}
		encoded, err := json.Marshal(user)
		if err != nil {
			return fmt.Errorf("encode user conversation entry: %w", err)
		}
		// 2. 先取得 Core 序号；后续本地失败保留已追加的文字历史。
		last, err := sdk.AppendConversation(c.Context(), session, encoded, assistant)
		if err != nil {
			return err
		}
		appended = true
		// 3. 图片、计数和越界清理一起提交，失败时一起回滚。
		for i, image := range images {
			row := conversationImage{Session: session, Number: user.Images[i], Seq: last - 1, MIME: image.MediaType, Bytes: image.Data}
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
		}
		if len(images) == 0 {
			return nil
		}
		if err := tx.Save(&settings).Error; err != nil {
			return err
		}
		return tx.Where("session = ? AND number <= ?", session, imageBoundary(settings.ImageCount)).Delete(&conversationImage{}).Error
	})
	if err != nil && appended {
		c.Logger().WarnContext(c.Context(), "image persistence failed after conversation append", "session", session, "error", err)
	}
	return err
}
