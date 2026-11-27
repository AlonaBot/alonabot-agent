package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/AlonaBot/AlonaBot/sdk"
	"github.com/AlonaBot/AlonaBot/sdk/button"
	"github.com/AlonaBot/AlonaBot/sdk/errcode"
	"github.com/AlonaBot/AlonaBot/sdk/media"
	"github.com/AlonaBot/AlonaBot/sdk/message"
	toolapi "github.com/AlonaBot/AlonaBot/sdk/tool"
)

var agentChoice = button.Define[string]("agent.choice")

type messagePart struct {
	Type       string            `json:"type"`
	Text       string            `json:"text"`
	Entities   []messageEntity   `json:"entities"`
	Content    string            `json:"content"`
	Source     map[string]string `json:"source"`
	Caption    string            `json:"caption"`
	Filename   string            `json:"filename"`
	MIME       string            `json:"mime"`
	UserID     string            `json:"user_id"`
	Display    string            `json:"display"`
	MessageRef string            `json:"message_ref"`
	Nodes      []messageNode     `json:"nodes"`
	Rows       [][]messageButton `json:"rows"`
	Version    uint32            `json:"version"`
	Payload    json.RawMessage   `json:"payload"`
}

type messageEntity struct {
	Type     message.EntityType `json:"type"`
	Offset   *uint32            `json:"offset_utf16"`
	Length   uint32             `json:"length_utf16"`
	URL      string             `json:"url"`
	Language string             `json:"language"`
	UserID   string             `json:"external_user_id"`
}

type messageNode struct {
	Parts      []json.RawMessage `json:"parts"`
	MessageRef string            `json:"message_ref"`
}

type messageButton struct {
	Label string  `json:"label"`
	URL   *string `json:"url"`
}

type messageReceipt struct {
	Status string            `json:"status"`
	Parts  []messageDelivery `json:"parts"`
	Error  string            `json:"error,omitempty"`
}

type messageDelivery struct {
	Index          uint32   `json:"part_index"`
	Segments       []uint32 `json:"segments"`
	Status         string   `json:"status"`
	Refs           []string `json:"message_refs"`
	IDsUnavailable bool     `json:"ids_unavailable,omitempty"`
	Error          string   `json:"error,omitempty"`
	ResendSafe     bool     `json:"resend_safe,omitempty"`
}

func (r *messageRound) sendMessage(arguments json.RawMessage) string {
	// 1. 完整验证结构、句柄和本地路径权限，确认前只选择来源，不访问文件。
	var input struct {
		Parts []json.RawMessage `json:"parts"`
	}
	if err := decodeMessageArguments(arguments, &input); err != nil {
		return toolResult("", err)
	}
	paths := []string{}
	uploads := map[string]message.MediaSource{}
	chain, summary, err := r.messageChain(input.Parts, uploads, &paths)
	if err != nil {
		return toolResult("", err)
	}
	confirmation := fmt.Sprintf("Send message to %s via %s", r.target.session(), r.target.Instance)
	for _, path := range paths {
		confirmation += "\n" + path
	}
	if len(confirmation) > toolSummaryBytes {
		return toolResult("", errcode.InvalidArgument.Errorf("message confirmation exceeds %d bytes; full paths and target must fit", toolSummaryBytes))
	}
	if len(paths) > 0 {
		err = toolapi.ConfirmRequired(r.context, sendMessageOperation, confirmation, toolapi.Write)
	} else {
		err = toolapi.Confirm(r.context, sendMessageOperation, confirmation, toolapi.Write)
	}
	if err != nil {
		return toolResult("", err)
	}
	// 2. 管理员确认全部路径后统一上传；上传失败保留具体路径和错误链。
	for _, path := range paths {
		if err := r.context.Err(); err != nil {
			return toolResult("", err)
		}
		source, err := uploadMessageFile(r.context, path)
		if err != nil {
			return toolResult("", err)
		}
		uploads[path] = source
	}
	if len(paths) > 0 {
		chain, _, err = r.messageChain(input.Parts, uploads, new([]string))
		if err != nil {
			return toolResult("", err)
		}
	}
	// 3. 只读预检通过后仅提交一次。unknown 保留尝试行为，真实错误原样返回。
	var support message.SupportStatus
	if r.scheduled {
		support, err = sdk.CheckMessage(r.context, r.target.session(), chain, sdk.Via(r.target.Instance))
	} else {
		support, err = r.context.CheckMessage(r.target.session(), chain, sdk.Via(r.target.Instance))
	}
	if err != nil {
		return toolResult("", err)
	}
	if support == message.SupportUnsupported {
		return toolResult("", errcode.InvalidArgument.New("message combination is unsupported; consult get_message_format"))
	}
	var result *message.SendResult
	if r.scheduled {
		result, err = sdk.Send(r.context, r.target.session(), chain, sdk.Via(r.target.Instance))
	} else {
		result, err = r.context.Send(chain)
	}
	receipt := sendReceipt(result, err)
	r.history = append(r.history, conversationEntry{Role: roleAssistant, Kind: "rich", Content: summary + "\n" + messageJSON(receipt)})
	return messageJSON(receipt)
}

