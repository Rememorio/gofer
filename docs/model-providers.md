# Model Providers

Gofer keeps the runtime independent from provider SDKs. Every adapter accepts
the same normalized messages, tool definitions, generation controls, and
stream contract, then returns text deltas, complete validated tool calls,
provider usage, and one normalized stop reason.

## Supported Protocols

| `provider` | Protocol | Typical endpoints |
| --- | --- | --- |
| `openai` | OpenAI Chat Completions | OpenAI, OpenRouter, vLLM, and compatible gateways |
| `anthropic` | Native Anthropic Messages | Anthropic and Messages-compatible endpoints |

Multiple entries may use either provider. The first entry is the
`lead_agent`; its `name`, or any other configured alias, can be selected as an
assistant or through run configuration. Model discovery includes the provider
name but never API keys, bearer tokens, or private headers.

```yaml
models:
  - name: fast
    provider: openai
    model: gpt-5-mini
    api_key: $OPENAI_API_KEY

  - name: claude
    provider: anthropic
    model: claude-sonnet-4-6
    api_key: $ANTHROPIC_API_KEY
    max_tokens: 8192
```

An OpenAI-compatible gateway is selected with `base_url`. Native Anthropic
accepts either `api_key`, which emits `X-Api-Key`, or `auth_token`, which emits
Bearer authorization. They are mutually exclusive. When credentials or the
endpoint are omitted, the official SDK's environment and credential chain is
used.

`models[].max_tokens` supplies a per-alias default when a run does not provide
`context.max_tokens`; a per-run value always wins. Anthropic requires an output
limit and uses 8192 when neither level specifies one. Anthropic temperature is
limited to the native zero-to-one range even though the normalized API also
accommodates providers that accept values through two.

## Normalized Messages and Tools

Both adapters preserve text, images, assistant tool calls, tool results, and
the top-level system prompt. The Anthropic adapter maps durable tool-result
messages back to user-role `tool_result` blocks as required by Messages and
supports both HTTPS image URLs and validated base64 data URLs.

Anthropic `tool_use` input may arrive across many `input_json_delta` events.
Gofer buffers each indexed block independently, validates the complete JSON,
orders parallel calls by content-block index, and emits no executable call
until its block closes. Unknown server-side content blocks fail explicitly;
they are never mistaken for local tools.

Provider token counts remain authoritative. Anthropic input, output,
reasoning, cache-read, and cache-creation fields map to the same usage journal
used by OpenAI-compatible calls, subagents, and context compaction.

## Stop Reasons

| Anthropic stop reason | Gofer reason |
| --- | --- |
| `end_turn`, `stop_sequence` | `end_turn` |
| `tool_use` | `tool_use` |
| `max_tokens`, `model_context_window_exceeded` | `max_tokens` |
| `refusal` | `content_filter` |

The always-on runtime guards then promote safe terminal outcomes to
`model_length_capped` or `safety_capped`. A tool call accompanying a length or
refusal stop may cross the adapter boundary only so the guard can suppress or
reject it before journaling and execution. Invalid IDs, names, JSON, event
ordering, and ordinary end turns paired with calls remain protocol failures.

`pause_turn` is not silently converted into completion because it requires a
provider-specific continuation exchange. The current adapter returns an
explicit protocol error instead.

The `openai` adapter currently targets Chat Completions. OpenAI Responses API,
provider-managed server tools, and CLI-backed account sessions remain separate
provider modules rather than compatibility flags on these adapters.
