package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"

	"github.com/AlonaBot/AlonaBot/agent/provider"
	"github.com/AlonaBot/AlonaBot/sdk"
	"github.com/AlonaBot/AlonaBot/sdk/errcode"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	toolNameLimit  = 64 // 六种 Provider 的共同命名范围。
	toolNamePrefix = "_"
	toolHashDigits = 16 // FNV-1a 64 位的十六进制宽度。
)

type roundTool struct {
	original   string // Core 完整服务名或 mcp/<server>/<tool>，执行和确认始终使用它。
	definition provider.Tool
	peer       *mcpPeer
	session    *mcp.ClientSession // 本轮目录捕获的来源，不随下一轮重建映射。
	mcpName    string
}

type toolCatalog struct {
	definitions []provider.Tool
	byName      map[string]roundTool
}

func (a *component) listTools(c *sdk.Context) (toolCatalog, error) {
	core, err := sdk.ListTools(c.Context())
	if err != nil {
		return toolCatalog{}, err
	}
	tools := make([]roundTool, 0, len(core))
	for _, tool := range core {
		tools = append(tools, roundTool{original: tool.Name, definition: provider.Tool{Description: tool.Description, InputSchema: tool.InputSchema}})
	}
	for _, peer := range a.mcp {
		session, entries, err := peer.tools(c.Context())
		var additions []roundTool
		names := make(map[string]bool)
		if err == nil {
			for _, tool := range entries {
				if tool == nil || tool.Name == "" || names[tool.Name] {
					err = fmt.Errorf("MCP tool names must be nonempty and unique")
					break
				}
				names[tool.Name] = true
				schema, encodeError := json.Marshal(tool.InputSchema)
				if encodeError != nil {
					err = fmt.Errorf("encode MCP tool schema: %w", encodeError)
					break
				}
				if schema[0] != '{' {
					err = fmt.Errorf("MCP tool input schema must be an object")
					break
				}
				additions = append(additions, roundTool{original: "mcp/" + peer.config.Name + "/" + tool.Name, peer: peer, session: session, mcpName: tool.Name, definition: provider.Tool{Description: tool.Description, InputSchema: schema}})
			}
		}
		if err != nil {
			if c.Context().Err() != nil {
				return toolCatalog{}, c.Context().Err()
			}
			c.Logger().WarnContext(c.Context(), "MCP tools unavailable", "server", peer.config.sanitize(peer.config.Name), "error", peer.config.sanitize(err.Error()))
			continue
		}
		tools = append(tools, additions...)
	}
	return mapTools(tools), nil
}

func mapTools(tools []roundTool) toolCatalog {
	// 按来源排序使碰撞处理与目录/分页的返回顺序无关。
	sort.Slice(tools, func(i, j int) bool { return tools[i].original < tools[j].original })
	catalog := toolCatalog{byName: make(map[string]roundTool, len(tools))}
	for _, tool := range tools {
		base := normalizedToolName(tool.original)
		hash := fnv.New64a()
		hash.Write([]byte(tool.original))
		suffix := "_" + fmt.Sprintf("%0*x", toolHashDigits, hash.Sum64())
		name := base[:min(len(base), toolNameLimit-len(suffix))] + suffix
		for collision := 1; ; collision++ {
			if _, exists := catalog.byName[name]; !exists {
				break
			}
			nextSuffix := suffix + "_" + strconv.Itoa(collision)
			name = base[:min(len(base), toolNameLimit-len(nextSuffix))] + nextSuffix
		}
		tool.definition.Name = name
		catalog.byName[name] = tool
		catalog.definitions = append(catalog.definitions, tool.definition)
	}
	return catalog
}

func normalizedToolName(original string) string {
	var name strings.Builder
	for _, ch := range original {
		if ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '_' {
			name.WriteRune(ch)
		} else {
			name.WriteByte('_')
		}
	}
	result := name.String()
	if result == "" || result[0] >= '0' && result[0] <= '9' {
		result = toolNamePrefix + result
	}
	return result
}

func (t roundTool) invoke(ctx context.Context, arguments json.RawMessage) string {
	if t.peer == nil {
		result, err := sdk.InvokeTool(ctx, t.original, arguments)
		return toolResult(string(result), err)
	}
	// 参数原文完整展示，确认成功后再发出唯一一次 tools/call。
	if err := sdk.ConfirmOperation(ctx, t.original, string(arguments)); err != nil {
		return toolResult("", err)
	}
	if err := ctx.Err(); err != nil {
		return toolResult("", err)
	}
	result, err := t.session.CallTool(ctx, &mcp.CallToolParams{Name: t.mcpName, Arguments: arguments})
	var content string
	if result != nil {
		encoded, encodeError := json.Marshal(result)
		if encodeError != nil {
			err = errors.Join(err, fmt.Errorf("encode MCP tool result: %w", encodeError))
		} else {
			content = string(encoded)
		}
	}
	// 完整 JSON 保留 content 的非文本块、structuredContent、isError；外部错误也先脱敏。
	return t.peer.config.sanitize(toolResult(content, err))
}

func toolResult(content string, err error) string {
	if err == nil {
		return content
	}
	code := errcode.Of(err)
	if code == 0 {
		code = errcode.Unavailable
	}
	// SDK Error.Error 保留 code 与 safe_message；Join 错误也保留全部说明。
	detail := err.Error()
	prefix := code.String() + ": "
	if strings.HasPrefix(detail, prefix) {
		detail = strings.TrimPrefix(detail, prefix)
	}
	failure := "error: " + code.String() + ": " + detail
	if content != "" {
		return content + "\n" + failure
	}
	return failure
}
