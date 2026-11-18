//go:build unix

package agent_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/AlonaBot/AlonaBot"
	"github.com/AlonaBot/AlonaBot/agent"
	"github.com/AlonaBot/AlonaBot/agent/provider"
	genresources "github.com/AlonaBot/AlonaBot/internal/generated"
	"github.com/AlonaBot/AlonaBot/sdk"
	"github.com/AlonaBot/AlonaBot/sdk/adapter"
	"github.com/AlonaBot/AlonaBot/sdk/errcode"
	"github.com/AlonaBot/AlonaBot/sdk/message"
)

const (
	agentID     = "alonabot-agent-official"
	loopID      = "agentloop"
	loopSession = "agentloop:group"
	testTimeout = 20 * time.Second
)

var agentResources = genresources.Resources{
	Manifest:     []byte("# 官方 Agent 组件的工程身份与静态图标。\nid: alonabot-agent-official\nname: AlonaBot Agent\nversion: 0.1.0\nauthor: AlonaBot\nrole: agent\ndescription: Official conversational agent\nsource: github.com/AlonaBot/AlonaBot\nicon: assets/icons/agent.svg\n"),
	ConfigSchema: []byte("{\"providers\":{\"type\":\"template_list\",\"description\":\"Providers\",\"templates\":{\"openai\":{\"name\":\"OpenAI\",\"display_item\":\"name\",\"items\":{\"name\":{\"type\":\"string\",\"description\":\"Name\",\"required\":true},\"protocol\":{\"type\":\"string\",\"description\":\"Protocol\",\"options\":[\"openai\",\"openai-compatible\",\"anthropic-messages\",\"openai-responses\",\"gemini-generate-content\",\"ollama-chat\"],\"default\":\"openai\"},\"base_url\":{\"type\":\"string\",\"description\":\"Base URL\",\"default\":\"https://api.openai.com/v1\"},\"api_key\":{\"type\":\"string\",\"description\":\"API key\",\"secret\":true}}},\"anthropic\":{\"name\":\"Anthropic\",\"display_item\":\"name\",\"items\":{\"name\":{\"type\":\"string\",\"description\":\"Name\",\"required\":true},\"protocol\":{\"type\":\"string\",\"description\":\"Protocol\",\"options\":[\"openai\",\"openai-compatible\",\"anthropic-messages\",\"openai-responses\",\"gemini-generate-content\",\"ollama-chat\"],\"default\":\"anthropic-messages\"},\"base_url\":{\"type\":\"string\",\"description\":\"Base URL\",\"default\":\"https://api.anthropic.com\"},\"api_key\":{\"type\":\"string\",\"description\":\"API key\",\"secret\":true}}},\"gemini\":{\"name\":\"Gemini\",\"display_item\":\"name\",\"items\":{\"name\":{\"type\":\"string\",\"description\":\"Name\",\"required\":true},\"protocol\":{\"type\":\"string\",\"description\":\"Protocol\",\"options\":[\"openai\",\"openai-compatible\",\"anthropic-messages\",\"openai-responses\",\"gemini-generate-content\",\"ollama-chat\"],\"default\":\"gemini-generate-content\"},\"base_url\":{\"type\":\"string\",\"description\":\"Base URL\",\"default\":\"https://generativelanguage.googleapis.com/v1beta\"},\"api_key\":{\"type\":\"string\",\"description\":\"API key\",\"secret\":true}}},\"deepseek\":{\"name\":\"DeepSeek\",\"display_item\":\"name\",\"items\":{\"name\":{\"type\":\"string\",\"description\":\"Name\",\"required\":true},\"protocol\":{\"type\":\"string\",\"description\":\"Protocol\",\"options\":[\"openai\",\"openai-compatible\",\"anthropic-messages\",\"openai-responses\",\"gemini-generate-content\",\"ollama-chat\"],\"default\":\"openai-compatible\"},\"base_url\":{\"type\":\"string\",\"description\":\"Base URL\",\"default\":\"https://api.deepseek.com/v1\"},\"api_key\":{\"type\":\"string\",\"description\":\"API key\",\"secret\":true}}},\"ollama\":{\"name\":\"Ollama\",\"display_item\":\"name\",\"items\":{\"name\":{\"type\":\"string\",\"description\":\"Name\",\"required\":true},\"protocol\":{\"type\":\"string\",\"description\":\"Protocol\",\"options\":[\"openai\",\"openai-compatible\",\"anthropic-messages\",\"openai-responses\",\"gemini-generate-content\",\"ollama-chat\"],\"default\":\"ollama-chat\"},\"base_url\":{\"type\":\"string\",\"description\":\"Base URL\",\"default\":\"http://localhost:11434\"},\"api_key\":{\"type\":\"string\",\"description\":\"API key\",\"secret\":true}}},\"custom\":{\"name\":\"Custom\",\"display_item\":\"name\",\"items\":{\"name\":{\"type\":\"string\",\"description\":\"Name\",\"required\":true},\"protocol\":{\"type\":\"string\",\"description\":\"Protocol\",\"options\":[\"openai\",\"openai-compatible\",\"anthropic-messages\",\"openai-responses\",\"gemini-generate-content\",\"ollama-chat\"],\"default\":\"openai-compatible\"},\"base_url\":{\"type\":\"string\",\"description\":\"Base URL\",\"required\":true},\"api_key\":{\"type\":\"string\",\"description\":\"API key\",\"secret\":true}}}}},\"personas\":{\"type\":\"list\",\"description\":\"Personas\",\"items\":{\"name\":{\"type\":\"string\",\"description\":\"Name\",\"required\":true},\"prompt\":{\"type\":\"text\",\"description\":\"System prompt\"}}},\"default_persona\":{\"type\":\"string\",\"description\":\"Default persona\",\"profile\":true},\"history_entries\":{\"type\":\"int\",\"description\":\"History entries\",\"default\":20,\"slider\":{\"min\":0,\"max\":500,\"step\":1}},\"mcp_servers\":{\"type\":\"list\",\"description\":\"MCP servers\",\"items\":{\"name\":{\"type\":\"string\",\"description\":\"Name\",\"required\":true},\"transport\":{\"type\":\"string\",\"description\":\"Transport\",\"options\":[\"stdio\",\"streamable-http\"],\"default\":\"stdio\"},\"command\":{\"type\":\"string\",\"description\":\"Command\",\"condition\":{\"transport\":\"stdio\"}},\"args\":{\"type\":\"list\",\"description\":\"Arguments\",\"condition\":{\"transport\":\"stdio\"}},\"env\":{\"type\":\"list\",\"description\":\"Environment variables\",\"items\":{\"name\":{\"type\":\"string\",\"description\":\"Name\",\"required\":true},\"value\":{\"type\":\"string\",\"description\":\"Secret value\",\"secret\":true}},\"condition\":{\"transport\":\"stdio\"}},\"url\":{\"type\":\"string\",\"description\":\"URL\",\"condition\":{\"transport\":\"streamable-http\"}},\"headers\":{\"type\":\"list\",\"description\":\"HTTP headers\",\"items\":{\"name\":{\"type\":\"string\",\"description\":\"Name\",\"required\":true},\"value\":{\"type\":\"string\",\"description\":\"Secret value\",\"secret\":true}},\"condition\":{\"transport\":\"streamable-http\"}}}},\"skills_dir\":{\"type\":\"string\",\"description\":\"Skills directory\",\"hint\":\"Empty disables skills. Relative paths use the plugin working directory. Restart the plugin after changing skills.\",\"default\":\"\"},\"llm_service_allowed_plugins\":{\"type\":\"list\",\"description\":\"Plugins allowed to generate text\",\"default\":[\"*\"]},\"chat_model\":{\"type\":\"string\",\"description\":\"Chat model\",\"options_from\":{\"source\":\"@agent/llm.models\",\"filter\":{\"type\":\"chat\"}},\"profile\":true},\"models\":{\"type\":\"list\",\"description\":\"Models\",\"items\":{\"name\":{\"type\":\"string\",\"description\":\"Name\",\"required\":true},\"provider\":{\"type\":\"string\",\"description\":\"Provider\",\"required\":true},\"model\":{\"type\":\"string\",\"description\":\"Model ID\",\"required\":true},\"type\":{\"type\":\"string\",\"description\":\"Model type\",\"options\":[\"chat\",\"speech_to_text\",\"text_to_speech\",\"embedding\",\"rerank\",\"decision\"],\"default\":\"chat\"},\"max_tokens\":{\"type\":\"int\",\"description\":\"Maximum output tokens\",\"hint\":\"Must be at least 1.\",\"default\":2048,\"condition\":{\"type\":\"chat\"}},\"image_input\":{\"type\":\"bool\",\"description\":\"Image input\",\"default\":false,\"condition\":{\"type\":\"chat\"}}}},\"max_attempts\":{\"type\":\"int\",\"description\":\"Maximum attempts per chat model\",\"hint\":\"Includes the first request. Must be at least 1.\",\"default\":3},\"fallback_chat_models\":{\"type\":\"list\",\"description\":\"Fallback chat models\",\"default\":[],\"options_from\":{\"source\":\"@agent/llm.models\",\"filter\":{\"type\":\"chat\"}}},\"image_caption_model\":{\"type\":\"string\",\"description\":\"Image caption model\",\"hint\":\"Choose a chat model that accepts images, or leave empty to keep image placeholders.\",\"options_from\":{\"source\":\"@agent/llm.models\",\"filter\":{\"type\":\"chat\"}},\"profile\":true},\"image_caption_prompt\":{\"type\":\"text\",\"description\":\"Image caption prompt\"},\"triggers\":{\"type\":\"list\",\"description\":\"Free conversation triggers\",\"options\":[\"mention\",\"wake_word\",\"private\"],\"labels\":[\"Group mention or reply\",\"Wake word\",\"Private chat\"],\"default\":[],\"profile\":true},\"wake_words\":{\"type\":\"list\",\"description\":\"Wake words\",\"default\":[],\"profile\":true},\"member_sessions\":{\"type\":\"bool\",\"description\":\"Separate group conversations by member\",\"default\":false,\"profile\":true}}"),
	I18n: map[string][]byte{
		"en-US.json": []byte("{\n  \"metadata\": {\n    \"name\": \"AlonaBot Agent\",\n    \"description\": \"Official conversational agent\"\n  },\n  \"messages\": {\n    \"agent.skills.header\": \"Available skills (use load_skill to read a skill):\",\n    \"agent.memory_error\": \"Memory could not be read ({code}).\",\n    \"agent.error\": \"The provider request failed ({code}).\",\n    \"agent.personas\": \"Personas: {names}\",\n    \"agent.unknown_persona\": \"Unknown persona: {name}\",\n    \"agent.reset\": \"Conversation cleared.\",\n    \"agent.tool_limit\": \"The tool call limit has been reached. This conversation turn has ended.\",\n    \"agent.tools_error\": \"The tool directory request failed ({code}).\",\n    \"agent.no_model\": \"Ask an administrator to configure and select a chat model.\",\n    \"agent.models\": \"Models: {names}\",\n    \"agent.unknown_model\": \"Unknown chat model: {name}\",\n    \"agent.profile_updated\": \"Profile updated: {name} ({sessions} sessions).\",\n    \"agent.profile_reloading\": \"The Agent will reload shortly.\",\n    \"agent.profile_current\": \"Current profile: {name} ({sessions} sessions).\",\n    \"agent.unknown_profile\": \"Unknown profile: {name}. Available profiles: {names}\"\n  },\n  \"config\": {\n    \"skills_dir\": {\n      \"description\": \"Skills directory\",\n      \"hint\": \"Empty disables skills. Relative paths use the plugin working directory. Restart the plugin after changing skills.\"\n    },\n    \"mcp_servers\": {\n      \"description\": \"MCP servers\",\n      \"items\": {\n        \"name\": {\n          \"description\": \"Name\"\n        },\n        \"transport\": {\n          \"description\": \"Transport\"\n        },\n        \"command\": {\n          \"description\": \"Command\"\n        },\n        \"args\": {\n          \"description\": \"Arguments\"\n        },\n        \"env\": {\n          \"description\": \"Environment variables\",\n          \"items\": {\n            \"name\": {\n              \"description\": \"Name\"\n            },\n            \"value\": {\n              \"description\": \"Secret value\"\n            }\n          }\n        },\n        \"url\": {\n          \"description\": \"URL\"\n        },\n        \"headers\": {\n          \"description\": \"HTTP headers\",\n          \"items\": {\n            \"name\": {\n              \"description\": \"Name\"\n            },\n            \"value\": {\n              \"description\": \"Secret value\"\n            }\n          }\n        }\n      }\n    },\n    \"llm_service_allowed_plugins\": {\n      \"description\": \"Plugins allowed to generate text\"\n    },\n    \"providers\": {\n      \"templates\": {\n        \"openai\": {\n          \"items\": {}\n        },\n        \"anthropic\": {\n          \"items\": {}\n        },\n        \"gemini\": {\n          \"items\": {}\n        },\n        \"deepseek\": {\n          \"items\": {}\n        },\n        \"ollama\": {\n          \"items\": {}\n        },\n        \"custom\": {\n          \"items\": {}\n        }\n      }\n    },\n    \"chat_model\": {\n      \"description\": \"Chat model\"\n    },\n    \"models\": {\n      \"description\": \"Models\",\n      \"items\": {\n        \"name\": {\n          \"description\": \"Name\"\n        },\n        \"provider\": {\n          \"description\": \"Provider\"\n        },\n        \"model\": {\n          \"description\": \"Model ID\"\n        },\n        \"type\": {\n          \"description\": \"Model type\"\n        },\n        \"max_tokens\": {\n          \"description\": \"Maximum output tokens\",\n          \"hint\": \"Must be at least 1.\"\n        },\n        \"image_input\": {\n          \"description\": \"Image input\"\n        }\n      }\n    },\n    \"max_attempts\": {\n      \"description\": \"Maximum attempts per chat model\",\n      \"hint\": \"Includes the first request. Must be at least 1.\"\n    },\n    \"fallback_chat_models\": {\n      \"description\": \"Fallback chat models\"\n    },\n    \"image_caption_model\": {\n      \"description\": \"Image caption model\",\n      \"hint\": \"Choose a chat model that accepts images, or leave empty to keep image placeholders.\"\n    },\n    \"image_caption_prompt\": {\n      \"description\": \"Image caption prompt\"\n    },\n    \"triggers\": {\n      \"description\": \"Free conversation triggers\",\n      \"labels\": [\n        \"Group mention or reply\",\n        \"Wake word\",\n        \"Private chat\"\n      ]\n    },\n    \"wake_words\": {\n      \"description\": \"Wake words\"\n    },\n    \"member_sessions\": {\n      \"description\": \"Separate group conversations by member\"\n    }\n  }\n}\n"),
		"zh-CN.json": []byte("{\n  \"metadata\": {\n    \"name\": \"AlonaBot Agent\",\n    \"description\": \"官方对话 Agent\"\n  },\n  \"messages\": {\n    \"agent.skills.header\": \"可用技能（使用 load_skill 读取技能）：\",\n    \"agent.memory_error\": \"记忆读取失败（{code}）。\",\n    \"agent.error\": \"Provider 请求失败（{code}）。\",\n    \"agent.personas\": \"人格：{names}\",\n    \"agent.unknown_persona\": \"找不到人格：{name}\",\n    \"agent.reset\": \"已清除会话历史。\",\n    \"agent.tool_limit\": \"已达到本轮工具调用上限，本轮对话已结束。\",\n    \"agent.tools_error\": \"工具目录请求失败（{code}）。\",\n    \"agent.no_model\": \"请管理员配置并选择对话模型。\",\n    \"agent.models\": \"模型：{names}\",\n    \"agent.unknown_model\": \"找不到对话模型：{name}\",\n    \"agent.profile_updated\": \"已更新配置文件：{name}（{sessions} 个会话）。\",\n    \"agent.profile_reloading\": \"Agent 即将重载。\",\n    \"agent.profile_current\": \"当前配置文件：{name}（{sessions} 个会话）。\",\n    \"agent.unknown_profile\": \"找不到配置文件：{name}。可用配置文件：{names}\"\n  },\n  \"commands\": {\n    \"ask\": {\n      \"description\": \"向 Agent 提问\"\n    },\n    \"agent on\": {\n      \"description\": \"开启当前配置文件的自由对话\"\n    },\n    \"agent off\": {\n      \"description\": \"关闭当前配置文件的自由对话\"\n    },\n    \"persona\": {\n      \"description\": \"列出人格或为当前配置文件选择人格\"\n    },\n    \"provider\": {\n      \"description\": \"列出模型或为当前配置文件选择对话模型\"\n    },\n    \"reset\": {\n      \"description\": \"清除本聊天的会话历史\"\n    },\n    \"profile\": {\n      \"description\": \"查看或切换配置文件\"\n    }\n  },\n  \"config\": {\n    \"skills_dir\": {\n      \"description\": \"技能目录\",\n      \"hint\": \"留空关闭技能。相对路径以插件工作目录为基准，修改技能后需重启插件。\"\n    },\n    \"providers\": {\n      \"description\": \"Provider\",\n      \"templates\": {\n        \"openai\": {\n          \"name\": \"OpenAI\",\n          \"items\": {\n            \"name\": {\n              \"description\": \"名称\"\n            },\n            \"protocol\": {\n              \"description\": \"协议\"\n            },\n            \"base_url\": {\n              \"description\": \"服务地址\"\n            },\n            \"api_key\": {\n              \"description\": \"API 密钥\"\n            }\n          }\n        },\n        \"anthropic\": {\n          \"name\": \"Anthropic\",\n          \"items\": {\n            \"name\": {\n              \"description\": \"名称\"\n            },\n            \"protocol\": {\n              \"description\": \"协议\"\n            },\n            \"base_url\": {\n              \"description\": \"服务地址\"\n            },\n            \"api_key\": {\n              \"description\": \"API 密钥\"\n            }\n          }\n        },\n        \"gemini\": {\n          \"name\": \"Gemini\",\n          \"items\": {\n            \"name\": {\n              \"description\": \"名称\"\n            },\n            \"protocol\": {\n              \"description\": \"协议\"\n            },\n            \"base_url\": {\n              \"description\": \"服务地址\"\n            },\n            \"api_key\": {\n              \"description\": \"API 密钥\"\n            }\n          }\n        },\n        \"deepseek\": {\n          \"name\": \"DeepSeek\",\n          \"items\": {\n            \"name\": {\n              \"description\": \"名称\"\n            },\n            \"protocol\": {\n              \"description\": \"协议\"\n            },\n            \"base_url\": {\n              \"description\": \"服务地址\"\n            },\n            \"api_key\": {\n              \"description\": \"API 密钥\"\n            }\n          }\n        },\n        \"ollama\": {\n          \"name\": \"Ollama\",\n          \"items\": {\n            \"name\": {\n              \"description\": \"名称\"\n            },\n            \"protocol\": {\n              \"description\": \"协议\"\n            },\n            \"base_url\": {\n              \"description\": \"服务地址\"\n            },\n            \"api_key\": {\n              \"description\": \"API 密钥\"\n            }\n          }\n        },\n        \"custom\": {\n          \"name\": \"自定义\",\n          \"items\": {\n            \"name\": {\n              \"description\": \"名称\"\n            },\n            \"protocol\": {\n              \"description\": \"协议\"\n            },\n            \"base_url\": {\n              \"description\": \"服务地址\"\n            },\n            \"api_key\": {\n              \"description\": \"API 密钥\"\n            }\n          }\n        }\n      }\n    },\n    \"personas\": {\n      \"description\": \"人格\",\n      \"items\": {\n        \"name\": {\n          \"description\": \"名称\"\n        },\n        \"prompt\": {\n          \"description\": \"系统提示词\"\n        }\n      }\n    },\n    \"default_persona\": {\n      \"description\": \"默认人格\"\n    },\n    \"history_entries\": {\n      \"description\": \"历史条数\"\n    },\n    \"mcp_servers\": {\n      \"description\": \"MCP 服务器\",\n      \"items\": {\n        \"name\": {\n          \"description\": \"名称\"\n        },\n        \"transport\": {\n          \"description\": \"传输方式\"\n        },\n        \"command\": {\n          \"description\": \"命令\"\n        },\n        \"args\": {\n          \"description\": \"参数\"\n        },\n        \"env\": {\n          \"description\": \"环境变量\",\n          \"items\": {\n            \"name\": {\n              \"description\": \"名称\"\n            },\n            \"value\": {\n              \"description\": \"秘密值\"\n            }\n          }\n        },\n        \"url\": {\n          \"description\": \"服务地址\"\n        },\n        \"headers\": {\n          \"description\": \"HTTP 请求头\",\n          \"items\": {\n            \"name\": {\n              \"description\": \"名称\"\n            },\n            \"value\": {\n              \"description\": \"秘密值\"\n            }\n          }\n        }\n      }\n    },\n    \"llm_service_allowed_plugins\": {\n      \"description\": \"允许生成文本的插件\"\n    },\n    \"chat_model\": {\n      \"description\": \"对话模型\"\n    },\n    \"models\": {\n      \"description\": \"模型\",\n      \"items\": {\n        \"name\": {\n          \"description\": \"名称\"\n        },\n        \"provider\": {\n          \"description\": \"提供商\"\n        },\n        \"model\": {\n          \"description\": \"模型 ID\"\n        },\n        \"type\": {\n          \"description\": \"模型类型\"\n        },\n        \"max_tokens\": {\n          \"description\": \"最大输出 token 数\",\n          \"hint\": \"至少为 1。\"\n        },\n        \"image_input\": {\n          \"description\": \"图片输入\"\n        }\n      }\n    },\n    \"max_attempts\": {\n      \"description\": \"每个对话模型的最大尝试次数\",\n      \"hint\": \"含首次请求，至少为 1。\"\n    },\n    \"fallback_chat_models\": {\n      \"description\": \"回退对话模型\"\n    },\n    \"image_caption_model\": {\n      \"description\": \"图像转述模型\",\n      \"hint\": \"选择支持图片的对话模型，留空则保留图片占位。\"\n    },\n    \"image_caption_prompt\": {\n      \"description\": \"图像转述提示词\"\n    },\n    \"triggers\": {\n      \"description\": \"自由对话触发方式\",\n      \"labels\": [\n        \"群聊提及或回复\",\n        \"唤醒词\",\n        \"私聊\"\n      ]\n    },\n    \"wake_words\": {\n      \"description\": \"唤醒词\"\n    },\n    \"member_sessions\": {\n      \"description\": \"群成员独立会话\"\n    }\n  }\n}\n"),
	},
	ServiceSchemas: map[string][]byte{
		"github.com/AlonaBot/AlonaBot/agent.memoryRequest":  []byte("{\"$schema\":\"https://json-schema.org/draft/2020-12/schema\",\"properties\":{\"operations\":{\"items\":{\"properties\":{\"old_text\":{\"type\":\"string\"},\"op\":{\"type\":\"string\"},\"target\":{\"type\":\"string\"},\"text\":{\"type\":\"string\"}},\"required\":[\"op\",\"target\"],\"type\":\"object\"},\"type\":[\"array\",\"null\"]}},\"required\":[\"operations\"],\"type\":\"object\"}"),
		"github.com/AlonaBot/AlonaBot/agent.memoryResponse": []byte("{\"$schema\":\"https://json-schema.org/draft/2020-12/schema\",\"properties\":{},\"type\":\"object\"}"),
		"github.com/AlonaBot/AlonaBot/sdk/llm.Request":      []byte("{\"$schema\":\"https://json-schema.org/draft/2020-12/schema\",\"properties\":{\"max_output_tokens\":{\"maximum\":9223372036854775807,\"minimum\":-9223372036854775808,\"type\":\"integer\"},\"messages\":{\"items\":{\"properties\":{\"role\":{\"type\":\"string\"},\"text\":{\"type\":\"string\"}},\"required\":[\"role\",\"text\"],\"type\":\"object\"},\"type\":[\"array\",\"null\"]},\"system\":{\"type\":\"string\"},\"model\":{\"type\":\"string\"}},\"required\":[\"messages\"],\"type\":\"object\"}"),
		"github.com/AlonaBot/AlonaBot/sdk/llm.Response":     []byte("{\"$schema\":\"https://json-schema.org/draft/2020-12/schema\",\"properties\":{\"input_tokens\":{\"maximum\":9223372036854775807,\"minimum\":-9223372036854775808,\"type\":[\"integer\",\"null\"]},\"model\":{\"type\":\"string\"},\"output_tokens\":{\"maximum\":9223372036854775807,\"minimum\":-9223372036854775808,\"type\":[\"integer\",\"null\"]},\"text\":{\"type\":\"string\"}},\"required\":[\"text\",\"model\"],\"type\":\"object\"}"),
	},
	ParamNames: map[string][]string{
		"github.com/AlonaBot/AlonaBot/agent.(*component).addTaskTools":       {"catalog"},
		"github.com/AlonaBot/AlonaBot/agent.(*component).appendImages":       {"db", "generation", "user", "assistant", "images"},
		"github.com/AlonaBot/AlonaBot/agent.(*component).conversationMemory": {"db", "key"},
		"github.com/AlonaBot/AlonaBot/agent.(*component).converse":           {"text"},
		"github.com/AlonaBot/AlonaBot/agent.(*component).generate":           {"req"},
		"github.com/AlonaBot/AlonaBot/agent.(*component).listTools":          {},
		"github.com/AlonaBot/AlonaBot/agent.(*component).resetConversation":  {},
		"github.com/AlonaBot/AlonaBot/agent.(*component).runScheduled":       {"session", "task"},
		"github.com/AlonaBot/AlonaBot/agent.(*component).scheduled":          {"task"},
		"github.com/AlonaBot/AlonaBot/agent.(*component).selectPersona":      {"name"},
		"github.com/AlonaBot/AlonaBot/agent.(*component).selectModel":        {"name"},
		"github.com/AlonaBot/AlonaBot/agent.(*component).selectProfile":      {"name"},
		"github.com/AlonaBot/AlonaBot/agent.cancelTask":                      {"arguments"},
		"github.com/AlonaBot/AlonaBot/agent.currentTaskTarget":               {},
		"github.com/AlonaBot/AlonaBot/agent.listTasks":                       {},
		"github.com/AlonaBot/AlonaBot/agent.memoryError":                     {"err"},
		"github.com/AlonaBot/AlonaBot/agent.memoryScope":                     {"target"},
		"github.com/AlonaBot/AlonaBot/agent.readImages":                      {},
		"github.com/AlonaBot/AlonaBot/agent.scheduleTask":                    {"arguments"},
		"github.com/AlonaBot/AlonaBot/agent.sessionKey":                      {},
		"github.com/AlonaBot/AlonaBot/agent:conversation.go:37":              {"chain"},
		"github.com/AlonaBot/AlonaBot/agent:conversation.go:39":              {},
		"github.com/AlonaBot/AlonaBot/agent:conversation.go:41":              {},
		"github.com/AlonaBot/AlonaBot/agent:conversation.go:42":              {},
		"github.com/AlonaBot/AlonaBot/agent:settings.go:59":                  {},
		"github.com/AlonaBot/AlonaBot/agent:settings.go:63":                  {},
		"github.com/AlonaBot/AlonaBot/agent:settings.go:70":                  {},
		"github.com/AlonaBot/AlonaBot/agent:memory.go:79":                    {"in"},
	},
}

