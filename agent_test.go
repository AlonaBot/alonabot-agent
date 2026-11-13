//go:build unix

package agent_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/AlonaBot/AlonaBot"
	"github.com/AlonaBot/AlonaBot/agent"
	"github.com/AlonaBot/AlonaBot/agent/provider"
	"github.com/AlonaBot/AlonaBot/sdk"
	"github.com/AlonaBot/AlonaBot/sdk/adapter"
	"github.com/AlonaBot/AlonaBot/sdk/errcode"
	"github.com/AlonaBot/AlonaBot/sdk/message"
)

const (
	agentID     = "alonabot_agent_official"
	loopID      = "agentloop"
	loopSession = message.Session("agentloop:group")
	testTimeout = 20 * time.Second
)

func init() {
	sdk.UseGeneratedAdapter((*loopback)(nil), sdk.Generated{
		Manifest:            []byte("id: alonabot_adapter_agentloop\nname: Agent test loopback\nversion: 0.1.0\nauthor: AlonaBot\nrole: adapter\nadapter_id: agentloop\nsource: github.com/AlonaBot/AlonaBot\n"),
		AdapterConfigSchema: []byte(`{}`),
	})
}

type operation struct {
	method, id, text string
	buttons          []message.Button
}

type loopback struct {
	core       adapter.Core
	cores      map[string]adapter.Core // 各实例的真实入站入口，启动后只读。
	edit       bool
	buttons    bool
	limit      uint64
	mu         sync.Mutex // 记录由 Core 并发调用的发送与编辑。
	operations []operation
	changed    chan struct{} // 关闭后同时唤醒 Provider 与测试断言，随后换成新通道。
	sequence   atomic.Uint64
}

func (a *loopback) Descriptor() *adapter.Descriptor {
	return &adapter.Descriptor{AdapterID: loopID, Capabilities: a.Capabilities()}
}

func (a *loopback) Start(_ context.Context, request adapter.StartRequest, core adapter.Core) (adapter.Instance, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cores == nil {
		a.cores = make(map[string]adapter.Core)
	}
	a.cores[request.InstanceID] = core
	if request.InstanceID == "main" {
		a.core = core
	}
	return &loopbackInstance{loopback: a, instance: request.InstanceID}, nil
}

type loopbackInstance struct {
	*loopback
	instance string
}

func (a *loopbackInstance) Account() adapter.Account {
	if a.instance == "main" {
		return a.loopback.Account()
	}
	return adapter.Account{ID: "bot-" + a.instance}
}

func (a *loopbackInstance) Send(ctx context.Context, to message.Session, chain message.Chain) (*message.SendResult, *errcode.Error) {
	result, err := a.loopback.Send(ctx, to, chain)
	result.AdapterInstanceID = a.instance
	return result, err
}

func (a *loopbackInstance) Edit(ctx context.Context, to message.Session, id string, chain message.Chain) (*message.SendResult, *errcode.Error) {
	result, err := a.loopback.Edit(ctx, to, id, chain)
	result.AdapterInstanceID = a.instance
	return result, err
}

func (*loopback) Account() adapter.Account { return adapter.Account{ID: "bot"} }

func (a *loopback) Capabilities() []message.Capability {
	return []message.Capability{
		{Name: "message.send", Version: 1, Supported: true, TextLimit: &message.TextLimit{Max: a.limit, Unit: message.TextUnicodeCodePoints}},
		{Name: "message.edit", Version: 1, Supported: a.edit},
		{Name: "message.buttons", Version: 1, Supported: a.buttons},
	}
}

func (a *loopback) record(method, id string, to message.Session, chain message.Chain) (*message.SendResult, *errcode.Error) {
	a.mu.Lock()
	op := operation{method: method, id: id, text: chain.Text()}
	for _, segment := range chain {
		if buttons, ok := segment.(message.ButtonsSegment); ok {
			for _, row := range buttons.Rows {
				op.buttons = append(op.buttons, row...)
			}
		}
	}
	a.operations = append(a.operations, op)
	close(a.changed)
	a.changed = make(chan struct{})
	a.mu.Unlock()
	return &message.SendResult{Status: message.SendAccepted, Session: to, AdapterInstanceID: "main", PrimaryPlatformMessageID: id, PlatformMessageIDs: []string{id}, PartResults: []message.PartResult{{Status: message.PartAccepted, PlatformMessageIDs: []string{id}}}}, nil
}

func (a *loopback) Send(_ context.Context, to message.Session, chain message.Chain) (*message.SendResult, *errcode.Error) {
	return a.record("send", fmt.Sprint(a.sequence.Add(1)), to, chain)
}

func (a *loopback) Edit(_ context.Context, to message.Session, id string, chain message.Chain) (*message.SendResult, *errcode.Error) {
	return a.record("edit", id, to, chain)
}

func (*loopback) Stop(context.Context) error { return nil }

func (a *loopback) snapshot() []operation {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]operation(nil), a.operations...)
}

