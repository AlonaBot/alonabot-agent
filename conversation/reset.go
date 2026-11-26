// Package conversation 提供官方 Agent 的会话服务契约。
package conversation

import "github.com/AlonaBot/AlonaBot/sdk/service"

// ResetRequest 使用父调用的会话和 Actor 清空当前会话。
type ResetRequest struct{}

// ResetResponse 确认当前会话已清空。
type ResetResponse struct{}

// Reset 绑定官方 Agent 的会话重置服务，调用方须保留原消息上下文。
var Reset = service.JSON[ResetRequest, ResetResponse]("alonabot-agent-official/conversation.reset")