var loopResources = genresources.Resources{Manifest: []byte("id: alonabot-adapter-agentloop\nname: Agent test loopback\nversion: 0.1.0\nauthor: AlonaBot\nrole: adapter\nadapter_id: agentloop\nsource: github.com/AlonaBot/AlonaBot\n"), AdapterConfigSchema: []byte(`{}`)}

type operation struct {
	method, id, text string
	to               string
	buttons          []message.Button
}

type loopback struct {
	core       adapter.Core
	cores      map[string]adapter.Core // 各实例的真实入站入口，启动后只读。
	edit       bool
	buttons    bool
	limit      uint64
	mu         sync.Mutex // 记录由 Core 并发调用的发送与编辑。
	operations []operation
	changed    chan struct{} // 关闭后同时唤醒 Provider 与测试断言，随后换成新通道。
	sequence   atomic.Uint64
	files      map[string][]byte // 启动前填入平台文件，取件时只读。
}

func (a *loopback) Descriptor() *adapter.Descriptor {
	return &adapter.Descriptor{AdapterID: loopID, Capabilities: a.Capabilities()}
}

func (a *loopback) Start(_ context.Context, request adapter.StartRequest, core adapter.Core) (adapter.Instance, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cores == nil {
		a.cores = make(map[string]adapter.Core)
	}
	a.cores[request.InstanceID] = core
	if request.InstanceID == "main" {
		a.core = core
	}
	return &loopbackInstance{loopback: a, instance: request.InstanceID}, nil
}

