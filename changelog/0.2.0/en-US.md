# alonabot-agent 0.2.0 - 2026-11-22

### Added

- Read quoted and forwarded-message nodes recursively for ordinary conversations and `/ask`, retaining authors, timestamps, text, image sources and node paths.
- Send actual images to vision models; text models receive captions with source paths. Preserve complete read errors and stop the affected model request.

### Distribution and upgrade

- Publish five `.tar.gz` platform archives, including Windows amd64; GitHub supplies source archives. Standalone `release.json`, signatures and public-key assets are retired.
- Use the SDK and packager from reviewed Core commit `2dbbded7184f` (`v0.1.2-0.20261122021903-2dbbded7184f`), which contains the v0.2.0 archive installation flow. The component uses the MIT License.
- Upgrade Core to v0.2.0 before installing or updating this release. Stop the component and back up its data before replacement; retain the same component ID, source and data directory. Historical sidecar cleanup follows verification of Core and WebUI.

Automatic menu entries for `/ask`, `/persona`, `/provider`, `/profile` and `/reset` are explicitly marked with `.Menu()`.
