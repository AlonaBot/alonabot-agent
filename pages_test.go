//go:build unix

package agent_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/AlonaBot/AlonaBot/agent/provider"
)

const pageRoutePrefix = "/plugins/" + agentID + "/pages/api/"

// TestAgentPages 经真实管理代理、SDK Session 和 SQLite 验证页面、人格、配置文件与会话正文主路径。
func TestAgentPages(t *testing.T) {
	requests := make(chan completionRequest, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request completionRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		requests <- request
		writeModelStream(w, "完整回复")
	}))
	t.Cleanup(server.Close)
	h, admin := startModelAdmin(t, agentConfig(server.URL, provider.OpenAICompatible))
	for _, page := range []string{"personas", "mcp", "skills", "data", "tasks"} {
		response, body := admin.request(t, "GET", "/plugins/"+agentID+"/pages/"+page, "", 200)
		if !strings.HasPrefix(response.Header.Get("Content-Type"), "text/html") || !strings.Contains(string(body), `href="kit.css"`) || !strings.Contains(string(body), `src="assets/pages.js"`) {
			t.Fatalf("invalid page %s: %s", page, body)
		}
	}
	for _, resource := range []struct{ path, content string }{{"kit.css", "text/css"}, {"kit.js", "javascript"}, {"assets/pages.css", "text/css"}, {"assets/pages.js", "javascript"}} {
		response, _ := admin.request(t, "GET", "/plugins/"+agentID+"/pages/"+resource.path, "", 200)
		if !strings.Contains(response.Header.Get("Content-Type"), resource.content) {
			t.Fatalf("resource %s has type %s", resource.path, response.Header.Get("Content-Type"))
		}
	}
	for _, locale := range []string{"en-US.json", "zh-CN.json"} {
		var messages struct {
			Pages map[string]string `json:"pages"`
		}
		if err := json.Unmarshal(agentResources.I18n[locale], &messages); err != nil {
			t.Fatal(err)
		}
		for _, page := range []string{"personas", "mcp", "skills", "data", "tasks"} {
			if messages.Pages["/pages/"+page] == "" {
				t.Fatalf("missing %s title in %s", page, locale)
			}
		}
	}
	_, packages := admin.request(t, "GET", "/api/v1/packages", "", 200)
	for _, page := range []string{"personas", "mcp", "skills", "data", "tasks"} {
		if !strings.Contains(string(packages), `"path":"/pages/`+page+`"`) {
			t.Fatalf("missing declared page %s: %s", page, packages)
		}
	}
	admin.request(t, "POST", pageRoutePrefix+"personas", `{"name":"中文 + / ?","prompt":"first system prompt"}`, 201)
	_, body := admin.request(t, "GET", "/api/v1/plugins/"+agentID+"/config/options?path=default_persona", "", 200)
	var choices struct {
		Options []struct{ Value, Label string }
	}
	if err := json.Unmarshal(body, &choices); err != nil {
		t.Fatal(err)
	}
	if len(choices.Options) != 1 || choices.Options[0].Value != "中文 + / ?" || choices.Options[0].Label != "中文 + / ?" {
		t.Fatalf("persona options: %s", body)
	}
	profile := admin.createProfile(t, "Pages", map[string]any{"default_persona": "中文 + / ?"})
	admin.assignProfile(t, "main", loopSession, profile)
	var activatedAt string
	if err := h.db.QueryRowContext(h.ctx, "SELECT changed_at FROM plugin_runtime WHERE plugin_id = ?", agentID).Scan(&activatedAt); err != nil {
		t.Fatal(err)
	}
	for index, prompt := range []string{"first system prompt", "updated system prompt"} {
		if index == 1 {
			admin.request(t, "PUT", pageRoutePrefix+"personas", `{"name":"中文 + / ?","prompt":"updated system prompt"}`, 200)
		}
		after := len(h.loop.snapshot())
		h.deliver(t, "/ask 你好 + / ?", "member", "zh-CN", false)
		h.waitTextAfter(t, after, "完整回复")
		h.waitEntries(t, (index+1)*2)
		select {
		case request := <-requests:
			if len(request.Messages) == 0 || request.Messages[0].Content != prompt {
				t.Fatalf("system prompt = %+v, want %q", request.Messages, prompt)
			}
		case <-h.ctx.Done():
			t.Fatal(h.ctx.Err())
		}
		var changedAt string
		if err := h.db.QueryRowContext(h.ctx, "SELECT changed_at FROM plugin_runtime WHERE plugin_id = ?", agentID).Scan(&changedAt); err != nil {
			t.Fatal(err)
		}
		if changedAt != activatedAt {
			t.Fatal("persona changes reloaded the agent")
		}
	}
	_, body = admin.request(t, "GET", pageRoutePrefix+"conversation?"+url.Values{"key": {loopSession + "@main"}}.Encode(), "", 200)
	var history struct {
		Entries []struct {
			Seq   uint64         `json:"seq"`
			Entry map[string]any `json:"entry"`
		}
		NextSeq uint64 `json:"next_seq"`
		More    bool
	}
	if err := json.Unmarshal(body, &history); err != nil {
		t.Fatal(err)
	}
	stored := h.readEntries(t)
	if len(history.Entries) != 5 || history.More || history.NextSeq != 0 {
		t.Fatalf("history = %s", body)
	}
	for i, entry := range history.Entries {
		if !reflect.DeepEqual(entry.Entry, stored[i]) {
			t.Fatalf("entry %d differs: %v != %v", i, entry.Entry, stored[i])
		}
		var seq uint64
		if err := h.db.QueryRowContext(h.ctx, "SELECT seq FROM conversation_entry WHERE plugin_id = ? AND conversation_key = ? ORDER BY seq LIMIT 1 OFFSET ?", agentID, loopSession+"@main", i).Scan(&seq); err != nil {
			t.Fatal(err)
		}
		if entry.Seq != seq {
			t.Fatalf("seq = %d, want %d", entry.Seq, seq)
		}
	}
}
