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

Run events are available as JSON or resumable server-sent events:

```text
GET /api/threads/{thread_id}/runs/{run_id}/events
GET /api/threads/{thread_id}/runs/{run_id}/stream
```

SSE sequence numbers are emitted as event IDs; clients may reconnect with
`Last-Event-ID`. Health is public at `/healthz`. Prometheus text exposition is
available at `/metrics`. When bearer authentication is enabled, all API routes
other than health require the permissions configured for that token.
