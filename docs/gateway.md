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
- Run feedback supports create, idempotent update, scoped list/delete, and
  aggregate positive/negative statistics. A unique thread/run/user boundary
  prevents ambiguous UI ratings.
- Run responses expose journal-derived token totals, call count, caller
  attribution, message count, and stop reason. The thread token-usage endpoint
  aggregates completed runs by model and caller and can optionally include
  active progress.
- `GET /api/threads/{thread_id}/runs/{run_id}/workspace-changes` returns the
  latest journaled workspace/output review. `include_files=false` keeps only
  the summary, while `include_diff=false` retains file metadata without text.
- Every service-finalized journal contains a `run.delivery` fact before its terminal
  event. Runs that changed outputs add a presentation verdict; incomplete
  delivery converts an otherwise successful run to an error.

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

## Token accounting

Every completed primary model call stores provider-reported usage, model name,
caller role, and stop reason with its immutable message event. Auxiliary
compaction calls use a dedicated usage event. Successful child agents return
their complete accounting metadata through the parent tool result; repeated
`get`, `wait`, or `list` observations are deduplicated by child run ID.

Thread aggregation reads those journals instead of maintaining mutable shadow
counters. This keeps in-memory, SQLite, and PostgreSQL behavior identical,
survives restart without schema migration, and lets older events degrade to an
`unknown` model bucket. Total tokens are input plus output; reasoning and cache
figures remain detailed fields and are not double-counted. Branch history seed
runs are marked synthetic and excluded.

## Workspace change review

The service captures a baseline before tools start. At finish it first cancels
and drains outstanding child agents, then compares the current `workspace` and
`outputs` roots and appends a `workspace_changes` event before the terminal run
event. Uploads are immutable user input and are deliberately excluded. So are
version-control/cache/build directories, browser frames, and externalized tool
results that represent process feedback rather than deliverables.

Review work is bounded to 2,000 scanned files, 200 returned changes, 256 KiB of
text per file, and 1 MiB of total diff content. UTF-8 (with or without BOM) and
BOM-marked UTF-16 receive unified diffs. Binary extensions or content,
secret-looking paths, larger files, and symlinks expose only metadata and a
reason; symlink targets are never followed. The summary still reports all
changes seen inside the scan bound when file or diff details are truncated.

The endpoint reads the immutable journal, so the same result survives restart
under the in-memory, SQLite, and PostgreSQL adapters. Runs without changes—or
legacy runs without a review event—return `available: false` with a stable
versioned empty shape.

## Output delivery receipts

A run-scoped, concurrency-safe tracker observes successful artifact-producing
tool results from the lead agent and its child agents. The terminal
`run.delivery` event always contains `presented`, ordered `paths`, and a
`by_tool` map. Early preflight failures and cancellations receive the same
zero-presentation receipt shape.

When the workspace review finds a regular file created or modified below
`/mnt/user-data/outputs`, the receipt also records `produced_paths`, the paths
attributed specifically to `present_files`, their intersection, a stage, and a
boolean verdict. Produced-path detection uses the complete bounded scan rather
than the smaller file-detail page, so response truncation cannot alter the
verdict. Presenting an unrelated pre-existing file is not sufficient.
The receipt is persisted before `run.completed`, `run.failed`, or
`run.cancelled`; short retries cover transient journal failures. A missing
match changes an otherwise successful run to failed. If changed outputs were
successfully presented but their receipt cannot be persisted, the run also
fails because delivery cannot be durably verified. Receipt failure remains
best effort for chat-only runs with no changed outputs.

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

Feedback rows reference both their thread and run, use a unique
thread/run/user key for atomic upserts, and are removed by foreign-key cascade
when their conversation is deleted. The in-memory adapter implements the same
isolation and duplicate rules for deterministic tests and disposable service
mode.
