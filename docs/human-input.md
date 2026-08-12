# Structured human input

Gofer implements DeerFlow's `ask_clarification` interaction as a durable pause,
not as an ordinary tool result followed by another model call. The tool is
available only to the lead agent. Delegated sub-agents must complete from the
context they receive and cannot dead-end a parent task by asking the user a
question.

## Lifecycle

When the model calls `ask_clarification`, the final clarification middleware:

1. keeps the first clarification call and discards sibling calls, so no write,
   command, network, or other side effect can race ahead of the user's answer;
2. normalizes the request and persists its fallback text plus structured
   artifact as the tool result;
3. runs normal finish hooks, including child-agent draining, workspace review,
   and the delivery receipt;
4. transitions the run from `running` to `interrupted` and appends
   `run.interrupted` with stop reason `human_input`.

An interrupted run is quiescent and resumable, but not terminal. SSE, join,
and create-and-wait requests nevertheless settle on `run.interrupted`, so a
client never waits forever. A later user answer starts a normal new run on the
same thread. Journal reconstruction supplies the original tool call and
result immediately before the answer, preserving valid provider history.

Pending requests are event-sourced. They therefore survive SQLite or
PostgreSQL restarts without a mutable shadow table. The service serializes the
history-check-and-input-append boundary, so a response replay is rejected
before another model request in a running process.

## Request protocol

The tool accepts one question, a `clarification_type`, optional context,
optional choices, or a structured form. Form fields take precedence over
choices.

```json
{
  "question": "Which environment should receive the release?",
  "clarification_type": "approach_choice",
  "context": "The deployment settings differ.",
  "options": ["Staging", "Production"]
}
```

The tool result contains a readable `content` fallback and the same request at
both `artifact.human_input` and `human_input`. The former is DeerFlow's
artifact shape; the latter is convenient for Gofer's normalized tool-result
API.

```json
{
  "version": 1,
  "kind": "human_input_request",
  "source": "ask_clarification",
  "request_id": "clarification:call-id",
  "tool_call_id": "call-id",
  "clarification_type": "approach_choice",
  "question": "Which environment should receive the release?",
  "input_mode": "choice_with_other",
  "options": [
    {"id": "option-1", "label": "Staging", "value": "Staging"},
    {"id": "option-2", "label": "Production", "value": "Production"}
  ]
}
```

Free-text and choice requests use protocol version 1. Forms use version 2 and
support `text`, `textarea`, `number`, `select`, `multi_select`, `checkbox`, and
`date`. A form is accepted only as a whole: it is bounded to 16 fields, 24
options per field, 200 Unicode characters per field label/value, and 16 KiB of
normalized field JSON. Duplicate names, JavaScript prototype names, malformed
entries, or an exceeded bound degrade the whole form to choices or free text.
Unknown field types and option-less selects safely degrade only that field to
text.

## Response protocol

A structured answer is a user message whose `additional_kwargs` contains
`human_input_response`. Form cards submit a readable text summary using the
same v1 text response.

```json
{
  "role": "user",
  "content": "Staging",
  "additional_kwargs": {
    "hide_from_ui": true,
    "human_input_response": {
      "version": 1,
      "kind": "human_input_response",
      "source": "ask_clarification",
      "request_id": "clarification:call-id",
      "response_kind": "option",
      "option_id": "option-1",
      "value": "Staging"
    }
  }
}
```

Gofer rejects an unknown request ID, a source mismatch, an option ID/value
that does not belong to the request, malformed metadata, or a response to an
already answered request. For compatibility, the next visible plain user
message closes the latest unanswered request even without structured metadata.
Hidden messages without a valid response do not close it.

Clients can inspect pending and answered state at:

```text
GET /api/threads/{thread_id}/human-input
GET /api/threads/{thread_id}/state
```

The generic state response sets `next` to `human_input` and adds the open
requests under `interrupts.human_input`. `GET /api/features` reports the
always-on capability as `human_input.enabled`.

## Non-interactive runs

Webhook or batch callers that cannot receive a synchronous question can set
`context.disable_clarification` to `true`. The tool then returns a normal
model-facing instruction to proceed with best judgment and state assumptions;
the run does not interrupt. The bypass is explicit per run and does not change
the durable protocol for interactive conversations.