type loopbackInstance struct {
	*loopback
	instance string
}

func (a *loopbackInstance) Account() adapter.Account {
	if a.instance == "main" {
		return a.loopback.Account()
	}
	return adapter.Account{ID: "bot-" + a.instance}
}

func (a *loopbackInstance) Send(ctx context.Context, to string, chain message.Chain) (*message.SendResult, *errcode.Error) {
	result, err := a.loopback.Send(ctx, to, chain)
	result.AdapterInstanceID = a.instance
	return result, err
}

func (a *loopbackInstance) Edit(ctx context.Context, to string, id string, chain message.Chain) (*message.SendResult, *errcode.Error) {
	result, err := a.loopback.Edit(ctx, to, id, chain)
	result.AdapterInstanceID = a.instance
	return result, err
}

func (*loopback) Account() adapter.Account { return adapter.Account{ID: "bot"} }

func (a *loopback) Capabilities() []message.Capability {
	return []message.Capability{
		{Name: "message.send", Version: 1, Supported: true, TextLimit: &message.TextLimit{Max: a.limit, Unit: message.TextUnicodeCodePoints}},
		{Name: "message.edit", Version: 1, Supported: a.edit},
		{Name: "message.buttons", Version: 1, Supported: a.buttons},
		{Name: "media.read", Version: 1, Supported: true},
	}
}

