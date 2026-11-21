# AlonaBot 官方 Agent

[![Go component](https://github.com/AlonaBot/alonabot-agent/actions/workflows/ci.yml/badge.svg)](https://github.com/AlonaBot/alonabot-agent/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

官方 Agent通过 AlonaBot SDK 与 Core 通信，独立安装、升级和替换。组件版本为 `0.1.0`。
## 引用节点与图片

普通触发和 `/ask` 共用完整消息链，递归读取合并转发的每个节点，保留作者、时间、正文及图片来源路径。引用节点按 Core 的当前会话授权读取；图片经 `media.Open` 取件后交给 Provider，节点作者作为当前用户提供的引用来源进入请求。

支持图片输入的模型收到实际图片与来源对应关系；文本模型沿用图片转述，转述附节点路径。历史保存节点文字及图片归属，图片缓存过期时保留原路径并标明不可用。节点或图片读取失败时回复具体错误并停止本次模型请求，完整错误进入日志。

## 开发

需要 Go 1.27.1。SDK 版本固定在 `go.mod`；GORM 使用上游正式版本。
```sh
go mod download
go test ./...
go build ./cmd/alonabot-agent
```

使用 Core CLI 从工程根目录打包：

```sh
alonabot check .
alonabot pack . --target linux/amd64 --out bin
```

[Core 开发指南](https://github.com/AlonaBot/AlonaBot/blob/main/docs/development.md)说明协议、调试和整合验证；[插件指南](https://github.com/AlonaBot/AlonaBot/blob/main/docs/plugin-guide.md)说明依赖和存储约定。

## 贡献

向本仓库提交组件变更，PR 标题采用 `fix/scope: 中文摘要` 等 `type/scope: 摘要` 形式。代码注释与文档使用简体中文，程序日志和错误使用英文。新依赖须先取得维护者批准；按影响运行检查并在 PR 中记录结果。

## 许可

采用 [MIT](LICENSE)。

## 目录

根包提供公开构造入口，`internal/agent/` 保存组件实现。`plugin.yaml`、Schema 与 i18n 是工程打包入口。

## 安装与发行

在运行中的 Core 上执行 `alonabot install --source AlonaBot/alonabot-agent --yes`。发行资产包含五平台包、`release.json`、签名和公钥。版本说明维护在 `changelog/<版本>/`。
