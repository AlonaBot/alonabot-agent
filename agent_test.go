package agent_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/AlonaBot/AlonaBot/plugintest"
	"github.com/AlonaBot/AlonaBot/sdk/message"
	agent "github.com/AlonaBot/alonabot-agent"
)

// TestRichMessages 经真实 Core、Agent 与脚本化 Provider 验证分段、回执、历史和按钮主路径。
func TestRichMessages(t *testing.T) {
	var mu sync.Mutex // HTTP Handler 记录与测试读取共享请求切片。
	var requests []map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		step := len(requests)
		requests = append(requests, request)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		delta := map[string]any{"content": "plain reply"}
		finish := "stop"
		switch step {
		case 0:
			delta = map[string]any{"content": "before", "tool_calls": []any{map[string]any{"index": 0, "id": "rich-1", "type": "function", "function": map[string]any{"name": "send_message", "arguments": `{"parts":[{"type":"image","source":{"https_url":"https://example.com/chart.png"},"caption":"chart"},{"type":"buttons","rows":[[{"label":"Continue"},{"label":"Docs","url":"https://example.com"}]]}]}`}}}}
			finish = "tool_calls"
		case 1:
			delta = map[string]any{"content": "after"}
		}
		chunk, _ := json.Marshal(map[string]any{"id": "scripted", "object": "chat.completion.chunk", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}})
		end, _ := json.Marshal(map[string]any{"id": "scripted", "object": "chat.completion.chunk", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}})
		if _, err := fmt.Fprintf(w, "data: %s\n\ndata: %s\n\ndata: [DONE]\n\n", chunk, end); err != nil {
			t.Error(err)
		}
	}))
	defer upstream.Close()
	p := agent.New()
	configuration, _ := json.Marshal(map[string]any{
		"providers": []any{map[string]any{"custom": map[string]any{"name": "scripted", "protocol": "openai-compatible", "base_url": upstream.URL, "api_key": "test"}}},
		"models":    []any{map[string]any{"name": "chat", "provider": "scripted", "model": "chat", "type": "chat", "max_tokens": 1024}}, "chat_model": "chat",
	})
	h := plugintest.Start(t, plugintest.Plugin(p), plugintest.Config(p, string(configuration)), plugintest.Admin("owner"), plugintest.AllowedInstances(p), plugintest.Capabilities("message.send", "message.buttons"))
	chat := plugintest.Group("rich")
	owner := plugintest.User{ID: "owner"}
	first := h.Text(chat, owner, "/ask send chart")
	for _, outcome := range first.Outcomes {
		if outcome.Err != nil {
			t.Fatalf("first round: %v", outcome.Err)
		}
	}
	if len(first.Sends) != 3 || first.Sends[0].Text() != "before" || first.Sends[2].Text() != "after" {
		t.Fatalf("delivery order = %+v", first.Sends)
	}
	if _, ok := first.Sends[1].Chain[0].(message.ImageSegment); !ok {
		t.Fatalf("middle delivery = %#v", first.Sends[1].Chain)
	}
	buttons := first.Sends[1].Chain[1].(message.ButtonsSegment)
	if buttons.Rows[0][0].Value != `"Continue"` || buttons.Rows[0][1].URL != "https://example.com" {
		t.Fatalf("buttons = %#v", buttons.Rows)
	}
	second := h.Text(chat, owner, "/ask next")
	second.RequireReply(t, "plain reply")
	mu.Lock()
	requestJSON, _ := json.Marshal(requests[2]["messages"])
	toolJSON, _ := json.Marshal(requests[0]["tools"])
	resultJSON, _ := json.Marshal(requests[1]["messages"])
	mu.Unlock()
	history := string(requestJSON)
	before, rich, after := strings.Index(history, "before"), strings.Index(history, "[image] chart"), strings.Index(history, "after")
	if before < 0 || rich <= before || after <= rich {
		t.Fatalf("history order = %s", history)
	}
	for _, rule := range []string{"Do not repeat", "resend_safe", "unknown", "get_message_format"} {
		if !strings.Contains(string(toolJSON), rule) {
			t.Fatalf("tool rule %q missing", rule)
		}
	}
	if !strings.Contains(string(resultJSON), "message_refs") || !strings.Contains(string(resultJSON), "accepted") {
		t.Fatalf("receipt missing: %s", resultJSON)
	}
	// /new 后旧按钮仍以当前点击者的普通文字开启会话。
	h.Text(chat, owner, "/new")
	clicked := h.Click(plugintest.User{ID: "reader"}, first.Sends[1], "agent.choice")
	clicked.RequireReply(t, "plain reply")
	mu.Lock()
	last := requests[len(requests)-1]
	lastJSON, _ := json.Marshal(last["messages"])
	lastTools, _ := json.Marshal(last["tools"])
	mu.Unlock()
	if !strings.Contains(string(lastJSON), "Continue") || strings.Contains(string(lastJSON), "before") {
		t.Fatalf("choice/reset request = %s", lastJSON)
	}
	if strings.Contains(string(lastTools), "Run a shell command") {
		t.Fatal("choice inherited the original administrator's tools")
	}
}
