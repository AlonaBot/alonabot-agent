package agent

import (
	"fmt"
	"strings"
	"time"

	"github.com/AlonaBot/AlonaBot/sdk"
	"github.com/AlonaBot/AlonaBot/sdk/message"
	"github.com/AlonaBot/alonabot-agent/provider"
)

type conversationInput struct {
	text        strings.Builder
	images      []provider.Image
	imagePaths  []string
	hasNodes    bool
	attachments map[string]message.MediaSource
}

// readConversationInput 按同一消息链展开节点和媒体，命令调用传入已去掉命令前缀的参数链。
func readConversationInput(c *sdk.Context, chain message.Chain) (*conversationInput, error) {
	input := &conversationInput{attachments: make(map[string]message.MediaSource)}
	if err := input.readChain(c, chain, "message", false); err != nil {
		return nil, err
	}
	if !input.hasNodes {
		input.imagePaths = nil
	}
	return input, nil
}

func (input *conversationInput) readChain(c *sdk.Context, chain message.Chain, path string, nested bool) error {
	for index, segment := range chain {
		at := fmt.Sprintf("%s[%d]", path, index)
		switch value := segment.(type) {
		case message.TextSegment:
			input.text.WriteString(value.Text)
		case message.MarkdownSegment:
			input.text.WriteString(value.Content)
		case message.ImageSegment:
			input.attachments[at] = value.Source
			image, err := readImage(c, value.Source)
			if err != nil {
				// 平台与 Core 的媒体令牌只用于授权，错误进入日志与回复前去掉令牌。
				detail := err.Error()
				if source := value.Source.Incoming; source != nil && source.Token != "" {
					detail = strings.ReplaceAll(detail, source.Token, "[REDACTED]")
				}
				return fmt.Errorf("%s: %s", at, detail)
			}
			input.images = append(input.images, image)
			input.imagePaths = append(input.imagePaths, at)
			fmt.Fprintf(&input.text, "\n[image: %s]\n", at)
		case message.ForwardSegment:
			input.hasNodes = true
			items := value.Items
			if value.PlatformForwardID != "" {
				var err error
				items, err = c.ReadNodes(value)
				if err != nil {
					return fmt.Errorf("%s.nodes: %w", at, err)
				}
			} else {
				var resolved []message.ForwardItem
				for nodeIndex, item := range items {
					if item.PlatformMessageID == "" {
						resolved = append(resolved, item)
						continue
					}
					nodes, err := c.ReadNodes(message.ForwardSegment{Items: []message.ForwardItem{item}})
					if err != nil {
						return fmt.Errorf("%s.nodes[%d]: %w", at, nodeIndex, err)
					}
					resolved = append(resolved, nodes...)
				}
				items = resolved
			}
			for nodeIndex, item := range items {
				nodePath := fmt.Sprintf("%s.nodes[%d]", at, nodeIndex)
				fmt.Fprintf(&input.text, "\n[node %s", nodePath)
				if item.Source != nil {
					fmt.Fprintf(&input.text, " author=%q user_id=%q", item.Source.DisplayName, item.Source.ExternalUserID)
				}
				if !item.OccurredAt.IsZero() {
					fmt.Fprintf(&input.text, " time=%s", item.OccurredAt.UTC().Format(time.RFC3339))
				}
				input.text.WriteString("]\n")
				if err := input.readChain(c, item.Chain, nodePath+".chain", true); err != nil {
					return err
				}
				fmt.Fprintf(&input.text, "\n[/node %s]\n", nodePath)
			}
		case message.FileSegment:
			input.attachments[at] = value.Source
			fmt.Fprintf(&input.text, "\n[file %s name=%q mime=%q] %s\n", at, value.Filename, value.MIME, value.Caption)
		case message.VideoSegment:
			input.attachments[at] = value.Source
			fmt.Fprintf(&input.text, "\n[video %s] %s\n", at, value.Caption)
		case message.VoiceSegment:
			input.attachments[at] = value.Source
			fmt.Fprintf(&input.text, "\n[voice %s] %s\n", at, value.Caption)
		case message.MentionSegment:
			if nested {
				fmt.Fprintf(&input.text, "@%s(%s)", value.Display, value.ExternalUserID)
			}
		}
	}
	return nil
}

// imageLegend 将当前请求的图片顺序对应回节点路径，历史图片过期后仍保留原来的归属。
func imageLegend(text string, paths []string, attached []bool) string {
	if len(paths) == 0 {
		return text
	}
	var out strings.Builder
	out.WriteString(text)
	out.WriteString("\n[attachments]\n")
	number := 0
	for index, path := range paths {
		if attached[index] {
			number++
			fmt.Fprintf(&out, "image %d: %s\n", number, path)
		} else {
			fmt.Fprintf(&out, "image unavailable: %s\n", path)
		}
	}
	return out.String()
}
