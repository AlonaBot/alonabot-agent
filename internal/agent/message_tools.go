package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/AlonaBot/AlonaBot/sdk"
	"github.com/AlonaBot/AlonaBot/sdk/errcode"
	"github.com/AlonaBot/AlonaBot/sdk/message"
	"github.com/AlonaBot/alonabot-agent/provider"
)

const (
	sendMessageToolName   = "send_message"
	messageFormatToolName = "get_message_format"
	messageFormatPageSize = 20 // 格式发现按用途分页，每页最多 20 个声明。
	sendMessageOperation  = "agent/send_message"
)

// messageRound 捕获一轮的目标、声明和附件；交互发送始终保留原 Context 的父调用。
type messageRound struct {
	context     *sdk.Context
	target      taskTarget
	scheduled   bool
	description *message.OutboundDescription
	attachments map[string]message.MediaSource
	history     []conversationEntry // 按实际投递顺序追加，富消息仅保存文字摘要与回执。
}

func newMessageRound(c *sdk.Context, target taskTarget, scheduled bool, attachments map[string]message.MediaSource) (*messageRound, error) {
	r := &messageRound{context: c, target: target, scheduled: scheduled, attachments: attachments}
	var err error
	if scheduled {
		r.description, err = sdk.DescribeMessage(c, target.session(), sdk.Via(target.Instance))
	} else {
		r.description, err = c.DescribeMessage(target.session(), sdk.Via(target.Instance))
	}
	return r, err
}

func (r *messageRound) addTools(catalog *toolCatalog) {
	for _, tool := range []roundTool{
		{local: r.sendMessage, definition: provider.Tool{Name: sendMessageToolName, Description: r.sendDescription(), InputSchema: r.sendSchema()}},
		{local: r.getMessageFormat, definition: provider.Tool{Name: messageFormatToolName,
			Description: "Use type to get complete fields, limits, source options and a directly sendable example; add combine_with to inspect combination rules. Use query only to discover platform extensions by purpose, then request each chosen type. Follow next_cursor for more results.",
			InputSchema: json.RawMessage(`{"type":"object","oneOf":[{"properties":{"type":{"type":"string","minLength":1},"combine_with":{"type":"array","items":{"type":"string"}}},"required":["type"],"additionalProperties":false},{"properties":{"query":{"type":"string"},"cursor":{"type":"string"}},"required":["query"],"additionalProperties":false}]}`)}},
	} {
		catalog.byName[tool.definition.Name] = tool
		catalog.definitions = append(catalog.definitions, tool.definition)
	}
}

func (r *messageRound) sendDescription() string {
	var out strings.Builder
	out.WriteString("Send one logical rich message to the current target. For plain text, write directly in the response body; do not call this tool or wrap plain replies in text parts. text/markdown parts are only for arranging text alongside other formats. Do not repeat content already sent by send_message in the response body. If unsure about combinations, call get_message_format first. Resend only parts explicitly marked resend_safe; when status is unknown, stop resending and tell the user the delivery result is unknown. No automatic send retry occurs. Standard format status for this target:")
	for _, format := range r.description.Formats {
		fmt.Fprintf(&out, " %s=%s;", format.Name, format.Status)
	}
	out.WriteString(" Request recursive forward fields, combination details and platform extension formats through get_message_format. Sources use exactly one of https_url, media_ref, attachment (this round's labeled path)")
	if !r.scheduled && r.context.Has(sdk.Admin) && r.context.User() != "" {
		out.WriteString(", local_path (absolute path, requires confirmation before reading)")
	}
	out.WriteString(". The tool returns all physical delivery statuses and confirmed message_refs.")
	return out.String()
}

func (r *messageRound) sendSchema() json.RawMessage {
	// 仅列常见字段，递归与扩展的完整 Schema 由详情工具按需给出。
	sources := map[string]any{}
	for _, name := range []string{"https_url", "media_ref", "attachment"} {
		sources[name] = map[string]any{"type": "string", "minLength": 1}
	}
	if !r.scheduled && r.context.Has(sdk.Admin) && r.context.User() != "" {
		sources["local_path"] = map[string]any{"type": "string", "minLength": 1}
	}
	var alternatives []any
	for _, name := range []string{"https_url", "media_ref", "attachment", "local_path"} {
		if _, exists := sources[name]; exists {
			alternatives = append(alternatives, map[string]any{"required": []string{name}})
		}
	}
	properties := map[string]any{
		"type": map[string]any{"type": "string", "description": "Standard: text, markdown, image, video, voice, file, mention, quote, forward, buttons; or a declared format discovered with get_message_format."},
		"text": map[string]any{"type": "string"}, "entities": map[string]any{"type": "array", "items": map[string]any{"type": "object"}, "description": "UTF-16 entities; get text format for complete fields."},
		"content": map[string]any{"type": "string"},
		"source":  map[string]any{"type": "object", "properties": sources, "oneOf": alternatives, "additionalProperties": false},
		"caption": map[string]any{"type": "string"}, "filename": map[string]any{"type": "string"}, "mime": map[string]any{"type": "string"},
		"user_id": map[string]any{"type": "string"}, "display": map[string]any{"type": "string"}, "message_ref": map[string]any{"type": "string"},
		"rows":    map[string]any{"type": "array", "items": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"label": map[string]any{"type": "string"}, "url": map[string]any{"type": "string"}}, "required": []string{"label"}, "additionalProperties": false}}},
		"nodes":   map[string]any{"type": "array", "items": map[string]any{"type": "object"}, "description": "Get forward format for recursive node fields."},
		"version": map[string]any{"type": "integer", "minimum": 1}, "payload": map[string]any{},
	}
	for _, format := range message.StandardFormats() {
		if format.Name == "text" {
			var schema struct {
				Properties map[string]json.RawMessage `json:"properties"`
			}
			if err := json.Unmarshal(format.SchemaJSON, &schema); err != nil {
				panic(err)
			} // SDK 固定标准声明。
			properties["entities"] = schema.Properties["entities"]
		}
	}
	encoded, _ := json.Marshal(map[string]any{"type": "object", "properties": map[string]any{"parts": map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "object", "properties": properties, "required": []string{"type"}, "additionalProperties": false}}}, "required": []string{"parts"}, "additionalProperties": false})
	return encoded
}