func (a *loopback) wait(ctx context.Context, ready func([]operation) bool) bool {
	for {
		a.mu.Lock()
		operations := append([]operation(nil), a.operations...)
		changed := a.changed
		a.mu.Unlock()
		if ready(operations) {
			return true
		}
		select { // 等到记录变化或本次测试取消，避免 Provider 与显示回调互相等待。
		case <-changed:
		case <-ctx.Done():
			return false
		}
	}
}

type harness struct {
	ctx  context.Context
	loop *loopback
	db   *sql.DB
	dir  string
	stop func() error
}

func startAgent(t *testing.T, loop *loopback, configuration map[string]any, coreExtra map[string]any, plugins ...*sdk.Plugin) *harness {
	t.Helper()
	return startAgentAt(t, t.TempDir(), loop, configuration, coreExtra, agent.New(), nil, plugins...)
}

func startAgentAt(t *testing.T, dir string, loop *loopback, configuration map[string]any, coreExtra map[string]any, root *sdk.Plugin, args []string, plugins ...*sdk.Plugin) *harness {
	t.Helper()
	pluginDir := filepath.Join(dir, "plugins", agentID)
	if err := os.MkdirAll(pluginDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(pluginDir, "config.json"), configuration)
	coreConfig := map[string]any{"listen": "127.0.0.1:0", "admins": []string{"agentloop:admin"}, "adapters": map[string]any{loopID: map[string]any{"instances": map[string]any{"main": map[string]any{}}}}}
	for key, value := range coreExtra {
		coreConfig[key] = value
	}
	writeJSON(t, filepath.Join(dir, "core_config.json"), coreConfig)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	logger := slog.Default()
	options := []alonabot.Option{alonabot.DataDir(dir), alonabot.Plugin(root), alonabot.Adapter(loop), alonabot.Args(args...)}
	for _, plugin := range plugins {
		options = append(options, alonabot.Plugin(plugin))
	}
	core, err := alonabot.Start(ctx, options...)
	if err != nil {
		cancel()
		slog.SetDefault(logger)
		t.Fatalf("start Agent: %v", err)
	}
	stop := sync.OnceValue(func() error { cancel(); return core.Wait() })
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Errorf("stop Agent: %v", err)
		}
		slog.SetDefault(logger)
	})
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "state.sqlite")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &harness{ctx: ctx, loop: loop, db: db, dir: dir, stop: stop}
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func newLoop(edit bool, limit uint64) *loopback {
	return &loopback{edit: edit, limit: limit, changed: make(chan struct{})}
}

func (h *harness) deliver(t *testing.T, text, user, locale string, toMe bool) {
	t.Helper()
	accepted, err := h.loop.core.Deliver(h.ctx, &message.Event{
		Type: "message.created", Source: message.SourceAdapter, AdapterID: loopID, AdapterInstanceID: "main", AccountID: "bot", Session: loopSession, ChatType: message.ChatGroup,
		Actor: &message.Actor{ExternalUserID: user, AdapterInstanceID: "main", Locale: locale}, Payload: &message.CreatedPayload{PlatformMessageID: fmt.Sprint(h.loop.sequence.Add(1)), Chain: message.Chain{message.Text(text)}, ToMe: toMe},
	}, nil)
	if err != nil || !accepted {
		t.Fatalf("deliver %q: accepted=%v, error=%v", text, accepted, err)
	}
}

func (h *harness) waitText(t *testing.T, text string) []operation {
	t.Helper()
	if !h.loop.wait(h.ctx, func(operations []operation) bool {
		return len(operations) > 0 && operations[len(operations)-1].text == text
	}) {
		t.Fatalf("wait for %q: %v", text, h.loop.snapshot())
	}
	return h.loop.snapshot()
}

func (h *harness) entries(t *testing.T) []map[string]string {
	t.Helper()
	rows, err := h.db.QueryContext(h.ctx, "SELECT entry_json FROM conversation_entry WHERE plugin_id = ? AND conversation_key = ? ORDER BY seq", agentID, string(loopSession)+"@main")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var entries []map[string]string
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var entry map[string]string
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return entries
}

func (h *harness) waitEntries(t *testing.T, count int) []map[string]string {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		entries := h.entries(t)
		if len(entries) == count+1 {
			if entries[0]["role"] != "system" || entries[0]["kind"] != "memory_snapshot" {
				t.Fatalf("missing initial memory snapshot: %v", entries)
			}
			for _, entry := range entries[1:] {
				if entry["kind"] != "" {
					t.Fatalf("unexpected snapshot in ordinary history: %v", entries)
				}
			}
			return entries[1:]
		}
		select {
		case <-ticker.C:
		case <-h.ctx.Done():
			t.Fatalf("conversation entries = %v, want %d", entries, count)
		}
	}
}

func agentConfig(url string, protocol provider.Protocol) map[string]any {
	return map[string]any{"providers": []any{map[string]any{"custom": map[string]any{"name": "test", "protocol": protocol, "base_url": url, "api_key": "test-key", "model": "test-model"}}}, "default_provider": "test"}
}

func writeDelta(w http.ResponseWriter, protocol provider.Protocol, text string) {
	var value any = map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": text}}}}
	if protocol == provider.AnthropicMessages {
		value = map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}}
		fmt.Fprint(w, "event: content_block_delta\n")
	}
	raw, _ := json.Marshal(value)
	fmt.Fprintf(w, "data: %s\n\n", raw)
	w.(http.Flusher).Flush()
}

