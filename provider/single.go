package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// Transcribe 将 WAV 音频转写为文本。
func (c *Client) Transcribe(ctx context.Context, audio []byte) (string, error) {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("model", c.config.Model); err != nil {
		return "", err
	}
	file, err := form.CreateFormFile("file", "audio.wav")
	if err != nil {
		return "", err
	}
	if _, err := file.Write(audio); err != nil {
		return "", err
	}
	if err := form.Close(); err != nil {
		return "", err
	}
	var result struct{ Text *string }
	if err := c.singleJSON(ctx, "audio/transcriptions", form.FormDataContentType(), &body, &result); err != nil {
		return "", err
	}
	if result.Text == nil {
		return "", errors.New("provider response is missing transcription text")
	}
	return *result.Text, nil
}

// Synthesize 将文本转成音频，返回原始字节和响应的媒体类型。
func (c *Client) Synthesize(ctx context.Context, text string) ([]byte, string, error) {
	const testVoice = "alloy" // OpenAI 的语音请求要求 voice；接口固定使用一种声音。
	request := map[string]string{"model": c.config.Model, "input": text, "voice": testVoice}
	response, err := c.singleRequest(ctx, "audio/speech", "application/json", jsonBody(request))
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	audio, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read provider audio: %w", c.safeRequestError(err))
	}
	mime := response.Header.Get("Content-Type")
	if len(audio) == 0 || !strings.HasPrefix(mime, "audio/") && mime != "application/octet-stream" {
		return nil, "", errors.New("provider response is missing audio")
	}
	return audio, mime, nil
}

// Embed 返回一段文本的嵌入向量。
func (c *Client) Embed(ctx context.Context, text string) ([]float64, error) {
	request := map[string]string{"model": c.config.Model, "input": text, "encoding_format": "float"}
	var result struct {
		Data []struct{ Embedding []float64 }
	}
	if err := c.singleJSON(ctx, "embeddings", "application/json", jsonBody(request), &result); err != nil {
		return nil, err
	}
	if len(result.Data) != 1 || len(result.Data[0].Embedding) == 0 {
		return nil, errors.New("provider response is missing the embedding vector")
	}
	return result.Data[0].Embedding, nil
}

// RankedText 保存文本在原列表中的序号和相关性分数。
type RankedText struct {
	Index int
	Score float64
}

// Rerank 返回 Provider 按相关性排列的文本序号与分数。
func (c *Client) Rerank(ctx context.Context, query string, texts []string) ([]RankedText, error) {
	request := map[string]any{"model": c.config.Model, "query": query, "documents": texts}
	var result struct {
		Results []struct {
			Index *int     `json:"index"`
			Score *float64 `json:"relevance_score"`
		}
	}
	if err := c.singleJSON(ctx, "rerank", "application/json", jsonBody(request), &result); err != nil {
		return nil, err
	}
	if result.Results == nil || len(result.Results) != len(texts) {
		return nil, errors.New("provider response must include a rerank result for each document")
	}
	seen := make([]bool, len(texts))
	ranked := make([]RankedText, 0, len(result.Results))
	for _, item := range result.Results {
		if item.Index == nil || *item.Index < 0 || *item.Index >= len(texts) || seen[*item.Index] || item.Score == nil {
			return nil, errors.New("provider returned an invalid rerank result")
		}
		seen[*item.Index] = true
		ranked = append(ranked, RankedText{Index: *item.Index, Score: *item.Score})
	}
	return ranked, nil
}

// Decide 返回布尔问题的肯定概率，范围为 0 到 1（含端点）。
func (c *Client) Decide(ctx context.Context, prompt string) (float64, error) {
	request := map[string]any{
		"model": c.config.Model,
		"messages": []map[string]string{
			{"role": roleSystem, "content": `Return a JSON object with one number, "probability": the probability that the answer is yes, between 0 and 1.`},
			{"role": roleUser, "content": prompt},
		},
		"response_format": map[string]string{"type": "json_object"},
	}
	var result struct {
		Choices []struct{ Message struct{ Content string } }
	}
	if err := c.singleJSON(ctx, "chat/completions", "application/json", jsonBody(request), &result); err != nil {
		return 0, err
	}
	var decision struct{ Probability *float64 }
	if len(result.Choices) == 0 || json.Unmarshal([]byte(result.Choices[0].Message.Content), &decision) != nil ||
		decision.Probability == nil || *decision.Probability < 0 || *decision.Probability > 1 {
		return 0, errors.New("provider decision must be a JSON object with a probability between 0 and 1")
	}
	return *decision.Probability, nil
}

