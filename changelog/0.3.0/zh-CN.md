# alonabot-agent 0.3.0 - 2026-11-23

### 变更

- 配置 Schema 文件改名为 `plugin_config_schema.json`，与 Core v0.3.0 一致 ([#8](https://github.com/AlonaBot/alonabot-agent/pull/8))。
- 人格、MCP、技能、记忆与用户任务页面加载时显示骨架，去掉多余留白；条目不足一页时隐藏计数与分页 ([#9](https://github.com/AlonaBot/alonabot-agent/pull/9))。
- 如果正在使用旧版 Agent，请先停用，再更新到 Core v0.3.0；随后通过管理端更新 Agent 并重新启用。配置、秘密和数据会保留。
