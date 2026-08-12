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
| `internal/thread` | Threads, messages, history, and checkpoints |
| `internal/model` | Normalized model request, response, stream, and usage types |
| `internal/usage` | Journal-derived run/thread token accounting and caller attribution |
| `internal/tool` | Typed registry, validation, policy, and execution |
| `internal/mcp` | MCP transports, bounded discovery, and namespaced tool adapters |
| `internal/policy` | Ordered authorization rules and tool resource extraction |
| `internal/workspace` | Per-thread files, uploads, outputs, search, and traversal safety |
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
