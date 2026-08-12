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
  upsert, and delete tools. Authenticated runs use the bearer principal as the
  user boundary; local unauthenticated runs use the `local` scope.
- The runtime estimates prompt size before every model turn and uses the active
  model to summarize older, tool-safe message groups when the configured
  context budget is exceeded.
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
```

Run events are available as JSON or resumable server-sent events:

```text
GET /api/threads/{thread_id}/runs/{run_id}/events
GET /api/threads/{thread_id}/runs/{run_id}/stream
```

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