func uploadMessageFile(c *sdk.Context, path string) (message.MediaSource, error) {
	file, err := os.Open(path)
	if err != nil {
		return message.MediaSource{}, fmt.Errorf("open message file %q: %w", path, err)
	}
	ref, uploadError := media.Upload(c, file, "")
	if err := errors.Join(uploadError, file.Close()); err != nil {
		return message.MediaSource{}, fmt.Errorf("upload message file %q: %w", path, err)
	}
	return ref.Source(), nil
}

func (r *messageRound) messageChain(parts []json.RawMessage, uploads map[string]message.MediaSource, paths *[]string) (message.Chain, string, error) {
	if len(parts) == 0 {
		return nil, "", errcode.InvalidArgument.New("parts must be non-empty")
	}
	var chain message.Chain
	var summary strings.Builder
	for index, raw := range parts {
		var part messagePart
		if err := decodeMessageArguments(raw, &part); err != nil {
			return nil, "", fmt.Errorf("parts[%d]: %w", index, err)
		}
		segment, description, err := r.messageSegment(part, raw, uploads, paths)
		if err != nil {
			return nil, "", fmt.Errorf("parts[%d]: %w", index, err)
		}
		chain = append(chain, segment)
		fmt.Fprintf(&summary, "[%s] %s\n", part.Type, description)
	}
	return chain, strings.TrimSuffix(summary.String(), "\n"), nil
}

