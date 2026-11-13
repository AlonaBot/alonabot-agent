// alonabot-agent 命令运行官方 Agent 组件。
package main

import (
	"github.com/AlonaBot/AlonaBot/agent"
	"github.com/AlonaBot/AlonaBot/sdk"
)

func main() { sdk.Run(agent.New()) }
