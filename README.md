# AlonaBot Agent

[简体中文](README.zh-CN.md)

Conversational agent with configurable models, personas, tools, and quoted or forwarded image input.

## Quick start

Use Core v0.3.0 or later. On the machine running Core:

```fish
alonabot install --source AlonaBot/alonabot-agent --yes
```

Configure a provider and chat model, then send `/ask Hello`.

Use `/new` (alias `/reset`) from `alonabot-plugin-builtin-official` to clear the current conversation. The Agent exposes the `conversation.reset` service for that plugin.

Administrators can use the `agent/shell` tool to run commands on the Core host in the Agent's data directory and receive complete stdout, stderr, and the exit code. Set its policy to allow, confirm, or deny in the WebUI tool settings.

## Documentation

- [Usage and configuration](docs/usage.md)
- [Development and checks](docs/development.md)
- [Packaging and releases](docs/releasing.md)
- [Core documentation](https://github.com/AlonaBot/AlonaBot/blob/main/docs/index.md)

## Contributing

[Contribution guide](CONTRIBUTING.md)

## License

[MIT](LICENSE)
