# Using the Agent

[简体中文](usage.zh-CN.md)

After installation, add a provider in the WebUI model settings with a name, protocol, base URL, and API key. Add a chat model referencing that provider and select it as `chat_model`. Provider protocols are `openai`, `openai-compatible`, `anthropic-messages`, `openai-responses`, `gemini-generate-content`, and `ollama-chat`. [plugin_config_schema.json](../plugin_config_schema.json) defines the configuration.

## Conversations and models

Send `/ask Hello` to start a conversation, and `/new` (alias `/reset`) to clear the current conversation. The current Profile's `triggers` controls free conversation: `mention`, `wake_word`, and `private`; the default list is empty. `wake_words` supplies wake words. Administrators can use `/agent on` and `/agent off` to toggle free conversation, `/provider` to list or choose a chat model, `/persona` to list or choose a persona, and `/profile` to view or select a configuration profile.

`history_entries` defaults to 20. `member_sessions` defaults to false and can separate group conversations by member. `max_attempts` defaults to 3, including the first request; `fallback_chat_models` lists alternate chat models. `image_caption_model` selects a chat model that accepts images; leave it empty to retain image placeholders. `image_caption_prompt` supplies caption instructions. Fields marked `profile` in the schema support Profile overrides.

## Quoted nodes and images

Ordinary triggers and `/ask` use the complete message chain, recursively reading every forwarded node while preserving authors, timestamps, text, and image source paths. Core authorizes quoted-node reads for the current conversation. The Agent opens images with `media.Open` and passes them to the provider; node authors enter the request as sources quoted by the current user.

Models configured for image input receive the actual images and their source mapping. Text models receive captions with node paths. History retains node text and image ownership; expired image caches retain the original paths with an unavailable marker. A node or image read failure replies with the specific error, stops that model request, and records the complete error in logs. Set the model's `image_input` to match its actual capability.

## Tools and extensions

The Agent uses the fixed `send_message` tool to send rich messages and `get_message_format` to inspect the current target's formats, limits, and combinations. Plain replies stay in the response body. Images, video, voice, files, mentions, quotes, forwarded nodes, buttons, and declared Adapter extensions form one logical message in display order. The tool checks the message before submitting it once. Explicitly unsupported formats return an error; unknown declarations still allow an attempt.

Media sources are HTTPS URLs, uploaded `media_ref` tokens, or this round's attachment paths, such as `message[0]` or `message[1].nodes[0].chain[2]`. Interactive Core administrators can also use an absolute `local_path`: the Agent shows every full path and the destination, then reads and uploads only after confirmation. Local files require confirmation even under an allow tool policy. Scheduled tasks and other users use the first three sources; scheduled tasks have no current attachments.

Discover extensions by purpose with `get_message_format({"query":"music"})` and follow `next_cursor`. Request a chosen name with `{"type":"name"}` to get every declared version, its fields, and a sendable example. Inspect combinations with `{"type":"buttons","combine_with":["image"]}`. The platform determines album, forward, and button delivery rules.

Before `send_message`, the preceding body segment finishes delivery; subsequent body text starts a new segment. Scheduled body text uses the limits queried at runtime. History records delivered body text and readable rich summaries in order. Rich summaries contain formats, captions, button labels, and receipts. Receipts preserve each physical delivery's segment indexes, references, errors, and status; only `rejected` and `not_attempted` carry `resend_safe`. Instructions tell the model to report unknown delivery without resending, and to avoid repeating already sent content in the body.

Rich-message sends preserve the original `parts` indexes. If a `BeforeSend` hook changes the message chain, Core rejects the send with `FAILED_PRECONDITION` before platform delivery; the tool result and rich history retain the rejected receipt and error. Body streaming keeps its existing hook behavior.

Link buttons open their URL. Choice buttons submit the visible label as a new turn under the clicking user's current permissions and normal tool confirmation. Clicking an old choice after `/new` still submits ordinary text.

`agent/shell` is available to administrators in interactive conversations. It runs shell commands on the Core host in the Agent's data directory using `sh -c` on Unix or `cmd /C` on Windows, returning complete stdout, stderr, and the exit code as JSON. It follows the call's cancellation and deadline. The WebUI tool settings let administrators choose allow, confirm, or deny.

`mcp_servers` supports `stdio` and `streamable-http`. Configure command, args, and secret env for stdio; configure URL and secret headers for HTTP. Use trusted services and grant the permissions they need. An empty `skills_dir` disables skills; relative paths use the plugin working directory. Use Reload skills after editing files. `llm_service_allowed_plugins` selects plugins allowed to call text generation and defaults to `["*"]`.

Management pages expose personas, memory, conversations, and usage. The public provider package is `provider/`, and the implementation is `internal/agent/`. See [development](development.md).
