# alonabot-agent 0.4.0-dev.0 - Unreleased

### Added

- Rich messaging through `send_message` and on-demand format discovery through `get_message_format`, including declared platform extensions, forwarded nodes, and visible choice buttons.
- Attachment reuse and administrator-confirmed local file uploads. Local file reads always require confirmation, including under an allow sending policy.

### Changed

- Interactive and scheduled conversations deliver body segments around rich messages and retain ordered text summaries and complete delivery receipts in history.
- Scheduled body splitting uses the target's current outbound description. This feature requires the Core SDK outbound description, message check, and required confirmation APIs.