func writeEnd(w http.ResponseWriter, protocol provider.Protocol) {
	if protocol == provider.AnthropicMessages {
		fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	} else {
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}
	w.(http.Flusher).Flush()
}

// TestAgentConversation 经真实 Core、SDK Session 和 SQLite 验证流显示、历史与断流。
func TestAgentConversation(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		protocol provider.Protocol
		edit     bool
		limit    uint64
		broken   bool
	}{
		{"openai", provider.OpenAI, true, 64, false},
		{"openai-compatible", provider.OpenAICompatible, true, 64, false},
		{"anthropic-messages", provider.AnthropicMessages, true, 64, false},
		{"without-edit", provider.OpenAICompatible, false, 64, false},
		{"without-edit-over-limit", provider.OpenAICompatible, false, 6, false},
		{"interrupted", provider.OpenAICompatible, true, 64, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			loop := newLoop(scenario.edit, scenario.limit)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				for i, text := range []string{"Hello", ", ", "world!"} {
					writeDelta(w, scenario.protocol, text)
					if scenario.edit && i < 2 {
						if !loop.wait(r.Context(), func(operations []operation) bool { return len(operations) > i }) {
							return
						}
					}
					if scenario.broken {
						return // 缺少 finish_reason，模拟已经显示文本后的断流。
					}
				}
				writeEnd(w, scenario.protocol)
			}))
			t.Cleanup(server.Close)
			h := startAgent(t, loop, agentConfig(server.URL, scenario.protocol), nil)
			h.deliver(t, "/ask hello", "member", "zh-CN", false)
			if scenario.broken {
				operations := h.waitText(t, "Provider 请求失败（ERROR_CODE_UNAVAILABLE）。")
				if len(operations) != 2 || operations[0].text != "Hello" || operations[0].method != "send" {
					t.Fatalf("partial content was not preserved: %v", operations)
				}
				entries := h.entries(t)
				if len(entries) != 1 || entries[0]["role"] != "system" || entries[0]["kind"] != "memory_snapshot" || entries[0]["content"] != "" {
					t.Fatalf("failed turn must retain only the empty snapshot: %v", entries)
				}
			} else {
				entries := h.waitEntries(t, 2)
				operations := loop.snapshot()
				if entries[0]["role"] != "user" || entries[0]["speaker"] != "member" || entries[0]["content"] != "hello" || entries[1]["role"] != "assistant" || entries[1]["content"] != "Hello, world!" {
					t.Fatalf("conversation entries = %v", entries)
				}
				if scenario.edit {
					if len(operations) < 3 || operations[0].method != "send" || operations[0].text != "Hello" {
						t.Fatalf("missing initial send or edits: %v", operations)
					}
					for _, op := range operations[1:] {
						if op.method != "edit" || op.id != operations[0].id {
							t.Errorf("stream changed the message ID: %v", operations)
						}
					}
					if operations[len(operations)-1].text != "Hello, world!" {
						t.Errorf("final edit = %v", operations)
					}
				} else {
					var combined strings.Builder
					for _, op := range operations {
						if op.method != "send" || uint64(utf8.RuneCountInString(op.text)) > scenario.limit {
							t.Errorf("non-editing output exceeds instance limit or edits: %v", operations)
						}
						combined.WriteString(op.text)
					}
					if combined.String() != "Hello, world!" || (scenario.limit == 64 && len(operations) != 1) || (scenario.limit == 6 && len(operations) < 2) {
						t.Errorf("non-editing output = %v", operations)
					}
				}
			}
			if calls.Load() != 1 {
				t.Errorf("provider requests = %d, want 1", calls.Load())
			}
		})
	}
}

func (h *harness) click(t *testing.T, op operation, allow bool, user string) {
	t.Helper()
	index := 0
	if !allow {
		index = 1
	}
	accepted, err := h.loop.core.Deliver(h.ctx, &message.Event{Type: "button.clicked", Source: message.SourceAdapter, AdapterID: loopID, AdapterInstanceID: "main", AccountID: "bot", Session: loopSession, ChatType: message.ChatGroup, Actor: &message.Actor{ExternalUserID: user, AdapterInstanceID: "main", Locale: "en-US"}, Payload: &message.ButtonClickedPayload{Value: op.buttons[index].CoreToken, CallbackQueryID: fmt.Sprint(h.loop.sequence.Add(1)), OriginPlatformMessageID: op.id}}, nil)
	if err != nil || !accepted {
		t.Fatalf("click confirmation: accepted=%v, error=%v", accepted, err)
	}
}

func (h *harness) confirmation(t *testing.T, after int) operation {
	t.Helper()
	var found operation
	if !h.loop.wait(h.ctx, func(operations []operation) bool {
		for _, op := range operations[after:] {
			if len(op.buttons) == 2 {
				found = op
				return true
			}
		}
		return false
	}) {
		t.Fatalf("wait confirmation: %v", h.loop.snapshot())
	}
	return found
}
