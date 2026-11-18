//go:build unix

package agent_test

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlonaBot/AlonaBot/agent/provider"
	"github.com/AlonaBot/AlonaBot/sdk/message"
)

// TestAgentTriggersAndMemberSessions 经真实会话和两处 SQLite 验证唤醒词、成员历史与图片隔离。
func TestAgentTriggersAndMemberSessions(t *testing.T) {
	requests := make(chan imageRequest, 6)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request imageRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		calls.Add(1)
		requests <- request
		w.Header().Set("Content-Type", "text/event-stream")
		writeDelta(w, provider.OpenAI, "seen")
		writeEnd(w, provider.OpenAI)
	}))
	t.Cleanup(server.Close)
	loop := newLoop(false, 64)
	loop.files = map[string][]byte{"a": pngFile(t, 1), "b": pngFile(t, 2)}
	configuration := agentConfig(server.URL, provider.OpenAI)
	configuration["models"].([]any)[0].(map[string]any)["image_input"] = true
	configuration["triggers"] = []string{"wake_word"}
	configuration["wake_words"] = []string{"Alona"}
	configuration["member_sessions"] = true
	h := startAgent(t, loop, configuration, nil)
	db := imageDatabase(t, h)
	key := func(user string) string { return loopSession + "@main@" + loopID + ":" + user }
	texts := make(map[string][]string)
	for round, user := range []string{"a", "b", "a", "b", "a", "b"} {
		if round == 4 {
			h.deliver(t, "/reset", "a", "en-US", false)
			h.waitText(t, "Conversation cleared.")
			if entries := h.readEntriesKey(t, key("a")); len(entries) != 0 {
				t.Fatalf("reset member history=%v", entries)
			}
			var count uint64
			if err := db.QueryRowContext(h.ctx, "SELECT image_count FROM chat_settings WHERE session=?", key("a")).Scan(&count); err != nil || count != 0 {
				t.Fatalf("reset member image count=%d: %v", count, err)
			}
			var rows int
			if err := db.QueryRowContext(h.ctx, "SELECT count(*) FROM conversation_images WHERE session=?", key("a")).Scan(&rows); err != nil || rows != 0 {
				t.Fatalf("reset member image rows=%d: %v", rows, err)
			}
			texts["a"] = nil
			if entries := h.readEntriesKey(t, key("b")); len(entries) != 5 {
				t.Fatalf("other member history after reset=%v", entries)
			}
		}
		text := fmt.Sprintf("Alona %s round %d", user, round+1)
		texts[user] = append(texts[user], text)
		h.deliverChain(t, message.Chain{message.Text(text), incomingImage(user)}, user, "en-US", false)
		// Core 历史与受管图片分别提交，等两处持久化后再交付下一轮。
		ticker := time.NewTicker(10 * time.Millisecond)
		for {
			var count uint64
			err := db.QueryRowContext(h.ctx, "SELECT image_count FROM chat_settings WHERE session=?", key(user)).Scan(&count)
			if err != nil && err != sql.ErrNoRows {
				t.Fatal(err)
			}
			if count == uint64(len(texts[user])) && len(h.readEntriesKey(t, key(user))) == 1+2*len(texts[user]) {
				break
			}
			select {
			case <-ticker.C:
			case <-h.ctx.Done():
				t.Fatal(h.ctx.Err())
			}
		}
		ticker.Stop()
		var request imageRequest
		select {
		case request = <-requests:
		case <-h.ctx.Done():
			t.Fatal(h.ctx.Err())
		}
		images, gotTexts := requestImages(t, request)
		if !reflect.DeepEqual(gotTexts, texts[user]) || len(images) != len(texts[user]) {
			t.Fatalf("member %s model texts/images=%q/%v, want %q", user, gotTexts, images, texts[user])
		}
		url := "data:image/png;base64," + base64.StdEncoding.EncodeToString(loop.files[user])
		for _, image := range images {
			if image != url {
				t.Fatalf("member %s received another member's image: %s", user, image)
			}
		}
		for member, expected := range texts {
			entries := h.readEntriesKey(t, key(member))
			if len(entries) != 1+2*len(expected) || entries[0]["kind"] != "memory_snapshot" {
				t.Fatalf("member %s entries=%v", member, entries)
			}
			for i, text := range expected {
				entry := entries[1+i*2]
				if entry["content"] != text || entry["speaker"] != member || !reflect.DeepEqual(entry["images"], []any{float64(i + 1)}) {
					t.Fatalf("member %s history entry=%v", member, entry)
				}
				var data []byte
				if err := db.QueryRowContext(h.ctx, "SELECT bytes FROM conversation_images WHERE session=? AND number=?", key(member), i+1).Scan(&data); err != nil || !bytes.Equal(data, loop.files[member]) {
					t.Fatalf("member %s stored image %d=%v: %v", member, i+1, data, err)
				}
			}
		}
		if got := calls.Load(); got != int32(round+1) {
			t.Fatalf("provider calls=%d, want %d", got, round+1)
		}
	}
}
