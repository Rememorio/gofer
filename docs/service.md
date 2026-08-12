# Running Gofer

Gofer ships as one process. `gofer serve` loads a strict, versioned YAML file,
opens the configured durable store, prepares thread workspaces, constructs all
enabled adapters, and listens until SIGINT or SIGTERM. Shutdown stops accepting
new HTTP work, cancels active runs, closes browser sessions, and releases the
database.

```sh
cp config.example.yaml config.yaml
export OPENAI_API_KEY=your-key
go run . serve --config config.yaml
```

SQLite is the default and its parent directory is created automatically. Use
`storage.driver: memory` for disposable development or `postgres` with a pgx
connection string for a shared service. The local sandbox remains disabled
until `allow_host_execution: true` is set explicitly; Docker is the preferred
boundary for untrusted commands.

Optional agent extensions are initialized before the listener becomes ready:

- `skills.enabled` scans strict `SKILL.md` packages and creates a read-only
  projection for sandboxed commands. Agents discover instructions through
  `describe_skill` and load them through `read_skill` only when relevant.
- `mcp.enabled` connects every configured stdio or Streamable HTTP server and
  atomically registers its namespaced tools. Startup fails if discovery is
  unsafe or incomplete.
- `memory.enabled` provides user-and-thread-scoped recall plus explicit search,
  upsert, and delete tools backed by the configured durable SQL store.
  Authenticated runs use the bearer principal as the user boundary; local
  unauthenticated runs use the `local` scope.
- The runtime estimates prompt size before every model turn and uses the active
  model to summarize older, tool-safe message groups when the configured
  context budget is exceeded.
- `tool_output` externalizes oversized tool results before they reach the
  journal or another model turn. The complete result remains readable under
  `/mnt/user-data/outputs/.tool-results` while the model receives a typed
  synopsis and bounded raw sample. `read_file` is exempt to prevent spill/read
  loops, and failed persistence falls back to strict inline truncation.
- Prompt-injection guardrails are always active. The current genuine user
  message is temporarily wrapped in explicit boundaries and reserved authority
  tags are escaped before each model call, without changing conversation
  history. Known remote web tools, browser snapshot tools, and all MCP tools
  recursively neutralize the same tokens in JSON keys and string values before
  result budgeting and persistence. Local shell and file results are not
  rewritten.
- `read_before_write.enabled` defaults to `true`. Existing workspace and output
  files must be read at their current full-content revision before each
  `write_file` or `str_replace`; ranged reads qualify because their result also
  carries the complete file revision. New files pass through, stale marks
  return a recoverable tool error, and same-path modifications are serialized
  across lead and child agents. While this gate is enabled, configuration
  validation requires `read_file` to remain exempt from tool-output budgeting.
- `loop_detection.enabled` defaults to `true`. Equivalent tool-call sets are
  tracked in a bounded sliding window, while a second counter catches excessive
  use of one tool even when its arguments change. A warning is supplied only to
  the next model request. Reaching a hard limit removes that response's tool
  calls before journaling or execution and completes the run with
  `stop_reason: loop_capped`. Per-tool frequency thresholds can override the
  global values.
- Tool history repair is always active at the final model-request boundary. If
  a cancelled or failed run persisted an assistant tool call without its
  result, the next request receives a temporary recoverable error result in the
  required position. Late results are regrouped by call ID, and orphan or
  excess results are withheld from the provider. These corrections never alter
  journal events or conversation APIs.
- Terminal-response recovery is always active for user turns that have executed
  a tool. The first empty end-turn response is discarded and retried once with
  a temporary reminder when the run still has model-turn budget. A repeated
  empty response, or an exhausted budget, persists a visible fallback and
  fails the run with `stop_reason: terminal_error`. Tool-call responses,
  visible text, provider truncation, and turns without tool results are not
  rewritten.
- Model length-cap reporting is always active. An output-token stop with
  non-whitespace text and no tool calls preserves the exact partial answer and
  completes with `stop_reason: model_length_capped`. Empty capped responses and
  responses that still carry tool intent fail with the provider truncation
  error; their tool calls are never journaled or executed.
- `subagent_spawn` starts bounded parallel child agents. Each child gets an
  isolated run journal and tool registry while sharing only the parent's
  policy-controlled workspace, configured extensions, and tenant scope. The
  parent can inspect, wait for, list, or cancel child work explicitly.
- `scheduler.enabled` polls durable one-shot and cron definitions using
  expiring, compare-and-claim leases. SQLite and PostgreSQL retain schedules,
  dispatch history, and their dedicated thread binding across restarts.

Create a thread and launch a run:

```sh
curl -sS -X POST http://127.0.0.1:8001/api/threads \
  -H 'content-type: application/json' \
  -d '{"title":"example"}'

curl -sS -X POST http://127.0.0.1:8001/api/threads/THREAD_ID/runs \
  -H 'content-type: application/json' \
  -d '{
    "assistant_id":"primary",
    "input":{"messages":[{"role":"user","content":"Write a short report."}]},
    "context":{"max_tokens":4096,"temperature":0.2}
}'
```

LangGraph-compatible clients can discover the default `lead_agent` alias and
every configured model alias:

```text
POST /api/assistants/search
GET  /api/assistants/{assistant_id}
GET  /api/assistants/{assistant_id}/graph
GET  /api/assistants/{assistant_id}/schemas
```

`lead_agent` selects the first configured model. Graph and schema responses
describe Gofer's agent/tool loop and message state without exposing provider
credentials.

`assistant_id` selects a configured model alias. Inputs accept OpenAI-style
roles as well as LangChain's `human` and `ai` message types. Text and image
content blocks, assistant tool calls, and tool results are normalized before
execution.

For later turns, clients may submit only the new user message. Gofer rebuilds
prior user, assistant, and tool messages from the durable journal and supplies
that history to the model automatically. Conversation feeds and state are
available from:

```text
GET  /api/threads?limit=50&offset=0&q=research
POST /api/threads/search
GET  /api/threads/{thread_id}/state
GET  /api/threads/{thread_id}/messages
GET  /api/threads/{thread_id}/runs
GET  /api/threads/{thread_id}/runs/{run_id}/messages
PATCH /api/threads/{thread_id}
DELETE /api/threads/{thread_id}
POST /api/threads/{thread_id}/branches
```

The branch request accepts `message_id`, the compatible `message_ids` list,
and an optional `title`. The selected message must be a completed assistant
turn and the source thread must have no active run. The new thread receives an
independent terminal history seed through that turn. A latest-turn branch
atomically clones workspace, upload, and output files; a historical branch
reports `skipped_historical_turn` and starts with an empty workspace so newer
files cannot appear in older conversation state. The response exposes both
the source identifiers and the workspace/history modes.

Run feedback is durable and owner-scoped:

```text
POST   /api/threads/{thread_id}/runs/{run_id}/feedback
PUT    /api/threads/{thread_id}/runs/{run_id}/feedback
GET    /api/threads/{thread_id}/runs/{run_id}/feedback
GET    /api/threads/{thread_id}/runs/{run_id}/feedback/stats
DELETE /api/threads/{thread_id}/runs/{run_id}/feedback
DELETE /api/threads/{thread_id}/runs/{run_id}/feedback/{feedback_id}
GET    /api/runs/{run_id}/feedback
```

Ratings are `1` or `-1`. `POST` accepts optional `message_id` and `comment`
fields, while `PUT` atomically creates or updates the caller's canonical
run-level rating. A user cannot read or delete another user's record, and run
IDs are always verified against the owning thread. Feedback uses
`threads:read`, `threads:write`, and `threads:delete` permissions.

Run events are available as JSON or resumable server-sent events:

```text
GET /api/threads/{thread_id}/runs/{run_id}/events
GET /api/threads/{thread_id}/runs/{run_id}/stream
GET /api/threads/{thread_id}/runs/{run_id}/join
POST /api/threads/{thread_id}/runs/{run_id}/stream
```

Create-and-consume variants avoid a second request. The stateless forms create
an owner-scoped thread unless `config.configurable.thread_id` selects an
existing owned thread:

```text
POST /api/threads/{thread_id}/runs/stream
POST /api/threads/{thread_id}/runs/wait
POST /api/runs/stream
POST /api/runs/wait
GET  /api/runs/{run_id}/messages
```

Every streaming creation response sets a canonical `Content-Location` header.
Terminal streams briefly drain the durable journal so a status transition
cannot race and hide the final `run.completed`, `run.failed`, or
`run.cancelled` event. Posting an existing stream with `action=interrupt` or
`action=rollback` requests cancellation; `wait=1` waits for terminal state.

Before that terminal event, every run emits one `run.delivery` receipt. Its
base fields report the artifact paths returned by presentation-capable tools
and group them by tool. If the run created or modified output files, the receipt
also verifies that `present_files` covered at least one produced path. An
otherwise successful run fails with an artifact-delivery error when no match
exists, preventing generated files from being silently left undisclosed.

Provider usage is available on individual run responses and as a thread
aggregate:

```text
GET /api/threads/{thread_id}/token-usage
GET /api/threads/{thread_id}/token-usage?include_active=true
```

