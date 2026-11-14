//go:build unix

package agent_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AlonaBot/AlonaBot/agent/provider"
)

type taskToolRequest struct {
	Tool      string `json:"tool"`
	Arguments any    `json:"arguments"`
	ID        string `json:"id"`
}

type taskToolReply struct{ id, content string }

type taskScript struct {
	replies    chan taskToolReply // Provider 将实际工具结果交给测试 goroutine。
	background chan completionRequest
}

func newTaskModel(t *testing.T, script *taskScript) *httptest.Server {
	t.Helper()
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request completionRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		last := request.Messages[len(request.Messages)-1]
		if last.Role == "tool" {
			script.replies <- taskToolReply{last.ToolCallID, last.Content}
			writeDelta(w, provider.OpenAICompatible, "completed/"+last.ToolCallID)
			writeEnd(w, provider.OpenAICompatible)
			return
		}
		if strings.HasPrefix(last.Content, "background ") {
			script.background <- request
			writeDelta(w, provider.OpenAICompatible, last.Content)
			writeEnd(w, provider.OpenAICompatible)
			return
		}
		var call taskToolRequest
		if err := json.Unmarshal([]byte(last.Content), &call); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		found := false
		for _, tool := range request.Tools {
			if tool.Function.Name == call.Tool {
				found = true
			}
		}
		if !found {
			t.Errorf("missing tool %q: %+v", call.Tool, request.Tools)
			w.WriteHeader(400)
			return
		}
		args, err := json.Marshal(call.Arguments)
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		event := map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": call.ID, "type": "function", "function": map[string]any{"name": call.Tool, "arguments": string(args)}}}}}}}
		raw, err := json.Marshal(event)
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		fmt.Fprintf(w, "data: %s\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", raw)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(model.Close)
	return model
}

type taskDriver struct {
	h        *harness
	script   *taskScript
	sequence int
	entries  int
}

func (d *taskDriver) call(t *testing.T, user, tool string, args any, confirmation *bool) string {
	t.Helper()
	d.sequence++
	id := fmt.Sprintf("task-call-%d", d.sequence)
	raw, err := json.Marshal(taskToolRequest{Tool: tool, Arguments: args, ID: id})
	if err != nil {
		t.Fatal(err)
	}
	after := len(d.h.loop.snapshot())
	d.h.deliver(t, "/ask "+string(raw), user, "en-US", false)
	if confirmation != nil {
		op := d.h.confirmation(t, after)
		if !strings.Contains(op.text, "agent/"+tool) {
			t.Fatalf("confirmation=%q", op.text)
		}
		d.h.click(t, op, *confirmation, user)
	}
	var result string
	select { // 工具可能等待确认，收到结果后再等前台轮次写完历史。
	case reply := <-d.script.replies:
		if reply.id != id {
			t.Fatalf("reply ID=%s want %s", reply.id, id)
		}
		result = reply.content
	case <-d.h.ctx.Done():
		t.Fatal(d.h.ctx.Err())
	}
	d.h.waitText(t, "completed/"+id)
	d.entries += 2
	d.h.waitEntries(t, d.entries)
	return result
}

func taskID(t *testing.T, result string) string {
	t.Helper()
	var task struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(result), &task); err != nil || task.ID == "" {
		t.Fatalf("schedule result=%s error=%v", result, err)
	}
	return task.ID
}

type taskListing struct {
	ID, Prompt     string
	At             *time.Time `json:"at"`
	Interval       int64      `json:"interval_seconds"`
	Cron, Timezone string
	NextAt         *time.Time `json:"next_at"`
}

func readTaskList(t *testing.T, result string) []taskListing {
	t.Helper()
	var tasks []taskListing
	if err := json.Unmarshal([]byte(result), &tasks); err != nil {
		t.Fatalf("list result=%s error=%v", result, err)
	}
	return tasks
}

func taskPermission(t *testing.T, h *harness, allow bool) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(h.dir, "state.sqlite")+"?mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("INSERT OR IGNORE INTO plugin_adapter_restriction(plugin_id) VALUES(?)", agentID); err != nil {
		t.Fatal(err)
	}
	statement := "DELETE FROM plugin_adapter_permission WHERE plugin_id=?"
	if allow {
		statement = "INSERT INTO plugin_adapter_permission(plugin_id,adapter_instance_id) VALUES(?, 'main')"
	}
	if _, err := db.Exec(statement, agentID); err != nil {
		t.Fatal(err)
	}
}

