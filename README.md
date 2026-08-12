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
registry, and event-journaled agent loop are in place. It is not yet a drop-in
DeerFlow replacement.

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
and [Roadmap](docs/roadmap.md) for the implementation sequence.

## Development

Gofer requires Go 1.26 or newer.

```sh
go run . version
make verify
make lint
make race
```

The CI gate enforces formatting, `go doc` rendering, exported API comments,
static analysis, cognitive and cyclomatic complexity limits, at least 85%
internal-package test coverage, race detection, vulnerability scanning, shell
and workflow linting, and CGO-free cross-platform builds.

## License

[MIT](LICENSE)
