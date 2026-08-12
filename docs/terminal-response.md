# Terminal Responses

Some providers return a valid `end_turn` after tool execution but supply no
visible text. Treating that as success leaves the user without an answer;
treating the empty normalized message as an ordinary assistant message violates
Gofer's message contract. Gofer therefore applies an always-on, per-run terminal
response guard.

## Recovery Sequence

The guard activates only when the current real user turn contains at least one
tool result. Responses containing visible text or tool calls are unchanged, as
are provider length/content-filter stops and invocations without a real user
message.

On the first empty post-tool `end_turn`:

1. If another model turn is available, the empty response is not added to
   conversation history.
2. Gofer appends a `model.retry` event containing the exact provider usage,
   model, caller, and turn number.
3. The next provider request receives a temporary user message asking it to
   review the existing results and return a concise visible answer. The message
   is tagged `internal_kind: terminal_response_recovery` and is never durable.

The retry is an ordinary model turn and consumes the same configured
`runtime.max_turns` budget as every other provider call. Its usage is later
recorded by the normal assistant-completion event, so the retry event and final
message together produce an exact two-call accounting record.

## Visible Failure Fallback

If the retry is also empty, or the first empty response occurs on the last
available turn, Gofer replaces it with a short visible fallback. That assistant
message is durable and tagged `internal_kind: terminal_response_fallback` with
an error reason. The run then fails explicitly with
`stop_reason: terminal_error`; lead-agent callers see the failed run, and child
agents return a failed tool outcome to their parent.

The one-retry budget is not refreshed when the retry asks for another tool.
This prevents an empty-response/tool-call cycle from bypassing the run's turn
limit. `GET /api/features` reports the guard as
`terminal_response.enabled`.
