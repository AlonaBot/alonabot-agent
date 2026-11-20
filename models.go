package agent

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/AlonaBot/AlonaBot/sdk/gormdb"
	"github.com/AlonaBot/alonabot-agent/provider"
)

//go:embed model_test.wav
var modelTestWAV []byte

type modelDiscoveryRequest struct {
	Provider string            `json:"provider"`
	Protocol provider.Protocol `json:"protocol"`
	BaseURL  string            `json:"base_url"`
	APIKey   string            `json:"api_key"`
}

type discoveredModel struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

type modelDiscoveryResponse struct {
	Models      []discoveredModel `json:"models"`
	Description string            `json:"description,omitempty"`
}

type modelTestResponse struct {
	OK          bool   `json:"ok"`
	DurationMS  int64  `json:"duration_ms"`
	Description string `json:"description,omitempty"`
}

func (a *component) discoverModels(w http.ResponseWriter, r *http.Request) {
	var request modelDiscoveryRequest
	if !decodeModelRequest(w, r, &request) {
		return
	}
	connection := provider.Config{Protocol: request.Protocol, BaseURL: request.BaseURL, APIKey: request.APIKey}
	if request.Provider != "" {
		if request.Protocol != "" || request.BaseURL != "" || request.APIKey != "" {
			writeModelError(w, http.StatusBadRequest, "Specify a provider name or a connection, not both.")
			return
		}
		selected := a.config.provider(request.Provider)
		if selected == nil {
			writeModelError(w, http.StatusNotFound, "Unknown configured provider.")
			return
		}
		connection = selected.connection
	} else {
		switch connection.Protocol {
		case provider.OpenAI, provider.OpenAICompatible, provider.OpenAIResponses, provider.AnthropicMessages, provider.GeminiGenerateContent, provider.OllamaChat:
		default:
			writeModelError(w, http.StatusBadRequest, "Provide a supported provider protocol.")
			return
		}
		if !validProviderBaseURL(connection.BaseURL) || connection.APIKey == "" && connection.Protocol != provider.OllamaChat {
			writeModelError(w, http.StatusBadRequest, "Provide an HTTP(S) base URL without credentials, query or fragment, and the required API key.")
			return
		}
	}
	ids, description, err := provider.Discover(r.Context(), connection, http.DefaultClient)
	if err != nil {
		writeModelError(w, http.StatusBadGateway, modelProbeError(err, "Model discovery"))
		return
	}
	response := modelDiscoveryResponse{Models: make([]discoveredModel, 0, len(ids)), Description: description}
	for _, id := range ids {
		response.Models = append(response.Models, discoveredModel{ID: id, Type: inferModelType(id)})
	}
	writeModelJSON(w, http.StatusOK, response)
}

func inferModelType(id string) string {
	id = strings.ToLower(id)
	switch {
	case strings.Contains(id, "embed"):
		return modelEmbedding
	case strings.Contains(id, "rerank"):
		return modelRerank
	case strings.Contains(id, "whisper"), strings.Contains(id, "transcribe"):
		return modelSpeechToText
	case strings.Contains(id, "tts"):
		return modelTextToSpeech
	default:
		return modelChat
	}
}

func (a *component) testModel(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Model string `json:"model"`
	}
	if !decodeModelRequest(w, r, &request) {
		return
	}
	if request.Model == "" {
		writeModelError(w, http.StatusBadRequest, "Provide a configured model name.")
		return
	}
	selected := a.config.model(request.Model)
	if selected == nil {
		writeModelError(w, http.StatusNotFound, "Unknown configured model.")
		return
	}
	started := time.Now()
	err := probeModel(r.Context(), selected)
	response := modelTestResponse{OK: err == nil, DurationMS: time.Since(started).Milliseconds()}
	if err != nil {
		response.Description = modelProbeError(err, "Model test")
	}
	writeModelJSON(w, http.StatusOK, response)
}

func probeModel(ctx context.Context, selected *configuredModel) error {
	switch selected.modelType {
	case modelChat:
		// 重用各 chat 协议的结束判断，但探测只请求当前端点一次。
		connection := selected.provider.connection
		connection.Model = selected.model
		client, err := provider.New(connection, &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		})
		if err != nil {
			return err
		}
		_, err = client.Stream(ctx, provider.Request{
			MaxTokens: selected.maxTokens,
			Messages:  []provider.Message{{Role: roleUser, Content: "Say hello."}},
		}, func(provider.Event) error { return nil })
		return err
	case modelSpeechToText:
		_, err := selected.client.Transcribe(ctx, modelTestWAV)
		return err
	case modelTextToSpeech:
		_, _, err := selected.client.Synthesize(ctx, "Hello there.")
		return err
	case modelEmbedding:
		_, err := selected.client.Embed(ctx, "hello")
		return err
	case modelRerank:
		_, err := selected.client.Rerank(ctx, "weather", []string{"It is sunny today.", "The book is on the table."})
		return err
	case modelDecision:
		_, err := selected.client.Decide(ctx, "Is the sky usually blue on a clear day?")
		return err
	default:
		return errors.New("unsupported model type")
	}
}

func (a *component) modelsUsage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	db, err := gormdb.Open(ctx)
	if err == nil {
		var rows []modelUsage
		rows, err = readUsage(ctx, db, time.Now())
		if err == nil {
			writeModelJSON(w, http.StatusOK, struct {
				Models []modelUsage `json:"models"`
			}{Models: rows})
			return
		}
	}
	slog.ErrorContext(ctx, "model usage read failed", "error", err)
	writeModelError(w, http.StatusInternalServerError, "Could not read model usage.")
}

// modelProbeError 只回传固定说明，Provider 的错误文本可能包含凭证或请求内容。
func modelProbeError(err error, operation string) string {
	switch providerErrorKind(err) {
	case "canceled":
		return operation + " was canceled."
	case "timeout":
		return operation + " timed out."
	case "network_error":
		return operation + " failed to reach the provider."
	default:
		return operation + " failed. Check the provider connection and model settings."
	}
}

func decodeModelRequest(w http.ResponseWriter, r *http.Request, value any) bool {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err == nil && decoder.Decode(new(json.RawMessage)) == io.EOF {
		return true
	}
	writeModelError(w, http.StatusBadRequest, "Invalid model request JSON.")
	return false
}

func writeModelError(w http.ResponseWriter, status int, description string) {
	writeModelJSON(w, status, struct {
		Description string `json:"description"`
	}{Description: description})
}

func writeModelJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Debug("write model response failed", "error", err)
	}
}