func (r *messageRound) messageSegment(part messagePart, raw json.RawMessage, uploads map[string]message.MediaSource, paths *[]string) (message.Segment, string, error) {
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, "", err
	}
	allowed := map[string]string{"text": "text entities", "markdown": "content", "image": "source caption", "video": "source caption", "voice": "source caption", "file": "source caption filename mime", "mention": "user_id display", "quote": "message_ref", "forward": "nodes", "buttons": "rows"}
	fieldNames, standard := allowed[part.Type]
	if !standard {
		fieldNames = "version payload"
	}
	for field := range fields {
		if field != "type" && !strings.Contains(" "+fieldNames+" ", " "+field+" ") {
			return nil, "", errcode.InvalidArgument.Errorf("field %q is invalid for %s", field, part.Type)
		}
		if field != "payload" && bytes.Equal(bytes.TrimSpace(fields[field]), []byte("null")) {
			return nil, "", errcode.InvalidArgument.Errorf("field %q must not be null", field)
		}
	}
	for _, format := range r.description.Formats {
		if format.Name == part.Type && format.Status == message.SupportUnsupported {
			return nil, "", errcode.InvalidArgument.Errorf("format %s is unsupported; consult get_message_format", part.Type)
		}
	}
	switch part.Type {
	case "text":
		if part.Text == "" {
			return nil, "", errcode.InvalidArgument.New("text is required")
		}
		entities := []message.TextEntity{}
		for _, entity := range part.Entities {
			if entity.Offset == nil || entity.Length == 0 {
				return nil, "", errcode.InvalidArgument.New("entities require offset_utf16 and positive length_utf16")
			}
			if entity.Type < message.EntityBold || entity.Type > message.EntityTextMention {
				return nil, "", errcode.InvalidArgument.Errorf("unknown text entity %d", entity.Type)
			}
			entities = append(entities, message.TextEntity{Type: entity.Type, OffsetUTF16: *entity.Offset, LengthUTF16: entity.Length, URL: entity.URL, Language: entity.Language, ExternalUserID: entity.UserID})
		}
		return message.TextSegment{Text: part.Text, Entities: entities}, part.Text, nil
	case "markdown":
		if part.Content == "" {
			return nil, "", errcode.InvalidArgument.New("content is required")
		}
		return message.Markdown(part.Content), part.Content, nil
	case "image", "video", "voice", "file":
		source, err := r.messageSource(part.Source, uploads, paths)
		if err != nil {
			return nil, "", err
		}
		switch part.Type {
		case "image":
			return message.ImageSegment{Source: source, Caption: part.Caption}, part.Caption, nil
		case "video":
			return message.VideoSegment{Source: source, Caption: part.Caption}, part.Caption, nil
		case "voice":
			return message.VoiceSegment{Source: source, Caption: part.Caption}, part.Caption, nil
		default:
			return message.FileSegment{Source: source, Caption: part.Caption, Filename: part.Filename, MIME: part.MIME}, part.Filename + " " + part.Caption, nil
		}
	case "mention":
		if part.UserID == "" {
			return nil, "", errcode.InvalidArgument.New("user_id is required")
		}
		return message.MentionSegment{ExternalUserID: part.UserID, Display: part.Display}, part.Display + " " + part.UserID, nil
	case "quote":
		if _, err := r.messageReference(part.MessageRef); err != nil {
			return nil, "", err
		}
		return message.Quote(part.MessageRef), part.MessageRef, nil
	case "forward":
		if len(part.Nodes) == 0 {
			return nil, "", errcode.InvalidArgument.New("nodes must be non-empty")
		}
		nodes := []message.ForwardItem{}
		var summaries []string
		var nodeFields []map[string]json.RawMessage
		if err := json.Unmarshal(fields["nodes"], &nodeFields); err != nil {
			return nil, "", errcode.InvalidArgument.New(err.Error())
		}
		for index, node := range part.Nodes {
			if len(nodeFields[index]) != 1 || (node.Parts != nil) == (node.MessageRef != "") {
				return nil, "", errcode.InvalidArgument.Errorf("nodes[%d] requires exactly one of parts or message_ref", index)
			}
			if node.MessageRef != "" {
				id, err := r.messageReference(node.MessageRef)
				if err != nil {
					return nil, "", err
				}
				nodes = append(nodes, message.ForwardItem{PlatformMessageID: id})
				summaries = append(summaries, node.MessageRef)
			} else {
				chain, summary, err := r.messageChain(node.Parts, uploads, paths)
				if err != nil {
					return nil, "", fmt.Errorf("nodes[%d]: %w", index, err)
				}
				nodes = append(nodes, message.ForwardItem{Chain: chain})
				summaries = append(summaries, summary)
			}
		}
		return message.Forward(nodes...), strings.Join(summaries, "\n"), nil
	case "buttons":
		if len(part.Rows) == 0 {
			return nil, "", errcode.InvalidArgument.New("button rows must be non-empty")
		}
		rows := [][]message.Button{}
		var labels []string
		var buttonFields [][]map[string]json.RawMessage
		if err := json.Unmarshal(fields["rows"], &buttonFields); err != nil {
			return nil, "", errcode.InvalidArgument.New(err.Error())
		}
		for rowIndex, row := range part.Rows {
			if len(row) == 0 {
				return nil, "", errcode.InvalidArgument.New("button row must be non-empty")
			}
			buttons := []message.Button{}
			for buttonIndex, value := range row {
				if value.Label == "" {
					return nil, "", errcode.InvalidArgument.New("button label is required")
				}
				if _, present := buttonFields[rowIndex][buttonIndex]["url"]; present && value.URL == nil {
					return nil, "", errcode.InvalidArgument.New("button URL must be a string")
				}
				if value.URL != nil {
					buttons = append(buttons, message.URLButton(value.Label, *value.URL))
				} else {
					buttons = append(buttons, agentChoice.Button(value.Label, value.Label))
				}
				labels = append(labels, value.Label)
			}
			rows = append(rows, buttons)
		}
		return message.Buttons(rows...), strings.Join(labels, " | "), nil
	default:
		for _, format := range r.description.Extensions {
			if format.Name == part.Type && format.Version == part.Version {
				if format.Status == message.SupportUnsupported {
					return nil, "", errcode.InvalidArgument.New("extension is unsupported")
				}
				if part.Payload == nil {
					return nil, "", errcode.InvalidArgument.New("extension payload is required")
				}
				return message.Extension(format.AdapterID, format.Name, part.Version, part.Payload), format.Description, nil
			}
		}
		return nil, "", errcode.InvalidArgument.New("unknown format/version; use get_message_format to discover declared formats")
	}
}

