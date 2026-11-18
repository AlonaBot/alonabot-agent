//go:build unix

package agent_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/AlonaBot/AlonaBot/agent/provider"
	pb "github.com/AlonaBot/AlonaBot/contracts/pb/alonabotv1"
)

func memoryDatabase(t *testing.T, h *harness) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(h.dir, "plugins", agentID, "dev.sqlite")+"?mode=rw&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func seedMemory(t *testing.T, db *sql.DB, target, scope, instance string, entries ...string) {
	t.Helper()
	raw, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO memory_record(target,scope,instance,entries) VALUES(?,?,?,?) ON CONFLICT(target,scope,instance) DO UPDATE SET entries=excluded.entries", target, scope, instance, string(raw)); err != nil {
		t.Fatal(err)
	}
}

func storedMemory(t *testing.T, db *sql.DB, target, scope, instance string) []string {
	t.Helper()
	var raw string
	if err := db.QueryRow("SELECT entries FROM memory_record WHERE target=? AND scope=? AND instance=?", target, scope, instance).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var result []string
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func setMemoryPolicy(t *testing.T, h *harness, policy pb.ToolPolicy) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(h.dir, "state.sqlite")+"?mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("INSERT INTO tool_policy(plugin_id,service_name,policy) VALUES(?,?,?) ON CONFLICT(plugin_id,service_name) DO UPDATE SET policy=excluded.policy", agentID, "memory", int(policy)); err != nil {
		t.Fatal(err)
	}
}

func writeMemoryCall(w http.ResponseWriter, name string, arguments json.RawMessage) {
	event := map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "memory-call", "type": "function", "function": map[string]any{"name": name, "arguments": string(arguments)}}}}}}}
	raw, _ := json.Marshal(event)
	fmt.Fprintf(w, "data: %s\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", raw)
	w.(http.Flusher).Flush()
}

func checkMemoryContext(t *testing.T, request completionRequest, notes, profile, other string) {
	t.Helper()
	var systems []string
	for _, message := range request.Messages {
		if message.Role == "system" {
			systems = append(systems, message.Content)
		}
	}
	want := []string{"You are AlonaBot, a helpful assistant. Reply in the user's language and keep answers concise.", notes}
	if profile != "" {
		want = append(want, profile)
	}
	if !reflect.DeepEqual(systems, want) {
		t.Errorf("system context=%q want %q", systems, want)
	}
	for _, message := range request.Messages {
		if strings.Contains(message.Content, other) {
			t.Errorf("another actor's profile leaked: %+v", message)
		}
	}
}

