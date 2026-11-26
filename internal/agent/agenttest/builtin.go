// Package agenttest 提供经真实 Core 调用会话服务的 builtin 测试入口。
package agenttest

import (
	"github.com/AlonaBot/AlonaBot/sdk"
	"github.com/AlonaBot/AlonaBot/sdk/message"
	"github.com/AlonaBot/alonabot-agent/conversation"
)

// New 创建白名单调用方，供 plugintest 验证跨插件调用。
func New() *sdk.Plugin {
	p := sdk.NewPlugin()
	p.Command("new").Permission(sdk.User).Handle(func(c *sdk.Context) (message.Content, error) {
		c.StopPropagation()
		if _, err := conversation.Reset.Call(c, conversation.ResetRequest{}); err != nil {
			return nil, err
		}
		return message.Text("session reset"), nil
	})
	return p
}
