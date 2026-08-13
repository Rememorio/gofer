# Getting started

This guide runs Gofer locally, completes a task, and points to the settings that
matter before exposing the service to other users.

## Requirements

- Go 1.26 or newer, or Docker with Compose
- an OpenAI-compatible API key or an Anthropic API key
- Git for cloning the repository

Gofer builds without CGO. SQLite is embedded, so a database service is not
required for a local installation.

## Run from source

```sh
git clone https://github.com/Rememorio/gofer.git
cd gofer
cp config.example.yaml config.yaml
export OPENAI_API_KEY=your-key
go run . serve --config config.yaml
```

The example configuration uses:

- `127.0.0.1:8001` for the HTTP listener;
- `.gofer/gofer.db` for SQLite;
- `.gofer/workspaces` for thread files and outputs;
- the `primary` OpenAI-compatible model alias;
- disabled host shell execution.

Configuration is strict YAML. Unknown fields, invalid combinations, and
missing referenced environment variables fail startup instead of being
silently ignored.

## Verify the service

```sh
curl -sS http://127.0.0.1:8001/healthz
go run . version --json
```

`/healthz` is the only API route that remains public when bearer authentication
is enabled. Prometheus metrics are also exposed without bearer authentication
at `/metrics`; restrict both operational endpoints at the network or reverse
proxy when needed.

## Complete a first run

The stateless wait endpoint creates a thread, starts a run, waits for its
terminal state, and returns one JSON response:

```sh
curl -sS -X POST http://127.0.0.1:8001/api/runs/wait \
  -H 'content-type: application/json' \
  -d '{
    "assistant_id": "primary",
    "input": {
      "messages": [
        {"role": "user", "content": "Create a short release checklist."}
      ]
    }
  }'
```

`assistant_id` selects a configured model alias. `lead_agent` selects the first
configured model.

For a conversation that continues across requests, create a thread and address
runs below that thread:

```sh
curl -sS -X POST http://127.0.0.1:8001/api/threads \
  -H 'content-type: application/json' \
  -d '{"title":"Release planning"}'

curl -sS -X POST http://127.0.0.1:8001/api/threads/THREAD_ID/runs/wait \
  -H 'content-type: application/json' \
  -d '{
    "assistant_id":"primary",
    "input":{"messages":[{"role":"user","content":"Draft the plan."}]}
  }'
```

Later runs may send only the new user message. Gofer reconstructs the durable
conversation before invoking the model.

## Stream a run

Use `/runs/stream` instead of `/runs/wait` for server-sent events:

```sh
curl -N -X POST http://127.0.0.1:8001/api/runs/stream \
  -H 'content-type: application/json' \
  -d '{
    "assistant_id":"primary",
    "input":{"messages":[{"role":"user","content":"Research the task."}]}
  }'
```

SSE event IDs are durable journal sequence numbers. Reconnect to a run stream
with `Last-Event-ID` to replay events after the last observed sequence.

## Run with Docker Compose

```sh
cd deploy
cp config.example.yaml config.yaml
export OPENAI_API_KEY=your-key
docker compose up --build
```

The example container runs as a non-root user with a read-only root filesystem
and a persistent named volume. See [Deployment](deployment.md) before mounting
a Docker socket or using PostgreSQL.

## Before exposing Gofer

1. Enable bearer authentication and issue least-privilege permissions.
2. Use PostgreSQL when multiple service instances share state.
3. Keep host shell execution disabled; configure the Docker sandbox for
   untrusted commands.
4. Review browser and web egress policy before allowing private addresses.
5. Back up the database and workspace root together.
6. Configure channel allowlists and binding flows explicitly.

Continue with [Configuration](configuration.md), [API](api.md), and the
[Security model](security.md).
