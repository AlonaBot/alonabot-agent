// Package agent 提供官方组件的公开构造入口。
package agent

import (
	"github.com/AlonaBot/AlonaBot/sdk"
	implementation "github.com/AlonaBot/alonabot-agent/internal/agent"
)

// New 创建独立组件并登记其业务行为。
func New() *sdk.Plugin {
	p := sdk.NewPlugin()
	implementation.Register(p)
	return p
}
