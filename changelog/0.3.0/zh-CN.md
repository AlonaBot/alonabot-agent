# alonabot-agent 0.3.0 - 2026-11-23

### 变更

- 配置 Schema 文件改名为 `plugin_config_schema.json`，与 Core v0.3.0 一致。见 [PR #8](https://github.com/AlonaBot/alonabot-agent/pull/8)。
- 人格、MCP、技能、记忆与用户任务页面加载时显示骨架，去掉多余留白；条目不足一页时隐藏计数与分页。见 [PR #9](https://github.com/AlonaBot/alonabot-agent/pull/9)。
- 首页精简，补齐中英文维护规范。

### 发行与升级

- SDK 与打包器固定到 Core 提交 `25a0f851099f`（`v0.2.1-0.20261123105642-25a0f851099f`）。
- 旧版 Agent 不能在 Core v0.3.0 上运行：先停用 Agent，升级 Core v0.3.0，再经管理端更新到本版并启用；配置值、秘密和数据保留。
