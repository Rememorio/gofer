# Model Length Caps

Providers signal output-budget exhaustion through values such as OpenAI's
`finish_reason: length` or Anthropic's `stop_reason: max_tokens`. Gofer's model
adapter normalizes these signals to `max_tokens`, then applies a narrow runtime
rule that distinguishes useful partial output from unsafe incomplete intent.

## Terminal Partial Text

When a max-token response contains non-whitespace text and no tool calls,
Gofer preserves the content byte-for-byte and promotes its terminal reason to
`model_length_capped`. The assistant message, provider usage, and run completion
are journaled normally. Run responses, `run.completed`, token-usage summaries,
and delegated-agent metadata all expose the capped reason, so clients can show
the partial answer while making its status unambiguous.

Gofer does not attempt to infer missing prose, reparse markup as a tool call, or
automatically continue generation. A caller can explicitly request continuation
in a later turn with full durable context.

## Unsafe Capped Shapes

Two shapes remain hard provider-truncation failures:

- A max-token response with no visible text cannot produce a valid assistant
  outcome.
- A max-token response carrying any tool call may contain incomplete arguments.
  It is rejected before assistant journaling, `tool.started`, or execution,
  even if the arguments happen to parse as JSON.

Content-filter stops are a separate safety category and are not relabeled as
length caps. `GET /api/features` reports this always-on behavior as
`model_length_reason.enabled`.
