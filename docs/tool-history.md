# Tool History Repair

Strict model providers require every assistant tool call to be followed by one
matching tool result. Real execution can violate that wire-level rule without
corrupting durable state: a user may cancel after the assistant message is
journaled but before its tool finishes, or compaction may retain only one side
of an older call/result pair.

Gofer repairs the transcript immediately before each provider invocation. It
runs after context compaction and all other temporary model-context middleware,
so it validates the exact message sequence that will cross the provider
boundary.

## Repair Rules

For each model request, Gofer builds a bounded index of result messages and
then emits a provider-safe sequence:

1. Existing results are grouped immediately after the assistant message that
   declared their call IDs and ordered to match that message's calls.
2. A call with no result receives a synthetic error result containing
   `code: interrupted_tool_call` and `recoverable: true`.
3. A result with no visible call is an orphan and is omitted. Extra results for
   an already-answered call are omitted as well.
4. Normalized assistant calls must already have non-empty names, unique IDs,
   and valid JSON arguments. Invalid new responses are rejected before they can
   enter history.

The synthetic message is tagged `internal_kind: tool_result_recovery`. It is
valid normalized tool content, so OpenAI-compatible providers receive exactly
the causal structure they require and the model can choose an alternative
action instead of failing with a protocol error.

## Durability and Bounds

Repair is a view, not a state mutation. Synthetic results and reordered views
are never appended to the event journal, returned by conversation endpoints,
or copied into a later run. Original assistant calls, completed results, and
the absence of interrupted results remain available for audit.

One repair pass accepts at most 10,000 messages and 10,000 assistant tool calls
and uses linear memory in that bounded request. Lead and child agents share the
same final middleware. `GET /api/features` reports the always-on capability as
`tool_history_repair.enabled`.
