# Safety Finish Reasons

OpenAI-compatible providers can stop a response with
`finish_reason: content_filter`. Some gateways still include a partially
generated tool call in that response. Even when its accumulated arguments are
valid JSON, executing it is unsafe: the provider may have stopped before the
model completed the intended operation or content.

Gofer normalizes the provider signal to `content_filter`, then applies an
always-on response guard before validation, loop detection, assistant
journaling, and tool execution.

## Response Repair

The guard produces one of three visible, tool-free outcomes:

- Existing refusal or safety text with no tool calls is preserved exactly.
- An empty response receives a neutral explanation that the provider stopped
  generation and suggests rephrasing the request.
- A response carrying tool calls has every call removed and receives an
  explanation that the calls were suppressed. Existing visible text is kept
  before that explanation.

All three outcomes preserve provider usage and complete with
`stop_reason: safety_capped`. The durable assistant message is tagged
`internal_kind: safety_termination`; lead runs expose the reason in
`run.completed`, and delegated-agent results carry it in metadata.

## Adapter and Runtime Boundaries

The OpenAI-compatible stream adapter permits valid accumulated tool calls to
reach the normalized response when the finish reason is `content_filter` or
`length`. It continues rejecting missing IDs, names, invalid JSON, changed
identities, unsupported tool types, and normal `stop` responses paired with
tool calls.

This narrow exception lets the runtime apply the authoritative safety policy
without weakening the normalized tool schema. Invalid or incomplete JSON never
becomes a `ToolCall`; valid but potentially truncated calls are observable only
inside the response guard and never appear in durable conversation history.

Content filtering is distinct from prompt-injection guardrails: finish-reason
handling responds to a provider termination signal, while input/result
guardrails neutralize untrusted structural tokens before model invocation.
`GET /api/features` reports this behavior as
`safety_finish_reason.enabled`.
