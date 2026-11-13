// Package agent 提供官方 Agent 组件，通过 SDK 管理聊天设置、会话历史和流式回复。
//
// [New] 构造独立的组件对象；Provider 客户端位于 provider 子包，聊天设置保存在
// 受管 SQLite，会话条目交由 Core 保存。生命周期与会话契约见 CONTRACTS §13。
package agent

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/AlonaBot/AlonaBot/agent/provider"
	"github.com/AlonaBot/AlonaBot/sdk"
	"github.com/AlonaBot/AlonaBot/sdk/config"
	"github.com/AlonaBot/AlonaBot/sdk/gormdb"
	"github.com/AlonaBot/AlonaBot/sdk/llm"
	"gorm.io/gorm"
)

//go:generate go run ../cmd/alonabot plugin generate

type configuredProvider struct {
	name      string
	client    *provider.Client
	maxTokens int
	protocol  provider.Protocol
	model     string
}

type persona struct{ name, prompt string }

type configuration struct {
	providers                []configuredProvider
	personas                 []persona
	defaultProvider          string
	defaultPersona           string
	historyEntries           int
	mcpServers               []mcpServer
	llmServiceAllowedPlugins []string
}

type component struct {
	config       configuration
	sessionLocks [sessionLockCount]sync.Mutex
	mcp          []*mcpPeer // 当前插件 Session 拥有，连接在首次对话时建立。
}

// New 创建官方 Agent，绑定生成资源并登记命令、迁移和生命周期回调。
func New() *sdk.Plugin {
	p := sdk.NewPlugin()
	sdk.UseGenerated(p, alonabotGenerated)
	a := &component{}
	gormdb.Migrations(p, []gormdb.Migration{{ID: 1, Up: func(ctx context.Context, tx *gorm.DB) error {
		return tx.WithContext(ctx).AutoMigrate(&chatSettings{})
	}}})
	p.OnStart(func(ctx context.Context) error {
		var err error
		a.config, err = readConfiguration(p.Config())
		if err == nil {
			for _, server := range a.config.mcpServers {
				a.mcp = append(a.mcp, newMCPPeer(server))
			}
		}
		return err
	})
	p.OnStop(a.closeMCP)
	a.registerCommands(p)
	a.registerConversation(p)
	sdk.Serve(p, llm.GenerateService).Access(sdk.AccessPublic).Handle(a.generate)
	sdk.ServeOptions(p, providersServiceName, a.providers)
	return p
}

func readConfiguration(values config.Values) (configuration, error) {
	// 1. 在配置边界验证名称、密钥与连接参数，客户端复用进程的代理环境。
	result := configuration{defaultProvider: values.String("default_provider"), defaultPersona: values.String("default_persona"), historyEntries: int(values.Int("history_entries"))}
	result.llmServiceAllowedPlugins = values.Strings("llm_service_allowed_plugins")
	names := make(map[string]bool)
	for _, item := range values.TemplateList("providers") {
		name := item.String("name")
		if names[name] {
			return configuration{}, fmt.Errorf("provider names must be unique")
		}
		names[name] = true
		if item.Int("max_tokens") < 1 {
			return configuration{}, fmt.Errorf("provider max_tokens must be at least 1")
		}
		client, err := provider.New(provider.Config{Protocol: provider.Protocol(item.String("protocol")), BaseURL: item.String("base_url"), APIKey: item.String("api_key"), Model: item.String("model")}, &http.Client{})
		if err != nil {
			return configuration{}, fmt.Errorf("configure provider: %w", err)
		}
		result.providers = append(result.providers, configuredProvider{name: name, client: client, maxTokens: int(item.Int("max_tokens")), protocol: provider.Protocol(item.String("protocol")), model: item.String("model")})
	}
	if (result.defaultProvider != "" || len(names) > 0) && !names[result.defaultProvider] {
		return configuration{}, fmt.Errorf("default_provider must name a configured provider")
	}
	// 2. 校验默认人格的引用；没有人格时允许留空。
	for _, item := range values.Objects("personas") {
		result.personas = append(result.personas, persona{name: item.String("name"), prompt: item.String("prompt")})
	}
	if (result.defaultPersona != "" || len(result.personas) > 0) && result.persona(result.defaultPersona) == nil {
		return configuration{}, fmt.Errorf("default_persona must name a configured persona")
	}
	// 3. MCP 仅校验当前传输使用的字段；凭证已经过 SDK secret 解析。
	servers, err := readMCPServers(values)
	if err != nil {
		return configuration{}, err
	}
	result.mcpServers = servers
	return result, nil
}

func (s *configuration) provider(name string) *configuredProvider {
	for i := range s.providers {
		if s.providers[i].name == name {
			return &s.providers[i]
		}
	}
	return nil
}

func (s *configuration) persona(name string) *persona {
	for i := range s.personas {
		if s.personas[i].name == name {
			return &s.personas[i]
		}
	}
	return nil
}
