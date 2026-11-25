# alonabot-agent 0.2.0 - 2026-11-22

### Added

- Read quoted and forwarded-message nodes recursively for ordinary conversations and `/ask`, retaining authors, timestamps, text, image sources and node paths.
- Send actual images to vision models; text models receive captions with source paths. Preserve complete read errors and stop the affected model request.
- `/ask`, `/persona`, `/provider`, `/profile` and `/reset` appear in the automatic command menu.

This release adopts the single-archive format and the MIT License. Installing it requires the archive installer introduced in Core v0.2.0.
