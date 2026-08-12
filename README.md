# Gofer

Gofer is a Go-native super-agent harness for long-running tasks, inspired by
[DeerFlow](https://github.com/bytedance/deer-flow). It is being built as a
single, inspectable binary with durable runs, tools, skills, sandboxed
execution, memory, and parallel sub-agents.

The name is intentional: a *gofer* is someone who gets things done, and this
one is written in Go.

## Status

Gofer is under active development. The engineering foundation, durable core,
normalized streaming model API, OpenAI-compatible Chat Completions and native
Anthropic Messages adapters, validated tool registry, event-journaled agent
loop, traversal-resistant thread workspaces,
artifact catalog, built-in file tools, and fail-closed policy middleware are in
place. MCP servers can contribute validated tools over stdio or Streamable
HTTP, and Skills packages support strict metadata, persistent enablement,
progressive discovery, and atomic read-only projection. It is not yet a
drop-in DeerFlow replacement. Bounded shell execution is available through a
fail-closed local driver or hardened ephemeral Docker containers. Stateful,
thread-scoped browser automation is available through Chrome DevTools Protocol
with bounded sessions, stable snapshot references, screenshot artifacts, and
request-level SSRF policy enforcement.
Opt-in web research adds normalized Brave or SearXNG search plus bounded HTML,
text, and JSON retrieval. Its guarded transport validates every DNS answer and
redirect, dials approved addresses directly, and marks all remote content as
untrusted before model use.
Conversation assistance now includes atomic first-turn titles, pre-send draft
polishing with slash-command preservation, and fail-soft follow-up suggestions.
All three use bounded tool-free model calls, and automatic titles never
overwrite a user rename.
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
exposed through the same owner-scoped API. Upload ingestion now enforces both
per-file and aggregate request limits, supports opt-in bounded Office/PDF to
Markdown conversion, injects sanitized current-file outlines without changing
durable user messages, and gives agents bounded historical upload discovery.
Structured clarification now uses DeerFlow-compatible request, choice, and
form artifacts. Clarification calls are side-effect exclusive, settle the run
as `interrupted`, survive durable-store restarts, and accept validated
structured or legacy next-turn answers without replaying a response.
LangGraph clients can discover configured assistants and use create-and-stream,
create-and-wait, stateless run, join, and run-scoped message compatibility
endpoints over the same durable journal.
Owner-scoped conversations can also branch from any completed assistant turn.
Gofer seeds an independent durable history and clones the latest workspace
atomically; historical branches omit newer files to preserve temporal
consistency.
Run feedback supports durable thumbs-up/down ratings, optional message and
comment context, idempotent updates, scoped deletion, and aggregate statistics
through the same authenticated thread boundary.
Provider-reported token usage is journaled per model call and aggregated by
thread, model, and caller. Main-agent, delegated subagent, and compaction calls
share one exact, restart-safe accounting path with current-context estimates.
Each run also records a bounded review of files changed below its workspace and
outputs roots. Text receives a unified diff, while secret-looking, binary,
large, and symlink paths remain metadata-only; uploads and transient process
feedback are excluded.
Terminal `run.delivery` receipts attribute presented artifacts by tool and
verify that every successful run which changed outputs explicitly delivered at
least one matching file. Missing or unverifiable delivery becomes a durable
run failure instead of silently stranding an output on disk.
Oversized tool results are kept out of both durable conversation history and
later model turns. Gofer atomically stores the complete result under the
thread's private `.tool-results` output directory and substitutes a typed,
bounded synopsis with a `read_file` reference; a strict head-and-tail fallback
applies when persistence is unavailable. Delivery and workspace review inspect
the original result where necessary but never count these spill files as user
artifacts.
Model-facing user input and attacker-influenceable remote tool results now pass
through structural prompt-injection guardrails. Framework authority tags and
forged user boundaries are neutralized without changing durable user text;
browser snapshots and every MCP tool are classified as untrusted by tool
metadata. Result sanitization runs before output budgeting, while local file
and shell output remains byte-preserved.
Existing files are protected by a version-aware read-before-write gate. A
successful full or ranged `read_file` records the complete content revision;
exactly one matching `write_file` or `str_replace` may follow before another
read is required. Same-path writes are serialized across parent and child
agents, so stale reads and concurrent duplicate appends fail with a
model-correctable result instead of racing.
Repetitive tool loops are bounded independently of the overall turn budget.
Gofer detects both equivalent call sets and excessive use of one tool, injects
a temporary model-facing warning before the limit, and strips the limiting
call set before execution with an explicit `loop_capped` terminal reason.
Interrupted tool turns are repaired at the provider boundary. Missing results
receive temporary recoverable errors, late results are restored beside their
calls, and orphan results are omitted from that request, while the durable
conversation remains an exact record of what actually happened.
Tool-using runs also have a visible terminal-response guarantee. One empty
post-tool answer receives a bounded automatic retry; another empty answer (or
an exhausted turn budget) records a user-visible fallback and fails the run
explicitly instead of silently completing or losing the provider usage.
When a provider reaches its output-token limit after producing visible
terminal text, Gofer preserves that partial answer and succeeds with the
explicit `model_length_capped` reason. Empty or tool-bearing capped responses
remain failures, so truncated tool arguments can never execute.
Provider safety stops are repaired into visible `safety_capped` outcomes.
Existing refusal text is preserved, empty responses receive a neutral
explanation, and every accompanying tool call is removed before loop
accounting, persistence, or execution.
The `gofer serve` command now assembles the model provider, durable store,
isolated workspaces, sandbox, browser, policy, runtime, gateway, graceful
shutdown, and Prometheus endpoint into a runnable service. MCP tools, projected
skills, scoped memory recall and editing, and model-backed context compaction
are wired into every run when enabled. Long-term memory uses the configured
SQLite/PostgreSQL store, supports atomic import/export and owner-scoped fact
management, and survives service restarts. Service control primitives include
digest-only bearer credentials and RBAC, persistent leased cron/one-shot
scheduling with an authenticated management API, durable channel bindings and
thread mappings, signed webhook ingress/callbacks, dependency-ordered
extensions, and bounded-cardinality Prometheus metrics.

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
Messaging identity, webhook, and callback contracts are documented in
[Channels](docs/channels.md), including signed webhooks and native Slack,
Telegram, Discord, Feishu/Lark, DingTalk, WeCom, and WeChat connections.
Operational boundaries are documented in [Service control plane](docs/control-plane.md).
Tool result externalization and fallback behavior are documented in
[Tool output budgets](docs/tool-output.md).
Model-input trust boundaries are documented in
[Prompt-injection guardrails](docs/guardrails.md).
File version gating is documented in
[Read before write](docs/read-before-write.md).
Repetitive-call safety is documented in
[Tool loop detection](docs/loop-detection.md).
Provider-safe interrupted transcripts are documented in
[Tool history repair](docs/tool-history.md).
Post-tool final-answer recovery is documented in
[Terminal responses](docs/terminal-response.md).
Provider output-length handling is documented in
[Model length caps](docs/model-length.md).
Provider content-filter handling is documented in
[Safety finish reasons](docs/safety-finish.md).
Provider configuration and normalized protocol behavior are documented in
[Model providers](docs/model-providers.md).
Search, direct document retrieval, and outbound network policy are documented
in [Web research](docs/web-research.md).
Automatic titles, input polishing, and follow-up suggestions are documented in
[Conversation services](docs/conversation-services.md).
Upload isolation, conversion, and model-context behavior are documented in
[Uploads and document ingestion](docs/uploads.md).
Durable clarification requests and replies are documented in
[Structured human input](docs/human-input.md).

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