func (a *loopback) ReadPlatformFile(_ context.Context, fileID string, offset uint64, length uint32) ([]byte, uint64, bool, error) {
	data, ok := a.files[fileID]
	if !ok {
		return nil, 0, false, fmt.Errorf("platform file not found")
	}
	if offset > uint64(len(data)) {
		return nil, 0, false, fmt.Errorf("platform file offset exceeds size")
	}
	end := min(offset+uint64(length), uint64(len(data)))
	return data[offset:end], end, end == uint64(len(data)), nil
}

func (a *loopback) record(method, id string, to string, chain message.Chain) (*message.SendResult, *errcode.Error) {
	a.mu.Lock()
	op := operation{method: method, id: id, text: chain.Text(), to: to}
	for _, segment := range chain {
		if buttons, ok := segment.(message.ButtonsSegment); ok {
			for _, row := range buttons.Rows {
				op.buttons = append(op.buttons, row...)
			}
		}
	}
	a.operations = append(a.operations, op)
	close(a.changed)
	a.changed = make(chan struct{})
	a.mu.Unlock()
	return &message.SendResult{Status: message.SendAccepted, Session: to, AdapterInstanceID: "main", PrimaryPlatformMessageID: id, PlatformMessageIDs: []string{id}, PartResults: []message.PartResult{{Status: message.PartAccepted, PlatformMessageIDs: []string{id}}}}, nil
}

