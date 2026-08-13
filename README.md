# Gofer

Gofer is a Go-native, single-binary agent runtime for durable, long-running
work. It brings model streaming, typed tools, workspace isolation, event
replay, persistence, scheduling, and chat channels into one inspectable
service.

Gofer is inspired by [DeerFlow](https://github.com/bytedance/deer-flow), but it
is an independent implementation—not an official ByteDance project or a port
of DeerFlow's Python internals. Its goal is to provide the same class of
super-agent backend through Go-native contracts and operationally simple
deployment.

Gofer is most useful when you want:

- durable conversation and control state across reconnects and process restarts;
- an HTTP and SSE backend for conversations, tools, files, and human input;
- explicit policy and sandbox checks before model-requested side effects;
- SQLite for a local deployment or PostgreSQL for shared instances;
- schedules, subagents, skills, MCP servers, and chat channels on one runtime;
- a codebase whose state transitions and failure modes can be inspected.

It does not include DeerFlow's web interface, Python extension ABI, or hosted
sandbox services. Those remain external clients and adapters around the Go
runtime.

## Quick Start

Gofer requires Go 1.26 or newer and an OpenAI-compatible API key.

```sh
git clone https://github.com/Rememorio/gofer.git
cd gofer
cp config.example.yaml config.yaml
export OPENAI_API_KEY=<key>
go run . serve --config config.yaml
```

The example configuration listens on `127.0.0.1:8001`, stores durable state
under `.gofer/`, and keeps host command execution disabled.

Check the service and complete a first run:

```sh
curl -sS http://127.0.0.1:8001/healthz

curl -sS -X POST http://127.0.0.1:8001/api/runs/wait \
  -H 'content-type: application/json' \
  -d '{
    "assistant_id": "primary",
    "input": {
      "messages": [
        {"role": "user", "content": "Write a short Go concurrency checklist."}
      ]
    }
  }'
```

See [Getting Started](docs/getting-started.md) for Anthropic, Docker Compose,
durable threads, streaming, and production settings.

## Design Principles

Durable first. Threads, runs, messages, tool activity, usage, and terminal
outcomes are journaled before clients observe them. Live notifications are
wake-up hints; ordered storage remains the source of truth.

Explicit execution. Model output cannot mutate the host directly. Every side
effect becomes a typed tool call and passes validation, policy, workspace, and
sandbox boundaries before execution.

One runtime. The gateway, agent loop, persistence, scheduler, channels,
metrics, and CLI ship as one Go binary. Model providers, databases, sandboxes,
and transports stay behind narrow adapters.

Inspectable state. Events can be replayed, workspaces can be reviewed, and
failures remain visible. Gofer favors ordinary Go interfaces and structured
formats over hidden background behavior.

## How It Works

A run follows the same durable lifecycle whether it starts from HTTP, a
schedule, or a chat channel:

1. Validate the request and persist a pending run with its first event.
2. Reconstruct conversation state and prepare bounded model context.
3. Stream assistant text and typed tool requests from the selected provider.
4. Validate and execute approved tools inside the thread's workspace boundary.
5. Persist tool results, usage, checkpoints, and events before publishing them.
6. Continue until the model finishes, requests human input, reaches a limit,
   is cancelled, or fails.
7. Commit one terminal outcome that JSON and resumable SSE clients can replay.

The runtime owns this state machine. HTTP, SQL drivers, model SDKs, sandboxes,
and channel providers are adapters around it. See
[Architecture](docs/architecture.md) for the package boundaries and invariants.

## Capabilities

| Area | Included capabilities |
| --- | --- |
| Runtime | Streaming model/tool loop, cancellation, replay, context compaction, terminal recovery |
| Coordination | Goals, todos, memory, structured human input, schedules, bounded parallel subagents |
| Tools | Scoped files and artifacts, shell sandbox, browser, web research, MCP, skills |
| Models | OpenAI-compatible Chat Completions and native Anthropic Messages |
| Persistence | In-memory development store, SQLite, PostgreSQL, resumable SSE |
| Platform | Bearer RBAC, Prometheus metrics, uploads, feedback, token usage, workspace review |
| Channels | Slack, Telegram, Discord, Feishu/Lark, DingTalk, WeCom, WeChat, GitHub, Buzz/Nostr, webhook |

## Safety Model

Gofer treats model output, repository content, uploads, remote pages, MCP
results, skills, and channel messages as untrusted. Host execution is disabled
by default; the Docker sandbox starts without network access and with bounded
CPU, memory, processes, time, input, and output.

These controls reduce authority and contain mistakes, but deployment choices
still matter. Review [Security](docs/security.md) before enabling host execution,
private-network access, remote Chrome, document conversion, MCP processes, or a
public listener.

## Documentation

| Guide | Use it for |
| --- | --- |
| [Getting Started](docs/getting-started.md) | Install, launch, and complete a first run |
| [Configuration](docs/configuration.md) | Models, storage, tools, auth, and optional services |
| [HTTP API](docs/api.md) | Threads, runs, streaming, files, memory, schedules, and resources |
| [Channels](docs/channels.md) | Providers, binding codes, commands, and webhooks |
| [Deployment](docs/deployment.md) | Containers, releases, persistence, and upgrades |
| [Security](docs/security.md) | Trust boundaries, isolation, network policy, and operations |
| [Architecture](docs/architecture.md) | Runtime layers, durable lifecycle, and contributor invariants |
| [Compatibility](docs/compatibility.md) | DeerFlow-aligned behavior and explicit non-goals |

The [documentation index](docs/README.md) is the stable entry point for all
project guides.

## Project Status

Gofer is usable and continuously validated, but its public API may still evolve
before 1.0. Compatibility claims are limited to behavior covered by contract
tests; the current reference baseline and exclusions are documented in
[Compatibility](docs/compatibility.md).

## Contributing

Focused bug fixes, tests, documentation, and well-scoped capabilities are
welcome. Read [CONTRIBUTING.md](CONTRIBUTING.md) before changing runtime or
protocol behavior, and report vulnerabilities privately through
[SECURITY.md](SECURITY.md).

## License

Gofer is released under the [MIT License](LICENSE).
