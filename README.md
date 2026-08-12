# Gofer

Gofer is a Go-native super-agent harness for long-running tasks, inspired by
[DeerFlow](https://github.com/bytedance/deer-flow). It is being built as a
single, inspectable binary with durable runs, tools, skills, sandboxed
execution, memory, and parallel sub-agents.

The name is intentional: a *gofer* is someone who gets things done, and this
one is written in Go.

## Status

Gofer is under active development. The engineering foundation, durable core,
normalized streaming model API, OpenAI-compatible adapter, validated tool
registry, event-journaled agent loop, traversal-resistant thread workspaces,
artifact catalog, built-in file tools, and fail-closed policy middleware are in
place. MCP servers can contribute validated tools over stdio or Streamable
HTTP, and Skills packages support strict metadata, persistent enablement,
progressive discovery, and atomic read-only projection. It is not yet a
drop-in DeerFlow replacement. Bounded shell execution is available through a
fail-closed local driver or hardened ephemeral Docker containers. Stateful,
thread-scoped browser automation is available through Chrome DevTools Protocol
with bounded sessions, stable snapshot references, screenshot artifacts, and
request-level SSRF policy enforcement.
Long-running task control now includes optimistic goal and todo state,
hierarchically scoped memory recall, safe conversation compaction, and bounded
parallel child agents with ordered lifecycle events. Goal and todo snapshots
are durable with SQLite/PostgreSQL and manageable through owner-scoped thread
APIs.
The durable service boundary now includes DeerFlow-shaped thread and run
responses, resumable run-event SSE, and transactional SQLite/PostgreSQL stores.
Owner-scoped thread search, patch/delete, run and message feeds, state lookup,
and journal-backed cross-run conversation continuity are also available.
Model and feature discovery, durable skill enablement, bounded multipart
uploads, persistent output discovery, and range-capable artifact delivery are
exposed through the same owner-scoped API.
LangGraph clients can discover configured assistants and use create-and-stream,
create-and-wait, stateless run, join, and run-scoped message compatibility
endpoints over the same durable journal.
The `gofer serve` command now assembles the model provider, durable store,
isolated workspaces, sandbox, browser, policy, runtime, gateway, graceful
shutdown, and Prometheus endpoint into a runnable service. MCP tools, projected
skills, scoped memory recall and editing, and model-backed context compaction
are wired into every run when enabled. Long-term memory uses the configured
SQLite/PostgreSQL store, supports atomic import/export and owner-scoped fact
management, and survives service restarts. Service control primitives include
digest-only bearer credentials and RBAC, persistent leased cron/one-shot
scheduling with an authenticated management API, channel normalization,
dependency-ordered extensions, and bounded-cardinality Prometheus metrics.

## Design

- **Go-native:** ordinary Go interfaces, contexts, goroutines, and explicit
  state machines rather than a translation of LangGraph internals.
- **Durable:** every run produces an append-only event history that can be
  streamed, inspected, resumed, and replayed.
- **Safe by construction:** model-requested actions pass through typed tools,
  authorization, workspace boundaries, and sandbox policy before execution.
- **Protocol-oriented:** DeerFlow-compatible HTTP and SSE behavior is verified
  with contract tests while the runtime remains independently designed.
- **Single binary:** the gateway, agent runtime, local persistence, CLI, and
  extension surfaces ship together by default.

See [Architecture](docs/architecture.md) for the planned component boundaries
and [Roadmap](docs/roadmap.md) for the implementation sequence. Command
isolation and its trust boundaries are documented in
[Sandbox](docs/sandbox.md). Browser lifecycle and network boundaries are
documented in [Browser automation](docs/browser.md).
The long-running task primitives are documented in
[Task control](docs/task-control.md).
HTTP contracts and persistence behavior are documented in
[Gateway and persistence](docs/gateway.md).
Operational boundaries are documented in [Service control plane](docs/control-plane.md).

## Development

Gofer requires Go 1.26 or newer.

```sh
go run . version
cp config.example.yaml config.yaml
OPENAI_API_KEY=... go run . serve --config config.yaml
make verify
make lint
make race
```

The CI gate enforces formatting, `go doc` rendering, exported API comments,
static analysis, cognitive and cyclomatic complexity limits, at least 85%
internal-package test coverage, race detection, vulnerability scanning, shell
and workflow linting, and CGO-free cross-platform builds.

See [Running Gofer](docs/service.md) for the launch request and operational
endpoints.

## License

[MIT](LICENSE)