func (a *loopback) Send(_ context.Context, to string, chain message.Chain) (*message.SendResult, *errcode.Error) {
	return a.record("send", fmt.Sprint(a.sequence.Add(1)), to, chain)
}

func (a *loopback) Edit(_ context.Context, to string, id string, chain message.Chain) (*message.SendResult, *errcode.Error) {
	return a.record("edit", id, to, chain)
}

func (*loopback) Stop(context.Context) error { return nil }

func (a *loopback) snapshot() []operation {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]operation(nil), a.operations...)
}

func (a *loopback) wait(ctx context.Context, ready func([]operation) bool) bool {
	for {
		a.mu.Lock()
		operations := append([]operation(nil), a.operations...)
		changed := a.changed
		a.mu.Unlock()
		if ready(operations) {
			return true
		}
		select { // 等到记录变化或本次测试取消，避免 Provider 与显示回调互相等待。
		case <-changed:
		case <-ctx.Done():
			return false
		}
	}
}

type harness struct {
	ctx   context.Context
	loop  *loopback
	db    *sql.DB
	dir   string
	stop  func() error
	ready *atomic.Uint64 // OnReady 完成次数，区分命令回复与后续重载。
}

func startAgent(t *testing.T, loop *loopback, configuration map[string]any, coreExtra map[string]any, plugins ...*sdk.Plugin) *harness {
	t.Helper()
	return startAgentAt(t, t.TempDir(), loop, configuration, coreExtra, agent.New(), nil, plugins...)
}

