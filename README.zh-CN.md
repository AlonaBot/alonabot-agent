# AlonaBot 官方 Agent

[English](README.md)

支持模型、人设、工具及引用和转发图片输入的对话 Agent。

## 快速使用

使用 Core v0.3.0 或更高版本，在运行 Core 的机器上执行：

```fish
alonabot install --source AlonaBot/alonabot-agent --yes
```

配置 Provider 与聊天模型后，发送 `/ask 你好`。

通过 `alonabot-plugin-builtin-official` 提供的 `/new`（别名 `/reset`）清空当前会话，Agent 向该插件开放 `conversation.reset` 服务。

管理员可使用 `agent/shell` 工具在 Core 主机的 Agent 数据目录中运行命令，获取完整 stdout、stderr 和退出码。在 WebUI 工具设置中可将策略改为 allow、confirm 或 deny。

## 文档

- [使用与配置](docs/usage.zh-CN.md)
- [开发与检查](docs/development.zh-CN.md)
- [打包与发布](docs/releasing.zh-CN.md)
- [AlonaBot 文档](https://alonabot.github.io/)

## 贡献

[贡献指南](CONTRIBUTING.zh-CN.md)

## 许可

[MIT](LICENSE)
