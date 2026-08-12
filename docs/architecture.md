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
| `internal/event` | Typed immutable events and ordered journal contracts |
| `internal/thread` | Threads, messages, history, and checkpoints |
| `internal/model` | Normalized model request, response, stream, and usage types |
| `internal/tool` | Typed registry, validation, policy, and execution |
| `internal/sandbox` | Local and isolated execution contracts |
| `internal/skill` | Skill discovery, validation, activation, and projection |
| `internal/subagent` | Bounded parallel delegation and event fan-in |
| `internal/memory` | Scoped retrieval, consolidation, and lifecycle |
| `internal/store` | SQLite and PostgreSQL persistence adapters |
| `internal/gateway` | DeerFlow-compatible REST and SSE transports |
| `internal/channel` | IM and webhook adapters |

## Invariants

1. A run has one ordered event sequence and one terminal outcome.
2. Persisted events precede external publication, allowing lossless replay.
3. Cancellation propagates through models, tools, sandboxes, and sub-agents.
4. Tool arguments and model/provider data are untrusted until validated.
5. Host side effects require policy approval before execution begins.
6. Public protocol changes are additive or carry an explicit migration.
7. Goroutines have an owner and a defined shutdown path.