func waitTaskWarning(t *testing.T, h *harness, key string) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		files, err := filepath.Glob(filepath.Join(h.dir, "logs", "*.log"))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range files {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(string(raw), "\n") {
				if strings.Contains(line, "scheduled agent task denied") && strings.Contains(line, key) && strings.Contains(line, "WARN") && strings.Contains(line, "PERMISSION_DENIED") {
					return
				}
			}
		}
		select {
		case <-ticker.C:
		case <-h.ctx.Done():
			t.Fatal("missing scheduled task WARN")
		}
	}
}

// TestAgentTasks 经真实 Start、SDK、SQLite 与回环 Adapter 验证任务创建、派发和发信许可。
func TestAgentTasks(t *testing.T) {
	script := &taskScript{replies: make(chan taskToolReply, 1), background: make(chan completionRequest, 4)}
	model := newTaskModel(t, script)
	loop := newLoop(false, 4096)
	loop.buttons = true
	h := startAgent(t, loop, agentConfig(model.URL, provider.OpenAICompatible), nil)
	d := &taskDriver{h: h, script: script}
	allow := true
	if result := d.call(t, "member", "schedule_task", map[string]any{"prompt": "background forbidden", "interval_seconds": 60}, nil); result != "error: PERMISSION_DENIED" {
		t.Fatalf("non-admin result=%s", result)
	}
	if tasks := readTaskList(t, d.call(t, "member", "list_tasks", map[string]any{}, nil)); len(tasks) != 0 {
		t.Fatalf("non-admin created tasks: %+v", tasks)
	}
	taskPermission(t, h, false)
	deniedID := taskID(t, d.call(t, "admin", "schedule_task", map[string]any{"prompt": "background denied", "at": time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)}, &allow))
	intervalID := taskID(t, d.call(t, "admin", "schedule_task", map[string]any{"prompt": "background interval", "interval_seconds": 60}, &allow))
	cronID := taskID(t, d.call(t, "admin", "schedule_task", map[string]any{"prompt": "background cron", "cron": "0 0 1 1 *", "timezone": "Asia/Shanghai"}, &allow))
	waitTaskWarning(t, h, deniedID)
	for _, op := range loop.snapshot() {
		if op.text == "background denied" {
			t.Fatalf("sent without permission: %+v", op)
		}
	}
	taskPermission(t, h, true)
	allowedID := taskID(t, d.call(t, "admin", "schedule_task", map[string]any{"prompt": "background allowed", "at": time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)}, &allow))
	h.waitText(t, "background allowed")
	tasks := readTaskList(t, d.call(t, "other", "list_tasks", map[string]any{}, nil))
	if len(tasks) != 2 {
		t.Fatalf("tasks after one-shots=%+v", tasks)
	}
	for _, task := range tasks {
		if task.NextAt == nil || !task.NextAt.After(time.Now()) {
			t.Errorf("missing future NextAt: %+v", task)
		}
		switch task.ID {
		case intervalID:
			if task.Interval != 60 || task.Cron != "" || task.At != nil || task.Prompt != "background interval" {
				t.Errorf("interval=%+v", task)
			}
		case cronID:
			if task.Cron != "0 0 1 1 *" || task.Timezone != "Asia/Shanghai" || task.Interval != 0 || task.At != nil || task.Prompt != "background cron" {
				t.Errorf("cron=%+v", task)
			}
		default:
			t.Errorf("unexpected task: %+v (at=%s,%s)", task, deniedID, allowedID)
		}
	}
	sent := 0
	for _, op := range loop.snapshot() {
		if op.text == "background allowed" {
			sent++
			if op.to != loopSession {
				t.Errorf("task target=%s want %s", op.to, loopSession)
			}
		}
	}
	if sent != 1 {
		t.Errorf("one-shot sends=%d want 1", sent)
	}
	counts := map[string]int{}
	for range 2 {
		select {
		case prompt := <-script.background:
			counts[prompt.Messages[len(prompt.Messages)-1].Content]++
		case <-h.ctx.Done():
			t.Fatal(h.ctx.Err())
		}
	}
	if counts["background denied"] != 1 || counts["background allowed"] != 1 {
		t.Fatalf("background runs=%v", counts)
	}
	select {
	case prompt := <-script.background:
		t.Fatalf("unexpected repeated run: %+v", prompt)
	default:
	}
}
