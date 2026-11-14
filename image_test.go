//go:build unix

package agent_test

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/AlonaBot/AlonaBot/agent/provider"
	"github.com/AlonaBot/AlonaBot/sdk/message"
)

type imageRequest struct {
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

func requestImages(t *testing.T, request imageRequest) ([]string, []string) {
	t.Helper()
	var images, texts []string
	for _, msg := range request.Messages {
		if msg.Role != "user" {
			continue
		}
		var text string
		if err := json.Unmarshal(msg.Content, &text); err == nil {
			texts = append(texts, text)
			continue
		}
		var parts []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			ImageURL struct {
				URL string `json:"url"`
			} `json:"image_url"`
		}
		if err := json.Unmarshal(msg.Content, &parts); err != nil {
			t.Fatal(err)
		}
		for _, part := range parts {
			switch part.Type {
			case "image_url":
				images = append(images, part.ImageURL.URL)
			case "text":
				text += part.Text
			}
		}
		texts = append(texts, text)
	}
	return images, texts
}

func pngFile(t *testing.T, n uint8) []byte {
	t.Helper()
	bitmap := image.NewRGBA(image.Rect(0, 0, 1, 1))
	bitmap.Set(0, 0, color.RGBA{R: n, G: 70, B: 120, A: 255})
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, bitmap); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func incomingImage(file string) message.Segment {
	return message.Image(message.MediaSource{Incoming: &message.IncomingMedia{Token: file, MIME: "image/png", Readability: message.Readable}})
}

func imageDatabase(t *testing.T, h *harness) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(h.dir, "plugins", agentID, "dev.sqlite")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func waitImageState(t *testing.T, h *harness, db *sql.DB, count uint64, numbers []uint64) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var storedCount uint64
		if err := db.QueryRowContext(h.ctx, "SELECT image_count FROM chat_settings WHERE session=?", string(loopSession)+"@main").Scan(&storedCount); err != nil {
			t.Fatal(err)
		}
		rows, err := db.QueryContext(h.ctx, "SELECT number FROM conversation_images WHERE session=? ORDER BY number", string(loopSession)+"@main")
		if err != nil {
			t.Fatal(err)
		}
		var storedNumbers []uint64
		for rows.Next() {
			var number uint64
			if err := rows.Scan(&number); err != nil {
				t.Fatal(err)
			}
			storedNumbers = append(storedNumbers, number)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		if storedCount == count && reflect.DeepEqual(storedNumbers, numbers) {
			return
		}
		select {
		case <-ticker.C:
		case <-h.ctx.Done():
			t.Fatalf("image count/numbers=%d/%v, want %d/%v", storedCount, storedNumbers, count, numbers)
		}
	}
}

// TestAgentImages 经真实入站媒体授权、SDK 取件与两处 SQLite 验证图片回放及 reset。
func TestAgentImages(t *testing.T) {
	requests := make(chan imageRequest, 6)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request imageRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		requests <- request
		w.Header().Set("Content-Type", "text/event-stream")
		writeDelta(w, provider.OpenAI, "seen")
		writeEnd(w, provider.OpenAI)
	}))
	t.Cleanup(server.Close)
	loop := newLoop(false, 64)
	loop.files = make(map[string][]byte)
	var urls []string
	for i := 1; i <= 5; i++ {
		data := pngFile(t, uint8(i))
		loop.files[fmt.Sprintf("platform-image-%d", i)] = data
		urls = append(urls, "data:image/png;base64,"+base64.StdEncoding.EncodeToString(data))
	}
	configuration := agentConfig(server.URL, provider.OpenAI)
	configuration["providers"].([]any)[0].(map[string]any)["custom"].(map[string]any)["image_input"] = true
	h := startAgent(t, loop, configuration, nil)
	db := imageDatabase(t, h)
	h.deliver(t, "/agent on", "admin", "en-US", false)
	h.waitText(t, "Free conversation is on.")
	// 每轮等两库完成，再交付下一轮，验证批边界引起的请求前缀变化。
	for i, wantCount := range []int{1, 2, 3, 4, 2} {
		n := i + 1
		h.deliverChain(t, message.Chain{incomingImage(fmt.Sprintf("platform-image-%d", n)), message.Text(fmt.Sprintf("picture %d", n))}, fmt.Sprintf("member-%d", n), "en-US", true)
		h.waitEntries(t, n*2)
		first := ((n - 1) / 3) * 3
		var numbers []uint64
		for number := first + 1; number <= n; number++ {
			numbers = append(numbers, uint64(number))
		}
		waitImageState(t, h, db, uint64(n), numbers)
		var request imageRequest
		select {
		case request = <-requests:
		case <-h.ctx.Done():
			t.Fatal(h.ctx.Err())
		}
		images, texts := requestImages(t, request)
		if len(images) != wantCount {
			t.Fatalf("round %d images=%d want %d", n, len(images), wantCount)
		}
		if n == 5 {
			if !reflect.DeepEqual(images, urls[3:]) {
				t.Fatalf("fifth round images=%v want %v", images, urls[3:])
			}
			for j := range 3 {
				if texts[j] != fmt.Sprintf("[image]\npicture %d", j+1) {
					t.Fatalf("expired image text=%q", texts[j])
				}
			}
		}
	}
	entries := h.readEntries(t)
	for i := range 5 {
		entry := entries[1+i*2]
		if len(entry) != 4 || entry["role"] != "user" || entry["content"] != fmt.Sprintf("picture %d", i+1) || entry["speaker"] != fmt.Sprintf("member-%d", i+1) || !reflect.DeepEqual(entry["images"], []any{float64(i + 1)}) {
			t.Fatalf("user entry=%v", entry)
		}
	}
	for _, number := range []int{4, 5} {
		var mime string
		var data []byte
		var seq uint64
		if err := db.QueryRowContext(h.ctx, "SELECT mime, bytes, seq FROM conversation_images WHERE session=? AND number=?", string(loopSession)+"@main", number).Scan(&mime, &data, &seq); err != nil {
			t.Fatal(err)
		}
		if mime != "image/png" || !bytes.Equal(data, loop.files[fmt.Sprintf("platform-image-%d", number)]) || seq != uint64(number*2) {
			t.Fatalf("stored image %d MIME=%s seq=%d bytes=%v", number, mime, seq, data)
		}
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	for i, url := range urls {
		if strings.Contains(string(raw), strings.TrimPrefix(url, "data:image/png;base64,")) || strings.Contains(string(raw), fmt.Sprintf("platform-image-%d", i+1)) || bytes.Contains(raw, loop.files[fmt.Sprintf("platform-image-%d", i+1)]) {
			t.Fatalf("media bytes or platform token leaked into Core history: %s", raw)
		}
	}
	h.deliver(t, "/reset", "member-5", "en-US", false)
	h.waitText(t, "Conversation cleared.")
	if entries := h.readEntries(t); len(entries) != 0 {
		t.Fatalf("history after reset=%v", entries)
	}
	waitImageState(t, h, db, 0, nil)
	h.deliverChain(t, message.Chain{incomingImage("platform-image-1"), message.Text("after reset")}, "member-1", "en-US", true)
	h.waitEntries(t, 2)
	waitImageState(t, h, db, 1, []uint64{1})
	images, _ := requestImages(t, <-requests)
	if !reflect.DeepEqual(images, urls[:1]) || !reflect.DeepEqual(h.readEntries(t)[1]["images"], []any{float64(1)}) {
		t.Fatalf("image numbering or replay after reset=%v", images)
	}
}
