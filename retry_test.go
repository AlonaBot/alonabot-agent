//go:build unix

package agent_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"github.com/AlonaBot/AlonaBot/agent/provider"
)

// TestAgentRetry 验证 /ask 在主模型三次失败后回退，并只追加一组会话条目。
func TestAgentRetry(t *testing.T) {
	var lock sync.Mutex
	var models []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct{ Model string }
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		lock.Lock()
		models = append(models, request.Model)
		lock.Unlock()
		if request.Model == "test-model" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeDelta(w, provider.OpenAICompatible, "fallback answer")
		writeEnd(w, provider.OpenAICompatible)
	}))
	t.Cleanup(server.Close)
	configuration := agentConfig(server.URL, provider.OpenAICompatible)
	configuration["models"] = append(configuration["models"].([]any), map[string]any{"name": "fallback", "provider": "test", "model": "fallback-model"})
	configuration["max_attempts"] = 3
	configuration["fallback_chat_models"] = []string{"fallback"}
	h := startAgent(t, newLoop(false, 64), configuration, nil)
	h.deliver(t, "/ask retry me", "member", "en-US", false)
	h.waitText(t, "fallback answer")
	entries := h.waitEntries(t, 2)
	if entries[0]["role"] != "user" || entries[0]["content"] != "retry me" || entries[1]["role"] != "assistant" || entries[1]["content"] != "fallback answer" {
		t.Fatalf("unexpected conversation entries: %v", entries)
	}
	lock.Lock()
	defer lock.Unlock()
	if want := []string{"test-model", "test-model", "test-model", "fallback-model"}; !reflect.DeepEqual(models, want) {
		t.Fatalf("requested models = %v, want %v", models, want)
	}
}
