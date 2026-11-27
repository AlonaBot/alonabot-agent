// Package agent 提供官方 Agent 组件，通过 SDK 使用配置文件、管理会话历史、图片和流式回复。
//
// [Register] 登记独立组件的业务行为；Provider 客户端位于 provider 子包。受管 SQLite 保存会话运行状态、
// 图片、记忆和模型用量，会话条目交由 Core 保存。模型与会话契约见 CONTRACTS §13。
package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sync"

	"github.com/AlonaBot/AlonaBot/sdk"
	"github.com/AlonaBot/AlonaBot/sdk/config"
	"github.com/AlonaBot/AlonaBot/sdk/gormdb"
	"github.com/AlonaBot/AlonaBot/sdk/llm"
	"github.com/AlonaBot/AlonaBot/sdk/options"
	"github.com/AlonaBot/AlonaBot/sdk/service"
	"github.com/AlonaBot/AlonaBot/sdk/task"
	"github.com/AlonaBot/alonabot-agent/provider"
	"gorm.io/gorm"
)

type configuredProvider struct {
	name       string
	connection provider.Config
}

const (
	modelChat         = "chat"
	modelSpeechToText = "speech_to_text"
	modelTextToSpeech = "text_to_speech"
	modelEmbedding    = "embedding"
	modelRerank       = "rerank"
	modelDecision     = "decision"
)

type configuredModel struct {
	name       string
	provider   *configuredProvider
	modelType  string
	client     *provider.Client
	maxTokens  int
	model      string
	imageInput bool
}

type configuration struct {
	providers                []configuredProvider
	models                   []configuredModel
	historyEntries           int
	mcpServers               []mcpServer
	skillsDir                string
	llmServiceAllowedPlugins []string
	maxAttempts              int64
	fallbackChatModels       []string
	imageCaptionPrompt       string
}

type component struct {
	config              configuration
	sessionLocks        [sessionLockCount]sync.Mutex
	mcpMu               sync.RWMutex
	mcp                 []*mcpPeer // 当前插件 Session 拥有，支持静态配置与页面热增删。
	managedSkillsDir    string     // 随插件数据目录持久化的托管技能目录（激活后首次访问时惰性绑定）。
	managedSkillsLoaded bool
	skills              []skill // skillsMu 保护；轮次捕获后保持不变。
	skillsMu            sync.RWMutex
	skillProblems       []skillProblem
	basePersona         string
}

// Register 登记组件的业务路由、存储和生命周期。
func Register(p *sdk.Plugin) {

	a := &component{}
	gormdb.Migrations(p, []gormdb.Migration{{ID: 1, Up: func(ctx context.Context, tx *gorm.DB) error {
		return tx.WithContext(ctx).AutoMigrate(&chatSettings{}, &conversationImage{}, &modelUsage{}, &persona{}, &conversationRecord{})
	}}, {ID: 2, Up: func(ctx context.Context, tx *gorm.DB) error {
		return tx.WithContext(ctx).AutoMigrate(&memoryRecord{})
	}}, {ID: 3, Up: func(ctx context.Context, tx *gorm.DB) error {
		return tx.WithContext(ctx).AutoMigrate(&managedMCPServer{})
	}}})
	p.OnStart(func(ctx context.Context) error {
		var err error
		a.config, err = readConfiguration(p.Config())
		if err == nil {
			a.basePersona = p.Config().String("default_persona")
			if a.basePersona != "" {
				if _, err := a.readPersona(ctx, a.basePersona); err != nil {
					return fmt.Errorf("default_persona: %w", err)
				}
			}
			a.skills, a.skillProblems = readAllSkills(a.config.skillsDir, "")
			a.mcpMu.Lock()
			a.mcp = nil
			for _, server := range a.config.mcpServers {
				a.mcp = append(a.mcp, newMCPPeer(server))
			}
			a.mcpMu.Unlock()
			if err := a.loadManagedMCP(ctx); err != nil {
				return fmt.Errorf("load managed MCP servers: %w", err)
			}
		}
		return err
	})
	p.OnStop(a.closeMCP)
	a.registerCommands(p)
	a.registerConversation(p)
	p.Route(http.MethodPost, "/models/discover").HandleFunc(a.discoverModels)
	p.Route(http.MethodPost, "/models/test").HandleFunc(a.testModel)
	p.Route(http.MethodGet, "/models/usage").HandleFunc(a.modelsUsage)
	task.On(p, agentTask).Handle(a.scheduled)
	registerMemory(p)
	service.Serve(p, llm.GenerateService).Access(service.AccessPublic).Handle(a.generate)
	options.Serve(p, modelsServiceName, a.models)
	options.Serve(p, "personas", a.personaOptions)
	a.registerPages(p)
}

