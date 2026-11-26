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

`agent/shell` is available to administrators in interactive conversations. It runs shell commands on the Core host in the Agent's data directory using `sh -c` on Unix or `cmd /C` on Windows, returning complete stdout, stderr, and the exit code as JSON. It follows the call's cancellation and deadline. The WebUI tool settings let administrators choose allow, confirm, or deny.

`mcp_servers` supports `stdio` and `streamable-http`. Configure command, args, and secret env for stdio; configure URL and secret headers for HTTP. Use trusted services and grant the permissions they need. An empty `skills_dir` disables skills; relative paths use the plugin working directory. Use Reload skills after editing files. `llm_service_allowed_plugins` selects plugins allowed to call text generation and defaults to `["*"]`.

Management pages expose personas, memory, conversations, and usage. The public provider package is `provider/`, and the implementation is `internal/agent/`. See [development](development.md).