func startAgentAt(t *testing.T, dir string, loop *loopback, configuration map[string]any, coreExtra map[string]any, root *sdk.Plugin, args []string, plugins ...*sdk.Plugin) *harness {
	t.Helper()
	ready := &atomic.Uint64{}
	root.OnReady(func(context.Context) error { ready.Add(1); return nil })
	pluginDir := filepath.Join(dir, "plugins", agentID)
	if err := os.MkdirAll(pluginDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(pluginDir, "config.json"), configuration)
	coreConfig := map[string]any{"listen": "127.0.0.1:0", "admins": []string{"agentloop:admin"}, "adapters": map[string]any{loopID: map[string]any{"instances": map[string]any{"main": map[string]any{}}}}}
	for key, value := range coreExtra {
		coreConfig[key] = value
	}
	writeJSON(t, filepath.Join(dir, "core_config.json"), coreConfig)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	logger := slog.Default()
	genresources.Bind(root, agentResources)
	genresources.Bind(loop, loopResources)
	options := []alonabot.Option{alonabot.DataDir(dir), alonabot.Plugin(root), alonabot.Adapter(loop), alonabot.Args(args...)}
	for _, plugin := range plugins {
		options = append(options, alonabot.Plugin(plugin))
	}
	core, err := alonabot.Start(ctx, options...)
	if err != nil {
		cancel()
		slog.SetDefault(logger)
		t.Fatalf("start Agent: %v", err)
	}
	stop := sync.OnceValue(func() error { cancel(); return core.Wait() })
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Errorf("stop Agent: %v", err)
		}
		slog.SetDefault(logger)
	})
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "state.sqlite")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &harness{ctx: ctx, loop: loop, db: db, dir: dir, stop: stop, ready: ready}
}

func (h *harness) waitReload(t *testing.T, before uint64) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for h.ready.Load() == before {
		select {
		case <-ticker.C:
		case <-h.ctx.Done():
			t.Fatal(h.ctx.Err())
		}
	}
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func newLoop(edit bool, limit uint64) *loopback {
	return &loopback{edit: edit, limit: limit, changed: make(chan struct{})}
}

func (h *harness) deliver(t *testing.T, text, user, locale string, toMe bool) {
	t.Helper()
	h.deliverChain(t, message.Chain{message.Text(text)}, user, locale, toMe)
}

func (h *harness) deliverChain(t *testing.T, chain message.Chain, user, locale string, toMe bool) {
	t.Helper()
	h.deliverEvent(t, &message.Event{
		Type: "message.created", Source: message.SourceAdapter, AdapterID: loopID, AdapterInstanceID: "main", AccountID: "bot", Session: loopSession, ChatType: message.ChatGroup,
		Actor: &message.Actor{ExternalUserID: user, AdapterInstanceID: "main", Locale: locale}, Payload: &message.CreatedPayload{PlatformMessageID: fmt.Sprint(h.loop.sequence.Add(1)), Chain: chain, ToMe: toMe},
	})
}

func (h *harness) deliverEvent(t *testing.T, event *message.Event) {
	t.Helper()
	event.SourceEventID = fmt.Sprint(h.loop.sequence.Add(1))
	accepted, err := h.loop.cores[event.AdapterInstanceID].Deliver(h.ctx, event, nil)
	if err != nil || !accepted {
		t.Fatalf("deliver %q: accepted=%v, error=%v", event.Type, accepted, err)
	}
}

