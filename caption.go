package agent

import (
	"errors"
	"strings"
	"time"

	"github.com/AlonaBot/AlonaBot/agent/provider"
	"github.com/AlonaBot/AlonaBot/sdk"
	"gorm.io/gorm"
)

// captionImages 按原顺序输出描述；未读到的图片和转述失败的图片保留占位。
func (a *component) captionImages(c *sdk.Context, db *gorm.DB, model *configuredModel, text string, images []provider.Image) string {
	var content strings.Builder
	for _, image := range images {
		if len(image.Data) == 0 {
			content.WriteString(imagePlaceholder + "\n")
			continue
		}
		if c.Err() != nil {
			break
		}
		request := provider.Request{MaxTokens: model.maxTokens, Messages: []provider.Message{{Role: roleUser, Content: a.config.imageCaptionPrompt, Images: []provider.Image{image}}}}
		var description strings.Builder
		result, err := model.client.Stream(c, request, func(event provider.Event) error {
			description.WriteString(event.Text)
			return c.Err()
		})
		if err == nil {
			// 转述失败时图片仍以占位继续对话，用量写入失败只记日志。
			if usageErr := recordUsage(c, db, model.name, time.Now(), result.Usage); usageErr != nil {
				c.Logger().WarnContext(c, "could not record model usage", "model", model.name, "error", usageErr)
			}
		}
		if err == nil && strings.TrimSpace(description.String()) == "" {
			err = errors.New("provider returned no image description")
		}
		if err != nil {
			c.Logger().WarnContext(c, "image caption request failed", "model", model.name, "error", err)
			content.WriteString(imagePlaceholder + "\n")
			continue
		}
		content.WriteString("[image: " + description.String() + "]\n")
	}
	content.WriteString(text)
	return content.String()
}
