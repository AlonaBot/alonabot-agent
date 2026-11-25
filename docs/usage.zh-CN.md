# Agent 使用

[English](usage.md)

安装后，在 WebUI 的模型设置中添加 Provider，填写名称、协议、地址及 API key，再添加引用该 Provider 的聊天模型并选择 `chat_model`。Provider 协议支持 `openai`、`openai-compatible`、`anthropic-messages`、`openai-responses`、`gemini-generate-content` 与 `ollama-chat`，以 [plugin_config_schema.json](../plugin_config_schema.json) 为配置依据。

## 对话与模型

发送 `/ask 你好` 发起对话。自由对话由当前 Profile 的 `triggers` 控制：`mention`、`wake_word`、`private`，默认空列表；`wake_words` 提供唤醒词。管理员可用 `/agent on`、`/agent off` 切换自由对话，`/provider` 查看或选择聊天模型，`/persona` 查看或选择人设，`/profile` 查看或选择配置文件；`/reset` 清空当前对话。

`history_entries` 默认为 20，`member_sessions` 默认为 false，可按成员分开群聊。`max_attempts` 默认为 3，包含首次调用；`fallback_chat_models` 指定备用聊天模型。`image_caption_model` 选择支持图片的聊天模型，留空保留图片占位；`image_caption_prompt` 指定转述提示。Profile 可覆盖的字段以 Schema 中的 `profile` 标记为准。

## 引用节点与图片

普通触发和 `/ask` 共用完整消息链，递归读取合并转发的每个节点，保留作者、时间、正文及图片来源路径。引用节点按 Core 的当前会话授权读取；图片经 `media.Open` 取件后交给 Provider，节点作者作为当前用户提供的引用来源进入请求。

支持图片输入的模型收到实际图片与来源对应关系；文本模型沿用图片转述，转述附节点路径。历史保存节点文字及图片归属，图片缓存过期时保留原路径并标明不可用。节点或图片读取失败时回复具体错误并停止本次模型请求，完整错误进入日志。模型的 `image_input` 需与实际能力一致。

## 工具与扩展

`mcp_servers` 支持 `stdio` 和 `streamable-http`。前者配置 command、args 和 secret env；后者配置 URL 和 secret headers。只配置可信服务并按实际权限开放工具。`skills_dir` 留空关闭 skills，相对路径基于插件工作目录，编辑文件后使用 Reload skills。`llm_service_allowed_plugins` 指定可调用文本生成服务的插件，默认 `["*"]`。

管理页面提供人设、记忆、会话及用量等入口。Provider 公共包位于 `provider/`，业务实现位于 `internal/agent/`；开发入口见[开发指南](development.zh-CN.md)。

## 运行日志

INFO 记录模型请求、完成耗时与用量，以及工具调用开始和完成摘要；请求正文、模型输出、工具参数与结果均省略。失败记录保留错误分类或具体错误，MCP 错误沿用凭据脱敏。可在 Core 日志页展开「详情」查看结构化字段，并用 Trace 串联事件、模型、工具与发送记录。
