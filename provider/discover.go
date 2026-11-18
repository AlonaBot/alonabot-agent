package provider

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// Discover 从连接配置读取模型 ID；列表端点不受支持时返回空列表和说明。
func Discover(ctx context.Context, config Config, httpClient *http.Client) ([]string, string, error) {
	if !supportedProtocol(config.Protocol) {
		return nil, "", errors.New("unsupported provider protocol")
	}
	if config.APIKey == "" && config.Protocol != OllamaChat {
		return nil, "", errors.New("provider API key is required")
	}
	baseURL, err := connectionURL(config)
	if err != nil {
		return nil, "", err
	}
	c := &Client{config: config, baseURL: baseURL, httpClient: httpClient}
	path, cursorKey := "models", ""
	switch config.Protocol {
	case AnthropicMessages:
		path, cursorKey = "v1/models", "after_id"
	case GeminiGenerateContent:
		cursorKey = "pageToken"
	case OllamaChat:
		path = "api/tags"
	}
	endpoint := baseURL.JoinPath(path)
	ids := []string{}
	seenCursors := make(map[string]bool)
	for {
		response, err := c.requestOnce(ctx, http.MethodGet, endpoint, "", nil)
		if err != nil {
			return nil, "", err
		}
		// 只有首页的端点错误表示不支持列表，后续页的同一状态仍是发现失败。
		if endpoint.RawQuery == "" {
			switch response.StatusCode {
			case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
				response.Body.Close()
				return []string{}, "Provider does not support listing models.", nil
			}
		}
		pageIDs, next, err := c.decodeModelPage(response)
		if err != nil {
			return nil, "", err
		}
		for _, id := range pageIDs {
			if id != "" && c.redact(id) == id {
				ids = append(ids, id)
			}
		}
		if next == "" {
			return ids, "", nil
		}
		if seenCursors[next] {
			return nil, "", errors.New("provider returned a repeated model list cursor")
		}
		seenCursors[next] = true
		endpoint.RawQuery = url.Values{cursorKey: {next}}.Encode()
	}
}

func (c *Client) decodeModelPage(response *http.Response) ([]string, string, error) {
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, "", c.singleStatusError(response)
	}
	var list struct {
		Data          []struct{ ID string }
		Models        []struct{ Name, Model string }
		HasMore       bool   `json:"has_more"`
		LastID        string `json:"last_id"`
		NextPageToken string `json:"nextPageToken"`
	}
	if err := c.decodeSingle(response.Body, &list); err != nil {
		return nil, "", err
	}
	var ids []string
	var next string
	switch c.config.Protocol {
	case GeminiGenerateContent, OllamaChat:
		if list.Models == nil {
			return nil, "", errors.New("provider response is missing the model list")
		}
		for _, item := range list.Models {
			id := strings.TrimPrefix(item.Name, "models/")
			if c.config.Protocol == OllamaChat {
				id = item.Model
				if id == "" {
					id = item.Name
				}
			}
			ids = append(ids, id)
		}
		if c.config.Protocol == GeminiGenerateContent {
			next = list.NextPageToken
		}
	default:
		if list.Data == nil {
			return nil, "", errors.New("provider response is missing the model list")
		}
		for _, item := range list.Data {
			ids = append(ids, item.ID)
		}
		if c.config.Protocol == AnthropicMessages && list.HasMore {
			if list.LastID == "" {
				return nil, "", errors.New("provider response is missing a model list cursor")
			}
			next = list.LastID
		}
	}
	return ids, next, nil
}
