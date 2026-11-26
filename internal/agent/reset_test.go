package agent_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/AlonaBot/AlonaBot/plugintest"
	"github.com/AlonaBot/AlonaBot/sdk"
	conversationapi "github.com/AlonaBot/AlonaBot/sdk/conversation"
	"github.com/AlonaBot/AlonaBot/sdk/message"
	agent "github.com/AlonaBot/alonabot-agent"
	"github.com/AlonaBot/alonabot-agent/internal/agent/agenttest"
)

// TestConversationReset 验证白名单插件继承群聊 Actor，只清空开启成员会话后的对应历史。
func TestConversationReset(t *testing.T) {
	p := agent.New()
	p.Command("seed").Permission(sdk.User).Handle(func(c *sdk.Context) (message.Content, error) {
		key := c.Session() + "@" + c.Instance()
		for _, session := range []string{key, key + "@" + c.User(), key + "@" + message.UID(plugintest.AdapterID, "bob")} {
			if _, err := conversationapi.Append(c, session, json.RawMessage(`{"role":"user","content":"saved"}`)); err != nil {
				return nil, err
			}
		}
		return message.Text("seeded"), nil
	})
	p.Command("history").Permission(sdk.User).Handle(func(c *sdk.Context) (message.Content, error) {
		key := c.Session() + "@" + c.Instance()
		var counts []int
		for _, session := range []string{key, key + "@" + c.User(), key + "@" + message.UID(plugintest.AdapterID, "bob")} {
			entries, _, _, err := conversationapi.List(c, session, 0)
			if err != nil {
				return nil, err
			}
			counts = append(counts, len(entries))
		}
		return message.Text(fmt.Sprint(counts)), nil
	})
	h := plugintest.Start(t, plugintest.Plugin(p), plugintest.Config(p, `{"member_sessions":true}`), plugintest.Plugin(agenttest.New()))
	chat, user := plugintest.Group("room"), plugintest.User{ID: "alice"}
	h.Text(chat, user, "/seed").RequireReply(t, "seeded")
	h.Text(chat, user, "/history").RequireReply(t, "[1 1 1]")
	h.Text(chat, user, "/new").RequireReply(t, "session reset")
	h.Text(chat, user, "/history").RequireReply(t, "[1 0 1]")
}
