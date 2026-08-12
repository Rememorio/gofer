# Gateway and persistence

Gofer exposes its core thread, run, and journal resources below `/api`. The
gateway uses the Go standard library HTTP stack and depends only on the durable
store interface, so transport code does not own model or database behavior.

## Current contract

- `POST /api/threads` creates a thread and accepts DeerFlow's optional
  `thread_id`, `assistant_id`, and `metadata` fields. A caller-provided Gofer
  thread ID makes creation idempotent.
- `GET /api/threads/{thread_id}` returns the DeerFlow-shaped status, values,
  interrupts, metadata, and timestamps, with Gofer's optional title as an
  additive field.
- `POST /api/threads/{thread_id}/runs` persists a pending run and its first
  `run.created` event, then hands a validated DeerFlow launch envelope to the
  configured starter.
- Run lookup, cancellation, event history, and SSE streaming use both thread
  and run IDs to enforce resource scoping.

The launch envelope supports input, command, assistant, metadata, config,
context, checkpoint, interrupt, stream, disconnect, and multitask fields.
Unsupported compatibility placeholders are accepted only as `null`, matching
DeerFlow's fail-fast behavior.

## SSE replay

The stream endpoint writes journal sequence numbers as SSE IDs. A reconnecting
client may provide `Last-Event-ID`; Gofer first replays every durable event
after that sequence, then watches for new commits. Notifications are only wake
hints—the consumer always rereads ordered storage—so coalescing cannot lose
events. Terminal runs close their stream after all available events are sent.

## SQL storage

`internal/store/sqlstore` owns the same `store.Store` contract for SQLite and
PostgreSQL. SQLite uses a pure-Go driver and one connection, enabling CGO-free
cross-platform builds. PostgreSQL uses pgx through `database/sql`.

Schema creation is idempotent. Run transitions and event appends use serializable
transactions and optimistic status/sequence checks. Threads, runs, event data,
timestamps, and metadata survive process restart. Journal watches poll the
database rather than relying only on process-local signals, so events written
by another service instance become visible to an SSE consumer.
