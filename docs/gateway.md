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
- `GET /api/threads` and `POST /api/threads/search` return stable, paginated,
  owner-scoped conversation feeds. `PATCH` merges title or metadata and
  `DELETE` removes terminal run history plus thread-scoped external resources.
- `GET /api/threads/{thread_id}/state`, `/messages`, and `/runs` expose durable
  conversation state. Run-scoped `/messages` returns the messages produced by
  one execution.
- `POST /api/threads/{thread_id}/branches` creates an owner-scoped conversation
  from a completed assistant turn. Its copied journal and optional latest
  workspace are independent from the source after creation.
- `POST /api/threads/{thread_id}/runs` persists a pending run and its first
  `run.created` event, then hands a validated DeerFlow launch envelope to the
  configured starter.
- Run lookup, cancellation, event history, and SSE streaming use both thread
  and run IDs to enforce resource scoping.
- Model, feature, and skill discovery never expose provider credentials. Skill
  state changes are durably stored when a SQL backend is active.
- Upload and artifact routes resolve through the thread workspace, preserve
  collision-free names, enforce size and path boundaries, and support HTTP
  range delivery.
- Goal and todo endpoints operate on the same compare-and-swap state used by
  agent control tools. SQLite and PostgreSQL persist each version and reject
  stale writers.
- Memory CRUD, status, reload, and import/export endpoints share the scoped
  store used by agent recall and tools. Atomic imports and exact-scope updates
  prevent cross-user or cross-thread replacement.
- Assistant discovery and create-and-stream/wait run variants match the
  LangGraph client initialization flow. Stateless runs either create an owned
  thread or reuse the explicitly configured owned thread.

The gateway reserves the `user_id` metadata key for its authenticated owner.
It is never accepted from or returned to clients. Every thread, run, event,
state, and search operation verifies that owner; legacy records without an
owner remain visible only in unauthenticated local mode.

## Conversation continuity

Input messages are committed to the run journal before model execution.
Assistant and tool messages already use the same durable event stream. Before
a later run starts, Gofer reconstructs the complete thread conversation and
merges the request's non-overlapping suffix, so clients may send only the new
turn or a compatible history window without duplicating context. The normal
context-window middleware still compacts long conversations before each model
call.

Branching writes the selected conversation prefix as a synthetic successful
run in the new thread. Latest-turn branches copy the entire user-data tree via
a bounded staging directory and atomic rename. Historical branches deliberately
skip that copy because the current files may have been created after the
selected message. Clone failures remove the newly created target without
changing source history or files.

The launch envelope supports input, command, assistant, metadata, config,
context, checkpoint, interrupt, stream, disconnect, and multitask fields.
Unsupported compatibility placeholders are accepted only as `null`, matching
DeerFlow's fail-fast behavior.

## SSE replay

The stream endpoint writes journal sequence numbers as SSE IDs. A reconnecting
client may provide `Last-Event-ID`; Gofer first replays every durable event
after that sequence, then watches for new commits. Notifications are only wake
hints—the consumer always rereads ordered storage—so coalescing cannot lose
events. A terminal status starts a bounded journal-drain window, and an
observed terminal event closes immediately. This covers the intentional
status-before-final-event commit ordering without leaving legacy incomplete
journals open forever.

## SQL storage

`internal/store/sqlstore` owns the same `store.Store` contract for SQLite and
PostgreSQL. SQLite uses a pure-Go driver and one connection, enabling CGO-free
cross-platform builds. PostgreSQL uses pgx through `database/sql`.

Schema creation is idempotent. Thread patches/deletes, run transitions, and
event appends use serializable transactions and optimistic status/sequence
checks. Threads, runs, event data, timestamps, and metadata survive process
restart. Deletion is rejected while a run or local cleanup goroutine remains
active. Journal watches poll the database rather than relying only on
process-local signals, so events written by another service instance become
visible to an SSE consumer.
