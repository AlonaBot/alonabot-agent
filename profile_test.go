//go:build unix

package agent_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/AlonaBot/AlonaBot/agent/provider"
	"github.com/AlonaBot/AlonaBot/sdk/message"
)

// TestAgentProfiles 经真实 Core、SDK Session 和 SQLite 验证配置文件选择、命令修改与后续对话。
func TestAgentProfiles(t *testing.T) {
	type completion struct {
		Model    string                           `json:"model"`
		Messages []struct{ Role, Content string } `json:"messages"`
	}
	requests := make(chan completion, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request completion
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		requests <- request
		w.Header().Set("Content-Type", "text/event-stream")
		writeDelta(w, provider.OpenAI, request.Messages[len(request.Messages)-1].Content)
		writeEnd(w, provider.OpenAI)
	}))
	t.Cleanup(server.Close)
	configuration := agentConfig(server.URL, provider.OpenAI)
	configuration["models"] = append(configuration["models"].([]any), map[string]any{"name": "second", "provider": "test", "model": "second-model", "type": "chat"})
	configuration["personas"] = []any{map[string]any{"name": "first", "prompt": "first system"}, map[string]any{"name": "second", "prompt": "second system"}}
	configuration["default_persona"] = "first"
	h, admin := startModelAdmin(t, configuration)
	first := admin.createProfile(t, "First", map[string]any{"chat_model": "test", "default_persona": "first", "triggers": []string{"mention"}})
	secondValues := map[string]any{"chat_model": "second", "default_persona": "second", "triggers": []string{"wake_word"}, "wake_words": []string{"Alona"}}
	second := admin.createProfile(t, "Second", secondValues)
	other := "agentloop:other"
	admin.assignProfile(t, "main", loopSession, first)
	admin.assignProfile(t, "main", other, second)

	checkTurn := func(session, text, model, persona string, toMe bool) {
		t.Helper()
		before := len(h.readEntriesKey(t, session+"@main"))
		h.deliverTo(t, "main", session, text, "member", message.ChatGroup, toMe)
		h.waitText(t, text)
		h.waitEntriesKey(t, session+"@main", max(1, before)+2)
		select {
		case request := <-requests:
			if request.Model != model || len(request.Messages) == 0 || request.Messages[0].Role != "system" || request.Messages[0].Content != persona || request.Messages[len(request.Messages)-1].Content != text {
				t.Fatalf("profile request = %+v, want %s/%s/%s", request, model, persona, text)
			}
		case <-h.ctx.Done():
			t.Fatal(h.ctx.Err())
		}
	}
	// 两份覆盖各自决定模型、人格与触发方式，未命中的消息不进入历史。
	h.deliverTo(t, "main", loopSession, "Alona ignored", "member", message.ChatGroup, false)
	checkTurn(loopSession, "mention first", "test-model", "first system", true)
	h.deliverTo(t, "main", other, "ignored mention", "member", message.ChatGroup, true)
	checkTurn(other, "Alona second", "second-model", "second system", false)

	for _, session := range []string{loopSession, other} {
		h.deliverTo(t, "main", session, "/profile First", "admin", message.ChatGroup, false)
		count := 1
		if session == other {
			count = 2
		}
		h.waitText(t, fmt.Sprintf("Current profile: First (%d sessions).", count))
	}
	for _, command := range []string{"/agent on", "/persona second", "/provider second"} {
		after := len(h.loop.snapshot())
		h.deliver(t, command, "admin", "en-US", false)
		h.waitTextAfter(t, after, "Profile updated: First (2 sessions).")
	}
	want := map[string]any{"chat_model": "second", "default_persona": "second", "triggers": []any{"mention", "wake_word", "private"}}
	if got := admin.profileOverrides(t, first); !reflect.DeepEqual(got, want) {
		t.Fatalf("shared profile overrides = %v, want %v", got, want)
	}
	for _, session := range []string{loopSession, other} {
		checkTurn(session, "shared "+session, "second-model", "second system", true)
	}
	raw, _ := json.Marshal(secondValues)
	var unchanged map[string]any
	if err := json.Unmarshal(raw, &unchanged); err != nil {
		t.Fatal(err)
	}
	if got := admin.profileOverrides(t, second); !reflect.DeepEqual(got, unchanged) {
		t.Fatalf("other profile changed: %v, want %v", got, unchanged)
	}
	select {
	case request := <-requests:
		t.Fatalf("unexpected extra conversation: %+v", request)
	default:
	}
}
