// alonabot-agent 命令运行官方 Agent 组件。
package main

import (
	"github.com/AlonaBot/AlonaBot/sdk"
	"github.com/AlonaBot/alonabot-agent"
)

func main() { sdk.Run(agent.New()) }