func (h *harness) waitText(t *testing.T, text string) []operation {
	t.Helper()
	if !h.loop.wait(h.ctx, func(operations []operation) bool {
		return len(operations) > 0 && operations[len(operations)-1].text == text
	}) {
		t.Fatalf("wait for %q: %v", text, h.loop.snapshot())
	}
	return h.loop.snapshot()
}

func (h *harness) waitTextAfter(t *testing.T, after int, text string) {
	t.Helper()
	if !h.loop.wait(h.ctx, func(operations []operation) bool {
		return len(operations) > after && operations[len(operations)-1].text == text
	}) {
		t.Fatalf("wait for new %q: %v", text, h.loop.snapshot())
	}
}

func (h *harness) entries(t *testing.T) []map[string]string {
	t.Helper()
	// 文本断言沿用字符串视图，原始条目读取保留 images 的数字数组。
	var entries []map[string]string
	for _, stored := range h.readEntries(t) {
		entry := make(map[string]string)
		for key, value := range stored {
			if text, ok := value.(string); ok {
				entry[key] = text
			}
		}
		entries = append(entries, entry)
	}
	return entries
}

func (h *harness) readEntries(t *testing.T) []map[string]any {
	t.Helper()
	return h.readEntriesKey(t, loopSession+"@main")
}

func (h *harness) readEntriesKey(t *testing.T, key string) []map[string]any {
	t.Helper()
	rows, err := h.db.QueryContext(h.ctx, "SELECT entry_json FROM conversation_entry WHERE plugin_id = ? AND conversation_key = ? ORDER BY seq", agentID, key)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var entries []map[string]any
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return entries
}

func (h *harness) waitEntries(t *testing.T, count int) []map[string]string {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		entries := h.entries(t)
		if len(entries) == count+1 {
			if entries[0]["role"] != "system" || entries[0]["kind"] != "memory_snapshot" {
				t.Fatalf("missing initial memory snapshot: %v", entries)
			}
			for _, entry := range entries[1:] {
				if entry["kind"] != "" {
					t.Fatalf("unexpected snapshot in ordinary history: %v", entries)
				}
			}
			return entries[1:]
		}
		select {
		case <-ticker.C:
		case <-h.ctx.Done():
			t.Fatalf("conversation entries = %v, want %d", entries, count)
		}
	}
}

func agentConfig(url string, protocol provider.Protocol) map[string]any {
	return map[string]any{"providers": []any{map[string]any{"custom": map[string]any{"name": "test", "protocol": protocol, "base_url": url, "api_key": "test-key"}}}, "models": []any{map[string]any{"name": "test", "provider": "test", "model": "test-model", "type": "chat"}}, "chat_model": "test"}
}

func writeDelta(w http.ResponseWriter, protocol provider.Protocol, text string) {
	var value any = map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": text}}}}
	if protocol == provider.AnthropicMessages {
		value = map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}}
		fmt.Fprint(w, "event: content_block_delta\n")
	}
	raw, _ := json.Marshal(value)
	fmt.Fprintf(w, "data: %s\n\n", raw)
	w.(http.Flusher).Flush()
}

func writeEnd(w http.ResponseWriter, protocol provider.Protocol) {
	if protocol == provider.AnthropicMessages {
		fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	} else {
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}
	w.(http.Flusher).Flush()
}

