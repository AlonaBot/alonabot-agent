# alonabot-agent 0.3.1 - 2026-11-23

### Fixed

- Persona, task, memory and other pages now wait for their text to load before showing the toolbar, so keys such as `pages.search` no longer flash. See [PR #12](https://github.com/AlonaBot/alonabot-agent/pull/12).
- The name field in the new-persona dialog now uses the page kit style, and the target filter on the task page no longer takes up a full row. See [PR #12](https://github.com/AlonaBot/alonabot-agent/pull/12).

### Distribution and upgrade

- Works with Core v0.3.0. You can update directly from 0.3.0; configuration values, secrets and data are kept.
