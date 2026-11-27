# Agent 使用

[English](usage.md)

安装后，在 WebUI 的模型设置中添加 Provider，填写名称、协议、地址及 API key，再添加引用该 Provider 的聊天模型并选择 `chat_model`。Provider 协议支持 `openai`、`openai-compatible`、`anthropic-messages`、`openai-responses`、`gemini-generate-content` 与 `ollama-chat`，以 [plugin_config_schema.json](../plugin_config_schema.json) 为配置依据。

## 对话与模型

发送 `/ask 你好` 发起对话，发送 `/new`（别名 `/reset`）清空当前会话。自由对话由当前 Profile 的 `triggers` 控制：`mention`、`wake_word`、`private`，默认空列表；`wake_words` 提供唤醒词。管理员可用 `/agent on`、`/agent off` 切换自由对话，`/provider` 查看或选择聊天模型，`/persona` 查看或选择人设，`/profile` 查看或选择配置文件。

`history_entries` 默认为 20，`member_sessions` 默认为 false，可按成员分开群聊。`max_attempts` 默认为 3，包含首次调用；`fallback_chat_models` 指定备用聊天模型。`image_caption_model` 选择支持图片的聊天模型，留空保留图片占位；`image_caption_prompt` 指定转述提示。Profile 可覆盖的字段以 Schema 中的 `profile` 标记为准。

## 引用节点与图片

普通触发和 `/ask` 共用完整消息链，递归读取合并转发的每个节点，保留作者、时间、正文及图片来源路径。引用节点按 Core 的当前会话授权读取；图片经 `media.Open` 取件后交给 Provider，节点作者作为当前用户提供的引用来源进入请求。

支持图片输入的模型收到实际图片与来源对应关系；文本模型沿用图片转述，转述附节点路径。历史保存节点文字及图片归属，图片缓存过期时保留原路径并标明不可用。节点或图片读取失败时回复具体错误并停止本次模型请求，完整错误进入日志。模型的 `image_input` 需与实际能力一致。

## 工具与扩展

Agent 用固定的 `send_message` 发送富消息，用 `get_message_format` 查询当前目标的格式、限制和组合。普通文字直接作为正文；图片、视频、语音、文件、提及、引用、合并转发、按钮和 Adapter 扩展按展示顺序放在同一逻辑消息中。工具先预检，再提交一次；明确不支持的格式会返回具体错误，声明未知时仍会尝试。

媒体来源可用 HTTPS URL、已上传的 `media_ref`，或本轮附件路径（例如 `message[0]`、`message[1].nodes[0].chain[2]`）。交互会话中的 Core 管理员还可用绝对 `local_path`：Agent 展示完整路径和目标，获得确认后才读取并上传。即使发送工具策略为 allow，本地文件也需要确认；定时任务和其他用户使用前三种来源，定时任务没有本轮附件。

扩展按用途查询，例如 `get_message_format({"query":"music"})`，有 `next_cursor` 时继续翻页；选中名称后用 `{"type":"名称"}` 读取各声明版本、字段和可发送示例。组合查询可用 `{"type":"buttons","combine_with":["image"]}`。平台会决定相册、转发与按钮如何投递。

`send_message` 会先结束并投递前一段正文，再发送富消息，后续正文另起一段。定时正文按运行时查询到的文本限额分块。历史依次保存已投递的正文和富消息文字摘要，富消息摘要仅含格式、说明、按钮标签和回执。回执保留物理投递的片段下标、句柄、错误与状态；只有 `rejected`、`not_attempted` 标记 `resend_safe`。结果为 `unknown` 时提示模型说明结果未知并停止补发。已发内容提示模型避免在正文复述。

链接按钮直接打开 URL；选项按钮把可见标签作为点击者的新一轮输入，使用点击者当前的权限与正常工具确认。`/new` 后点击旧选项仍按普通文字处理。

`agent/shell` 向交互会话中的管理员开放，在 Core 主机的 Agent 数据目录中执行 shell 命令：Unix 使用 `sh -c`，Windows 使用 `cmd /C`，以 JSON 返回完整 stdout、stderr 和退出码，并随调用上下文取消或到期。在 WebUI 工具设置中可选择 allow、confirm 或 deny 策略。

`mcp_servers` 支持 `stdio` 和 `streamable-http`。前者配置 command、args 和 secret env；后者配置 URL 和 secret headers。只配置可信服务并按实际权限开放工具。`skills_dir` 留空关闭 skills，相对路径基于插件工作目录，编辑文件后使用 Reload skills。`llm_service_allowed_plugins` 指定可调用文本生成服务的插件，默认 `["*"]`。

管理页面提供人设、记忆、会话及用量等入口。Provider 公共包位于 `provider/`，业务实现位于 `internal/agent/`；开发入口见[开发指南](development.zh-CN.md)。