func jsonBody(value any) *bytes.Reader {
	// 请求只包含可直接编码为 JSON 的字符串、切片和对象。
	data, _ := json.Marshal(value)
	return bytes.NewReader(data)
}

func (c *Client) singleJSON(ctx context.Context, path, contentType string, body io.Reader, result any) error {
	response, err := c.singleRequest(ctx, path, contentType, body)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	return c.decodeSingle(response.Body, result)
}

func (c *Client) decodeSingle(body io.Reader, result any) error {
	var payload json.RawMessage
	decoder := json.NewDecoder(body)
	if err := decoder.Decode(&payload); err != nil {
		return fmt.Errorf("decode provider response: %w", c.safeRequestError(err))
	}
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		if err == nil {
			return errors.New("provider response contains more than one JSON value")
		}
		return fmt.Errorf("decode provider response after JSON value: %w", c.safeRequestError(err))
	}
	var envelope struct{ Error json.RawMessage }
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return fmt.Errorf("decode provider response: %w", c.safeRequestError(err))
	}
	if raw := envelope.Error; len(raw) > 0 && string(raw) != "null" {
		if message := providerErrorMessage(raw); message != "" {
			return fmt.Errorf("provider reported an error: %s", c.redact(message))
		}
		return errors.New("provider reported an error")
	}
	if err := json.Unmarshal(payload, result); err != nil {
		return fmt.Errorf("decode provider response: %w", c.safeRequestError(err))
	}
	return nil
}

func (c *Client) singleRequest(ctx context.Context, path, contentType string, body io.Reader) (*http.Response, error) {
	if c.config.Protocol != OpenAI && c.config.Protocol != OpenAICompatible {
		return nil, errors.New("model type requires openai or openai-compatible protocol")
	}
	response, err := c.requestOnce(ctx, http.MethodPost, c.baseURL.JoinPath(path), contentType, body)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		return nil, c.singleStatusError(response)
	}
	return response, nil
}

func (c *Client) requestOnce(ctx context.Context, method string, endpoint *url.URL, contentType string, body io.Reader) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return nil, fmt.Errorf("build provider request: %w", c.safeRequestError(err))
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	switch c.config.Protocol {
	case AnthropicMessages:
		request.Header.Set("x-api-key", c.config.APIKey)
		request.Header.Set("anthropic-version", anthropicVersion)
	case GeminiGenerateContent:
		request.Header.Set("x-goog-api-key", c.config.APIKey)
	default:
		if c.config.APIKey != "" {
			request.Header.Set("Authorization", "Bearer "+c.config.APIKey)
		}
	}
	client := *c.httpClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("send provider request: %w", c.safeRequestError(err))
	}
	return response, nil
}

func (c *Client) singleStatusError(response *http.Response) error {
	var body struct{ Error json.RawMessage }
	_ = json.NewDecoder(response.Body).Decode(&body) // 非 JSON 响应仍保留 HTTP 状态。
	message := providerErrorMessage(body.Error)
	if message == "" {
		return fmt.Errorf("provider returned HTTP status %d", response.StatusCode)
	}
	return fmt.Errorf("provider returned HTTP status %d: %s", response.StatusCode, c.redact(message))
}

var credentialText = regexp.MustCompile(`(?i)\b(?:authorization|proxy-authorization|x-api-key|x-goog-api-key)\b["']?\s*[:=][^\r\n]*|\b(?:bearer|basic)\s+[^\s,"']+|https?://[^\s"'<>]+`)

// 先匹配带 userinfo 的 URI，避免相邻文本被前一段普通 URL 的匹配吞入。
var credentialURI = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*:|^|[\s"'(<\[,{=])//[^/?#@\s"<>]*@[^\s"'<>]*`)

func (c *Client) redact(message string) string {
	message = credentialURI.ReplaceAllStringFunc(message, func(value string) string {
		prefix, _, _ := strings.Cut(value, "//")
		if len(prefix) == 1 {
			return prefix + redactedAPIKey
		}
		return redactedAPIKey
	})
	message = credentialText.ReplaceAllStringFunc(message, func(value string) string {
		address, err := url.Parse(value)
		if err == nil && address.Host != "" && address.User == nil && address.RawQuery == "" && address.Fragment == "" {
			return value
		}
		return redactedAPIKey
	})
	if c.config.APIKey != "" {
		message = strings.ReplaceAll(message, c.config.APIKey, redactedAPIKey)
	}
	return message
}

type sanitizedRequestError struct {
	cause   error
	message string
}

func (e sanitizedRequestError) Error() string { return e.message }
func (e sanitizedRequestError) Unwrap() error { return e.cause }

func (c *Client) safeRequestError(err error) error {
	return sanitizedRequestError{cause: err, message: c.redact(err.Error())}
}