The response includes input/output/total tokens, completed run count, per-model
tokens and run participation, and `lead_agent`, `subagent`, and `middleware`
caller buckets. By default aggregation includes successful and failed runs;
`include_active=true` also exposes durable progress from running runs.
Automatically retried empty responses are represented by `model.retry` events,
so their provider usage and LLM call count remain included exactly once.
`context_usage` estimates the currently materialized conversation against the
configured context limit and reports a one-decimal percentage. Synthetic
branch-history runs are excluded from billing totals.

Run-scoped workspace review is available separately from the live event feed:

```text
GET /api/threads/{thread_id}/runs/{run_id}/workspace-changes
GET /api/threads/{thread_id}/runs/{run_id}/workspace-changes?include_files=false
GET /api/threads/{thread_id}/runs/{run_id}/workspace-changes?include_diff=false
```

The response contains an `available` flag, version, created/modified/deleted
and line counts, bounded file records, and the limits applied during capture.
Files below `/mnt/user-data/workspace` and `/mnt/user-data/outputs` participate;
uploads do not. Sensitive-looking, binary, large, and symlink paths never
include content. Authentication uses the same owner-scoped `runs:read`
permission as run state and event history.

Clients can discover configured capabilities and manage runtime skills without
receiving provider credentials:

```text
GET  /api/models
GET  /api/models/{model_name}
GET  /api/features
GET  /api/skills
GET  /api/skills/{skill_name}
POST /api/skills/{skill_name}/enable
POST /api/skills/{skill_name}/disable
POST /api/skills/reload
```

Skill changes are serialized, persisted by SQL stores, and projected
atomically. A failed projection rolls the requested state change back.

Thread files use bounded multipart uploads and stable virtual paths:

```text
POST   /api/threads/{thread_id}/uploads
GET    /api/threads/{thread_id}/uploads/limits
GET    /api/threads/{thread_id}/uploads/list
DELETE /api/threads/{thread_id}/uploads/{filename}
GET    /api/threads/{thread_id}/artifacts
GET    /api/threads/{thread_id}/artifacts/{virtual_path}
```

Duplicate upload names are made collision-free. Artifact responses support
HTTP ranges and force active HTML or SVG content to download with MIME
sniffing disabled. With authentication enabled, these routes require
`resources:read` or `resources:write` and remain scoped to the thread owner.

Long-running goal state is shared by the HTTP API and the agent's control
tools:

```text
GET    /api/threads/{thread_id}/goal
PUT    /api/threads/{thread_id}/goal
DELETE /api/threads/{thread_id}/goal
GET    /api/threads/{thread_id}/control
PUT    /api/threads/{thread_id}/todos
```

Goal responses follow DeerFlow's `{ "goal": ... }` shape. The `control`
resource includes the optimistic version and ordered todos. HTTP mutations are
rejected while a run is non-terminal, and SQL stores recover the same state
after restart. These routes use the existing `threads:read` and
`threads:write` permissions.

User-global memory facts use DeerFlow-shaped camel-case responses and atomic
scope replacement for imports:

```text
GET    /api/memory
POST   /api/memory/reload
DELETE /api/memory
POST   /api/memory/facts
PATCH  /api/memory/facts/{fact_id}
DELETE /api/memory/facts/{fact_id}
GET    /api/memory/export
POST   /api/memory/import
GET    /api/memory/config
GET    /api/memory/status
```

Search parameters `q`, comma-separated `tags`, and `limit` are additive.
SQLite and PostgreSQL retain facts, topics, confidence, expiry, source, and
scope across restarts. Import validates the complete batch before replacing
the authenticated user's global scope. Reads require `memory:read`; mutations
require `memory:write`.

SSE sequence numbers are emitted as event IDs; clients may reconnect with
`Last-Event-ID`. Health is public at `/healthz`. Prometheus text exposition is
available at `/metrics`. When bearer authentication is enabled, all API routes
other than health require the permissions configured for that token.

Create and manage scheduled work through the same service:

```sh
curl -sS -X POST http://127.0.0.1:8001/api/scheduled-tasks \
  -H 'content-type: application/json' \
  -d '{
    "title":"Morning research",
    "prompt":"Summarize the latest project activity.",
    "schedule_type":"cron",
    "schedule":"0 9 * * 1-5",
    "timezone":"Asia/Shanghai"
  }'
```

The collection supports `GET` and `POST`. Individual resources support `GET`,
`PATCH`, and `DELETE`, plus `POST` actions at `/pause`, `/resume`, and
`/trigger`. A task without `thread_id` gets a dedicated thread on first
dispatch and reuses it thereafter. With authentication enabled, reads require
`scheduled:read` and mutations require `scheduled:write`.