// TestAgentMemory 经真实 Core、SDK Session、SQLite 与模型工具循环验证快照、隔离和策略。
func TestAgentMemory(t *testing.T) {
	// 快照、用户隔离与策略必须在同一条真实工具链中取证，因此保留这些子场景。
	for _, scenario := range []struct {
		name        string
		policy      pb.ToolPolicy
		allow, full bool
	}{
		{name: "allow-snapshot-and-profiles", allow: true, full: true},
		{name: "confirm-allow", policy: pb.ToolPolicy_TOOL_POLICY_CONFIRM, allow: true},
		{name: "confirm-reject", policy: pb.ToolPolicy_TOOL_POLICY_CONFIRM},
		{name: "deny", policy: pb.ToolPolicy_TOOL_POLICY_DENY},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var requests atomic.Int32
			var h *harness
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request completionRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				switch requests.Add(1) {
				case 1:
					checkMemoryContext(t, request, "original note", "private A", "private B")
					var memoryName string
					for _, tool := range request.Tools {
						if strings.HasPrefix(tool.Function.Description, "Update chat notes") {
							memoryName = tool.Function.Name
						}
					}
					if memoryName == "" {
						t.Errorf("memory tool directory=%+v", request.Tools)
						w.WriteHeader(500)
						return
					}
					// 目录取得后改变管理员策略，证明执行时仍经 Core 读取生效策略。
					if scenario.policy != 0 {
						setMemoryPolicy(t, h, scenario.policy)
					}
					writeMemoryCall(w, memoryName, json.RawMessage(`{"operations":[{"op":"add","target":"memory","text":"temporary"},{"op":"patch","target":"memory","old_text":"original","text":"updated note"},{"op":"remove","target":"memory","old_text":"temporary"},{"op":"add","target":"user","text":"A preference"}]}`))
				case 2:
					expected := "{}"
					if !scenario.allow {
						expected = "error: ERROR_CODE_PERMISSION_DENIED:"
					}
					checkToolResult(t, request, "memory-call", expected)
					writeDelta(w, provider.OpenAICompatible, "first done")
					writeEnd(w, provider.OpenAICompatible)
				case 3:
					checkMemoryContext(t, request, "original note", "private B", "private A")
					writeDelta(w, provider.OpenAICompatible, "second done")
					writeEnd(w, provider.OpenAICompatible)
				case 4:
					checkMemoryContext(t, request, "updated note", "private A\nA preference", "private B")
					writeDelta(w, provider.OpenAICompatible, "reset done")
					writeEnd(w, provider.OpenAICompatible)
				default:
					t.Error("unexpected provider request")
					w.WriteHeader(500)
				}
			}))
			t.Cleanup(model.Close)
			loop := newLoop(false, 4096)
			loop.buttons = true
			h = startAgent(t, loop, agentConfig(model.URL, provider.OpenAICompatible), nil)
			db := memoryDatabase(t, h)
			seedMemory(t, db, "memory", loopSession+"@main", "main", "original note")
			seedMemory(t, db, "user", "member", "main", "private A")
			seedMemory(t, db, "user", "other", "main", "private B")
			// 触发器只计工具真正提交的写入；初始 fixture 不计入。
			for _, statement := range []string{"CREATE TABLE memory_writes(target TEXT NOT NULL)", "CREATE TRIGGER audit_memory_update AFTER UPDATE ON memory_record BEGIN INSERT INTO memory_writes VALUES(new.target); END", "CREATE TRIGGER audit_memory_insert AFTER INSERT ON memory_record BEGIN INSERT INTO memory_writes VALUES(new.target); END"} {
				if _, err := db.Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
			h.deliver(t, "/ask write memory", "member", "en-US", false)
			if scenario.policy == pb.ToolPolicy_TOOL_POLICY_CONFIRM {
				confirmation := h.confirmation(t, 0)
				if !strings.Contains(confirmation.text, agentID+"/memory") || !strings.Contains(confirmation.text, "updated note") || !strings.Contains(confirmation.text, "A preference") {
					t.Fatalf("actual parameters missing: %s", confirmation.text)
				}
				h.click(t, confirmation, scenario.allow, "member")
			}
			h.waitText(t, "first done")
			h.waitEntries(t, 2)
			wantNote, wantProfile := "original note", []string{"private A"}
			writes := 0
			if scenario.allow {
				wantNote = "updated note"
				wantProfile = append(wantProfile, "A preference")
				writes = 1
			}
			if got := storedMemory(t, db, "memory", loopSession+"@main", "main"); !reflect.DeepEqual(got, []string{wantNote}) {
				t.Fatalf("notes=%v want %s", got, wantNote)
			}
			if got := storedMemory(t, db, "user", "member", "main"); !reflect.DeepEqual(got, wantProfile) {
				t.Fatalf("profile=%v want %v", got, wantProfile)
			}
			for _, target := range []string{"memory", "user"} {
				var count int
				if err := db.QueryRow("SELECT count(*) FROM memory_writes WHERE target=?", target).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count != writes {
					t.Errorf("%s committed writes=%d want %d", target, count, writes)
				}
			}
			if scenario.full {
				h.deliver(t, "/ask second", "other", "en-US", false)
				h.waitText(t, "second done")
				h.waitEntries(t, 4)
				if entries := h.entries(t); entries[0]["content"] != "original note" || entries[3]["speaker"] != "other" {
					t.Fatalf("frozen snapshot or speaker=%v", entries)
				}
				h.deliver(t, "/reset", "member", "en-US", false)
				h.waitText(t, "Conversation cleared.")
				if entries := h.entries(t); len(entries) != 0 {
					t.Fatalf("reset retained history: %v", entries)
				}
				h.deliver(t, "/ask after reset", "member", "en-US", false)
				h.waitText(t, "reset done")
				h.waitEntries(t, 2)
				if entries := h.entries(t); entries[0]["content"] != "updated note" {
					t.Fatalf("reset snapshot=%v", entries)
				}
			}
			wantRequests := int32(2)
			if scenario.full {
				wantRequests = 4
			}
			if requests.Load() != wantRequests {
				t.Fatalf("provider requests=%d want %d", requests.Load(), wantRequests)
			}
			for _, entry := range h.entries(t) {
				if strings.Contains(entry["content"], "private") || strings.Contains(entry["content"], "A preference") {
					t.Fatalf("profile persisted in chat history: %v", entry)
				}
			}
		})
	}
}
