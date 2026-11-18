//go:build unix

package agent_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/AlonaBot/AlonaBot/agent/provider"
	"github.com/AlonaBot/AlonaBot/sdk"
)

type modelAdmin struct {
	address string
	cookie  *http.Cookie
	csrf    string
}

const modelAdminOrigin = "http://127.0.0.1"

var modelListenerLog = regexp.MustCompile(`HTTP listener started listen=(127\.0\.0\.1:\d+)`)

func startModelAdmin(t *testing.T, configuration map[string]any, plugins ...*sdk.Plugin) (*harness, modelAdmin) {
	t.Helper()
	h := startAgent(t, newLoop(false, 1024), configuration, map[string]any{"base_url": modelAdminOrigin}, plugins...)
	// Start 返回时 Core 已同步记录由系统分配的端口。
	logs, err := filepath.Glob(filepath.Join(h.dir, "logs", "alonabot-*.log"))
	if err != nil {
		t.Fatal(err)
	}
	var admin modelAdmin
	for _, name := range logs {
		log, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if match := modelListenerLog.FindSubmatch(log); match != nil {
			admin.address = string(match[1])
			break
		}
	}
	if admin.address == "" {
		t.Fatal("Core listener address missing from startup log")
	}
	token, err := os.ReadFile(filepath.Join(h.dir, "admin_token"))
	if err != nil {
		t.Fatal(err)
	}
	response, body := admin.request(t, http.MethodPost, "/api/auth/login", fmt.Sprintf(`{"token":%q}`, strings.TrimSpace(string(token))), http.StatusOK)
	var login struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.Unmarshal(body, &login); err != nil {
		t.Fatal(err)
	}
	if len(response.Cookies()) != 1 || login.CSRF == "" {
		t.Fatal("login did not return admin cookie and CSRF token")
	}
	admin.cookie, admin.csrf = response.Cookies()[0], login.CSRF
	return h, admin
}

func (a modelAdmin) request(t *testing.T, method, path, body string, status int) (*http.Response, []byte) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), method, "http://"+a.address+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Origin", modelAdminOrigin)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if a.cookie != nil {
		request.AddCookie(a.cookie)
	}
	if a.csrf != "" {
		request.Header.Set("X-Alonabot-CSRF", a.csrf)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != status {
		t.Fatalf("%s %s: status %d, want %d: %s", method, path, response.StatusCode, status, raw)
	}
	return response, raw
}

const modelRoutePrefix = "/plugins/" + agentID + "/models/"

type modelTotals struct {
	Model        string `json:"model"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
}

func (a modelAdmin) assertUsage(t *testing.T, want ...modelTotals) {
	t.Helper()
	_, body := a.request(t, http.MethodGet, modelRoutePrefix+"usage", "", http.StatusOK)
	var usage struct{ Models []modelTotals }
	if err := json.Unmarshal(body, &usage); err != nil {
		t.Fatal(err)
	}
	if want == nil {
		want = []modelTotals{}
	}
	if !reflect.DeepEqual(usage.Models, want) {
		t.Fatalf("usage = %+v, want %+v", usage.Models, want)
	}
}

func writeModelStream(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	writeDelta(w, provider.OpenAICompatible, text)
	fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n")
}

// TestAgentModels 经 Core 管理会话发现、测试模型并读取实际对话累加的用量。
func TestAgentModels(t *testing.T) {
	var lists, completions atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("provider authentication mismatch")
			http.Error(w, "authentication mismatch", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			lists.Add(1)
			fmt.Fprint(w, `{"data":[{"id":"test-model"}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
			completions.Add(1)
			var request struct {
				Model  string
				Stream bool
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Model != "test-model" || !request.Stream {
				t.Errorf("unexpected chat request: %+v, %v", request, err)
				http.Error(w, "chat request mismatch", http.StatusBadRequest)
				return
			}
			writeModelStream(w, "Hello from the model.")
		default:
			t.Errorf("unexpected provider request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	h, admin := startModelAdmin(t, agentConfig(server.URL+"/v1", provider.OpenAICompatible))
	_, discovery := admin.request(t, http.MethodPost, modelRoutePrefix+"discover", `{"provider":"test"}`, http.StatusOK)
	var discovered struct{ Models []struct{ ID, Type string } }
	if err := json.Unmarshal(discovery, &discovered); err != nil || !reflect.DeepEqual(discovered.Models, []struct{ ID, Type string }{{"test-model", "chat"}}) {
		t.Fatalf("discover = %s, %v", discovery, err)
	}
	_, result := admin.request(t, http.MethodPost, modelRoutePrefix+"test", `{"model":"test"}`, http.StatusOK)
	var tested struct {
		OK         bool
		DurationMS *int64 `json:"duration_ms"`
	}
	if err := json.Unmarshal(result, &tested); err != nil || !tested.OK || tested.DurationMS == nil || *tested.DurationMS < 0 {
		t.Fatalf("test = %s, %v", result, err)
	}
	admin.assertUsage(t)
	h.deliver(t, "/ask hello", "member", "en-US", false)
	h.waitText(t, "Hello from the model.")
	admin.assertUsage(t, modelTotals{Model: "test", InputTokens: 7, OutputTokens: 3})
	if l, c := lists.Load(), completions.Load(); l != 1 || c != 2 {
		t.Errorf("upstream requests: lists=%d, completions=%d", l, c)
	}
}