// TestAgentConversation 经真实 Core、SDK Session 和 SQLite 验证流显示、历史与断流。
func TestAgentConversation(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		protocol provider.Protocol
		edit     bool
		limit    uint64
		broken   bool
	}{
		{"openai", provider.OpenAI, true, 64, false},
		{"openai-compatible", provider.OpenAICompatible, true, 64, false},
		{"anthropic-messages", provider.AnthropicMessages, true, 64, false},
		{"without-edit", provider.OpenAICompatible, false, 64, false},
		{"without-edit-over-limit", provider.OpenAICompatible, false, 6, false},
		{"interrupted", provider.OpenAICompatible, true, 64, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			loop := newLoop(scenario.edit, scenario.limit)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := calls.Add(1)
				var request struct {
					Model               string `json:"model"`
					MaxTokens           int    `json:"max_tokens"`
					MaxCompletionTokens int    `json:"max_completion_tokens"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				wantModel, wantTokens := "test-model", 2048
				if call == 2 {
					wantModel, wantTokens = "second-model", 37
				}
				tokens := request.MaxTokens
				if scenario.protocol == provider.OpenAI {
					tokens = request.MaxCompletionTokens
				}
				if request.Model != wantModel || tokens != wantTokens {
					t.Errorf("model request = %+v, want %s/%d", request, wantModel, wantTokens)
				}
				if scenario.protocol == provider.AnthropicMessages {
					if r.Header.Get("x-api-key") != "test-key" {
						t.Error("models must share the provider API key")
					}
				} else if r.Header.Get("Authorization") != "Bearer test-key" {
					t.Error("models must share the provider API key")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				for i, text := range []string{"Hello", ", ", "world!"} {
					writeDelta(w, scenario.protocol, text)
					if scenario.edit && i < 2 {
						if !loop.wait(r.Context(), func(operations []operation) bool { return len(operations) > i }) {
							return
						}
					}
					if scenario.broken {
						return // 缺少 finish_reason，模拟已经显示文本后的断流。
					}
				}
				writeEnd(w, scenario.protocol)
			}))
			t.Cleanup(server.Close)
			configuration := agentConfig(server.URL, scenario.protocol)
			configuration["models"] = append(configuration["models"].([]any), map[string]any{"name": "second", "provider": "test", "model": "second-model", "type": "chat", "max_tokens": 37})
			h := startAgent(t, loop, configuration, nil)
			h.deliver(t, "/ask hello", "member", "zh-CN", false)
			if scenario.broken {
				operations := h.waitText(t, "Provider 请求失败（ERROR_CODE_UNAVAILABLE）。")
				if len(operations) != 2 || operations[0].text != "Hello" || operations[0].method != "send" {
					t.Fatalf("partial content was not preserved: %v", operations)
				}
				entries := h.entries(t)
				if len(entries) != 1 || entries[0]["role"] != "system" || entries[0]["kind"] != "memory_snapshot" || entries[0]["content"] != "" {
					t.Fatalf("failed turn must retain only the empty snapshot: %v", entries)
				}
			} else {
				entries := h.waitEntries(t, 2)
				operations := loop.snapshot()
				if entries[0]["role"] != "user" || entries[0]["speaker"] != "member" || entries[0]["content"] != "hello" || entries[1]["role"] != "assistant" || entries[1]["content"] != "Hello, world!" {
					t.Fatalf("conversation entries = %v", entries)
				}
				if scenario.edit {
					if len(operations) < 3 || operations[0].method != "send" || operations[0].text != "Hello" {
						t.Fatalf("missing initial send or edits: %v", operations)
					}
					for _, op := range operations[1:] {
						if op.method != "edit" || op.id != operations[0].id {
							t.Errorf("stream changed the message ID: %v", operations)
						}
					}
					if operations[len(operations)-1].text != "Hello, world!" {
						t.Errorf("final edit = %v", operations)
					}
				} else {
					var combined strings.Builder
					for _, op := range operations {
						if op.method != "send" || uint64(utf8.RuneCountInString(op.text)) > scenario.limit {
							t.Errorf("non-editing output exceeds instance limit or edits: %v", operations)
						}
						combined.WriteString(op.text)
					}
					if combined.String() != "Hello, world!" || (scenario.limit == 64 && len(operations) != 1) || (scenario.limit == 6 && len(operations) < 2) {
						t.Errorf("non-editing output = %v", operations)
					}
				}
			}
			wantCalls := int32(1)
			if !scenario.broken {
				generation := h.ready.Load()
				h.deliver(t, "/provider second", "admin", "en-US", false)
				h.waitText(t, "Profile updated: 默认 (1 sessions).\nThe Agent will reload shortly.")
				h.waitReload(t, generation)
				h.deliver(t, "/ask next", "member", "en-US", false)
				h.waitEntries(t, 4)
				wantCalls = 2
			}
			if calls.Load() != wantCalls {
				t.Errorf("provider requests = %d, want %d", calls.Load(), wantCalls)
			}
		})
	}
}

func (h *harness) click(t *testing.T, op operation, allow bool, user string) {
	t.Helper()
	index := 0
	if !allow {
		index = 1
	}
	accepted, err := h.loop.core.Deliver(h.ctx, &message.Event{Type: "button.clicked", Source: message.SourceAdapter, AdapterID: loopID, AdapterInstanceID: "main", AccountID: "bot", Session: loopSession, ChatType: message.ChatGroup, Actor: &message.Actor{ExternalUserID: user, AdapterInstanceID: "main", Locale: "en-US"}, Payload: &message.ButtonClickedPayload{Value: op.buttons[index].CoreToken, CallbackQueryID: fmt.Sprint(h.loop.sequence.Add(1)), OriginPlatformMessageID: op.id}}, nil)
	if err != nil || !accepted {
		t.Fatalf("click confirmation: accepted=%v, error=%v", accepted, err)
	}
}

func (h *harness) confirmation(t *testing.T, after int) operation {
	t.Helper()
	var found operation
	if !h.loop.wait(h.ctx, func(operations []operation) bool {
		for _, op := range operations[after:] {
			if len(op.buttons) == 2 {
				found = op
				return true
			}
		}
		return false
	}) {
		t.Fatalf("wait confirmation: %v", h.loop.snapshot())
	}
	return found
}

func (h *harness) deliverTo(t *testing.T, instance, session, text, user string, chatType message.ChatType, toMe bool) {
	t.Helper()
	account := "bot"
	if instance != "main" {
		account += "-" + instance
	}
	h.deliverEvent(t, &message.Event{Type: "message.created", Source: message.SourceAdapter, AdapterID: loopID, AdapterInstanceID: instance, AccountID: account, Session: session, ChatType: chatType, Actor: &message.Actor{ExternalUserID: user, AdapterInstanceID: instance, Locale: "en-US"}, Payload: &message.CreatedPayload{PlatformMessageID: fmt.Sprint(h.loop.sequence.Add(1)), Chain: message.Chain{message.Text(text)}, ToMe: toMe}})
}

func (h *harness) waitEntriesKey(t *testing.T, key string, count int) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for len(h.readEntriesKey(t, key)) != count {
		select {
		case <-ticker.C:
		case <-h.ctx.Done():
			t.Fatalf("conversation %s entries=%v, want %d", key, h.readEntriesKey(t, key), count)
		}
	}
}

func (a modelAdmin) createProfile(t *testing.T, name string, overrides map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"name": name, "color": "teal"})
	if err != nil {
		t.Fatal(err)
	}
	_, body := a.request(t, http.MethodPost, "/api/v1/profiles", string(raw), http.StatusOK)
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(map[string]any{"overrides": overrides})
	if err != nil {
		t.Fatal(err)
	}
	a.request(t, http.MethodPatch, "/api/v1/profiles/"+created.ID+"/plugins/"+agentID, string(raw), http.StatusOK)
	return created.ID
}

func (a modelAdmin) assignProfile(t *testing.T, instance, session, profile string) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"instance": instance, "session": session, "profile": profile})
	if err != nil {
		t.Fatal(err)
	}
	a.request(t, http.MethodPut, "/api/v1/profiles/assignments/sessions", string(raw), http.StatusOK)
}

func (a modelAdmin) profileOverrides(t *testing.T, id string) map[string]any {
	t.Helper()
	_, body := a.request(t, http.MethodGet, "/api/v1/profiles/"+id+"/plugins", "", http.StatusOK)
	var plugins []struct {
		PluginID  string         `json:"plugin_id"`
		Overrides map[string]any `json:"overrides"`
	}
	if err := json.Unmarshal(body, &plugins); err != nil {
		t.Fatal(err)
	}
	for _, plugin := range plugins {
		if plugin.PluginID == agentID {
			return plugin.Overrides
		}
	}
	t.Fatal("Agent missing from profile plugins")
	return nil
}
