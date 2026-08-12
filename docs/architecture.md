# Architecture

Gofer treats an agent run as a durable state machine driven by immutable
events. Model providers, tools, sandboxes, storage engines, and transports are
adapters around that core.

```text
HTTP / CLI / Channels
         |
         v
  Gateway and Run Service
         |
         v
   Durable Agent Runtime ----> Event Journal / Checkpoints
      |       |       |
      v       v       v
   Models   Tools   Sub-agents
              |
              v
      Policy and Sandbox
```

## Dependency Direction

The runtime domain must not import HTTP frameworks, database drivers, provider
SDKs, or sandbox implementations. Adapters implement small interfaces owned by
their consumers. This keeps replay tests deterministic and prevents one SDK
from defining Gofer's public contracts.

Planned package groups:

| Area | Responsibility |
| --- | --- |
| `internal/runtime` | Run state machine, model/tool loop, cancellation, limits |
| `internal/contextwindow` | Boundary-safe conversation compaction and summaries |
| `internal/conversation` | Cross-run history reconstruction, branching, overlap merging, and input journaling |
| `internal/control` | Optimistic per-thread goals and ordered todo plans |
| `internal/event` | Typed immutable events and ordered journal contracts |
| `internal/feedback` | Owner-scoped run ratings, comments, and aggregate statistics |
| `internal/delivery` | Run-scoped presentation tracking and terminal output verification |
| `internal/thread` | Threads, messages, history, and checkpoints |
| `internal/model` | Normalized model request, response, stream, and usage types |
| `internal/usage` | Journal-derived run/thread token accounting and caller attribution |
| `internal/tool` | Typed registry, validation, policy, and execution |
| `internal/guardrail` | Temporary user-input framing and recursive untrusted-result neutralization |
| `internal/loopdetect` | Bounded repeated-call and per-tool frequency detection |
| `internal/toolhistory` | Transient provider-safe tool call/result transcript repair |
| `internal/terminalresponse` | Bounded empty post-tool response recovery and visible fallback |
| `internal/modellength` | Safe visible provider length-cap preservation |
| `internal/tooloutput` | Typed result synopsis, atomic spill storage, and context-budget fallback |
| `internal/mcp` | MCP transports, bounded discovery, and namespaced tool adapters |
| `internal/policy` | Ordered authorization rules and tool resource extraction |
| `internal/readbeforewrite` | Version-aware existing-file mutation gate and same-path execution serialization |
| `internal/workspace` | Per-thread files, uploads, outputs, search, and traversal safety |
| `internal/workspacechange` | Bounded run snapshots, privacy-aware diffs, and journal projection |
| `internal/artifact` | Explicit user-facing output registration and streaming |
| `internal/sandbox` | Local and isolated execution contracts |
| `internal/browser` | Thread-scoped CDP sessions, snapshots, network guards, and browser tools |
| `internal/skill` | Skill discovery, validation, activation, and projection |
| `internal/subagent` | Bounded parallel delegation and event fan-in |
| `internal/memory` | Scoped retrieval, consolidation, and lifecycle |
| `internal/store` | Core durable store contract and in-memory reference adapter |
| `internal/store/sqlstore` | Transactional SQLite and PostgreSQL persistence |
| `internal/gateway` | DeerFlow-compatible REST and SSE transports |
| `internal/channel` | IM and webhook adapters |
| `internal/auth` | Bearer authentication, principal context, and RBAC policy |
| `internal/scheduler` | Cron/one-shot validation, durable leases, and dispatch |
| `internal/extension` | Dependency-ordered component lifecycle and rollback |
| `internal/observe` | Bounded-cardinality metrics and Prometheus exposition |

## Invariants

1. A run has one ordered event sequence and one terminal outcome.
2. Persisted events precede external publication, allowing lossless replay.
3. Cancellation propagates through models, tools, sandboxes, and sub-agents.
4. Tool arguments and model/provider data are untrusted until validated.
5. Host side effects require policy approval before execution begins.
6. Public protocol changes are additive or carry an explicit migration.
7. Goroutines have an owner and a defined shutdown path.
8. Extension discovery is bounded and publishes an atomic validated snapshot.
9. Skill packages never follow symlinks and project into an agent-visible read-only tree.
10. Host command execution is disabled by default; container execution starts from a read-only, capability-free, network-isolated baseline.
11. Browser navigation and every intercepted HTTP request pass through the same fail-closed address policy.
12. Child agents have explicit depth, total-count, and parallelism limits and an owned cancellation path.
13. Long-term memory retrieval never crosses its authenticated user scope.
14. Non-public HTTP routes fail closed when authentication or permission is absent.
15. Scheduled work is dispatched only under an expiring owner lease.
16. Durable thread state and files never cross an authenticated owner boundary.
17. Conversation branches never mutate their source or expose files newer than the selected turn.
18. Token totals are derived from immutable provider usage events and never double-count cache or reasoning detail.
19. Workspace reviews drain child agents and commit before the terminal event; sensitive or unbounded content never enters the journal.
20. Every service-finalized run has one delivery receipt; a successful run that changed outputs must have presented a matching path.
21. Raw tool-result observers run before context transforms; only the bounded transformed result enters the journal and later model requests.
22. User text stays durable in its original form; every model call receives temporary authority-tag neutralization and explicit user boundaries.
23. Remote-content results are sanitized before budgeting or persistence, and tool trust classification is explicit metadata rather than a name heuristic.
24. Every existing-file mutation consumes a matching current read revision; same-scope, same-path checks and execution are serialized.
25. Loop warnings are temporary model context; hard-capped tool calls never enter the journal or execute.
26. Every provider-bound tool call has exactly one adjacent result; recovery never rewrites durable history.
27. A tool-using user turn ends with visible assistant text or an explicit durable run failure.
28. Length-capped tool intent never executes; only visible tool-free partial text may complete as capped.
