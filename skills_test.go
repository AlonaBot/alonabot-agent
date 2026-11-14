//go:build unix

package agent_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/AlonaBot/AlonaBot/agent/provider"
)

// TestAgentSkills 经真实 Core、SDK Session、SQLite 和技能文件验证启动诊断与工具循环。
func TestAgentSkills(t *testing.T) {
	directory := t.TempDir()
	files := map[string]string{
		"a-zeta":    "---\nname: zeta\ndescription: Zeta description\n---\nZeta body\n",
		"b-alpha":   "---\nname: alpha\ndescription: Alpha description\nextra: ignored\n---\nAlpha body\n",
		"c-no-yaml": "missing frontmatter",
		"d-no-desc": "---\nname: no-description\n---\ninvalid body",
		"e-large":   strings.Repeat("x", 256*1024+1),
	}
	for name, content := range files {
		path := filepath.Join(directory, name)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "SKILL.md"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var calls atomic.Int32 // Provider 回调与测试 goroutine 共享请求计数。
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request completionRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var found bool
		for _, tool := range request.Tools {
			if tool.Function.Name == "load_skill" {
				found = true
			}
		}
		if !found {
			t.Error("load_skill missing from provider tools")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		callSkill := func(id, name string) {
			arguments, err := json.Marshal(map[string]string{"name": name})
			if err != nil {
				t.Error(err)
				return
			}
			fmt.Fprintf(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":"load_skill","arguments":%q}}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`, id, string(arguments))
		}
		switch calls.Add(1) {
		case 1:
			want := "Available skills (use load_skill to read a skill):\nalpha: Alpha description\nzeta: Zeta description"
			if len(request.Messages) != 4 || request.Messages[0].Role != "system" || request.Messages[0].Content != "Persona prompt" || request.Messages[1].Role != "system" || request.Messages[1].Content != want || request.Messages[2].Role != "system" || request.Messages[2].Content != "" || request.Messages[3].Role != "user" {
				t.Errorf("unexpected initial prompt: %+v", request.Messages)
			}
			callSkill("load", "alpha")
		case 2:
			checkToolResult(t, request, "load", "Alpha body\n")
			if got := request.Messages[len(request.Messages)-1].Content; got != "Alpha body\n" {
				t.Errorf("skill body = %q", got)
			}
			callSkill("missing", "absent")
		case 3:
			checkToolResult(t, request, "missing", "error: NOT_FOUND: unknown skill")
			if got := request.Messages[len(request.Messages)-1].Content; got != "error: NOT_FOUND: unknown skill" {
				t.Errorf("unknown skill result = %q", got)
			}
			writeDelta(w, provider.OpenAICompatible, "skills finished")
			writeEnd(w, provider.OpenAICompatible)
		default:
			t.Error("unexpected provider request")
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(model.Close)
	configuration := agentConfig(model.URL, provider.OpenAICompatible)
	configuration["skills_dir"] = directory
	configuration["personas"] = []any{map[string]any{"name": "test", "prompt": "Persona prompt"}}
	configuration["default_persona"] = "test"
	// Adapter 未声明按钮能力，成功返回也证明加载技能无需额外确认。
	h := startAgent(t, newLoop(false, 1024), configuration, nil)
	if err := os.WriteFile(filepath.Join(directory, "b-alpha", "SKILL.md"), []byte("changed after startup"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.deliver(t, "/ask skills", "member", "en-US", false)
	h.waitText(t, "skills finished")
	h.waitEntries(t, 2)
	if calls.Load() != 3 {
		t.Errorf("provider requests = %d, want 3", calls.Load())
	}
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(h.dir, "logs", "*.log"))
	if err != nil {
		t.Fatal(err)
	}
	var diagnostics []string
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.Contains(line, "invalid skill file") {
				diagnostics = append(diagnostics, line)
			}
		}
	}
	if len(diagnostics) != 3 {
		t.Fatalf("skill diagnostics = %v, want 3", diagnostics)
	}
	for i, name := range []string{"c-no-yaml", "d-no-desc", "e-large"} {
		if !strings.Contains(diagnostics[i], "WARN") || !strings.Contains(diagnostics[i], filepath.Join(directory, name, "SKILL.md")) || !strings.Contains(diagnostics[i], "error=") {
			t.Errorf("missing warning path or reason: %s", diagnostics[i])
		}
	}
}
