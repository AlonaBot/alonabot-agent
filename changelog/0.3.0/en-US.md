# alonabot-agent 0.3.0 - 2026-11-23

### Changed

- Rename the configuration Schema file to `plugin_config_schema.json` to match Core v0.3.0. See [PR #8](https://github.com/AlonaBot/alonabot-agent/pull/8).
- Persona, MCP, skill, memory and user task pages show skeletons while loading and drop extra padding; counts and pagination are hidden when everything fits on one page. See [PR #9](https://github.com/AlonaBot/alonabot-agent/pull/9).
- Trim the README and complete the bilingual maintenance guide.

### Distribution and upgrade

- The SDK and packager are pinned to Core commit `25a0f851099f` (`v0.2.1-0.20261123105642-25a0f851099f`).
- Earlier Agent releases do not run on Core v0.3.0: disable the Agent, upgrade Core to v0.3.0, then update to this release through the management API and enable it. Configuration values, secrets and data are retained.