func readConfiguration(values config.Values) (configuration, error) {
	// 1. 提供商只保存连接与密钥，模型引用同一份配置。
	result := configuration{historyEntries: int(values.Int("history_entries"))}
	result.llmServiceAllowedPlugins = values.Strings("llm_service_allowed_plugins")
	result.skillsDir = values.String("skills_dir")
	names := make(map[string]bool)
	for i, item := range values.TemplateList("providers") {
		path := fmt.Sprintf("$.providers[%d].%s", i, item.Key())
		name := item.String("name")
		if names[name] {
			return configuration{}, fmt.Errorf("%s.name: provider names must be unique", path)
		}
		names[name] = true
		connection := provider.Config{Protocol: provider.Protocol(item.String("protocol")), BaseURL: item.String("base_url"), APIKey: item.String("api_key")}
		if !validProviderBaseURL(connection.BaseURL) {
			return configuration{}, fmt.Errorf("%s.base_url: must be HTTP(S) without credentials, query, or fragment", path)
		}
		if connection.APIKey == "" && connection.Protocol != provider.OllamaChat {
			return configuration{}, fmt.Errorf("%s.api_key: API key is required", path)
		}
		result.providers = append(result.providers, configuredProvider{name: name, connection: connection})
	}
	// 2. 模型名称独立去重；校验类型后复制连接，加入该条目的远端模型 ID。
	clear(names)
	for i, item := range values.Objects("models") {
		path := fmt.Sprintf("$.models[%d]", i)
		name := item.String("name")
		if names[name] {
			return configuration{}, fmt.Errorf("%s.name: model names must be unique", path)
		}
		names[name] = true
		selected := configuredModel{name: name, provider: result.provider(item.String("provider")), model: item.String("model"), modelType: item.String("type")}
		if selected.provider == nil {
			return configuration{}, fmt.Errorf("%s.provider: must name a configured provider", path)
		}
		connection := selected.provider.connection
		if selected.modelType == modelChat {
			selected.maxTokens = int(item.Int("max_tokens"))
			selected.imageInput = item.Bool("image_input")
			if selected.maxTokens < 1 {
				return configuration{}, fmt.Errorf("%s.max_tokens: must be at least 1", path)
			}
		} else if connection.Protocol != provider.OpenAI && connection.Protocol != provider.OpenAICompatible {
			return configuration{}, fmt.Errorf("%s.type: model type requires openai or openai-compatible protocol", path)
		}
		connection.Model = selected.model
		client, err := provider.New(connection, &http.Client{})
		if err != nil {
			return configuration{}, fmt.Errorf("%s: configure model: %w", path, err)
		}
		selected.client = client
		result.models = append(result.models, selected)
	}
	if name := values.String("chat_model"); name != "" {
		selected := result.model(name)
		if selected == nil || selected.modelType != modelChat {
			return configuration{}, fmt.Errorf("$.chat_model: must name a configured chat model")
		}
	}
	result.maxAttempts = values.Int("max_attempts")
	if result.maxAttempts < 1 {
		return configuration{}, fmt.Errorf("$.max_attempts: must be at least 1")
	}
	result.fallbackChatModels = values.Strings("fallback_chat_models")
	for i, name := range result.fallbackChatModels {
		if selected := result.model(name); selected == nil || selected.modelType != modelChat {
			return configuration{}, fmt.Errorf("$.fallback_chat_models[%d]: must name a configured chat model", i)
		}
	}
	if name := values.String("image_caption_model"); name != "" {
		selected := result.model(name)
		if selected == nil || selected.modelType != modelChat || !selected.imageInput {
			return configuration{}, fmt.Errorf("$.image_caption_model: must name a configured chat model with image_input enabled")
		}
	}
	result.imageCaptionPrompt = values.String("image_caption_prompt")
	// 3. MCP 仅校验当前传输使用的字段；凭证已经过 SDK secret 解析。
	servers, err := readMCPServers(values)
	if err != nil {
		return configuration{}, err
	}
	result.mcpServers = servers
	return result, nil
}

func validProviderBaseURL(raw string) bool {
	address, err := url.Parse(raw)
	return err == nil && address.Host != "" && (address.Scheme == "http" || address.Scheme == "https") &&
		address.User == nil && address.RawQuery == "" && address.Fragment == ""
}

func (s *configuration) provider(name string) *configuredProvider {
	for i := range s.providers {
		if s.providers[i].name == name {
			return &s.providers[i]
		}
	}
	return nil
}

func (s *configuration) model(name string) *configuredModel {
	for i := range s.models {
		if s.models[i].name == name {
			return &s.models[i]
		}
	}
	return nil
}
