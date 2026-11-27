package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/AlonaBot/AlonaBot/sdk"
	conversationapi "github.com/AlonaBot/AlonaBot/sdk/conversation"
	mediaapi "github.com/AlonaBot/AlonaBot/sdk/media"
	"github.com/AlonaBot/AlonaBot/sdk/message"
	"github.com/AlonaBot/alonabot-agent/provider"
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
	if incoming.MIME != "" && !supportedImageMIME(incoming.MIME) {
		return provider.Image{}, fmt.Errorf("unsupported image MIME: %q", incoming.MIME)
	}
	reader, err := mediaapi.Open(ctx, source)
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
	mime := incoming.MIME
	if mime == "" {
		// 入站媒体可省略未知 MIME；只在缺少声明时按内容判断。
		mime = http.DetectContentType(data)
		if !supportedImageMIME(mime) {
			return provider.Image{}, fmt.Errorf("unsupported image MIME: %q", mime)
		}
	}
	return provider.Image{MediaType: mime, Data: data}, nil
}

func supportedImageMIME(mime string) bool {
	switch mime {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	default:
		return false
	}
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
	attached := make([]bool, len(entry.Images))
	for index, number := range entry.Images {
		image, ok := images[number]
		if enabled && ok && image.Seq == entry.seq {
			result.Images = append(result.Images, provider.Image{MediaType: image.MIME, Data: image.Bytes})
			attached[index] = true
		} else {
			result.Content = prependImages(result.Content, 1)
		}
	}
	result.Content = imageLegend(result.Content, entry.ImagePaths, attached)
	return result
}

// appendImages 在会话锁内执行；Core 追加后本地事务失败时，文字历史仍保留。
func (a *component) appendImages(c *sdk.Context, db *gorm.DB, generation uint64, user conversationEntry, assistants []conversationEntry, images []provider.Image) error {
	session := conversationKey(c)
	// 已投递的内容在请求取消后仍需保留；会话追加仅要求插件 Session 身份。
	ctx := context.WithoutCancel(c)
	var appended bool
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 1. 以最终设置核对 reset 代次，并从最新计数分配连续编号。
		settings, err := a.settings(ctx, session, tx)
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
		entries := []json.RawMessage{encoded}
		for _, assistant := range assistants {
			entry, err := json.Marshal(assistant)
			if err != nil {
				return fmt.Errorf("encode assistant conversation entry: %w", err)
			}
			entries = append(entries, entry)
		}
		last, err := conversationapi.Append(ctx, session, entries...)
		if err != nil {
			return err
		}
		appended = true
		// 3. 图片、计数和越界清理一起提交，失败时一起回滚。
		for i, image := range images {
			row := conversationImage{Session: session, Number: user.Images[i], Seq: last - uint64(len(assistants)), MIME: image.MediaType, Bytes: image.Data}
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
		c.Logger().WarnContext(c, "image persistence failed after conversation append", "session", session, "error", err)
	}
	if appended {
		// 管理员可能在生成期间清空正文和目录；仍持有会话锁时恢复目录。
		if directoryError := db.WithContext(ctx).Save(&conversationRecord{Key: session}).Error; directoryError != nil {
			err = errors.Join(err, fmt.Errorf("register conversation: %w", directoryError))
		}
	}
	return err
}