func decodeMessageArguments(raw json.RawMessage, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return errcode.InvalidArgument.New(err.Error())
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errcode.InvalidArgument.New("expected one JSON object")
	}
	return nil
}

func messageJSON(value any) string {
	encoded, err := json.Marshal(value)
	return toolResult(string(encoded), err)
}

func (r *messageRound) getMessageFormat(arguments json.RawMessage) string {
	var input struct {
		Type        *string   `json:"type"`
		CombineWith *[]string `json:"combine_with"`
		Query       *string   `json:"query"`
		Cursor      *string   `json:"cursor"`
	}
	if err := decodeMessageArguments(arguments, &input); err != nil {
		return toolResult("", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(arguments, &fields); err != nil {
		return toolResult("", errcode.InvalidArgument.New(err.Error()))
	}
	_, hasType := fields["type"]
	_, hasQuery := fields["query"]
	_, hasCursor := fields["cursor"]
	_, hasCombination := fields["combine_with"]
	for _, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return toolResult("", errcode.InvalidArgument.New("format arguments must be strings or arrays, not null"))
		}
	}
	if hasType == hasQuery || hasType && hasCursor || hasQuery && hasCombination {
		return toolResult("", errcode.InvalidArgument.New("choose type/combine_with or query/cursor"))
	}
	if input.Query != nil {
		matches := []message.MessageFormat{}
		query := strings.ToLower(*input.Query)
		for _, format := range r.description.Extensions {
			if strings.Contains(strings.ToLower(format.Name), query) || strings.Contains(strings.ToLower(format.Description), query) {
				matches = append(matches, format)
			}
		}
		sort.Slice(matches, func(i, j int) bool {
			if matches[i].Name == matches[j].Name {
				return matches[i].Version < matches[j].Version
			}
			return matches[i].Name < matches[j].Name
		})
		offset := 0
		if input.Cursor != nil {
			var err error
			offset, err = strconv.Atoi(*input.Cursor)
			if err != nil || offset < 0 || offset > len(matches) {
				return toolResult("", errcode.InvalidArgument.New("invalid format cursor"))
			}
		}
		entries := make([]map[string]string, 0)
		end := min(offset+messageFormatPageSize, len(matches))
		for _, format := range matches[offset:end] {
			entries = append(entries, map[string]string{"name": format.Name, "description": format.Description})
		}
		result := map[string]any{"formats": entries}
		if end < len(matches) {
			result["next_cursor"] = strconv.Itoa(end)
		}
		return messageJSON(result)
	}
	formats := []map[string]any{}
	for _, format := range r.description.Formats {
		if format.Name == *input.Type {
			formats = append(formats, r.agentFormat(format, false))
		}
	}
	for _, format := range r.description.Extensions {
		if format.Name == *input.Type {
			formats = append(formats, r.agentFormat(format, true))
		}
	}
	if len(formats) == 0 {
		return toolResult("", errcode.InvalidArgument.New("unknown format; use get_message_format with query to discover declared extensions"))
	}
	names := map[string]bool{*input.Type: true}
	if input.CombineWith != nil {
		for _, name := range *input.CombineWith {
			names[name] = true
		}
	}
	rules := []message.CombinationRule{}
	for _, rule := range r.description.Combinations {
		for _, name := range rule.Formats {
			if names[name] {
				rules = append(rules, rule)
				break
			}
		}
	}
	return messageJSON(map[string]any{"formats": formats, "combinations": rules})
}

func (r *messageRound) agentFormat(format message.MessageFormat, extension bool) map[string]any {
	sources := []string{}
	for _, name := range format.Sources {
		switch name {
		case "incoming":
			sources = append(sources, "attachment")
		case "media_ref", "https_url":
			sources = append(sources, name)
		}
	}
	if !r.scheduled && r.context.Has(sdk.Admin) && r.context.User() != "" && (format.Name == "image" || format.Name == "video" || format.Name == "voice" || format.Name == "file") {
		sources = append(sources, "local_path")
	}
	if extension && len(format.ExampleJSON) > 0 {
		format.ExampleJSON, _ = json.Marshal(map[string]any{"parts": []any{map[string]any{"type": format.Name, "version": format.Version, "payload": format.ExampleJSON}}})
	}
	view := map[string]any{"name": format.Name, "version": format.Version, "status": format.Status, "description": format.Description}
	if len(format.SchemaJSON) > 0 {
		view["schema_json"] = format.SchemaJSON
	}
	if len(format.ExampleJSON) > 0 {
		view["example_json"] = format.ExampleJSON
	}
	if len(sources) > 0 {
		view["sources"] = sources
	}
	if format.AdapterID != "" {
		view["adapter_id"] = format.AdapterID
	}
	for name, limit := range map[string]*message.TextLimit{"text_limit": format.TextLimit, "button_label_limit": format.ButtonLabelLimit} {
		if limit != nil {
			view[name] = map[string]any{"max": limit.Max, "unit": limit.Unit}
		}
	}
	return view
}