func (r *messageRound) messageSource(source map[string]string, uploads map[string]message.MediaSource, paths *[]string) (message.MediaSource, error) {
	if len(source) != 1 {
		return message.MediaSource{}, errcode.InvalidArgument.New("source requires exactly one of https_url, media_ref, attachment or local_path")
	}
	for name, value := range source {
		if value == "" {
			return message.MediaSource{}, errcode.InvalidArgument.New("media source must be non-empty")
		}
		switch name {
		case "https_url":
			parsed, err := url.Parse(value)
			if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
				return message.MediaSource{}, errcode.InvalidArgument.New("https_url must be an absolute HTTPS URL")
			}
			return message.MediaSource{HTTPSURL: value}, nil
		case "media_ref":
			return message.MediaSource{MediaRef: value}, nil
		case "attachment":
			attachment, exists := r.attachments[value]
			if !exists {
				return message.MediaSource{}, errcode.InvalidArgument.Errorf("attachment %q is unavailable in this round", value)
			}
			return attachment, nil
		case "local_path":
			if r.scheduled || r.context.User() == "" || !r.context.Has(sdk.Admin) {
				return message.MediaSource{}, errcode.PermissionDenied.New("local_path requires an interactive Core Admin")
			}
			if !filepath.IsAbs(value) {
				return message.MediaSource{}, errcode.InvalidArgument.New("local_path must be absolute")
			}
			if uploaded, exists := uploads[value]; exists {
				return uploaded, nil
			}
			*paths = append(*paths, value)
			return message.MediaSource{}, nil
		default:
			return message.MediaSource{}, errcode.InvalidArgument.Errorf("unknown source %q", name)
		}
	}
	panic("validated source has one entry")
}

func (r *messageRound) messageReference(ref string) (string, error) {
	session, id, instance, err := message.ParseRef(ref)
	if err != nil {
		return "", err
	}
	if session != r.target.session() || instance != r.target.Instance {
		return "", errcode.InvalidArgument.New("message_ref must belong to the current target and instance")
	}
	return id, nil
}

func sendReceipt(result *message.SendResult, err error) messageReceipt {
	receipt := messageReceipt{Status: "unknown", Parts: []messageDelivery{}}
	if err != nil {
		receipt.Error = err.Error()
	}
	if result == nil {
		return receipt
	}
	receipt.Status = map[message.SendStatus]string{message.SendAccepted: "accepted", message.SendPartial: "partial", message.SendRejected: "rejected", message.SendUnknown: "unknown"}[result.Status]
	for _, part := range result.PartResults {
		delivery := messageDelivery{Index: part.PartIndex, Segments: part.SegmentIndexes, Status: map[message.PartStatus]string{message.PartAccepted: "accepted", message.PartRejected: "rejected", message.PartUnknown: "unknown", message.PartNotAttempted: "not_attempted"}[part.Status], Refs: []string{}}
		for _, id := range part.PlatformMessageIDs {
			delivery.Refs = append(delivery.Refs, message.Ref(result.Session, id, result.AdapterInstanceID))
		}
		delivery.IDsUnavailable = part.MessageIDsUnavailable || part.Status == message.PartAccepted && len(delivery.Refs) == 0
		delivery.ResendSafe = part.Status == message.PartRejected || part.Status == message.PartNotAttempted
		if part.Error != nil {
			delivery.Error = part.Error.Error()
		}
		receipt.Parts = append(receipt.Parts, delivery)
	}
	return receipt
}
